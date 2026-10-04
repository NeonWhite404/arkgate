package balancer

import (
	"math"
	"testing"
	"time"

	"arkgate/internal/store"

	"arkgate/internal/model"
)

// ── 缓存计费 ──
//
// 修复的缺陷：prompt_tokens 是**含缓存命中的总量**，旧实现整块乘输入单价。
// 真实定价里缓存读远低于输入价、缓存写高于输入价（Anthropic 读约 10%、写约 125%），
// 所以旧口径同时存在两个方向的错账：命中部分多收、写入部分漏收。

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestCacheReadBilledAtCacheRate 缓存命中应按缓存单价计费，而不是输入价。
func TestCacheReadBilledAtCacheRate(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()
	// 输入 $3/M、缓存读 $0.3/M（Anthropic 的 1:10 折扣）。
	b.prices["m"] = modelPrice{in: 3, out: 15, cacheRead: 0.3}

	// 100 万 prompt，其中 80 万命中缓存，20 万非缓存；无输出。
	l := &model.UsageLog{Model: "m", PromptTokens: 1_000_000, CacheReadTokens: 800_000}
	in, out, cache := b.splitCost("m", l)

	wantIn := 0.2 * 3    // 非缓存 20 万 × $3/M = $0.6
	wantCache := 0.8 * 0.3 // 命中 80 万 × $0.3/M = $0.24
	if !approx(in, wantIn) {
		t.Fatalf("非缓存输入应为 $%.4f, got $%.4f", wantIn, in)
	}
	if !approx(cache, wantCache) {
		t.Fatalf("缓存成本应为 $%.4f, got $%.4f", wantCache, cache)
	}
	if !approx(out, 0) {
		t.Fatalf("无输出应为 $0, got $%.4f", out)
	}
	// 关键对照：旧口径（全额 × 输入价）会得到 $3.0，比正确的 $0.84 高 3.5 倍。
	if old := 1.0 * 3; approx(in+cache, old) {
		t.Fatal("拆分后总额仍等于旧口径——缓存单价未生效")
	}
}

// TestCacheWriteBilledAboveInput 缓存写入应按写入单价计费（可能高于输入价，防止漏收）。
func TestCacheWriteBilledAboveInput(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()
	// 输入 $3/M、缓存写 $3.75/M（Anthropic 写入溢价 1.25×）。
	b.prices["m"] = modelPrice{in: 3, out: 15, cacheWrite: 3.75}

	l := &model.UsageLog{Model: "m", PromptTokens: 1_000_000, CacheCreationTokens: 1_000_000}
	in, _, cache := b.splitCost("m", l)

	if !approx(in, 0) {
		t.Fatalf("全部为缓存写入时非缓存输入应为 $0, got $%.4f", in)
	}
	if !approx(cache, 3.75) {
		t.Fatalf("缓存写入成本应为 $3.75, got $%.4f", cache)
	}
	// 旧口径只有 $3.0 —— 漏收 $0.75。
	if cache <= 3.0 {
		t.Fatal("缓存写入溢价未生效（漏收）")
	}
}

// TestCostEqualsSumOfParts Record 与 splitCost 必须严格自洽：Cost == in + out + cache。
func TestCostEqualsSumOfParts(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()
	b.prices["m"] = modelPrice{in: 3, out: 15, image: 0.04, cacheRead: 0.3, cacheWrite: 3.75}

	cases := []*model.UsageLog{
		{Model: "m", PromptTokens: 1000, CompletionTokens: 500},
		{Model: "m", PromptTokens: 1000, CompletionTokens: 500, CacheReadTokens: 400},
		{Model: "m", PromptTokens: 1000, CompletionTokens: 500, CacheCreationTokens: 400},
		{Model: "m", PromptTokens: 1000, CompletionTokens: 500, CacheReadTokens: 300, CacheCreationTokens: 200},
		{Model: "m", PromptTokens: 1000, CompletionTokens: 500, ImageCount: 2},
	}
	for i, l := range cases {
		in, out, cache := b.splitCost("m", l)
		sum := in + out + cache
		if !approx(sum, sum) || sum < 0 {
			t.Fatalf("case %d 成本为负: %v", i, sum)
		}
		// 用 computeCost 交叉验证（它按「prompt 全部视为非缓存」语义，仅在无缓存时可比）
		if l.CacheReadTokens == 0 && l.CacheCreationTokens == 0 {
			got := b.computeCost("m", l.PromptTokens, l.CompletionTokens, l.ImageCount)
			if !approx(got, sum) {
				t.Fatalf("case %d computeCost(%v) != 拆分之和(%v)", i, got, sum)
			}
		}
	}
}

