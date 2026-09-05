// Package model 定义 ArkGate 的核心数据结构，供各包共享。
//
// 拓扑：树状结构。
//   - 叶节点 = 接入点 Endpoint（账号 × 上游模型标识，服务某个易读模型名）。
//     限流（并发/RPM/TPM）与熔断均只落在叶节点上。
//   - 父节点 = 账号 Account（某供应商的账号，持有一条不透明的上游 API Key）。
//     账号不单独限流、不单独熔断；只有 active/disabled 状态 + 统计聚合，
//     管理员可从账号视角查看其下所有接入点的汇总统计。
//
// 路由语义：客户端用自己的子 Key + 想请求的易读模型名；网关在该模型名对应的
// 所有「叶节点」里做加权轮询，命中一个叶子后替换子 Key → 账号真实 Key，
// 并把 model 换成该叶子的上游模型标识转发。
//
// 不透明字符串原则：上游 API Key 与模型标识（Endpoint.EP）都是任意字符串——
// 不校验前缀、不归一化、不推断类型（ep- 只是 Ark 平台的生成规则，网关不识别）。
package model

import "time"

// 账号状态
const (
	AccountActive   = "active"
	AccountDisabled = "disabled"
)

// 模型类型
const (
	ModelTypeText   = "text"
	ModelTypeImage  = "image"
	// ModelTypeRouter 虚拟路由模型：不承接上游流量（没有接入点映射），
	// 网关按「估算输入长度」把发往该名字的请求分流到其它真实模型。
	// 参考 OpenAI 的 auto / 各家 router 类模型：客户端只管请求这个名字，
	// 具体打哪个模型由网关在入口解析决定。
	ModelTypeRouter = "router"
)

// 虚拟路由模型的配置边界。
const (
	// MaxRouterRules 单个路由模型允许的分流规则条数上限（防误配出超大配置）。
	MaxRouterRules = 32
	// MaxRouterDepth 路由链解析深度上限：目标本身也可以是路由模型（链式），
	// 超过该层数视为配置异常，拒绝解析而非无限递归。
	MaxRouterDepth = 8
)

// 熔断与冷却相关常量。
const (
	// 连续失败达到该次数后熔断（仅叶节点/接入点级）。
	CircuitBreakerThreshold = 5
	// 熔断基准冷却时长，随失败次数指数退避。
	CircuitCooldownBase = 1 * time.Second
	CircuitCooldownMax  = 60 * time.Second
)

// Account 对应一个上游供应商账号（含其 API Key）。
// 真正请求上游时，网关用该 Key 替换下游传来的子 Key。
// 账号自身不参与流控与熔断；仅提供 active/disabled 开关 + 统计聚合。
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`      // 供应商 id（ark | openai | custom…），默认 ark
	BaseURL      string `json:"base_url"`      // 覆盖 provider 默认 base；custom 必填
	ArkAPIKeyEnc string `json:"-"`             // 加密后的 API Key（不透明字符串），不外发
	KeyHint      string `json:"key_hint"`      // 末 4 位，用于 UI 展示
	Status       string `json:"status"`        // active | disabled
	Weight       int    `json:"weight"`        // 账号级默认权重（其下叶节点 weight=0 时回落到此）
	CreatedAt    int64  `json:"created_at"`
	LastUsedAt   int64  `json:"last_used_at"`

	// 能力覆盖（三态）：0 继承 provider 默认（Go 零值即继承），1 强制是，-1 强制否。
	// custom 等方言服务器的 responses/images 能力参差，需要账号级纠偏。
	CapResponses int `json:"cap_responses"`
	CapImages    int `json:"cap_images"`

	// 累计统计（聚合其下所有接入点，用于「从账号视角」管理）
	TotalRequests    int64 `json:"total_requests"`
	SuccessRequests  int64 `json:"success_requests"`
	FailRequests     int64 `json:"fail_requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	TotalImages      int64 `json:"total_images"`
}

// RouterRule 虚拟路由模型的一条分流规则：估算输入 tokens ≤ MaxInputTokens 时，
// 请求被交给 Target（易读模型名）。规则按阈值升序保存，命中取第一条满足的；
// 输入超过全部阈值时使用 RouterConfig.DefaultTarget。
type RouterRule struct {
	MaxInputTokens int64  `json:"max_input_tokens"` // 阈值（估算输入 tokens，0 = 仅空输入命中）
	Target         string `json:"target"`           // 目标易读模型名（文本或路由模型）
}

// RouterConfig 虚拟路由模型（Type=router）的分流配置。
// 输入长度为粗估（见 gateway 的估算规则），只用于选路，不参与计量计费。
type RouterConfig struct {
	Rules         []RouterRule `json:"rules"`          // 按阈值升序；命中取第一条满足的
	DefaultTarget string       `json:"default_target"` // 超过全部阈值时的目标
}

