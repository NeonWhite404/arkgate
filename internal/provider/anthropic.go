// Anthropic 协议转换桥。
//
// 下游永远是 OpenAI 格式（/v1/chat/completions）；当模型的上游协议是
// anthropic（Model.Provider="anthropic"）时，网关在这里做双向转换：
//
//	OpenAI chat 请求 ──► Anthropic POST {base}/v1/messages
//	Anthropic 响应   ──► OpenAI chat.completion 响应
//	Anthropic SSE    ──► OpenAI chat.completion.chunk SSE
//
// 供应商类型已从账号下沉到模型（同一上游主机可同时供应 gpt 系与 claude 系），
// 账号只提供 base_url 与密钥；模型声明协议。非流式走官方 anthropic-sdk-go
//（认证头 X-Api-Key、anthropic-version、重试与错误类型都由 SDK 负责），
// 流式与 OpenAI 路径同架构：自研 HTTP 拿原始 SSE，在 Pump 里逐事件转换——
// Anthropic 的事件流与 OpenAI chunk 不同构，字节透传不成立，必须重写。
//
// 转换边界（v1）：支持 system/developer 消息抽取、多轮对话（合并连续同角色）、
// text 内容分段、temperature/top_p/stop；**不支持 tools / function calling 与
// 图像输入**——检测到即报 400（静默丢弃会让下游误以为内容已送达）。
// 响应中的 thinking（推理）块丢弃：OpenAI 格式没有对应字段。
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
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultAnthropicMaxTokens 是 Anthropic 上游的 max_tokens 兜底默认值：
// Anthropic 协议强制要求该字段（缺了必 400），请求未携带且目录也未设置
// 模型输出上限时使用。
const DefaultAnthropicMaxTokens = 8192

// anthropicVersion 是 Anthropic API 的版本头（SDK 请求自动带，自研流式路径手动带）。
const anthropicVersion = "2023-06-01"

// MessageStream 是「已打开、可泵出」的上游流抽象：OpenAI 路径的原样字节流
//（*Stream）与 Anthropic 路径的转换流（*AnthropicStream）共用同一签名，
// 网关据此统一处理首字节重试与用量结算。
type MessageStream interface {
	Pump(sink io.Writer) (pt, ct int64, err error)
	Close()
}

// ─────────────────────────── 请求转换（OpenAI → Anthropic） ───────────────────────────

// openAIChatRequest 只解析转换需要的字段（其余字段对 Anthropic 无对应语义，忽略）。
type openAIChatRequest struct {
	Messages    []openAIMessage  `json:"messages"`
	Temperature *float64         `json:"temperature"`
	TopP        *float64         `json:"top_p"`
	Stop        json.RawMessage  `json:"stop"` // string 或 []string
	Tools       json.RawMessage  `json:"tools"`
	ToolChoice  json.RawMessage  `json:"tool_choice"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"` // string | content parts 数组 | null
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	Name       string           `json:"name"`
	ToolCallID string           `json:"tool_call_id"` // role=tool 消息的结果归属
}

// openAIToolCall 是 OpenAI 消息里的工具调用（assistant 发起 / 响应返回共用）。
// Index 仅为流式增量（OpenAI tool_calls 数组元素的下标）使用。
type openAIToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function openAIToolCallFn `json:"function"`
}

// ptr 返回值指针（流式 tool_calls 增量的 index 字段用）。
func ptr(i int) *int { return &i }

type openAIToolCallFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openAITextPart 是 content 数组元素的宽松形态：只关心 text 字段。
type openAITextPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// anthropicMessagesRequest 是发给上游的 /v1/messages 请求体（手拼结构而非
// SDK 类型化参数：这里的字段集合是我们转换语义的唯一真源）。
type anthropicMessagesRequest struct {
	Model         string                `json:"model"`
	MaxTokens     int64                 `json:"max_tokens"`
	System        string                `json:"system,omitempty"`
	Messages      []anthropicMessage    `json:"messages"`
	Temperature   *float64              `json:"temperature,omitempty"`
	TopP          *float64              `json:"top_p,omitempty"`
	StopSequences []string              `json:"stop_sequences,omitempty"`
	Tools         []anthropicToolDef    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage       `json:"tool_choice,omitempty"`
	Stream        bool                  `json:"stream"`
}

