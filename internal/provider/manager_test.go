package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsRequestFault 锁定客户端错误分类：仅 400/413/422 且错误体命中超限特征
// 才视为请求方问题；限流/鉴权/参数无关错误仍计入端点熔断。
func TestIsRequestFault(t *testing.T) {
	cases := []struct {
		code int
		body string
		want bool
	}{
		{400, `{"error":{"message":"This model's maximum context length is 128000 tokens"}}`, true},
		{413, "The number of input tokens exceeds the model's limit", true},
		{422, `{"message":"prompt is too long: 200000 tokens > 131072 maximum"}`, true},
		{400, `{"error":{"message":"Invalid value for 'temperature'"}}`, false},
		{429, "quota exceeded for this account", false}, // 含 exceed 但状态码不对
		{401, "invalid api key", false},
		{404, `{"error":{"message":"model not found"}}`, false},
	}
	for _, c := range cases {
		got := IsRequestFault(&HTTPError{Code: c.code, Body: []byte(c.body)})
		if got != c.want {
			t.Fatalf("IsRequestFault(%d, %q) = %v, want %v", c.code, c.body, got, c.want)
		}
	}
	if IsRequestFault(errors.New("plain network error")) {
		t.Fatalf("non-HTTPError must not be request fault")
	}
}

// TestChatPassthroughAndModelSwap 锁定透传语义：除 model 换成上游标识外，
// 其余字段（含供应商私有字段）原样到达上游；Key 为不透明字符串原样进鉴权头。
func TestChatPassthroughAndModelSwap(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"ep-xyz","usage":{"prompt_tokens":3,"completion_tokens":5},"choices":[]}`)
	}))
	defer srv.Close()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "ark"), BaseURL: srv.URL, Key: "some-opaque-key-9999"}
	down := []byte(`{"model":"doubao","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`)
	raw, usage, err := m.Chat(context.Background(), rt, down, "ep-xyz")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer some-opaque-key-9999" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody["model"] != "ep-xyz" {
		t.Fatalf("model not swapped: %v", gotBody["model"])
	}
	if _, ok := gotBody["thinking"]; !ok {
		t.Fatal("vendor-specific field `thinking` was dropped")
	}
	if _, ok := gotBody["messages"]; !ok {
		t.Fatal("messages dropped")
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
	if !strings.Contains(string(raw), "ep-xyz") {
		t.Fatal("raw response not passed through")
	}
}

// TestBaseURLKeepsVersionSegment：base URL 带 /v1 版本段时相对路径拼接必须保留它。
func TestBaseURLKeepsVersionSegment(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"usage":{}}`)
	}))
	defer srv.Close()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "openai"), BaseURL: srv.URL + "/v1", Key: "k"}
	if _, _, err := m.Chat(context.Background(), rt, []byte(`{"model":"gpt-4o"}`), "gpt-4o"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %s, want /v1/chat/completions", gotPath)
	}
}

// TestUpstreamErrorPassthrough：上游非 2xx 的状态码与错误体必须完整保留。
func TestUpstreamErrorPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited by upstream"}}`)
	}))
	defer srv.Close()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "ark"), BaseURL: srv.URL, Key: "k"}
	_, _, err := m.Chat(context.Background(), rt, []byte(`{"model":"m"}`), "ep-1")
	he, ok := AsHTTPError(err)
	if !ok {
		t.Fatalf("want HTTPError, got %v", err)
	}
	if he.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", he.Code)
	}
	if !strings.Contains(string(he.Body), "rate limited by upstream") {
		t.Fatalf("body not preserved: %s", he.Body)
	}
}

// TestChatStreamForward：SSE 逐字节原样回传 + include_usage 强制注入 + 用量提取。
func TestChatStreamForward(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":11}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer srv.Close()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "ark"), BaseURL: srv.URL, Key: "k"}
	var sink strings.Builder
	usage, err := m.ChatStream(context.Background(), rt, []byte(`{"model":"m","stream":true}`), "ep-1", &sink)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if gotBody["model"] != "ep-1" {
		t.Fatalf("model = %v", gotBody["model"])
	}
	if gotBody["stream"] != true {
		t.Fatal("stream not forced true")
	}
	so, _ := gotBody["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Fatal("include_usage not forced")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 11 {
		t.Fatalf("usage = %+v", usage)
	}
	if !strings.Contains(sink.String(), `"delta":{"content":"he"}`) {
		t.Fatal("SSE bytes not forwarded verbatim")
	}
	if !strings.Contains(sink.String(), "data: [DONE]") {
		t.Fatal("terminator missing")
	}
}

