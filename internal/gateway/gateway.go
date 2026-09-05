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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
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
	g := &Gateway{cfg: cfg, store: st, box: box, bal: bal, mgr: provider.NewManager()}
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
	mux.HandleFunc("/v1/messages", g.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", g.handleMessagesCountTokens)
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
	// Anthropic 客户端（Claude Code）用 x-api-key 头携带 Key，与 Bearer 同权。
	token := bearerToken(r)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if token == "" {
		return nil, errors.New("缺少 API Key（Authorization: Bearer sk-xxx 或 X-Api-Key）")
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

// applyRouter 把虚拟路由模型（type=router）解析成承接请求的真实目标模型名。
// 非路由模型原样返回，调用方无感。tokens 为调用方按请求形态估算的输入长度
//（chat/responses/anthropic 各有估算器）。
//
// 白名单语义：子 Key 的 AllowedModels 只需包含路由名本身——解析出的目标由网关
// 补进传给 balancer 的白名单副本，否则「只授权了路由名」的子 Key 会在 Select
// 阶段被自己的白名单挡掉。目标自身的 fallback 链目标仍需显式授权（与既有
// 「fallback 不越权」语义一致）。白名单为空（= 全部）时原样透传，不能反向收窄。
func (g *Gateway) applyRouter(name string, tokens int64, allowModels []string) (string, []string, error) {
	resolved, err := g.bal.ResolveRouter(name, tokens)
	if err != nil || resolved == name {
		return name, allowModels, err
	}
	if len(allowModels) > 0 {
		allowModels = append(append([]string{}, allowModels...), resolved)
	}
	return resolved, allowModels, nil
}

// recordAttempt 结算一次尝试：叶节点熔断 + 统计 + 日志 + 释放并发 + 喂 TPM。
// ip 为下游调用方地址（由各链路入口用 clientIP(r) 取一次后传入）。
func (g *Gateway) recordAttempt(sk *model.SubKey, ip string, leaf *model.Endpoint, ri routeInfo,
	requestedModel, actualModel, modality string, pt, ct, images int64, ferr error, start time.Time) {
	ok := ferr == nil
	// 客户端请求自身导致的失败（上下文超限等）：统计/日志照记，但不计入端点熔断。
	clientErr := !ok && provider.IsRequestFault(ferr)
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
		ClientIP:         ip,
	}
	if !ok {
		l.Status = "error"
		l.Error = errText(ferr)
		if clientErr {
			// 标注请求方责任：日志页可直接过滤定位，且不计入端点熔断。
			l.Error = "请求超限（客户端侧）：" + l.Error
		}
	}
	// 喂 TPM：计费单位与叶节点 tpm_limit 的语义一致——文本喂 token 数，
	// 图像喂张数（图像响应即使带 token 用量也不混入张数窗口）。
	units := pt + ct
	if modality == model.ModelTypeImage {
		units = images
	}
	g.bal.TPMAdd(leaf, units)
	g.bal.Record(l, leaf, ok, clientErr)
	g.bal.Release(leaf)
}

// chatOutputKeys / responsesOutputKeys 各端点中表达「输出上限」的请求字段。
var (
	chatOutputKeys      = []string{"max_tokens", "max_completion_tokens"}
	responsesOutputKeys = []string{"max_output_tokens"}
)

// clampOutput 把请求体中的输出上限字段裁剪到模型最大输出 limit。
// max_tokens 是上限语义而非承诺，裁剪不会截断输出，可省掉一次注定 4xx 的
// 上游调用（省配额、不进失败统计）。仅在确实超过时重编码请求体，否则原样
// 返回，保持字节透传。
func clampOutput(body []byte, keys []string, limit int64) ([]byte, bool) {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return body, false
	}
	changed := false
	for _, k := range keys {
		raw, ok := req[k]
		if !ok {
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) != nil || v <= float64(limit) {
			continue // 未超限或非数值字段不碰
		}
		req[k] = json.RawMessage(strconv.AppendInt(nil, limit, 10))
		changed = true
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

// clientIP 取下游调用方 IP：优先反代头（X-Forwarded-For 首跳 / X-Real-IP），
// 否则用连接的 RemoteAddr。
//
// 注意：这两个头由调用方可控，只有网关确实部署在受信反代之后才可信。因此该值
// **只用于日志展示与排查**，不参与鉴权、限流或任何路由判断——否则伪造一个头
// 就能绕过基于 IP 的策略。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // 无端口形式（少见）原样记录
	}
	return host
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
	// 模态正向校验：图像模型不能打 chat 接口（在打上游之前给出明确错误，
	// 而不是转发后被上游 4xx 且记一次「客户端超限」）。
	if mt := g.bal.ModelType(modelName); mt == model.ModelTypeImage {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "模型 "+modelName+" 是图像模型，不支持 chat 接口"))
		return
	}
	// 虚拟路由模型解析：type=router 的名字不直接承接流量，先按输入长度
	// 换算成真实目标模型，之后的选路/裁剪/粘性都针对目标进行。
	origName := modelName
	selName, allowModels, rerr := g.applyRouter(modelName, estimateInputTokens(balancer.APIChat, body), sk.AllowedModels)
	if rerr != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", rerr.Error()))
		return
	}
	modelName = selName
	// 能力前置校验：目录设置了最大输出时裁剪输出上限字段，省掉注定失败的上游调用。
	// 放在路由解析之后：路由模型自身不设上限，裁剪依据是最终承接请求的目标模型。
	if _, maxOut := g.bal.ModelLimits(modelName); maxOut > 0 {
		if nb, changed := clampOutput(body, chatOutputKeys, maxOut); changed {
			body = nb
		}
	}

	if wantsStream(body) {
		// 流式：首字节之前未向客户端写过数据，失败可换叶子重试；
		// 首字节到手后才提交 SSE 响应头，之后单次尝试到流结束。
		g.streamForward(w, r, sk, body, origName, modelName, allowModels, balancer.APIChat)
		return
	}
	g.chatNonStream(w, r, sk, body, origName, modelName, allowModels)
}

