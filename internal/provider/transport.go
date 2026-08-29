package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Route 是网关选叶后解析出的「一条可发送路由」：
// 供应商定义 + 最终 base URL + 不透明 Key。
type Route struct {
	Def     Def
	BaseURL string
	Key     string // 不透明字符串：不校验前缀/格式，原样进 Authorization
}

// TextUsage 文本调用计量（chat 与 responses 统一折算到 prompt/completion）。
type TextUsage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// ImageUsage 图像调用计量：张数为主；个别供应商（gpt-image-1）附带 token 用量。
type ImageUsage struct {
	Count            int64
	PromptTokens     int64
	CompletionTokens int64
}

// HTTPError 上游返回非 2xx；Body 原样保留供网关透传。
type HTTPError struct {
	Code int
	Body []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream http %d: %s", e.Code, truncate(e.Body, 300))
}

// AsHTTPError 判断 err 是否为上游 HTTP 错误。
func AsHTTPError(err error) (*HTTPError, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

// requestFaultHints 上游「请求本身超限/非法」类错误体特征（宽松子串匹配，
// 仅对 400/413/422 生效）。判定宁可偏宽：误判的代价只是少计一次端点失败，
// 漏判的代价是把客户端错误累加成健康端点的熔断。
var requestFaultHints = []string{
	"context length", "maximum context", "context_length", "context window",
	"input token", "prompt token", "prompt is too long", "prompt too long",
	"too long", "token limit", "length limit", "max_tokens", "max_completion_tokens",
	"max_output_tokens", "supports at most", "exceed",
}

// IsRequestFault 判断 err 是否由客户端请求自身问题导致（上下文超限、
// max_tokens 超上限、参数非法等 4xx）。这类错误是请求方的错，不是端点故障，
// 调用方应据此跳过端点熔断计数（透传行为不变）。
func IsRequestFault(err error) bool {
	he, ok := AsHTTPError(err)
	if !ok {
		return false
	}
	switch he.Code {
	case 400, 413, 422:
	default:
		return false
	}
	body := strings.ToLower(string(he.Body))
	for _, h := range requestFaultHints {
		if strings.Contains(body, h) {
			return true
		}
	}
	return false
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// prepareBody 把下游原始请求体改装为上游请求体：整体透传，仅替换 model 为
// 上游模型标识（不透明字符串：ep-xxx / gpt-4o / doubao-xxx 等）。
// 其余字段（含各供应商私有参数）一律原样保留，避免猜测性改写误伤。
func prepareBody(down []byte, upstreamModel string) ([]byte, error) {
	raw := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(down))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	raw["model"] = upstreamModel
	return json.Marshal(raw)
}

// prepareStreamBody 在 prepareBody 基础上强制流式 + include_usage
// （用量从 final chunk 提取；保留下游 stream_options 的其它字段）。
func prepareStreamBody(down []byte, upstreamModel string) ([]byte, error) {
	body, err := prepareBody(down, upstreamModel)
	if err != nil {
		return nil, err
	}
	raw := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // 保持大整数（seed 等）原样往返
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	raw["stream"] = true
	so, _ := raw["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	raw["stream_options"] = so
	return json.Marshal(raw)
}

// ─────────────────────────── 用量提取（原始字节轻量抽取，不依赖 SDK 版本） ───────────────────────────

// ExtractChatUsage 从 chat/completions 响应提取 usage。
func ExtractChatUsage(raw []byte) *TextUsage {
	u := &TextUsage{}
	var parsed struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Usage != nil {
		u.PromptTokens = parsed.Usage.PromptTokens
		u.CompletionTokens = parsed.Usage.CompletionTokens
	}
	return u
}

// ExtractResponsesUsage 从 responses 响应提取 usage（input/output → prompt/completion）。
func ExtractResponsesUsage(raw []byte) *TextUsage {
	u := &TextUsage{}
	var parsed struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Usage != nil {
		u.PromptTokens = parsed.Usage.InputTokens
		u.CompletionTokens = parsed.Usage.OutputTokens
	}
	return u
}

// ExtractImageUsage 从 images/generations 响应提取张数与可能存在的 token 用量。
func ExtractImageUsage(raw []byte) *ImageUsage {
	u := &ImageUsage{}
	var parsed struct {
		Data  []json.RawMessage `json:"data"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		u.Count = int64(len(parsed.Data))
		if parsed.Usage != nil {
			u.PromptTokens = parsed.Usage.InputTokens
			u.CompletionTokens = parsed.Usage.OutputTokens
		}
	}
	return u
}

// ExtractN 从请求体取 n（图像张数，默认 1），供流式图像计量。
func ExtractN(body []byte) int64 {
	var v struct {
		N int64 `json:"n"`
	}
	if json.Unmarshal(body, &v) == nil && v.N > 0 {
		return v.N
	}
	return 1
}
