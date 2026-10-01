// 用量查询结果缓存（带单飞去重）。
//
// 动机：前端「用量分析」页的交互是「点 facet 行下钻 → 重新查询」。同一组参数
//（区间 + 粒度 + 维度 + 实体）在短时间内会被反复请求（页面抖动、多点几下、
// 多个管理标签页），每次都要走一遍聚合查询。这里加一层短 TTL 缓存。
//
// **单飞（single-flight）是关键**，不是可有可无的优化：TTL 到期瞬间若有 N 个并发
// 请求打同一个 key，没有单飞就会同时发起 N 次聚合查询——缓存反而放大了数据库
// 压力（典型的缓存击穿）。有单飞时只有第一个真正执行，其余等它的结果。
//
// 与 rollup 的关系：rollup 让单次查询变快，缓存让重复查询不用查。两者互补：
// 实时区间（水位未覆盖）只能回落原始表，这时缓存的价值更大。
package store

import (
	"sync"
	"time"
)

// UsageCacheTTL 缓存有效期。
//
// 取 30 秒：用量分析是「看趋势」的场景，30 秒的陈旧度对判断无影响；而前端两次
// 点击之间的间隔通常远小于此，命中率足够高。再长会让「刚发完请求刷新页面看不到」
// 变成常见困惑。
const UsageCacheTTL = 30 * time.Second

// usageCacheMaxEntries 条目上限，防止维度/实体组合无限增长撑爆内存。
// 超出时简单地整体清空（而非 LRU）：用量分析的 key 数量本就不大，清空代价可接受，
// 换来实现复杂度最小。
const usageCacheMaxEntries = 512

type usageCacheEntry struct {
	res     *UsageQueryResult
	expires time.Time
}

// usageFlight 一次进行中的加载，供并发请求共享结果。
type usageFlight struct {
	done chan struct{}
	res  *UsageQueryResult
	err  error
}

// UsageCache 用量查询结果缓存。零值不可用，请用 NewUsageCache。
type UsageCache struct {
	ttl time.Duration

	mu       sync.Mutex
	items    map[string]usageCacheEntry
	inflight map[string]*usageFlight
	// hits/misses 仅用于观测。
	hits, misses int64
}

// NewUsageCache 创建缓存；ttl <= 0 时用 UsageCacheTTL。
func NewUsageCache(ttl time.Duration) *UsageCache {
	if ttl <= 0 {
		ttl = UsageCacheTTL
	}
	return &UsageCache{
		ttl:      ttl,
		items:    make(map[string]usageCacheEntry),
		inflight: make(map[string]*usageFlight),
	}
}

// GetOrLoad 返回 key 对应的结果；未命中时调用 load 并缓存。
//
// 同一 key 的并发调用只会执行一次 load（单飞）；其余等待并共享同一个结果（含错误）。
// loaded 表示本次是否真正执行了 load（false = 命中缓存或共享了在途请求）。
// 只有成功的结果才进缓存：失败不能被缓存，否则一次瞬时故障会让后续 30 秒都失败。
func (c *UsageCache) GetOrLoad(key string, load func() (*UsageQueryResult, error)) (*UsageQueryResult, bool, error) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok && time.Now().Before(e.expires) {
		c.hits++
		c.mu.Unlock()
		return e.res, false, nil
	}
	// 已有在途请求：等它，不要再打一次数据库。
	// 共享在途结果也算命中——否则命中率会被严重低估（并发越高越失真）。
	if fl, ok := c.inflight[key]; ok {
		c.hits++
		c.mu.Unlock()
		<-fl.done
		return fl.res, false, fl.err
	}
	fl := &usageFlight{done: make(chan struct{})}
	c.inflight[key] = fl
	c.misses++
	c.mu.Unlock()

	// 在锁外执行加载：聚合查询可能耗时，持锁会阻塞所有其它 key 的查询。
	res, err := load()

	fl.res, fl.err = res, err
	c.mu.Lock()
	delete(c.inflight, key)
	if err == nil && res != nil {
		if len(c.items) >= usageCacheMaxEntries {
			c.items = make(map[string]usageCacheEntry, usageCacheMaxEntries)
		}
		c.items[key] = usageCacheEntry{res: res, expires: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
	close(fl.done)
	return res, true, err
}

// Invalidate 清空缓存（写操作后调用，避免看到过期数据）。
func (c *UsageCache) Invalidate() {
	c.mu.Lock()
	c.items = make(map[string]usageCacheEntry)
	c.mu.Unlock()
}

// Stats 返回 (命中, 未命中) 计数，供观测。
func (c *UsageCache) Stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
