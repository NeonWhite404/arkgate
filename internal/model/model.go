// Package model 定义 ArkGate 的核心数据结构，供各包共享。
//
// 拓扑：树状结构。
//   - 叶节点 = 接入点 Endpoint（账号 × 真实 ep-xxx，服务某个易读模型名）。
//     限流（并发/RPM/TPM）与熔断均只落在叶节点上。
//   - 父节点 = 账号 Account（火山方舟账号，持有长效 API Key）。
//     账号不单独限流、不单独熔断；只有 active/disabled 状态 + 统计聚合，
//     管理员可从账号视角查看其下所有接入点的汇总统计。
//
// 路由语义：客户端用自己的子 Key + 想请求的易读模型名；网关在该模型名对应的
// 所有「叶节点（ep 接入点）」里做加权轮询，命中一个叶子后替换子 Key → 账号真实
// Key，并把 model 换成真实 ep-xxx 转发。
package model

import "time"

// 账号状态
const (
	AccountActive   = "active"
	AccountDisabled = "disabled"
)

// 熔断与冷却相关常量。
const (
	// 连续失败达到该次数后熔断（仅叶节点/接入点级）。
	CircuitBreakerThreshold = 5
	// 熔断基准冷却时长，随失败次数指数退避。
	CircuitCooldownBase = 1 * time.Second
	CircuitCooldownMax  = 60 * time.Second
)

// Account 对应一个火山方舟账号（含其长效 API Key）。
// 真正请求火山时，网关用该 Key 替换下游传来的子 Key。
// 账号自身不参与流控与熔断；仅提供 active/disabled 开关 + 统计聚合。
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ArkAPIKeyEnc string `json:"-"`        // 加密后的 API Key，不外发
	KeyHint      string `json:"key_hint"` // 末 4 位，用于 UI 展示
	Status       string `json:"status"`   // active | disabled
	Weight       int    `json:"weight"`   // 账号级默认权重（其下叶节点 weight=0 时回落到此）
	CreatedAt    int64  `json:"created_at"`
	LastUsedAt   int64  `json:"last_used_at"`

	// 累计统计（聚合其下所有接入点，用于「从账号视角」管理）
	TotalRequests    int64 `json:"total_requests"`
	SuccessRequests  int64 `json:"success_requests"`
	FailRequests     int64 `json:"fail_requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Model 易读模型名目录（下游看到的名字，例如 doubao-seed-1-6）。
// Fallback 是该模型在所有账号都不可用时的有序 fallback 链：
// 请求该模型 → 若所有 {账号, ep} 元组均不可用 → 按顺序尝试 Fallback 里的模型。
type Model struct {
	Name        string   `json:"name"`
	Display     string   `json:"display"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Fallback    []string `json:"fallback"`
	CreatedAt   int64    `json:"created_at"`
}

// Endpoint 是树上的叶节点：账号（父）× 真实接入点 ep-xxx，服务某个易读模型名。
// 限流（并发/RPM/TPM）与熔断都落在这一层，是最小的流控与路由单元。
type Endpoint struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"` // 父节点
	Model     string `json:"model"`      // 易读模型名
	EP        string `json:"ep"`         // 真实 ep-xxx 或火山 Model ID
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`

	// 叶节点级流量控制
	Weight         int   `json:"weight"` // 0 = 继承账号权重
	MaxConcurrency int   `json:"max_concurrency"`
	RPMLimit       int   `json:"rpm_limit"`
	TPMLimit       int64 `json:"tpm_limit"`

	// 叶节点级累计统计
	LastUsedAt       int64 `json:"last_used_at"`
	TotalRequests    int64 `json:"total_requests"`
	SuccessRequests  int64 `json:"success_requests"`
	FailRequests     int64 `json:"fail_requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// 运行时状态（仅内存，由 balancer 维护；json 不直接暴露内部窗口）
	Runtime *EndpointRuntime `json:"-"`
	// 快照用：序列化时暴露给管理 UI 的运行态概要。
	RuntimeInfo *EndpointRuntimeInfo `json:"runtime,omitempty"`
	// Synthetic 标记「透传合成的临时叶子」（非持久化，无 ep_ ID）。
	Synthetic bool `json:"-"`
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
	ExpiresAt        int64    `json:"expires_at"`
	CreatedAt        int64    `json:"created_at"`
	LastUsedAt       int64    `json:"last_used_at"`
	TotalRequests    int64    `json:"total_requests"`
	TotalTokens      int64    `json:"total_tokens"`
}

// UsageLog 单次请求日志（用于管理页日志列表）。
type UsageLog struct {
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	SubKeyID         string `json:"subkey_id"`
	SubKeyName       string `json:"subkey_name"`
	AccountID        string `json:"account_id"`
	AccountName      string `json:"account_name"`
	EndpointID       string `json:"endpoint_id"`
	EP               string `json:"ep"`              // 真实调用到火山的 ep-xxx（或 Model ID）
	RequestedModel   string `json:"requested_model"` // 客户端请求的模型名
	Model            string `json:"model"`           // 实际路由到的模型名（fallback 后）
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Status           string `json:"status"` // ok | error
	LatencyMs        int64  `json:"latency_ms"`
	Error            string `json:"error"`
}
