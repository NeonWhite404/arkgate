package model

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		// 某些精简容器缺少 tzdata；跳过而不是误报失败。
		t.Skipf("时区数据不可用: %v", err)
	}
	return loc
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

// TestWindowStartDay 自然日窗口 + 自定义重置时刻。
func TestWindowStartDay(t *testing.T) {
	sh := mustLoad(t, "Asia/Shanghai")
	rule := QuotaRule{Period: QuotaPeriodDay, ResetHour: 0}

	cases := []struct{ in, want string }{
		{"2026-10-04 00:00", "2026-10-04"},
		{"2026-10-04 12:00", "2026-10-04"},
		{"2026-10-04 23:59", "2026-10-04"},
	}
	for _, c := range cases {
		if got := WindowDay(rule, at(t, sh, c.in)); got != c.want {
			t.Errorf("ResetHour=0 %s → %s, want %s", c.in, got, c.want)
		}
	}

	// ResetHour=8：每天 8 点开始新周期。8 点前仍属于「昨天」的窗口。
	rule8 := QuotaRule{Period: QuotaPeriodDay, ResetHour: 8}
	cases8 := []struct{ in, want string }{
		{"2026-10-04 07:59", "2026-10-03"},
		{"2026-10-04 08:00", "2026-10-04"},
		{"2026-10-04 23:59", "2026-10-04"},
		// 跨月边界：11/1 03:00 属于 10/31 的窗口。
		{"2026-11-01 03:00", "2026-10-31"},
		{"2026-11-01 08:00", "2026-11-01"},
	}
	for _, c := range cases8 {
		if got := WindowDay(rule8, at(t, sh, c.in)); got != c.want {
			t.Errorf("ResetHour=8 %s → %s, want %s", c.in, got, c.want)
		}
	}
}

// TestWindowStartWeek 周窗口。
//
// 关键用例是「先应用 ResetHour 再回退 weekday」——顺序反了会在
// ResetHour>0 时把周一早晨错误地算到上一周（见 WindowStart 注释第 1 条）。
func TestWindowStartWeek(t *testing.T) {
	sh := mustLoad(t, "Asia/Shanghai")
	// 2026-10-04 是周日；2026-09-28 是周一。
	rule := QuotaRule{Period: QuotaPeriodWeek, ResetWeekday: 1, ResetHour: 0}

	cases := []struct{ in, want string }{
		{"2026-09-28 00:00", "2026-09-28"}, // 周一
		{"2026-09-30 12:00", "2026-09-28"},
		{"2026-10-04 23:59", "2026-09-28"}, // 周日仍属该周
		{"2026-10-05 00:00", "2026-10-05"}, // 下周一
	}
	for _, c := range cases {
		if got := WindowDay(rule, at(t, sh, c.in)); got != c.want {
			t.Errorf("week %s → %s, want %s", c.in, got, c.want)
		}
	}

	// ResetHour=8 + 周一：周一 07:00 的业务日是周日 → 应回退到**上周一**。
	//
	// 这正是「顺序错误」会露馅的地方：
	//   正确：业务日 = 周日(10/04) → 回退到最近周一 = 09/28
	//   错误：先回退到周一(10/05)，发现 7<8 再退一周 = 09/28 —— 恰好相同！
	// 所以还要一个「周三 07:00」的用例来区分：
	//   正确：业务日 = 周二(09/29) → 回退到周一 = 09/28
	//   错误：回退到周一(09/28)，7<8 再退一周 = 09/21  ← 明显不同
	rule8 := QuotaRule{Period: QuotaPeriodWeek, ResetWeekday: 1, ResetHour: 8}
	cases8 := []struct{ in, want string }{
		{"2026-09-30 07:00", "2026-09-28"}, // 周三晨 → 本周一（不是上周一）
		{"2026-09-30 08:00", "2026-09-28"},
		{"2026-10-05 07:00", "2026-09-28"}, // 周一晨 → 业务日周日 → 上周一
		{"2026-10-05 08:00", "2026-10-05"}, // 周一起点
	}
	for _, c := range cases8 {
		got := WindowDay(rule8, at(t, sh, c.in))
		if got != c.want {
			t.Errorf("week+ResetHour=8 %s → %s, want %s（顺序错误会在此暴露）",
				c.in, got, c.want)
		}
	}

	// 自定义 weekday：以周日为起点（ISO 8601 编号 7）。
	// 这也是「为什么不用 time.Weekday 的 0=周日」的验证点：
	// 若沿用 0..6，周日(0) 会与「未设置」的零值撞车。
	ruleSun := QuotaRule{Period: QuotaPeriodWeek, ResetWeekday: 7, ResetHour: 0}
	if got := WindowDay(ruleSun, at(t, sh, "2026-09-30 12:00")); got != "2026-09-27" {
		t.Errorf("week(周日为始) 09-30 → %s, want 2026-09-27", got)
	}
	// 周六起始（编号 6）：09-30 是周三，最近的周六是 09-26。
	ruleSat := QuotaRule{Period: QuotaPeriodWeek, ResetWeekday: 6, ResetHour: 0}
	if got := WindowDay(ruleSat, at(t, sh, "2026-09-30 12:00")); got != "2026-09-26" {
		t.Errorf("week(周六为始) 09-30 → %s, want 2026-09-26", got)
	}
}

