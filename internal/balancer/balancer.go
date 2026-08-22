// Package balancer 实现多账号负载均衡。
//
// 拓扑：树状结构。
//   - 叶节点 = 接入点 Endpoint（账号 × 真实 ep）。限流（并发/RPM/TPM）与熔断只落在叶节点。
//   - 父节点 = 账号 Account。不做流控、不做熔断，只提供 active/disabled 开关与统计聚合。
//
// 并发模型：
//   - Go net/http 每个请求一个 goroutine，转发全程无全局串行锁。
//   - 叶节点运行态（并发、连续失败、熔断、RPM/TPM）统一在「持锁 Refresh」时惰性初始化，
//     之后对 int32/int64 用 atomic、对窗口用独立 mutex，读侧用 RLock 快照，无数据竞争。
//   - 用量统计与日志经有界 channel 的「阻塞投递」落库（背压）——不丢弃，避免子 Key 日限额被绕过。
//
// 路由决策（Select）：
//  1. 客户端给出易读模型名 model + 账号白名单 allowed + 排除集 exclude；
//  2. 收集「model」下所有可用叶节点（账号 active、叶 enabled、未熔断、未超并发/RPM/TPM）；
//  3. 在候选叶节点上做平滑加权轮询（权重：叶.weight > 账号.weight > 1）；
//  4. 返回命中的叶节点（账号 + 真实 ep），调用方据此转发。
package balancer

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/store"
)

var (
	// ErrNoAccount 表示没有可用账号。
	ErrNoAccount = errors.New("没有可用账号")
	// ErrNoEndpoint 表示易读模型名没有对应的接入点映射。
	ErrNoEndpoint = errors.New("模型没有对应的接入点")
	// ErrAllThrottled 表示所有候选叶节点都被限流或熔断。
	ErrAllThrottled = errors.New("所有可用接入点均被限流或熔断")
)

// maxFallbackChain 限制 fallback 链展开后的总长度，防止配置成环或过深时
// 单次请求退避到无限多个模型上。
const maxFallbackChain = 16

