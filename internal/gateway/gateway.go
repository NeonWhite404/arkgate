// Package gateway 实现 OpenAI 兼容转发层。
//
// 对外暴露 /v1/*。下游用子 Key（sk-xxx）鉴权 + 易读模型名调用；网关在「该模型
// 的可用叶节点集合」里做负载均衡（按 API 能力过滤：responses/images 仅路由到
// 有能力的账号），命中一个叶子后把 model 换成该叶子的上游模型标识（不透明字符串）、
// 子 Key 换成账号真实 API Key，经 provider.Manager 透传转发，再把响应以原始字节回传。
//
// 并发：每个 HTTP 请求在一个独立 goroutine 中处理；转发复用 provider 的共享连接池；
// 运行态更新走原子操作，统计/日志经 balancer 阻塞投递异步落库。
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
	"arkgate/internal/provider"
	"arkgate/internal/secure"
	"arkgate/internal/store"
)

// Gateway 汇聚依赖。
type Gateway struct {
	cfg     *config.Config
	store   Store
	box     *secure.Box
	bal     *balancer.Balancer
	mgr     *provider.Manager
	handler http.Handler
}

// Store 是网关所需的存储接口（由 store.Store 实现）。
type Store interface {
	ListModels() ([]*model.Model, error)
	GetSubKeyByHash(hash string) (*model.SubKey, error)
	GetAccount(id string) (*model.Account, error)
	GetDailyUsage(subkeyID string) (*store.DailyUsage, error)
}

// New 构造网关。
func New(cfg *config.Config, st Store, box *secure.Box, bal *balancer.Balancer) *Gateway {
	g := &Gateway{cfg: cfg, store: st, box: box, bal: bal, mgr: provider.NewManager(cfg.RequestTimeout)}
	g.handler = g.routes()
	return g
}

// Handler 返回 http.Handler。
func (g *Gateway) Handler() http.Handler { return g.handler }

func (g *Gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.chatCompletions)
	mux.HandleFunc("/v1/responses", g.responses)
	mux.HandleFunc("/v1/images/generations", g.imagesGenerations)
	mux.HandleFunc("/v1/models", g.listModels)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errBody("invalid_request_error", "不支持的接口"))
	})
	return mux
}

// ─────────────────────────── 鉴权 ───────────────────────────

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

