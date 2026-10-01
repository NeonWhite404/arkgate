package balancer

import (
	"sync/atomic"
	"testing"
	"time"

	"arkgate/internal/model"
)

// 审计：pin 在已熔断叶子上的会话，其后续请求应能正常改选其它叶子（不应被粘死）。
func TestAuditStickyToBrokenEndpoint(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()

	acc := &model.Account{ID: "a", Name: "a", Provider: "openai", Status: "active", Weight: 1}
	e1 := &model.Endpoint{ID: "e1", AccountID: "a", Model: "m", EP: "up1", Enabled: true, Weight: 1}
	e2 := &model.Endpoint{ID: "e2", AccountID: "a", Model: "m", EP: "up2", Enabled: true, Weight: 1}
	mdl := &model.Model{Name: "m", Type: model.ModelTypeText, Enabled: true}
	b.seed([]*model.Account{acc}, []*model.Endpoint{e1, e2}, []*model.Model{mdl})
	b.sessionTTL = time.Minute

	// 首次选举把会话钉在某个叶子上。
	first, err := b.Select("m", nil, nil, APIChat, "sk1")
	if err != nil {
		t.Fatalf("首次选举失败: %v", err)
	}
	pinned := first.ID
	other := "e1"
	if pinned == "e1" {
		other = "e2"
	}

	// 让被钉住的叶子彻底熔断（冷却未到期）。
	atomic.StoreInt64(&b.endpoints[pinned].Runtime.CircuitOpenUntil, time.Now().Add(time.Minute).UnixNano())

	// 后续同 key 请求应改选另一个叶子，而不是失败。
	got, err := b.Select("m", nil, nil, APIChat, "sk1")
	if err != nil {
		t.Fatalf("粘住的叶子熔断后应改选其它叶子, got err=%v", err)
	}
	if got.ID != other {
		t.Fatalf("应改选 %s, got %s", other, got.ID)
	}
}

// 审计：粘住的叶子处于 HalfOpen，且探测资格被他人占用时，请求应改选其它叶子。
func TestAuditStickyToHalfOpenEndpoint(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()

	acc := &model.Account{ID: "a", Name: "a", Provider: "openai", Status: "active", Weight: 1}
	e1 := &model.Endpoint{ID: "e1", AccountID: "a", Model: "m", EP: "up1", Enabled: true, Weight: 1}
	e2 := &model.Endpoint{ID: "e2", AccountID: "a", Model: "m", EP: "up2", Enabled: true, Weight: 1}
	mdl := &model.Model{Name: "m", Type: model.ModelTypeText, Enabled: true}
	b.seed([]*model.Account{acc}, []*model.Endpoint{e1, e2}, []*model.Model{mdl})
	b.sessionTTL = time.Minute

	first, _ := b.Select("m", nil, nil, APIChat, "sk1")
	pinned := first.ID
	other := "e1"
	if pinned == "e1" {
		other = "e2"
	}
	rt := b.endpoints[pinned].Runtime
	// HalfOpen 且探测资格已被占用。
	atomic.StoreInt64(&rt.CircuitOpenUntil, time.Now().Add(-time.Millisecond).UnixNano())
	atomic.StoreInt32(&rt.Probing, 1)
	atomic.StoreInt64(&rt.ProbeStartedAt, time.Now().UnixNano())

	got, err := b.Select("m", nil, nil, APIChat, "sk1")
	if err != nil {
		t.Fatalf("探测中的叶子不应让请求失败: %v", err)
	}
	if got.ID != other {
		t.Fatalf("应改选 %s, got %s", other, got.ID)
	}
}

// 审计：所有叶子都在 HalfOpen 且探测资格被占用时，应返回明确错误而非 nil。
func TestAuditAllHalfOpenReturnsError(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()

	acc := &model.Account{ID: "a", Name: "a", Provider: "openai", Status: "active", Weight: 1}
	ep := &model.Endpoint{ID: "e1", AccountID: "a", Model: "m", EP: "up1", Enabled: true, Weight: 1}
	mdl := &model.Model{Name: "m", Type: model.ModelTypeText, Enabled: true}
	b.seed([]*model.Account{acc}, []*model.Endpoint{ep}, []*model.Model{mdl})

	rt := b.endpoints["e1"].Runtime
	atomic.StoreInt64(&rt.CircuitOpenUntil, time.Now().Add(-time.Millisecond).UnixNano())
	atomic.StoreInt32(&rt.Probing, 1)
	atomic.StoreInt64(&rt.ProbeStartedAt, time.Now().UnixNano())

	got, err := b.Select("m", nil, nil, APIChat, "")
	if err == nil {
		t.Fatal("全部探测中时应返回错误（否则调用方会拿到 nil 叶子）")
	}
	if got != nil {
		t.Fatal("出错时不应返回叶子")
	}
	t.Logf("错误: %v", err)
}
