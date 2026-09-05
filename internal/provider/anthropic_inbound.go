// Anthropic 入站协议桥（下游 = Anthropic 客户端，如 Claude Code）。
//
// 与 anthropic.go 的出站桥互补：/v1/messages 请求进来后，按上游模型协议分流——
//   - 上游 anthropic：原生透传（仅替换 model，SSE 原样字节转发，sniff 提取用量）；
//   - 上游 openai：请求/响应/流式双向转换，tools 全量三段式映射。
//
// 转换语义参照 cc-switch（farion1231/cc-switch）的实现：
//   - system（字符串或块数组）合并为单条 system 消息；
//   - 单 text 块简化为纯字符串 content；assistant 纯 tool_calls 时 content=null；
//   - tool_result 块拆成独立 tool role 消息（保持相邻，利于 prompt cache）；
//   - tool_choice：any→required、tool→function 选择器；
//   - 流式 message_start 惰性发送、message_delta 缓存到 [DONE]（保证 usage
//     完整）、多个 finish_reason 只认第一个、EOF 未 [DONE] 时补发收尾事件；
//   - stop_reason 映射：tool_calls→tool_use、length→max_tokens、其余→end_turn
//     （Anthropic 没有 content_filter 停止原因）。
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ─────────────────────────── 入站请求解析与转换（Anthropic → OpenAI） ───────────────────────────

// anthropicInboundRequest 是 /v1/messages 请求体（宽松解析：未知字段忽略）。
type anthropicInboundRequest struct {
	Model         string                    `json:"model"`
	MaxTokens     *int64                    `json:"max_tokens"`
	System        json.RawMessage           `json:"system"` // string | text 块数组
	Messages      []anthropicInboundMessage `json:"messages"`
	Temperature   *float64                  `json:"temperature"`
	TopP          *float64                  `json:"top_p"`
	StopSequences []string                  `json:"stop_sequences"`
	Tools         []anthropicToolDef        `json:"tools"`
	ToolChoice    json.RawMessage           `json:"tool_choice"`
	Stream        bool                      `json:"stream"`
	Metadata      json.RawMessage           `json:"metadata"` // 透传无意义，忽略
	Thinking      json.RawMessage           `json:"thinking"` // 扩展思考：OpenAI 无对应语义，忽略
}

type anthropicInboundMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | 块数组 | null
}

// anthropicInboundBlock 是入站多态内容块（只声明转换关心的字段）。
type anthropicInboundBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use（assistant 历史轮）
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result（user 工具结果回填）
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string | 块数组
	IsError   bool            `json:"is_error"`
	// image
	Source *anthropicImageSource `json:"source"`
}

// openAIInboundChatRequest 是转换产出的 OpenAI chat 请求体。model 字段填
// 客户端请求的易读名，转发前由 prepareBody 统一替换为上游标识。
type openAIInboundChatRequest struct {
	Model       string               `json:"model"`
	Messages    []openAIInboundMsg   `json:"messages"`
	MaxTokens   *int64               `json:"max_tokens,omitempty"`
	Temperature *float64             `json:"temperature,omitempty"`
	TopP        *float64             `json:"top_p,omitempty"`
	Stop        []string             `json:"stop,omitempty"`
	Tools       []openAIInboundTool  `json:"tools,omitempty"`
	ToolChoice  any                  `json:"tool_choice,omitempty"`
	Stream      bool                 `json:"stream"`
}