func (g *Gateway) authSubKey(r *http.Request, modality string) (*model.SubKey, error) {
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
	// 日限额按「当日真实用量」判定（usage_daily 由异步统计落库时累计；
	// 在途请求可能带来短暂超量，属可接受误差）。
	// token 限额约束全部模态；图像张数限额只约束图像请求。
	if sk.DailyLimitTokens > 0 || (sk.DailyLimitImages > 0 && modality == model.ModelTypeImage) {
		du, err := g.store.GetDailyUsage(sk.ID)
		if err != nil {
			// 日用量读取失败时放行（可用性优先），但必须留痕——否则限额会被静默绕过。
			log.Printf("gateway: 读取子 Key %s 日用量失败，本次跳过日限额检查: %v", sk.ID, err)
		} else {
			if sk.DailyLimitTokens > 0 && du.Tokens >= sk.DailyLimitTokens {
				return nil, errors.New("该 API Key 已达到当日 token 用量上限")
			}
			if sk.DailyLimitImages > 0 && modality == model.ModelTypeImage && du.Images >= sk.DailyLimitImages {
				return nil, errors.New("该 API Key 已达到当日图像张数上限")
			}
		}
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

// ─────────────────────────── 路由解析 ───────────────────────────

// routeInfo 是一次转发所需的全部上游信息。
type routeInfo struct {
	rt          provider.Route
	accountID   string
	accountName string
}

// resolveRoute 把选中的叶节点解析成可发送路由：
// 账号 → 供应商定义 → 最终 base URL → 解密真实 Key（不透明字符串，原样进鉴权头）。
func (g *Gateway) resolveRoute(leaf *model.Endpoint) (routeInfo, error) {
	acc, err := g.store.GetAccount(leaf.AccountID)
	if err != nil {
		return routeInfo{}, errors.New("账号不存在")
	}
	def, ok := provider.Get(acc.Provider)
	if !ok {
		def = provider.FallbackDef(acc.Provider)
	}
	baseURL, err := def.ResolveBaseURL(acc.BaseURL)
	if err != nil {
		return routeInfo{}, errors.New("账号未配置 base URL")
	}
	key, err := g.box.Decrypt(acc.ArkAPIKeyEnc)
	if err != nil {
		return routeInfo{}, errors.New("账号密钥解密失败")
	}
	return routeInfo{
		rt:          provider.Route{Def: def, BaseURL: baseURL, Key: key},
		accountID:   acc.ID,
		accountName: acc.Name,
	}, nil
}

// recordAttempt 结算一次尝试：叶节点熔断 + 统计 + 日志 + 释放并发 + 喂 TPM。
func (g *Gateway) recordAttempt(sk *model.SubKey, leaf *model.Endpoint, ri routeInfo,
	requestedModel, actualModel, modality string, pt, ct, images int64, ferr error, start time.Time) {
	ok := ferr == nil
	l := &model.UsageLog{
		TS:               time.Now().Unix(),
		SubKeyID:         sk.ID,
		SubKeyName:       sk.Name,
		AccountID:        ri.accountID,
		AccountName:      ri.accountName,
		Provider:         ri.rt.Def.ID,
		EndpointID:       leaf.ID,
		EP:               leaf.EP, // 实际调用的上游模型标识
		RequestedModel:   requestedModel,
		Model:            actualModel, // 记录真实命中的模型（fallback 后）
		Modality:         modality,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      pt + ct,
		ImageCount:       images,
		Status:           "ok",
		LatencyMs:        time.Since(start).Milliseconds(),
	}
	if !ok {
		l.Status = "error"
		l.Error = errText(ferr)
	}
	// 喂 TPM：计费单位与叶节点 tpm_limit 的语义一致——文本喂 token 数，
	// 图像喂张数（图像响应即使带 token 用量也不混入张数窗口）。
	units := pt + ct
	if modality == model.ModelTypeImage {
		units = images
	}
	g.bal.TPMAdd(leaf, units)
	g.bal.Record(l, leaf, ok)
	g.bal.Release(leaf)
}

// ─────────────────────────── /v1/chat/completions ───────────────────────────

func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("invalid_request_error", "仅支持 POST"))
		return
	}
	sk, err := g.authSubKey(r, model.ModelTypeText)
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

	if wantsStream(body) {
		// 流式：一旦开始写首字节就无法重试，因此采用「预隔离」策略——
		// 先绑定 leaf 与真实 Key，若后续失败只写终止帧，不再做跨叶子的第二次尝试。
		g.streamOnce(w, r, sk, body, modelName, balancer.APIChat)
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
		leaf, actualModel, err := g.bal.SelectWithFallback(modelName, sk.AllowedAccounts, sk.AllowedModels, exclude, balancer.APIChat)
		if err != nil {
			lastErr = err
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeText, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		respBody, usage, ferr := g.mgr.Chat(r.Context(), ri.rt, body, leaf.EP)
		if ferr == nil {
			var pt, ct int64
			if usage != nil {
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeText, pt, ct, 0, nil, start)
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
		g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeText, pt, ct, 0, ferr, start)
		exclude[leaf.ID] = true
		lastErr = ferr
	}

	// 全部尝试失败：按真实错误,而非一刀切 429。
	g.writeUpstreamError(w, lastErr)
}

// ─────────────────────────── /v1/responses ───────────────────────────

func (g *Gateway) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("invalid_request_error", "仅支持 POST"))
		return
	}
	sk, err := g.authSubKey(r, model.ModelTypeText)
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

	if wantsStream(body) {
		g.streamOnce(w, r, sk, body, modelName, balancer.APIResponses)
		return
	}
	g.responsesNonStream(w, r, sk, body, modelName)
}

// responsesNonStream 非流式 responses：与 chat 相同的重试语义。
func (g *Gateway) responsesNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte, modelName string) {
	start := time.Now()
	var lastErr error
	exclude := map[string]bool{}

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		leaf, actualModel, err := g.bal.SelectWithFallback(modelName, sk.AllowedAccounts, sk.AllowedModels, exclude, balancer.APIResponses)
		if err != nil {
			lastErr = err
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeText, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		respBody, usage, ferr := g.mgr.Responses(r.Context(), ri.rt, body, leaf.EP)
		if ferr == nil {
			var pt, ct int64
			if usage != nil {
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeText, pt, ct, 0, nil, start)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeText, pt, ct, 0, ferr, start)
		exclude[leaf.ID] = true
		lastErr = ferr
	}

	g.writeUpstreamError(w, lastErr)
}

// ─────────────────────────── /v1/images/generations ───────────────────────────

func (g *Gateway) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("invalid_request_error", "仅支持 POST"))
		return
	}
	sk, err := g.authSubKey(r, model.ModelTypeImage)
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
	// 类型校验：文本模型不允许打图像接口（反之图像模型打 chat/responses 会在
	// 路由层被同类型 fallback 约束挡下，这里先做正向校验给出明确错误）。
	if m, err := g.store.ListModels(); err == nil {
		for _, mm := range m {
			if mm.Name == modelName && mm.Type == model.ModelTypeText {
				writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "模型 "+modelName+" 不是图像模型"))
				return
			}
		}
	}

	if wantsStream(body) {
		g.streamOnce(w, r, sk, body, modelName, balancer.APIImages)
		return
	}
	g.imagesNonStream(w, r, sk, body, modelName)
}

