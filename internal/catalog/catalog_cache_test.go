package catalog

import "testing"

// 内嵌快照补齐缓存价格后，Lookup 应能返回它们。
// 在此之前快照被裁剪掉了这两个字段，导致「从目录补全」拿不到缓存价——
// 用户必须手工填，而手工填的往往是错的（把缓存读当免费）。
func TestEmbeddedSnapshotHasCachePrices(t *testing.T) {
	c := New()
	if c.Count() < 1000 {
		t.Fatalf("目录条目过少: %d", c.Count())
	}
	// Anthropic：读约 10%、写约 125%
	e, ok := c.Lookup("claude-sonnet-4-5")
	if !ok {
		t.Skip("内嵌快照无该模型（上游目录变动），跳过")
	}
	if e.CostCacheRead <= 0 {
		t.Fatalf("claude-sonnet-4-5 缓存读价应已补齐, got %v", e.CostCacheRead)
	}
	if e.CostCacheWrite <= 0 {
		t.Fatalf("claude-sonnet-4-5 缓存写价应已补齐, got %v", e.CostCacheWrite)
	}
	// 商业关系：读显著低于输入价、写高于输入价。
	if !(e.CostCacheRead < e.CostIn*0.5) {
		t.Fatalf("缓存读应远低于输入价: read=%v in=%v", e.CostCacheRead, e.CostIn)
	}
	if !(e.CostCacheWrite > e.CostIn) {
		t.Fatalf("缓存写应高于输入价: write=%v in=%v", e.CostCacheWrite, e.CostIn)
	}
	t.Logf("claude-sonnet-4-5: in=$%.3f/M read=$%.3f/M write=$%.3f/M",
		e.CostIn, e.CostCacheRead, e.CostCacheWrite)

	// OpenAI：读约 50%
	g, ok := c.Lookup("gpt-4o")
	if ok && g.CostCacheRead > 0 {
		t.Logf("gpt-4o: in=$%.3f/M read=$%.3f/M", g.CostIn, g.CostCacheRead)
		if g.CostCacheRead >= g.CostIn {
			t.Fatalf("gpt-4o 缓存读应低于输入价: read=%v in=%v", g.CostCacheRead, g.CostIn)
		}
	}
}