// anthropicToolDef 是 Anthropic 的工具声明（OpenAI parameters ↔ input_schema）。
type anthropicToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock 是多态内容块：text / image / tool_use / tool_result。
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	// image
	Source *anthropicImageSource `json:"source,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ConversionError 表示下游请求内容无法转换为上游协议（tools、图像输入等）。
// 这是客户端侧问题：网关应以 400 呈现、不跨叶子重试、不计入端点熔断。
type ConversionError struct{ Msg string }

func (e *ConversionError) Error() string { return e.Msg }

func convErr(format string, args ...any) error {
	return &ConversionError{Msg: fmt.Sprintf(format, args...)}
}

// AnthropicRequest 把 OpenAI chat 请求体转换成 Anthropic messages 请求体。
// maxTokens 由网关解析好传入（Anthropic 必填，见 gateway.anthropicMaxTokens）；
// stream 由流式路径置 true。
// tools 完整映射（声明/决策/调用/结果四段），保证 OpenAI 下游的 agent 客户端
// 在 Anthropic 上游上照常工作——网关对两侧都是透明的。
func AnthropicRequest(down []byte, upstreamModel string, maxTokens int64, stream bool) ([]byte, error) {
	var req openAIChatRequest
	if err := json.Unmarshal(down, &req); err != nil {
		return nil, convErr("请求体不是合法的 OpenAI chat 格式")
	}

	out := anthropicMessagesRequest{
		Model:       upstreamModel,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      stream,
	}

	// 工具声明：function.parameters ↔ input_schema（schema 轻量清洗，参照
	// cc-switch：根级补 object 类型，剔除 format:uri 等严格校验器会拒的修饰）。
	if len(req.Tools) > 0 && !isJSONNull(req.Tools) {
		var tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(req.Tools, &tools) != nil {
			return nil, convErr("tools 参数无法解析")
		}
		for _, t := range tools {
			if t.Type != "" && t.Type != "function" {
				return nil, convErr("暂不支持的工具类型 %s（仅 function）", t.Type)
			}
			schema := t.Function.Parameters
			if len(schema) == 0 || isJSONNull(schema) {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			out.Tools = append(out.Tools, anthropicToolDef{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: cleanJSONSchema(schema),
			})
		}
	}
	// 工具选择：required ↔ any（Anthropic 无 required 字面值）；none 上游
	// 无对应语义，回落 auto（工具在声明里存在但模型可自行决定不调用）。
	if len(req.ToolChoice) > 0 && !isJSONNull(req.ToolChoice) {
		var tc any
		if json.Unmarshal(req.ToolChoice, &tc) != nil {
			return nil, convErr("tool_choice 参数无法解析")
		}
		switch v := tc.(type) {
		case string:
			switch v {
			case "required":
				out.ToolChoice = json.RawMessage(`{"type":"any"}`)
			case "none":
				out.ToolChoice = json.RawMessage(`{"type":"auto"}`)
			default: // auto
				out.ToolChoice = json.RawMessage(`{"type":"auto"}`)
			}
		case map[string]any:
			if v["type"] == "function" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ := fn["name"].(string)
					sel, _ := json.Marshal(map[string]any{"type": "tool", "name": name})
					out.ToolChoice = sel
				}
			}
		}
	}

	var system []string
	var msgs []anthropicMessage
	appendBlocks := func(role string, blocks []anthropicBlock) {
		// 空 blocks 跳过：openai 允许 content 为空串/null，但 Anthropic 的
		// content:null 会被上游拒绝——不能生成空内容消息。
		if len(blocks) == 0 {
			return
		}
		// Anthropic 严格要求 user/assistant 交替：合并连续同角色消息。
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			return
		}
		msgs = append(msgs, anthropicMessage{Role: role, Content: blocks})
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := openAIText(m.Content)
			if err != nil {
				return nil, convErr("第 %d 条消息：%v", i+1, err)
			}
			if text != "" {
				system = append(system, text)
			}
		case "user", "assistant":
			blocks, err := openAIContentBlocks(m.Content)
			if err != nil {
				return nil, convErr("第 %d 条消息：%v", i+1, err)
			}
			// 助手已发起的工具调用 → tool_use 块。
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
				if len(input) == 0 || !json.Valid([]byte(input)) {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, anthropicBlock{Type: "tool_use",
					ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			appendBlocks(m.Role, blocks)
		case "tool":
			// 工具结果 → user 消息里的 tool_result 块（OpenAI 的 tool 角色
			// 在 Anthropic 协议中表达为「下一轮 user 携带 tool_result」）。
			// content 归一为 JSON 字符串：tool_result.content 只接受字符串或
			// 块数组，直接透传原文在空值/非 JSON 文本时会产生非法 JSON。
			var contentText string
			if len(m.Content) > 0 && !isJSONNull(m.Content) {
				if json.Unmarshal(m.Content, &contentText) != nil {
					contentText = strings.TrimSpace(string(m.Content))
				}
			}
			if contentText == "" {
				contentText = "(empty)"
			}
			encoded, merr := json.Marshal(contentText)
			if merr != nil {
				return nil, convErr("第 %d 条消息：工具结果无法编码", i+1)
			}
			appendBlocks("user", []anthropicBlock{{Type: "tool_result",
				ToolUseID: m.ToolCallID, Content: encoded}})
		default:
			return nil, convErr("不支持的消息角色 %s", m.Role)
		}
	}
	if len(msgs) == 0 {
		return nil, convErr("没有可发送的消息内容")
	}
	out.Messages = msgs
	if len(system) > 0 {
		out.System = strings.Join(system, "\n\n")
	}
	if req.Stop != nil && !isJSONNull(req.Stop) {
		var one string
		if json.Unmarshal(req.Stop, &one) == nil {
			out.StopSequences = []string{one}
		} else {
			var many []string
			if json.Unmarshal(req.Stop, &many) == nil {
				out.StopSequences = many
			}
		}
	}
	return json.Marshal(out)
}

// cleanJSONSchema 轻量清洗工具参数 schema（参照 cc-switch 的 clean_schema）：
// 根级补 type:object / properties，剔除 format:uri（严格 schema 校验器会拒绝），
// 递归处理 properties 与 items。清洗失败时原样返回（宁可上游报错，不静默丢字段）。
func cleanJSONSchema(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	cleanSchemaValue(&v, true)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func cleanSchemaValue(v *any, isRoot bool) {
	obj, ok := (*v).(map[string]any)
	if !ok {
		return
	}
	_, hasType := obj["type"]
	if isRoot && !hasType {
		obj["type"] = "object"
		if _, hasProps := obj["properties"]; !hasProps {
			obj["properties"] = map[string]any{}
		}
	}
	if obj["format"] == "uri" {
		delete(obj, "format")
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		for k := range props {
			pv := props[k]
			cleanSchemaValue(&pv, false)
			props[k] = pv
		}
	}
	if items, ok := obj["items"]; ok {
		cleanSchemaValue(&items, false)
		obj["items"] = items
	}
}

// openAIText 解析字符串形态的 content（system 等角色用）。
func openAIText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errors.New("content 需要是字符串")
	}
	return s, nil
}

// openAIContentBlocks 把 OpenAI 的 content（字符串或分段数组）转换成
// Anthropic 内容块：text 收集、image_url 转 image 块（data URI 拆 base64、
// http URL 用 url source）；其余分段类型明确拒绝，绝不静默丢弃。
func openAIContentBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var parts []openAITextPart
	if json.Unmarshal(raw, &parts) == nil {
		var blocks []anthropicBlock
		for _, p := range parts {
			switch p.Type {
			case "", "text":
				if p.Text != "" {
					blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
				}
			case "image_url":
				if p.ImageURL == nil || p.ImageURL.URL == "" {
					return nil, errors.New("image_url 分段缺少 url")
				}
				blocks = append(blocks, anthropicBlock{Type: "image",
					Source: imageSourceFromURL(p.ImageURL.URL)})
			default:
				return nil, errors.New("暂不支持 " + p.Type + " 内容分段")
			}
		}
		return blocks, nil
	}
	return nil, errors.New("content 格式无法识别")
}

// imageSourceFromURL 把 OpenAI 的 image_url 拆成 Anthropic image source：
// data URI → base64 source；http(s) URL → url source。
func imageSourceFromURL(u string) *anthropicImageSource {
	if mt, data, ok := strings.Cut(u, ","); ok && strings.HasPrefix(mt, "data:") && strings.Contains(mt, ";base64") {
		media := strings.TrimPrefix(mt, "data:")
		mediaType := strings.TrimSuffix(media, ";base64")
		if mediaType == "" {
			mediaType = "image/png"
		}
		return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}
	}
	return &anthropicImageSource{Type: "url", URL: u}
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// ─────────────────────────── 响应转换（Anthropic → OpenAI） ───────────────────────────

type anthropicMessageResponse struct {
	ID         string           `json:"id"`
	Model      string           `json:"model"`
	Role       string           `json:"role"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      anthropicUsage   `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// openAIChatResponse 是转换产出的非流式响应（字段保持 OpenAI 客户端必需集）。
type openAIChatResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []openAIResponseChoice `json:"choices"`
	Usage   openAIUsage            `json:"usage"`
}

type openAIResponseChoice struct {
	Index        int          `json:"index"`
	Message      openAIMsgOut `json:"message"`
	FinishReason string       `json:"finish_reason"`
}

type openAIMsgOut struct {
	Role      string           `json:"role"`
	Content   any              `json:"content"` // 字符串；纯工具调用时为 null（cc-switch 同款口径）
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// AnthropicResponseToOpenAI 把 Anthropic 的非流式 message 响应转换成
// OpenAI chat.completion 响应。usage 与计费口径：input→prompt、output→completion。
func AnthropicResponseToOpenAI(raw []byte, fallbackModel string) ([]byte, *TextUsage, error) {
	var msg anthropicMessageResponse
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, nil, errors.New("上游响应不是合法的 Anthropic message 格式")
	}
	var sb strings.Builder
	var toolCalls []openAIToolCall
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_use":
			// input 对象 → arguments 字符串（OpenAI 形态）；空 input 用 {}。
			args := strings.TrimSpace(string(b.Input))
			if len(args) == 0 || !json.Valid([]byte(args)) {
				args = "{}"
			}
			toolCalls = append(toolCalls, openAIToolCall{ID: b.ID, Type: "function",
				Function: openAIToolCallFn{Name: b.Name, Arguments: args}})
		default:
			// thinking 等块丢弃：OpenAI 格式无对应字段，拼进正文会污染对话。
		}
	}
	model := msg.Model
	if model == "" {
		model = fallbackModel
	}
	choice := openAIResponseChoice{Index: 0,
		FinishReason: anthropicFinishReason(msg.StopReason, len(toolCalls) > 0)}
	if len(toolCalls) > 0 {
		// 纯工具调用：content 置 null（cc-switch 同款口径，兼容严格客户端）。
		choice.Message = openAIMsgOut{Role: "assistant", Content: nil, ToolCalls: toolCalls}
	} else {
		choice.Message = openAIMsgOut{Role: "assistant", Content: sb.String()}
	}
	out := openAIChatResponse{
		ID:      msg.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIResponseChoice{choice},
		Usage: openAIUsage{
			PromptTokens:     msg.Usage.InputTokens,
			CompletionTokens: msg.Usage.OutputTokens,
			TotalTokens:      msg.Usage.InputTokens + msg.Usage.OutputTokens,
		},
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return data, &TextUsage{PromptTokens: msg.Usage.InputTokens, CompletionTokens: msg.Usage.OutputTokens}, nil
}

// anthropicFinishReason 把 Anthropic stop_reason 映射成 OpenAI finish_reason。
// 有工具调用时优先 tool_calls（stop_reason 可能仍是 end_turn）。
func anthropicFinishReason(reason string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_calls"
	}
	switch reason {
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		// end_turn / stop_sequence / 空值统一映射为 stop。
		return "stop"
	}
}

// openAIErrorBody 把上游错误摘要包装成 OpenAI 错误结构——两种协议的错误
// 对下游呈现一致，客户端按统一形状解析。
func openAIErrorBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": "upstream_error", "message": msg},
	})
	return b
}

// anthropicErrorMessage 从 Anthropic 错误体里尽量提取人类可读的消息。
func AnthropicErrorMessage(body []byte, fallback string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	var generic struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &generic) == nil && generic.Message != "" {
		return generic.Message
	}
	summary := strings.TrimSpace(string(body))
	if summary == "" {
		return fallback
	}
	if len(summary) > 300 {
		summary = summary[:300] + "…"
	}
	return summary
}

// ─────────────────────────── 流式转换（Anthropic SSE → OpenAI chunk） ───────────────────────────

// anthropicStreamState 累积一条流的身份与用量，把 Anthropic 事件逐个转成
// OpenAI chunk。事件口径（data JSON 的 type 字段）：
//
//	message_start        → 记录 id/model/input_tokens，发首个 role chunk
//	content_block_start  → tool_use 块开启：发 tool_calls 起始 chunk（id/name）
//	content_block_delta  → text_delta 发 content chunk；input_json_delta 发
//	                       tool_calls arguments 增量；thinking_delta 丢弃
//	message_delta        → 记录 output_tokens/stop_reason，发 finish chunk
//	message_stop         → 发 [DONE]，流结束
//	error                → 转为错误（网关用 SSE error 帧收尾）
//	ping / block_stop    → 忽略
type anthropicStreamState struct {
	id         string
	model      string
	pt         int64
	ct         int64
	stopReason string
	roleSent   bool
	done       bool
	// 工具调用映射状态：content_block_start(tool_use) 分配 OpenAI tool index，
	// 其后的 input_json_delta 按当前块号补 arguments 片段。上游对空 input 可能
	// 不发任何 input_json_delta——块关闭时补一段 "{}"，避免客户端拿到空 arguments。
	curTool      bool
	curToolID    string
	curToolName  string
	curToolIndex int
	curSentArgs  bool
	nextToolIdx  int
}

// openAIChunk 是流式产出的 chat.completion.chunk。
type openAIChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []openAIChunkChoice `json:"choices"`
}

type openAIChunkChoice struct {
	Index        int          `json:"index"`
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type openAIChunkDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

func (s *anthropicStreamState) emit(choices []openAIChunkChoice) ([]byte, error) {
	return json.Marshal(openAIChunk{
		ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(),
		Model: s.model, Choices: choices,
	})
}

// convertEvent 转换单个 Anthropic SSE 事件，返回要写给客户端的字节
//（可能为空 = 该事件无需产出）。
func (s *anthropicStreamState) convertEvent(data []byte) ([]byte, error) {
	var evt struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Index        int `json:"index"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage  *anthropicUsage `json:"usage"`
		ErrObj *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil, fmt.Errorf("Anthropic 事件无法解析: %w", err)
	}
	switch evt.Type {
	case "message_start":
		if evt.Message != nil {
			s.id = evt.Message.ID
			if evt.Message.Model != "" {
				s.model = evt.Message.Model
			}
			s.pt = evt.Message.Usage.InputTokens
		}
		if s.model == "" {
			s.model = "anthropic"
		}
		s.roleSent = true
		return s.emit([]openAIChunkChoice{{Index: 0, Delta: openAIChunkDelta{Role: "assistant"}}})
	case "content_block_start":
		// tool_use 块：分配 OpenAI tool index，先发 id/name（arguments 空串起步）。
		if evt.ContentBlock != nil && evt.ContentBlock.Type == "tool_use" {
			s.curTool = true
			s.curToolID = evt.ContentBlock.ID
			s.curToolName = evt.ContentBlock.Name
			s.curToolIndex = s.nextToolIdx
			s.curSentArgs = false
			s.nextToolIdx++
			return s.emit([]openAIChunkChoice{{Index: 0, Delta: openAIChunkDelta{
				ToolCalls: []openAIToolCall{{Index: ptr(s.curToolIndex), ID: s.curToolID,
					Type: "function", Function: openAIToolCallFn{Name: s.curToolName}}},
			}}})
		}
		return nil, nil
	case "content_block_delta":
		if evt.Delta == nil {
			return nil, nil
		}
		switch evt.Delta.Type {
		case "text_delta":
			if evt.Delta.Text == "" {
				return nil, nil
			}
			return s.emit([]openAIChunkChoice{{Index: 0, Delta: openAIChunkDelta{Content: evt.Delta.Text}}})
		case "input_json_delta":
			if !s.curTool || evt.Delta.PartialJSON == "" {
				return nil, nil
			}
			s.curSentArgs = true
			return s.emit([]openAIChunkChoice{{Index: 0, Delta: openAIChunkDelta{
				ToolCalls: []openAIToolCall{{Index: ptr(s.curToolIndex),
					Function: openAIToolCallFn{Arguments: evt.Delta.PartialJSON}}},
			}}})
		}
		return nil, nil // thinking_delta / signature_delta 等丢弃
	case "content_block_stop":
		// 块关闭。上游对空 input（{}）可能不发任何 input_json_delta——补一段
		// "{}"，否则 OpenAI 客户端拿到空 arguments（json 解析会失败）。
		if s.curTool && !s.curSentArgs {
			s.curTool = false
			return s.emit([]openAIChunkChoice{{Index: 0, Delta: openAIChunkDelta{
				ToolCalls: []openAIToolCall{{Index: ptr(s.curToolIndex),
					Function: openAIToolCallFn{Arguments: "{}"}}},
			}}})
		}
		s.curTool = false
		return nil, nil
	case "message_delta":
		if evt.Delta != nil {
			s.stopReason = evt.Delta.StopReason
		}
		if evt.Usage != nil && evt.Usage.OutputTokens > 0 {
			s.ct = evt.Usage.OutputTokens
		}
		fr := anthropicFinishReason(s.stopReason, s.nextToolIdx > 0)
		return s.emit([]openAIChunkChoice{{Index: 0, Delta: openAIChunkDelta{}, FinishReason: &fr}})
	case "message_stop":
		s.done = true
		return []byte("data: [DONE]\n\n"), nil
	case "error":
		msg := "上游流式错误"
		if evt.ErrObj != nil && evt.ErrObj.Message != "" {
			msg = evt.ErrObj.Message
		}
		return nil, errors.New(msg)
	default:
		return nil, nil // ping 等其它事件
	}
}