// TestWindowStartMonth 月窗口 + 跨月回退。
func TestWindowStartMonth(t *testing.T) {
	sh := mustLoad(t, "Asia/Shanghai")

	rule := QuotaRule{Period: QuotaPeriodMonth, ResetHour: 0}
	cases := []struct{ in, want string }{
		{"2026-10-01 00:00", "2026-10-01"},
		{"2026-10-31 23:59", "2026-10-01"},
		{"2026-11-01 00:00", "2026-11-01"},
		{"2026-02-28 12:00", "2026-02-01"},
	}
	for _, c := range cases {
		if got := WindowDay(rule, at(t, sh, c.in)); got != c.want {
			t.Errorf("month %s → %s, want %s", c.in, got, c.want)
		}
	}

	// ResetHour=8：每月 1 日 03:00 的业务日是上月最后一天
	// → 窗口起点应是**上月 1 号**，而不是本月 1 号。
	// 这锁定了 WindowStart 注释第 2 条（month 的 ResetHour 回退可能跨月）。
	rule8 := QuotaRule{Period: QuotaPeriodMonth, ResetHour: 8}
	cases8 := []struct{ in, want string }{
		{"2026-11-01 03:00", "2026-10-01"},
		{"2026-11-01 08:00", "2026-11-01"},
		{"2026-03-01 03:00", "2026-02-01"},
	}
	for _, c := range cases8 {
		if got := WindowDay(rule8, at(t, sh, c.in)); got != c.want {
			t.Errorf("month+ResetHour=8 %s → %s, want %s（跨月回退错误）",
				c.in, got, c.want)
		}
	}
}