type openAIInboundMsg struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"` // 字符串 / parts 数组 / null
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIInboundTool struct {
	Type     string   `json:"type"`
	Function struct {
		Name        string        `json:"name"`
		Description string        `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// OpenAIRequestFromAnthropic 把 /v1/messages 请求体转换成 OpenAI chat 请求体。
func OpenAIRequestFromAnthropic(down []byte) ([]byte, error) {
	var req anthropicInboundRequest
	if err := json.Unmarshal(down, &req); err != nil {
		return nil, convErr("请求体不是合法的 Anthropic messages 格式")
	}
	if len(req.Messages) == 0 {
		return nil, convErr("messages 不能为空")
	}
	out := openAIInboundChatRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
	}
	// system：字符串或块数组，合并为单条 system 消息（参照 cc-switch：
	// 跨轮字节稳定，不影响上游 prompt cache）。
	if sys := anthropicSystemText(req.System); sys != "" {
		out.Messages = append(out.Messages, openAIInboundMsg{Role: "system", Content: sys})
	}
	for i, m := range req.Messages {
		msgs, err := convertInboundMessage(m)
		if err != nil {
			return nil, convErr("第 %d 条消息：%v", i+1, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}
	// 工具声明：input_schema ↔ parameters（schema 轻量清洗同出站桥）。
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 || isJSONNull(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, openAIInboundTool{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters"`
			}{Name: t.Name, Description: t.Description, Parameters: cleanJSONSchema(schema)},
		})
	}
	if tc := anthropicToolChoiceToOpenAI(req.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	return json.Marshal(out)
}

// anthropicSystemText 提取 system 字段文本（字符串或 text 块数组，"\n" 连接）。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || isJSONNull(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertInboundMessage 把一条 Anthropic 消息转换成 1..n 条 OpenAI 消息：
// user 消息里的 tool_result 块各自拆成独立 tool 消息（cc-switch 同款口径），
// 其余内容归入同角色的普通消息。
func convertInboundMessage(m anthropicInboundMessage) ([]openAIInboundMsg, error) {
	// 字符串 content 快路径。
	if len(m.Content) > 0 && !isJSONNull(m.Content) {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return []openAIInboundMsg{{Role: m.Role, Content: s}}, nil
		}
	}
	var blocks []anthropicInboundBlock
	if len(m.Content) > 0 && !isJSONNull(m.Content) {
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil, errors.New("content 格式无法识别")
		}
	}

	var (
		out       []openAIInboundMsg
		parts     []map[string]any // 本消息累积的普通内容（text/image）
		toolCalls []openAIToolCall
	)
	flushContent := func() {
		if len(parts) == 0 && len(toolCalls) == 0 {
			return
		}
		msg := openAIInboundMsg{Role: m.Role}
		switch {
		case len(parts) == 1 && parts[0]["type"] == "text":
			// 单 text 块简化为纯字符串（cc-switch 同款：利于上游缓存）。
			msg.Content = parts[0]["text"]
		case len(parts) > 0:
			msg.Content = parts
		case len(toolCalls) > 0:
			msg.Content = nil // 纯工具调用：content 置 null
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		out = append(out, msg)
		parts, toolCalls = nil, nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "", "text":
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case "image":
			if b.Source == nil {
				return nil, errors.New("image 块缺少 source")
			}
			url := b.Source.URL
			if b.Source.Type == "base64" {
				media := b.Source.MediaType
				if media == "" {
					media = "image/png"
				}
				url = "data:" + media + ";base64," + b.Source.Data
			}
			parts = append(parts, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": url}})
		case "tool_use":
			input := strings.TrimSpace(string(b.Input))
			if len(input) == 0 || !json.Valid([]byte(input)) {
				input = "{}"
			}
			toolCalls = append(toolCalls, openAIToolCall{ID: b.ID, Type: "function",
				Function: openAIToolCallFn{Name: b.Name, Arguments: input}})
		case "tool_result":
			// 工具结果立即拆成独立 tool 消息（ flush 之前的普通内容，保持相邻）。
			flushContent()
			out = append(out, openAIInboundMsg{Role: "tool",
				ToolCallID: b.ToolUseID, Content: toolResultText(b.Content, b.IsError)})
		case "thinking", "redacted_thinking":
			// 历史轮的思考块：OpenAI 无对应字段，丢弃（不影响语义连续性）。
		default:
			return nil, errors.New("暂不支持 " + b.Type + " 内容块")
		}
	}
	flushContent()
	if len(out) == 0 {
		out = append(out, openAIInboundMsg{Role: m.Role, Content: ""})
	}
	return out, nil
}

