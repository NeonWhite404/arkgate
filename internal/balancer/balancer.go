// Package balancer 实现多账号负载均衡。
//
// 拓扑：树状结构。
//   - 叶节点 = 接入点 Endpoint（账号 × 上游模型标识）。限流（并发/RPM/TPM）与熔断只落在叶节点。
//   - 父节点 = 账号 Account。不做流控、不做熔断，只提供 active/disabled 开关与统计聚合。
//
// 并发模型：
//   - Go net/http 每个请求一个 goroutine，转发全程无全局串行锁。
//   - 叶节点运行态（并发、连续失败、熔断、RPM/TPM）统一在「持锁 Refresh」时惰性初始化，
//     之后对 int32/int64 用 atomic、对窗口用独立 mutex，读侧用 RLock 快照，无数据竞争。
//   - 用量统计与日志经有界 channel 的「阻塞投递」落库（背压）——不丢弃，避免子 Key 日限额被绕过。
//
// 路由决策（Select）：
//  1. 客户端给出易读模型名 model + 账号白名单 allowed + 排除集 exclude + 目标 API；
//  2. 收集「model」下所有可用叶节点（账号 active、叶 enabled、账号对该 API 有能力、
//     未熔断、未超并发/RPM/TPM）；
//  3. 在候选叶节点上做平滑加权轮询（权重：叶.weight > 账号.weight > 1）；
//  4. 返回命中的叶节点（账号 + 上游模型标识），调用方据此转发。
//
// 不透明字符串：模型标识不做任何前缀识别——路由的唯一真源是映射表，
// 无映射即报错（无透传逃生通道）。
package balancer

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/provider"
	"arkgate/internal/store"
)

var (
	// ErrNoAccount 表示没有可用账号。
	ErrNoAccount = errors.New("没有可用账号")
	// ErrNoEndpoint 表示易读模型名没有对应的接入点映射。
	ErrNoEndpoint = errors.New("模型没有对应的接入点")
	// ErrAllThrottled 表示所有候选叶节点都被限流或熔断。
	ErrAllThrottled = errors.New("所有可用接入点均被限流或熔断")
	// ErrNoCapable 表示模型有映射，但没有任何账号支持目标 API（responses/images）。
	ErrNoCapable = errors.New("该模型没有支持此 API 的账号映射")
)

// maxFallbackChain 限制 fallback 链展开后的总长度，防止配置成环或过深时
// 单次请求退避到无限多个模型上。
const maxFallbackChain = 16

// API 标记请求打向哪类对外接口，用于路由时的账号能力过滤。
type API int

const (
	APIChat API = iota
	APIResponses
	APIImages
)

// Balancer 是负载均衡器。
type Balancer struct {
	mu        sync.RWMutex
	accounts  map[string]*model.Account    // accountID -> account
	models    map[string]*model.Model      // 易读名 -> model（仅启用）
	endpoints map[string]*model.Endpoint   // endpointID -> endpoint（叶节点）
	modelApps map[string][]*model.Endpoint // 易读名 -> 该模型下全部叶子
	defs      map[string]provider.Def      // accountID -> 供应商定义（Refresh 时解析缓存）
	prices    map[string]modelPrice         // 易读名 -> 单价（含停用模型，供成本核算）
	limits    map[string][2]int64          // 易读名 -> [上下文,最大输出] 上限（含停用模型；0=不校验）

	wrrMu    sync.Mutex
	wrrState map[string]*wrrState

	sessMu    sync.Mutex
	sessions  map[string]sessEntry // 粘性会话：stickyKey -> 命中叶节点
	sessionTTL time.Duration

	logCh  chan *model.UsageLog
	statCh chan statOp
	// 通道満导致的同步落库次数（atomic 自增：Record 在并发请求路径上）。
	// 非零即「落库跟不上」的信号，供管理端观测。
	statsOverflow atomic.Int64
	logsOverflow  atomic.Int64
	done          chan struct{}
	wg     sync.WaitGroup
	store  *store.Store
	once   sync.Once
}

type wrrState struct {
	mu      sync.Mutex
	current map[string]int
	total   int
}

// sessEntry 粘性会话条目：记住上次命中的叶节点与时间。
type sessEntry struct {
	endpointID string
	ts         time.Time
}

// statChCap / logChCap 统计通道容量。
//
// 2024 设计回顾：原先通道满时会在 HTTP 请求路径上**永久阻寨**（阻塞投递）。
// 后果是「一个卡住的数据库写入会把响应拖延」——但此时客户端的回复已经发出、
// 上游已经计费，阻塞只会把可观测故障放大成全线拖慢（实测：把 consume 停掉后
// Record 在第 4097 次调用上永久挂住）。改成「优先入队，满则直接同步落库」，
// 既不丢数据也不阻寨转发。
const (
	statChCap = 4096
	logChCap  = 4096
)

type statOp struct {
	accountID  string
	endpointID string
	subkeyID   string
	ok         bool
	prompt     int64
	completion int64
	images     int64
	cost       float64
}

// New 构造 Balancer 并启动后台消费者。sessionTTL 为会话粘性时长（0 = 关闭）。
func New(st *store.Store, sessionTTL time.Duration) *Balancer {
	b := &Balancer{
		accounts:   map[string]*model.Account{},
		models:     map[string]*model.Model{},
		endpoints:  map[string]*model.Endpoint{},
		modelApps:  map[string][]*model.Endpoint{},
		defs:       map[string]provider.Def{},
		prices:     map[string]modelPrice{},
		limits:     map[string][2]int64{},
		wrrState:   map[string]*wrrState{},
		sessions:   map[string]sessEntry{},
		sessionTTL: sessionTTL,
		logCh:      make(chan *model.UsageLog, logChCap),
		statCh:     make(chan statOp, statChCap),
		done:       make(chan struct{}),
		store:      st,
	}
	b.Refresh()
	b.wg.Add(1)
	go b.consume()
	return b
}

// Close 停止后台消费者。
func (b *Balancer) Close() {
	b.once.Do(func() {
		close(b.done)
		b.wg.Wait()
	})
}

func (b *Balancer) consume() {
	defer b.wg.Done()
	for {
		select {
		case <-b.done:
			b.drain()
			return
		case l := <-b.logCh:
			_ = b.store.AddUsageLog(l)
		case op := <-b.statCh:
			b.applyStat(op)
		}
	}
}

