// Package provider 定义多供应商抽象。
//
// 设计要点：
//   - 供应商差异以「数据」表达（Def 注册表）：新增一家 OpenAI 兼容供应商通常只需
//     在 registry 加一项，不改代码。
//   - 传输层复用 openai-go 官方 SDK 作为骨干（鉴权 / base URL / 瞬时重试 / 连接池），
//     请求体与响应均为原始字节透传（仅替换 model）——见 manager.go。
//   - 上游 API Key 与模型标识都是「不透明字符串」：不校验前缀、不归一化、不推断类型。
//     （ep- 只是 Ark 平台的生成规则，网关不做任何识别。）
package provider

import (
	"errors"
	"sort"
	"strings"
)

// ErrNoBaseURL 表示供应商无默认 base URL 且账号未提供覆盖。
var ErrNoBaseURL = errors.New("provider: 未配置 base URL")

// Capabilities 该供应商原生支持的对外接口。
type Capabilities struct {
	Chat      bool `json:"chat"`      // POST /chat/completions
	Responses bool `json:"responses"` // POST /responses
	Images    bool `json:"images"`    // POST /images/generations
}

// Def 描述一类上游供应商。
type Def struct {
	ID             string       `json:"id"`
	DisplayName    string       `json:"display_name"`
	DefaultBaseURL string       `json:"default_base_url"` // custom 为空 → 账号必填 base_url
	Native         Capabilities `json:"native"`
}

var registry = map[string]Def{
	"ark": {
		ID:             "ark",
		DisplayName:    "火山方舟 Ark",
		DefaultBaseURL: "https://ark.cn-beijing.volces.com/api/v3",
		Native:         Capabilities{Chat: true, Responses: true, Images: true},
	},
	"openai": {
		ID:             "openai",
		DisplayName:    "OpenAI",
		DefaultBaseURL: "https://api.openai.com/v1",
		Native:         Capabilities{Chat: true, Responses: true, Images: true},
	},
	"custom": {
		ID:          "custom",
		DisplayName: "自定义（OpenAI 兼容）",
		// 无默认 base：DeepSeek / Moonshot / vLLM / OpenRouter 等兼容方言，账号必填。
		// responses/images 能力未知，默认关；账号可用三态覆盖打开。
		Native: Capabilities{Chat: true},
	},
}

// Get 按注册表取供应商定义。
func Get(id string) (Def, bool) {
	d, ok := registry[id]
	return d, ok
}

// List 返回全部供应商定义（按 ID 排序），供管理端下拉。
func List() []Def {
	out := make([]Def, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FallbackDef 返回一个「仅 chat」的兜底定义：库中出现了未注册的 provider id
// （手改数据库等）时，让账号仍可用于 chat 而不是整体失联。
func FallbackDef(id string) Def {
	return Def{ID: id, DisplayName: id, Native: Capabilities{Chat: true}}
}

// NormalizeBaseURL 规范化 base URL（去空白、去尾斜杠）。
func NormalizeBaseURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// IsHTTPURL 判断是否为合法的 http(s) URL 前缀。
func IsHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// ResolveBaseURL 解析「该供应商 + 账号覆盖」的最终 base URL。
func (d Def) ResolveBaseURL(accountOverride string) (string, error) {
	if u := NormalizeBaseURL(accountOverride); u != "" {
		return u, nil
	}
	if d.DefaultBaseURL == "" {
		return "", ErrNoBaseURL
	}
	return d.DefaultBaseURL, nil
}
