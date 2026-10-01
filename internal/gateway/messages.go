// /v1/messages —— Anthropic 入站协议（Messages API）。
//
// 让 Claude Code 等原生 Anthropic 客户端直连网关（参考 cc-switch 的转换桥思路）：
// 鉴权与限额口径与 /v1 其余端点完全一致（子 Key、日限额、模型白名单），下游
// 无论用什么协议都得到完整的能力（含 tools 全量三段式），中间层对两侧透明：
//   - 上游模型是 anthropic 协议 → 原生透传（仅替换 model，SSE 原样字节转发）；
//   - 上游模型是 openai 协议 → 请求/响应/流式双向转换（provider 入站桥）。
//
// 错误响应使用 Anthropic 错误信封（{"type":"error","error":{...}}），让客户端
// 按自家协议解析；上游错误体（无论何种协议）提取 message 后重新装封。
package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/model"
	"arkgate/internal/provider"
)

// errBodyAnthropic 构造 Anthropic 错误信封。
func errBodyAnthropic(typ, msg string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
}

// anthropicErrTypeForStatus 把 HTTP 状态码映射为 Anthropic 错误类型。
func anthropicErrTypeForStatus(code int) string {
	switch {
	case code == 401 || code == 403:
		return "authentication_error"
	case code == 404:
		return "not_found_error"
	case code == 429:
		return "rate_limit_error"
	case code >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// writeUpstreamErrorAnthropic 是 writeUpstreamError 的 /v1/messages 版：
// 状态码逻辑一致，错误体统一装进 Anthropic 信封（message 从上游错误体中
// 提取——OpenAI 与 Anthropic 的错误体都兼容 {"error":{"message"}}）。
func (g *Gateway) writeUpstreamErrorAnthropic(w http.ResponseWriter, err error) {
	if err == nil {
		err = errors.New("未知错误")
	}
	var convErr *provider.ConversionError
	if errors.As(err, &convErr) {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", err.Error()))
		return
	}
	if he, ok := provider.AsHTTPError(err); ok {
		status := he.Code
		if status < 400 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, errBodyAnthropic(anthropicErrTypeForStatus(status),
			provider.AnthropicErrorMessage(he.Body, err.Error())))
		return
	}
	writeJSON(w, http.StatusTooManyRequests, errBodyAnthropic("rate_limit_error", err.Error()))
}

// messagesEntryState 是 /v1/messages 入口解析出的公共信息。
type messagesEntryState struct {
	origName    string
	selName     string
	allowModels []string
	body        []byte
}

// handleMessages 处理 POST /v1/messages（流式/非流式）。
func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBodyAnthropic("invalid_request_error", "仅支持 POST"))
		return
	}
	sk, err := g.authSubKey(r, model.ModelTypeText)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBodyAnthropic("authentication_error", err.Error()))
		return
	}
	// 与 /v1 其它入口共用限长读取；Anthropic 入站需用 Anthropic 错误信封回错。
	body, berr := readBody(w, r)
	if berr != nil {
		writeJSON(w, anthropicBodyErrCode(berr), errBodyAnthropic("invalid_request_error",
			anthropicBodyErrMsg(berr)))
		return
	}
	// stream 归一：Anthropic 客户端也可能会发字符串型 stream；归一后无论走
	// 原生透传还是 OpenAI 转换桥，发出的 stream 都是合法布尔。
	if nb, changed := provider.NormalizeStreamFlag(body); changed {
		body = nb
	}
	// Anthropic 协议必填字段：model / max_tokens（缺失时按 Anthropic 语义 400）。
	modelName := extractModel(body)
	if modelName == "" {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "model 不能为空"))
		return
	}
	var probe struct {
		MaxTokens *int64          `json:"max_tokens"`
		Stream    json.RawMessage `json:"stream"`
	}
	if json.Unmarshal(body, &probe) != nil {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "请求体不是合法的 JSON"))
		return
	}
	if probe.MaxTokens == nil || *probe.MaxTokens <= 0 {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "max_tokens: field required（Anthropic 协议必填）"))
		return
	}
	if !modelAllowed(sk, modelName) {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "该 API Key 无权访问模型 "+modelName))
		return
	}
	// 图像模型没有 chat 语义；路由模型按输入文本长度正常解析。
	if mt := g.bal.ModelType(modelName); mt == model.ModelTypeImage {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "模型 "+modelName+" 是图像模型，不支持 messages 接口"))
		return
	}

	origName := modelName
	selName, allowModels, rerr := g.applyRouter(modelName, estimateAnthropicInputTokens(body), sk.AllowedModels)
	if rerr != nil {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", rerr.Error()))
		return
	}

	st := &messagesEntryState{origName: origName, selName: selName,
		allowModels: allowModels, body: body}
	if wantsStream(body) {
		g.messagesStreamForward(w, r, sk, st)
		return
	}
	g.messagesNonStream(w, r, sk, st)
}

