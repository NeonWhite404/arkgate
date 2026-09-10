package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"strings"
)

// Route 是网关选叶后解析出的「一条可发送路由」：
// 供应商定义 + 最终 base URL + 不透明 Key。
type Route struct {
	Def     Def
	BaseURL string
	Key     string // 不透明字符串：不校验前缀/格式，原样进 Authorization
	Headers map[string]string
}

// forbiddenRequestHeaders 是网关必须独占的头：认证、hop-by-hop、以及协议层
// 强制字段。映射级自定义头不能覆盖它们——否则能冒充认证、破坏分帧或改变协议。
var forbiddenRequestHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "x-api-key": true,
	"host": true, "content-length": true, "transfer-encoding": true,
	"connection": true, "keep-alive": true, "te": true, "trailer": true,
	"upgrade": true, "proxy-connection": true,
	"content-type": true, "accept": true, "anthropic-version": true,
}

// validHeaderNameToken 校验 header 名是合法 RFC token（白名单字符 + 数字字母）。
// 只拦换行不够：空格 / NUL / 其它控制字符会在出站 transport 层才炸，或悄悄被丢。
func validHeaderNameToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '!', c == '#', c == '$', c == '%', c == '&', c == '\'',
			c == '*', c == '+', c == '-', c == '.', c == '^', c == '_',
			c == '`', c == '|', c == '~':
		default:
			return false
		}
	}
	return true
}

// ValidateRequestHeaders 在保存前校验映射级请求头，确保：
//   - 名称是合法 token（拒绝空格 / 控制字符 / 冒号等）；
//   - 值不含 CR/LF/NUL（防请求拆分）；
//   - 不可覆盖认证 / 传输级 / 协议头；
//   - 两个仅在大小写上不同的键不会规范化成同一个头（否则出站结果不确定）。
func ValidateRequestHeaders(headers map[string]string) error {
	seen := make(map[string]string, len(headers))
	for name, value := range headers {
		trimmed := strings.TrimSpace(name)
		canonical := textproto.CanonicalMIMEHeaderKey(trimmed)
		if !validHeaderNameToken(trimmed) {
			return fmt.Errorf("请求头名称非法: %q", name)
		}
		if forbiddenRequestHeaders[strings.ToLower(canonical)] {
			return fmt.Errorf("请求头 %s 属于认证、传输级或协议头，禁止覆盖", canonical)
		}
		if prior, dup := seen[canonical]; dup {
			return fmt.Errorf("请求头 %s 与 %s 仅大小写不同，会规范化为同一头", name, prior)
		}
		seen[canonical] = name
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("请求头 %s 的值包含非法字符", canonical)
		}
	}
	return nil
}

// applyRequestHeaders 把映射级自定义头写入待发送请求。跳过禁止覆盖/非法的头
// 作为第二道防线（认证与协议头的正确性不依赖 admin 是否记得校验）。
func applyRequestHeaders(dst http.Header, headers map[string]string) {
	for name, value := range headers {
		canonical := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if !validHeaderNameToken(strings.TrimSpace(name)) || forbiddenRequestHeaders[strings.ToLower(canonical)] {
			continue
		}
		dst.Set(canonical, value)
	}
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
// max_tokens 超上限、参数非法、协议转换不支持的内容等 4xx）。这类错误是
// 请求方的错，不是端点故障，调用方应据此跳过端点熔断计数（透传行为不变）。
func IsRequestFault(err error) bool {
	var ce *ConversionError
	if errors.As(err, &ce) {
		return true // 协议转换拒绝的内容（tools/图像输入等）同样是请求方问题
	}
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

// isMaxTokensCompatibilityError 只识别上游明确要求用 max_completion_tokens
// 替代 max_tokens 的参数错误。不能把所有 400 都重写，否则会掩盖真实的客户端错误。
func isMaxTokensCompatibilityError(err error) bool {
	he, ok := AsHTTPError(err)
	if !ok || (he.Code != 400 && he.Code != 422) {
		return false
	}
	body := strings.ToLower(string(he.Body))
	return strings.Contains(body, "max_tokens") &&
		strings.Contains(body, "max_completion_tokens") &&
		(strings.Contains(body, "unsupported parameter") || strings.Contains(body, "not supported"))
}

// useMaxCompletionTokens 把 chat 请求的 max_tokens 迁移为
// max_completion_tokens。兼容上游已经同时收到两个字段的情况：删除旧字段，
// 保留调用方显式提供的 max_completion_tokens。
func useMaxCompletionTokens(body []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false
	}
	value, hasOld := raw["max_tokens"]
	if !hasOld {
		return body, false
	}
	if _, hasNew := raw["max_completion_tokens"]; !hasNew {
		raw["max_completion_tokens"] = value
	}
	delete(raw, "max_tokens")
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return out, true
}

// isThinkingCompatibilityError 识别上游明确要求用 reasoning_effort 替代 thinking
// 的参数错误（OpenAI 新模型不接受 Ark/DeepSeek 风格的 thinking 开关）。
// 只匹配错误体同时提到 thinking 与 reasoning_effort 的明确提示，避免误伤其它 400。
func isThinkingCompatibilityError(err error) bool {
	he, ok := AsHTTPError(err)
	if !ok || he.Code != 400 {
		return false
	}
	body := strings.ToLower(string(he.Body))
	return strings.Contains(body, "thinking") &&
		strings.Contains(body, "reasoning_effort") &&
		(strings.Contains(body, "not supported") || strings.Contains(body, "unsupported"))
}

// thinkingEffort 把 thinking.type 映射为 reasoning_effort。遵循 OpenAI 规范中
// reasoning_effort 的合法取值（none/minimal/low/medium/high/xhigh/max）：
// 同名值原样透传，不做降级或换算；仅 Ark/DeepSeek 风格的布尔开关
// （enabled/disabled/auto）做明确的语义桥接。
func thinkingEffort(raw json.RawMessage) string {
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	v := strings.ToLower(strings.TrimSpace(t.Type))
	switch v {
	// reasoning_effort 自身的合法取值：原样透传。
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return v
	// Ark/DeepSeek 布尔开关：enabled/auto/disabled 的明确语义。
	case "enabled":
		return "high"
	case "auto":
		return "medium"
	case "disabled":
		return "none"
	}
	return ""
}

// useReasoningEffort 把请求体的 thinking 字段迁移为 reasoning_effort。
// thinking 无法识别时仅删除该字段（不再发送上游拒绝的参数）。
func useReasoningEffort(body []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false
	}
	tv, hasThinking := raw["thinking"]
	if !hasThinking {
		return body, false
	}
	delete(raw, "thinking")
	if effort := thinkingEffort(tv); effort != "" {
		if b, merr := json.Marshal(effort); merr == nil {
			raw["reasoning_effort"] = b
		}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return out, true
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