func (b *Balancer) applyStat(op statOp) {
	if b.store == nil {
		// 无 store（单测构造）：只同步内存副本，不碰数据库。
		b.accumulateInMemory(op)
		return
	}
	if op.endpointID != "" {
		_ = b.store.AccumulateEndpoint(op.endpointID, op.ok, op.prompt, op.completion)
	}
	if op.accountID != "" {
		_ = b.store.AccumulateAccount(op.accountID, op.ok, op.prompt, op.completion)
	}
	if op.subkeyID != "" {
		_ = b.store.AccumulateSubKey(op.subkeyID, op.ok, op.prompt, op.completion)
	}
	if op.images > 0 {
		_ = b.store.AccumulateImages(op.accountID, op.endpointID, op.subkeyID, op.images)
		_ = b.store.AddDailyUsage(op.subkeyID, op.prompt+op.completion, op.images, 1, op.cost)
	} else {
		_ = b.store.AddDailyUsage(op.subkeyID, op.prompt+op.completion, 0, 1, op.cost)
	}
	// 落库的同时同步内存副本：Snapshot* 读的是 b.accounts / b.endpoints，
	// 若只写 DB，管理 UI 会一直显示上次 Refresh 时的陈旧统计。
	b.accumulateInMemory(op)
}

// accumulateInMemory 把一次统计增量同步到内存中的账号/叶节点副本，
// 使 SnapshotAccounts / SnapshotEndpoints 无需 Refresh 即可反映最新用量。
func (b *Balancer) accumulateInMemory(op statOp) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().Unix()
	if a, ok := b.accounts[op.accountID]; ok {
		bumpStats(&a.TotalRequests, &a.SuccessRequests, &a.FailRequests,
			&a.PromptTokens, &a.CompletionTokens, &a.TotalTokens, op)
		a.TotalImages += op.images
		a.LastUsedAt = now
	}
	if e, ok := b.endpoints[op.endpointID]; ok {
		bumpStats(&e.TotalRequests, &e.SuccessRequests, &e.FailRequests,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, op)
		e.TotalImages += op.images
		e.LastUsedAt = now
	}
}

// bumpStats 施加一次请求的统计增量（调用方须持有 b.mu 写锁）。
func bumpStats(total, success, fail, prompt, completion, tokens *int64, op statOp) {
	*total++
	if op.ok {
		*success++
	} else {
		*fail++
	}
	*prompt += op.prompt
	*completion += op.completion
	*tokens += op.prompt + op.completion
}

func (b *Balancer) drain() {
	for {
		select {
		case l := <-b.logCh:
			_ = b.store.AddUsageLog(l)
		case op := <-b.statCh:
			b.applyStat(op)
		default:
			return
		}
	}
}

// Refresh 从 store 重载账号/模型/叶子，重建内存索引。
// 管理端增删改后调用。叶节点 Runtime 在持锁状态下惰性初始化并按 ID 保留。
func (b *Balancer) Refresh() {
	accounts, _ := b.store.ListAccounts()
	modelsList, _ := b.store.ListModels()
	endpoints, _ := b.store.ListEndpoints()

	accMap := map[string]*model.Account{}
	modMap := map[string]*model.Model{}
	epMap := map[string]*model.Endpoint{}
	modelApps := map[string][]*model.Endpoint{}
	defMap := map[string]provider.Def{}
	priceMap := map[string]modelPrice{}
	limitMap := map[string][2]int64{}

	for _, a := range accounts {
		accMap[a.ID] = a
		defMap[a.ID] = resolveDef(a)
	}
	for _, m := range modelsList {
		// 价格索引包含停用模型：日志里的历史用量仍要按当时定价折算。
		priceMap[m.Name] = modelPrice{
			in: m.PriceInput, out: m.PriceOutput, image: m.PriceImage,
			cacheRead: m.PriceCacheRead, cacheWrite: m.PriceCacheWrite,
		}
		limitMap[m.Name] = [2]int64{m.ContextTokens, m.MaxOutputTokens}
		if m.Enabled {
			modMap[m.Name] = m
		}
	}
	for _, e := range endpoints {
		if !e.Enabled {
			continue
		}
		epMap[e.ID] = e
		if e.Model != "" {
			modelApps[e.Model] = append(modelApps[e.Model], e)
		}
	}

	b.mu.Lock()
	// 保留旧叶子的 Runtime；新叶子在锁内初始化。
	for id, old := range b.endpoints {
		if ne, ok := epMap[id]; ok && old.Runtime != nil {
			ne.Runtime = old.Runtime
		}
	}
	for _, e := range epMap {
		e.EnsureRuntime() // 持锁初始化，杜绝数据竞争
	}
	b.accounts = accMap
	b.models = modMap
	b.endpoints = epMap
	b.modelApps = modelApps
	b.defs = defMap
	b.prices = priceMap
	b.limits = limitMap
	b.mu.Unlock()

	// 清理粘性会话：指向已不存在叶子的、以及超过 TTL 的过期项
	// （读侧只清理被再次访问的键，这里兜底清剩余的，防止长期运行下缓慢膨胀）。
	if b.sessionTTL > 0 {
		b.sessMu.Lock()
		for k, s := range b.sessions {
			if _, ok := epMap[s.endpointID]; !ok || time.Since(s.ts) > b.sessionTTL {
				delete(b.sessions, k)
			}
		}
		b.sessMu.Unlock()
	}
}

// resolveDef 解析账号的供应商定义；未注册的 id 回落为「仅 chat」兜底，
// 避免手改数据库的账号整体失联。
func resolveDef(a *model.Account) provider.Def {
	if d, ok := provider.Get(a.Provider); ok {
		return d
	}
	return provider.FallbackDef(a.Provider)
}

// AccountDef 返回账号的供应商定义（Refresh 时缓存的快照）。
func (b *Balancer) AccountDef(accountID string) (provider.Def, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	d, ok := b.defs[accountID]
	return d, ok
}

// ─────────────────────────── 能力判定 ───────────────────────────

// capEnabled 三态覆盖判定：0 继承默认（Go 零值即继承），1 强制是，-1 强制否。
func capEnabled(cover int, native bool) bool {
	if cover > 0 {
		return true
	}
	if cover < 0 {
		return false
	}
	return native
}

// accountSupports 判断账号是否支持目标 API。
func (b *Balancer) accountSupports(accountID string, api API) bool {
	if api == APIChat {
		return true // chat 是所有 OpenAI 兼容供应商的基础能力
	}
	d, ok := b.defs[accountID]
	if !ok {
		return false
	}
	acc := b.accounts[accountID]
	if acc == nil {
		return false
	}
	switch api {
	case APIResponses:
		return capEnabled(acc.CapResponses, d.Native.Responses)
	case APIImages:
		return capEnabled(acc.CapImages, d.Native.Images)
	}
	return false
}

