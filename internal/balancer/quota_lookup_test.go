package balancer

import (
	"testing"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/store"
)

// newQuotaTestBalancer 建一个带真实 SQLite 的 balancer（配额规则要从库里回源，
// 所以必须是真 store，不能是 nil）。
func newQuotaTestBalancer(t *testing.T) (*Balancer, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("打开 store: %v", err)
	}
	b := New(st, 0)
	// 必须关闭：Windows 上未关闭的 SQLite 句柄会让 t.TempDir 的清理失败
	// （报 "being used by another process"），表现为测试 FAIL 但断言其实全过。
	t.Cleanup(func() { b.Close(); st.Close() })
	return b, st
}

// TestQuotaRuleSeesFreshSubKey 锁定一个实测到的真 bug。
//
// 背景：子 Key 的增删改走 admin handler，而那些 handler **不调 Refresh**
// （Refresh 做三次全表扫描 + 抢写锁，代价太大）。所以如果配额规则只从
// Refresh 快照里读，新建的子 Key 会在「创建后 ~ 下次 Refresh 前」按**零值规则**
// 计——也就是按「自然日 + 默认倍率」而不是用户配置的周期。
//
// 实测表现（修复前）：新建「每周 + 8 点重置」的子 Key 后立刻请求，
// usage_daily 里写的是 `day=今天, window_secs=86400`（自然日），
// 门户读「本周窗口」自然读不到，显示 0。
//
// 修复：quotaRuleOf 带 TTL 缓存并回源查库。
func TestQuotaRuleSeesFreshSubKey(t *testing.T) {
	b, st := newQuotaTestBalancer(t)

	// 先 Refresh 一次，制造「索引已建好」的状态——之后新建的 Key 不在快照里。
	b.Refresh()

	sk := &model.SubKey{
		ID:                 "sk_fresh",
		Name:               "新建的周配额 Key",
		Enabled:            true,
		QuotaPeriod:        model.QuotaPeriodWeek,
		QuotaResetWeekday:  1,
		QuotaResetHour:     8,
		CacheReadPermille:  100,
		CacheWritePermille: 1250,
	}
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("写入子 Key: %v", err)
	}

	// 关键：**不调 Refresh**，直接读规则。
	rule := b.quotaRuleOf(sk.ID)
	if rule.Period != model.QuotaPeriodWeek {
		t.Fatalf("新建子 Key 的周期应立刻可见, got %q（说明配额缓存没回源，用了过期快照）", rule.Period)
	}
	if rule.ResetHour != 8 {
		t.Errorf("重置时刻应为 8, got %d", rule.ResetHour)
	}
	if rule.ReadPermille != 100 {
		t.Errorf("缓存读倍率应为 100, got %d", rule.ReadPermille)
	}

	// 同理：改配置也要立刻生效（否则「改了限额不生效」会被当成界面 bug）。
	sk.QuotaPeriod = model.QuotaPeriodMonth
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("更新子 Key: %v", err)
	}
	b.quotaMu.Lock()
	delete(b.quotaCache, sk.ID) // 清掉 TTL 缓存，模拟 5 秒后
	b.quotaMu.Unlock()
	if got := b.quotaRuleOf(sk.ID).Period; got != model.QuotaPeriodMonth {
		t.Fatalf("改周期后应立即生效, got %q", got)
	}
}

// TestQuotaRuleUnknownSubKeyFallsBackToDay 未知/已删子 Key 回落零值规则。
//
// 零值必须等价于「自然日 + 无加权」——QuotaCount 在无缓存 token 时恰好等于
// prompt+completion，这样升级前创建的旧子 Key 口径完全不变。
func TestQuotaRuleUnknownSubKeyFallsBackToDay(t *testing.T) {
	b, _ := newQuotaTestBalancer(t)

	for _, id := range []string{"", "sk_不存在"} {
		rule := b.quotaRuleOf(id)
		if rule.Period != model.QuotaPeriodDay {
			t.Errorf("id=%q 应回落自然日, got %q", id, rule.Period)
		}
		// 无缓存 token 时加权 == 原始 token 数（旧口径）。
		if got := model.QuotaCount(rule, 1000, 100, 0, 0); got != 1100 {
			t.Errorf("id=%q 无缓存时应等于 prompt+completion, got %d", id, got)
		}
	}
}

// TestQuotaRuleCacheTTL 锁定 TTL 语义：TTL 内命中缓存，过期后回源。
//
// 全是热路径行为，写错不会报错、只会「改配置不生效」或「每请求查库」。
func TestQuotaRuleCacheTTL(t *testing.T) {
	b, st := newQuotaTestBalancer(t)
	sk := &model.SubKey{ID: "sk_ttl", Name: "t", Enabled: true,
		QuotaPeriod: model.QuotaPeriodWeek}
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("写入: %v", err)
	}

	if got := b.quotaRuleOf("sk_ttl").Period; got != model.QuotaPeriodWeek {
		t.Fatalf("首次读取应回源拿到 week, got %q", got)
	}

	// 直接改库（绕过 balancer），TTL 内应仍读缓存 —— 证明「有缓存」。
	sk.QuotaPeriod = model.QuotaPeriodMonth
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("更新: %v", err)
	}
	if got := b.quotaRuleOf("sk_ttl").Period; got != model.QuotaPeriodWeek {
		t.Fatalf("TTL 内应命中缓存（仍是 week）, got %q", got)
	}

	// 把缓存时间戳往前拨超过 TTL，应回源拿到新值 —— 证明「会过期」。
	b.quotaMu.Lock()
	e := b.quotaCache["sk_ttl"]
	e.at = time.Now().Add(-2 * quotaCacheTTL)
	b.quotaCache["sk_ttl"] = e
	b.quotaMu.Unlock()
	if got := b.quotaRuleOf("sk_ttl").Period; got != model.QuotaPeriodMonth {
		t.Fatalf("TTL 过期后应回源拿到 month, got %q", got)
	}
}

// TestRefreshReseedsQuotaCache 锁定 Refresh 会把最新配置灌进配额缓存。
//
// 这样 admin 调了 Refresh（账号/模型/接入点变更时）就能立刻刷新配额口径，
// 不必干等 TTL。
func TestRefreshReseedsQuotaCache(t *testing.T) {
	b, st := newQuotaTestBalancer(t)
	sk := &model.SubKey{ID: "sk_r", Name: "r", Enabled: true,
		QuotaPeriod: model.QuotaPeriodDay}
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("写入: %v", err)
	}
	b.Refresh()

	sk.QuotaPeriod = model.QuotaPeriodWeek
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("更新: %v", err)
	}
	b.Refresh() // 应重新灌入
	if got := b.quotaRuleOf("sk_r").Period; got != model.QuotaPeriodWeek {
		t.Fatalf("Refresh 后应拿到最新周期 week, got %q", got)
	}
}
