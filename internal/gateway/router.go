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

	"arkgate/internal/balancer"
	"arkgate/internal/model"
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
	total := int64(model.EstBaseTokens)
	for _, msg := range v.Messages {
		total += contentTokens(msg.Content) + model.EstMsgOverhead
	}
	return total
}

// estimateResponsesTokens 估算 responses 请求的输入 tokens：
// instructions（字符串或消息项数组）+ input（字符串，或项数组——每项取
// content 分段，并计入 function_call 参数与 function_call_output 输出）。
func estimateResponsesTokens(body []byte) int64 {
	var v struct {
		Instructions json.RawMessage `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	total := int64(model.EstBaseTokens) + responsesInstructionsTokens(v.Instructions)

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
			total += contentTokens(it.Content) + model.EstMsgOverhead
			if it.FunctionCall != nil {
				total += textTokens(it.FunctionCall.Arguments)
			}
			if it.FunctionCallOutput != nil {
				total += textTokens(it.FunctionCallOutput.Output)
			}
		}
		return total
	}
	total += contentTokens(v.Input)
	return total
}

// responsesInstructionsTokens 统计 instructions 的估算 tokens：官方允许
// string 或 Array<ResponseInputItem>，数组形态逐项取文本 / content 分段。
func responsesInstructionsTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if n := contentTokens(raw); n > 0 {
		return n
	}
	var items []struct {
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var n int64
		for _, it := range items {
			if it.Text != "" {
				n += textTokens(it.Text)
			} else {
				n += contentTokens(it.Content)
			}
		}
		return n
	}
	return 0
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
	total := int64(model.EstBaseTokens) + anthropicContentTokens(v.System)
	for _, m := range v.Messages {
		total += anthropicContentTokens(m.Content) + model.EstMsgOverhead
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

// contentTokens / textTokens / isCJK 的统一实现在 model 包（估算口径必须与
// 「上游无 usage 时的兜底值」共用同一份，否则同一次请求的输入与输出不可比）。
func contentTokens(raw json.RawMessage) int64 { return model.TextContentTokens(raw) }

func textTokens(s string) int64 { return model.TextTokens(s) }
