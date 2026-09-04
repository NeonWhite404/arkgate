package balancer

import (
	"testing"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/provider"
)

// newTestBalancer 构造一个不走真实 store 的 Balancer（不启动 consumer）。
func newTestBalancer() *Balancer {
	return &Balancer{
		accounts:  map[string]*model.Account{},
		models:    map[string]*model.Model{},
		endpoints: map[string]*model.Endpoint{},
		modelApps: map[string][]*model.Endpoint{},
		defs:      map[string]provider.Def{},
		prices:    map[string][3]float64{},
		wrrState:  map[string]*wrrState{},
		sessions:  map[string]sessEntry{},
	}
}

func (b *Balancer) seed(accounts []*model.Account, endpoints []*model.Endpoint, models []*model.Model) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range accounts {
		b.accounts[a.ID] = a
		b.defs[a.ID] = resolveDef(a)
	}
	for _, e := range endpoints {
		e.Enabled = true
		e.EnsureRuntime()
		b.endpoints[e.ID] = e
		b.modelApps[e.Model] = append(b.modelApps[e.Model], e)
	}
	for _, m := range models {
		if m.Enabled {
			b.models[m.Name] = m
		}
	}
}

func mkAcc(id string, weight int) *model.Account {
	return &model.Account{ID: id, Name: id, Status: model.AccountActive, Weight: weight}
}

func mkEP(id, accID, modelName, ep string, weight int) *model.Endpoint {
	return &model.Endpoint{ID: id, AccountID: accID, Model: modelName, EP: ep, Enabled: true, Weight: weight}
}

func TestSelectWeightedRoundRobin(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1), mkAcc("a2", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 3),
		mkEP("e2", "a2", "m", "ep-2", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		e, err := b.Select("m", nil, nil, APIChat, "")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		counts[e.EP]++
		b.Release(e)
	}
	if counts["ep-1"] != 3 || counts["ep-2"] != 1 {
		t.Fatalf("want ep-1 x3 / ep-2 x1, got %v", counts)
	}
}

// TestSelectMultipleEndpointsSameAccount 同一账号下同一模型挂多个接入点
// （同模型的不同发布版本）：都参与 WRR，按各自权重分摊，互不覆盖。
func TestSelectMultipleEndpointsSameAccount(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-250615", 1),
		mkEP("e2", "a1", "m", "ep-250828", 3),
	}, []*model.Model{{Name: "m", Enabled: true}})

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		e, err := b.Select("m", nil, nil, APIChat, "")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		counts[e.EP]++
		b.Release(e)
	}
	if counts["ep-250828"] != 3 || counts["ep-250615"] != 1 {
		t.Fatalf("want new-version x3 / old-version x1, got %v", counts)
	}

	// 其中一个版本熔断时，同账号的另一个版本仍可承接（叶级熔断不牵连兄弟叶）。
	b.mu.Lock()
	b.endpoints["e2"].Runtime.CircuitOpenUntil = 1 << 62
	b.mu.Unlock()
	for i := 0; i < 3; i++ {
		e, err := b.Select("m", nil, nil, APIChat, "")
		if err != nil {
			t.Fatalf("select after circuit: %v", err)
		}
		if e.EP != "ep-250615" {
			t.Fatalf("want surviving sibling leaf, got %s", e.EP)
		}
		b.Release(e)
	}
}

func TestSelectNoEndpoint(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1)}, nil, []*model.Model{{Name: "known", Enabled: true}})
	_, err := b.Select("unknown-model", nil, nil, APIChat, "")
	if err != ErrNoEndpoint {
		t.Fatalf("want ErrNoEndpoint for unknown model with no mappings, got %v", err)
	}
	_, err = b.Select("known", nil, nil, APIChat, "")
	if err != ErrNoAccount {
		t.Fatalf("want ErrNoAccount for known model with no endpoints, got %v", err)
	}
}

func TestSelectRespectAllThrottled(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})

	b.mu.Lock()
	b.endpoints["e1"].Runtime.CircuitOpenUntil = 1 << 62
	b.mu.Unlock()

	_, err := b.Select("m", nil, nil, APIChat, "")
	if err != ErrAllThrottled {
		t.Fatalf("want ErrAllThrottled, got %v", err)
	}
}