// AnthropicStream 是一条已打开（已收到 message_start）的 Anthropic 上游流，
// Pump 时把事件流转换成 OpenAI chunk 写给 sink。
type AnthropicStream struct {
	resp   *http.Response
	rdr    *bufio.Reader
	first  []byte // openStream 已缓冲的首个 data 载荷（message_start）
	st     *anthropicStreamState
	cancel context.CancelFunc // 首 token 超时场景创建的子 ctx（可能为 nil）
}

// Close 关闭流并释放底层资源。
func (s *AnthropicStream) Close() {
	if s == nil {
		return
	}
	if s.resp != nil && s.resp.Body != nil {
		s.resp.Body.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// Pump 把剩余事件转换为 OpenAI chunk 写给 sink（含已缓冲的首个事件），
// 返回流中提取的用量（input→prompt，output→completion）。
func (s *AnthropicStream) Pump(sink io.Writer) (pt, ct int64, err error) {
	st := s.st
	line := s.first
	for {
		if len(line) > 0 {
			out, cerr := st.convertEvent(line)
			if cerr != nil {
				return st.pt, st.ct, cerr
			}
			if len(out) > 0 {
				if _, werr := sink.Write(out); werr != nil {
					return st.pt, st.ct, werr
				}
			}
			if st.done {
				return st.pt, st.ct, nil
			}
		}
		line, err = s.rdr.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 上游没发 message_stop 就断流：与 OpenAI 路径对齐，
				// 不伪造收尾帧，原样结束（已完成的内容有效）。
				return st.pt, st.ct, nil
			}
			return st.pt, st.ct, err
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // event: / 注释 / 空行
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		line = payload
	}
}

// anthropicEndpoint 解析 {base}/v1/messages 完整 URL。与 SDK 的相对路径
// 语义一致（url 引用解析）：base 以 /v1 结尾（OpenAI 习惯）或裸域名都能
// 拼出正确地址，因此同一账号的 base_url 可同时服务两种协议的模型。
func anthropicEndpoint(base string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return strings.TrimRight(base, "/") + "/v1/messages"
	}
	ref, err := url.Parse("v1/messages")
	if err != nil {
		return strings.TrimRight(base, "/") + "/v1/messages"
	}
	return u.ResolveReference(ref).String()
}

