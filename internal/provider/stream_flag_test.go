package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestNormalizeStreamFlag 锁定 stream 字段归一：下游把 stream 写成字符串/数字
// 时改写成干净布尔，无法识别的取值删除字段，合法布尔与缺失字段保持字节不变。
//
// 回归背景：wantsStream 曾用 `struct{ Stream bool }` 解码字符串型 stream 失败后
// 静默回落非流式，请求体被 prepareBody 原样透传，上游直接报
// "stream 必须是布尔值" / "cannot unmarshal string into Go struct field .stream of type bool"。
func TestNormalizeStreamFlag(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // "" 表示期望字段被删除
		changed bool
	}{
		{"布尔 true 原样", `{"stream":true}`, `true`, false},
		{"布尔 false 原样", `{"stream":false}`, `false`, false},
		{"字符串 true", `{"stream":"true"}`, `true`, true},
		{"字符串 false", `{"stream":"false"}`, `false`, true},
		{"字符串大写 TRUE", `{"stream":"TRUE"}`, `true`, true},
		{"字符串带空白", `{"stream":" false "}`, `false`, true},
		{"字符串 1", `{"stream":"1"}`, `true`, true},
		{"字符串 0", `{"stream":"0"}`, `false`, true},
		{"字符串 yes/on/enabled", `{"stream":"yes"}`, `true`, true},
		{"字符串 no/off/disabled", `{"stream":"off"}`, `false`, true},
		{"字符串空串", `{"stream":""}`, `false`, true},
		{"数字 1", `{"stream":1}`, `true`, true},
		{"数字 0", `{"stream":0}`, `false`, true},
		{"数字 1.0", `{"stream":1.0}`, `true`, true},
		{"无法识别的字符串", `{"model":"m","stream":"maybe"}`, ``, true},
		{"无法识别的数字", `{"model":"m","stream":7}`, ``, true},
		{"null", `{"model":"m","stream":null}`, ``, true},
		{"对象", `{"model":"m","stream":{}}`, ``, true},
		{"数组", `{"model":"m","stream":[]}`, ``, true},
		{"字段缺失", `{"model":"m"}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := NormalizeStreamFlag([]byte(c.in))
			if changed != c.changed {
				t.Fatalf("changed = %v, want %v (out=%s)", changed, c.changed, out)
			}
			if !changed {
				if string(out) != c.in {
					t.Fatalf("未改写时必须字节原样: got %s want %s", out, c.in)
				}
				return
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("归一结果不是合法 JSON: %s", out)
			}
			got, ok := m["stream"]
			if c.want == "" {
				if ok {
					t.Fatalf("期望删除 stream 字段，实际为 %s", got)
				}
				if _, hasModel := m["model"]; !hasModel {
					t.Fatalf("归一不得丢弃其它字段: %s", out)
				}
				return
			}
			if !ok {
				t.Fatalf("期望 stream=%s，实际字段被删除", c.want)
			}
			if string(got) != c.want {
				t.Fatalf("stream = %s, want %s", got, c.want)
			}
		})
	}
}

// TestNormalizeStreamFlagNonJSON 非法 JSON 必须原样返回（不改写、不报错），
// 保持既有的「非法请求体由上游决定如何拒绝」语义。
func TestNormalizeStreamFlagNonJSON(t *testing.T) {
	for _, in := range []string{`not json`, ``, `{"stream":`, `[1,2,3]`} {
		out, changed := NormalizeStreamFlag([]byte(in))
		if changed {
			t.Fatalf("非法/非对象 JSON 不应改写: %q -> %s", in, out)
		}
		if string(out) != in {
			t.Fatalf("必须字节原样: %q -> %q", in, out)
		}
	}
}

// TestNormalizeStreamFlagPreservesFields 归一后其余字段（含大整数与嵌套结构）
// 必须完整保留，不得引入科学计数法或丢失私有参数。
func TestNormalizeStreamFlagPreservesFields(t *testing.T) {
	in := `{"model":"m","stream":"false","seed":12345678901234567890,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	out, changed := NormalizeStreamFlag([]byte(in))
	if !changed {
		t.Fatal("expected rewrite")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if string(m["stream"]) != "false" {
		t.Fatalf("stream = %s", m["stream"])
	}
	if string(m["seed"]) != "12345678901234567890" {
		t.Fatalf("大整数被改写: %s", m["seed"])
	}
	if string(m["stream_options"]) != `{"include_usage":true}` {
		t.Fatalf("stream_options 丢失: %s", m["stream_options"])
	}
	if string(m["model"]) != `"m"` {
		t.Fatalf("model 丢失: %s", m["model"])
	}
}

// TestNormalizeStreamFlagKeepsLiteralFidelity 锁定「未涉及的字段逐字节保留」：
// 归一走的是 map[string]json.RawMessage（而非 map[string]any），因此浮点尾零、
// 转义序列、字面 null 都不会被重新编码成别的形式。
// 若改用 map[string]any，1.50 会变成 1.5、大整数会丢精度。
func TestNormalizeStreamFlagKeepsLiteralFidelity(t *testing.T) {
	in := `{"model":"m","stream":"off","f":1.50,"big":12345678901234567890,"e":null,"u":"\u4e2d\u6587","nested":{"a":[1,2,{"b":null}]}}`
	out, changed := NormalizeStreamFlag([]byte(in))
	if !changed {
		t.Fatal("expected rewrite")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	checks := map[string]string{
		"f":      "1.50",
		"big":    "12345678901234567890",
		"e":      "null",
		"u":      `"\u4e2d\u6587"`,
		"nested": `{"a":[1,2,{"b":null}]}`,
	}
	for k, want := range checks {
		if got := string(m[k]); got != want {
			t.Fatalf("字段 %s 被重新编码: got %s want %s", k, got, want)
		}
	}
	if string(m["stream"]) != "false" {
		t.Fatalf(`stream "off" 应归一为 false, got %s`, m["stream"])
	}
}

// TestIsClientCancel 锁定「下游断开」判定：context.Canceled 及其常见包装要识别，
// 上游真实错误与 nil 不得误判（误判会把真故障排除出熔断计数）。
func TestIsClientCancel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"裸 canceled", context.Canceled, true},
		{"包装 canceled", fmt.Errorf("Post \"http://x\": %w", context.Canceled), true},
		{"DeadlineExceeded 不算客户端取消", context.DeadlineExceeded, false},
		{"broken pipe", errors.New("write tcp: broken pipe"), true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"上游 HTTP 400", &HTTPError{Code: 400, Body: []byte(`{}`)}, false},
		{"首 token 超时", ErrFirstToken, false},
		{"普通错误", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsClientCancel(c.err); got != c.want {
			t.Fatalf("%s: IsClientCancel = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIsRequestFaultTypeErrors 锁定参数类型错误的分类：字符串型 stream 触发的
// 上游错误必须被判为「客户端侧问题」，不得计入端点熔断。
//
// 回归背景：火山方舟返回 400 "stream 必须是布尔值"、Go 系聚合器返回 500
// "json: cannot unmarshal string into Go struct field ... of type bool"，
// 后者此前因「500 不在白名单」被当成端点故障，连续 5 次熔断健康账号。
func TestIsRequestFaultTypeErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"Ark stream 布尔校验 400",
			&HTTPError{Code: 400, Body: []byte("{\"error\":{\"code\":\"InvalidParameter\",\"message\":\"The parameter stream specified in the request are not valid: expected a boolean, but got \\\"false\\\" instead.\"}}")},
			true,
		},
		{
			"中文 stream 布尔校验 400",
			&HTTPError{Code: 400, Body: []byte(`{"error":{"message":"stream 必须是布尔值","type":"invalid_request"}}`)},
			true,
		},
		{
			"Go 反序列化失败 500",
			&HTTPError{Code: 500, Body: []byte(`{"error":{"message":"json: cannot unmarshal string into Go struct field request.stream of type bool (request id: 20261001)","type":"error"}}`)},
			true,
		},
		{
			"Go 反序列化失败 502",
			&HTTPError{Code: 502, Body: []byte(`{"error":{"message":"cannot unmarshal number into Go struct field .max_tokens of type int"}}`)},
			true,
		},
		{
			// 真正的上游 500 故障不得被误判为客户端问题，否则熔断永不生效。
			"真实 500 故障",
			&HTTPError{Code: 500, Body: []byte(`{"error":{"message":"internal server error"}}`)},
			false,
		},
		{
			"真实 502 故障",
			&HTTPError{Code: 502, Body: []byte(`<html>bad gateway</html>`)},
			false,
		},
		{
			"上下文超限仍是客户端问题",
			&HTTPError{Code: 400, Body: []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`)},
			true,
		},
		{
			"429 限流不算客户端问题",
			&HTTPError{Code: 429, Body: []byte(`{"error":{"message":"rate limit exceeded"}}`)},
			false,
		},
	}
	for _, c := range cases {
		if got := IsRequestFault(c.err); got != c.want {
			t.Fatalf("%s: IsRequestFault = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRequestFaultBodiesAreTypeErrors 确保新增的类型错误词条不会过度吞噬
// 正常的 4xx 业务错误（如参数缺失/鉴权失败），避免把真故障排除出熔断。
func TestRequestFaultBodiesAreTypeErrors(t *testing.T) {
	notFaults := []string{
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"model not found"}}`,
		`{"error":{"message":"insufficient quota"}}`,
	}
	for _, body := range notFaults {
		if IsRequestFault(&HTTPError{Code: 400, Body: []byte(body)}) {
			t.Fatalf("不应判为请求方问题: %s", body)
		}
	}
}

// TestNormalizeStreamFlagThenPrepareBody 端到端锁定：字符串 stream 经过网关入口
// 归一后，非流式路径（prepareBody）发给上游的也必须是布尔。
func TestNormalizeStreamFlagThenPrepareBody(t *testing.T) {
	body, changed := NormalizeStreamFlag([]byte(`{"model":"m","stream":"false","messages":[]}`))
	if !changed {
		t.Fatal("expected rewrite")
	}
	out, err := prepareBody(body, "ep-1", nil)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	var v struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("上游将收到非法 stream（复现原 bug）: %s err=%v", out, err)
	}
	if v.Stream {
		t.Fatalf("stream 应为 false，实际 true: %s", out)
	}
}

// TestPrepareBodyStillPassesThroughUnknownFields 归一只针对 stream，其余字段
// （含未知私有参数）仍须原样透传——不得变成白名单式重建。
func TestPrepareBodyStillPassesThroughUnknownFields(t *testing.T) {
	body, _ := NormalizeStreamFlag([]byte(`{"model":"m","stream":"true","vendor_flag":{"a":1},"thinking":{"type":"enabled"}}`))
	out, err := prepareBody(body, "ep-2", nil)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["model"]) != `"ep-2"` {
		t.Fatalf("model 未替换: %s", m["model"])
	}
	if string(m["vendor_flag"]) != `{"a":1}` {
		t.Fatalf("私有参数丢失: %s", m["vendor_flag"])
	}
	if string(m["thinking"]) != `{"type":"enabled"}` {
		t.Fatalf("thinking 丢失: %s", m["thinking"])
	}
}

var _ = http.MethodPost

// TestExtractChatUsageCacheTokens 锁定缓存 token 抽取：OpenAI 走
// prompt_tokens_details.cached_tokens，Anthropic 风格走 cache_* 顶层字段，
// 两者都要能识别，且 prompt/completion 不受影响。
//
// 回归背景：缓存字段曾只接在流式路径上，非流式响应里的 cached_tokens 被丢弃，
// 导致缓存命中率恒为 0（假指标比没有指标更糟）。
func TestExtractChatUsageCacheTokens(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantPt, wantCt int64
		wantCreate, wantRead int64
	}{
		{
			name: "OpenAI prompt_tokens_details.cached_tokens",
			body: `{"usage":{"prompt_tokens":1000,"completion_tokens":200,
			        "prompt_tokens_details":{"cached_tokens":800}}}`,
			wantPt: 1000, wantCt: 200, wantRead: 800,
		},
		{
			name: "Anthropic 风格顶层 cache 字段",
			body: `{"usage":{"prompt_tokens":1000,"completion_tokens":200,
			        "cache_creation_input_tokens":300,"cache_read_input_tokens":500}}`,
			wantPt: 1000, wantCt: 200, wantCreate: 300, wantRead: 500,
		},
		{
			name: "无缓存字段（旧上游）",
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			wantPt: 10, wantCt: 5,
		},
		{
			name: "cached_tokens=0 不应覆盖已有的 cache_read",
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":5,
			        "cache_read_input_tokens":7,"prompt_tokens_details":{"cached_tokens":0}}}`,
			wantPt: 10, wantCt: 5, wantRead: 7,
		},
		{
			name: "无 usage",
			body: `{"choices":[]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := ExtractChatUsage([]byte(c.body))
			if u == nil {
				t.Fatal("nil usage")
			}
			if u.PromptTokens != c.wantPt || u.CompletionTokens != c.wantCt {
				t.Fatalf("prompt/completion = %d/%d, want %d/%d",
					u.PromptTokens, u.CompletionTokens, c.wantPt, c.wantCt)
			}
			if u.CacheCreationTokens != c.wantCreate || u.CacheReadTokens != c.wantRead {
				t.Fatalf("cache = create %d read %d, want create %d read %d",
					u.CacheCreationTokens, u.CacheReadTokens, c.wantCreate, c.wantRead)
			}
		})
	}
}
