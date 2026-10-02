package catalog

import (
	"encoding/json"
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestLookupEmbedded 锁定内嵌快照的查询口径：精确/大小写/点号归一/后缀键，以及
// 归一化换算（per-token → per-1M、图像成本双格式）。
func TestLookupEmbedded(t *testing.T) {
	c := New()
	if c.Count() < 1000 {
		t.Fatalf("embedded catalog too small: %d", c.Count())
	}
	if c.Source() != "embedded" {
		t.Fatalf("source = %q", c.Source())
	}
	e, ok := c.Lookup("gpt-4o")
	if !ok || e.MaxOutput != 16384 || e.MaxInput != 128000 ||
		!almost(e.CostIn, 2.5) || !almost(e.CostOut, 10) {
		t.Fatalf("gpt-4o entry: %+v ok=%v", e, ok)
	}
	// 大小写不敏感。
	if _, ok := c.Lookup("  GPT-4O "); !ok {
		t.Fatalf("case/trim-insensitive lookup failed")
	}
	// 版本号点号归一：目录键 gpt-3.5-turbo ↔ 查询 gpt-3-5-turbo。
	if e, ok := c.Lookup("gpt-3-5-turbo"); !ok || e.MaxOutput == 0 {
		t.Fatalf("digit-dot variant lookup failed: %+v", e)
	}
	// provider/name 键的后缀命中。
	if _, ok := c.Lookup("openai/gpt-4o"); !ok {
		t.Fatalf("slash-suffix lookup failed")
	}
	// 图像模型：input_cost_per_image → CostImage。
	if e, ok := c.Lookup("dall-e-3"); !ok || !almost(e.CostImage, 0.04) {
		t.Fatalf("dall-e-3 entry: %+v ok=%v", e, ok)
	}
	if _, ok := c.Lookup("definitely-not-a-model-xyz"); ok {
		t.Fatalf("unexpected hit for unknown model")
	}
	if _, ok := c.Lookup("   "); ok {
		t.Fatalf("blank name must not match")
	}
}

// TestReloadAndNormalize 锁定在线刷新解析：新旧最大输出字段兜底、图像成本双字段、
// 负值按未提供处理、空目录报错且内存不动。
func TestReloadAndNormalize(t *testing.T) {
	c := New()
	before := c.Count()
	full := []byte(`{
	  "test-a": {"max_tokens": 8192, "input_cost_per_token": 3e-07,
	             "output_cost_per_token": 1e-06, "mode": "chat", "litellm_provider": "x"},
	  "test-b": {"max_output_tokens": 32768, "max_input_tokens": 131072,
	             "input_cost_per_image": 0.04, "mode": "image_generation"},
	  "test-c": {"max_tokens": -1, "input_cost_per_token": 1e-6, "mode": "chat"},
	  "provider/test-d": {"max_output_tokens": 4096, "mode": "chat"}
	}`)
	if err := c.Reload(full); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.Source() != "remote" || c.Count() != 4 || before == 0 {
		t.Fatalf("after reload: source=%s count=%d (before=%d)", c.Source(), c.Count(), before)
	}
	e, ok := c.Lookup("test-a")
	if !ok || e.MaxOutput != 8192 || !almost(e.CostIn, 0.3) || !almost(e.CostOut, 1) || e.Provider != "x" {
		t.Fatalf("test-a entry: %+v ok=%v", e, ok)
	}
	e, ok = c.Lookup("test-b")
	if !ok || e.MaxOutput != 32768 || e.MaxInput != 131072 || !almost(e.CostImage, 0.04) {
		t.Fatalf("test-b entry: %+v ok=%v", e, ok)
	}
	// 负值视为未提供：max_tokens=-1 → MaxOutput=0（条目仍有价格，正常入库）。
	if e, ok = c.Lookup("test-c"); !ok || e.MaxOutput != 0 {
		t.Fatalf("test-c entry: %+v ok=%v", e, ok)
	}
	// provider/name 键既可全名命中也可后缀命中。
	if _, ok = c.Lookup("provider/test-d"); !ok {
		t.Fatalf("full slash key lookup failed")
	}
	if _, ok = c.Lookup("test-d"); !ok {
		t.Fatalf("slash suffix lookup failed")
	}
	// 空目录报错，且原索引保持可用。
	if err := c.Reload([]byte(`{}`)); err == nil {
		t.Fatalf("empty reload must fail")
	}
	if c.Source() != "remote" || c.Count() != 4 {
		t.Fatalf("failed reload must not touch state: source=%s count=%d", c.Source(), c.Count())
	}
	if _, ok = c.Lookup("test-a"); !ok {
		t.Fatalf("index unusable after failed reload")
	}
}

// TestDigitDots 锁定版本号点号归一：只转换数字之间的连字符。
// 多版本段会逐段转换（qwen-2-5-72b → qwen-2.5.72b）——变换对索引与查询
// 两侧对称，单版本段（gpt-3-5-turbo → gpt-3.5-turbo）即可正确命中。
func TestDigitDots(t *testing.T) {
	cases := map[string]string{
		"doubao-1-5-pro-32k": "doubao-1.5-pro-32k",
		"gpt-4o":             "gpt-4o",
		"gpt-4o-2024":        "gpt-4o-2024", // 数字-字母不动
		"qwen-2-5-72b":       "qwen-2.5.72b",
	}
	for in, want := range cases {
		if got := digitDots(in); got != want {
			t.Fatalf("digitDots(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseCachePrices 缓存单价解析：LiteLLM 的 cache_read_input_token_cost /
// cache_creation_input_token_cost 必须换算为 $/1M，且**只取基础档**。
//
// 为什么要锁「只取基础档」：上游目录里同名价格有几十个分级变体
// （_above_200k_tokens、_batches、_priority、_flex…），那些是特定上下文长度/
// 服务档位的价格。本项目不建模分级定价，误取高价档会让计费偏高——比不填更糟。
func TestParseCachePrices(t *testing.T) {
	var entries map[string]rawEntry
	raw := []byte(`{
		"claude-3-5-sonnet": {
			"input_cost_per_token": 3e-06,
			"output_cost_per_token": 1.5e-05,
			"cache_read_input_token_cost": 3e-07,
			"cache_creation_input_token_cost": 3.75e-06,
			"cache_read_input_token_cost_above_200k_tokens": 6e-07,
			"cache_creation_input_token_cost_above_200k_tokens": 7.5e-06,
			"cache_read_input_token_cost_batches": 1.5e-07,
			"litellm_provider": "anthropic",
			"mode": "chat"
		},
		"no-cache-prices": {
			"input_cost_per_token": 1e-06,
			"output_cost_per_token": 2e-06,
			"mode": "chat"
		},
		"zero-cache-price": {
			"input_cost_per_token": 1e-06,
			"cache_read_input_token_cost": 0,
			"mode": "chat"
		}
	}`)
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cr := entries["claude-3-5-sonnet"]
	e := cr.normalize()
	// 换算为 $/1M：3e-07 → 0.3，3.75e-06 → 3.75。
	if !almost(e.CostCacheRead, 0.3) {
		t.Fatalf("缓存读取单价应为 $0.3/M, got %v", e.CostCacheRead)
	}
	if !almost(e.CostCacheWrite, 3.75) {
		t.Fatalf("缓存写入单价应为 $3.75/M, got %v", e.CostCacheWrite)
	}
	// 分级变体不得被误取（基础档才是默认档）。
	if almost(e.CostCacheRead, 0.6) || almost(e.CostCacheWrite, 7.5) {
		t.Fatal("取到了 _above_200k_tokens 分级档，应按基础档计价")
	}
	// 商业含义：读 10% 折扣、写 25% 溢价，与输入价的关系应成立。
	if !(e.CostCacheRead < e.CostIn) {
		t.Fatalf("缓存读取应低于输入价: read=%v in=%v", e.CostCacheRead, e.CostIn)
	}
	if !(e.CostCacheWrite > e.CostIn) {
		t.Fatalf("缓存写入应高于输入价: write=%v in=%v", e.CostCacheWrite, e.CostIn)
	}

	// 上游没有缓存价格 → 保持 0（= 未设置），计费回落到输入价。
	nc := entries["no-cache-prices"]
	if got := nc.normalize(); got.CostCacheRead != 0 || got.CostCacheWrite != 0 {
		t.Fatalf("无缓存价格时应为 0, got read=%v write=%v", got.CostCacheRead, got.CostCacheWrite)
	}
	// 显式 0（非正值）同样按未设置处理，避免用 0 当作「免费」。
	zc := entries["zero-cache-price"]
	if got := zc.normalize(); got.CostCacheRead != 0 {
		t.Fatalf("显式 0 应按未设置处理, got %v", got.CostCacheRead)
	}
}
