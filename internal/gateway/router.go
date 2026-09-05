// 虚拟路由模型（type=router）的输入长度估算。
//
// 分流只关心「量级」，不追求精确：按经验规则把文本折算成 token 数——
//   - CJK 字符（中日韩）约 1 字符 ≈ 1 token；
//   - 其它字符（拉丁/空白/标点/代码）约 4 字符 ≈ 1 token；
//   - 另加对话级 3 token 与每条消息 4 token 的结构开销（OpenAI 风格）。
//
// 估算结果只用于选路由（往大模型还是小模型送），不参与计量计费——
// 计量始终取上游返回的真实 usage。宁可略偏高：把请求错送到更大的模型
// 比撑爆小模型的上下文代价小。
package gateway

import (
	"encoding/json"
	"unicode"

	"arkgate/internal/balancer"
)

// 估算的经验参数（见文件头注释）。
const (
	estBaseTokens    = 3 // 对话级固定开销
	estMsgOverhead   = 4 // 每条消息的结构开销
	estOtherPerToken = 4 // 非CJK字符：约 4 字符折 1 token
)

// estimateInputTokens 按 API 形态估算输入 token 数（图像请求不参与路由，恒 0）。
func estimateInputTokens(api balancer.API, body []byte) int64 {
	switch api {
	case balancer.APIResponses:
		return estimateResponsesTokens(body)
	case balancer.APIImages:
		return 0
	default:
		return estimateChatTokens(body)
	}
}

// estimateChatTokens 估算 chat/completions 请求的输入 tokens：
// 逐条 messages 累加 content（字符串或分段数组）+ 每条消息的结构开销。
func estimateChatTokens(body []byte) int64 {
	var v struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	total := int64(estBaseTokens)
	for _, msg := range v.Messages {
		total += contentTokens(msg.Content) + estMsgOverhead
	}
	return total
}

// estimateResponsesTokens 估算 responses 请求的输入 tokens：
// instructions + input（字符串，或消息数组——每项 content 再按分段解析）。
func estimateResponsesTokens(body []byte) int64 {
	var v struct {
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	total := int64(estBaseTokens) + textTokens(v.Instructions)
	var items []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(v.Input, &items) == nil && len(items) > 0 {
		for _, it := range items {
			total += contentTokens(it.Content) + estMsgOverhead
		}
		return total
	}
	total += contentTokens(v.Input)
	return total
}

// estimateAnthropicInputTokens 估算 /v1/messages 请求的输入 tokens：
// system（字符串或 text 块数组）+ messages（content 字符串或块数组，
// tool_result/tool_use 块的文本按 content/text 字段粗取）。
func estimateAnthropicInputTokens(body []byte) int64 {
	var v struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	total := int64(estBaseTokens) + anthropicContentTokens(v.System)
	for _, m := range v.Messages {
		total += anthropicContentTokens(m.Content) + estMsgOverhead
	}
	return total
}

// anthropicContentTokens 计算入站内容（字符串或块数组）的估算 tokens：
// 块数组取 text 字段（与 contentTokens 同口径，图像等分段忽略）。
func anthropicContentTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if n := contentTokens(raw); n > 0 {
		return n
	}
	// contentTokens 不认识的形态可能是 Anthropic 块数组——再按 text 字段兜一次。
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var n int64
		for _, b := range blocks {
			n += textTokens(b.Text)
		}
		return n
	}
	return 0
}

// contentTokens 计算一段「内容」的估算 tokens：字符串，或分段数组
// （[{type:"text", text:"..."}, ...]，只统计 text 字段；图像等分段忽略）。
// null / 无法识别的形态按 0 处理（粗估不需要兜底报错）。
func contentTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return textTokens(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var n int64
		for _, p := range parts {
			n += textTokens(p.Text)
		}
		return n
	}
	return 0
}

// textTokens 估算一段文本的 token 数：CJK 约 1 字 1 token，其余约 4 字符 1 token
// （向上取整）。
func textTokens(s string) int64 {
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
	return int64(cjk + (other+estOtherPerToken-1)/estOtherPerToken)
}

// isCJK 判断是否中日韩表意文字（这些文字密度高，约 1 字符 ≈ 1 token）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