func TestCircuitBreakerRecord(t *testing.T) {
	b := newTestBalancer()
	e := mkEP("e1", "a1", "m", "ep-1", 1)
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{e}, nil)

	for i := 0; i < model.CircuitBreakerThreshold; i++ {
		b.Record(&model.UsageLog{AccountID: "a1", EndpointID: e.ID}, e, false, false)
	}
	e.EnsureRuntime()
	if e.Runtime.CircuitOpenUntil <= 0 {
		t.Fatalf("expected endpoint circuit to open after failures")
	}
	// 成功应复位熔断。（阻塞投递需 consumer 消费；直接读内存态即可）
	b.Record(&model.UsageLog{AccountID: "a1", EndpointID: e.ID}, e, true, false)
	if e.Runtime.CircuitOpenUntil != 0 || e.Runtime.ConsecutiveFailures != 0 {
		t.Fatalf("expected circuit reset on success")
	}
}

// TestRecordClientErrNoCircuit 客户端请求自身导致的失败（上下文超限等）
// 不计入端点熔断：统计照记，但连续次数不累加、熔断不打开。
func TestRecordClientErrNoCircuit(t *testing.T) {
	b := newTestBalancer()
	e := mkEP("e1", "a1", "m", "ep-1", 1)
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{e}, nil)

	for i := 0; i < model.CircuitBreakerThreshold*3; i++ {
		b.Record(&model.UsageLog{AccountID: "a1", EndpointID: e.ID}, e, false, true)
	}
	e.EnsureRuntime()
	if e.Runtime.CircuitOpenUntil != 0 || e.Runtime.ConsecutiveFailures != 0 {
		t.Fatalf("client-caused failures must not trip circuit: %+v", e.Runtime)
	}
}

func TestTupleThrottlingExcludesSingleEndpoint(t *testing.T) {
	b := newTestBalancer()
	b.seed(
		[]*model.Account{mkAcc("a1", 1)},
		[]*model.Endpoint{
			mkEP("e1", "a1", "m", "ep-1", 1),
			mkEP("e2", "a1", "m", "ep-2", 1),
		},
		[]*model.Model{{Name: "m", Enabled: true}},
	)
	b.mu.Lock()
	b.endpoints["e1"].RPMLimit = 1
	b.endpoints["e1"].Runtime.RPM.Add(1)
	b.mu.Unlock()

	for i := 0; i < 4; i++ {
		e, err := b.Select("m", nil, nil, APIChat, "")
		if err != nil {
			t.Fatalf("select should still succeed via e2: %v", err)
		}
		if e.EP != "ep-2" {
			t.Fatalf("expected ep-2 (e1 throttled), got %s", e.EP)
		}
		b.Release(e)
	}
}

// TestCapabilityFiltering 锁定「responses/images 只路由到有能力的账号」语义：
// 能力 = provider 默认 × 账号三态覆盖（-1 继承 / 0 强制否 / 1 强制是）。
func TestCapabilityFiltering(t *testing.T) {
	b := newTestBalancer()
	aCustom := mkAcc("a1", 1)
	aCustom.Provider = "custom" // custom 默认仅 chat
	aArk := mkAcc("a2", 1)
	aArk.Provider = "ark" // ark 原生支持 chat/responses/images
	b.seed([]*model.Account{aCustom, aArk}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
		mkEP("e2", "a2", "m", "ep-2", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})

	// chat：两个账号都可用。
	if e, err := b.Select("m", nil, nil, APIChat, ""); err != nil || e == nil {
		t.Fatalf("chat should select, got %v/%v", e, err)
	}
	// responses/images：只有 ark 账号可用。
	for _, api := range []API{APIResponses, APIImages} {
		e, err := b.Select("m", nil, nil, api, "")
		if err != nil || e.AccountID != "a2" {
			t.Fatalf("api %d: want ark leaf, got %v/%v", api, e, err)
		}
		b.Release(e)
	}

	// 账号三态覆盖：custom 强制打开 images（1）→ 参与图像路由；ark 强制关闭 responses（-1）→ 退出。
	b.mu.Lock()
	b.accounts["a1"].CapImages = 1
	b.accounts["a2"].CapResponses = -1
	b.mu.Unlock()

	// images：a1/a2 都有能力 → 排除 a2 的叶子后应命中 a1（覆盖生效）。
	e, err := b.Select("m", nil, map[string]bool{"e2": true}, APIImages, "")
	if err != nil || e.AccountID != "a1" {
		t.Fatalf("custom with CapImages=1 should serve images, got %v/%v", e, err)
	}
	b.Release(e)

	// responses：ark 被强制关闭、custom 原生不支持 → 无可用账号。
	if _, err := b.Select("m", nil, nil, APIResponses, ""); err != ErrNoCapable {
		t.Fatalf("ark with CapResponses=-1 should yield ErrNoCapable, got %v", err)
	}
}

