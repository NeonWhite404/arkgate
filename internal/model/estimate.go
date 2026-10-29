// 输入 token 估算（虚拟路由选型 + 上游未上报用量时的兜底）。
//
// 口径（经验规则，不追求精确）：
//   - CJK 字符（中日韩）约 1 字符 ≈ 1 token；
//   - 其它字符（拉丁/空白/标点/代码）约 4 字符 ≈ 1 token；
//   - 另加对话级 3 token 与每条消息 4 token 的结构开销（OpenAI 风格）。
//
// 放在 model 包的理由：gateway（路由选型、上游无 usage 时兜底）与 provider
// （流式输出字符数折算）都要用同一套口径。两处各写一份必然漂移——估算值的
// 唯一价值是「同一错误在两边一致」，口径不一致会让输入与输出不可比。
//
// **用途边界**：估算永远不覆盖上游真实值。只有当上游完全没有返回 usage 时，
// 才用它兜底，并在 usage_logs.usage_source 标记为「估算」（2）。
package model

import (
	"encoding/json"
	"unicode"
)

// 估算的经验参数（见文件头注释）。
const (
	EstBaseTokens    = 3 // 对话级固定开销
	EstMsgOverhead   = 4 // 每条消息的结构开销
	EstOtherPerToken = 4 // 非CJK字符：约 4 字符折 1 token
)

// EstimateChatTokens 估算 chat/completions 请求的输入 tokens：
// 逐条 messages 累加 content（字符串或分段数组）+ 每条消息的结构开销。
func EstimateChatTokens(body []byte) int64 {
	var v struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	total := int64(EstBaseTokens)
	for _, msg := range v.Messages {
		total += TextContentTokens(msg.Content) + EstMsgOverhead
	}
	return total
}

// EstimateResponsesTokens 估算 responses 请求的输入 tokens：
// instructions（字符串或消息项数组）+ input（字符串，或项数组——每项取
// content 分段，并计入 function_call 参数与 function_call_output 输出）。
func EstimateResponsesTokens(body []byte) int64 {
	var v struct {
		Instructions json.RawMessage `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	total := int64(EstBaseTokens) + ResponsesInstructionsTokens(v.Instructions)

	type inputItem struct {
		Content      json.RawMessage `json:"content"`
		FunctionCall *struct {
			Arguments string `json:"arguments"`
		} `json:"function_call"`
		FunctionCallOutput *struct {
			Output string `json:"output"`
		} `json:"function_call_output"`
	}
	var items []inputItem
	if json.Unmarshal(v.Input, &items) == nil && len(items) > 0 {
		for _, it := range items {
			total += TextContentTokens(it.Content) + EstMsgOverhead
			if it.FunctionCall != nil {
				total += TextTokens(it.FunctionCall.Arguments)
			}
			if it.FunctionCallOutput != nil {
				total += TextTokens(it.FunctionCallOutput.Output)
			}
		}
		return total
	}
	total += TextContentTokens(v.Input)
	return total
}

// ResponsesInstructionsTokens 统计 instructions 的估算 tokens：官方允许
// string 或 Array<ResponseInputItem>，数组形态逐项取文本 / content 分段。
func ResponsesInstructionsTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if n := TextContentTokens(raw); n > 0 {
		return n
	}
	var items []struct {
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var n int64
		for _, it := range items {
			n += TextTokens(it.Text) + TextContentTokens(it.Content)
		}
		return n
	}
	return 0
}

// TextContentTokens 统计一个 content 字段的估算 tokens（字符串或分段数组）。
func TextContentTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return TextTokens(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var n int64
		for _, p := range parts {
			n += TextTokens(p.Text)
		}
		return n
	}
	return 0
}

// TextTokens 估算一段文本的 token 数：CJK 约 1 字 1 token，其余约 4 字符
// 1 token（向上取整）。
func TextTokens(s string) int64 {
	if s == "" {
		return 0
	}
	var cjk, other int
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return int64(cjk + (other+EstOtherPerToken-1)/EstOtherPerToken)
}

// isCJK 判断是否中日韩表意文字（这些文字密度高，约 1 字符 ≈ 1 token）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