// chatNonStream 非流式：支持失败后排除当前叶子、切换下一个重试。
// origName 是客户端请求的模型名（路由解析前），selName 是实际选路用的名字
// （路由解析后）；日志里 requested_model 记 origName，让「路由模型 → 真实模型」
// 的分流在日志里可见。
func (g *Gateway) chatNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte,
	origName, selName string, allowModels []string) {
	start := time.Now()
	ip := clientIP(r)
	var lastErr error
	exclude := map[string]bool{}

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		// 粘性会话只在全新请求的首次选举上生效；重试路径为空，避免钉错。
		sticky := ""
		if attempt == 0 {
			sticky = sk.ID
		}
		leaf, actualModel, err := g.bal.SelectWithFallback(selName, sk.AllowedAccounts, allowModels, exclude, balancer.APIChat, sticky)
		if err != nil {
			// 已有具体尝试失败（上游错误/超时）时保留它：排除集导致的全忙错误
			// 只是重试的副产品，不如真实失败原因有信息量。
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		// 上游协议分支：Anthropic 协议模型走转换桥（请求/响应双向转换），
		// 其余 OpenAI 兼容模型走原样字节透传。max_tokens 在此处按实际命中的
		// 模型解析（Anthropic 必填；fallback 可能命中与请求模型不同的模型）。
		var respBody []byte
		var usage *provider.TextUsage
		var ferr error
		if g.bal.ModelProtocol(actualModel) == model.ModelProtocolAnthropic {
			respBody, usage, ferr = g.mgr.AnthropicChat(r.Context(), ri.rt, body, leaf.EP,
				g.anthropicMaxTokens(body, actualModel), g.cfg.Timeouts.Request())
		} else {
			respBody, usage, ferr = g.mgr.Chat(r.Context(), ri.rt, body, leaf.EP, g.cfg.Timeouts.Request())
		}
		if ferr == nil {
			var pt, ct int64
			if usage != nil {
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, pt, ct, 0, nil, start)
			// 透传上游真实状态码与 body。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		// 协议转换拒绝（tools/图像输入等请求内容问题）：换叶子没有意义，
		// 直接以 400 收尾，不进入重试循环（recordAttempt 也不计端点熔断）。
		var convErr *provider.ConversionError
		if errors.As(ferr, &convErr) {
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, 0, 0, 0, ferr, start)
			writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", ferr.Error()))
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, pt, ct, 0, ferr, start)
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
	if mt := g.bal.ModelType(modelName); mt == model.ModelTypeImage {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "模型 "+modelName+" 是图像模型，不支持 responses 接口"))
		return
	}
	// 虚拟路由模型解析：同 chat（responses 的输入结构不同，估算走对应分支）。
	origName := modelName
	selName, allowModels, rerr := g.applyRouter(modelName, estimateInputTokens(balancer.APIResponses, body), sk.AllowedModels)
	if rerr != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", rerr.Error()))
		return
	}
	modelName = selName
	// 上游协议校验：Anthropic 协议上游只有 messages（chat）能力，
	// responses 透传对它没有意义，在入口给出明确错误。
	if p := g.bal.ModelProtocol(modelName); p == model.ModelProtocolAnthropic {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "模型 "+origName+" 的上游使用 Anthropic 协议，仅支持 /v1/chat/completions"))
		return
	}
	// 能力前置校验：responses 的输出上限字段是 max_output_tokens；同 chat，
	// 裁剪依据放在路由解析之后（用目标模型的上限）。
	if _, maxOut := g.bal.ModelLimits(modelName); maxOut > 0 {
		if nb, changed := clampOutput(body, responsesOutputKeys, maxOut); changed {
			body = nb
		}
	}

	if wantsStream(body) {
		g.streamForward(w, r, sk, body, origName, modelName, allowModels, balancer.APIResponses)
		return
	}
	g.responsesNonStream(w, r, sk, body, origName, modelName, allowModels)
}