// imagesNonStream 非流式图像生成：与 chat 相同的重试语义。
func (g *Gateway) imagesNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte, modelName string) {
	start := time.Now()
	var lastErr error
	exclude := map[string]bool{}

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		leaf, actualModel, err := g.bal.SelectWithFallback(modelName, sk.AllowedAccounts, sk.AllowedModels, exclude, balancer.APIImages)
		if err != nil {
			lastErr = err
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeImage, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		respBody, usage, ferr := g.mgr.Images(r.Context(), ri.rt, body, leaf.EP)
		if ferr == nil {
			var images int64
			var pt, ct int64
			if usage != nil {
				images = usage.Count
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeImage, pt, ct, images, nil, start)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		g.recordAttempt(sk, leaf, ri, modelName, actualModel, model.ModelTypeImage, pt, ct, 0, ferr, start)
		exclude[leaf.ID] = true
		lastErr = ferr
	}

	g.writeUpstreamError(w, lastErr)
}

// ─────────────────────────── 流式（单次尝试，三类接口共用） ───────────────────────────

// streamOnce 流式转发：单次尝试。绑定 leaf 后转发；上游失败时如果尚未写头，
// 才回错误；已写头则以 SSE error 帧收尾。具体子路径由各 Transport 方法内部决定。
func (g *Gateway) streamOnce(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte,
	modelName string, api balancer.API) {

	start := time.Now()
	leaf, actualModel, err := g.bal.SelectWithFallback(modelName, sk.AllowedAccounts, sk.AllowedModels, nil, api)
	if err != nil {
		g.writeUpstreamError(w, err)
		return
	}
	ri, derr := g.resolveRoute(leaf)
	if derr != nil {
		g.recordAttempt(sk, leaf, ri, modelName, actualModel, modalityOf(api), 0, 0, 0, derr, start)
		g.writeUpstreamError(w, derr)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		g.recordAttempt(sk, leaf, ri, modelName, actualModel, modalityOf(api), 0, 0, 0, errors.New("无法建立流式响应"), start)
		writeJSON(w, http.StatusInternalServerError, errBody("server_error", "无法建立流式响应"))
		return
	}

	var (
		pt, ct, images int64
		ferr           error
	)
	switch api {
	case balancer.APIResponses:
		u, e := g.mgr.ResponsesStream(r.Context(), ri.rt, body, leaf.EP, w)
		if u != nil {
			pt, ct = u.PromptTokens, u.CompletionTokens
		}
		ferr = e
	case balancer.APIImages:
		images, ferr = g.mgr.ImagesStream(r.Context(), ri.rt, body, leaf.EP, w)
	default: // chat
		u, e := g.mgr.ChatStream(r.Context(), ri.rt, body, leaf.EP, w)
		if u != nil {
			pt, ct = u.PromptTokens, u.CompletionTokens
		}
		ferr = e
	}
	fl.Flush()
	if ferr != nil {
		// 流已开始，无法更改状态码；用 SSE error 帧收尾。
		writeSSEError(w, ferr)
	}
	g.recordAttempt(sk, leaf, ri, modelName, actualModel, modalityOf(api), pt, ct, images, ferr, start)
}

func modalityOf(api balancer.API) string {
	if api == balancer.APIImages {
		return model.ModelTypeImage
	}
	return model.ModelTypeText
}

// ─────────────────────────── /v1/models ───────────────────────────

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	sk, err := g.authSubKey(r, model.ModelTypeText)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody("invalid_request_error", err.Error()))
		return
	}
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		Type    string `json:"type"` // 自定义扩展：text | image（标准客户端忽略）
	}
	all, err := g.store.ListModels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("server_error", err.Error()))
		return
	}
	items := make([]modelObj, 0, len(all))
	for _, m := range all {
		if !m.Enabled {
			continue
		}
		if !modelAllowed(sk, m.Name) {
			continue // 只返回该子 Key 有权访问的模型
		}
		t := m.Type
		if t == "" {
			t = model.ModelTypeText
		}
		items = append(items, modelObj{ID: m.Name, Object: "model", Created: 1, OwnedBy: "arkgate", Type: t})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

// ─────────────────────────── 错误输出 ───────────────────────────

// writeUpstreamError 把最终错误以合理状态码 + OpenAI 结构回给客户端
// （透传上游错误体，而非折叠成 429）。
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, err error) {
	if err == nil {
		err = errors.New("未知错误")
	}
	if he, ok := provider.AsHTTPError(err); ok {
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
	// 本地错误（无账号/全熔断/限流/无能力等）用 429 + OpenAI 结构。
	writeJSON(w, http.StatusTooManyRequests, errBody("upstream_error", errText(err)))
}

func writeSSEError(w http.ResponseWriter, err error) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": errText(err), "type": "upstream_error"}})
	_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
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
	if code >= 400 {
		log.Printf("gateway: error %d: %+v", code, v)
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("gateway: write json: %v", err)
	}
}