// toolResultText 把 tool_result 的 content 归一为字符串（string | 块数组）。
// is_error 时加前缀标记，让上游模型感知失败（OpenAI tool 消息无错误语义）。
func toolResultText(raw json.RawMessage, isErr bool) string {
	text := ""
	if len(raw) == 0 || isJSONNull(raw) {
		text = ""
	} else if s := strings.TrimSpace(string(raw)); strings.HasPrefix(s, "\"") {
		json.Unmarshal(raw, &text)
	} else {
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) == nil {
			parts := make([]string, 0, len(blocks))
			for _, b := range blocks {
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.Join(parts, "\n")
		} else {
			text = s
		}
	}
	if text == "" {
		text = "(empty)"
	}
	if isErr {
		text = "[tool error] " + text
	}
	return text
}

// anthropicToolChoiceToOpenAI 把 Anthropic tool_choice 映射为 OpenAI 形态；
// 未设置时返回 nil。
func anthropicToolChoiceToOpenAI(raw json.RawMessage) any {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "any":
			return "required"
		case "none":
			return "none"
		default: // auto
			return "auto"
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		switch obj.Type {
		case "any":
			return "required"
		case "tool":
			return map[string]any{"type": "function",
				"function": map[string]any{"name": obj.Name}}
		default: // auto / none
			return obj.Type
		}
	}
	return nil
}

// ─────────────────────────── 响应转换（OpenAI → Anthropic） ───────────────────────────

// AnthropicResponseFromOpenAI 把 OpenAI chat.completion 响应转换成
// Anthropic message 响应。tool_calls 映射回 tool_use 块（arguments 字符串
// 解析为 input 对象，解析失败回 {}）。
func AnthropicResponseFromOpenAI(raw []byte, fallbackModel string) ([]byte, *TextUsage, error) {
	var src openAIChatResponse
	if err := json.Unmarshal(raw, &src); err != nil || len(src.Choices) == 0 {
		return nil, nil, errors.New("上游响应不是合法的 OpenAI chat 格式")
	}
	choice := src.Choices[0]
	var content []anthropicBlock
	addText := func(t string) {
		if t != "" {
			content = append(content, anthropicBlock{Type: "text", Text: t})
		}
	}
	switch c := choice.Message.Content.(type) {
	case string:
		addText(c)
	case []any:
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if pm["type"] == "text" || pm["type"] == "output_text" {
				if t, _ := pm["text"].(string); t != "" {
					addText(t)
				}
			}
		}
	}
	for _, tc := range choice.Message.ToolCalls {
		input := map[string]any{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		inputBytes, _ := json.Marshal(input)
		content = append(content, anthropicBlock{Type: "tool_use",
			ID: tc.ID, Name: tc.Function.Name, Input: inputBytes})
	}
	if len(content) == 0 {
		content = []anthropicBlock{{Type: "text", Text: ""}}
	}
	model := src.Model
	if model == "" {
		model = fallbackModel
	}
	out := map[string]any{
		"id": src.ID, "type": "message", "role": "assistant", "model": model,
		"content":       content,
		"stop_reason":   openaiFinishToStopReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  src.Usage.PromptTokens,
			"output_tokens": src.Usage.CompletionTokens,
		},
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return data, &TextUsage{PromptTokens: src.Usage.PromptTokens,
		CompletionTokens: src.Usage.CompletionTokens}, nil
}

// openaiFinishToStopReason 把 OpenAI finish_reason 映射为 Anthropic stop_reason
//（参照 cc-switch：Anthropic 没有 content_filter，统一落 end_turn）。
func openaiFinishToStopReason(finish string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	default:
		// stop / content_filter / 空值 → end_turn
		return "end_turn"
	}
}

// ─────────────────────────── 原生透传（Anthropic 下游 ↔ Anthropic 上游） ───────────────────────────

