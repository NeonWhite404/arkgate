// Package gateway 实现 OpenAI 兼容转发层。
//
// 对外暴露 /v1/*。下游用子 Key（sk-xxx）鉴权 + 易读模型名调用；网关在「该模型
// 的可用 ep-接入点（叶节点）集合」里做负载均衡，命中一个叶子后把 model 换成真实
// ep-xxx、子 Key 换成账号真实 API Key，转发到火山 /api/v3，再把响应以 OpenAI
// 兼容格式回传。
//
// 并发：每个 HTTP 请求在一个独立 goroutine 中处理；转发使用 upstream.Client 的
// 连接池；运行态更新走原子操作，统计/日志经 balancer 阻塞投递异步落库。
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/config"
	"arkgate/internal/model"
	"arkgate/internal/secure"
	"arkgate/internal/upstream"
)

// Gateway 汇聚依赖。
type Gateway struct {
	cfg     *config.Config
	store   Store
	box     *secure.Box
	bal     *balancer.Balancer
	up      *upstream.Client
	handler http.Handler
}

// Store 是网关所需的存储接口（由 store.Store 实现）。
type Store interface {
	ListSubKeys() ([]*model.SubKey, error)
	GetSubKeyByHash(hash string) (*model.SubKey, error)
	GetAccount(id string) (*model.Account, error)
}

// New 构造网关。
func New(cfg *config.Config, st Store, box *secure.Box, bal *balancer.Balancer) *Gateway {
	g := &Gateway{cfg: cfg, store: st, box: box, bal: bal, up: upstream.New(cfg)}
	g.handler = g.routes()
	return g
}

// Handler 返回 http.Handler。
func (g *Gateway) Handler() http.Handler { return g.handler }

func (g *Gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.chatCompletions)
	mux.HandleFunc("/v1/models", g.listModels)
	mux.HandleFunc("/v1/responses", g.responses)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errBody("invalid_request_error", "不支持的接口"))
	})
	return mux
}

// ─────────────────────────── 鉴权与认证 ───────────────────────────

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

func (g *Gateway) authSubKey(r *http.Request) (*model.SubKey, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errors.New("缺少 API Key（Authorization: Bearer sk-xxx）")
	}
	sk, err := g.store.GetSubKeyByHash(hashKey(token))
	if err != nil {
		return nil, errors.New("无效的 API Key")
	}
	if !sk.Enabled {
		return nil, errors.New("该 API Key 已被禁用")
	}
	if sk.ExpiresAt > 0 && sk.ExpiresAt < time.Now().Unix() {
		return nil, errors.New("该 API Key 已过期")
	}
	if sk.DailyLimitTokens > 0 && sk.TotalTokens >= sk.DailyLimitTokens {
		return nil, errors.New("该 API Key 已达到当日用量上限")
	}
	return sk, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &v)
	return strings.TrimSpace(v.Model)
}

func wantsStream(body []byte) bool {
	var v struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &v) == nil {
		return v.Stream
	}
	return false
}

func modelAllowed(sk *model.SubKey, name string) bool {
	if len(sk.AllowedModels) == 0 {
		return true
	}
	for _, m := range sk.AllowedModels {
		if m == name {
			return true
		}
	}
	return false
}

// ─────────────────────────── /v1/chat/completions ───────────────────────────

func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("invalid_request_error", "仅支持 POST"))
		return
	}
	sk, err := g.authSubKey(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody("invalid_request_error", err.Error()))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "读取请求体失败"))
		return
	}

	modelName := extractModel(body)
	if modelName == "" {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "model 不能为空"))
		return
	}
	if !modelAllowed(sk, modelName) {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "该 API Key 无权访问模型 "+modelName))
		return
	}

	stream := wantsStream(body)

	// 流式：一旦开始写首字节就无法重试，因此采用「预隔离」策略——
	// 先绑定 leaf 与真实 Key，若后续失败只写终止帧，不再做跨叶子的第二次尝试。
	if stream {
		g.chatStream(w, r, sk, body, modelName)
		return
	}
	g.chatNonStream(w, r, sk, body, modelName)
}

