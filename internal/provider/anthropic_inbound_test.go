package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOpenAIRequestFromAnthropic 覆盖入站请求转换：system 合并、单 text 块
// 简化、tool_result 拆独立 tool 消息、tool_use → tool_calls、图像互转、
// tool_choice 映射、thinking 丢弃、未知块拒绝。
func TestOpenAIRequestFromAnthropic(t *testing.T) {
	down := []byte(`{
		"model":"claude-x","max_tokens":2048,"stream":false,
		"system":[{"type":"text","text":"第一条"},{"type":"text","text":"第二条"}],
		"messages":[
			{"role":"user","content":"你好"},
			{"role":"assistant","content":[
				{"type":"text","text":"我查一下"},
				{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"北京"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"晴天 25 度"}
			]},
			{"role":"user","content":[
				{"type":"text","text":"谢谢"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
			]}
		],
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"auto"}
	}`)
	out, err := OpenAIRequestFromAnthropic(down)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got openAIInboundChatRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 2048 {
		t.Fatalf("max_tokens: %+v", got.MaxTokens)
	}
	// 期望消息序列：system、user、assistant(text+tool_calls)、tool、user(text+image)。
	if len(got.Messages) != 5 {
		t.Fatalf("want 5 messages, got %d: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "第一条\n第二条" {
		t.Fatalf("system: %+v", got.Messages[0])
	}
	asst := got.Messages[2]
	if asst.Role != "assistant" || asst.Content != "我查一下" || len(asst.ToolCalls) != 1 ||
		asst.ToolCalls[0].ID != "tu_1" || asst.ToolCalls[0].Function.Name != "get_weather" ||
		asst.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("assistant: %+v", asst)
	}
	toolMsg := got.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "tu_1" || toolMsg.Content != "晴天 25 度" {
		t.Fatalf("tool msg: %+v", toolMsg)
	}
	// image 块转回 data URI parts。
	last := got.Messages[4]
	parts, ok := last.Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user parts: %+v", last.Content)
	}
	img, _ := parts[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	if iu["url"] != "data:image/jpeg;base64,QUJD" {
		t.Fatalf("image url: %+v", iu)
	}
	// tools 声明 + tool_choice。
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_weather" ||
		!strings.Contains(string(got.Tools[0].Function.Parameters), "city") {
		t.Fatalf("tools: %+v", got.Tools)
	}
	if got.ToolChoice != "auto" {
		t.Fatalf("tool_choice: %+v", got.ToolChoice)
	}

	// tool_choice any/tool 形态映射。
	for raw, want := range map[string]string{
		`"any"`:                     "required",
		`{"type":"any"}`:            "required",
		`{"type":"tool","name":"f"}`: "f",
	} {
		body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tool_choice":` + raw + `}`)
		out, err := OpenAIRequestFromAnthropic(body)
		if err != nil {
			t.Fatalf("tool_choice %s: %v", raw, err)
		}
		if !strings.Contains(string(out), want) {
			t.Fatalf("tool_choice %s want %q in %s", raw, want, out)
		}
	}

	// thinking 块丢弃；未知块拒绝。
	ok1 := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":".."},{"type":"text","text":"ok"}]}]}`)
	out, err = OpenAIRequestFromAnthropic(ok1)
	if err != nil || !strings.Contains(string(out), `"ok"`) || strings.Contains(string(out), "..") {
		t.Fatalf("thinking drop: %v %s", err, out)
	}
	bad := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`)
	if _, err := OpenAIRequestFromAnthropic(bad); err == nil {
		t.Fatal("unknown block must be rejected")
	}
}

// TestAnthropicResponseFromOpenAI 覆盖响应反向转换：tool_calls → tool_use、
// finish 映射、usage、空 content 兜底。
func TestAnthropicResponseFromOpenAI(t *testing.T) {
	raw := []byte(`{
		"id":"cmpl-1","object":"chat.completion","model":"gpt-x",
		"choices":[{"index":0,"finish_reason":"tool_calls",
			"message":{"role":"assistant","content":null,
				"tool_calls":[{"id":"call_1","type":"function",
					"function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}}],
		"usage":{"prompt_tokens":21,"completion_tokens":9,"total_tokens":30}
	}`)
	out, _, err := AnthropicResponseFromOpenAI(raw, "fb")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		Content    []anthropicBlock `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      anthropicUsage   `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "message" || got.Role != "assistant" || got.Model != "gpt-x" || got.ID != "cmpl-1" {
		t.Fatalf("head: %+v", got)
	}
	if got.StopReason != "tool_use" || got.Usage.InputTokens != 21 || got.Usage.OutputTokens != 9 {
		t.Fatalf("stop/usage: %+v %+v", got.StopReason, got.Usage)
	}
	var tu map[string]any
	for _, b := range got.Content {
		if b.Type == "tool_use" {
			tu = map[string]any{"id": b.ID, "name": b.Name}
			var in map[string]any
			json.Unmarshal([]byte(b.Input), &in)
			tu["input"] = in
		}
	}
	if tu == nil || tu["id"] != "call_1" || tu["name"] != "get_weather" ||
		tu["input"].(map[string]any)["city"] != "北京" {
		t.Fatalf("tool_use block: %+v", tu)
	}
	// finish 映射：length→max_tokens、stop→end_turn；空 content 补空 text 块。
	for finish, want := range map[string]string{"length": "max_tokens", "stop": "end_turn", "content_filter": "end_turn"} {
		body := []byte(`{"choices":[{"finish_reason":"` + finish + `","message":{"role":"assistant","content":"hi"}}],"usage":{}}`)
		out, _, err := AnthropicResponseFromOpenAI(body, "fb")
		if err != nil {
			t.Fatalf("%s: %v", finish, err)
		}
		if !strings.Contains(string(out), `"stop_reason":"`+want+`"`) {
			t.Fatalf("%s → want %s in %s", finish, want, out)
		}
	}
	empty := []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}],"usage":{}}`)
	out, _, _ = AnthropicResponseFromOpenAI(empty, "fb")
	if !strings.Contains(string(out), `"content":[{`) {
		t.Fatalf("empty content must still yield a text block: %s", out)
	}
}

// TestOpenAIToAnthropicStreamConvert 覆盖流式状态机：message_start 惰性、
// text 块、tool_calls 增量（懒启动 + input_json_delta）、usage 缓存到收尾。
func TestOpenAIToAnthropicStreamConvert(t *testing.T) {
	s := &openAIToAnthropicState{toolBlocks: map[int]*toolBlockState{}}
	chunks := []string{
		`{"id":"cmpl-1","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"cmpl-1","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`{"id":"cmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":""}}]}}]}`,
		`{"id":"cmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}`,
		`{"id":"cmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`{"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"cmpl-1","choices":[],"usage":{"prompt_tokens":30,"completion_tokens":8}}`,
	}
	var buf bytes.Buffer
	for _, c := range chunks {
		if _, err := s.handleChunk([]byte(c), &buf); err != nil {
			t.Fatalf("chunk %s: %v", c, err)
		}
	}
	if _, _, err := s.finalize(&buf); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	out := buf.String()
	// 事件顺序与完整性（map 序列化字段按字母序，断言做顺序无关的子串匹配）。
	for _, want := range []string{
		"event: message_start", `"input_tokens":0`, // 首发不带真实 usage
		`"text":""`, "event: content_block_delta", `"text_delta"`,
		`"type":"tool_use"`, `"id":"call_1"`, `"name":"f"`,
		`"input_json_delta"`, `{\"a\"`,
		"event: content_block_stop", "event: message_delta",
		`"stop_reason":"tool_use"`, `"input_tokens":30,"output_tokens":8`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// finalize 幂等。
	var buf2 bytes.Buffer
	if _, _, err := s.finalize(&buf2); err != nil || buf2.Len() != 0 {
		t.Fatalf("finalize must be idempotent, got %s", buf2.String())
	}
	// 多 finish_reason 只认第一个（deltaEmitted 语义下 pendingDelta 不被覆盖）。
	s2 := &openAIToAnthropicState{toolBlocks: map[int]*toolBlockState{}}
	s2.handleChunk([]byte(`{"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"length"}]}`), &buf2)
	second := `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	s2.handleChunk([]byte(second), &buf2)
	var tail bytes.Buffer
	s2.finalize(&tail)
	if !strings.Contains(tail.String(), "max_tokens") || strings.Contains(tail.String(), "tool_use") {
		t.Fatalf("first finish_reason must win: %s", tail.String())
	}
}

// TestAnthropicNativePassthrough 原生透传全链路：请求体仅换 model、响应原样、
// 错误体保持 Anthropic 形状、流式 SSE 原样字节转发 + 用量提取。
func TestAnthropicNativePassthrough(t *testing.T) {
	var gotBody map[string]any
	var gotKey, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		gotKey = r.Header.Get("X-Api-Key")
		gotVer = r.Header.Get("anthropic-version")
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "messages") && r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\n")
			fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":6}}}`+"\n")
			fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer srv.Close()

	m := NewManager()
	rt := Route{BaseURL: srv.URL + "/v1", Key: "k"}
	down := []byte(`{"model":"claude-name","max_tokens":99,"messages":[{"role":"user","content":"x"}]}`)

	// 非流式：model 替换 + 响应原样 + 用量。
	out, usage, err := m.AnthropicNativeChat(context.Background(), rt, down, "claude-ep", 5*time.Second)
	if err != nil {
		t.Fatalf("native chat: %v", err)
	}
	if gotBody["model"] != "claude-ep" || gotBody["max_tokens"] != float64(99) {
		t.Fatalf("body: %+v", gotBody)
	}
	if !strings.Contains(string(out), `"id":"m1"`) || usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Fatalf("out/usage: %s %+v", out, usage)
	}
	if gotKey != "k" || gotVer != "2023-06-01" {
		t.Fatalf("headers: %s %s", gotKey, gotVer)
	}

	// 流式：SSE 原样转发 + sniff 用量。
	st, err := m.OpenAnthropicNativeStream(context.Background(), rt, down, "claude-ep", 5*time.Second)
	if err != nil {
		t.Fatalf("native stream: %v", err)
	}
	defer st.Close()
	var buf bytes.Buffer
	pt, ct, err := st.Pump(&buf)
	if err != nil {
		t.Fatalf("pump: %v", err)
	}
	if !strings.Contains(buf.String(), "event: message_start") || !strings.Contains(buf.String(), "message_stop") {
		t.Fatalf("raw sse: %s", buf.String())
	}
	if pt != 6 || ct != 0 {
		t.Fatalf("sniff usage: pt=%d ct=%d", pt, ct)
	}
}

// TestAnthropicNativeErrorPassthrough 上游错误体保持 Anthropic 形状原样透传。
func TestAnthropicNativeErrorPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	m := NewManager()
	rt := Route{BaseURL: srv.URL, Key: "k"}
	down := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	_, _, err := m.AnthropicNativeChat(context.Background(), rt, down, "m", 5*time.Second)
	he, ok := AsHTTPError(err)
	if !ok || he.Code != 429 {
		t.Fatalf("want HTTPError 429, got %v", err)
	}
	if !strings.Contains(string(he.Body), "rate_limit_error") {
		t.Fatalf("error body must stay anthropic-shaped: %s", he.Body)
	}
}