// responsesNonStream 非流式 responses：与 chat 相同的重试语义（origName/selName 见 chatNonStream）。
func (g *Gateway) responsesNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte,
	origName, selName string, allowModels []string) {
	start := time.Now()
	ip := clientIP(r)
	var lastErr error
	exclude := map[string]bool{}

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		sticky := ""
		if attempt == 0 {
			sticky = sk.ID
		}
		leaf, actualModel, err := g.bal.SelectWithFallback(selName, sk.AllowedAccounts, allowModels, exclude, balancer.APIResponses, sticky)
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		respBody, usage, ferr := g.mgr.Responses(r.Context(), ri.rt, body, leaf.EP, g.cfg.Timeouts.Request())
		if ferr == nil {
			var pt, ct int64
			if usage != nil {
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, pt, ct, 0, nil, start)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, model.ModelTypeText, pt, ct, 0, ferr, start)
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
	// 类型正向校验（在打上游之前给出明确错误，而不是转发后被上游 4xx）：
	// 文本模型不能打图像接口；路由模型按输入文本长度设计分流语义，同样不承接图像。
	// 口径：balancer 的启用模型目录（停用/未知名走后面的 Select 报错）。
	switch g.bal.ModelType(modelName) {
	case model.ModelTypeText:
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "模型 "+modelName+" 不是图像模型"))
		return
	case model.ModelTypeRouter:
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "模型 "+modelName+" 是路由模型，不支持图像生成接口"))
		return
	}

	if wantsStream(body) {
		g.streamForward(w, r, sk, body, modelName, modelName, sk.AllowedModels, balancer.APIImages)
		return
	}
	g.imagesNonStream(w, r, sk, body, modelName)
}

// imagesNonStream 非流式图像生成：与 chat 相同的重试语义。
func (g *Gateway) imagesNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte, modelName string) {
	start := time.Now()
	ip := clientIP(r)
	var lastErr error
	exclude := map[string]bool{}

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		sticky := ""
		if attempt == 0 {
			sticky = sk.ID
		}
		leaf, actualModel, err := g.bal.SelectWithFallback(modelName, sk.AllowedAccounts, sk.AllowedModels, exclude, balancer.APIImages, sticky)
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, ip, leaf, ri, modelName, actualModel, model.ModelTypeImage, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		respBody, usage, ferr := g.mgr.Images(r.Context(), ri.rt, body, leaf.EP, g.cfg.Timeouts.Request())
		if ferr == nil {
			var images int64
			var pt, ct int64
			if usage != nil {
				images = usage.Count
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, ip, leaf, ri, modelName, actualModel, model.ModelTypeImage, pt, ct, images, nil, start)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		g.recordAttempt(sk, ip, leaf, ri, modelName, actualModel, model.ModelTypeImage, pt, ct, 0, ferr, start)
		exclude[leaf.ID] = true
		lastErr = ferr
	}

	g.writeUpstreamError(w, lastErr)
}

// ─────────────────────────── 流式（三类接口共用） ───────────────────────────

