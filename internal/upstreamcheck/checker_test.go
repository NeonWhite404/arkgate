package upstreamcheck

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/provider"
	"arkgate/internal/store"
)

// fakeStore 记录每次批量同步的调用（账号 → 当前有效 EP 集合），
// 供检查器行为断言；错误可注入。
type fakeStore struct {
	mu      sync.Mutex
	accts   []*model.Account
	syncs   map[string][]string // 每账号最后一次同步的集合
	syncErr error
}

func (f *fakeStore) ListAccounts() ([]*model.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*model.Account, len(f.accts))
	copy(out, f.accts)
	return out, nil
}

func (f *fakeStore) SyncEndpointUpstreamPresence(accountID string, presentEPs []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncErr != nil {
		return 0, f.syncErr
	}
	f.syncs[accountID] = append([]string(nil), presentEPs...)
	return 0, nil
}

func (f *fakeStore) lastSync(accountID string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.syncs[accountID]
	return v, ok
}

func (f *fakeStore) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncs)
}

// fakeLister 用「账号名 → 模型 ID 列表」模拟上游；返回的列表在调用时复制，
// 允许测试在运行中热改（模拟模型重新出现）。
type fakeLister struct {
	mu    sync.Mutex
	byAcc map[string][]string
	block chan struct{} // 非 nil：每次调用在此阻塞，直到收到信号
	err   error
	calls int
}

func (f *fakeLister) call(ctx context.Context, rt provider.Route, timeout time.Duration) ([]provider.UpstreamModel, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	err := f.err
	list := append([]string(nil), f.byAcc[rt.Key]...)
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]provider.UpstreamModel, 0, len(list))
	for _, id := range list {
		out = append(out, provider.UpstreamModel{ID: id})
	}
	return out, nil
}

func (f *fakeLister) setModels(accKey string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byAcc[accKey] = ids
}

func (f *fakeLister) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeLister) setBlock(block chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = block
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testCfg 组装一个用假依赖驱动的检查器（不启动 Run，手动触发 runOnce）。
// st 接受任意 AccountStore 实现：fakeStore（行为断言）或真实 store（豁免等 SQL 语义）。
func testCfg(st AccountStore, lister *fakeLister) *Checker {
	resolve := func(acc *model.Account) (provider.Route, error) {
		return provider.Route{BaseURL: "http://fake", Key: acc.ID}, nil
	}
	return New(st, resolve, lister.call, time.Minute, 5*time.Second)
}

func TestCheckerMarksMissingAndRestores(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{"a1": {"ep-1"}}}
	c := testCfg(st, up)

	c.runOnce()
	if got, ok := st.lastSync("a1"); !ok || len(got) != 1 || got[0] != "ep-1" {
		t.Fatalf("first sync = %v %v", got, ok)
	}

	// 上游模型消失：下一轮同步集合缩小，缺失项由 store 层标记（这里锁定集合口径）。
	up.setModels("a1")
	c.runOnce()
	if got, ok := st.lastSync("a1"); !ok || len(got) != 0 {
		t.Fatalf("after removal sync = %v %v", got, ok)
	}

	// 重新出现：集合恢复。
	up.setModels("a1", "ep-1", "ep-2")
	c.runOnce()
	if got, _ := st.lastSync("a1"); len(got) != 2 || got[0] != "ep-1" || got[1] != "ep-2" {
		t.Fatalf("after restore sync = %v", got)
	}
}

func TestCheckerEmptyListMarksAll(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	// 成功空列表：与「失败」不同，会以空集合触发同步。
	up := &fakeLister{byAcc: map[string][]string{"a1": {}}}
	c := testCfg(st, up)

	c.runOnce()
	if got, ok := st.lastSync("a1"); !ok || len(got) != 0 {
		t.Fatalf("empty success must sync empty set, got %v %v", got, ok)
	}
}

func TestCheckerFailureKeepsState(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{"a1": {"ep-1"}}}
	c := testCfg(st, up)

	up.setErr(errors.New("boom"))
	c.runOnce()
	if n := st.syncCount(); n != 0 {
		t.Fatalf("failed probe must not write state, got %d syncs", n)
	}
	// HTTP 错误同理。
	up.setErr(&provider.HTTPError{Code: 401, Body: []byte("nope")})
	c.runOnce()
	if n := st.syncCount(); n != 0 {
		t.Fatalf("HTTP failure must not write state, got %d syncs", n)
	}
}

func TestCheckerSkipsDisabledAccount(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
		{ID: "a2", Name: "乙", Status: model.AccountDisabled},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{"a1": {"ep-1"}, "a2": {"ep-2"}}}
	c := testCfg(st, up)

	c.runOnce()
	if _, ok := st.lastSync("a1"); !ok {
		t.Fatalf("active account must be synced")
	}
	if _, ok := st.lastSync("a2"); ok {
		t.Fatalf("disabled account must be skipped")
	}
}

func TestCheckerSameEPAcrossAccountsIsolated(t *testing.T) {
	// 同名 EP 挂在两个账号下：每个账号按自己的上游列表独立同步。
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
		{ID: "a2", Name: "乙", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{
		"a1": {"ep-common", "ep-a"},
		"a2": {"ep-b"},
	}}
	c := testCfg(st, up)

	c.runOnce()
	got1, _ := st.lastSync("a1")
	got2, _ := st.lastSync("a2")
	if len(got1) != 2 || len(got2) != 1 || got2[0] != "ep-b" {
		t.Fatalf("per-account sync: a1=%v a2=%v", got1, got2)
	}
}