// ExtractAnthropicUsage 从 Anthropic message 响应提取用量（input/output）。
func ExtractAnthropicUsage(raw []byte) *TextUsage {
	var parsed struct {
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		return &TextUsage{PromptTokens: parsed.Usage.InputTokens,
			CompletionTokens: parsed.Usage.OutputTokens}
	}
	return &TextUsage{}
}

// AnthropicNativeChat 原生透传非流式请求（下游与上游同为 Anthropic 协议）：
// 仅替换 model，响应原样返回，错误体保持 Anthropic 形状原样透传。
func (m *Manager) AnthropicNativeChat(ctx context.Context, rt Route, down []byte,
	upstreamModel string, timeout time.Duration) ([]byte, *TextUsage, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var (
		captured []byte
		status   int
	)
	client := anthropicClient(rt, m.httpc)
	var raw []byte
	err = client.Post(ctx, "v1/messages", json.RawMessage(body), &raw,
		option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			resp, err := next(req)
			if err != nil {
				return resp, err
			}
			if resp != nil && resp.Body != nil {
				data, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if rerr == nil {
					captured, status = data, resp.StatusCode
					resp.Body = io.NopCloser(bytes.NewReader(data))
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(nil))
				}
			}
			return resp, nil
		}))
	if err != nil {
		if status >= 400 {
			// 原生路径：错误体已是 Anthropic 形状，原样透传。
			return nil, nil, &HTTPError{Code: status, Body: captured}
		}
		var ae *anthropic.Error
		if errors.As(err, &ae) && ae.StatusCode >= 400 {
			return nil, nil, &HTTPError{Code: ae.StatusCode, Body: []byte(ae.Error())}
		}
		return nil, nil, err
	}
	return raw, ExtractAnthropicUsage(raw), nil
}

