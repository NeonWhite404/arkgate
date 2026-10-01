package store

import (
	"testing"
	"time"

	"arkgate/internal/model"
)

// mkRollupLog 造一条带 v13 分类与缓存信息的日志。
func mkRollupLog(ts int64, model_, sub, acc string, pt, ct int64, status, kind string, cost float64, stream bool) model.UsageLog {
	l := modelLog(ts, model_, sub, acc, pt, ct, status, cost)
	l.ErrorKind = kind
	l.CacheReadTokens = pt / 4
	l.IsStream = stream
	if stream && status == "ok" {
		l.FirstTokenMs = 100
	}
	return l
}

// seedRollupData 灌入跨多个小时/多天的样本，覆盖成功、上游错误、客户端错误。
func seedRollupData(t *testing.T, s *Store, now int64) {
	t.Helper()
	samples := []struct {
		off    int64
		model_ string
		sub    string
		acc    string
		pt, ct int64
		status string
		kind   string
		cost   float64
		stream bool
	}{
		{-30, "m1", "s1", "a1", 100, 50, "ok", "", 0.10, true},
		{-120, "m2", "s1", "a2", 200, 25, "ok", "", 0.20, false},
		{-180, "m1", "s2", "a1", 300, 10, "error", model.ErrorKindUpstreamError, 0, false},
		{-4000, "m1", "s1", "a1", 400, 40, "ok", "", 0.40, true},
		{-4000 - 86400, "m2", "s2", "a2", 500, 60, "error", model.ErrorKindClientCancel, 0, false},
		{-4000 - 86400, "m1", "s1", "a1", 600, 70, "ok", "", 0.60, false},
		{-4000 - 2*86400, "m2", "s1", "a1", 700, 80, "error", model.ErrorKindClientInvalid, 0, false},
	}
	for _, x := range samples {
		l := mkRollupLog(now+x.off, x.model_, x.sub, x.acc, x.pt, x.ct, x.status, x.kind, x.cost, x.stream)
		if err := s.AddUsageLog(&l); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}
}

// TestRollupMatchesRaw 是本组测试的核心：对同一批数据，预聚合路径与原始表路径
// 必须给出**逐字段一致**的结果。
//
// 为什么必须锁死：两条路径一旦口径漂移（例如错误分类的 SQL 条件写法不同），会出现
// 「同一批数据、不同时间查询给出不同答案」——这是最难排查的一类 bug，因为两边的数
// 都「看起来合理」。历史上成功率口径就曾被客户端取消污染过。
func TestRollupMatchesRaw(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix() - 60 // 留出余量，避免边界
	seedRollupData(t, s, now)

	// 区间：覆盖全部样本，且右端早于 now（保证 rollup 能覆盖到）。
	to := now + 10
	from := now - 3*86400

	// 先跑全量 rollup。
	if _, _, err := s.RunRollupOnce(to); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// 注意：RunRollupOnce 的水位是 now-lookback，可能不足以覆盖 to。
	// 测试里显式把水位推到 to，模拟「历史区间已被聚合」的稳态。
	if err := s.SetRollupWatermark(to, now); err != nil {
		t.Fatalf("watermark: %v", err)
	}

	// 断言覆盖判断成立（否则下面的对比会退化成 raw vs raw，白测）。
	q := UsageQuery{From: from, To: to, Granularity: "hour"}
	if !s.rollupCovers(q) {
		t.Fatalf("rollupCovers 应为 true（水位=%d, to=%d）", mustWM(t, s), to)
	}

	got, err := s.QueryUsage(q)
	if err != nil {
		t.Fatalf("query rollup: %v", err)
	}
	if got.Source != "rollup" {
		t.Fatalf("应走 rollup 路径, 实际 source=%q", got.Source)
	}
	want, err := s.queryUsageRaw(q)
	if err != nil {
		t.Fatalf("query raw: %v", err)
	}
	assertUsageEqual(t, "hour", got, want)

	// 天粒度同样比对（走天表，与原始表的 CASE 分桶口径需一致）。
	qd := UsageQuery{From: from, To: to, Granularity: "day"}
	if s.rollupCovers(qd) {
		gotD, err := s.QueryUsage(qd)
		if err != nil {
			t.Fatalf("query rollup day: %v", err)
		}
		if gotD.Source != "rollup" {
			t.Fatalf("天粒度应走 rollup, source=%q", gotD.Source)
		}
		wantD, err := s.queryUsageRaw(qd)
		if err != nil {
			t.Fatalf("query raw day: %v", err)
		}
		assertUsageEqual(t, "day", gotD, wantD)
	}
}

func mustWM(t *testing.T, s *Store) int64 {
	t.Helper()
	wm, err := s.RollupWatermark()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	return wm
}

