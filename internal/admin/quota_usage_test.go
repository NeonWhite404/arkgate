package admin

import (
	"testing"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/store"
)

// newQuotaTestAdmin 建一个只带 store 的 Admin（withQuotaUsage 只用 store）。
func newQuotaTestAdmin(t *testing.T) *Admin {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// bal/box/cfg 在该函数里用不到，留 nil 即可（这也是个断言：
	// withQuotaUsage 不得依赖转发组件，否则列表页会被转发状态拖累）。
	return &Admin{store: st}
}

// TestWithQuotaUsageUsesWeightedWindow 子 Key 列表的配额用量必须是
// **加权窗口口径**，且窗口起始日按各自的周期规则算。
//
// 为什么这条重要：管理员在列表里看到「已用 / 限额」，必须与限额判定
// 用的是同一个数（usage_daily.tokens，加权）。若这里错用原始
// total_tokens，管理员会看到「明明没超却显示超了」，或反过来。
func TestWithQuotaUsageUsesWeightedWindow(t *testing.T) {
	a := newQuotaTestAdmin(t)

	// 日周期 + 加权。
	day := &model.SubKey{ID: "sk_day", Name: "日", DailyLimitTokens: 1000,
		CacheReadPermille: 100, QuotaPeriod: model.QuotaPeriodDay}
	// 周周期（窗口起始日应落在周一）。
	week := &model.SubKey{ID: "sk_week", Name: "周", DailyLimitTokens: 5000,
		QuotaPeriod: model.QuotaPeriodWeek, QuotaResetWeekday: 1}
	if err := a.store.UpsertSubKey(day); err != nil {
		t.Fatal(err)
	}
	if err := a.store.UpsertSubKey(week); err != nil {
		t.Fatal(err)
	}

	// 按各自的窗口日写用量。
	dayWin := model.WindowDay(model.QuotaRuleOf(day), time.Now())
	weekWin := model.WindowDay(model.QuotaRuleOf(week), time.Now())
	// 日 Key 的用量写在**今天**；周 Key 的写在周窗口起始日。
	if err := a.store.AddWindowUsage(store.WindowUsage{SubKeyID: "sk_day",
		Day: dayWin, Tokens: 640, WindowSecs: 86400}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.AddWindowUsage(store.WindowUsage{SubKeyID: "sk_week",
		Day: weekWin, Tokens: 4321, WindowSecs: 604800}); err != nil {
		t.Fatal(err)
	}

	got := a.withQuotaUsage([]*model.SubKey{day, week})
	if len(got) != 2 {
		t.Fatalf("应返回 2 项, got %d", len(got))
	}
	if got[0].Quota == nil || got[1].Quota == nil {
		t.Fatalf("quota 不应为 nil: %+v", got)
	}
	if got[0].Quota.UsedTokens != 640 {
		t.Fatalf("日 Key 用量错误: %d", got[0].Quota.UsedTokens)
	}
	if got[0].Quota.WindowDay != dayWin || got[0].Quota.Period != model.QuotaPeriodDay {
		t.Fatalf("日 Key 窗口信息错误: %+v", got[0].Quota)
	}
	if !got[0].Quota.Weighted || got[0].Quota.ReadPermille != 100 {
		t.Fatalf("应按配置报告加权状态: %+v", got[0].Quota)
	}
	// 周 Key 必须读到**周窗口**的行，而不是今天的行——这是 v15 修过的
	// 「周/月周期读不到行导致进度永远为 0」缺陷的回归防线。
	if got[1].Quota.UsedTokens != 4321 {
		t.Fatalf("周 Key 应读到周窗口用量, got %d (window=%s)",
			got[1].Quota.UsedTokens, got[1].Quota.WindowDay)
	}
	if got[1].Quota.Period != model.QuotaPeriodWeek {
		t.Fatalf("周 Key 周期错误: %s", got[1].Quota.Period)
	}
	if got[1].Quota.Weighted {
		t.Fatalf("周 Key 未配倍率，Weighted 应为 false（界面据此显示「未加权」）: %+v", got[1].Quota)
	}
}

// TestWithQuotaUsageZeroWhenNoRows 无用量行时返回零值而不是 nil/错误。
// 界面据此显示 0% 而不是「—」或整列消失。
func TestWithQuotaUsageZeroWhenNoRows(t *testing.T) {
	a := newQuotaTestAdmin(t)
	sk := &model.SubKey{ID: "sk_empty", Name: "空", DailyLimitTokens: 999}
	if err := a.store.UpsertSubKey(sk); err != nil {
		t.Fatal(err)
	}
	got := a.withQuotaUsage([]*model.SubKey{sk})
	if len(got) != 1 || got[0].Quota == nil {
		t.Fatalf("应有 quota 字段: %+v", got)
	}
	if got[0].Quota.UsedTokens != 0 || got[0].Quota.Requests != 0 || got[0].Quota.Images != 0 {
		t.Fatalf("无行时应为零用量: %+v", got[0].Quota)
	}
	if got[0].Quota.LimitTokens != 999 {
		t.Fatalf("限额应透传: %+v", got[0].Quota)
	}
}

// TestWithQuotaUsageEmptyInput 空列表不 panic、不报错。
func TestWithQuotaUsageEmptyInput(t *testing.T) {
	a := newQuotaTestAdmin(t)
	if got := a.withQuotaUsage(nil); len(got) != 0 {
		t.Fatalf("nil 输入应为空, got %+v", got)
	}
	if got := a.withQuotaUsage([]*model.SubKey{}); len(got) != 0 {
		t.Fatalf("空切片应为空, got %+v", got)
	}
}

// TestWithQuotaUsageEmbedsSubKey 返回项必须内嵌原始 SubKey 字段
// （前端依赖 s.name / s.key / s.total_tokens 等已有列）。
func TestWithQuotaUsageEmbedsSubKey(t *testing.T) {
	a := newQuotaTestAdmin(t)
	sk := &model.SubKey{ID: "sk_x", Name: "名字", Key: "sk-plain"}
	if err := a.store.UpsertSubKey(sk); err != nil {
		t.Fatal(err)
	}
	got := a.withQuotaUsage([]*model.SubKey{sk})
	if got[0].SubKey == nil || got[0].Name != "名字" || got[0].Key != "sk-plain" {
		t.Fatalf("内嵌字段丢失: %+v", got[0])
	}
	if got[0].ID != "sk_x" {
		t.Fatalf("ID 应可直达: %s", got[0].ID)
	}
}
