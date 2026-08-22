package balancer

import (
	"testing"

	"arkgate/internal/model"
)

// newTestBalancer 构造一个不走真实 store 的 Balancer（不启动 consumer）。
func newTestBalancer() *Balancer {
	return &Balancer{
		accounts:  map[string]*model.Account{},
		models:    map[string]*model.Model{},
		endpoints: map[string]*model.Endpoint{},
		modelApps: map[string][]*model.Endpoint{},
		wrrState:  map[string]*wrrState{},
	}
}

func (b *Balancer) seed(accounts []*model.Account, endpoints []*model.Endpoint, models []*model.Model) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range accounts {
		b.accounts[a.ID] = a
	}
	for _, e := range endpoints {
		e.Enabled = true
		e.EnsureRuntime()
		b.endpoints[e.ID] = e
		b.modelApps[e.Model] = append(b.modelApps[e.Model], e)
	}
	for _, m := range models {
		if m.Enabled {
			m.Enabled = true
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
		e, err := b.Select("m", nil, nil)
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

func TestSelectNoEndpoint(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1)}, nil, []*model.Model{{Name: "known", Enabled: true}})
	_, err := b.Select("unknown-model", nil, nil)
	if err != ErrNoEndpoint {
		t.Fatalf("want ErrNoEndpoint for unknown model with no mappings, got %v", err)
	}
	_, err = b.Select("known", nil, nil)
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

	_, err := b.Select("m", nil, nil)
	if err != ErrAllThrottled {
		t.Fatalf("want ErrAllThrottled, got %v", err)
	}
}

func TestCircuitBreakerRecord(t *testing.T) {
	b := newTestBalancer()
	e := mkEP("e1", "a1", "m", "ep-1", 1)
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{e}, nil)

	for i := 0; i < model.CircuitBreakerThreshold; i++ {
		b.Record(&model.UsageLog{AccountID: "a1", EndpointID: e.ID}, e, false)
	}
	e.EnsureRuntime()
	if e.Runtime.CircuitOpenUntil <= 0 {
		t.Fatalf("expected endpoint circuit to open after failures")
	}
	// 成功应复位熔断。（阻塞投递需 consumer 消费；直接读内存态即可）
	b.Record(&model.UsageLog{AccountID: "a1", EndpointID: e.ID}, e, true)
	if e.Runtime.CircuitOpenUntil != 0 || e.Runtime.ConsecutiveFailures != 0 {
		t.Fatalf("expected circuit reset on success")
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
		e, err := b.Select("m", nil, nil)
		if err != nil {
			t.Fatalf("select should still succeed via e2: %v", err)
		}
		if e.EP != "ep-2" {
			t.Fatalf("expected ep-2 (e1 throttled), got %s", e.EP)
		}
		b.Release(e)
	}
}

func TestPassthroughExcludeByAccount(t *testing.T) {
	b := newTestBalancer()
	b.seed([]*model.Account{mkAcc("a1", 1), mkAcc("a2", 1)}, nil, nil)

	// 无映射，模型名 ep- 开头 → 透传叶子 ID = passthrough:a1:ep-9 / passthrough:a2:ep-9。
	// 失败排出 a1 后，应选到 a2。
	exclude := map[string]bool{"passthrough:a1:ep-9": true}
	e, err := b.Select("ep-9", nil, exclude)
	if err != nil {
		t.Fatalf("select passthrough: %v", err)
	}
	if e.AccountID != "a2" {
		t.Fatalf("want a2 after excluding a1, got %s", e.AccountID)
	}
	if !e.Synthetic {
		t.Fatalf("expected synthetic leaf")
	}
	b.Release(e)
}

func TestTPMLimitThrottles(t *testing.T) {
	b := newTestBalancer()
	e := mkEP("e1", "a1", "m", "ep-1", 1)
	b.seed([]*model.Account{mkAcc("a1", 1)}, []*model.Endpoint{e}, []*model.Model{{Name: "m", Enabled: true}})
	b.mu.Lock()
	b.endpoints["e1"].TPMLimit = 100
	b.mu.Unlock()

	// 喂 101 tokens 后，TPM 窗口超限，Select 应返回 ErrAllThrottled。
	b.TPMAdd(e, 101)
	_, err := b.Select("m", nil, nil)
	if err != ErrAllThrottled {
		t.Fatalf("want ErrAllThrottled for TPM exhausted, got %v", err)
	}
}

func TestFallbackChain(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "a", Enabled: true, Fallback: []string{"b", "c"}},
		{Name: "b", Enabled: true, Fallback: []string{"d"}},
		{Name: "d", Enabled: true},
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
		{Name: "a", Enabled: true, Fallback: []string{"b", "c", "d"}},
	})
	got := b.FallbackChain("a")
	want := []string{"a", "b", "c", "d"}
	if !equalStrs(got, want) {
		t.Fatalf("all configured fallbacks must be kept: want %v, got %v", want, got)
	}
}

func TestFallbackChainDedupes(t *testing.T) {
	b := newTestBalancer()
	b.seed(nil, nil, []*model.Model{
		{Name: "a", Enabled: true, Fallback: []string{"b", "c"}},
		{Name: "b", Enabled: true, Fallback: []string{"c", "a"}},
		{Name: "c", Enabled: true},
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
		{Name: "a", Enabled: true, Fallback: []string{"b"}},
		{Name: "b", Enabled: true, Fallback: []string{"a"}},
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
		{Name: "m", Enabled: true, Fallback: []string{"m2"}},
		{Name: "m2", Enabled: true},
	})
	b.mu.Lock()
	b.endpoints["e1"].Runtime.CircuitOpenUntil = 1 << 62 // 熔断 m 的唯一元组
	b.mu.Unlock()

	ep, actual, err := b.SelectWithFallback("m", nil, nil, nil)
	if err != nil {
		t.Fatalf("select with fallback: %v", err)
	}
	if ep.EP != "ep-2" || actual != "m2" {
		t.Fatalf("want fallback to m2/ep-2, got %s/%s", actual, ep.EP)
	}
	b.Release(ep)
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

	accs := b.SnapshotAccounts()
	if len(accs) != 1 {
		t.Fatalf("want 1 account, got %d", len(accs))
	}
	a := accs[0]
	if a.TotalRequests != 2 || a.SuccessRequests != 1 || a.FailRequests != 1 {
		t.Fatalf("account counters: total=%d ok=%d fail=%d", a.TotalRequests, a.SuccessRequests, a.FailRequests)
	}
	if a.PromptTokens != 13 || a.CompletionTokens != 5 || a.TotalTokens != 18 {
		t.Fatalf("account tokens: p=%d c=%d t=%d", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
	if a.LastUsedAt == 0 {
		t.Fatal("LastUsedAt should be stamped")
	}

	eps := b.SnapshotEndpoints()
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	if eps[0].TotalRequests != 2 || eps[0].TotalTokens != 18 {
		t.Fatalf("endpoint counters: total=%d tokens=%d", eps[0].TotalRequests, eps[0].TotalTokens)
	}
}

// TestAccumulateInMemoryUnknownIDs 未知 ID 不应 panic（例如叶子刚被删除，
// 但仍有在途请求回报统计）。
func TestAccumulateInMemoryUnknownIDs(t *testing.T) {
	b := newTestBalancer()
	b.accumulateInMemory(statOp{accountID: "nope", endpointID: "nope", ok: true, prompt: 1})
}