// chatNonStream 非流式：支持失败后排除当前叶子、切换下一个重试。
func (g *Gateway) chatNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte, modelName string) {
	start := time.Now()
	var lastErr error
	exclude := map[string]bool{}

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		leaf, err := g.bal.Select(modelName, sk.AllowedAccounts, exclude)
		if err != nil {
			lastErr = err
			break
		}
		acc, apiKey, derr := g.resolveKey(leaf)
		if derr != nil {
			g.recordAttempt(sk, leaf, modelName, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}
		_ = acc

		respBody, usage, ferr := g.up.ChatCompletion(r.Context(), apiKey, body, leaf.EP)
		if ferr == nil {
			pt, ct := int64(0), int64(0)
			if usage != nil {
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, leaf, modelName, pt, ct, nil, start)
			// 透传上游真实状态码与 body。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		g.recordAttempt(sk, leaf, modelName, pt, ct, ferr, start)
		exclude[leaf.ID] = true
		lastErr = ferr
	}

	// 全部尝试失败：按真实错误,而非一刀切 429。
	g.writeUpstreamError(w, lastErr)
}

// chatStream 流式：单次尝试。绑定 leaf 后转发；上游失败时如果尚未写头，才回错误。
func (g *Gateway) chatStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte, modelName string) {
	start := time.Now()
	leaf, err := g.bal.Select(modelName, sk.AllowedAccounts, nil)
	if err != nil {
		g.writeUpstreamError(w, err)
		return
	}
	_, apiKey, derr := g.resolveKey(leaf)
	if derr != nil {
		g.recordAttempt(sk, leaf, modelName, 0, 0, derr, start)
		g.writeUpstreamError(w, derr)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		g.recordAttempt(sk, leaf, modelName, 0, 0, errors.New("无法建立流式响应"), start)
		writeJSON(w, http.StatusInternalServerError, errBody("server_error", "无法建立流式响应"))
		return
	}
	pt, ct, ferr := g.up.Stream(r.Context(), apiKey, body, leaf.EP, w)
	fl.Flush()
	if ferr != nil {
		// 流已开始，无法更改状态码；用 SSE error 帧收尾。
		writeSSEError(w, ferr)
	}
	g.recordAttempt(sk, leaf, modelName, pt, ct, ferr, start)
}

// resolveKey 解析叶节点对应账号的真实 API Key。
func (g *Gateway) resolveKey(leaf *model.Endpoint) (*model.Account, string, error) {
	acc, err := g.store.GetAccount(leaf.AccountID)
	if err != nil {
		return nil, "", errors.New("账号不存在")
	}
	key, err := g.box.Decrypt(acc.ArkAPIKeyEnc)
	if err != nil {
		return nil, "", errors.New("账号密钥解密失败")
	}
	return acc, key, nil
}

// recordAttempt 结算一次尝试：叶节点熔断 + 统计 + 日志 + 释放并发 + 喂 TPM。
func (g *Gateway) recordAttempt(sk *model.SubKey, leaf *model.Endpoint, modelName string, pt, ct int64, ferr error, start time.Time) {
	ok := ferr == nil
	l := &model.UsageLog{
		TS:               time.Now().Unix(),
		SubKeyID:         sk.ID,
		SubKeyName:       sk.Name,
		AccountID:        leaf.AccountID,
		EndpointID:       leaf.ID,
		Model:            modelName,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      pt + ct,
		Status:           "ok",
		LatencyMs:        time.Since(start).Milliseconds(),
	}
	if acc, err := g.store.GetAccount(leaf.AccountID); err == nil {
		l.AccountName = acc.Name
	}
	if !ok {
		l.Status = "error"
		l.Error = errText(ferr)
	}
	// 先喂 TPM（用实际 token 净增量），再记录，保证限流语义一致。
	g.bal.TPMAdd(leaf, pt+ct)
	g.bal.Record(l, leaf, ok)
	g.bal.Release(leaf)
}

// writeUpstreamError 把最终错误以合理状态码 + OpenAI 结构回给客户端
// （透传上游错误体，而非折叠成 429）。
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, err error) {
	if err == nil {
		err = errors.New("未知错误")
	}
	if he, ok := upstream.AsHTTPError(err); ok {
		// 透传上游状态码与原始 body（若 body 是 JSON 错误结构则原样转发）。
		status := he.Code
		if status < 400 {
			status = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(he.Body) > 0 {
			_, _ = w.Write(he.Body)
			return
		}
	}
	// 本地错误（无账号/全熔断/限流等）用 429 + OpenAI 结构。
	writeJSON(w, http.StatusTooManyRequests, errBody("upstream_error", errText(err)))
}

func writeSSEError(w http.ResponseWriter, err error) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": errText(err), "type": "upstream_error"}})
	_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

// ─────────────────────────── /v1/models ───────────────────────────

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	sk, err := g.authSubKey(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody("invalid_request_error", err.Error()))
		return
	}
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	names := g.bal.SnapshotModelNames()
	items := make([]modelObj, 0, len(names))
	for _, n := range names {
		if !modelAllowed(sk, n) {
			continue // 只返回该子 Key 有权访问的模型
		}
		items = append(items, modelObj{ID: n, Object: "model", Created: 1, OwnedBy: "arkgate"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

// ─────────────────────────── /v1/responses（非主路径） ───────────────────────────

func (g *Gateway) responses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, errBody("invalid_request_error", "Responses API 暂未实现，请使用 /v1/chat/completions"))
}

// ─────────────────────────── 工具 ───────────────────────────

func errBody(typ, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"type": typ, "message": msg, "code": nil}}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("gateway: write json: %v", err)
	}
}