// ─────────────────────────── 路由 ───────────────────────────

// Select 按「平滑加权轮询 + 熔断 + 限流 + 能力过滤」选出一个可用叶节点。
//
// modelName 为下游请求里的易读模型名；stickyKey 为粘性会话标识（一般为子 Key ID，
// 空 = 不启用粘性）。粘性命中时直接复用上次的叶节点（仍要求其在可用候选中），
// 且不扰动 WRR 计数；未命中时正常 WRR 选举并记住结果。
func (b *Balancer) Select(modelName string, allowed []string, exclude map[string]bool, api API, stickyKey string) (ep *model.Endpoint, err error) {
	// 模型必须在目录里且处于启用状态才承接流量。b.models 只装启用模型，
	// 而 modelApps 是按「端点启用」建的索引——少了这道校验，停用（或已从目录
	// 删除但映射还在）的模型仍会被选中，界面上的「停用」形同虚设。
	// fallback 链上的目标同样过这一关：链只决定尝试顺序，不豁免启用校验。
	if !b.modelNameKnown(modelName) {
		return nil, ErrNoEndpoint
	}
	cands, poolKey := b.usableEndpoints(modelName, allowed, exclude, api)
	if len(cands) == 0 {
		if b.hasAnyEndpoint(modelName, allowed, api) {
			return nil, ErrAllThrottled
		}
		if b.hasAnyEndpointRaw(modelName, allowed) {
			return nil, ErrNoCapable
		}
		if b.modelNameKnown(modelName) {
			return nil, ErrNoAccount
		}
		return nil, ErrNoEndpoint
	}

	// 会话粘性：同 Key + 模型的后续请求在 TTL 内固定到同一叶子，提升上游
	// prompt cache 命中率。粘性命中同样要做并发/RPM 占位。
	//
	// 若粘住的叶子正处于 HalfOpen 探索期，而探测资格已被别人拿走（claimProbe
	// 返回 false），则**不粘**：继续走 WRR 选其它健康叶子。把请求堆在一个
	// 待验证的叶子上没有必要。
	if stickyKey != "" && b.sessionTTL > 0 {
		if pinned := b.sessionPick(stickyKey+"|"+modelName, cands); pinned != nil {
			if b.claimProbe(pinned) {
				atomic.AddInt32(&pinned.Runtime.Concurrency, 1)
				pinned.Runtime.RPM.Add(1)
				return pinned, nil
			}
		}
	}

	// HalfOpen 闸门：优先让「冷却已到期但还没人探测」的叶子先接受一次验证；
	// 同时把「正在探测中（资格已被别人拿走）」的叶子从本次候选里剔除，否则下面的
	// WRR 会把它当普通叶子选中，探测闸门形同虚设（实测：50 个并发全部放行）。
	var probing *model.Endpoint
	cands, probing = b.splitProbeCandidates(cands)
	if probing != nil {
		atomic.AddInt32(&probing.Runtime.Concurrency, 1)
		probing.Runtime.RPM.Add(1)
		return probing, nil
	}
	if len(cands) == 0 {
		// 所有候选都在探测中：本次不放行。返回 ErrAllThrottled（“暂不可用”），
		// 与全部熔断/限流的语义一致，调用方会尝试 fallback 或重试。
		return nil, ErrAllThrottled
	}

	// 权重表只算一次，供 WRR 累计与总数两处共用（此前 getWrrState 与 Select
	// 各算一遍，每次选举多走一轮读锁）。
	weights := b.weightTable(cands)
	state := b.getWrrState(poolKey, weights)

	state.mu.Lock()
	defer state.mu.Unlock()

	var picked *model.Endpoint
	for _, e := range cands {
		w := weights[e.ID]
		state.current[e.ID] += w
		if picked == nil || state.current[e.ID] > state.current[picked.ID] {
			picked = e
		}
	}
	if picked == nil {
		return nil, ErrNoAccount
	}
	state.current[picked.ID] -= state.total

	// 仅在「全新请求」的首次选举上记录粘性会话；重试路径 stickyKey 为空，
	// 不会把失败叶子的接替者错误地钉成新会话。
	if stickyKey != "" && b.sessionTTL > 0 {
		b.sessionPut(stickyKey+"|"+modelName, picked.ID)
	}

	atomic.AddInt32(&picked.Runtime.Concurrency, 1)
	picked.Runtime.RPM.Add(1)

	return picked, nil}

// sessionPick 查询粘性会话：TTL 内且目标叶在候选集中才命中（读时惰性淘汰过期项）。
func (b *Balancer) sessionPick(key string, cands []*model.Endpoint) *model.Endpoint {
	b.sessMu.Lock()
	s, ok := b.sessions[key]
	if ok && time.Since(s.ts) > b.sessionTTL {
		delete(b.sessions, key)
		ok = false
	}
	b.sessMu.Unlock()
	if !ok {
		return nil
	}
	for _, e := range cands {
		if e.ID == s.endpointID {
			return e
		}
	}
	return nil
}

// sessionPut 记录粘性会话。
func (b *Balancer) sessionPut(key, endpointID string) {
	b.sessMu.Lock()
	b.sessions[key] = sessEntry{endpointID: endpointID, ts: time.Now()}
	b.sessMu.Unlock()
}

// FallbackChain 解析某易读模型名的有序 fallback 链。
//
// **只取该模型自己配置的那一列，不向下传递**：链 = [请求名] + 它 Fallback 列表里
// 的每一项（保持配置顺序）。即 A.Fallback=[B,C]、B.Fallback=[D] → [A, B, C]，
// D 不会被拉进来。这样「界面上看到的退避目标」就是「网关实际会尝试的目标」，
// 不会出现管理员从未为 A 配置过、却因中间模型的配置而被打到的模型。
// 去重、排除自身，总长上限 maxFallbackChain。
//
// 同类型约束：请求模型已知时，链上只保留与其 Type 相同的候选
// （文本模型永远不会退避到图像模型，反之亦然）。
func (b *Balancer) FallbackChain(modelName string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if modelName == "" {
		return nil
	}
	chain := []string{modelName}
	root, known := b.models[modelName]
	if !known {
		return chain // 未登记/已停用：无从读取其配置，只保留自身
	}
	seen := map[string]bool{modelName: true}
	for _, f := range root.Fallback {
		if f == "" || seen[f] {
			continue
		}
		// 同类型约束：已登记且跨类型的目标明确跳过（文本永不退避到图像）。
		// 未登记/未启用的目标保留在链上——Select 时自然失败并继续，
		// 维持「配置的每一项都会被尝试」的既有语义。
		if fm, exists := b.models[f]; exists && fm.Type != root.Type {
			continue
		}
		seen[f] = true
		chain = append(chain, f)
		if len(chain) >= maxFallbackChain {
			break
		}
	}
	return chain
}