func TestCheckerSlowProbeDoesNotOverlap(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{"a1": {"ep-1"}}}
	block := make(chan struct{})
	up.setBlock(block)
	// Run 是单工作协程：ticker 只有在本轮结束后才会触发下一轮。
	// 探测阻塞时，任何时刻在途调用数都必须 <=1（无重叠）。
	c := New(st, func(acc *model.Account) (provider.Route, error) {
		return provider.Route{BaseURL: "http://fake", Key: acc.ID}, nil
	}, up.call, 5*time.Millisecond, 5*time.Second)
	c.Run()
	defer c.Stop()

	deadline := time.Now().Add(time.Second)
	for up.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if up.callCount() == 0 {
		t.Fatal("run must start an immediate round")
	}
	// 在探测阻塞期间反复观察：ticker 周期已过多次，调用数必须停在 1。
	time.Sleep(40 * time.Millisecond)
	if got := up.callCount(); got != 1 {
		t.Fatalf("probe overlapped while blocked: calls=%d", got)
	}
	close(block)
	// 释放后第二轮应能接上（此前被 ticker 积压的触发最多合并为一次）。
	deadline = time.Now().Add(time.Second)
	for up.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := up.callCount(); got < 2 {
		t.Fatalf("next round never ran after unblock: calls=%d", got)
	}
	c.Stop()
}

func TestCheckerStopUnblocksRunningProbe(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{"a1": {"ep-1"}}}
	block := make(chan struct{})
	up.setBlock(block)
	// 周期取长：本轮阻塞在探测上，ticker 不会干扰。
	c := New(st, func(acc *model.Account) (provider.Route, error) {
		return provider.Route{BaseURL: "http://fake", Key: acc.ID}, nil
	}, up.call, time.Hour, 5*time.Second)
	c.Run()

	// 等探测进入阻塞（调用数到 1 且未完成）。
	deadline := time.Now().Add(time.Second)
	for up.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if up.callCount() == 0 {
		t.Fatal("run must start an immediate round")
	}

	stopped := make(chan struct{})
	go func() {
		// 必须在探测释放前就能返回：取消生命周期根让在途探测立刻退出。
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		// 通过：Stop 未被在途探测阻塞。
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked by in-flight probe")
	}
	close(block)
	// 取消后的探测不该留下任何同步写入。
	if n := st.syncCount(); n != 0 {
		t.Fatalf("cancelled probe must not write state, got %d syncs", n)
	}
}

func TestCheckerRunTickerStops(t *testing.T) {
	st := &fakeStore{accts: []*model.Account{
		{ID: "a1", Name: "甲", Status: model.AccountActive},
	}, syncs: map[string][]string{}}
	up := &fakeLister{byAcc: map[string][]string{"a1": {"ep-1"}}}
	c := New(st, func(acc *model.Account) (provider.Route, error) {
		return provider.Route{BaseURL: "http://fake", Key: acc.ID}, nil
	}, up.call, 10*time.Millisecond, 5*time.Second)

	c.Run()
	// 启动即跑一轮 + 短周期下再跑若干轮；等第一批同步出现即可。
	deadline := time.Now().Add(2 * time.Second)
	for st.syncCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	first := st.syncCount()
	if first == 0 {
		t.Fatal("run must execute an immediate round")
	}
	c.Stop()
	// 等一个周期以上：Stop 后 ticker 必须停，不能再有新增同步。
	time.Sleep(60 * time.Millisecond)
	if after := st.syncCount(); after != first {
		t.Fatalf("syncs grew after Stop: %d -> %d", first, after)
	}
}

// TestCheckerHonorsEndpointExemption 端到端豁免（接入点粒度）：接真实 store
// （豁免语义在 SQL 里，fake 记录器表达不了）+ 假上游，锁定「同模型下豁免的接入点
// 不被标记、未豁免的兄弟接入点照常标记」，以及撤销豁免后重新参与检查。
func TestCheckerHonorsEndpointExemption(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	// 同一模型 m1 下两条接入点：只有 e-ex 豁免——粒度证据就在同模型互为兄弟。
	for _, e := range []*model.Endpoint{
		{ID: "e-ck", AccountID: "a1", Model: "m1", EP: "ep-ck", Enabled: true},
		{ID: "e-ex", AccountID: "a1", Model: "m1", EP: "ep-ex", Enabled: true, SkipUpstreamCheck: true},
	} {
		if err := st.UpsertEndpoint(e); err != nil {
			t.Fatalf("upsert endpoint %s: %v", e.ID, err)
		}
	}
	if err := st.UpsertAccount(&model.Account{
		ID: "a1", Name: "甲", Provider: "custom", BaseURL: "http://fake", Status: model.AccountActive,
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	// 上游模型列表为空：未豁免的 e-ck 应被标记，豁免的 e-ex 不动。
	up := &fakeLister{byAcc: map[string][]string{"a1": {}}}
	c := testCfg(st, up)
	c.runOnce()

	deleted := func(id string) bool {
		e, err := st.GetEndpoint(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return e.UpstreamDeleted
	}
	if !deleted("e-ck") {
		t.Fatalf("non-exempt endpoint must be marked when missing upstream")
	}
	if deleted("e-ex") {
		t.Fatalf("exempt endpoint must never be marked")
	}

	// 撤销豁免后重新参与检查（同一模型下，兄弟不受影响）。
	ep, err := st.GetEndpoint("e-ex")
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	ep.SkipUpstreamCheck = false
	if err := st.UpsertEndpoint(ep); err != nil {
		t.Fatalf("revoke exemption: %v", err)
	}
	c.runOnce()
	if !deleted("e-ex") {
		t.Fatalf("endpoint must rejoin check after revoking exemption")
	}
}
