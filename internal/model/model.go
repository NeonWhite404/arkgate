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
	ModelTypeText  = "text"
	ModelTypeImage = "image"
	// ModelTypeRouter 虚拟路由模型：不承接上游流量（没有接入点映射），
	// 网关按「估算输入长度」把发往该名字的请求分流到其它真实模型。
	// 参考 OpenAI 的 auto / 各家 router 类模型：客户端只管请求这个名字，
	// 具体打哪个模型由网关在入口解析决定。
	ModelTypeRouter = "router"
)

// 模型的上游协议（Model.Provider）。供应商类型从账号下沉到模型：
// 同一个上游主机（账号）可能同时供应 OpenAI 协议的 gpt 系与 Anthropic
// 协议的 claude 系模型，用哪个协议说话由模型决定，账号只提供主机与密钥。
const (
	// ModelProtocolOpenAI 是空值的语义：OpenAI 兼容协议（chat/completions），
	// 覆盖 ark / openai / custom 三类账号供应商——它们在协议层是同一种方言。
	ModelProtocolOpenAI = ""
	// ModelProtocolAnthropic 走 Anthropic /v1/messages 协议：
	// 网关把下游的 OpenAI 请求转换成 messages 格式（响应/流式反向转换）。
	ModelProtocolAnthropic = "anthropic"
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
	Provider     string `json:"provider"` // 供应商 id（ark | openai | custom…），默认 ark
	BaseURL      string `json:"base_url"` // 覆盖 provider 默认 base；custom 必填
	ArkAPIKeyEnc string `json:"-"`        // 加密后的 API Key（不透明字符串），不外发
	KeyHint      string `json:"key_hint"` // 末 4 位，用于 UI 展示
	Status       string `json:"status"`   // active | disabled
	Weight       int    `json:"weight"`   // 账号级默认权重（其下叶节点 weight=0 时回落到此）
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
	Name string `json:"name"`
	Type string `json:"type"` // text | image | router（默认 text）
	// Provider 上游协议（供应商类型已从账号下沉到模型）："" = OpenAI 兼容
	// （默认，覆盖 ark/openai/custom 账号）；"anthropic" = Anthropic /v1/messages，
	// 由网关做 OpenAI↔Anthropic 双向转换。账号只提供主机（base_url）与密钥。
	Provider    string   `json:"provider"`
	Display     string   `json:"display"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Fallback    []string `json:"fallback"`
	CreatedAt   int64    `json:"created_at"`

	PriceInput  float64 `json:"price_input"`  // 输入 token 单价：$ / 1M tokens
	PriceOutput float64 `json:"price_output"` // 输出 token 单价：$ / 1M tokens
	PriceImage  float64 `json:"price_image"`  // 图像单价：$ / 张

	// 缓存 token 单价（$ / 1M），0 = 未设置。
	//
	// 为什么必须单列：上游报告的 prompt_tokens 是**含缓存命中的总量**，
	// 而各家对缓存的定价与原价不同（Anthropic 缓存读取约为输入价 10%、
	// 写入约 125%；OpenAI 读取约 50%）。只用一个 PriceInput 会在两个方向同时算错：
	// 命中部分按原价**多收**，而缓存写入实际**漏收**。
	//
	// 0 = 未设置：回落到 PriceInput，等价于旧行为，不改变历史账单口径。
	PriceCacheRead  float64 `json:"price_cache_read"`
	PriceCacheWrite float64 `json:"price_cache_write"`

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

	// UpstreamDeleted 上游模型检查器的发现状态：最近一次成功探测中，该 EP 已
	// 不在账号上游的模型列表里。它是「观察结果」而不是路由开关——不影响网关
	// 选路、不自动停用、与熔断/限流互不干扰；仅用于管理界面提示，且随检查器
	// 后续成功探测自动恢复（false）。与 Enabled（用户手动启停）语义独立，
	// 普通编辑请求不应覆盖它（仅检查器的批量同步有权写入）。
	UpstreamDeleted bool `json:"upstream_deleted"`

	// SkipUpstreamCheck 本条接入点的豁免：开启后检查器不再对它做任何状态写入
	// （既不标记「上游已删除」也不恢复）。适用于上游 GET /models 列表不完整
	// （例如只返回模型名、不含 ep-* 接入点）而该接入点实际可用的场景。
	// 粒度是接入点而非模型：同一模型下不同账号/不同 ep 可各自决定（某账号的
	// /models 不完整、另一账号完整时互不牵连）。豁免只影响检查器——不改变路由、
	// 限流与统计；开启时既有的「上游已删除」标记会被清除（豁免 = 不观察也不展示）。
	SkipUpstreamCheck bool `json:"skip_upstream_check"`

	// 叶节点级流量控制
	Weight         int               `json:"weight"` // 0 = 继承账号权重
	MaxConcurrency int               `json:"max_concurrency"`
	RPMLimit       int               `json:"rpm_limit"`
	TPMLimit       int64             `json:"tpm_limit"` // 文本=tokens/min；图像=张/min
	RequestHeaders map[string]string `json:"request_headers,omitempty"`

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
	// Probing 为 1 表示「冷却已到期，正有一个探测请求在试」——即 HalfOpen 态。
	//
	// 为什么需要它：没有探测闸门时，冷却到期那一瞬间所有被压住的请求会**同时**
	// 涌向刚恢复（或压根没恢复）的上游（探针实测：50 个并发请求 50 个全部放行）。
	// 后果是双向的：上游只是抖动 → 涌入的流量把它再次打挂，退避被反复重置；
	// 上游真没恢复 → 这批请求全失败、立刻重新熔断，等于白等一轮冷却。
	// HalfOpen 只放**一个**请求去探，用最小代价换取「不集体冲击上游」。
	Probing int32
	// ProbeStartedAt 是探测租约的开始时间（unix nano），供判断探测者是否失联，
	// 避免叶子永久卡在 HalfOpen（一个请求都不放）。
	ProbeStartedAt int64
	RPM     *Window
	TPM     *Window
}

// EndpointRuntimeInfo 是 EndpointRuntime 的 JSON 可见快照。
type EndpointRuntimeInfo struct {
	Concurrency     int32 `json:"concurrency"`
	CircuitOpen     bool  `json:"circuit_open"`      // 是否熔断中
	CircuitRemainMS int64 `json:"circuit_remain_ms"` // 剩余冷却毫秒
	// HalfOpen 表示冷却已到期、正在放行一个探测请求试上游（界面可展示为「探测中」）。
	HalfOpen  bool `json:"half_open"`
	RPMCurrent int64 `json:"rpm_current"`
	TPMCurrent int64 `json:"tpm_current"`
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
	// ── 配额周期（v15）──
	// QuotaPeriod: ""|"day" = 自然日（与旧行为等价）；"week"；"month"。
	// 窗口起点永远落在自然日上（自然日是最小单位，不做小时/滚动窗口），
	// 因此仍复用 usage_daily.day 存窗口起始日，无需新表。
	QuotaPeriod string `json:"quota_period"`
	// QuotaResetWeekday 仅 week 生效，用 ISO 8601 编号：1=周一 .. 7=周日。
	// **0 = 未设置**（等价周一）——刻意不用 time.Weekday 的 0=周日，
	// 否则「未设置的零值」与「显式选周日」无法区分，会导致
	// 零值构造的 SubKey 与从库读出的 SubKey 语义不一致。
	QuotaResetWeekday int `json:"quota_reset_weekday"`
	// QuotaResetHour 每日切分点（0-23，本地时区）。
	// 用途：跨时区部署时统一重置时刻（例如设 8 表示每天早上 8 点开始新周期）。
	QuotaResetHour int `json:"quota_reset_hour"`
	// ── 缓存倍率（v15，千分比；0 = 未设置用默认）──
	//
	// 为什么需要：prompt_tokens 含缓存命中量，而缓存读单价通常只有输入的 10%
	// （OpenAI 系为 50%）。按原价计入额度会让「缓存用得多」反而更快撞限额——
	// 与 v14 的计费口径（已按缓存价拆分）自相矛盾。
	//
	// 用千分比整数而非浮点：与「0 = 未设置」惯例一致（零值安全）、
	// 无浮点相等比较与 JSON 精度问题。1000 = 1.0x。
	CacheReadPermille  int64 `json:"cache_read_permille"`
	CacheWritePermille int64 `json:"cache_write_permille"`
	// WindowLimitRequests 窗口内请求数上限（0 = 不限）。
	// 高频小请求是比 token 更常见的滥用形态，token 限额抓不住。
	WindowLimitRequests int64 `json:"window_limit_requests"`
	ExpiresAt           int64 `json:"expires_at"`
	CreatedAt        int64    `json:"created_at"`
	LastUsedAt       int64    `json:"last_used_at"`
	TotalRequests    int64    `json:"total_requests"`
	TotalTokens      int64    `json:"total_tokens"`
	TotalImages      int64    `json:"total_images"`
}

// UsageLog 单次请求日志（用于管理页日志列表）。
type UsageLog struct {
	ID               int64   `json:"id"`
	TS               int64   `json:"ts"`
	SubKeyID         string  `json:"subkey_id"`
	SubKeyName       string  `json:"subkey_name"`
	AccountID        string  `json:"account_id"`
	AccountName      string  `json:"account_name"`
	Provider         string  `json:"provider"` // 命中账号的供应商
	EndpointID       string  `json:"endpoint_id"`
	EP               string  `json:"ep"`              // 实际调用的上游模型标识
	RequestedModel   string  `json:"requested_model"` // 客户端请求的模型名
	Model            string  `json:"model"`           // 实际路由到的模型名（fallback 后）
	Modality         string  `json:"modality"`        // text | image
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	ImageCount       int64   `json:"image_count"`
	Cost             float64 `json:"cost"` // 本次请求成本（按模型定价折算；未定价为 0）
	// 成本拆分（v13）：input/output/cache 三部分之和即 Cost（computeCost 同时写四者，
	// 保证「拆分之和 == 总额」这一不变量，避免两处口径漂移）。
	InputCost  float64 `json:"input_cost"`
	OutputCost float64 `json:"output_cost"`
	CacheCost  float64 `json:"cache_cost"`
	// 缓存 token（v13）：prompt cache 的写入/读取量，用于命中率与缓存成本分析。
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	// ErrorKind 错误分类（v13）。见 model.ErrorKind* 常量；成功为空串。
	// 比自由文本 Error 更适合聚合：日志文本会带上游请求 id/模型标识，无法直接分组。
	ErrorKind    string `json:"error_kind"`
	IsStream     bool   `json:"is_stream"`
	Status       string `json:"status"` // ok | error
	LatencyMs    int64  `json:"latency_ms"`
	FirstTokenMs int64  `json:"first_token_ms"` // 首字耗时（流式首字节；非流式为 0）
	Error        string `json:"error"`
	ClientIP     string `json:"client_ip"`  // 下游调用方 IP（代理场景取 XFF 首跳）
	UserAgent    string `json:"user_agent"` // 下游 UA 摘要（截断；仅管理端可见）
}

// 错误分类枚举（usage_logs.error_kind）。区分的目的是把「不是上游故障」的失败
// 从上游失败率里剔出去——否则成功率和熔断都会被客户端行为污染。
const (
	ErrorKindNone            = ""                  // 成功
	ErrorKindUpstreamError   = "upstream_error"    // 真实上游 4xx/5xx
	ErrorKindUpstreamTimeout = "upstream_timeout"  // 上游整体超时 / 首 token 超时
	ErrorKindClientInvalid   = "client_invalid"    // 请求方参数/内容问题（含流类型错、上下文超限）
	ErrorKindClientCancel    = "client_cancel"     // 下游断连（context canceled）
	ErrorKindLocal           = "local_error"       // 无账号/全熔断/限流/无能力/转换拒绝
)