// OpenAnthropicNativeStream 打开 Anthropic 上游的原样字节流（仅替换 model）。
// 与转换流不同：SSE 不做任何重写，Stream.Pump 原样转发并经 sniff 提取用量。
func (m *Manager) OpenAnthropicNativeStream(ctx context.Context, rt Route, down []byte,
	upstreamModel string, firstTokenTimeout time.Duration) (_ *Stream, err error) {

	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	var fired atomic.Bool
	var timer *time.Timer
	if firstTokenTimeout > 0 {
		reqCtx, cancel = context.WithCancel(ctx)
		defer func() {
			if err != nil {
				cancel()
			}
		}()
		timer = time.AfterFunc(firstTokenTimeout, func() {
			if fired.CompareAndSwap(false, true) {
				cancel()
			}
		})
		defer timer.Stop()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		anthropicEndpoint(rt.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", rt.Key)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := m.httpc.Do(req)
	if err != nil {
		if timer != nil && fired.Load() {
			return nil, fmt.Errorf("%w（等待 %s 无输出）", ErrFirstToken, firstTokenTimeout)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("upstream timeout")
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// 原生路径：错误体已是 Anthropic 形状，原样透传。
		return nil, &HTTPError{Code: resp.StatusCode, Body: respBody}
	}

	reader := bufio.NewReader(resp.Body)
	// 原样透传：从第一个字节起全部保留（event: 行是 Anthropic SSE 协议的
	// 一部分，客户端按其区分事件类型），首行到达即视为「上游开始响应」。
	first, rerr := reader.ReadBytes('\n')
	if rerr != nil && len(bytes.TrimSpace(first)) == 0 {
		resp.Body.Close()
		if timer != nil && fired.Load() {
			return nil, fmt.Errorf("%w（等待 %s 无输出）", ErrFirstToken, firstTokenTimeout)
		}
		return nil, fmt.Errorf("上游未返回任何数据: %w", rerr)
	}
	if timer != nil {
		if !fired.CompareAndSwap(false, true) {
			resp.Body.Close()
			return nil, fmt.Errorf("%w（首字节与超时同时到达）", ErrFirstToken)
		}
		timer.Stop()
	}
	return &Stream{resp: resp, rdr: reader, first: first,
		sniff: anthropicSSESniff, cancel: cancel}, nil
}

// anthropicSSESniff 从 Anthropic 原生 SSE 的 data 载荷提取用量：
// message_start 带 input_tokens，message_delta 带 output_tokens。
func anthropicSSESniff(payload []byte) (pt, ct int64, ok bool) {
	var evt struct {
		Type    string         `json:"type"`
		Message *struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(payload, &evt) != nil {
		return 0, 0, false
	}
	switch evt.Type {
	case "message_start":
		if evt.Message != nil {
			return evt.Message.Usage.InputTokens, 0, true
		}
	case "message_delta":
		if evt.Usage != nil {
			return 0, evt.Usage.OutputTokens, true
		}
	}
	return 0, 0, false
}

// ─────────────────────────── 流式转换（OpenAI chunk → Anthropic SSE） ───────────────────────────
//
// 状态机参照 cc-switch 的 streaming.rs：
//   - message_start 惰性发送（首个带 choices 的 chunk 才发）；
//   - 非 tool 块（thinking/text）单实例，类型切换前先 close 旧块；
//   - tool 块按 OpenAI tool index 一块一槽，懒启动：id+name 齐全才发
//     content_block_start，之前的 arguments 片段先缓存；
//   - message_delta（stop_reason + usage）缓存到 [DONE]/EOF 才发（保证 usage
//     完整），多个 finish_reason 只认第一个；
//   - 上游错误以 event: error 收尾，不把失败伪装成正常完成。

// anthropicSSEEvent 生成一个 Anthropic SSE 事件帧（event: X\ndata: {...}\n\n）。
func anthropicSSEEvent(name string, payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("event: " + name + "\ndata: " + string(data) + "\n\n")
}

type openAIToAnthropicState struct {
	id, model string
	pt, ct    int64
	finish    string
	// message_start 是否已发送 / 收尾序列是否已发送。
	started bool
	closed  bool
	// 非 tool 内容块（thinking/text）单实例管理。
	nonToolType  string // "" | "thinking" | "text"
	nonToolIndex int
	nonToolOpen  bool
	// 工具块：openai tool index → anthropic 块号与懒启动状态。
	nextBlockIndex  int
	toolBlocks      map[int]*toolBlockState
	pendingDelta    *anthropicDeltaPayload // message_delta 缓存（usage 完整后再发）
	deltaEmitted    bool
}

type toolBlockState struct {
	anthropicIndex int
	id, name       string
	started        bool
	pendingArgs    strings.Builder
}

// anthropicDeltaPayload 是 message_delta 事件的负载。
type anthropicDeltaPayload struct {
	StopReason string         `json:"stop_reason"`
	Usage      anthropicUsage `json:"usage"`
}

// anthropicFromOpenAIStream 是「OpenAI chunk 流 → Anthropic SSE 事件流」的
// 转换流：包装打开的 OpenAI 原始流，Pump 时逐 chunk 重写为 Anthropic 事件。
type anthropicFromOpenAIStream struct {
	st *Stream
	s  *openAIToAnthropicState
}

func (s *anthropicFromOpenAIStream) Pump(sink io.Writer) (pt, ct int64, err error) {
	pt, ct, err = s.pumpLines(sink)
	return
}

func (s *anthropicFromOpenAIStream) pumpLines(sink io.Writer) (int64, int64, error) {
	st, state := s.st, s.s
	line := st.first
	for {
		if len(line) > 0 {
			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				if bytes.Equal(payload, []byte("[DONE]")) {
					return state.finalize(sink)
				}
				if len(payload) > 0 {
					done, err := state.handleChunk(payload, sink)
					if err != nil {
						return state.pt, state.ct, err
					}
					if done {
						return state.pt, state.ct, nil
					}
				}
			}
			// 非 data 行（注释/空行）跳过
		}
		next, rerr := st.rdr.ReadBytes('\n')
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				// 未收到 [DONE]：补发缓存的 message_delta/message_stop。
				return state.finalize(sink)
			}
			return state.pt, state.ct, rerr
		}
		line = next
	}
}

func (s *anthropicFromOpenAIStream) Close() { s.st.Close() }

// handleChunk 处理一个 OpenAI chunk，写出对应的 Anthropic 事件。
// 返回 done=true 表示流已收尾（message_stop 已发）。
func (s *openAIToAnthropicState) handleChunk(payload []byte, sink io.Writer) (bool, error) {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"` // DeepSeek 等兼容字段 → thinking 块
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false, nil // 非法 chunk 跳过（宁少事件，不中断整条流）
	}
	if chunk.ID != "" && s.id == "" {
		s.id = chunk.ID
	}
	if chunk.Model != "" && s.model == "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.pt = chunk.Usage.PromptTokens
		s.ct = chunk.Usage.CompletionTokens
	}
	if len(chunk.Choices) == 0 {
		return false, nil // include_usage 的末尾 chunk 只带 usage
	}
	choice := chunk.Choices[0]

	// message_start 惰性发送（首个带 choices 的 chunk）。
	if !s.started {
		s.started = true
		if err := writeSSE(sink, anthropicSSEEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": s.id, "type": "message", "role": "assistant", "model": s.model,
				"content":      []any{},
				"stop_reason":  nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})); err != nil {
			return false, err
		}
	}

	// thinking（兼容上游的 reasoning_content 字段）。
	if choice.Delta.Reasoning != "" {
		if err := s.pushNonToolBlock("thinking", "thinking_delta",
			map[string]any{"type": "thinking_delta", "thinking": choice.Delta.Reasoning}, sink); err != nil {
			return false, err
		}
	}
	// 文本增量。
	if choice.Delta.Content != "" {
		if err := s.pushNonToolBlock("text", "text_delta",
			map[string]any{"type": "text_delta", "text": choice.Delta.Content}, sink); err != nil {
			return false, err
		}
	}
	// 工具调用增量：按 OpenAI index 分槽，懒启动（id+name 齐全才 start）。
	for _, tc := range choice.Delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		tb := s.toolBlocks[idx]
		if tb == nil {
			tb = &toolBlockState{anthropicIndex: s.nextBlockIndex}
			s.nextBlockIndex++
			s.toolBlocks[idx] = tb
		}
		if tc.ID != "" {
			tb.id = tc.ID
		}
		if tc.Function.Name != "" {
			tb.name = tc.Function.Name
		}
		if !tb.started {
			// 块未启动：非 tool 块先关闭（工具块紧随其后）。
			if err := s.closeNonToolBlock(sink); err != nil {
				return false, err
			}
			if tb.id != "" && tb.name != "" {
				tb.started = true
				if err := writeSSE(sink, anthropicSSEEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": tb.anthropicIndex,
					"content_block": map[string]any{"type": "tool_use", "id": tb.id,
						"name": tb.name, "input": map[string]any{}},
				})); err != nil {
					return false, err
				}
			}
		}
		if tb.started && tc.Function.Arguments != "" {
			if err := writeSSE(sink, anthropicSSEEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": tb.anthropicIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
			})); err != nil {
				return false, err
			}
		} else if !tb.started && tc.Function.Arguments != "" {
			tb.pendingArgs.WriteString(tc.Function.Arguments)
		}
	}
	// finish_reason：只认第一个（部分上游会发多个），缓存 message_delta 到收尾。
	if choice.FinishReason != nil && !s.deltaEmitted && s.pendingDelta == nil {
		s.pendingDelta = &anthropicDeltaPayload{
			StopReason: openaiFinishToStopReason(*choice.FinishReason, len(choice.Delta.ToolCalls) > 0 || len(s.toolBlocks) > 0),
		}
	}
	return false, nil
}

