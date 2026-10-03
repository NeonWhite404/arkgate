package store

import (
	"testing"
	"time"

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

// TestRepairDailyCosts 修复 usage_daily.cost 恒 0 的历史数据。
//
// 复现真实缺陷：某次重构漏填 statOp.cost，导致日用量表成本恒为 0，
// 而 usage_logs.cost 正常。门户「今日成本」直接显示 $0（用户可见），
// 周/总费用却正确——同一页面自相矛盾。
func TestRepairDailyCosts(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertSubKey(&model.SubKey{ID: "sk1", Name: "k", KeyHash: "h", Enabled: true}); err != nil {
		t.Fatalf("subkey: %v", err)
	}
	// 造「坏行」：日用量 cost=0（模拟缺陷期间的写入），日志 cost 正常。
	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		if err := st.AddUsageLog(&model.UsageLog{
			TS: now, SubKeyID: "sk1", Model: "m", Status: "ok",
			PromptTokens: 100, TotalTokens: 100, Cost: 0.002955,
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	if err := st.AddDailyUsage("sk1", 300, 0, 3, 0); err != nil { // cost=0：坏值
		t.Fatalf("daily: %v", err)
	}

	// 预演：应报告需要修复，但不写。
	n, err := st.RepairDailyCosts(now-3600, now+3600, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if n != 1 {
		t.Fatalf("预演应报告 1 天待修复, got %d", n)
	}
	du, _ := st.GetDailyUsage("sk1")
	if du.Cost != 0 {
		t.Fatalf("预演不应写库, got %v", du.Cost)
	}

	// 真跑。
	if _, err := st.RepairDailyCosts(now-3600, now+3600, false); err != nil {
		t.Fatalf("repair: %v", err)
	}
	du, _ = st.GetDailyUsage("sk1")
	want := 3 * 0.002955
	if !approxEq(du.Cost, want) {
		t.Fatalf("修复后日成本应为 %v, got %v", want, du.Cost)
	}
	// 关键：tokens/images/requests 必须**原样保留**——只修成本列，
	// 重算计数在日志被清理的场景下反而会把数字改小。
	if du.Tokens != 300 || du.Requests != 3 {
		t.Fatalf("修复成本时不应改动其它列: tokens=%d requests=%d", du.Tokens, du.Requests)
	}
	// 再次执行应无变化（幂等）。
	n, err = st.RepairDailyCosts(now-3600, now+3600, true)
	if err != nil {
		t.Fatalf("dry-run2: %v", err)
	}
	if n != 0 {
		t.Fatalf("幂等：二次预演应为 0, got %d", n)
	}
}

// TestBackfillPaginationNoSkipNoDup 锁定回填的分页正确性。
//
// 审计时的推理：查询 `ORDER BY id LIMIT ? OFFSET ?`，而批内会 UPDATE 命中行。
// 因 UPDATE **不修改 id、不修改 WHERE 条件（ts 区间）、更不修改排序键**，
// 后续批次的 OFFSET 仍精确跳过已处理行 → 不漏行、不重复行。
//
// 但「推理正确」不等于「有回归保护」：若将来有人把排序键改成 ts
// （ts 有重复值且可能被改动），或把 UPDATE 改成同时写 ts，分页立刻出错。
// 本测试用**刻意跨批边界**的数据量 + 严格校验每次 id 只被处理一次来锁定。
//
// 做法：用一个记录「每条 id 被扫描次数」的计价函数（CostFunc 每行调用一次），
// 断言每个 id 恰好被调用一次。
func TestBackfillPaginationNoSkipNoDup(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// 5200 条 > 批大小 5000，必然触发第二次分页。
	//
	// **ts 必须有重复值**：生产中 TS 取 nowUnix()，同秒并发请求会拿到相同 ts。
	// 初版测试给每行唯一递增 ts，结果「把排序键换成 ts」的反向验证竟然通过——
	// 数据没制造出会破坏分页的条件，测试等于没锁住任何东西。
	// 这里刻意让每 7 行共用同一个 ts（模拟同秒批量写入）。
	// **cost 必须各不相同**：反向验证时发现，若所有行 cost 相同（初版就是全
	// 0.003），「按 cost 排序」也是稳定的，模拟不出任何分页问题——测试等于
	// 只验证了「不会崩」，没验证「不重不漏」。
	// 这里让 cost 随 i 变化，使「排序键被 UPDATE 修改」这类改法必然暴露。
	const n = 5200
	for i := 0; i < n; i++ {
		if err := st.AddUsageLog(&model.UsageLog{
			TS: int64(1000 + i/7), Model: "paged", Status: "ok",
			PromptTokens: int64(1000 + i), TotalTokens: 1000,
			CacheReadTokens: 600,
			// 旧值刻意各不相同（排序敏感）；新值也各不相同但单调性改变，
			// 使「按 cost 排序 + 改写 cost」会在批次间重排 → OFFSET 漏行。
			Cost: 0.003 + float64(i%97)*1e-6,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// 计价函数被调用一次 = 该行被扫描一次。用 map 统计调用次数。
	// 注意 CostFunc 签名没有行 id，所以用「文本区间」间接验证不现实——
	// 改为在函数里按调用次数计数，并单独校验最终每行只被更新一次。
	var calls int64
	counting := func(modelName string, prompt, completion, images, cacheRead, cacheWrite int64) (float64, float64, float64, bool) {
		calls++
		return float64(prompt-cacheRead) / 1e6 * 3, 0, float64(cacheRead) / 1e6 * 0.3, true
	}

	res, err := st.BackfillCosts(0, 999999, counting, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// 每个 id 恰好扫描一次：既无漏行（< n）也无重复行（> n）。
	if calls != n {
		t.Fatalf("每行应恰好被扫描一次：期望 %d, got %d（漏行或重复行）", n, calls)
	}
	if res.Scanned != n {
		t.Fatalf("Scanned 应为 %d, got %d", n, res.Scanned)
	}
	if res.Updated != n {
		t.Fatalf("Updated 应为 %d, got %d（有行被跳过或重复更新）", n, res.Updated)
	}

	// 全量核对：统计仍是旧值 0.003 的行数——必须为 0（漏行的直接表现）。
	//
	// 用直接 SQL 而非 QueryUsageLogs：后者上限 200 行，抽样无法发现漏行。
	st.mu.RLock()
	// 新口径 = (prompt-cacheRead)/1e6*3 + cacheRead/1e6*0.3 = (1000+i-600)/1e6*3 + 0.00018
	// 未回填的行 = 旧值（0.003 + (i%97)*1e-6）。用「不等于新口径」来判定残留。
	var stale int64
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM usage_logs
		 WHERE model='paged'
		   AND ABS(cost - (CAST(prompt_tokens AS REAL)-600)/1e6*3 - 600/1e6*0.3) > 1e-9`,
	).Scan(&stale); err != nil {
		st.mu.RUnlock()
		t.Fatalf("count stale: %v", err)
	}
	var total int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE model='paged'`).Scan(&total); err != nil {
		st.mu.RUnlock()
		t.Fatalf("count total: %v", err)
	}
	st.mu.RUnlock()
	if total != n {
		t.Fatalf("数据量应为 %d, got %d", n, total)
	}
	if stale != 0 {
		t.Fatalf("有 %d 行仍是旧值 0.003（分页漏行）；期望全部被回填", stale)
	}
}