// SelectWithFallback 先按请求的模型名做常规 Select；若该模型在所有账号的元组都
// 不可用（ErrAllThrottled / ErrNoCapable / ErrNoAccount / ErrNoEndpoint），自动沿
// fallback 链依次尝试后续模型。返回命中的叶节点与实际使用的模型名。
//
// allowedAccounts 为账号白名单（子 Key 限定可用的账号）；allowedModels 为模型
// 白名单（子 Key 限定的模型；非空时，fallback 链里不在白名单的模型会被跳过，
// 避免「子 Key 仅授权模型 A，却经 fallback 打到未授权的模型 B」）。
//
// 链上全部失败时，返回信息量最大的错误（如「全被限流/熔断」优先于「没有映射」），
// 而不是链上最后一个模型的失败原因。
func (b *Balancer) SelectWithFallback(modelName string, allowedAccounts, allowedModels []string, exclude map[string]bool, api API, stickyKey string) (*model.Endpoint, string, error) {
	allowModel := setOf(allowedModels)
	chain := b.FallbackChain(modelName)
	var bestErr error
	for _, name := range chain {
		if len(allowModel) > 0 && !allowModel[name] {
			continue // 子 Key 未授权的 fallback 目标，跳过
		}
		ep, err := b.Select(name, allowedAccounts, exclude, api, stickyKey)
		if err == nil {
			return ep, name, nil
		}
		if selectErrPriority(err) >= selectErrPriority(bestErr) {
			bestErr = err
		}
	}
	return nil, modelName, bestErr
}

// selectErrPriority 错误信息量排序：值越大越值得向客户端呈现。
func selectErrPriority(err error) int {
	switch {
	case errors.Is(err, ErrAllThrottled):
		return 3 // 有容量但全忙——最值得等待重试
	case errors.Is(err, ErrNoCapable):
		return 2 // 有映射但没有账号支持此 API
	case errors.Is(err, ErrNoAccount):
		return 1
	default:
		return 0 // ErrNoEndpoint 等
	}
}

// ─────────────────────────── 虚拟路由模型 ───────────────────────────

// ResolveRouter 解析虚拟路由模型（type=router）：按估算的输入 tokens 选出
// 承接请求的真实目标模型名。非路由模型原样返回（resolved == name，调用方
// 无需区分）。估算口径见 gateway 的 estimateInputTokens——只用于选路，
// 不参与计量计费。
//
// 解析「沿路由链走到底」：目标本身也可以是路由模型（链式分流）。管理端
// 保存时已做环校验，这里再加运行时防线（深度上限 + 回到起点的环检测），
// 防手改数据库造成死循环。任何一跳的目标不存在或已停用（不在启用模型
// 集合 b.models 中）都直接报错，让客户端拿到明确原因，而不是一个含糊的
// 「没有对应的接入点」。
func (b *Balancer) ResolveRouter(name string, inputTokens int64) (string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if m, ok := b.models[name]; !ok || m.Type != model.ModelTypeRouter {
		return name, nil
	}
	cur := name
	for depth := 0; depth < model.MaxRouterDepth; depth++ {
		m := b.models[cur]
		if m == nil || m.Type != model.ModelTypeRouter {
			return cur, nil // 已落到真实模型，解析结束
		}
		next := pickRouterTarget(m.Router, inputTokens)
		if next == "" {
			return "", fmt.Errorf("路由模型 %s 未配置可用的分流规则", cur)
		}
		if _, ok := b.models[next]; !ok {
			return "", fmt.Errorf("路由目标 %s 不存在或已停用", next)
		}
		if next == name {
			return "", fmt.Errorf("路由配置成环（绕回 %s）", name)
		}
		cur = next
	}
	return "", fmt.Errorf("路由链超过 %d 层，拒绝解析（%s）", model.MaxRouterDepth, name)
}

// pickRouterTarget 按估算输入 tokens 命中规则：规则按阈值升序，取第一条
// 「输入 ≤ 阈值」的；输入超过全部阈值时用默认目标。无可用目标返回空串。
func pickRouterTarget(rc *model.RouterConfig, inputTokens int64) string {
	if rc == nil {
		return ""
	}
	for _, r := range rc.Rules {
		if r.Target != "" && inputTokens <= r.MaxInputTokens {
			return r.Target
		}
	}
	return rc.DefaultTarget
}

// ModelType 返回模型类型（仅查启用中的目录；未知/已停用返回空串）。
// 网关入口用它在打上游之前拦截「图像模型打 chat 接口」这类模态错配。
func (b *Balancer) ModelType(name string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if m, ok := b.models[name]; ok {
		return m.Type
	}
	return ""
}

// ModelProtocol 返回模型的上游协议（供应商类型下沉到模型层）：
// "" = OpenAI 兼容（默认），model.ModelProtocolAnthropic = Anthropic /v1/messages。
// 未知/已停用返回空串（选路阶段自然失败）。
func (b *Balancer) ModelProtocol(name string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if m, ok := b.models[name]; ok {
		return m.Provider
	}
	return ""
}

// protocolSupportsAPI 判断某上游协议是否支持目标 API：Anthropic 协议上游
// 只有 messages（chat）能力，responses/images 在模型层直接判不可用——
// 即使账号能力声明支持（那是账号的 OpenAI 协议能力，与协议转换无关）。
func protocolSupportsAPI(protocol string, api API) bool {
	if api == APIChat || protocol != model.ModelProtocolAnthropic {
		return true
	}
	return false
}

// weightTable 计算每个候选叶子的统一权重（叶.weight > 账号.weight > 1）。
func (b *Balancer) weightTable(cands []*model.Endpoint) map[string]int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]int, len(cands))
	for _, e := range cands {
		w := e.Weight
		if w <= 0 {
			if acc, ok := b.accounts[e.AccountID]; ok && acc.Weight > 0 {
				w = acc.Weight
			} else {
				w = 1
			}
		}
		out[e.ID] = w
	}
	return out
}