// pushNonToolBlock 管理 thinking/text 单实例块：类型切换先 close 旧块再开新块。
func (s *openAIToAnthropicState) pushNonToolBlock(blockType, deltaType string, delta map[string]any, sink io.Writer) error {
	if s.nonToolType != blockType {
		if err := s.closeNonToolBlock(sink); err != nil {
			return err
		}
		s.nonToolType = blockType
		s.nonToolIndex = s.nextBlockIndex
		s.nextBlockIndex++
		s.nonToolOpen = true
		// content_block_start 按协议带对应类型的空字段（text→text:""，
		// thinking→thinking:""），与官方 SSE 形状一致。
		block := map[string]any{"type": blockType}
		if blockType == "text" {
			block["text"] = ""
		} else {
			block["thinking"] = ""
		}
		if err := writeSSE(sink, anthropicSSEEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": s.nonToolIndex,
			"content_block": block,
		})); err != nil {
			return err
		}
	}
	return writeSSE(sink, anthropicSSEEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.nonToolIndex, "delta": delta,
	}))
}

// closeNonToolBlock 关闭当前打开的非 tool 块（未打开则空操作）。
func (s *openAIToAnthropicState) closeNonToolBlock(sink io.Writer) error {
	if !s.nonToolOpen {
		return nil
	}
	s.nonToolOpen = false
	s.nonToolType = ""
	return writeSSE(sink, anthropicSSEEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.nonToolIndex,
	}))
}