// TestNoCapableVsAllThrottled 区分「有能力但全被熔断/限流」与「根本没有支持此
// API 的账号」两种失败，错误语义不能混淆。
func TestNoCapableVsAllThrottled(t *testing.T) {
	b := newTestBalancer()
	a := mkAcc("a1", 1)
	a.Provider = "custom" // 无 images 能力
	b.seed([]*model.Account{a}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})

	// 能力不符 → ErrNoCapable（不是 ErrAllThrottled）。
	if _, err := b.Select("m", nil, nil, APIImages, ""); err != ErrNoCapable {
		t.Fatalf("want ErrNoCapable, got %v", err)
	}

	// 有能力的账号被熔断 → ErrAllThrottled。
	b.mu.Lock()
	b.accounts["a1"].CapImages = 1
	b.endpoints["e1"].Runtime.CircuitOpenUntil = 1 << 62
	b.mu.Unlock()
	if _, err := b.Select("m", nil, nil, APIImages, ""); err != ErrAllThrottled {
		t.Fatalf("want ErrAllThrottled, got %v", err)
	}
}

func TestTPMLimitThrottles(t *testing.T) {
	b := newTestBalancer()
	e := mkEP("e1", "a1", "m", "ep-1", 1)
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{e}, []*model.Model{{Name: "m", Enabled: true}})
	b.mu.Lock()
	b.endpoints["e1"].TPMLimit = 100
	b.mu.Unlock()

	// 喂 101 单位后，TPM 窗口超限，Select 应返回 ErrAllThrottled。
	b.TPMAdd(e, 101)
	_, err := b.Select("m", nil, nil, APIChat, "")
	if err != ErrAllThrottled {
		t.Fatalf("want ErrAllThrottled for TPM exhausted, got %v", err)
	}
}

func TestFallbackChain(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "a", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"b", "c"}},
		{Name: "b", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"d"}},
		{Name: "d", Type: model.ModelTypeText, Enabled: true},
	})
	got := b.FallbackChain("a")
	// 广度优先：a 的两个候选 b、c 都要保留（按配置顺序），再展开 b 的 d。
	want := []string{"a", "b", "c", "d"}
	if !equalStrs(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// TestFallbackChainKeepsAllBranches 锁定「列表里每一项都会被尝试」这一语义——
// UI 承诺「逗号分隔，按顺序尝试」，早期实现只取首项，会静默丢弃后续候选。
func TestFallbackChainKeepsAllBranches(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "a", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"b", "c", "d"}},
	})
	got := b.FallbackChain("a")
	want := []string{"a", "b", "c", "d"}
	if !equalStrs(got, want) {
		t.Fatalf("all configured fallbacks must be kept: want %v, got %v", want, got)
	}
}

// TestFallbackChainSameTypeOnly 锁定「fallback 链限定同类型」：文本模型不得
// 退避到图像模型；未登记的目标保留在链上（Select 时自然失败，维持
// 「配置的每一项都会被尝试」的既有语义），仅跨类型的已登记目标被跳过。
func TestFallbackChainSameTypeOnly(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "t1", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"img", "t2", "ghost"}},
		{Name: "img", Type: model.ModelTypeImage, Enabled: true},
		{Name: "t2", Type: model.ModelTypeText, Enabled: true},
		{Name: "i1", Type: model.ModelTypeImage, Enabled: true, Fallback: []string{"t1", "i2"}},
		{Name: "i2", Type: model.ModelTypeImage, Enabled: true},
	})
	if got := b.FallbackChain("t1"); !equalStrs(got, []string{"t1", "t2", "ghost"}) {
		t.Fatalf("text chain must skip image targets only, got %v", got)
	}
	if got := b.FallbackChain("i1"); !equalStrs(got, []string{"i1", "i2"}) {
		t.Fatalf("image chain must skip text targets, got %v", got)
	}
}

