package store

import (
	"testing"
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