// TestWindowStartDST 夏令时切换日的窗口。
//
// 这是最容易出错的一类：DST 切换当天的时间轴不是均匀 24 小时，
// 用 AddDate/Add 做算术的行为与直觉不同。这里用纽约时区锁定。
func TestWindowStartDST(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	rule := QuotaRule{Period: QuotaPeriodDay, ResetHour: 0}

	// 2026-03-08 是美东春季前移日（02:00 → 03:00，该日只有 23 小时）。
	// 当天任意时刻的窗口起点都应是 03-08。
	for _, in := range []string{"2026-03-08 00:30", "2026-03-08 12:00", "2026-03-08 23:30"} {
		if got := WindowDay(rule, at(t, ny, in)); got != "2026-03-08" {
			t.Errorf("DST 春季 %s → %s, want 2026-03-08", in, got)
		}
	}
	// 次日应正常翻页。
	if got := WindowDay(rule, at(t, ny, "2026-03-09 00:30")); got != "2026-03-09" {
		t.Errorf("DST 春季次日 → %s, want 2026-03-09", got)
	}

	// 2026-11-01 是美东秋季回退日（02:00 → 01:00，该日有 25 小时）。
	// 01:30 会出现两次，Go 解析取第一次；无论哪次都应在同一个窗口内。
	for _, in := range []string{"2026-11-01 00:30", "2026-11-01 01:30", "2026-11-01 23:30"} {
		if got := WindowDay(rule, at(t, ny, in)); got != "2026-11-01" {
			t.Errorf("DST 秋季 %s → %s, want 2026-11-01", in, got)
		}
	}

	// 周窗口跨 DST：起点必须仍是周一（不能因少/多一小时而偏移到周日/周二）。
	wk := QuotaRule{Period: QuotaPeriodWeek, ResetWeekday: 1, ResetHour: 0}
	if got := WindowDay(wk, at(t, ny, "2026-03-11 12:00")); got != "2026-03-09" {
		t.Errorf("DST 周窗口 → %s, want 2026-03-09", got)
	}
	if got := WindowDay(wk, at(t, ny, "2026-11-04 12:00")); got != "2026-11-02" {
		t.Errorf("DST 周窗口(秋) → %s, want 2026-11-02", got)
	}
}

// TestQuotaRuleOfDefaults 默认值与脏值处理。
func TestQuotaRuleOfDefaults(t *testing.T) {
	// 全零值（v15 之前的旧行）：等价「自然日 + **不加权**」。
	//
	// 「不加权」而不是「用默认倍率」是**兼容性硬要求**：旧子 Key 读出来
	// CacheReadPermille=0，若解释成默认 0.1x，同一份请求升级后会突然少扣
	// 一半额度（实测：1170 vs 2200）。加权必须是管理员的显式选择。
	r := QuotaRuleOf(&SubKey{})
	if r.Period != QuotaPeriodDay {
		t.Errorf("空周期应回落 day, got %q", r.Period)
	}
	if r.Weight {
		t.Error("未配置倍率的子 Key 不应启用加权（否则静默放宽旧额度）")
	}
	// 只配一个倍率也启用加权，另一个取默认值（不能当 0 = 免费）。
	r = QuotaRuleOf(&SubKey{CacheReadPermille: 500})
	if !r.Weight || r.ReadPermille != 500 || r.WritePermille != DefaultCacheWritePermille {
		t.Errorf("只配读倍率应启用加权且写取默认: Weight=%v read=%d write=%d",
			r.Weight, r.ReadPermille, r.WritePermille)
	}
	if r.ResetWeekday != 1 || r.ResetHour != 0 {
		t.Errorf("weekday/hour 默认应为 1/0, got %d/%d", r.ResetWeekday, r.ResetHour)
	}

	// 显式设置：不得被默认值覆盖。
	r = QuotaRuleOf(&SubKey{
		QuotaPeriod: "month", QuotaResetWeekday: 5, QuotaResetHour: 8,
		CacheReadPermille: 500, CacheWritePermille: 2000,
	})
	if r.Period != "month" || r.ResetWeekday != 5 || r.ResetHour != 8 ||
		r.ReadPermille != 500 || r.WritePermille != 2000 {
		t.Errorf("显式值被覆盖: %+v", r)
	}

	// 未知周期回落 day（不报错——库里可能有手改的脏值）。
	if got := NormalizeQuotaPeriod("hourly"); got != QuotaPeriodDay {
		t.Errorf("未知周期应回落 day, got %q", got)
	}

	// 越界 weekday/hour 被夹紧而不是 panic。
	r = QuotaRuleOf(&SubKey{QuotaResetWeekday: 99, QuotaResetHour: -5})
	if r.ResetWeekday != 1 || r.ResetHour != 0 {
		t.Errorf("越界值应回落默认, got %d/%d", r.ResetWeekday, r.ResetHour)
	}

	// 周日（ISO 7）必须可显式表达，不能被当成「未设置」。
	r = QuotaRuleOf(&SubKey{QuotaResetWeekday: 7})
	if r.ResetWeekday != 7 {
		t.Errorf("显式周日(7)应保留, got %d", r.ResetWeekday)
	}
}