func TestFallbackChainDedupes(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "a", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"b", "c"}},
		{Name: "b", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"c", "a"}},
		{Name: "c", Type: model.ModelTypeText, Enabled: true},
	})
	got := b.FallbackChain("a")
	want := []string{"a", "b", "c"}
	if !equalStrs(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func equalStrs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFallbackChainCycle(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "a", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"b"}},
		{Name: "b", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"a"}},
	})
	got := b.FallbackChain("a")
	if len(got) != 2 {
		t.Fatalf("cycle should terminate after 2, got %v", got)
	}
}

func TestSelectWithFallback(t *testing.T) {
	b := newTestBalancer()
	// m 的元组全部熔断；fallback 模型 m2 可用。
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
		mkEP("e2", "a1", "m2", "ep-2", 1),
	}, []*model.Model{
		{Name: "m", Type: model.ModelTypeText, Enabled: true, Fallback: []string{"m2"}},
		{Name: "m2", Type: model.ModelTypeText, Enabled: true},
	})
	b.mu.Lock()
	b.endpoints["e1"].Runtime.CircuitOpenUntil = 1 << 62 // 熔断 m 的唯一元组
	b.mu.Unlock()

	ep, actual, err := b.SelectWithFallback("m", nil, nil, nil, APIChat, "")
	if err != nil {
		t.Fatalf("select with fallback: %v", err)
	}
	if ep.EP != "ep-2" || actual != "m2" {
		t.Fatalf("want fallback to m2/ep-2, got %s/%s", actual, ep.EP)
	}
	b.Release(ep)
}

// TestSelectWithFallbackNoCapable 验证 images 请求在仅有 chat 能力账号时沿
// fallback 链失败后返回明确错误。
func TestSelectWithFallbackNoCapable(t *testing.T) {
	b := newTestBalancer()
	a := mkAcc("a1", 1)
	a.Provider = "custom"
	b.seed([]*model.Account{a}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
	}, []*model.Model{
		{Name: "m", Type: model.ModelTypeImage, Enabled: true, Fallback: []string{"m2"}},
		{Name: "m2", Type: model.ModelTypeImage, Enabled: true},
	})
	_, _, err := b.SelectWithFallback("m", nil, nil, nil, APIImages, "")
	if err != ErrNoCapable {
		t.Fatalf("want ErrNoCapable, got %v", err)
	}
}