// Model 易读模型名目录（下游看到的名字，例如 doubao-seed-1-6）。
// Type 区分文本/图像/路由：图像模型只能被 /v1/images/generations 调用，
// 文本模型只能被 chat/responses 调用；路由模型是虚拟名字，由网关在入口
// 按输入长度解析成真实目标后再走正常路由。
// Fallback 是该模型在所有账号都不可用时的有序 fallback 链（仅限同 Type）：
// 请求该模型 → 若所有 {账号, 模型标识} 元组均不可用 → 按顺序尝试 Fallback 里的模型。
// 价格字段用于成本核算：文本模型填 input/output（每百万 token 单价），
// 图像模型填 image（每张单价）；0 表示未定价（成本计 0）。
type Model struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // text | image | router（默认 text）
	Display     string   `json:"display"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Fallback    []string `json:"fallback"`
	CreatedAt   int64    `json:"created_at"`

	PriceInput  float64 `json:"price_input"`  // 输入 token 单价：$ / 1M tokens
	PriceOutput float64 `json:"price_output"` // 输出 token 单价：$ / 1M tokens
	PriceImage  float64 `json:"price_image"`  // 图像单价：$ / 张

	// 能力上限（0 = 未设置：不校验，允许目录按模型名自动补全；人工填写的值优先）。
	ContextTokens   int64 `json:"context_tokens"`    // 上下文窗口（tokens）
	MaxOutputTokens int64 `json:"max_output_tokens"` // 单次最大输出（tokens），非 0 时网关裁剪 max_tokens

	// 虚拟路由配置（仅 Type=router 使用；nil = 无配置）。
	Router *RouterConfig `json:"router,omitempty"`
}

// Endpoint 是树上的叶节点：账号（父）× 上游模型标识 EP，服务某个易读模型名。
// 限流（并发/RPM/TPM）与熔断都落在这一层，是最小的流控与路由单元。
// EP 是不透明字符串：Ark 的 ep-xxx、OpenAI 的 gpt-4o 等都放这里。
type Endpoint struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"` // 父节点
	Model     string `json:"model"`      // 易读模型名
	EP        string `json:"ep"`         // 上游模型标识 / 接入点（不透明字符串）
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`

	// 叶节点级流量控制
	Weight         int   `json:"weight"` // 0 = 继承账号权重
	MaxConcurrency int   `json:"max_concurrency"`
	RPMLimit       int   `json:"rpm_limit"`
	TPMLimit       int64 `json:"tpm_limit"` // 文本=tokens/min；图像=张/min

	// 叶节点级累计统计
	LastUsedAt       int64 `json:"last_used_at"`
	TotalRequests    int64 `json:"total_requests"`
	SuccessRequests  int64 `json:"success_requests"`
	FailRequests     int64 `json:"fail_requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	TotalImages      int64 `json:"total_images"`

	// 运行时状态（仅内存，由 balancer 维护；json 不直接暴露内部窗口）
	Runtime *EndpointRuntime `json:"-"`
	// 快照用：序列化时暴露给管理 UI 的运行态概要。
	RuntimeInfo *EndpointRuntimeInfo `json:"runtime,omitempty"`
}

// EndpointRuntime 叶节点运行时状态：并发计数、熔断、RPM/TPM 窗口。
type EndpointRuntime struct {
	Concurrency         int32
	ConsecutiveFailures int32
	CircuitOpenUntil    int64 // unix nano；0 表示未熔断
	RPM                 *Window
	TPM                 *Window
}

// EndpointRuntimeInfo 是 EndpointRuntime 的 JSON 可见快照。
type EndpointRuntimeInfo struct {
	Concurrency     int32 `json:"concurrency"`
	CircuitOpen     bool  `json:"circuit_open"`      // 是否熔断中
	CircuitRemainMS int64 `json:"circuit_remain_ms"` // 剩余冷却毫秒
	RPMCurrent      int64 `json:"rpm_current"`
	TPMCurrent      int64 `json:"tpm_current"`
}

// EnsureRuntime 初始化叶节点运行时状态（由 balancer 在持锁的 Refresh 中调用，避免数据竞争）。
func (e *Endpoint) EnsureRuntime() {
	if e.Runtime == nil {
		e.Runtime = &EndpointRuntime{
			RPM: NewWindow(time.Minute),
			TPM: NewWindow(time.Minute),
		}
	}
}

// EffectiveWeight 返回叶节点实际参与 WRR 的权重：叶权重 > 账号权重 > 1。
func (e *Endpoint) EffectiveWeight(acc *Account) int {
	if e.Weight > 0 {
		return e.Weight
	}
	if acc != nil && acc.Weight > 0 {
		return acc.Weight
	}
	return 1
}

// SubKey 下游调用方使用的子 Key（sk-xxx）。
type SubKey struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Key              string   `json:"key"` // sk-xxx，明文，供用户复制
	KeyHash          string   `json:"-"`
	Enabled          bool     `json:"enabled"`
	AllowedModels    []string `json:"allowed_models"`     // 空 = 全部
	AllowedAccounts  []string `json:"allowed_accounts"`   // 空 = 全部
	DailyLimitTokens int64    `json:"daily_limit_tokens"` // 0 = 不限
	DailyLimitImages int64    `json:"daily_limit_images"` // 0 = 不限
	ExpiresAt        int64    `json:"expires_at"`
	CreatedAt        int64    `json:"created_at"`
	LastUsedAt       int64    `json:"last_used_at"`
	TotalRequests    int64    `json:"total_requests"`
	TotalTokens      int64    `json:"total_tokens"`
	TotalImages      int64    `json:"total_images"`
}

// UsageLog 单次请求日志（用于管理页日志列表）。
type UsageLog struct {
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	SubKeyID         string `json:"subkey_id"`
	SubKeyName       string `json:"subkey_name"`
	AccountID        string `json:"account_id"`
	AccountName      string `json:"account_name"`
	Provider         string `json:"provider"`        // 命中账号的供应商
	EndpointID       string `json:"endpoint_id"`
	EP               string `json:"ep"`              // 实际调用的上游模型标识
	RequestedModel   string `json:"requested_model"` // 客户端请求的模型名
	Model            string `json:"model"`           // 实际路由到的模型名（fallback 后）
	Modality         string `json:"modality"`        // text | image
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	ImageCount       int64  `json:"image_count"`
	Cost             float64 `json:"cost"` // 本次请求成本（按模型定价折算；未定价为 0）
	Status           string `json:"status"` // ok | error
	LatencyMs        int64  `json:"latency_ms"`
	Error            string `json:"error"`
	ClientIP         string `json:"client_ip"` // 下游调用方 IP（代理场景取 XFF 首跳）
}
