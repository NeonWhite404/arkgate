package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errTest 供缓存测试注入失败。
var errTest = errors.New("mock load failure")

// TestUsageCacheSingleFlight 锁定缓存**单飞**：TTL 到期瞬间的 N 个并发请求
// 只能触发一次 load。
//
// 为什么这是核心不变量：没有单飞时，N 个并发请求会同时发起 N 次聚合查询——
// 缓存不但没减轻数据库压力，反而在击穿瞬间把它放大了 N 倍。前端「快速切换
// facet / 多个管理标签页」正好会造出这种并发。
func TestUsageCacheSingleFlight(t *testing.T) {
	c := NewUsageCache(time.Minute)
	var loads int32

	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时冲进去
			_, _, err := c.GetOrLoad("k", func() (*UsageQueryResult, error) {
				atomic.AddInt32(&loads, 1)
				time.Sleep(30 * time.Millisecond) // 模拟聚合查询耗时
				return &UsageQueryResult{Source: "raw"}, nil
			})
			if err != nil {
				t.Errorf("load: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("单飞失效：%d 个并发请求触发了 %d 次 load（期望 1）", n, got)
	}
	hits, misses := c.Stats()
	if misses != 1 {
		t.Fatalf("misses = %d, want 1", misses)
	}
	if hits != n-1 {
		t.Fatalf("hits = %d, want %d", hits, n-1)
	}
}

// TestUsageCacheHitAndExpiry 命中不重复 load；TTL 过期后重新 load。
func TestUsageCacheHitAndExpiry(t *testing.T) {
	c := NewUsageCache(40 * time.Millisecond)
	var loads int32
	load := func() (*UsageQueryResult, error) {
		atomic.AddInt32(&loads, 1)
		return &UsageQueryResult{Source: "raw"}, nil
	}

	if _, loaded, _ := c.GetOrLoad("k", load); !loaded {
		t.Fatal("首次应执行 load")
	}
	if _, loaded, _ := c.GetOrLoad("k", load); loaded {
		t.Fatal("TTL 内应命中缓存")
	}
	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}

	time.Sleep(60 * time.Millisecond)
	if _, loaded, _ := c.GetOrLoad("k", load); !loaded {
		t.Fatal("TTL 过期后应重新 load")
	}
	if got := atomic.LoadInt32(&loads); got != 2 {
		t.Fatalf("loads = %d, want 2", got)
	}
}

// TestUsageCacheErrorNotCached 失败的加载不得进缓存：
// 否则一次瞬时故障会让后续整个 TTL 内都返回失败（错误被「记住」了）。
func TestUsageCacheErrorNotCached(t *testing.T) {
	c := NewUsageCache(time.Minute)
	var loads int32
	load := func() (*UsageQueryResult, error) {
		if atomic.AddInt32(&loads, 1) == 1 {
			return nil, errTest
		}
		return &UsageQueryResult{Source: "raw"}, nil
	}

	if _, _, err := c.GetOrLoad("k", load); err == nil {
		t.Fatal("首次应返回错误")
	}
	// 第二次必须重新执行 load 并成功。
	res, loaded, err := c.GetOrLoad("k", load)
	if err != nil || res == nil {
		t.Fatalf("失败不应被缓存：err=%v res=%v", err, res)
	}
	if !loaded {
		t.Fatal("第二次应重新 load（首次的失败未被缓存）")
	}
}

// TestUsageCacheErrorSharedWithWaiters 单飞等待者应拿到同一个错误，
// 而不是各自的 nil 结果（否则并发下会出现「部分成功部分空」的诡异现象）。
func TestUsageCacheErrorSharedWithWaiters(t *testing.T) {
	c := NewUsageCache(time.Minute)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := c.GetOrLoad("k", func() (*UsageQueryResult, error) {
				time.Sleep(20 * time.Millisecond)
				return nil, errTest
			})
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("goroutine %d 未拿到错误（单飞共享语义不一致）", i)
		}
	}
}

// TestUsageCacheInvalidate 写操作后清缓存。
func TestUsageCacheInvalidate(t *testing.T) {
	c := NewUsageCache(time.Minute)
	var loads int32
	load := func() (*UsageQueryResult, error) {
		atomic.AddInt32(&loads, 1)
		return &UsageQueryResult{Source: "raw"}, nil
	}
	c.GetOrLoad("k", load)
	c.Invalidate()
	if _, loaded, _ := c.GetOrLoad("k", load); !loaded {
		t.Fatal("Invalidate 后应重新 load")
	}
	if got := atomic.LoadInt32(&loads); got != 2 {
		t.Fatalf("loads = %d, want 2", got)
	}
}

// TestUsageCacheEviction 条目上限：超限后不 panic、仍能正常工作。
func TestUsageCacheEviction(t *testing.T) {
	c := NewUsageCache(time.Minute)
	load := func() (*UsageQueryResult, error) {
		return &UsageQueryResult{Source: "raw"}, nil
	}
	for i := 0; i < usageCacheMaxEntries+50; i++ {
		key := string(rune('a'+i%26)) + string(rune('0'+i%10)) + time.Duration(i).String()
		if _, _, err := c.GetOrLoad(key, load); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
	}
	// 清空后仍可用
	if _, _, err := c.GetOrLoad("final", load); err != nil {
		t.Fatalf("超限后应仍可用: %v", err)
	}
}