// TestResponsesUsage：非流式取顶层 usage.input/output_tokens；
// 流式从 response.completed 事件提取。
func TestResponsesUsage(t *testing.T) {
	u := ExtractResponsesUsage([]byte(`{"usage":{"input_tokens":4,"output_tokens":6}}`))
	if u.PromptTokens != 4 || u.CompletionTokens != 6 {
		t.Fatalf("non-stream usage = %+v", u)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n")
	}))
	defer srv.Close()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "openai"), BaseURL: srv.URL, Key: "k"}
	var sink strings.Builder
	usage, err := m.ResponsesStream(context.Background(), rt, []byte(`{"model":"m","stream":true}`), "gpt-4o", &sink)
	if err != nil {
		t.Fatalf("responses stream: %v", err)
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 2 {
		t.Fatalf("stream usage = %+v", usage)
	}
	if !strings.Contains(sink.String(), "response.created") {
		t.Fatal("events not forwarded verbatim")
	}
}

// TestImagesUsage：张数 = data 数组长度；gpt-image-1 式 token 用量一并提取。
func TestImagesUsage(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"AA"},{"b64_json":"BB"}],"usage":{"input_tokens":10,"output_tokens":0}}`)
	}))
	defer srv.Close()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "ark"), BaseURL: srv.URL, Key: "k"}
	raw, usage, err := m.Images(context.Background(), rt, []byte(`{"model":"seedream","prompt":"a cat","n":2}`), "doubao-seedream-4-0")
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	if gotPath != "/images/generations" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotBody["model"] != "doubao-seedream-4-0" || gotBody["n"] != float64(2) {
		t.Fatalf("body = %v", gotBody)
	}
	if usage.Count != 2 || usage.PromptTokens != 10 {
		t.Fatalf("usage = %+v", usage)
	}
	if !strings.Contains(string(raw), "b64_json") {
		t.Fatal("raw response not passed through")
	}
}

// TestRegistryFallbackDef：未注册的 provider id 回落为仅 chat 的兜底定义。
func TestRegistryFallbackDef(t *testing.T) {
	d := FallbackDef("mystery")
	if d.Native.Chat != true || d.Native.Images || d.Native.Responses {
		t.Fatalf("fallback def = %+v", d)
	}
	if _, ok := Get("ark"); !ok {
		t.Fatal("ark should be registered")
	}
	if _, ok := Get("mystery"); ok {
		t.Fatal("mystery should not be registered")
	}
}

// TestResolveBaseURL：账号覆盖优先于默认；无默认且无覆盖时报错。
func TestResolveBaseURL(t *testing.T) {
	ark, _ := Get("ark")
	if u, err := ark.ResolveBaseURL("https://over.example/v1/"); err != nil || u != "https://over.example/v1" {
		t.Fatalf("override: %q %v", u, err)
	}
	custom, _ := Get("custom")
	if _, err := custom.ResolveBaseURL(""); err != ErrNoBaseURL {
		t.Fatalf("custom without base: %v", err)
	}
	if u, _ := custom.ResolveBaseURL(" http://x/v1 "); u != "http://x/v1" {
		t.Fatalf("custom with base: %q", u)
	}
}

func mustDef(t *testing.T, id string) Def {
	t.Helper()
	d, ok := Get(id)
	if !ok {
		t.Fatalf("provider %s not registered", id)
	}
	return d
}

// TestOpenStreamFirstTokenTimeout：上游建连后迟迟不出首字节时，
// openStream 必须在超时后返回 ErrFirstToken（可重试），且不向 sink 写任何字节。
func TestOpenStreamFirstTokenTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 挂住：不写响应头也不写字节
	}))
	defer func() { close(release); srv.Close() }()

	m := NewManager(time.Second)
	rt := Route{Def: mustDef(t, "ark"), BaseURL: srv.URL, Key: "k"}

	start := time.Now()
	st, err := m.OpenChatStream(context.Background(), rt, []byte(`{"model":"m","stream":true}`), "ep-1", 150*time.Millisecond)
	if err == nil {
		st.Close()
		t.Fatal("want first-token timeout error")
	}
	if !errors.Is(err, ErrFirstToken) {
		t.Fatalf("want ErrFirstToken, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout not enforced timely: %v", time.Since(start))
	}

	// 对照组：正常出字节的上游，短超时也能打开并泵完整流。
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer okSrv.Close()
	rt2 := Route{Def: mustDef(t, "ark"), BaseURL: okSrv.URL, Key: "k"}
	st2, err := m.OpenChatStream(context.Background(), rt2, []byte(`{"model":"m","stream":true}`), "ep-1", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("open ok stream: %v", err)
	}
	var sink strings.Builder
	pt, ct, perr := st2.Pump(&sink)
	st2.Close()
	if perr != nil {
		t.Fatalf("pump: %v", perr)
	}
	if pt != 1 || ct != 2 {
		t.Fatalf("pump usage = %d/%d", pt, ct)
	}
	if !strings.Contains(sink.String(), "[DONE]") {
		t.Fatal("stream bytes missing")
	}
}
