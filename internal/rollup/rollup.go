// Package rollup 用量分析预聚合的后台推进器。
//
// 职责：周期调用 store.RunRollupOnce，把原始日志滚动聚合进小时/天表，让
// 「用量分析」页查历史区间时不必扫全表。
//
// 为什么单独一个包、而不是塞进 balancer：聚合是**只读原始表 + 写聚合表**的旁路
// 任务，与转发热路径完全无关。放在 balancer 里会让人误以为它影响选路；放在 main
// 里则便于独立启停与测试。
//
// 与写入的关系：SQLite 单写者。聚合走事务写聚合表，会与转发路径的统计落库争锁；
// 因此周期取较长（默认 1 分钟），且单轮工作量很小（只聚合最近 lookback 窗口内的
// 新桶）。历史回填请用管理端 rebuild 接口手动触发，不要靠调小周期。
package rollup

import (
	"context"
	"log"
	"time"

	"arkgate/internal/store"
)

// DefaultInterval 聚合周期。
//
// 为什么是 1 分钟：水位语义是「now - 5min lookback」，所以每轮只需重算最近约
// 6 分钟的小时桶（至多 6 个桶 × 6 个维度）。1 分钟一轮既能保证「历史区间很快
// 可用 rollup」，又不会给 SQLite 单写者带来持续压力。
const DefaultInterval = time.Minute

// Runner 周期性推进预聚合。
type Runner struct {
	store    *store.Store
	interval time.Duration
	// OnError 观测回调（nil = 只打日志）。生产由 main 传 nil，测试用它断言。
	OnError func(err error)
	// OnRound 每轮完成后的回调（行数、水位），用于测试同步等待。
	OnRound func(rows, watermark int64)

	cancel context.CancelFunc
	done   chan struct{}
}

// New 创建推进器。interval <= 0 时用 DefaultInterval。
func New(st *store.Store, interval time.Duration) *Runner {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Runner{store: st, interval: interval, done: make(chan struct{})}
}

// Start 启动后台推进（非阻塞）。可重复调用，只有第一次生效。
func (r *Runner) Start() {
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.loop(ctx)
}

// Stop 停止推进并等待当前轮结束。幂等。
func (r *Runner) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel = nil
}

func (r *Runner) loop(ctx context.Context) {
	defer close(r.done)
	// 启动后先跑一轮：否则重启后要等一个周期才有新数据入聚合表，
	// 「刚重启 → 打开用量分析页」会退化到全表扫。
	r.runOnce()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runOnce()
		}
	}
}

func (r *Runner) runOnce() {
	now := time.Now().Unix()
	rows, wm, err := r.store.RunRollupOnce(now)
	if err != nil {
		// 聚合失败不致命：查询会自动回落原始表，功能不受影响，只是慢一点。
		// 因此这里只记录、不退出——退出会让「统计延迟」永久停在失败点。
		log.Printf("用量预聚合失败（查询将回落原始表）: %v", err)
		if r.OnError != nil {
			r.OnError(err)
		}
		return
	}
	if r.OnRound != nil {
		r.OnRound(rows, wm)
	}
}