// Balancer 是负载均衡器。
type Balancer struct {
	mu        sync.RWMutex
	accounts  map[string]*model.Account    // accountID -> account
	models    map[string]*model.Model      // 易读名 -> model
	endpoints map[string]*model.Endpoint   // endpointID -> endpoint（叶节点）
	modelApps map[string][]*model.Endpoint // 易读名 -> 该模型下全部叶子

	wrrMu    sync.Mutex
	wrrState map[string]*wrrState

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

type statOp struct {
	accountID  string
	endpointID string
	subkeyID   string
	ok         bool
	prompt     int64
	completion int64
}

// New 构造 Balancer 并启动后台消费者。
func New(st *store.Store) *Balancer {
	b := &Balancer{
		accounts:  map[string]*model.Account{},
		models:    map[string]*model.Model{},
		endpoints: map[string]*model.Endpoint{},
		modelApps: map[string][]*model.Endpoint{},
		wrrState:  map[string]*wrrState{},
		logCh:     make(chan *model.UsageLog, 4096),
		statCh:    make(chan statOp, 4096),
		done:      make(chan struct{}),
		store:     st,
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
		a.LastUsedAt = now
	}
	if e, ok := b.endpoints[op.endpointID]; ok {
		bumpStats(&e.TotalRequests, &e.SuccessRequests, &e.FailRequests,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, op)
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

	for _, a := range accounts {
		accMap[a.ID] = a
	}
	for _, m := range modelsList {
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
	b.mu.Unlock()
}

// Select 按「平滑加权轮询 + 熔断 + 限流」选出一个可用叶节点。
//
// modelName 为下游请求里的易读模型名。
func (b *Balancer) Select(modelName string, allowed []string, exclude map[string]bool) (ep *model.Endpoint, err error) {
	cands, poolKey := b.usableEndpoints(modelName, allowed, exclude)
	if len(cands) == 0 {
		if b.hasAnyEndpoint(modelName, allowed) {
			return nil, ErrAllThrottled
		}
		if b.modelNameKnown(modelName) {
			return nil, ErrNoAccount
		}
		return nil, ErrNoEndpoint
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

	atomic.AddInt32(&picked.Runtime.Concurrency, 1)
	picked.Runtime.RPM.Add(1)

	return picked, nil
}

// FallbackChain 解析某易读模型名的有序 fallback 链。
//
// 按广度优先展开：请求名在前，然后是它 Fallback 列表里的每一项（保持配置顺序），
// 再依次展开这些模型各自的 Fallback。去重、防环，总长上限 maxFallbackChain。
// 例如 A.Fallback=[B,C]，B.Fallback=[D] → [A, B, C, D]。
//
// UI 把该字段描述为「逗号分隔，按顺序尝试」，因此这里必须遍历列表全部条目，
// 而不能只取首项——否则用户配置的第二个及以后的候选会被静默忽略。
func (b *Balancer) FallbackChain(modelName string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if modelName == "" {
		return nil
	}
	chain := []string{modelName}
	seen := map[string]bool{modelName: true}
	for i := 0; i < len(chain) && len(chain) < maxFallbackChain; i++ {
		m, ok := b.models[chain[i]]
		if !ok {
			continue
		}
		for _, f := range m.Fallback {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			chain = append(chain, f)
			if len(chain) >= maxFallbackChain {
				break
			}
		}
	}
	return chain
}

// SelectWithFallback 先按请求的模型名做常规 Select；若该模型在所有账号的元组都
// 不可用（ErrAllThrottled / ErrNoAccount / ErrNoEndpoint），自动沿 fallback 链
// 依次尝试后续模型。返回命中的叶节点与实际使用的模型名。
//
// allowedAccounts 为账号白名单（子 Key 限定可用的账号）；allowedModels 为模型
// 白名单（子 Key 限定的模型；非空时，fallback 链里不在白名单的模型会被跳过，
// 避免「子 Key 仅授权模型 A，却经 fallback 打到未授权的模型 B」）。
func (b *Balancer) SelectWithFallback(modelName string, allowedAccounts, allowedModels []string, exclude map[string]bool) (*model.Endpoint, string, error) {
	allowModel := setOf(allowedModels)
	chain := b.FallbackChain(modelName)
	var lastErr error
	for _, name := range chain {
		if len(allowModel) > 0 && !allowModel[name] {
			continue // 子 Key 未授权的 fallback 目标，跳过
		}
		ep, err := b.Select(name, allowedAccounts, exclude)
		if err == nil {
			return ep, name, nil
		}
		lastErr = err
	}
	return nil, modelName, lastErr
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
func (b *Balancer) usableEndpoints(modelName string, allowed []string, exclude map[string]bool) ([]*model.Endpoint, string) {
	allow := setOf(allowed)
	b.mu.RLock()
	defer b.mu.RUnlock()

	now := time.Now()
	var cands []*model.Endpoint
	for _, e := range b.modelApps[modelName] {
		if !b.endpointUsable(e, allow, exclude, now) {
			continue
		}
		cands = append(cands, e)
	}

	// 透传真实 ID：为「无映射」的模型名，给每个符合白名单的 active 账号合成一个临时叶子。
	// 合成叶子是纯内存对象，不持久化、不熔断、不限流（透传属逃生通道，保持简单）。
	if looksLikeRealID(modelName) && len(cands) == 0 {
		for id, a := range b.accounts {
			if a.Status != model.AccountActive {
				continue
			}
			if len(allow) > 0 && !allow[id] {
				continue
			}
			leaf := newPassthrough(id, modelName, a.Weight)
			if exclude != nil && exclude[leaf.ID] {
				continue
			}
			cands = append(cands, leaf)
		}
	}

	return cands, poolKeyOf(modelName, allowed)
}

// newPassthrough 合成一个透传叶节点（纯路由用途，不限流不熔断）。
// 其 ID 用「账号 ID」而非「passthrough:id」——这样失败方写入 exclude 的 ep.ID
// 恰好等于该账号 ID，透传分支的 exclude[accountID] 判定才能命中，避免重试打同一账号。
func newPassthrough(accountID, modelName string, weight int) *model.Endpoint {
	e := &model.Endpoint{
		ID:        "passthrough:" + accountID + ":" + modelName,
		AccountID: accountID,
		Model:     modelName,
		EP:        modelName,
		Enabled:   true,
		Weight:    weight,
		Synthetic: true,
	}
	e.EnsureRuntime()
	return e
}

// passthroughExcludeKey 返回透传叶子在 exclude 集合里使用的键。
// 透传叶子无持久化 ID，路径上（查询与写入）统一用账号 ID 作为排除键，保证失败重试能切账号。
func passthroughExcludeKey(e *model.Endpoint) string {
	return e.AccountID
}

func looksLikeRealID(s string) bool {
	return strings.HasPrefix(s, "ep-")
}

func (b *Balancer) endpointUsable(e *model.Endpoint, allow map[string]bool, exclude map[string]bool, now time.Time) bool {
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

func (b *Balancer) hasAnyEndpoint(modelName string, allowed []string) bool {
	allow := setOf(allowed)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, known := b.models[modelName]; !known && !looksLikeRealID(modelName) {
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
		return true
	}
	if looksLikeRealID(modelName) {
		for id, a := range b.accounts {
			if a.Status != model.AccountActive {
				continue
			}
			if len(allow) > 0 && !allow[id] {
				continue
			}
			return true
		}
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

// TPMAdd 在拿到实际 token 用量后喂入叶节点 TPM 窗口（含请求前未知预估值时也可预喂）。
func (b *Balancer) TPMAdd(e *model.Endpoint, tokens int64) {
	if e == nil || e.Runtime == nil || e.Runtime.TPM == nil {
		return
	}
	e.Runtime.TPM.Add(tokens)
}

// Record 上报一次请求结果：驱动叶节点熔断 + 账号/叶/子Key 统计 + 写日志。
// 走阻塞投递（背压），保证统计不丢，避免 SubKey 日限额被绕过。
func (b *Balancer) Record(l *model.UsageLog, ep *model.Endpoint, ok bool) {
	if ep != nil && ep.Runtime != nil {
		rt := ep.Runtime
		if ok {
			atomic.StoreInt32(&rt.ConsecutiveFailures, 0)
			atomic.StoreInt64(&rt.CircuitOpenUntil, 0)
		} else {
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
	}
	// 阻塞投递，保证不丢。
	sendStat := func(op statOp) {
		if b.statCh == nil {
			return
		}
		b.statCh <- op
	}
	if l.SubKeyID != "" {
		op.subkeyID = l.SubKeyID
	}
	sendStat(op)
	if b.logCh != nil {
		b.logCh <- l
	}
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

// EndpointUsable 判断叶节点是否可用（未熔断、未禁用、账号可用）。
func (b *Balancer) EndpointUsable(e *model.Endpoint) bool {
	if e == nil || !e.Enabled {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if acc, ok := b.accounts[e.AccountID]; !ok || acc.Status != model.AccountActive {
		return false
	}
	if e.Runtime == nil {
		return false
	}
	return atomic.LoadInt64(&e.Runtime.CircuitOpenUntil) <= time.Now().UnixNano()
}

// AccountActive 判断账号是否启用。
func (b *Balancer) AccountActive(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.accounts[id]
	return ok && a.Status == model.AccountActive
}