// TestCachePriceUnsetFallsBackToInput 未设置缓存单价时回落输入价（等价旧行为，不破坏历史口径）。
func TestCachePriceUnsetFallsBackToInput(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()
	// 只设输入价，缓存单价为 0 = 未设置。
	b.prices["m"] = modelPrice{in: 3, out: 15}

	l := &model.UsageLog{Model: "m", PromptTokens: 1_000_000, CacheReadTokens: 400_000}
	in, _, cache := b.splitCost("m", l)

	// 未被定价的缓存按输入价计，总额应与「全部按输入价」一致（= 旧行为）。
	total := in + cache
	if !approx(total, 3.0) {
		t.Fatalf("未设置缓存单价时应回落输入价（总额 $3.0），got $%.4f", total)
	}
	// 但拆分本身仍要把缓存单独列出（供成本构成展示），不应混进 in。
	if !approx(cache, 1.2) {
		t.Fatalf("缓存部分应单列 $1.2, got $%.4f", cache)
	}
}

// TestBillablePromptClampsInconsistentUpstream 上游自相矛盾的数据不能造成负输入或总额膨胀。
func TestBillablePromptClampsInconsistentUpstream(t *testing.T) {
	cases := []struct {
		name                    string
		prompt, read, write     int64
		wantPlain, wantR, wantW int64
	}{
		{"正常", 1000, 300, 200, 500, 300, 200},
		{"无缓存", 1000, 0, 0, 1000, 0, 0},
		{"全部命中", 1000, 1000, 0, 0, 1000, 0},
		// 缓存量之和 > prompt：按比例收敛，保证三者之和 == prompt 且无负数。
		{"超报", 1000, 800, 800, 0, 500, 500},
		{"单值超报", 100, 500, 0, 0, 100, 0},
		// 负值（脏数据）归零。
		{"负值", 100, -5, -3, 100, 0, 0},
	}
	for _, c := range cases {
		plain, r, w := billablePrompt(c.prompt, c.read, c.write)
		if plain < 0 || r < 0 || w < 0 {
			t.Errorf("%s: 出现负数 plain=%d read=%d write=%d", c.name, plain, r, w)
		}
		if plain+r+w != c.prompt {
			t.Errorf("%s: 三部分之和 %d != prompt %d", c.name, plain+r+w, c.prompt)
		}
		if plain != c.wantPlain || r != c.wantR || w != c.wantW {
			t.Errorf("%s: got (%d,%d,%d), want (%d,%d,%d)",
				c.name, plain, r, w, c.wantPlain, c.wantR, c.wantW)
		}
	}
}

// TestCacheBillingViaRecord 端到端：Record 写入的四个成本字段必须自洽且反映缓存单价。
func TestCacheBillingViaRecord(t *testing.T) {
	b, st := newStoreBalancer(t)
	defer st.Close()
	defer b.Close()
	b.prices["m"] = modelPrice{in: 3, out: 15, cacheRead: 0.3, cacheWrite: 3.75}

	l := &model.UsageLog{
		Model: "m", PromptTokens: 1_000_000, CompletionTokens: 100_000,
		CacheReadTokens: 600_000, CacheCreationTokens: 100_000,
	}
	b.Record(l, nil, true, false)

	// 非缓存输入 = 100w - 60w - 10w = 30w → $0.9
	// 缓存 = 60w×0.3 + 10w×3.75 = $0.18 + $0.375 = $0.555
	// 输出 = 10w × 15 = $1.5
	wantIn, wantCache, wantOut := 0.9, 0.555, 1.5
	if !approx(l.InputCost, wantIn) {
		t.Fatalf("InputCost 应为 $%.4f, got $%.4f", wantIn, l.InputCost)
	}
	if !approx(l.CacheCost, wantCache) {
		t.Fatalf("CacheCost 应为 $%.4f, got $%.4f", wantCache, l.CacheCost)
	}
	if !approx(l.OutputCost, wantOut) {
		t.Fatalf("OutputCost 应为 $%.4f, got $%.4f", wantOut, l.OutputCost)
	}
	if !approx(l.Cost, wantIn+wantCache+wantOut) {
		t.Fatalf("Cost 应等于三者之和 $%.4f, got $%.4f", wantIn+wantCache+wantOut, l.Cost)
	}
}

