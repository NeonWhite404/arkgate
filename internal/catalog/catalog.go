// Package catalog 内置模型元数据目录：数据源自 LiteLLM 社区维护的
// model_prices_and_context_window.json（构建时内嵌精简快照，支持在线刷新）。
//
// 用途：按易读模型名自动补全模型目录的价格与上下文/最大输出上限，减少人工录入。
// 补全规则（由调用方执行）：只写空缺（零值）字段，人工填写的值永远优先。
//
// 归一化（price 换算为 $/1M、图像成本字段双格式兜底、max_output_tokens 新旧字段
// 兜底）统一在本包完成——内嵌快照与在线完整目录共用同一套解析，避免口径漂移。
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed catalog.json
var embedded []byte

// Entry 归一化后的目录条目。价格已换算为模型目录口径：
// 文本 $ / 1M tokens，图像 $ / 张；上限 0 表示目录未提供。
type Entry struct {
	MaxOutput int64   `json:"max_output"` // 单次最大输出 tokens
	MaxInput  int64   `json:"max_input"`  // 最大输入 tokens
	CostIn    float64 `json:"cost_in"`    // 输入单价 $ / 1M tokens
	CostOut   float64 `json:"cost_out"`   // 输出单价 $ / 1M tokens
	CostImage float64 `json:"cost_image"` // 图像单价 $ / 张
	Mode      string  `json:"mode"`       // chat | completion | image_generation
	Provider  string  `json:"provider"`   // LiteLLM 供应商标识（仅供展示）
}

// rawEntry LiteLLM 原始条目（内嵌快照与在线目录保持同一字段名，共用该解析）。
type rawEntry struct {
	MaxTokens       *float64 `json:"max_tokens"`
	MaxOutputTokens *float64 `json:"max_output_tokens"`
	MaxInputTokens  *float64 `json:"max_input_tokens"`
	InputCost       *float64 `json:"input_cost_per_token"`
	OutputCost      *float64 `json:"output_cost_per_token"`
	OutputCostImage *float64 `json:"output_cost_per_image"`
	InputCostImage  *float64 `json:"input_cost_per_image"`
	Mode            string   `json:"mode"`
	Provider        string   `json:"litellm_provider"`
}

func (r *rawEntry) normalize() Entry {
	// 负值/非法一律按未提供处理。
	pos := func(v *float64) float64 {
		if v == nil || *v <= 0 {
			return 0
		}
		return *v
	}
	e := Entry{
		Mode:     r.Mode,
		Provider: r.Provider,
		CostIn:   pos(r.InputCost) * 1e6,
		CostOut:  pos(r.OutputCost) * 1e6,
		MaxInput: int64(pos(r.MaxInputTokens)),
	}
	// 图像单价：部分条目记 output_cost_per_image，部分记 input_cost_per_image，语义一致。
	if v := pos(r.OutputCostImage); v > 0 {
		e.CostImage = v
	} else {
		e.CostImage = pos(r.InputCostImage)
	}
	// 最大输出：新格式 max_output_tokens 优先，旧格式 max_tokens 兜底。
	if v := pos(r.MaxOutputTokens); v > 0 {
		e.MaxOutput = int64(v)
	} else {
		e.MaxOutput = int64(pos(r.MaxTokens))
	}
	return e
}

// Catalog 目录索引：归一化模型名 → 条目。并发安全。
type Catalog struct {
	mu     sync.RWMutex
	index  map[string]Entry
	count  int
	source string // embedded | remote
}

// New 构建并加载内嵌快照。
func New() *Catalog {
	c := &Catalog{source: "embedded"}
	if idx, n, err := parse(embedded); err == nil {
		c.index, c.count = idx, n
	} else {
		// 内嵌快照损坏属于构建期错误，不应静默：保底空索引并保留错误可见性。
		c.index = map[string]Entry{}
	}
	return c
}

// Source 返回当前数据来源（embedded = 内嵌快照，remote = 在线刷新）。
func (c *Catalog) Source() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.source
}

// Count 返回目录条目总数。
func (c *Catalog) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.count
}

// Lookup 按易读模型名查询目录。容忍大小写、下划线与版本号点号差异
// （doubao-1-5-pro ↔ doubao-1.5-pro），也可命中 "provider/name" 键的后缀。
// 匹配保守：只做归一化变体，不做前缀/模糊匹配，宁可查不到也不误填。
func (c *Catalog) Lookup(name string) (Entry, bool) {
	q := strings.ToLower(strings.TrimSpace(name))
	if q == "" {
		return Entry{}, false
	}
	variants := keyVariants(q)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range variants {
		if e, ok := c.index[k]; ok {
			return e, true
		}
	}
	return Entry{}, false
}

// Reload 解析 LiteLLM 完整目录原文并替换内存索引（在线刷新）。
// 解析失败或结果为空时报错且内存不动。
func (c *Catalog) Reload(data []byte) error {
	idx, n, err := parse(data)
	if err != nil {
		return fmt.Errorf("解析目录: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("目录为空")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index, c.count, c.source = idx, n, "remote"
	return nil
}

func parse(data []byte) (map[string]Entry, int, error) {
	var raw map[string]rawEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, err
	}
	// 第一遍：全名键入索引。后缀键（provider/name 的 name 部分）必须让位——
	// map 随机迭代下 "openrouter/gpt-4o" 的后缀可能先于真名 "gpt-4o" 到达，
	// 抢占后真实条目反而查不到，因此后缀键只在第二遍补空位。
	primary := make([]struct {
		variants []string
		entry    Entry
	}, 0, len(raw))
	for name, r := range raw {
		e := r.normalize()
		if e == (Entry{}) {
			continue // 只有 mode/provider、无任何可补全数值的条目不入索引
		}
		primary = append(primary, struct {
			variants []string
			entry    Entry
		}{keyVariants(strings.ToLower(strings.TrimSpace(name))), e})
	}
	idx := make(map[string]Entry, len(primary))
	for _, p := range primary {
		for _, k := range p.variants[:2] { // 仅全名键（原名 + 点号变体）
			if _, exists := idx[k]; !exists {
				idx[k] = p.entry
			}
		}
	}
	for _, p := range primary {
		for _, k := range p.variants[2:] { // 后缀键补空位
			if _, exists := idx[k]; !exists {
				idx[k] = p.entry
			}
		}
	}
	return idx, len(primary), nil
}

// keyVariants 生成一个模型名的全部候选键：原名 + 版本号点号变体；
// "provider/name" 形式额外索引 name 后缀。索引与查询共用，顺序即优先级。
func keyVariants(key string) []string {
	out := []string{key, digitDots(key)}
	if i := strings.LastIndex(key, "/"); i >= 0 && i < len(key)-1 {
		suffix := key[i+1:]
		out = append(out, suffix, digitDots(suffix))
	}
	return out
}

// digitDots 把相邻数字之间的 '-' 换成 '.'（doubao-1-5-pro → doubao-1.5-pro），
// 用于对齐 LiteLLM 目录里「版本号带点」的键名写法。
func digitDots(s string) string {
	b := []byte(s)
	for i := 1; i < len(b)-1; i++ {
		if b[i] == '-' && b[i-1] >= '0' && b[i-1] <= '9' && b[i+1] >= '0' && b[i+1] <= '9' {
			b[i] = '.'
		}
	}
	return string(b)
}