// usableEndpoints 收集「model 可用」的候选叶节点。
func (b *Balancer) usableEndpoints(modelName string, allowed []string, exclude map[string]bool, api API) ([]*model.Endpoint, string) {
	allow := setOf(allowed)
	b.mu.RLock()
	defer b.mu.RUnlock()

	// 协议能力过滤（模型层）：Anthropic 协议模型不承接 responses/images，
	// fallback 链上的这类目标也会在这里被跳过。
	if !protocolSupportsAPI(b.protocolOfLocked(modelName), api) {
		return nil, poolKeyOf(modelName, allowed)
	}
	now := time.Now()
	var cands []*model.Endpoint
	for _, e := range b.modelApps[modelName] {
		if !b.endpointUsable(e, allow, exclude, now, api) {
			continue
		}
		cands = append(cands, e)
	}
	return cands, poolKeyOf(modelName, allowed)
}

func (b *Balancer) endpointUsable(e *model.Endpoint, allow map[string]bool, exclude map[string]bool, now time.Time, api API) bool {
	if exclude != nil && exclude[e.ID] {
		return false
	}
	acc := b.accounts[e.AccountID]
	if acc == nil || acc.Status != model.AccountActive {
		return false
	}
	if len(allow) > 0 && !allow[e.AccountID] {
		return false
	}
	if !b.accountSupports(e.AccountID, api) {
		return false
	}
	rt := e.Runtime
	if rt == nil { // 防御：极小概率未初始化
		return false
	}
	// 熔断中（冷却未到期）不可选。冷却到期后进入 HalfOpen 候选：不再无条放行，
	// 由 claimProbe 决定谁去做那个探测请求（见该函数）。
	if atomic.LoadInt64(&rt.CircuitOpenUntil) > now.UnixNano() {
		return false
	}
	if e.MaxConcurrency > 0 && atomic.LoadInt32(&rt.Concurrency) >= int32(e.MaxConcurrency) {
		return false
	}
	if e.RPMLimit > 0 && rt.RPM.Count() >= int64(e.RPMLimit) {
		return false
	}
	if e.TPMLimit > 0 && rt.TPM.Sum() >= e.TPMLimit {
		return false
	}
	return true
}

// ── 熔断 HalfOpen（探测态） ──
//
// 背景：冷却到期后原来的实现是**无条放行**。探针实测：50 个并发请求 50 个全部
// 通过。这会把刚恢复（或压根没恢复）的上游瞬间打满：上游只是抖动 → 被涌入流量
// 再次打挂，退避反复重置；上游真没恢复 → 这批请求全失败、立刻重新熔断，白等一轮。
//
// 现在的语义：
//   Closed（未熔断）：正常运行。
//   Open（冷却中）：一个不放行。
//   HalfOpen（冷却到期、探测中）：**只放一个**请求去试，其余仍视为不可用。
//     探测成功 → 立即转 Closed（Record 里清零熔断状态）。
//     探测失败 → 立即转回 Open（Record 里重新置 CircuitOpenUntil）。

// probeTTL 探测态的「租约」时长。
//
// 为什么需要过期：探测请求可能因进程崩溃/连接池异常而永远不回来，
// 若不在一定时间后释放 Probing 标志，该叶子会永久卡在 HalfOpen（一个请求都不放）。
// 取 30 秒：远大于正常请求耗时，又短到不会让故障叶子长期无法恢复探测。
const probeTTL = 30 * time.Second

// claimProbe 尝试认领某叶子的探测资格。返回 true 表示本次请求作为探测请求放行。
//
// 只在「熔断冷却已到期」时有意义：此时该叶处于 HalfOpen 候选，谁抢到谁去试。
// 用 CAS 而非 Load+Store：并发下必须保证**只有一个**胜出，否则又是集体涌入。
func (b *Balancer) claimProbe(e *model.Endpoint) bool {
	rt := e.Runtime
	if rt == nil {
		return false
	}
	// 未熔断过（CircuitOpenUntil == 0）的叶子不是探测对象，直接放行。
	openUntil := atomic.LoadInt64(&rt.CircuitOpenUntil)
	if openUntil == 0 {
		return true
	}
	if openUntil > time.Now().UnixNano() {
		return false // 仍在冷却中（应该到不了这里，双保险）
	}
	// 冷却已到期：抢探测资格。
	if !atomic.CompareAndSwapInt32(&rt.Probing, 0, 1) {
		// 已有人在探测：本请求不放行。注意**不重置** Probing——否则会把
		// 探测者的租约抢走，导致多个请求同时去试。
		return false
	}
	// 记下探测开始时间，供 releaseStaleProbe 判断租约是否过期。
	atomic.StoreInt64(&rt.ProbeStartedAt, time.Now().UnixNano())
	return true
}

// releaseStaleProbe 释放过期未归还的探测租约（探测请求可能崩溃/永不返回）。
// 由 Record 与 selectable 两侧偶尔调用，无需额外协程。
func releaseStaleProbe(rt *model.EndpointRuntime) {
	if rt == nil || atomic.LoadInt32(&rt.Probing) == 0 {
		return
	}
	started := atomic.LoadInt64(&rt.ProbeStartedAt)
	if started == 0 || time.Since(time.Unix(0, started)) > probeTTL {
		// 租约过期：允许后续请求重新探测，避免叶子永久卡在 HalfOpen。
		atomic.StoreInt32(&rt.Probing, 0)
	}
}

// selectedRuntime 返回叶子运行时（可能为 nil）。
func selectedRuntime(e *model.Endpoint) *model.EndpointRuntime {
	if e == nil {
		return nil
	}
	return e.Runtime
}


// splitProbeCandidates 把候选集拆成「可正常参与 WRR 的」与「探测归属」。
//
// 返回：
//   - keep：可正常承接流量的叶子（含从未熔断的，以及探测资格被本次认领到的）。
//   - probe：本次认领到探测资格的叶子（非 nil 时应直接选它并 return）。
//
// 语义要点：处于 HalfOpen 且**别人正在探测**的叶子会从 keep 中剔除——它既不该
// 参与 WRR，也不该被选中，直到探测结束（成功转 Closed / 失败回 Open）。
func (b *Balancer) splitProbeCandidates(cands []*model.Endpoint) (keep []*model.Endpoint, probe *model.Endpoint) {
	keep = make([]*model.Endpoint, 0, len(cands))
	for _, e := range cands {
		rt := e.Runtime
		if rt == nil || atomic.LoadInt64(&rt.CircuitOpenUntil) == 0 {
			keep = append(keep, e) // 从未熔断：普通候选
			continue
		}
		// 熔断过且冷却已到期（selectable 已保证未到期的不会进来）= HalfOpen 候选。
		releaseStaleProbe(rt) // 探测者可能已失联，先释放过期租约
		if probe == nil && atomic.CompareAndSwapInt32(&rt.Probing, 0, 1) {
			atomic.StoreInt64(&rt.ProbeStartedAt, time.Now().UnixNano())
			probe = e
			continue
		}
		// 本次没抢到探测资格（别人在探）：本叶子不参与本轮选举。
	}
	return keep, probe
}