// assertUsageEqual 逐字段比对两条路径的结果（总量、时序、facet）。
// 时序/ facet 允许「桶集合顺序」不同，先归一化再比。
func assertUsageEqual(t *testing.T, label string, got, want *UsageQueryResult) {
	t.Helper()

	g, w := got.Summary, want.Summary
	if g.Requests != w.Requests || g.Success != w.Success {
		t.Fatalf("[%s] requests/success 不一致: rollup=%d/%d raw=%d/%d",
			label, g.Requests, g.Success, w.Requests, w.Success)
	}
	if g.PromptTokens != w.PromptTokens || g.CompletionTokens != w.CompletionTokens ||
		g.TotalTokens != w.TotalTokens || g.Images != w.Images {
		t.Fatalf("[%s] token/image 不一致: rollup=%d/%d/%d/%d raw=%d/%d/%d/%d", label,
			g.PromptTokens, g.CompletionTokens, g.TotalTokens, g.Images,
			w.PromptTokens, w.CompletionTokens, w.TotalTokens, w.Images)
	}
	if !feq(g.Cost, w.Cost) || !feq(g.InputCost, w.InputCost) ||
		!feq(g.OutputCost, w.OutputCost) || !feq(g.CacheCost, w.CacheCost) {
		t.Fatalf("[%s] 成本不一致: rollup=%v/%v/%v/%v raw=%v/%v/%v/%v", label,
			g.Cost, g.InputCost, g.OutputCost, g.CacheCost,
			w.Cost, w.InputCost, w.OutputCost, w.CacheCost)
	}
	if g.CacheCreationTokens != w.CacheCreationTokens || g.CacheReadTokens != w.CacheReadTokens {
		t.Fatalf("[%s] 缓存 token 不一致: rollup=%d/%d raw=%d/%d", label,
			g.CacheCreationTokens, g.CacheReadTokens, w.CacheCreationTokens, w.CacheReadTokens)
	}
	if g.UpstreamErrors != w.UpstreamErrors || g.ClientErrors != w.ClientErrors {
		t.Fatalf("[%s] 错误分类不一致: rollup up=%d cl=%d raw up=%d cl=%d", label,
			g.UpstreamErrors, g.ClientErrors, w.UpstreamErrors, w.ClientErrors)
	}
	if g.StreamRequests != w.StreamRequests || g.FirstTokenMsSum != w.FirstTokenMsSum {
		t.Fatalf("[%s] 流式/TTFT 不一致: rollup=%d/%d raw=%d/%d", label,
			g.StreamRequests, g.FirstTokenMsSum, w.StreamRequests, w.FirstTokenMsSum)
	}
	if g.LatencyMsSum != w.LatencyMsSum {
		t.Fatalf("[%s] 延迟和不一致: rollup=%d raw=%d", label, g.LatencyMsSum, w.LatencyMsSum)
	}

	// 时序：按 bucket 归一成 map 比对（两边的桶集合与每桶数值都必须一致）。
	gs, ws := bucketMap(got.Series), bucketMap(want.Series)
	if len(gs) != len(ws) {
		t.Fatalf("[%s] 时序桶数不一致: rollup=%d raw=%d\nrollup=%v\nraw=%v",
			label, len(gs), len(ws), bucketKeys(got.Series), bucketKeys(want.Series))
	}
	for k, gb := range gs {
		wb, ok := ws[k]
		if !ok {
			t.Fatalf("[%s] rollup 多出桶 %d（raw 无此桶）", label, k)
		}
		if gb.PromptTokens != wb.PromptTokens || gb.CompletionTokens != wb.CompletionTokens ||
			gb.Requests != wb.Requests || gb.Success != wb.Success ||
			gb.Images != wb.Images || !feq(gb.Cost, wb.Cost) ||
			gb.CacheReadTokens != wb.CacheReadTokens || gb.UpstreamErrors != wb.UpstreamErrors {
			t.Fatalf("[%s] 桶 %d 不一致:\nrollup=%+v\nraw   =%+v", label, k, gb, wb)
		}
	}

	// facets：按 key 归一比对。
	gf, wf := facetMap(got.Facets), facetMap(want.Facets)
	if len(gf) != len(wf) {
		t.Fatalf("[%s] facet 数不一致: rollup=%d raw=%d", label, len(gf), len(wf))
	}
	for k, gv := range gf {
		wv, ok := wf[k]
		if !ok {
			t.Fatalf("[%s] rollup 多出 facet %q", label, k)
		}
		if gv.Requests != wv.Requests || gv.Success != wv.Success ||
			gv.PromptTokens != wv.PromptTokens || gv.CompletionTokens != wv.CompletionTokens ||
			gv.TotalTokens != wv.TotalTokens || gv.Images != wv.Images || !feq(gv.Cost, wv.Cost) ||
			gv.CacheReadTokens != wv.CacheReadTokens || gv.UpstreamErrors != wv.UpstreamErrors {
			t.Fatalf("[%s] facet %q 不一致:\nrollup=%+v\nraw   =%+v", label, k, gv, wv)
		}
	}
}

