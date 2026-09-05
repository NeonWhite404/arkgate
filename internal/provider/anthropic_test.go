package provider

import (
	"context"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAnthropicRequest 覆盖请求转换的核心规则：
// system 抽取、连续同角色合并、content 分段、stop、协议边界拒绝。
func TestAnthropicRequest(t *testing.T) {
	mk := func(msgs string) []byte {
		return []byte(`{"model":"any","messages":` + msgs + `}`)
	}

	// 1. 基本转换 + system 抽取 + 连续同角色合并。
	out, err := AnthropicRequest(mk(`[
		{"role":"system","content":"你是助手"},
		{"role":"developer","content":"附加规则"},
		{"role":"user","content":"你好"},
		{"role":"assistant","content":"你好！"},
		{"role":"assistant","content":"有什么可以帮你？"},
		{"role":"user","content":[{"type":"text","text":"讲个笑话"},{"type":"text","text":"短的"}]}
	]`), "claude-x", 1024, false)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got anthropicMessagesRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Model != "claude-x" || got.MaxTokens != 1024 || got.Stream {
		t.Fatalf("base fields: %+v", got)
	}
	if got.System != "你是助手\n\n附加规则" {
		t.Fatalf("system = %q", got.System)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("want 3 merged messages, got %d: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content[0].Text != "你好" {
		t.Fatalf("msg0: %+v", got.Messages[0])
	}
	// 连续同角色合并为一条消息（text 块顺序保留，不再拼接成单块——
	// 块追加才能同时容纳 image/tool_use 等混合内容）。
	if got.Messages[1].Content[0].Text != "你好！" || got.Messages[1].Content[1].Text != "有什么可以帮你？" {
		t.Fatalf("merge failed: %+v", got.Messages[1])
	}
	if got.Messages[2].Content[0].Text != "讲个笑话" || got.Messages[2].Content[1].Text != "短的" {
		t.Fatalf("parts: %+v", got.Messages[2])
	}

	// 2. stop 字符串 → stop_sequences；temperature/top_p 透传。
	out, err = AnthropicRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],"stop":["A","B"],"temperature":0.5,"top_p":0.9}`), "m", 10, true)
	if err != nil {
		t.Fatalf("convert2: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal2: %v", err)
	}
	if len(got.StopSequences) != 2 || !got.Stream || got.Temperature == nil || *got.Temperature != 0.5 {
		t.Fatalf("stop/stream/params: %+v", got)
	}

	// 3. 协议边界：tools / tool_choice / tool_calls / tool 角色 / 图像已全量
	// 支持；仅未知分段类型与空消息仍拒绝。
	bad := []string{
		`{"messages":[{"role":"system","content":"only system"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"audio","audio":{}}]}]}`,
	}
	for i, body := range bad {
		if _, err := AnthropicRequest([]byte(body), "m", 10, false); err == nil {
			t.Fatalf("case %d should be rejected: %s", i, body)
		}
	}
	// tools 映射：parameters → input_schema（schema 清洗 + 根 object 补齐）。
	withTools := []byte(`{"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"properties":{"p":{"type":"string","format":"uri"}}}}}],
		"tool_choice":"required"}`)
	out, err = AnthropicRequest(withTools, "m", 10, false)
	if err != nil {
		t.Fatalf("tools convert: %v", err)
	}
	var tc anthropicMessagesRequest
	if json.Unmarshal(out, &tc) != nil {
		t.Fatalf("tools unmarshal")
	}
	if len(tc.Tools) != 1 || tc.Tools[0].Name != "f" || tc.Tools[0].Description != "d" {
		t.Fatalf("tools mapped: %+v", tc.Tools)
	}
	var schema map[string]any
	json.Unmarshal([]byte(tc.Tools[0].InputSchema), &schema)
	if schema["type"] != "object" || strings.Contains(string(tc.Tools[0].InputSchema), "uri") {
		t.Fatalf("schema cleaned: %s", tc.Tools[0].InputSchema)
	}
	if tc.ToolChoice == nil || string(tc.ToolChoice) != `{"type":"any"}` {
		t.Fatalf("tool_choice: %s", tc.ToolChoice)
	}
	// 助手 tool_calls → tool_use 块；tool 角色 → user/tool_result。
	withCalls := []byte(`{"messages":[
		{"role":"user","content":"x"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"result"}]}`)
	out, err = AnthropicRequest(withCalls, "m", 10, false)
	if err != nil {
		t.Fatalf("calls convert: %v", err)
	}
	if err := json.Unmarshal(out, &tc); err != nil {
		t.Fatalf("calls unmarshal: %v", err)
	}
	if len(tc.Messages) != 3 ||
		tc.Messages[1].Content[0].Type != "tool_use" || tc.Messages[1].Content[0].ID != "c1" ||
		!strings.Contains(string(tc.Messages[1].Content[0].Input), `"a"`) ||
		tc.Messages[2].Role != "user" || tc.Messages[2].Content[0].Type != "tool_result" ||
		tc.Messages[2].Content[0].ToolUseID != "c1" {
		t.Fatalf("calls mapped: %+v", tc.Messages)
	}
}

// TestAnthropicResponseToOpenAI 覆盖响应转换：文本拼接、finish_reason 映射、
// usage 口径、thinking 块丢弃、model 回填。
func TestAnthropicResponseToOpenAI(t *testing.T) {
	raw := []byte(`{
		"id":"msg_01","type":"message","role":"assistant","model":"claude-x",
		"content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"你好"},{"type":"text","text":"!"}],
		"stop_reason":"max_tokens",
		"usage":{"input_tokens":12,"output_tokens":34}
	}`)
	out, usage, err := AnthropicResponseToOpenAI(raw, "fallback")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got openAIChatResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "msg_01" || got.Model != "claude-x" || got.Object != "chat.completion" {
		t.Fatalf("head: %+v", got)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "你好!" || got.Choices[0].FinishReason != "length" {
		t.Fatalf("choice: %+v", got.Choices[0])
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 34 {
		t.Fatalf("usage: %+v", usage)
	}
	if got.Usage.TotalTokens != 46 {
		t.Fatalf("total tokens: %+v", got.Usage)
	}
	// stop_reason 缺省映射为 stop。
	out, _, _ = AnthropicResponseToOpenAI([]byte(`{"content":[{"type":"text","text":"a"}]}`), "fb")
	if !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Fatalf("default finish_reason: %s", out)
	}
	// 非法响应必须报错。
	if _, _, err := AnthropicResponseToOpenAI([]byte(`not-json`), "fb"); err == nil {
		t.Fatal("bad response must error")
	}
}

// TestAnthropicStreamConvert 覆盖事件流转 OpenAI chunk 的状态机：
// role chunk → content chunks → finish chunk（usage）→ [DONE]。
func TestAnthropicStreamConvert(t *testing.T) {
	st := &anthropicStreamState{model: "claude-x"}
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-real","usage":{"input_tokens":7}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"你"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":".."}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"好"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	var buf bytes.Buffer
	roleSeen := false
	for _, e := range events {
		out, err := st.convertEvent([]byte(e))
		if err != nil {
			t.Fatalf("event %s: %v", e, err)
		}
		if len(out) == 0 {
			continue
		}
		if bytes.Equal(out, []byte("data: [DONE]\n\n")) {
			buf.Write(out)
			continue
		}
		var chunk openAIChunk
		if err := json.Unmarshal(out, &chunk); err != nil {
			t.Fatalf("chunk unmarshal: %v (%s)", err, out)
		}
		if chunk.Object != "chat.completion.chunk" || chunk.ID != "msg_1" || chunk.Model != "claude-real" {
			t.Fatalf("chunk head: %+v", chunk)
		}
		c := chunk.Choices[0]
		if c.Delta.Role == "assistant" {
			roleSeen = true
		}
		if c.Delta.Content != "" {
			buf.WriteString(c.Delta.Content)
		}
		if c.FinishReason != nil && *c.FinishReason != "stop" {
			t.Fatalf("finish reason: %s", *c.FinishReason)
		}
	}
	if !roleSeen {
		t.Fatal("first chunk must carry role delta")
	}
	if buf.String() != "你好data: [DONE]\n\n" {
		t.Fatalf("text/done output: %q", buf.String())
	}
	if st.pt != 7 || st.ct != 3 {
		t.Fatalf("usage: pt=%d ct=%d", st.pt, st.ct)
	}

	// error 事件转为错误。
	st2 := &anthropicStreamState{}
	if _, err := st2.convertEvent([]byte(`{"type":"error","error":{"message":"overloaded"}}`)); err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("error event: %v", err)
	}
}

// TestAnthropicEndpoint 覆盖 base URL 拼接：/v1 结尾（OpenAI 习惯）与裸域名
// 两种形态都必须得到正确的 /v1/messages 地址（同一账号混布两种协议的前提）。
func TestAnthropicEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://proxy:8000/v1":        "http://proxy:8000/v1/messages",
		"http://proxy:8000/v1/":       "http://proxy:8000/v1/messages",
		"https://api.anthropic.com":   "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/":  "https://api.anthropic.com/v1/messages",
		"http://localhost:9000":       "http://localhost:9000/v1/messages",
	}
	for base, want := range cases {
		if got := anthropicEndpoint(base); got != want {
			t.Fatalf("anthropicEndpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

// fakeAnthropicUpstream 起一个最小 Anthropic 上游：记录收到的请求头与体，
// 按路径返回非流式 message 或 SSE 事件流。路径精确匹配 /v1/messages——
// 锁定「base 以 /v1 结尾不会拼出 /v1/v1/messages」的回归。
func fakeAnthropicUpstream(t *testing.T, capture *map[string]any, headers *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if capture != nil {
			*capture = body
		}
		if headers != nil {
			*headers = map[string]string{
				"x-api-key":         r.Header.Get("X-Api-Key"),
				"anthropic-version": r.Header.Get("anthropic-version"),
			}
		}
		if r.URL.Path == "/v1/messages" && body["stream"] == true {
			// keep-alive 空闲连接会让 httptest.Server.Close 阻塞 5s，直接断开。
			w.Header().Set("Connection", "close")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\n")
			fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_s","model":"claude-real","usage":{"input_tokens":5}}}`+"\n")
			fmt.Fprint(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`+"\n")
			fmt.Fprint(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`+"\n")
			fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-real",`+
			`"content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":9,"output_tokens":4}}`)
	}))
}

