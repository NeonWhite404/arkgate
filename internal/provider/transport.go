package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// Route 是网关选叶后解析出的「一条可发送路由」：
// 供应商定义 + 最终 base URL + 不透明 Key。
type Route struct {
	Def     Def
	BaseURL string
	Key     string // 不透明字符串：不校验前缀/格式，原样进 Authorization
	Headers map[string]string

	// BodyDefaults 接入点级默认请求体参数：下游未提供时由 prepareBody 补上。
	// 与 Headers 同构——都是「管理员为这条接入点补的下游不会发的字段」。
	BodyDefaults map[string]json.RawMessage
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
	// 缓存 token（v13）：上游 prompt cache 的写入/读取量。两家口径不同，统一到这里：
	//   - OpenAI：prompt_tokens_details.cached_tokens → CacheReadTokens（OpenAI 不报写入量）
	//   - Anthropic：cache_creation_input_tokens → CacheCreationTokens
	//                       cache_read_input_tokens     → CacheReadTokens
	// 注意：Anthropic 的 input_tokens **不含**缓存部分，这里不做加法改写（保持上游原值，
	// 否则成本与 TPM 口径会被改变）；展示层需要总量时自行相加。
	CacheCreationTokens int64
	CacheReadTokens     int64
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

// requestTypeFaultHints 是「请求体字段类型/取值非法」类错误体特征。这类错误
// 由下游把参数写错类型引发（典型：stream 写成字符串 "false"），不是端点故障。
// 与 requestFaultHints 分开维护：这些特征在 5xx 上也可能是请求方问题——不少
// 上游（Go 系聚合器）把「JSON 解码失败」包装成 500 返回。
var requestTypeFaultHints = []string{
	"must be a boolean", "must be boolean", "expected a boolean", "expected boolean",
	"must be a string", "must be an integer", "must be a number",
	"cannot unmarshal", "unmarshal", "invalid type",
	"不是布尔", "必须是布尔值", "字段类型", "参数类型",
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
	body := strings.ToLower(string(he.Body))
	switch he.Code {
	case 400, 413, 422:
		for _, h := range requestFaultHints {
			if strings.Contains(body, h) {
				return true
			}
		}
		// 类型错误词条在 4xx 上同样成立（如 400 + "must be a boolean"）。
		for _, h := range requestTypeFaultHints {
			if strings.Contains(body, h) {
				return true
			}
		}
		return false
	case 500, 502:
		// 部分上游把「请求体 JSON 解码失败」包装成 5xx——这仍是请求方问题。
		// 只认明确的解码/类型失败短语，避免把真实的 5xx 故障误判为客户端问题。
		for _, h := range requestTypeFaultHints {
			if strings.Contains(body, h) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// IsClientCancel 判断错误是否由「下游客户端断开/取消」导致，而非上游故障。
//
// 网关把 r.Context() 一路传给上游调用，因此下游客户端主动断开（用户 Ctrl-C、
// SDK 取消、中间反代超时切断）会让上游请求以 context.Canceled 收尾。这既不是
// 端点故障、也不是请求内容问题：调用方应据此跳过端点熔断计数、终止重试循环
//（父 ctx 已死，换叶子重试必然立刻再次取消），并把日志单独归类。
func IsClientCancel(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	// 流式写 sink 失败（客户端已断开）同样归入此类。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "client disconnected")
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// prepareBody 把下游原始请求体改装为上游请求体：整体透传，替换 model 为
// 上游模型标识（不透明字符串：ep-xxx / gpt-4o / doubao-xxx 等），并按需补上
// 接入点配置的默认参数。其余字段（含各供应商私有参数）一律原样保留，
// 避免猜测性改写误伤。
//
// defaults 的合并语义是「**下游优先**」：下游请求体里已经有该键就不动它。
// 理由：默认参数是为了补上「下游不会发」的字段而存在（机器人框架发不出
// extra_body），一旦下游自己发了值（例如用户在会话里指定了不同的 persona），
// 再覆盖就把调用方的显式意图顶掉了——这比少一个默认值更难排查。
//
// model 永远是网关的唯一真源，即使 defaults 里带了这个键也不生效（
// 由 ValidateBodyParams 在写入端拦住，这里是第二道防线）。
func prepareBody(down []byte, upstreamModel string, defaults map[string]json.RawMessage) ([]byte, error) {
	raw := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(down))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	// 先补默认，再设 model：顺序反了会被下行代码的 model 赋值盖住，
	// 但这单靠顺序保不对——下面显式跳过保留键才是真正的保证。
	for k, v := range defaults {
		if reservedBodyKeys[k] {
			continue
		}
		if _, exists := raw[k]; exists {
			continue // 下游优先
		}
		// 用 json.RawMessage 直接嵌（已是合法 JSON 字节），不经过 any 往返。
		raw[k] = v
	}
	raw["model"] = upstreamModel
	return json.Marshal(raw)
}

// reservedBodyKeys 是网关独占的请求体字段，任何默认参数注入都不得触碰。
//
// 与 forbiddenRequestHeaders 同一类红线：
//   - model：路由唯一真源，被改就没法正确转发/计费/记录日志。
//   - stream：决定下游拿到流式还是非流式；注入 true 会让下游解析不了响应。
//   - stream_options：与 stream 配对（include_usage），网关自己会写。
var reservedBodyKeys = map[string]bool{
	"model":          true,
	"stream":         true,
	"stream_options": true,
}

// ValidateBodyParams 校验接入点默认请求体参数。写入端（管理 API）调用，
// 让配置错误在保存时就要报出来，而不是等到转发时静默失效。
//
// 只做两项检查：不能用保留键；值必须是合法 JSON。值本身可以是任意结构
// （对象/数组/字符串/数字/布尔/null），不做白名单——上游方言千奇百怪，
// 限定形状等于把可用性锁死在网关的认知里。
func ValidateBodyParams(params map[string]json.RawMessage) error {
	for k, v := range params {
		name := strings.TrimSpace(k)
		if name == "" {
			return fmt.Errorf("默认请求体参数的字段名不能为空")
		}
		if reservedBodyKeys[name] {
			return fmt.Errorf("字段 %s 由网关独占（model/stream/stream_options），不能作为默认参数", name)
		}
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) == 0 {
			return fmt.Errorf("字段 %s 的值不能为空", name)
		}
		if !json.Valid(trimmed) {
			return fmt.Errorf("字段 %s 的值不是合法 JSON", name)
		}
	}
	return nil
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

// NormalizeStreamFlag 把下游请求体里的 stream 字段归一为 JSON 布尔。
//
// 动机：wantsStream 用 `struct{ Stream bool }` 解码，字符串 "true"/"false" 会
// 让解码整体失败并被静默当作非流式；此时请求走 prepareBody 原样透传，字符串
// 就照发给上游，上游直接 4xx/5xx：
//
//	stream 必须是布尔值 / InvalidParameter: expected a boolean, but got "false"
//	json: cannot unmarshal string into Go struct field ***.stream of type bool
//
// 归一后再判定流式，两条路径发出去的都是干净布尔。规则：
//   - 已经是布尔 / 数字 0|1 → 原样；
//   - 字符串可识别的真值/假值 → 改写为布尔；
//   - 其它无法识别的字符串 → 删除该字段（字段缺失等价于默认非流式，语义不变
//     且绝不会引发上游类型错误）；
//   - 字段不存在 / 请求体非法 → 原样返回，不改写（保持字节透传语义）。
func NormalizeStreamFlag(body []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body, false
	}
	v, ok := raw["stream"]
	if !ok {
		return body, false
	}
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 {
		return body, false
	}

	switch trimmed[0] {
	case 't', 'f':
		return body, false // 已是布尔字面量
	}

	normalized, replaced := normalizeStreamValue(trimmed)
	if !replaced {
		return body, false
	}
	if normalized == nil {
		delete(raw, "stream") // 无法识别：删字段而非留脏值
	} else {
		raw["stream"] = normalized
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return out, true
}

// normalizeStreamValue 归一单个 stream 取值。返回 (新值, 是否命中需要改写)；
// 新值为 nil 表示应删除该字段。布尔字面量由调用方提前短路，这里只处理
// 字符串与数字形态。
func normalizeStreamValue(v []byte) (json.RawMessage, bool) {
	// 数字：1/0（含 1.0/0.0）映射为布尔，其它数字视为无法识别 → 删除。
	if c := v[0]; c == '-' || (c >= '0' && c <= '9') {
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return nil, true
		}
		switch f {
		case 0:
			return json.RawMessage("false"), true
		case 1:
			return json.RawMessage("true"), true
		default:
			return nil, true
		}
	}

	// 字符串：解出内容后按真值/假值表判定。
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil, true
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on", "enable", "enabled":
			return json.RawMessage("true"), true
		case "false", "0", "no", "off", "disable", "disabled", "":
			return json.RawMessage("false"), true
		}
		return nil, true // 无法识别的字符串：删除字段
	}

	// null / 对象 / 数组等：删除字段（stream 无这些合法形态）。
	return nil, true
}

// prepareStreamBody 在 prepareBody 基础上强制流式 + include_usage
// （用量从 final chunk 提取；保留下游 stream_options 的其它字段）。
func prepareStreamBody(down []byte, upstreamModel string, defaults map[string]json.RawMessage) ([]byte, error) {
	body, err := prepareBody(down, upstreamModel, defaults)
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
// 缓存 token 兼容两种口径（见 TextUsage 注释）：OpenAI 的
// prompt_tokens_details.cached_tokens 与 Anthropic 风格的 cache_* 字段
// （Anthropic 原生响应不经过本函数，但转换桥的下游可能透传这些字段）。
func ExtractChatUsage(raw []byte) *TextUsage {
	u := &TextUsage{}
	var parsed struct {
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Usage != nil {
		u.PromptTokens = parsed.Usage.PromptTokens
		u.CompletionTokens = parsed.Usage.CompletionTokens
		u.CacheCreationTokens = parsed.Usage.CacheCreationTokens
		u.CacheReadTokens = parsed.Usage.CacheReadTokens
		if d := parsed.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
			u.CacheReadTokens = d.CachedTokens
		}
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