func feq(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func bucketMap(in []*UsageBucket) map[int64]*UsageBucket {
	m := make(map[int64]*UsageBucket, len(in))
	for _, b := range in {
		m[b.Bucket] = b
	}
	return m
}

func bucketKeys(in []*UsageBucket) []int64 {
	out := make([]int64, 0, len(in))
	for _, b := range in {
		out = append(out, b.Bucket)
	}
	return out
}

func facetMap(in []*UsageFacet) map[string]*UsageFacet {
	m := make(map[string]*UsageFacet, len(in))
	for _, f := range in {
		m[f.Key] = f
	}
	return m
}

// TestRollupByDimension 逐个维度验证：带实体下钻时两条路径也必须一致。
func TestRollupByDimension(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix() - 60
	seedRollupData(t, s, now)
	to, from := now+10, now-3*86400
	if _, _, err := s.RunRollupOnce(to); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if err := s.SetRollupWatermark(to, now); err != nil {
		t.Fatalf("watermark: %v", err)
	}

	for _, dim := range []string{"", "model", "subkey", "account", "endpoint", "provider"} {
		for _, gran := range []string{"hour", "day"} {
			q := UsageQuery{From: from, To: to, Granularity: gran, Dim: dim}
			got, err := s.QueryUsage(q)
			if err != nil {
				t.Fatalf("dim=%q gran=%q: %v", dim, gran, err)
			}
			if got.Source != "rollup" {
				t.Fatalf("dim=%q gran=%q 应走 rollup, source=%q", dim, gran, got.Source)
			}
			want, err := s.queryUsageRaw(q)
			if err != nil {
				t.Fatalf("raw dim=%q gran=%q: %v", dim, gran, err)
			}
			assertUsageEqual(t, dim+"/"+gran, got, want)
		}
	}
}

// TestRollupEntityDrilldown 实体过滤下钻（facet 点击行）两条路径一致。
func TestRollupEntityDrilldown(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix() - 60
	seedRollupData(t, s, now)
	to, from := now+10, now-3*86400
	if _, _, err := s.RunRollupOnce(to); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if err := s.SetRollupWatermark(to, now); err != nil {
		t.Fatalf("watermark: %v", err)
	}

	cases := []struct{ dim, entity string }{
		{"model", "m1"},
		{"model", "m2"},
		{"subkey", "s1"},
		{"account", "a1"},
	}
	for _, c := range cases {
		q := UsageQuery{From: from, To: to, Granularity: "hour", Dim: c.dim, Entity: c.entity}
		got, err := s.QueryUsage(q)
		if err != nil {
			t.Fatalf("%s=%s: %v", c.dim, c.entity, err)
		}
		if got.Source != "rollup" {
			t.Fatalf("%s=%s 应走 rollup, source=%q", c.dim, c.entity, got.Source)
		}
		want, err := s.queryUsageRaw(q)
		if err != nil {
			t.Fatalf("raw %s=%s: %v", c.dim, c.entity, err)
		}
		assertUsageEqual(t, c.dim+"="+c.entity, got, want)
		if got.Summary.Requests == 0 {
			t.Fatalf("%s=%s 应有用量（否则测试退化成空对比）", c.dim, c.entity)
		}
	}
}

// TestRollupFallsBackToRaw 水位未覆盖 / 水位未建立时必须回落原始表，且结果正确。
func TestRollupFallsBackToRaw(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix() - 60
	seedRollupData(t, s, now)

	// 1) 水位为 0（从未聚合）：必须回落 raw。
	q := UsageQuery{From: now - 3*86400, To: now + 10, Granularity: "hour"}
	res, err := s.QueryUsage(q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Source != "raw" {
		t.Fatalf("水位未建立应回落 raw, source=%q", res.Source)
	}
	if res.Summary.Requests != 7 {
		t.Fatalf("raw 结果应含 7 条, got %d", res.Summary.Requests)
	}

	// 2) 水位早于区间右端（有实时数据未聚合）：必须回落 raw。
	if err := s.SetRollupWatermark(now-86400, now); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	res2, err := s.QueryUsage(UsageQuery{From: now - 3*86400, To: now + 10, Granularity: "hour"})
	if err != nil {
		t.Fatalf("query2: %v", err)
	}
	if res2.Source != "raw" {
		t.Fatalf("水位未覆盖区间右端应回落 raw, source=%q (watermark=%d to=%d)",
			res2.Source, mustWM(t, s), now+10)
	}
}