func (b *Balancer) hasAnyEndpoint(modelName string, allowed []string, api API) bool {
	allow := setOf(allowed)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, known := b.models[modelName]; !known {
		return false
	}
	// 协议能力过滤（模型层）口径与 usableEndpoints 一致，保证错误落在
	// ErrNoCapable（「没有支持此 API 的映射」）而不是误导性的 ErrAllThrottled。
	if !protocolSupportsAPI(b.protocolOfLocked(modelName), api) {
		return false
	}
	for _, e := range b.modelApps[modelName] {
		acc, ok := b.accounts[e.AccountID]
		if !ok || acc.Status != model.AccountActive {
			continue
		}
		if len(allow) > 0 && !allow[e.AccountID] {
			continue
		}
		if !b.accountSupports(e.AccountID, api) {
			continue
		}
		return true
	}
	return false
}

// protocolOfLocked 读模型上游协议（调用方须持有 b.mu 读锁及以上）。
func (b *Balancer) protocolOfLocked(modelName string) string {
	if m, ok := b.models[modelName]; ok {
		return m.Provider
	}
	return ""
}

// hasAnyEndpointRaw 判断模型是否存在叶节点（不看能力与运行态），
// 用于区分「有能力但全被限流/熔断」与「根本没有支持此 API 的账号」。
func (b *Balancer) hasAnyEndpointRaw(modelName string, allowed []string) bool {
	allow := setOf(allowed)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, e := range b.modelApps[modelName] {
		acc, ok := b.accounts[e.AccountID]
		if !ok || acc.Status != model.AccountActive {
			continue
		}
		if len(allow) > 0 && !allow[e.AccountID] {
			continue
		}
		return true
	}
	return false
}

// modelNameKnown 判断该易读模型名是否存在于目录。
func (b *Balancer) modelNameKnown(modelName string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.models[modelName]
	return ok
}

func setOf(list []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range list {
		m[s] = true
	}
	return m
}

func poolKeyOf(modelName string, allowed []string) string {
	if len(allowed) == 0 {
		return modelName + "|*"
	}
	cp := append([]string(nil), allowed...)
	sort.Strings(cp)
	return modelName + "|" + strings.Join(cp, ",")
}

// getWrrState 取（或建）该选举池的 WRR 状态，并按给定权重表刷新总权重。
// weights 由调用方（Select）算好传入，保证与选举用的是同一张表。
func (b *Balancer) getWrrState(poolKey string, weights map[string]int) *wrrState {
	b.wrrMu.Lock()
	defer b.wrrMu.Unlock()
	st, ok := b.wrrState[poolKey]
	if !ok {
		st = &wrrState{current: map[string]int{}}
		b.wrrState[poolKey] = st
	}
	total := 0
	for _, w := range weights {
		total += w
	}
	st.total = total
	return st
}

// Release 在请求完成后释放叶节点并发占位。
func (b *Balancer) Release(e *model.Endpoint) {
	if e == nil || e.Runtime == nil {
		return
	}
	atomic.AddInt32(&e.Runtime.Concurrency, -1)
}

// TPMAdd 在拿到实际用量后喂入叶节点 TPM 窗口。
// 单位是「计费单位」：文本请求喂 token 数，图像请求喂张数。
func (b *Balancer) TPMAdd(e *model.Endpoint, units int64) {
	if e == nil || e.Runtime == nil || e.Runtime.TPM == nil {
		return
	}
	e.Runtime.TPM.Add(units)
}

// ModelLimits 返回模型能力上限（上下文、单次最大输出；0 = 未设置不校验）。
// 与价格索引同口径：包含停用模型，供网关前置校验。
func (b *Balancer) ModelLimits(name string) (context, maxOut int64) {
	b.mu.RLock()
	lim, ok := b.limits[name]
	b.mu.RUnlock()
	if !ok {
		return 0, 0
	}
	return lim[0], lim[1]
}

// Record 上报一次请求结果：驱动叶节点熔断 + 账号/叶/子Key 统计 + 写日志。
// 图像张数从 l.ImageCount 读取（0 表示文本请求）。
// 成本按实际命中的模型名（l.Model，fallback 后）折算并写回 l.Cost。
// 走阻塞投递（背压），保证统计不丢，避免 SubKey 日限额被绕过。
// clientErr 表示失败由客户端请求自身导致（如上下文超限、max_tokens 超上限）：
// 统计与日志照记，但不计入端点连续失败——这是请求方的错，不应熔断健康端点。
func (b *Balancer) Record(l *model.UsageLog, ep *model.Endpoint, ok, clientErr bool) {
	if ep != nil && ep.Runtime != nil {
		rt := ep.Runtime
		switch {
		case ok:
			// 成功：无论之前是 Closed 还是 HalfOpen，都回到 Closed。
			// 探测成功就是走这里——清零熔断状态与探测标志，叶子恢复承接流量。
			atomic.StoreInt32(&rt.ConsecutiveFailures, 0)
			atomic.StoreInt64(&rt.CircuitOpenUntil, 0)
			atomic.StoreInt32(&rt.Probing, 0)
			atomic.StoreInt64(&rt.ProbeStartedAt, 0)
		case clientErr:
			// 请求方问题（上下文超限等）：不算上游故障。
			// 但若该叶子正处于探测中，必须**释放探测租约**——否则探测标志一直挂着，
			// 后续请求再也不会去试，叶子永久卡在 HalfOpen。
			atomic.StoreInt32(&rt.Probing, 0)
			atomic.StoreInt64(&rt.ProbeStartedAt, 0)
		default:
			// 探测失败是「上游还没好」的直接证据，必须**立即重新熔断**，而不是等
			// 再次累积到阈值——否则探测失败后闸门被释放，下一批请求会立刻涌入
			// 一个已知故障的上游。
			if atomic.LoadInt32(&rt.Probing) == 1 {
				atomic.StoreInt32(&rt.ConsecutiveFailures, model.CircuitBreakerThreshold)
				atomic.StoreInt64(&rt.CircuitOpenUntil, nowPlusCooldown(model.CircuitBreakerThreshold))
			} else {
				fails := atomic.AddInt32(&rt.ConsecutiveFailures, 1)
				if fails >= model.CircuitBreakerThreshold {
					atomic.StoreInt64(&rt.CircuitOpenUntil, nowPlusCooldown(fails))
				}
			}
			// 无论哪种路径都释放探测租约，让下一个冷却周期能重新探测；
			// 不释放会让叶子在 HalfOpen 上挂死（既不恢复也不探测）。
			atomic.StoreInt32(&rt.Probing, 0)
			atomic.StoreInt64(&rt.ProbeStartedAt, 0)
		}
	}
	op := statOp{
		accountID:  l.AccountID,
		endpointID: l.EndpointID,
		subkeyID:   l.SubKeyID,
		ok:         ok,
		prompt:     l.PromptTokens,
		completion: l.CompletionTokens,
		images:     l.ImageCount,
	}
	// 成本核算：即使请求失败（tokens 全 0）结果也是 0，无需分支。
	// 同时写回拆分列——**先算拆分再汇总**，保证「拆分之和 == 总额」是结构上成立的，
	// 而不是两条独立公式碰巧相等（旧实现分两路算，缓存单价改动后必然发散）。
	l.InputCost, l.OutputCost, l.CacheCost = b.splitCost(l.Model, l)
	l.Cost = l.InputCost + l.OutputCost + l.CacheCost
	b.enqueueStat(op, l)
}