// TestWindowSecs 窗口长度落库值。
func TestWindowSecs(t *testing.T) {
	if WindowSecs("") != 86400 || WindowSecs("day") != 86400 {
		t.Error("day 窗口应为 86400（含空值回落）")
	}
	if WindowSecs("week") != 7*86400 {
		t.Error("week 窗口应为 7*86400")
	}
	if WindowSecs("month") != 30*86400 {
		t.Error("month 窗口应为标称 30*86400")
	}
}

// TestQuotaCount 加权配额计数。
func TestQuotaCount(t *testing.T) {
	rule := QuotaRule{Period: QuotaPeriodDay, Weight: true, ReadPermille: 100, WritePermille: 1250}

	// 1000 prompt（600 读 + 100 写 + 300 普通）+ 100 输出
	// 普通部分 = 300 + 100 = 400
	// 读加权 = 600 * 0.1 = 60
	// 写加权 = 100 * 1.25 = 125
	// 合计 = 585
	if got := QuotaCount(rule, 1000, 100, 600, 100); got != 585 {
		t.Errorf("加权计数应为 585, got %d", got)
	}

	// 关键回归：无缓存时，加权计数必须**恰好等于**原口径（prompt+completion）。
	// 否则升级会静默改变所有现有子 Key 的额度。
	if got := QuotaCount(rule, 1000, 100, 0, 0); got != 1100 {
		t.Errorf("无缓存时应为原口径 1100, got %d", got)
	}

	// 同理：倍率全 1000（1.0x）时也应等于原口径。
	rule1x := QuotaRule{Weight: true, ReadPermille: 1000, WritePermille: 1000}
	if got := QuotaCount(rule1x, 1000, 100, 600, 100); got != 1100 {
		t.Errorf("1.0x 倍率应为原口径 1100, got %d", got)
	}

	// 缓存读被低估 → 同样用量消耗的配额更少（这是本功能的目的）。
	full := QuotaCount(QuotaRule{Weight: true, ReadPermille: 1000, WritePermille: 1000}, 1000, 0, 900, 0)
	discount := QuotaCount(QuotaRule{Weight: true, ReadPermille: 100, WritePermille: 1000}, 1000, 0, 900, 0)
	if !(discount < full) {
		t.Errorf("缓存倍率 0.1 应比 1.0 消耗更少配额: %d vs %d", discount, full)
	}

	// 脏数据夹紧：缓存量之和 > prompt 时，按**比例**收敛到恰好等于 prompt。
	// 600 read + 600 write、prompt=1000 → read=300, write=300, plain=400
	//（1.0x 倍率下加权计数 = 400+300+300 = 1000，总额不膨胀也不缩水）。
	if got := QuotaCount(QuotaRule{Weight: true, ReadPermille: 1000, WritePermille: 1000}, 1000, 0, 600, 600); got != 1000 {
		t.Errorf("脏数据应夹紧为 1000, got %d", got)
	}
	// 但拆分本身必须是按比例（不能偏袒便宜的那一侧——否则会系统性少扣配额）。
	plain, r, w := SplitPrompt(1000, 800, 800)
	if plain != 0 || r != 500 || w != 500 {
		t.Errorf("超报应按比例收敛为 (0,500,500), got (%d,%d,%d)", plain, r, w)
	}
	// 全零安全。
	if got := QuotaCount(rule, 0, 0, 0, 0); got != 0 {
		t.Errorf("零用量应为 0, got %d", got)
	}
}