// finalize 发送收尾序列：关闭打开的块 → message_delta（缓存中的 stop_reason +
// usage）→ message_stop。重复调用安全。
func (s *openAIToAnthropicState) finalize(sink io.Writer) (int64, int64, error) {
	if s.closed {
		return s.pt, s.ct, nil
	}
	s.closed = true
	if !s.started {
		// 上游一个内容块都没给（空流）：仍发一条合规的最小事件序列，
		// 让客户端拿到结构完整的响应而不是悬挂连接。
		if err := writeSSE(sink, anthropicSSEEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": s.id, "type": "message", "role": "assistant", "model": s.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": s.pt, "output_tokens": s.ct},
			},
		})); err != nil {
			return s.pt, s.ct, err
		}
	}
	if err := s.closeNonToolBlock(sink); err != nil {
		return s.pt, s.ct, err
	}
	// 关闭所有已启动的工具块。
	for _, tb := range s.toolBlocks {
		if !tb.started {
			continue
		}
		if err := writeSSE(sink, anthropicSSEEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": tb.anthropicIndex,
		})); err != nil {
			return s.pt, s.ct, err
		}
	}
	delta := s.pendingDelta
	if delta == nil {
		delta = &anthropicDeltaPayload{StopReason: "end_turn"}
	}
	delta.Usage = anthropicUsage{InputTokens: s.pt, OutputTokens: s.ct}
	if err := writeSSE(sink, anthropicSSEEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": delta.StopReason, "stop_sequence": nil},
		"usage": delta.Usage,
	})); err != nil {
		return s.pt, s.ct, err
	}
	if err := writeSSE(sink, anthropicSSEEvent("message_stop", map[string]any{"type": "message_stop"})); err != nil {
		return s.pt, s.ct, err
	}
	return s.pt, s.ct, nil
}

// writeSSE 把事件帧写给 sink。
func writeSSE(sink io.Writer, frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	_, err := sink.Write(frame)
	return err
}

// OpenChatStreamAsAnthropic 打开 OpenAI 上游流并包装成 Anthropic 事件流
//（/v1/messages + OpenAI 协议上游的流式路径）。down 是已转换的 OpenAI 请求体
//（prepareStreamBody 会强制 stream + include_usage）。
func (m *Manager) OpenChatStreamAsAnthropic(ctx context.Context, rt Route, down []byte,
	upstreamModel string, firstTokenTimeout time.Duration) (*anthropicFromOpenAIStream, error) {
	body, err := prepareStreamBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	st, err := m.openStream(ctx, rt, "chat/completions", body, firstTokenTimeout, chatUsageFromChunk)
	if err != nil {
		return nil, err
	}
	return &anthropicFromOpenAIStream{st: st,
		s: &openAIToAnthropicState{toolBlocks: map[int]*toolBlockState{}}}, nil
}