// enqueueStat 投递一次统计：优先入队（不阻塞）；通道满时**在调用方同步落库**。
//
// 为什么不阻塞入队：Record 在 HTTP 请求路径上。阻塞意味着「数据库写不动」
// 会反过来拖死转发（客户端已在等响应），把一个可观测的存储问题放大成全线故障。
// 为什么不丢弃：统计是计费与限额的依据，丢了就是少计费/超发额度。
//
// 退化成同步写是安全的选择：此时转发确实变慢，但数据不丢、故障仍然可见。
//
// 注意顺序：先写 stat 再写 log。stat 供实时聚合/日用量（限流依据），必须先落地。
func (b *Balancer) enqueueStat(op statOp, l *model.UsageLog) {
	if b.statCh != nil {
		select {
		case b.statCh <- op:
			b.enqueueLog(l)
			return
		default:
		}
	}
	// 通道已满（或未初始化）：同步落库，不再入队以免重复计数。
	b.statsOverflow.Add(1)
	b.applyStat(op)
	b.enqueueLogForce(l)
}

// enqueueLog 投递日志：同样不阻塞。満时同步写入。
func (b *Balancer) enqueueLog(l *model.UsageLog) {
	if b.logCh == nil {
		return
	}
	select {
	case b.logCh <- l:
	default:
		b.enqueueLogForce(l)
	}
}

// enqueueLogForce 同步写日志（通道満或未初始化）。
func (b *Balancer) enqueueLogForce(l *model.UsageLog) {
	b.logsOverflow.Add(1)
	if b.store != nil {
		_ = b.store.AddUsageLog(l)
	}
}

// StatsOverflow 返回因通道満而退化为同步写库的次数（stat, log）。
// 非零本身就是信号：说明落库速度跟不上请求速度，或者 consume 卡住了。
func (b *Balancer) StatsOverflow() (stats, logs int64) {
	return b.statsOverflow.Load(), b.logsOverflow.Load()
}

// computeCost 按模型定价折算一次请求的成本：
// 输入/输出单价为 $ / 1M tokens，图像单价为 $ / 张；未定价模型返回 0。
//
// 保留该函数供测试与「手上只有三个计数」的场景；**主路径（Record）不走它**，
// 而是先算三拆分再相加——缓存计费需要 cache token，这里只能拿到 prompt 总数。
// 语义差异：本函数把传入的 pt 视为**非缓存**输入。
// modelPrice 是某个模型的单价集合（单位：$ / 1M tokens；图像为 $ / 张）。
//
// 为什么要单列缓存读/写单价：上游报告的 prompt_tokens 是**含缓存命中的总量**。
// 若一律按 PriceInput 计费，会产生两个方向的错账：
//   - 缓存命中部分被按原价**多收**（Anthropic 读取仅约输入价 10%）；
//   - 缓存写入实际**漏收**（Anthropic 写入约输入价 125%，高于原价）。
// 拆分后才能三项相加得出正确总额。
type modelPrice struct {
	in, out, image       float64
	cacheRead, cacheWrite float64
}

// inRate 返回非缓存输入 token 的有效单价。
//
// 注意语义：这是**非缓存部分**的单价，调用方需先从 prompt_tokens 里扣除缓存量
// （见 billablePrompt）。直接把 prompt_tokens 乘这个单价会把缓存部分重复计费。
func (p modelPrice) inRate() float64 { return p.in }

// cacheReadRate / cacheWriteRate 返回缓存单价；**0 = 未设置回落输入单价**。
//
// 回落而非置零：0 在项目里的约定是「未设置」而不是「免费」。若把未定价当作免费，
// 管理员不填缓存单价就会静默少计费（而不是按原价多计费）——前者更难被发现。
func (p modelPrice) cacheReadRate() float64 {
	if p.cacheRead != 0 {
		return p.cacheRead
	}
	return p.in
}

func (p modelPrice) cacheWriteRate() float64 {
	if p.cacheWrite != 0 {
		return p.cacheWrite
	}
	return p.in
}

