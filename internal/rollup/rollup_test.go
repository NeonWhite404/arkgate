package rollup

import (
	"testing"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/store"
)

// TestLateLogNotLost 是本包最重要的测试：**迟到的日志不能被永久漏掉**。
//
// 背景（ArkGate 特有、比 sub2api 危险）：balancer.Record 是异步落库的，
// 一条 ts=T 的日志可能在 T+数秒才进表。若水位越过 T 之后不再回头，这条日志就
// 永远收不进聚合表——不报错、不告警，只会**永久少计**。
//
// 为什么用「跨小时」构造：AggregateHourly 会把 start 向前对齐到整点并重算整个
// 小时桶，所以同一小时内的迟到日志天然安全。真正的风险在**更早的小时桶**：
// 一旦水位越过那个小时，后续轮次不再触及它。本测试正是构造这种情形。
func TestLateLogNotLost(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	base := time.Now().Unix()
	// 选一个「上一小时」的桶，模拟某轮聚合已经把它覆盖过。
	oldTS := (base/3600)*3600 - 3600 + 60 // 上一小时的第 60 秒
	oldBucket := oldTS - oldTS%3600

	r := New(s, time.Hour)

	// 1) 第一轮把 [oldTS 所在小时, now) 聚合掉——此时还没有这条日志。
	r.runOnce()
	// 显式确认这一轮确实覆盖了 oldBucket（否则下面的断言没有意义）。
	if n, _ := s.RollupBucketForTest(oldBucket, "", ""); n != 0 {
		t.Fatalf("前置条件不成立：该桶本应还是空的, requests=%d", n)
	}
	wm1, err := s.RollupWatermark()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	// 2) 事后再插入一条属于「上一小时」的日志（模拟异步落库严重迟到）。
	l := model.UsageLog{
		TS: oldTS, SubKeyID: "s1", SubKeyName: "s1", AccountID: "a1", AccountName: "a1",
		Model: "m1", RequestedModel: "m1", Modality: model.ModelTypeText,
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		Status: "ok", LatencyMs: 12,
	}
	if err := s.AddUsageLog(&l); err != nil {
		t.Fatalf("add late log: %v", err)
	}

	// 3) 再跑一轮。
	r.runOnce()

	// 4) 这条迟到日志必须已被聚合进聚合表。
	// 直接查聚合表而**不是** QueryUsage：后者会在水位未覆盖时回落原始表，
	// 从而在「聚合漏了」的情况下也返回正确答案，测试就白写了。
	aggRequests, aggPrompt := s.RollupBucketForTest(oldBucket, "", "")
	if aggRequests != 1 || aggPrompt != 100 {
		wm2, _ := s.RollupWatermark()
		t.Fatalf("迟到日志未被聚合：bucket=%d requests=%d prompt=%d（期望 1/100）"+
			" wm1=%d wm2=%d oldTS=%d；说明水位推进越过了尚未落库的日志——永久性少计且不报错",
			oldBucket, aggRequests, aggPrompt, wm1, wm2, oldTS)
	}
	// 对外查询也必须正确。
	res, err := s.QueryUsage(store.UsageQuery{
		From: oldBucket, To: base + 60, Granularity: "hour",
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Summary.Requests != 1 || res.Summary.PromptTokens != 100 {
		t.Fatalf("查询结果漏了迟到日志: %+v", res.Summary)
	}
}

// TestWatermarkNeverExceedsLookback 锁定水位的推进边界：
// 水位必须始终落后于 now 至少一个 lookback，否则迟到日志会漏。
func TestWatermarkNeverExceedsLookback(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	r := New(s, time.Hour)
	r.runOnce()

	wm, err := s.RollupWatermark()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	// 允许几秒的测试执行误差。
	const slack = 5
	if wm > now-slack {
		t.Fatalf("水位(%d) 不得逼近 now(%d)：必须留出 lookback 窗口给迟到日志", wm, now)
	}
	if now-wm < 60 {
		t.Fatalf("水位落后量 %ds 过小，不足以吸收异步落库延迟", now-wm)
	}
}

// TestRunnerStopIsIdempotent 启停幂等：重复 Stop 不应 panic 或阻塞。
func TestRunnerStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	r := New(s, 50*time.Millisecond)
	// 未启动就 Stop：应安全返回。
	r.Stop()

	r.Start()
	r.Start() // 重复 Start 不叠加 goroutine
	time.Sleep(180 * time.Millisecond)
	r.Stop()
	r.Stop() // 重复 Stop 不 panic
	r.Stop()
}

// TestRunnerKeepsGoingAfterError 单轮失败不应让推进器退出：
// 查询会自动回落原始表，功能不受影响，但水位必须还能继续推进（自愈）。
func TestRunnerKeepsGoingAfterError(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	var rounds int
	r := New(s, 30*time.Millisecond)
	r.OnRound = func(rows, wm int64) { rounds++ }
	r.Start()
	defer r.Stop()

	// 等若干轮跑完，然后关库——之后聚合必然失败。
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && rounds < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if rounds < 2 {
		t.Fatalf("应至少跑完 2 轮, got %d", rounds)
	}

	// 关库后聚合一定报错，但进程不应崩溃、也不应停止被后续 Stop 回收。
	_ = s.Close()
	time.Sleep(120 * time.Millisecond)
	r.Stop() // 必须能正常停下
}
