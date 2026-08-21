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
