package store

import (
	"testing"

	"arkgate/internal/model"
)

// 测试用计价函数：输入 $3/M、输出 $15/M、缓存读 $0.3/M、缓存写 $3.75/M。
// 与 e2e 用的价格一致，便于交叉核对。
func testCostFn(modelName string, prompt, completion, images, cacheRead, cacheWrite int64) (float64, float64, float64, bool) {
	if modelName != "priced" {
		return 0, 0, 0, false
	}
	plain := prompt - cacheRead - cacheWrite
	if plain < 0 {
		plain = 0
	}
	in := float64(plain) / 1e6 * 3
	cache := float64(cacheRead)/1e6*0.3 + float64(cacheWrite)/1e6*3.75
	out := float64(completion)/1e6*15 + float64(images)*0.04
	return in, out, cache, true
}

func approxEq(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// TestBackfillCostsRecomputesLegacyBilling 回填应把旧口径成本改成新口径。
//
// 旧口径：prompt_tokens 全额 × 输入价（缓存不区分）。
// 新口径：非缓存输入 × 输入价 + 缓存读/写 × 各自单价。
func TestBackfillCostsRecomputesLegacyBilling(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// 造一条「旧口径」日志：1000 prompt（600 读 + 100 写 + 300 非缓存）+ 100 输出。
	// 旧成本 = 1000/1e6*3 + 100/1e6*15 = 0.003 + 0.0015 = 0.0045
	if err := st.AddUsageLog(&model.UsageLog{
		TS: 1000, Model: "priced", Status: "ok",
		PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		CacheReadTokens: 600, CacheCreationTokens: 100,
		Cost: 0.0045, InputCost: 0.003, OutputCost: 0.0015, CacheCost: 0,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 先 dry-run：只统计，不写。
	res, err := st.BackfillCosts(0, 9999, testCostFn, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Scanned != 1 || res.Updated != 1 {
		t.Fatalf("dry-run 应报告 1 条待更新, got scanned=%d updated=%d", res.Scanned, res.Updated)
	}
	if !approxEq(res.OldTotal, 0.0045) {
		t.Fatalf("旧总额应为 0.0045, got %v", res.OldTotal)
	}
	// 新总额 = 0.0009 + 0.000375 + 0.00018 + 0.0015 = 0.002955
	if !approxEq(res.NewTotal, 0.002955) {
		t.Fatalf("新总额应为 0.002955, got %v", res.NewTotal)
	}

	// dry-run 不得写库。
	logs, _, _ := st.QueryUsageLogs(LogFilter{}, 10, 0)
	if !approxEq(logs[0].Cost, 0.0045) {
		t.Fatalf("dry-run 不应修改数据, cost=%v", logs[0].Cost)
	}

	// 真跑。
	res, err = st.BackfillCosts(0, 9999, testCostFn, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Updated != 1 {
		t.Fatalf("应更新 1 条, got %d", res.Updated)
	}
	logs, _, _ = st.QueryUsageLogs(LogFilter{}, 10, 0)
	l := logs[0]
	if !approxEq(l.InputCost, 0.0009) || !approxEq(l.CacheCost, 0.000555) ||
		!approxEq(l.OutputCost, 0.0015) || !approxEq(l.Cost, 0.002955) {
		t.Fatalf("回填后成本不对: in=%v cache=%v out=%v cost=%v",
			l.InputCost, l.CacheCost, l.OutputCost, l.Cost)
	}
	// 三拆分必须自洽（回填最容易犯的错是只改总额忘了拆分）。
	if !approxEq(l.InputCost+l.OutputCost+l.CacheCost, l.Cost) {
		t.Fatal("回填后三拆分之和 != 总额")
	}
}

// TestBackfillCostsIdempotent 重复回填不应再产生变更（幂等）。
func TestBackfillCostsIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	for i := 0; i < 3; i++ {
		if err := st.AddUsageLog(&model.UsageLog{
			TS: int64(100 + i), Model: "priced", Status: "ok",
			PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
			CacheReadTokens: 600, CacheCreationTokens: 100, Cost: 0.0045,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	first, err := st.BackfillCosts(0, 9999, testCostFn, false)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Updated != 3 {
		t.Fatalf("首轮应更新 3 条, got %d", first.Updated)
	}
	second, err := st.BackfillCosts(0, 9999, testCostFn, false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Updated != 0 {
		t.Fatalf("重复回填不应再变更, got %d", second.Updated)
	}
	// 两次的总额应完全一致。
	if !approxEq(first.NewTotal, second.NewTotal) {
		t.Fatalf("重复回填总额不稳定: %v vs %v", first.NewTotal, second.NewTotal)
	}
}

// TestBackfillCostsRespectsInterval 回填只动区间内的行。
//
// 这是安全边界：区间外（例如已对账的历史）必须原样不动。
func TestBackfillCostsRespectsInterval(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// 注意：必须给**所有**行带缓存 token，否则新老口径算出的成本恰好相同，
	// 逐行比较后会正确地跳过更新——那样就测不出「区间边界是否生效」了。
	// 初版测试没带缓存 token，断言「区间内被改动」直接不成立（测试写错，非代码错）。
	for _, ts := range []int64{100, 200, 300} {
		if err := st.AddUsageLog(&model.UsageLog{
			TS: ts, Model: "priced", Status: "ok",
			PromptTokens: 1000, TotalTokens: 1000,
			CacheReadTokens: 600, // 使新老口径产生差异：旧 0.003 → 新 0.00012+0.0018
			Cost:            0.003,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// 只回填 [150, 250) —— 应只命中 ts=200 那条。
	res, err := st.BackfillCosts(150, 250, testCostFn, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Scanned != 1 {
		t.Fatalf("应只扫描区间内 1 条, got %d", res.Scanned)
	}
	if res.Updated != 1 {
		t.Fatalf("区间内 1 条应被更新, got %d", res.Updated)
	}
	logs, _, _ := st.QueryUsageLogs(LogFilter{}, 10, 0)
	for _, l := range logs {
		switch l.TS {
		case 100, 300:
			if !approxEq(l.Cost, 0.003) {
				t.Fatalf("区间外 ts=%d 被改动: cost=%v", l.TS, l.Cost)
			}
		case 200:
			// 新口径 = (1000-600)/1e6*3 + 600/1e6*0.3 = 0.0012+0.00018 = 0.00138
			if !approxEq(l.Cost, 0.00138) {
				t.Fatalf("区间内 ts=200 回填值不对: cost=%v（期望 0.00138）", l.Cost)
			}
		}
	}
}

// TestBackfillCostsUnpricedReported 未定价模型的条数必须单独报出。
//
// 为什么重要：把有 token 的记录算成 0 元通常意味着模型被删或从未定价，
// 静默清零会让「回填后总额变少」被误读为「计费修正的收益」。
func TestBackfillCostsUnpricedReported(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	for i := 0; i < 2; i++ {
		if err := st.AddUsageLog(&model.UsageLog{
			TS: int64(100 + i), Model: "no-such-model", Status: "ok",
			PromptTokens: 500, TotalTokens: 500, Cost: 0.0015,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	res, err := st.BackfillCosts(0, 9999, testCostFn, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Unpriced != 2 {
		t.Fatalf("应报告 2 条未定价, got %d", res.Unpriced)
	}
	if res.NewTotal != 0 {
		t.Fatalf("未定价行新总额应为 0, got %v", res.NewTotal)
	}
}

// TestBackfillCostsLargeSet 超过单批上限（5000）时分页读取不重不漏。
func TestBackfillCostsLargeSet(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	const n = 5200 // 刻意跨过 5000 的批边界
	for i := 0; i < n; i++ {
		if err := st.AddUsageLog(&model.UsageLog{
			TS: int64(1000 + i), Model: "priced", Status: "ok",
			PromptTokens: 100, TotalTokens: 100, Cost: 0.0003,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	res, err := st.BackfillCosts(0, 999999, testCostFn, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Scanned != n {
		t.Fatalf("应扫描全部 %d 条（分页不重不漏）, got %d", n, res.Scanned)
	}
	if res.Updated != n {
		t.Fatalf("应更新全部 %d 条, got %d", n, res.Updated)
	}
	// 分页正确性由 Scanned/Updated == n 锁定（若分页漏行或重复行，这两个数会对不上）。
	// 再跑一次应 0 变更——证明回填幂等，且「逐行比较后跳过」避免了无意义写入。
	again, err := st.BackfillCosts(0, 999999, testCostFn, false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if again.Updated != 0 {
		t.Fatalf("二次回填应为 0 变更, got %d", again.Updated)
	}
}