// streamForward 流式转发：首字节之前可跨叶子重试。
//
// origName/selName 的语义见 chatNonStream：requested_model 记请求名（路由解析前），
// 实际选路用 selName（路由解析后）。
//
// 每一轮先选叶、解析路由，再经 provider 打开上游流并等待首 token：
//   - 打开失败（建连失败 / 非 2xx / 首 token 超时）时未向客户端写过任何字节，
//     记一次失败统计后排除当前叶子，换下一个继续；
//   - 首字节到手才提交 SSE 响应头并开始 Pump，此后无法重试，流中错误用
//     SSE error 帧收尾；
//   - 所有叶子都失败时，以普通 HTTP 错误透传（此时响应头尚未提交）。
func (g *Gateway) streamForward(w http.ResponseWriter, r *http.Request, sk *model.SubKey, body []byte,
	origName, selName string, allowModels []string, api balancer.API) {

	start := time.Now()
	modality := modalityOf(api)
	ip := clientIP(r)

	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("server_error", "无法建立流式响应"))
		return
	}

	exclude := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		sticky := ""
		if attempt == 0 {
			sticky = sk.ID
		}
		leaf, actualModel, err := g.bal.SelectWithFallback(selName, sk.AllowedAccounts, allowModels, exclude, api, sticky)
		if err != nil {
			// 同非流式：保留已有的具体失败原因（如首 token 超时），避免被
			// 排除集导致的全忙错误掩盖。
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, modality, 0, 0, 0, derr, start)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		// 打开上游流：成功 = 已收到首字节；失败时未写过任何客户端字节，可重试。
		// Anthropic 协议模型走转换桥（SSE 重写为 OpenAI chunk），其余原样透传。
		var st provider.MessageStream
		var oerr error
		if g.bal.ModelProtocol(actualModel) == model.ModelProtocolAnthropic {
			st, oerr = g.mgr.OpenAnthropicChatStream(r.Context(), ri.rt, body, leaf.EP,
				g.anthropicMaxTokens(body, actualModel), g.cfg.Timeouts.FirstToken())
		} else {
			st, oerr = g.openStream(r.Context(), ri.rt, body, leaf.EP, api)
		}
		if oerr != nil {
			// 协议转换拒绝（请求内容问题）：不重试，直接 400（响应头尚未提交）。
			var convErr *provider.ConversionError
			if errors.As(oerr, &convErr) {
				g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, modality, 0, 0, 0, oerr, start)
				writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", oerr.Error()))
				return
			}
			g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, modality, 0, 0, 0, oerr, start)
			exclude[leaf.ID] = true
			lastErr = oerr
			continue
		}

		// 首字节到手：提交 SSE 响应头，此后不再跨叶子重试。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // 防反代缓冲 SSE

		var pt, ct, images int64
		perr := func() error {
			switch api {
			case balancer.APIImages:
				a, b, e := st.Pump(w)
				pt, ct = a, b
				if e == nil {
					images = provider.ExtractN(body) // 张数按请求 n 计量；失败不计费
				}
				return e
			default:
				a, b, e := st.Pump(w)
				pt, ct = a, b
				return e
			}
		}()
		st.Close()
		fl.Flush()
		if perr != nil {
			// 流已开始，无法更改状态码；用 SSE error 帧收尾。
			writeSSEError(w, perr)
		}
		g.recordAttempt(sk, ip, leaf, ri, origName, actualModel, modality, pt, ct, images, perr, start)
		return
	}

	// 所有叶子都失败：响应头未提交，可写正常 HTTP 错误。
	g.writeUpstreamError(w, lastErr)
}

// openStream 按目标 API 打开对应的上游流（首 token 超时取自运行时配置，0 = 不限时）。
// 返回值以 MessageStream 接口呈现，与 Anthropic 转换流共用 Pump/Close 签名。
func (g *Gateway) openStream(ctx context.Context, rt provider.Route, body []byte, ep string, api balancer.API) (provider.MessageStream, error) {
	timeout := g.cfg.Timeouts.FirstToken()
	switch api {
	case balancer.APIResponses:
		return g.mgr.OpenResponsesStream(ctx, rt, body, ep, timeout)
	case balancer.APIImages:
		return g.mgr.OpenImagesStream(ctx, rt, body, ep, timeout)
	default:
		return g.mgr.OpenChatStream(ctx, rt, body, ep, timeout)
	}
}

// anthropicMaxTokens 解析 Anthropic 协议必填的 max_tokens：请求显式携带的值
//（已经 clampOutput 按目标模型上限裁剪）优先；未携带时用目录里的模型输出上限；
// 两者都没有时回落保守默认——Anthropic 没有默认值，缺了必然 400。
func (g *Gateway) anthropicMaxTokens(body []byte, modelName string) int64 {
	if v := openAIMaxTokensOf(body); v > 0 {
		return v
	}
	if _, maxOut := g.bal.ModelLimits(modelName); maxOut > 0 {
		return maxOut
	}
	return provider.DefaultAnthropicMaxTokens
}

// openAIMaxTokensOf 读取 OpenAI 请求体里显式的输出上限（chat 的两个键）。
func openAIMaxTokensOf(body []byte) int64 {
	var v struct {
		MaxTokens          *int64 `json:"max_tokens"`
		MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	if v.MaxTokens != nil && *v.MaxTokens > 0 {
		return *v.MaxTokens
	}
	if v.MaxCompletionTokens != nil && *v.MaxCompletionTokens > 0 {
		return *v.MaxCompletionTokens
	}
	return 0
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
		// 能力上限扩展（0 省略）：目录设置后下游可据此约束请求规模。
		ContextWindow   int64 `json:"context_window,omitempty"`
		MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
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
		items = append(items, modelObj{ID: m.Name, Object: "model", Created: 1, OwnedBy: "arkgate", Type: t,
			ContextWindow: m.ContextTokens, MaxOutputTokens: m.MaxOutputTokens})
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