// billablePrompt 拆分 prompt_tokens 为「非缓存输入」与「缓存读/写」三部分，
// 保证三者之和 == prompt_tokens（不重不漏）。
//
// 防御点：上游字段可能自相矛盾（缓存量之和大于 prompt_tokens，或单一缓存量
// 就是超了）。此时不能让非缓存部分变成负数凭空减少费用，也不能让总额超出实际
// 报告的 prompt_tokens——夹紧到 [0, prompt] 并把两个缓存量按比例收敛。
func billablePrompt(prompt, cacheRead, cacheWrite int64) (plain, read, write int64) {
	if prompt < 0 {
		prompt = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	read, write = cacheRead, cacheWrite
	if read+write > prompt {
		// 缓存量超报：按比例缩到恰好等于 prompt_tokens，避免负数输入或总额膨胀。
		total := read + write
		read = read * prompt / total
		write = prompt - read
	}
	return prompt - read - write, read, write
}

// pricesFor 读价格表（持读锁）。
func (b *Balancer) pricesFor(modelName string) (modelPrice, bool) {
	b.mu.RLock()
	p, ok := b.prices[modelName]
	b.mu.RUnlock()
	return p, ok
}

// CacheSavings 估算某模型下「缓存命中」相对「全部未命中」节省/多付的金额。
//
// 返回值 = 缓存部分实付 - 同量 token 按输入价应付。
//   - 负值 = 节省（缓存读便宜，常态）；
//   - 正值 = 多付（缓存写入溢价的贡献往往小于读取折扣，所以通常仍为负）。
//
// 供门户展示「缓存为你省了多少钱」。未定价模型返回 (0, false)，调用方应显示
// “—”而不是 $0——后者会被读成“没有节省”，而真相是“不知道”。
func (b *Balancer) CacheSavings(modelName string, cacheRead, cacheWrite int64) (float64, bool) {
	p, ok := b.pricesFor(modelName)
	if !ok || (cacheRead == 0 && cacheWrite == 0) {
		return 0, ok
	}
	paid := float64(cacheRead)/1e6*p.cacheReadRate() + float64(cacheWrite)/1e6*p.cacheWriteRate()
	hypothetical := float64(cacheRead+cacheWrite) / 1e6 * p.inRate()
	return paid - hypothetical, true
}

func (b *Balancer) computeCost(modelName string, pt, ct, images int64) float64 {
	in, out, cache, _ := b.splitCostOf(modelName, &model.UsageLog{
		PromptTokens: pt, CompletionTokens: ct, ImageCount: images,
	})
	return in + out + cache
}

// splitCost 把一次请求的成本拆成输入/输出/缓存三部分（供用量分析按成本构成下钻）。
//
// 关系：**Cost = in + out + cache**（Record 里就是三者相加得到 Cost）。
// 其中 in 仅覆盖**非缓存**输入，cache 覆盖缓存读+写；图像费用归入 out
// （目前图像无独立拆分列，混入输出更贴近「非输入侧费用」的直觉）。
//
// 为什么输入要拆出缓存：prompt_tokens 是含缓存命中的总量，若整块按输入单价算，
// 命中部分会被多收（缓存读通常更便宜），而缓存写入又会被漏收（它通常更贵）。
// 拆分后三项之和仍等于正确总额，且缓存成本可单独展示。
func (b *Balancer) splitCost(modelName string, l *model.UsageLog) (in, out, cache float64) {
	in, out, cache, _ = b.splitCostOf(modelName, l)
	return in, out, cache
}

// splitCostOf 是 splitCost 的完整版，额外返回是否命中定价表。
func (b *Balancer) splitCostOf(modelName string, l *model.UsageLog) (in, out, cache float64, priced bool) {
	p, ok := b.pricesFor(modelName)
	if !ok {
		return 0, 0, 0, false
	}
	plain, read, write := billablePrompt(l.PromptTokens, l.CacheReadTokens, l.CacheCreationTokens)
	in = float64(plain) / 1e6 * p.inRate()
	cache = float64(read)/1e6*p.cacheReadRate() + float64(write)/1e6*p.cacheWriteRate()
	out = float64(l.CompletionTokens)/1e6*p.out + float64(l.ImageCount)*p.image
	return in, out, cache, true
}

func nowPlusCooldown(fails int32) int64 {
	extra := fails - model.CircuitBreakerThreshold
	if extra < 0 {
		extra = 0
	}
	if extra > 6 {
		extra = 6
	}
	cooldown := time.Duration(1<<extra) * model.CircuitCooldownBase
	if cooldown > model.CircuitCooldownMax {
		cooldown = model.CircuitCooldownMax
	}
	return time.Now().Add(cooldown).UnixNano()
}

// SnapshotAccounts 返回账号快照（含聚合统计，供管理 UI）。
func (b *Balancer) SnapshotAccounts() []*model.Account {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*model.Account, 0, len(b.accounts))
	ids := make([]string, 0, len(b.accounts))
	for id := range b.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := b.accounts[id]
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// SnapshotEndpoints 返回叶节点快照（含运行态概要：并发/熔断/RPM/TPM 窗口计数）。
func (b *Balancer) SnapshotEndpoints() []*model.Endpoint {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*model.Endpoint, 0, len(b.endpoints))
	for _, e := range b.endpoints {
		cp := *e
		cp.Runtime = nil
		if e.Runtime != nil {
			now := time.Now().UnixNano()
			openUntil := atomic.LoadInt64(&e.Runtime.CircuitOpenUntil)
			info := &model.EndpointRuntimeInfo{
				Concurrency: atomic.LoadInt32(&e.Runtime.Concurrency),
				CircuitOpen: openUntil > now,
			}
			if info.CircuitOpen {
				info.CircuitRemainMS = (openUntil - now) / int64(time.Millisecond)
			} else if openUntil != 0 {
				// 熔断过、冷却已到期：处于 HalfOpen（要么正有探测在跑，要么等下一个
				// 请求去探）。展示出来以免界面把「刚过冷却但还没验证成功」误显示为
				// 完全健康——此时其实还不能放心承接流量。
				info.HalfOpen = true
			}
			if e.Runtime.RPM != nil {
				info.RPMCurrent = e.Runtime.RPM.Count()
			}
			if e.Runtime.TPM != nil {
				info.TPMCurrent = e.Runtime.TPM.Sum()
			}
			cp.RuntimeInfo = info
		}
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out
}

// SnapshotModelNames 返回当前启用的易读模型名集合。
func (b *Balancer) SnapshotModelNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.models))
	for name := range b.models {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CircuitOpen 按 ID 判断叶节点是否熔断中。SnapshotEndpoints 返回的副本已清空
// Runtime，不能直接喂给「按对象判可用」一类的函数（会把所有启用端点误判为熔断）。
func (b *Balancer) CircuitOpen(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.endpoints[id]
	if e == nil || e.Runtime == nil {
		return false
	}
	return atomic.LoadInt64(&e.Runtime.CircuitOpenUntil) > time.Now().UnixNano()
}

// CircuitHalfOpen 按 ID 判断叶节点是否处于 HalfOpen（熔断冷却已到期、探测尚未成功）。
//
// 与 CircuitOpen 分开而不是合并成一个三态枚举：两者在界面上的含义不同
// （“熔断中”= 完全不转移量；“探测中”= 只放一个请求验证），而且调用方大多只关心
// 其中一种。需要区分时两个函数各调一次即可（总览页就是这么用的）。
func (b *Balancer) CircuitHalfOpen(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.endpoints[id]
	if e == nil || e.Runtime == nil {
		return false
	}
	openUntil := atomic.LoadInt64(&e.Runtime.CircuitOpenUntil)
	// 熔断过（非 0）且冷却已到期 → 处于探测态。
	return openUntil != 0 && openUntil <= time.Now().UnixNano()
}

// AccountActive 判断账号是否启用。
func (b *Balancer) AccountActive(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.accounts[id]
	return ok && a.Status == model.AccountActive
}