// anthropicClient 构造非流式调用用的 SDK 客户端（与 openai 路径一致：
// 同叶子瞬时重试 1 次，跨叶子重试由网关负责；超时由调用方 ctx 施加）。
//
// 注意 SDK 的 base URL 约定：WithBaseURL 会给非空路径强制补尾斜杠，相对
// 路径 "v1/messages" 在 "/v1/" 结尾的 base 上会拼出 /v1/v1/messages。因此
// 这里剥掉 /v1 后缀再交给 SDK（裸域名 / 子路径 base 均得到正确的最终地址）。
func anthropicClient(rt Route, httpc *http.Client) *anthropic.Client {
	base := strings.TrimSuffix(strings.TrimRight(rt.BaseURL, "/"), "/v1")
	client := anthropic.NewClient(
		option.WithAPIKey(rt.Key),
		option.WithBaseURL(base),
		option.WithHTTPClient(httpc),
		option.WithMaxRetries(1),
	)
	return &client
}

// AnthropicChat 转发非流式 chat（Anthropic 协议）：请求/响应双向转换，
// 上游错误包装成 OpenAI 错误结构透传状态码。
// maxTokens 为已解析的 max_tokens（Anthropic 必填）；timeout 为整体超时（0 = 不限）。
func (m *Manager) AnthropicChat(ctx context.Context, rt Route, down []byte, upstreamModel string,
	maxTokens int64, timeout time.Duration) ([]byte, *TextUsage, error) {
	body, err := AnthropicRequest(down, upstreamModel, maxTokens, false)
	if err != nil {
		return nil, nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var (
		captured []byte // 上游响应原始字节（含错误响应）
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
			return nil, nil, &HTTPError{Code: status, Body: openAIErrorBody(AnthropicErrorMessage(captured, err.Error()))}
		}
		var ae *anthropic.Error
		if errors.As(err, &ae) && ae.StatusCode >= 400 {
			return nil, nil, &HTTPError{Code: ae.StatusCode, Body: openAIErrorBody(ae.Error())}
		}
		return nil, nil, err
	}
	out, usage, cerr := AnthropicResponseToOpenAI(raw, upstreamModel)
	if cerr != nil {
		return nil, nil, cerr
	}
	return out, usage, nil
}

