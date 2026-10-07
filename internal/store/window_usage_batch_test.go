package store

import (
	"testing"

	"arkgate/internal/model"
)

// TestGetWindowUsageBatch 批量读取窗口用量。
//
// 用于子 Key 列表给每行显示「已用 / 限额」：逐个查会是 N+1（N 个子 Key → N 次
// 查询），列表页可观测地变慢。这里确认批量版结果与单查一致。
func TestGetWindowUsageBatch(t *testing.T) {
	s := newTestStore(t)

	// 三个子 Key，各自不同窗口起始日（周期可不同）与用量。
	if err := s.AddWindowUsage(WindowUsage{SubKeyID: "k1", Day: "2026-10-01",
		Tokens: 1000, Images: 1, Cost: 0.5, WindowSecs: 86400}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWindowUsage(WindowUsage{SubKeyID: "k1", Day: "2026-10-01",
		Tokens: 500, Images: 0, Cost: 0.25, WindowSecs: 86400}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWindowUsage(WindowUsage{SubKeyID: "k2", Day: "2026-09-28",
		Tokens: 7777, Images: 3, Cost: 1.5, WindowSecs: 604800}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetWindowUsageBatch(map[string]string{
		"k1": "2026-10-01",
		"k2": "2026-09-28",
		"k3": "2026-10-01", // 无行 → 零用量，不是错误
	})
	if err != nil {
		t.Fatalf("批量查询: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 项（含零用量的 k3）, got %d", len(got))
	}
	if got["k1"].Tokens != 1500 || got["k1"].Requests != 2 || got["k1"].Images != 1 {
		t.Fatalf("k1 聚合错误: %+v", got["k1"])
	}
	if got["k2"].Tokens != 7777 || got["k2"].Images != 3 {
		t.Fatalf("k2 错误: %+v", got["k2"])
	}
	if got["k3"].Tokens != 0 || got["k3"].Requests != 0 {
		t.Fatalf("k3 应为零用量: %+v", got["k3"])
	}

	// 与单查结果必须一致（批量版不能有独立口径）。
	single, err := s.GetWindowUsage("k1", "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	if single.Tokens != got["k1"].Tokens || single.Requests != got["k1"].Requests ||
		single.Images != got["k1"].Images {
		t.Fatalf("批量与单查不一致: %+v vs %+v", got["k1"], single)
	}

	// 窗口日不匹配时读不到（证明 day 是查询条件而非摆设）——周周期读成日窗口
	// 会让门户显示 0，是 v15 修过的一类缺陷。
	wrong, err := s.GetWindowUsageBatch(map[string]string{"k2": "2026-10-01"})
	if err != nil {
		t.Fatal(err)
	}
	if wrong["k2"].Tokens != 0 {
		t.Fatalf("窗口日不匹配时应为零用量, got %+v", wrong["k2"])
	}
}

// TestSubKeyRawTokensSince 批量读取子 Key 自各自起点起的**上游原始 token**。
//
// 这一列存在的理由是「加权配额」与「上游原始量」是两个量纲（见门户对比行
// 与子 Key 列表）：前者存 usage_daily（已乘缓存倍率），后者只能从 usage_logs
// 聚合。测试钉住四点：
//  1. 只统计 ts>=since 的日志（窗口外一律不计）；
//  2. 各 Key 的起点**各自生效**（周期不同不能共用一个时间）；
//  3. 无日志的 Key 返回 0 而不是缺失；
//  4. 与 SubKeyLogStats 同口径（都是 SUM(total_tokens)）。
func TestSubKeyRawTokensSince(t *testing.T) {
	s := newTestStore(t)

	// 用相对时间构造，避免把测试写死到某个日期。
	t0 := nowUnix()
	add := func(sub string, ts, total int64) {
		t.Helper()
		if err := s.AddUsageLog(&model.UsageLog{TS: ts, SubKeyID: sub,
			Model: "m", Status: "ok", TotalTokens: total}); err != nil {
			t.Fatal(err)
		}
	}
	// k1：窗口内 100+200，窗口外 9999（不得计入）。
	add("k1", t0-60, 100)
	add("k1", t0-30, 200)
	add("k1", t0-1000, 9999)
	// k2：起点更早，所以能拿到多于 k1 的量。
	add("k2", t0-60, 50)
	add("k2", t0-500, 500)

	got, err := s.SubKeyRawTokensSince(map[string]int64{
		"k1": t0 - 100,
		"k2": t0 - 600,
		"k3": t0 - 100, // 无日志 → 0，不是缺失
	})
	if err != nil {
		t.Fatalf("批量查询: %v", err)
	}
	if got["k1"] != 300 {
		t.Fatalf("k1 应只统计窗口内（300）, got %d（窗口外被算进来了？）", got["k1"])
	}
	if got["k2"] != 550 {
		t.Fatalf("k2 起点更早，应为 550, got %d（各自起点没生效？）", got["k2"])
	}
	if v, ok := got["k3"]; !ok || v != 0 {
		t.Fatalf("k3 应有 0 值而非缺失: %v ok=%v", v, ok)
	}

	// 与既有单 Key 聚合同口径（同一数据源，不能各自一套算法）。
	st, err := s.SubKeyLogStats("k2", t0-600)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens != got["k2"] {
		t.Fatalf("应与 SubKeyLogStats 一致: %d vs %d", st.Tokens, got["k2"])
	}
}

// TestSubKeyRawTokensSinceEmpty 空输入不报错、不发查询。
func TestSubKeyRawTokensSinceEmpty(t *testing.T) {
	s := newTestStore(t)
	for _, in := range []map[string]int64{nil, {}} {
		got, err := s.SubKeyRawTokensSince(in)
		if err != nil || len(got) != 0 {
			t.Fatalf("空输入应正常返回空 map: %v %+v", err, got)
		}
	}
}

// TestGetWindowUsageBatchEmpty 空输入不报错、不发查询。
func TestGetWindowUsageBatchEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetWindowUsageBatch(nil)
	if err != nil {
		t.Fatalf("nil 输入应正常返回: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应为空 map, got %+v", got)
	}
	got2, err := s.GetWindowUsageBatch(map[string]string{})
	if err != nil || len(got2) != 0 {
		t.Fatalf("空 map 输入: %v %+v", err, got2)
	}
}