// messagesNonStream 非流式 /v1/messages：与 chat 相同的重试语义
// （失败排除当前叶换下一个，首字节前对客户端无感）。
func (g *Gateway) messagesNonStream(w http.ResponseWriter, r *http.Request, sk *model.SubKey, st *messagesEntryState) {
	start := time.Now()
	ip := clientIP(r)
	meta := newReqMeta(r, false)
	var lastErr error
	exclude := map[string]bool{}
	// OpenAI 协议上游的请求转换只做一次（内容与叶子无关），失败直接 400。
	var convertedBody []byte

	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		sticky := ""
		if attempt == 0 {
			sticky = sk.ID
		}
		leaf, actualModel, err := g.bal.SelectWithFallback(st.selName, sk.AllowedAccounts, st.allowModels, exclude, balancer.APIChat, sticky)
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, 0, 0, 0, derr, 0, start, meta)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		var respBody []byte
		var usage *provider.TextUsage
		var ferr error
		if g.bal.ModelProtocol(actualModel) == model.ModelProtocolAnthropic {
			// 原生透传：请求体仅替换 model，响应/错误原样（Anthropic 形状）。
			respBody, usage, ferr = g.mgr.AnthropicNativeChat(upstreamCtx(r), ri.rt, st.body, leaf.EP, g.cfg.Timeouts.Request())
		} else {
			if convertedBody == nil {
				convertedBody, ferr = provider.OpenAIRequestFromAnthropic(st.body)
				if ferr != nil {
					g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, 0, 0, 0, ferr, 0, start, meta)
					writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", ferr.Error()))
					return
				}
			}
			respBody, usage, ferr = g.mgr.Chat(upstreamCtx(r), ri.rt, convertedBody, leaf.EP, g.cfg.Timeouts.Request())
			if ferr == nil {
				// 响应反向转换：OpenAI chat.completion → Anthropic message。
				respBody, usage, ferr = provider.AnthropicResponseFromOpenAI(respBody, actualModel)
			}
		}
		if ferr == nil {
			var pt, ct int64
			if usage != nil {
				pt, ct = usage.PromptTokens, usage.CompletionTokens
			}
			g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, pt, ct, 0, nil, 0, start, meta)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
			return
		}
		var pt, ct int64
		if usage != nil {
			pt, ct = usage.PromptTokens, usage.CompletionTokens
		}
		if g.failAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, pt, ct, 0, ferr, 0, start, meta) {
			return // 下游已断开：换叶子重试无意义（父 ctx 已死）
		}
		exclude[leaf.ID] = true
		lastErr = ferr
	}
	g.writeUpstreamErrorAnthropic(w, lastErr)
}