// OpenAnthropicChatStream 打开 Anthropic 协议的流式请求并等待首个数据事件
//（message_start）。成功返回后未向客户端写过任何字节，失败可换叶子重试；
// 首字节超时语义与 OpenAI 路径一致（CAS 抢占，宁弃流不杀好流）。
func (m *Manager) OpenAnthropicChatStream(ctx context.Context, rt Route, down []byte, upstreamModel string,
	maxTokens int64, firstTokenTimeout time.Duration) (_ *AnthropicStream, err error) {

	body, err := AnthropicRequest(down, upstreamModel, maxTokens, true)
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
				cancel() // 失败路径释放子 ctx；成功路径由 Stream.Close 负责
			}
		}()
		timer = time.AfterFunc(firstTokenTimeout, func() {
			if fired.CompareAndSwap(false, true) {
				cancel()
			}
		})
		defer timer.Stop()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, anthropicEndpoint(rt.BaseURL), bytes.NewReader(body))
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
		return nil, &HTTPError{Code: resp.StatusCode, Body: openAIErrorBody(AnthropicErrorMessage(respBody, "上游返回 "+strconv.Itoa(resp.StatusCode)))}
	}

	// 等首个 data 行（message_start）：期间的任何失败都可安全重试。
	reader := bufio.NewReader(resp.Body)
	var first []byte
	for {
		line, rerr := reader.ReadBytes('\n')
		if rerr != nil && len(bytes.TrimSpace(line)) == 0 {
			resp.Body.Close()
			if timer != nil && fired.Load() {
				return nil, fmt.Errorf("%w（等待 %s 无输出）", ErrFirstToken, firstTokenTimeout)
			}
			return nil, fmt.Errorf("上游未返回任何数据: %w", rerr)
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				first = payload
				break
			}
		}
		// event: 行 / ping 注释行：继续读（仍在首 token 等待窗口内）
		if timer != nil && fired.Load() {
			resp.Body.Close()
			return nil, fmt.Errorf("%w（等待 %s 无输出）", ErrFirstToken, firstTokenTimeout)
		}
	}
	if timer != nil {
		// 首个数据事件到手。CAS 失败说明定时器恰好同时触发并已取消请求——
		// 宁可放弃这条已到手的流，也不让它中途被取消。
		if !fired.CompareAndSwap(false, true) {
			resp.Body.Close()
			return nil, fmt.Errorf("%w（首字节与超时同时到达）", ErrFirstToken)
		}
		timer.Stop()
	}
	st := &anthropicStreamState{model: upstreamModel}
	// 首事件必须是 message_start（错误事件在 convertEvent 里转为错误返回）。
	if _, err := st.convertEvent(first); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return &AnthropicStream{resp: resp, rdr: reader, first: first, st: st, cancel: cancel}, nil
}