// TestAnthropicChatEndToEnd 非流式全链路：SDK 发出的请求头/路径/体 + 响应转换 + 用量。
func TestAnthropicChatEndToEnd(t *testing.T) {
	var capture map[string]any
	var headers map[string]string
	srv := fakeAnthropicUpstream(t, &capture, &headers)
	defer srv.Close()

	m := NewManager()
	rt := Route{BaseURL: srv.URL + "/v1", Key: "sk-test"}
	down := []byte(`{"model":"down-name","messages":[{"role":"user","content":"hi"}]}`)
	out, usage, err := m.AnthropicChat(context.Background(), rt, down, "claude-ep", 512, 10*time.Second)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if headers["x-api-key"] != "sk-test" || headers["anthropic-version"] != "2023-06-01" {
		t.Fatalf("auth headers: %+v", headers)
	}
	if capture["model"] != "claude-ep" || capture["max_tokens"] != float64(512) {
		t.Fatalf("upstream body: %+v", capture)
	}
	var got openAIChatResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("out unmarshal: %v", err)
	}
	if got.Choices[0].Message.Content != "hi there" || got.Model != "claude-real" {
		t.Fatalf("converted: %+v", got)
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 4 {
		t.Fatalf("usage: %+v", usage)
	}
}

// TestOpenAnthropicChatStream 流式全链路：Pump 输出 OpenAI chunk 序列与用量。
func TestOpenAnthropicChatStream(t *testing.T) {
	var capture map[string]any
	srv := fakeAnthropicUpstream(t, &capture, nil)
	defer srv.Close()

	m := NewManager()
	rt := Route{BaseURL: srv.URL + "/v1", Key: "k"}
	down := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	st, err := m.OpenAnthropicChatStream(context.Background(), rt, down, "claude-ep", 128, 5*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if capture["stream"] != true || capture["max_tokens"] != float64(128) {
		t.Fatalf("stream body: %+v", capture)
	}
	var buf bytes.Buffer
	pt, ct, err := st.Pump(&buf)
	if err != nil {
		t.Fatalf("pump: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, "hello") ||
		!strings.Contains(out, `"finish_reason":"stop"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("pump output: %s", out)
	}
	if pt != 5 || ct != 2 {
		t.Fatalf("usage: pt=%d ct=%d", pt, ct)
	}
}

// TestAnthropicChatUpstreamError 上游错误体被包装成 OpenAI 结构并保留状态码。
func TestAnthropicChatUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	m := NewManager()
	rt := Route{BaseURL: srv.URL, Key: "bad"}
	down := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, _, err := m.AnthropicChat(context.Background(), rt, down, "claude-ep", 100, 5*time.Second)
	he, ok := AsHTTPError(err)
	if !ok {
		t.Fatalf("want HTTPError, got %v", err)
	}
	if he.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", he.Code)
	}
	if !strings.Contains(string(he.Body), "invalid x-api-key") || !strings.Contains(string(he.Body), `"error"`) {
		t.Fatalf("error body not OpenAI-shaped: %s", he.Body)
	}
}

// TestAnthropicStreamFirstTokenTimeout 上游挂起时首 token 超时生效。
func TestAnthropicStreamFirstTokenTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	m := NewManager()
	rt := Route{BaseURL: srv.URL, Key: "k"}
	down := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, err := m.OpenAnthropicChatStream(context.Background(), rt, down, "m", 100, 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "首") {
		t.Fatalf("want first-token timeout, got %v", err)
	}
}

// TestAnthropicStreamToolsToOpenAI 出站流式 tools：Anthropic tool_use 块
//（content_block_start/input_json_delta/stop_reason tool_use）→ OpenAI
// tool_calls 增量 chunk（id/name 起始 + arguments 片段 + finish_reason）。
func TestAnthropicStreamToolsToOpenAI(t *testing.T) {
	st := &anthropicStreamState{model: "claude-x"}
	events := []string{
		`{"type":"message_start","message":{"id":"m1","model":"claude-real","usage":{"input_tokens":9}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"run_bash"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	}
	var buf bytes.Buffer
	for _, e := range events {
		out, err := st.convertEvent([]byte(e))
		if err != nil {
			t.Fatalf("event %s: %v", e, err)
		}
		if len(out) > 0 {
			buf.Write(out)
			buf.WriteString("\n")
		}
	}
	out := buf.String()
	// 起始 chunk：index + id + name。
	if !strings.Contains(out, `"id":"tu_1"`) {
		t.Fatalf("tool call start missing: %s", out)
	}
	if !strings.Contains(out, `"name":"run_bash"`) {
		t.Fatalf("tool name missing: %s", out)
	}
	// arguments 分片按序拼接（嵌在 JSON 字符串里，引号被转义为 \"）。
	i1 := strings.Index(out, `{\"cmd\":`)
	i2 := strings.Index(out, `\"ls\"}`)
	if i1 < 0 || i2 < 0 || i2 < i1 {
		t.Fatalf("argument fragments out of order: %s", out)
	}
	// finish_reason 映射为 tool_calls。
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason: %s", out)
	}
	// usage 仍正确提取（message_delta 的 output_tokens）。
	if st.pt != 9 || st.ct != 7 {
		t.Fatalf("usage: pt=%d ct=%d", st.pt, st.ct)
	}
}

// TestAnthropicRequestEmptyContent 边界回归：空 content 消息被跳过（不生成
// Anthropic 的 content:null——上游会拒绝）；tool 结果空值归一为 JSON 字符串。
func TestAnthropicRequestEmptyContent(t *testing.T) {
	// 空 user 消息 + 后续正常消息：空消息跳过、不破坏交替合并。
	out, err := AnthropicRequest([]byte(`{"messages":[
		{"role":"user","content":""},
		{"role":"user","content":"你好"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":""},
		{"role":"tool","tool_call_id":"c1","content":"plain result"}
	]}`), "m", 10, false)
	if err != nil {
		t.Fatalf("empty content convert: %v", err)
	}
	var got anthropicMessagesRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal (tool content must stay valid JSON): %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("want 3 messages (empty skipped), got %d: %+v", len(got.Messages), got.Messages)
	}
	// tool_result.content 必须是 JSON 字符串（两条 tool 结果合并进同一 user 消息）。
	if len(got.Messages[2].Content) != 2 ||
		string(got.Messages[2].Content[0].Content) != `"(empty)"` ||
		string(got.Messages[2].Content[1].Content) != `"plain result"` {
		t.Fatalf("tool result encoding: %+v", got.Messages[2].Content)
	}
}

// TestAnthropicStreamEmptyToolArgs 出站流式：tool_use 块没有 input_json_delta
//（空 input）时，块关闭补一段 "{}"，避免 OpenAI 客户端拿到空 arguments。
func TestAnthropicStreamEmptyToolArgs(t *testing.T) {
	st := &anthropicStreamState{model: "claude-x"}
	events := []string{
		`{"type":"message_start","message":{"id":"m1","model":"c","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"noop"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}
	var buf bytes.Buffer
	for _, e := range events {
		out, err := st.convertEvent([]byte(e))
		if err != nil {
			t.Fatalf("event: %v", err)
		}
		if len(out) > 0 {
			buf.Write(out)
		}
	}
	if !strings.Contains(buf.String(), `"arguments":"{}"`) {
		t.Fatalf("empty tool args must be filled with {}: %s", buf.String())
	}
}