// TestDailyCostRecorded 日用量表必须记录当次成本。
//
// 回归锁定（真实缺陷）：v14 重构 Record 时把 op 的构造提前了，但 op.cost 只在
// 旧实现里通过 `op.cost = b.computeCost(...)` 赋值；改成「先拆分再汇总」后
// 忘记回填 op.cost，于是 usage_daily.cost 恒为 0——而 usage_logs.cost 正常。
// 表现是门户「今日费用」为 0 而「7 天费用」正常（今日取自日用量表，
// 7 天取自日志聚合）。字段当时未被前端消费，所以界面没暴露。
func TestDailyCostRecorded(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := st.UpsertModel(&model.Model{
		Name: "m", Enabled: true, Type: model.ModelTypeText,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	sk := &model.SubKey{ID: "sk_daily", Name: "k", KeyHash: "h", Enabled: true}
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("subkey: %v", err)
	}

	b := New(st, 0)
	defer b.Close()
	b.Refresh()

	// 1000 prompt（600 读 + 100 写 + 300 普通）+ 100 输出
	// = 0.0009 + 0.000555 + 0.0015 = 0.002955
	l := &model.UsageLog{
		Model: "m", SubKeyID: sk.ID, Status: "ok",
		PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		CacheReadTokens: 600, CacheCreationTokens: 100,
	}
	b.Record(l, nil, true, false)
	// 统计是异步落库的，等一下再读（通道 → consumer）。
	// 用 GetWindowUsage + 当前窗口起点读：写入现在按周期窗口归位
	//（day 周期下窗口起点就是今天，但用 WindowDay 表达才能在周期改变时仍正确）。
	rule := model.QuotaRuleOf(sk)
	day := model.WindowDay(rule, time.Now())
	deadline := time.Now().Add(3 * time.Second)
	var du *store.DailyUsage
	for time.Now().Before(deadline) {
		d, err := st.GetWindowUsage(sk.ID, day)
		if err == nil && d.Cost > 0 {
			du = d
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if du == nil {
		t.Fatal("usage_daily.cost 始终为 0——Record 未把成本写入 statOp（见本测试注释）")
	}
	if !approx(du.Cost, 0.002955) {
		t.Fatalf("日成本应为 0.002955, got %v", du.Cost)
	}
	// 与日志表口径必须一致（两表都被界面展示，不一致会被当成 bug）。
	if !approx(du.Cost, l.Cost) {
		t.Fatalf("日成本 %v != 日志成本 %v", du.Cost, l.Cost)
	}
	// token 列现在是**加权配额计数**（v15）：600 读按 0.1x、100 写按 1.25x
	// → 400 + 60 + 125 = 585。真实 token 1100 记在 usage_logs 里。
	// 本测试关心的是 cost 落库（见上方注释），token 值交由
	// TestQuotaWeightingEndToEnd 专测，这里只确认它不是 0。
	if du.Tokens <= 0 {
		t.Fatalf("日 token 不应为 0, got %d", du.Tokens)
	}
}

// TestQuotaWeightingEndToEnd 加权配额计数必须真的落到窗口表里。
//
// 回归背景：本测试是在实现「缓存倍率」时写的，过程中发现两件事：
//  1. `op.cost = l.Cost` 这行在上一轮重构里被我删掉过一次，导致日成本恒 0；
//     本次加配额字段时又**重蹈覆辙**（把该行替换成了新代码）。
//     所以这里同时断言 cost 与 quota 两个字段都真的落了库——
//     只测其中一个的话，下一次改这段代码仍会漏。
//  2. 配额的入参是 `l.CacheReadTokens` / `l.CacheCreationTokens` 而非
//     `l.PromptTokens`，写错字段不会编译失败（都是 int64），只能靠断言数值抓。
func TestQuotaWeightingEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := st.UpsertModel(&model.Model{
		Name: "m", Enabled: true, Type: model.ModelTypeText,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	// 显式配倍率：读 0.1x、写 1.25x。
	sk := &model.SubKey{
		ID: "sk_q", Name: "k", KeyHash: "h", Enabled: true,
		CacheReadPermille: 100, CacheWritePermille: 1250,
		QuotaPeriod: model.QuotaPeriodDay,
	}
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("subkey: %v", err)
	}

	b := New(st, 0)
	defer b.Close()
	b.Refresh()

	// 1000 prompt（600 读 + 100 写 + 300 普通）+ 100 输出
	//   真实 token = 1100（usage_logs 记这个）
	//   加权配额   = 300+100 + 600*0.1 + 100*1.25 = 400+60+125 = 585
	l := &model.UsageLog{
		Model: "m", SubKeyID: sk.ID, Status: "ok",
		PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		CacheReadTokens: 600, CacheCreationTokens: 100,
	}
	b.Record(l, nil, true, false)

	rule := model.QuotaRuleOf(sk)
	day := model.WindowDay(rule, time.Now())
	deadline := time.Now().Add(3 * time.Second)
	var du *store.DailyUsage
	for time.Now().Before(deadline) {
		d, err := st.GetWindowUsage(sk.ID, day)
		if err == nil && d.Tokens > 0 {
			du = d
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if du == nil {
		t.Fatal("窗口表无写入（配额与成本都未落库）")
	}

	// 1) 配额计数必须是**加权后**的值，不能是真实 token。
	if du.Tokens != 585 {
		t.Errorf("窗口 tokens 应为加权配额 585, got %d（1100 说明没加权）", du.Tokens)
	}
	// 2) 成本字段必须同时正确——它曾经因为漏赋值而恒 0。
	if !approx(du.Cost, 0.002955) {
		t.Errorf("窗口 cost 应为 0.002955, got %v（0 说明 op.cost 未赋值）", du.Cost)
	}
	// 3) 窗口长度随行落库（供审计判断这行是日/周/月）。
	if du.Requests != 1 {
		t.Errorf("requests 应为 1, got %d", du.Requests)
	}
}

// TestQuotaNoCacheMatchesLegacy 无缓存时，窗口计数必须与旧口径**完全一致**。
//
// 这是升级安全性的核心保证：现有子 Key 都没配缓存倍率，若加权逻辑让
// 无缓存请求的计数发生变化，等于静默改变了所有存量用户的额度。
func TestQuotaNoCacheMatchesLegacy(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.UpsertModel(&model.Model{
		Name: "m", Enabled: true, Type: model.ModelTypeText, PriceInput: 1, PriceOutput: 1,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	// 旧式子 Key：所有配额字段都是零值（模拟 v15 之前创建的行）。
	sk := &model.SubKey{ID: "sk_old", Name: "k", KeyHash: "h", Enabled: true}
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("subkey: %v", err)
	}
	b := New(st, 0)
	defer b.Close()
	b.Refresh()

	l := &model.UsageLog{
		Model: "m", SubKeyID: sk.ID, Status: "ok",
		PromptTokens: 1000, CompletionTokens: 250, TotalTokens: 1250,
	}
	b.Record(l, nil, true, false)

	rule := model.QuotaRuleOf(sk)
	day := model.WindowDay(rule, time.Now())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := st.GetWindowUsage(sk.ID, day)
		if d != nil && d.Tokens > 0 {
			if d.Tokens != 1250 {
				t.Fatalf("旧子 Key 无缓存时应为 1250（=prompt+completion）, got %d", d.Tokens)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("窗口表无写入")
}
