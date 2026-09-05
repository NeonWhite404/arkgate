package gateway

import (
	"encoding/json"
	"testing"
)

// tt 构造 chat 请求体 JSON。
func tt(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestTextTokens 验证 CJK/非CJK 的混合折算口径：CJK 1 字 1 token，
// 其余 4 字符 1 token（向上取整）。
func TestTextTokens(t *testing.T) {
	if got := textTokens(""); got != 0 {
		t.Fatalf("empty want 0, got %d", got)
	}
	// 4 个 ASCII 字符 → 1 token；3 个 → 1 token（向上取整）；8 个 → 2。
	if got := textTokens("abcd"); got != 1 {
		t.Fatalf("abcd want 1, got %d", got)
	}
	if got := textTokens("abc"); got != 1 {
		t.Fatalf("abc want 1, got %d", got)
	}
	if got := textTokens("abcdefgh"); got != 2 {
		t.Fatalf("8 ascii want 2, got %d", got)
	}
	// 纯中文 4 字 → 4 token。
	if got := textTokens("你好世界"); got != 4 {
		t.Fatalf("4 cjk want 4, got %d", got)
	}
	// 混合：2 CJK + 8 ASCII → 2 + 2 = 4。
	if got := textTokens("你好abcdefgh"); got != 4 {
		t.Fatalf("mixed want 4, got %d", got)
	}
}

// TestEstimateChatTokens 覆盖 chat 请求的 content 形态：
// 字符串、分段数组、null（tool 消息）、结构开销。
func TestEstimateChatTokens(t *testing.T) {
	// 2 条消息：字符串内容 "abcd"（1）+ null（0）+ 基数 3 + 每条 4×2。
	body := tt(t, map[string]any{
		"model": "any",
		"messages": []any{
			map[string]any{"role": "user", "content": "abcd"},
			map[string]any{"role": "assistant", "content": nil},
		},
	})
	if got := estimateChatTokens(body); got != 1+0+3+8 {
		t.Fatalf("want %d, got %d", 1+0+3+8, got)
	}

	// 分段数组：[{type:text,text:"abcd"},{type:image_url,...}] 只算 text → 1。
	body = tt(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "abcd"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
			}},
		},
	})
	if got := estimateChatTokens(body); got != 1+3+4 {
		t.Fatalf("parts want 8, got %d", got)
	}

	// 无法解析的 body → 0（粗估不报错，路由按「最短输入」兜底）。
	if got := estimateChatTokens([]byte("{bad")); got != 0 {
		t.Fatalf("bad body want 0, got %d", got)
	}
}

// TestEstimateResponsesTokens 覆盖 responses 的 input 形态：
// 字符串、消息数组（content 为字符串/分段）、instructions。
func TestEstimateResponsesTokens(t *testing.T) {
	// input 为字符串 "abcdefgh"（2）+ 基数 3。
	body := tt(t, map[string]any{"model": "any", "input": "abcdefgh"})
	if got := estimateResponsesTokens(body); got != 2+3 {
		t.Fatalf("string input want 5, got %d", got)
	}

	// input 为消息数组：content "abcd"（1）+ 分段 "abcdefgh"（2）→ 3 + 3 + 4×2。
	body = tt(t, map[string]any{
		"instructions": "",
		"input": []any{
			map[string]any{"role": "user", "content": "abcd"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "abcdefgh"},
			}},
		},
	})
	if got := estimateResponsesTokens(body); got != 3+3+8 {
		t.Fatalf("array input want %d, got %d", 3+3+8, got)
	}

	// instructions 也计入。
	body = tt(t, map[string]any{"instructions": "abcd", "input": ""})
	if got := estimateResponsesTokens(body); got != 1+3 {
		t.Fatalf("instructions want 4, got %d", got)
	}
}

// TestEstimateInputTokensDispatch 各 API 形态分发正确；图像请求恒 0。
func TestEstimateInputTokensDispatch(t *testing.T) {
	chatBody := tt(t, map[string]any{"messages": []any{map[string]any{"content": "abcd"}}})
	if got := estimateInputTokens(0, chatBody); got != 1+3+4 {
		t.Fatalf("chat dispatch want 8, got %d", got)
	}
	respBody := tt(t, map[string]any{"input": "abcdefgh"})
	if got := estimateInputTokens(1, respBody); got != 2+3 {
		t.Fatalf("responses dispatch want 5, got %d", got)
	}
	if got := estimateInputTokens(2, chatBody); got != 0 {
		t.Fatalf("images want 0, got %d", got)
	}
}

// TestIsCJK 覆盖中日韩四类文字与拉丁字符的判定。
func TestIsCJK(t *testing.T) {
	for _, r := range "中日本한" {
		if !isCJK(r) {
			t.Fatalf("rune %q should be CJK", r)
		}
	}
	// 平假名/片假名。
	for _, r := range "かなカナ" {
		if !isCJK(r) {
			t.Fatalf("rune %q should be CJK", r)
		}
	}
	for _, r := range "abc 1!" {
		if isCJK(r) {
			t.Fatalf("rune %q should not be CJK", r)
		}
	}
}

// TestContentTokensNilAndObject null / 对象形态按 0 处理，不报错。
func TestContentTokensNilAndObject(t *testing.T) {
	if got := contentTokens(json.RawMessage("null")); got != 0 {
		t.Fatalf("null want 0, got %d", got)
	}
	if got := contentTokens(json.RawMessage(`{"role":"user"}`)); got != 0 {
		t.Fatalf("object want 0, got %d", got)
	}
	// 数组里混入无 text 字段的项：只统计有 text 的。
	raw := json.RawMessage(`[{"type":"text","text":"abcd"},{"type":"image_url"}]`)
	if got := contentTokens(raw); got != 1 {
		t.Fatalf("mixed parts want 1, got %d", got)
	}
}

// TestOpenAIMaxTokensOf 覆盖 Anthropic max_tokens 解析的两个键与缺省。
func TestOpenAIMaxTokensOf(t *testing.T) {
	if v := openAIMaxTokensOf([]byte(`{"max_tokens":100}`)); v != 100 {
		t.Fatalf("max_tokens: %d", v)
	}
	if v := openAIMaxTokensOf([]byte(`{"max_completion_tokens":200}`)); v != 200 {
		t.Fatalf("max_completion_tokens: %d", v)
	}
	// 两个键同时存在时 max_tokens 优先。
	if v := openAIMaxTokensOf([]byte(`{"max_tokens":100,"max_completion_tokens":200}`)); v != 100 {
		t.Fatalf("both: %d", v)
	}
	if v := openAIMaxTokensOf([]byte(`{}`)); v != 0 {
		t.Fatalf("absent: %d", v)
	}
}