// TestSplitPromptMatchesBilling 拆分结果必须与计费侧同口径。
//
// 配额与费用各算一套拆分是最危险的分歧点：同一份用量可能「费用算便宜、
// 配额算贵」。这里锁定不变量（三段之和 == prompt，无负值）。
func TestSplitPromptMatchesBilling(t *testing.T) {
	cases := []struct{ prompt, read, write int64 }{
		{1000, 600, 100}, {0, 0, 0}, {100, 0, 0}, {100, 100, 0}, {100, 0, 100},
		{100, 200, 0}, {100, 0, 200}, {100, 200, 200}, // 自相矛盾
		{-5, 10, 10}, // 负输入
	}
	for _, c := range cases {
		plain, r, w := SplitPrompt(c.prompt, c.read, c.write)
		if plain < 0 || r < 0 || w < 0 {
			t.Errorf("%v: 出现负值 plain=%d read=%d write=%d", c, plain, r, w)
		}
		p := c.prompt
		if p < 0 {
			p = 0
		}
		if plain+r+w != p {
			t.Errorf("%v: 三段之和 %d != prompt %d", c, plain+r+w, p)
		}
	}
}

// TestQuotaCountLegacyUnweighted 锁定向后兼容：**未配置倍率的子 Key 必须
// 按原口径计数**，即使上游报告了缓存 token。
//
// 这条测试的由来（实测出来的真 bug）：初版实现把「倍率 0」解释为
// 「用默认倍率 0.1x / 1.25x」，于是升级后**所有旧子 Key 的额度消耗都会变小**——
// 一条 1000 prompt + 100 输出、其中 600 读 100 写的请求，
// 旧口径记 1100，新实现记 585，**用户凭空多出一倍的额度**。
//
// 这不只是「数字不好看」：配额是硬性拒绝阈值，静默放宽等于资损。
// 而 e2e 里旧式子 Key 实测就是 1170（=2×585）而不是 2200（=2×1100）。
func TestQuotaCountLegacyUnweighted(t *testing.T) {
	legacy := QuotaRuleOf(&SubKey{}) // 旧行：三个新列全零
	if legacy.Weight {
		t.Fatal("旧子 Key 不应启用加权")
	}
	// 与「升级前唯一的计费口径」逐字一致。
	if got := QuotaCount(legacy, 1000, 100, 600, 100); got != 1100 {
		t.Errorf("旧子 Key 应记 1100（prompt+completion）, got %d", got)
	}
	// 无缓存 token 时新旧口径本来就相等——这里一并锁定，
	// 免得有人把「无缓存时相等」误当成「兼容性已验证」。
	weighted := QuotaRuleOf(&SubKey{CacheReadPermille: 100, CacheWritePermille: 1250})
	if got := QuotaCount(weighted, 1000, 100, 0, 0); got != 1100 {
		t.Errorf("无缓存 token 时加权口径也应等于 1100, got %d", got)
	}
	if got := QuotaCount(weighted, 1000, 100, 600, 100); got != 585 {
		t.Errorf("显式配置后应记 585, got %d", got)
	}
}

// TestQuotaRuleReadOnlyConfigKeepsWriteDefault 只调读取倍率时，
// 写入倍率取默认值而不是 0（0 = 免费会静默放宽额度）。
func TestQuotaRuleReadOnlyConfigKeepsWriteDefault(t *testing.T) {
	r := QuotaRuleOf(&SubKey{CacheReadPermille: 250})
	if r.ReadPermille != 250 {
		t.Errorf("读倍率应为配置值 250, got %d", r.ReadPermille)
	}
	if r.WritePermille != DefaultCacheWritePermille {
		t.Errorf("未配置的写倍率应取默认 %d（不能是 0=免费）, got %d",
			DefaultCacheWritePermille, r.WritePermille)
	}
	// 反向：只调写倍率时读取默认。
	r = QuotaRuleOf(&SubKey{CacheWritePermille: 2000})
	if r.ReadPermille != DefaultCacheReadPermille || r.WritePermille != 2000 {
		t.Errorf("只配写倍率: read=%d(应 %d) write=%d(应 2000)",
			r.ReadPermille, DefaultCacheReadPermille, r.WritePermille)
	}
}