// TestAccumulateInMemoryUpdatesSnapshot 锁定「统计落库的同时也更新内存副本」这一
// 语义。SnapshotAccounts/SnapshotEndpoints 读的是内存 map，若只写 DB，管理 UI 会
// 一直显示上次 Refresh 时的陈旧数字。
func TestAccumulateInMemoryUpdatesSnapshot(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})

	b.accumulateInMemory(statOp{accountID: "a1", endpointID: "e1", ok: true, prompt: 10, completion: 5})
	b.accumulateInMemory(statOp{accountID: "a1", endpointID: "e1", ok: false, prompt: 3, completion: 0})
	b.accumulateInMemory(statOp{accountID: "a1", endpointID: "e1", ok: true, images: 2})

	accs := b.SnapshotAccounts()
	if len(accs) != 1 {
		t.Fatalf("want 1 account, got %d", len(accs))
	}
	a := accs[0]
	if a.TotalRequests != 3 || a.SuccessRequests != 2 || a.FailRequests != 1 {
		t.Fatalf("account counters: total=%d ok=%d fail=%d", a.TotalRequests, a.SuccessRequests, a.FailRequests)
	}
	if a.PromptTokens != 13 || a.CompletionTokens != 5 || a.TotalTokens != 18 {
		t.Fatalf("account tokens: p=%d c=%d t=%d", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
	if a.TotalImages != 2 {
		t.Fatalf("account images: %d", a.TotalImages)
	}
	if a.LastUsedAt == 0 {
		t.Fatal("LastUsedAt should be stamped")
	}

	eps := b.SnapshotEndpoints()
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	if eps[0].TotalRequests != 3 || eps[0].TotalTokens != 18 || eps[0].TotalImages != 2 {
		t.Fatalf("endpoint counters: total=%d tokens=%d images=%d", eps[0].TotalRequests, eps[0].TotalTokens, eps[0].TotalImages)
	}
}

// TestAccumulateInMemoryUnknownIDs 未知 ID 不应 panic（例如叶子刚被删除，
// 但仍有在途请求回报统计）。
func TestAccumulateInMemoryUnknownIDs(t *testing.T) {
	b := newTestBalancer()
	b.accumulateInMemory(statOp{accountID: "nope", endpointID: "nope", ok: true, prompt: 1})
}

// TestSessionSticky 锁定会话粘性语义：同一 stickyKey + 模型在 TTL 内固定到
// 同一叶子；粘性目标不可用时退回正常选举；TTL=0 时不粘。
func TestSessionSticky(t *testing.T) {
	b := newTestBalancer()
	b.sessionTTL = time.Minute
	b.seed([]*model.Account{mkAcc("a1", 1), mkAcc("a2", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
		mkEP("e2", "a2", "m", "ep-2", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})

	e1, err := b.Select("m", nil, nil, APIChat, "sk-1")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	b.Release(e1)
	for i := 0; i < 3; i++ {
		e, err := b.Select("m", nil, nil, APIChat, "sk-1")
		if err != nil {
			t.Fatalf("sticky select: %v", err)
		}
		if e.ID != e1.ID {
			t.Fatalf("sticky session must pin leaf %s, got %s", e1.ID, e.ID)
		}
		b.Release(e)
	}

	// 粘性目标熔断后应退回候选集里的另一片叶子。
	b.mu.Lock()
	b.endpoints[e1.ID].Runtime.CircuitOpenUntil = 1 << 62
	b.mu.Unlock()
	e2, err := b.Select("m", nil, nil, APIChat, "sk-1")
	if err != nil {
		t.Fatalf("fallback select: %v", err)
	}
	if e2.ID == e1.ID {
		t.Fatal("pinned leaf is circuit-open, must pick another")
	}
	b.Release(e2)

	// TTL=0（关闭）时不粘：两次选举允许落在不同叶子（不强制，但会话表不写入）。
	b2 := newTestBalancer()
	b2.sessionTTL = 0
	b2.seed([]*model.Account{mkAcc("a1", 1), mkAcc("a2", 1)}, []*model.Endpoint{
		mkEP("e1", "a1", "m", "ep-1", 1),
		mkEP("e2", "a2", "m", "ep-2", 1),
	}, []*model.Model{{Name: "m", Enabled: true}})
	if _, err := b2.Select("m", nil, nil, APIChat, "sk-1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(b2.sessions) != 0 {
		t.Fatalf("sessionTTL=0 must not record sessions, got %d", len(b2.sessions))
	}
}

// TestComputeCost 锁定成本核算公式：输入/输出 $/1M tokens、图像 $/张；
// 未定价模型为 0；Record 会把成本写回日志。
func TestComputeCost(t *testing.T) {
	b := newTestBalancer()
	b.prices["m"] = [3]float64{2, 8, 0.5} // 输入 $2/M、输出 $8/M、图像 $0.5/张

	got := b.computeCost("m", 1_000_000, 500_000, 2)
	if diff := got - (2 + 4 + 1); diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("want cost 7, got %v", got)
	}
	if got := b.computeCost("unknown-model", 1_000_000, 0, 0); got != 0 {
		t.Fatalf("unpriced model must cost 0, got %v", got)
	}

	// Record 应把成本写回日志（statOp 同步携带）。
	b.statCh = make(chan statOp, 8)
	b.logCh = make(chan *model.UsageLog, 8)
	l := &model.UsageLog{Model: "m", PromptTokens: 2_000_000}
	b.Record(l, nil, true, false)
	if l.Cost != 4 {
		t.Fatalf("Record must stamp cost on log, got %v", l.Cost)
	}
	<-b.statCh
	<-b.logCh
}
