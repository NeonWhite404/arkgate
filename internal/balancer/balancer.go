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
	prices    map[string][3]float64        // 易读名 -> [输入,输出,图像] 单价（含停用模型，供成本核算）
	limits    map[string][2]int64          // 易读名 -> [上下文,最大输出] 上限（含停用模型；0=不校验）

	wrrMu    sync.Mutex
	wrrState map[string]*wrrState

	sessMu    sync.Mutex
	sessions  map[string]sessEntry // 粘性会话：stickyKey -> 命中叶节点
	sessionTTL time.Duration

	logCh  chan *model.UsageLog
	statCh chan statOp
	done   chan struct{}
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
		prices:     map[string][3]float64{},
		limits:     map[string][2]int64{},
		wrrState:   map[string]*wrrState{},
		sessions:   map[string]sessEntry{},
		sessionTTL: sessionTTL,
		logCh:      make(chan *model.UsageLog, 4096),
		statCh:     make(chan statOp, 4096),
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
	priceMap := map[string][3]float64{}
	limitMap := map[string][2]int64{}

	for _, a := range accounts {
		accMap[a.ID] = a
		defMap[a.ID] = resolveDef(a)
	}
	for _, m := range modelsList {
		// 价格索引包含停用模型：日志里的历史用量仍要按当时定价折算。
		priceMap[m.Name] = [3]float64{m.PriceInput, m.PriceOutput, m.PriceImage}
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

	// 清理指向已不存在叶子的粘性会话（TTL 过期在读侧惰性淘汰）。
	if b.sessionTTL > 0 {
		b.sessMu.Lock()
		for k, s := range b.sessions {
			if _, ok := epMap[s.endpointID]; !ok {
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
	if stickyKey != "" && b.sessionTTL > 0 {
		if pinned := b.sessionPick(stickyKey+"|"+modelName, cands); pinned != nil {
			atomic.AddInt32(&pinned.Runtime.Concurrency, 1)
			pinned.Runtime.RPM.Add(1)
			return pinned, nil
		}
	}

	state := b.getWrrState(poolKey, cands)

	state.mu.Lock()
	defer state.mu.Unlock()

	// 由 getWrrState 一次性算出统一权重，Select 与其读取同一张权重表，避免两次取值不一致破坏平滑 WRR。
	weights := b.weightTable(cands)
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

	return picked, nil
}

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

// hasAnyEndpoint 判断模型是否存在「账号可用 + 能力匹配」的叶节点（不含运行态判断）。
func (b *Balancer) hasAnyEndpoint(modelName string, allowed []string, api API) bool {
	allow := setOf(allowed)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, known := b.models[modelName]; !known {
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

func (b *Balancer) getWrrState(poolKey string, cands []*model.Endpoint) *wrrState {
	weights := b.weightTable(cands)
	b.wrrMu.Lock()
	defer b.wrrMu.Unlock()
	st, ok := b.wrrState[poolKey]
	if !ok {
		st = &wrrState{current: map[string]int{}}
		b.wrrState[poolKey] = st
	}
	total := 0
	for _, e := range cands {
		total += weights[e.ID]
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
		if ok {
			atomic.StoreInt32(&rt.ConsecutiveFailures, 0)
			atomic.StoreInt64(&rt.CircuitOpenUntil, 0)
		} else if !clientErr {
			fails := atomic.AddInt32(&rt.ConsecutiveFailures, 1)
			if fails >= model.CircuitBreakerThreshold {
				atomic.StoreInt64(&rt.CircuitOpenUntil, nowPlusCooldown(fails))
			}
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
	op.cost = b.computeCost(l.Model, l.PromptTokens, l.CompletionTokens, l.ImageCount)
	l.Cost = op.cost
	// 阻塞投递，保证不丢。
	if b.statCh != nil {
		b.statCh <- op
	}
	if b.logCh != nil {
		b.logCh <- l
	}
}

// computeCost 按模型定价折算一次请求的成本：
// 输入/输出单价为 $ / 1M tokens，图像单价为 $ / 张；未定价模型返回 0。
func (b *Balancer) computeCost(modelName string, pt, ct, images int64) float64 {
	b.mu.RLock()
	p, ok := b.prices[modelName]
	b.mu.RUnlock()
	if !ok {
		return 0
	}
	return float64(pt)/1e6*p[0] + float64(ct)/1e6*p[1] + float64(images)*p[2]
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

// AccountActive 判断账号是否启用。
func (b *Balancer) AccountActive(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.accounts[id]
	return ok && a.Status == model.AccountActive
}