// messagesStreamForward 流式 /v1/messages：首字节之前可跨叶子重试。
// 上游 anthropic → 原样字节转发；上游 openai → 转换泵逐 chunk 重写为
// Anthropic 事件流（message_start → 块序列 → message_delta → message_stop）。
func (g *Gateway) messagesStreamForward(w http.ResponseWriter, r *http.Request, sk *model.SubKey, st *messagesEntryState) {
	start := time.Now()
	ip := clientIP(r)
	meta := newReqMeta(r, true) // 本函数只服务流式请求

	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBodyAnthropic("api_error", "无法建立流式响应"))
		return
	}

	exclude := map[string]bool{}
	var lastErr error
	var convertedBody []byte
	for attempt := 0; attempt <= g.cfg.MaxRetriesAvailable; attempt++ {
		sticky := ""
		if attempt == 0 {
			sticky = sk.ID
		}
		leaf, actualModel, err := g.bal.SelectWithFallback(st.selName, sk.AllowedAccounts, st.allowModels, exclude, balancer.APIChat, sticky)
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		ri, derr := g.resolveRoute(leaf)
		if derr != nil {
			g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, 0, 0, 0, derr, 0, start, meta)
			exclude[leaf.ID] = true
			lastErr = derr
			continue
		}

		var stream provider.MessageStream
		var oerr error
		if g.bal.ModelProtocol(actualModel) == model.ModelProtocolAnthropic {
			stream, oerr = g.mgr.OpenAnthropicNativeStream(r.Context(), ri.rt, st.body, leaf.EP, g.cfg.Timeouts.FirstToken())
		} else {
			if convertedBody == nil {
				convertedBody, oerr = provider.OpenAIRequestFromAnthropic(st.body)
				if oerr != nil {
					g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, 0, 0, 0, oerr, 0, start, meta)
					writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", oerr.Error()))
					return
				}
			}
			stream, oerr = g.mgr.OpenChatStreamAsAnthropic(r.Context(), ri.rt, convertedBody, leaf.EP, g.cfg.Timeouts.FirstToken())
		}
		if oerr != nil {
			// 协议转换拒绝（请求内容问题）：不重试，直接 400。
			var convErr *provider.ConversionError
			if errors.As(oerr, &convErr) {
				g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, 0, 0, 0, oerr, 0, start, meta)
				writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", oerr.Error()))
				return
			}
			// 首字节前打开失败：换叶子重试；下游断开则直接收尾。
			if g.failAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, 0, 0, 0, oerr, 0, start, meta) {
				return
			}
			exclude[leaf.ID] = true
			lastErr = oerr
			continue
		}

		// 首字节到手：提交 SSE 响应头，此后不再跨叶子重试。
		firstTokenMs := time.Since(start).Milliseconds()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// 逐写即刷（与 streamForward 同款 flushWriter）：SSE 事件必须及时推送，
		// 不能缓冲到整条上游流结束后才落客户端。
		pt, ct, perr := stream.Pump(flushWriter{w: w, f: fl})
		stream.Close()
		fl.Flush()
		if perr != nil {
			// 流已开始，无法更改状态码；用 Anthropic error 事件收尾
			//（不把失败伪装成正常完成）。
			frames, _ := json.Marshal(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": perr.Error()},
			})
			_, _ = w.Write([]byte("event: error\ndata: " + string(frames) + "\n\n"))
			fl.Flush()
		}
		g.recordAttempt(sk, ip, leaf, ri, st.origName, actualModel, model.ModelTypeText, pt, ct, 0, perr, firstTokenMs, start, meta)
		return
	}
	g.writeUpstreamErrorAnthropic(w, lastErr)
}

// handleMessagesCountTokens 处理 POST /v1/messages/count_tokens。
// 按入站估算器给出粗略值（与虚拟路由同一套口径），不打上游。
func (g *Gateway) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBodyAnthropic("invalid_request_error", "仅支持 POST"))
		return
	}
	sk, err := g.authSubKey(r, model.ModelTypeText)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBodyAnthropic("authentication_error", err.Error()))
		return
	}
	// 与 /v1 其它入口共用限长读取；Anthropic 入站需用 Anthropic 错误信封回错。
	body, berr := readBody(w, r)
	if berr != nil {
		writeJSON(w, anthropicBodyErrCode(berr), errBodyAnthropic("invalid_request_error",
			anthropicBodyErrMsg(berr)))
		return
	}
	modelName := extractModel(body)
	if modelName == "" {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "model 不能为空"))
		return
	}
	if !modelAllowed(sk, modelName) {
		writeJSON(w, http.StatusBadRequest, errBodyAnthropic("invalid_request_error", "该 API Key 无权访问模型 "+modelName))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicInputTokens(body)})
}

// anthropicBodyErrCode 请求体读取失败的 HTTP 状态码。
// 超限用 413（语义正确：重试无用），其它读取失败用 400。
func anthropicBodyErrCode(err error) int {
	if errors.Is(err, errRequestBodyTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// anthropicBodyErrMsg 请求体读取失败的文案（与 OpenAI 入口保持同样的信息量）。
func anthropicBodyErrMsg(err error) string {
	if errors.Is(err, errRequestBodyTooLarge) {
		return "请求体过大：上限 64MB"
	}
	return "读取请求体失败"
}
