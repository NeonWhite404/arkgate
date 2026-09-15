// Package upstreamcheck 实现「上游模型存在性检查器」：定期用账号凭据探测
// 各上游的模型列表（GET {base}/models），与本地接入点映射（Endpoint.EP）比对，
// 把「上游已消失」的映射标记为 UpstreamDeleted，重新出现的自动恢复。
//
// 设计边界：
//   - 仅做观察与持久化标记：不删映射、不自动停用、不触碰熔断/限流/路由；
//     该标记只供管理界面提示，网关选路完全不受影响。
//   - 只有 ListModels 成功且解析完成时才写状态。超时、鉴权错误、限流、非 2xx、
//     网络错误、解析失败一律只记录错误并保留原状态——宁可少更新，不可误伤。
//   - 比较口径：上游返回的模型 ID 与本地 Endpoint.EP 精确比较（不透明字符串，
//     不归一化、不推断）；账号之间相互隔离，同名 EP 在其它账号下不受影响。
//   - 生命周期：启动后立即执行一轮，随后按固定周期执行；单工作协程 + 每轮
//     阻塞上游请求，天然不会重叠。Stop 取消后 ticker 停止、在途请求因
//     context 取消及时退出，再继续服务关闭流程。
package upstreamcheck

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"arkgate/internal/model"
	"arkgate/internal/provider"
)

// 默认参数（生产配置显式可见；测试可注入很短的周期与超时）。
const (
	DefaultInterval = 10 * time.Minute
	DefaultTimeout  = 30 * time.Second
)

// AccountStore 检查器所需的存储面（依赖收敛到最小，便于 fake 测试）。
type AccountStore interface {
	ListAccounts() ([]*model.Account, error)
	SyncEndpointUpstreamPresence(accountID string, presentEPs []string) (int64, error)
}

// RouteResolver 把账号解析成可发送的上游路由（供应商定义 + base URL + 解密 Key）。
type RouteResolver func(acc *model.Account) (provider.Route, error)

// ModelLister 拉取上游模型列表（provider.Manager.ListModels 满足该签名）。
type ModelLister func(ctx context.Context, rt provider.Route, timeout time.Duration) ([]provider.UpstreamModel, error)

// Checker 上游模型存在性检查器。
type Checker struct {
	store    AccountStore
	resolve  RouteResolver
	list     ModelLister
	interval time.Duration
	timeout  time.Duration

	stopOnce sync.Once
	ctx      context.Context    // 生命周期根：Stop 时取消，在途探测随之退出
	cancel   context.CancelFunc // 与 ctx 配套
	done     chan struct{}      // Stop 后关闭，Run 的主循环据此退出
	wg       sync.WaitGroup
}

// New 构造检查器。interval 为周期（<=0 回落到默认值），timeout 为单次探测超时
// （<=0 回落到默认值）。调用 Run 后即开始工作，无需再手动启动。
func New(st AccountStore, resolve RouteResolver, list ModelLister, interval, timeout time.Duration) *Checker {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Checker{
		store:    st,
		resolve:  resolve,
		list:     list,
		interval: interval,
		timeout:  timeout,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
}

// Run 启动检查循环：立即执行一轮，此后每 interval 执行一轮。单工作协程
// 顺序执行，同一实例不会重叠；在途探测阻塞在下一轮开始之前（ticker 在
// 本轮结束后才重新触发）。done 关闭后尽快退出。
func (c *Checker) Run() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			c.runOnce()
			select {
			case <-c.done:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop 停止检查器：取消生命周期根（正在执行的上游请求因 ctx 取消及时退出，
// 本轮不再写任何状态），ticker 不再触发。阻塞等待工作协程退出，保证调用方
// 关闭存储前无残留访问。
func (c *Checker) Stop() {
	c.stopOnce.Do(func() {
		c.cancel()
		close(c.done)
	})
	c.wg.Wait()
}

// runOnce 执行一轮检查（导出以便单测驱动单轮；Run 也走同一路径）。
func (c *Checker) runOnce() {
	ctx, cancel := context.WithTimeout(c.ctx, c.timeout)
	defer cancel()

	accounts, err := c.store.ListAccounts()
	if err != nil {
		log.Printf("upstreamcheck: 读取账号失败: %v", err)
		return
	}
	for _, acc := range accounts {
		if acc.Status != model.AccountActive {
			continue
		}
		rt, err := c.resolve(acc)
		if err != nil {
			log.Printf("upstreamcheck: 账号 %s(%s) 无法构造上游路由: %v", acc.Name, acc.ID, err)
			continue
		}
		models, err := c.list(ctx, rt, c.timeout)
		if err != nil {
			if he, ok := provider.AsHTTPError(err); ok {
				log.Printf("upstreamcheck: 账号 %s(%s) 拉取模型列表失败（上游返回 %d）: %s",
					acc.Name, acc.ID, he.Code, truncateForLog(string(he.Body)))
			} else {
				log.Printf("upstreamcheck: 账号 %s(%s) 拉取模型列表失败: %v", acc.Name, acc.ID, err)
			}
			// 失败不清状态：保留上一轮的观察结果，等下一轮成功探测再刷新。
			continue
		}
		eps := make([]string, 0, len(models))
		for _, m := range models {
			eps = append(eps, m.ID)
		}
		sort.Strings(eps) // 仅稳定日志与测试断言，不影响状态语义
		restored, err := c.store.SyncEndpointUpstreamPresence(acc.ID, eps)
		if err != nil {
			log.Printf("upstreamcheck: 账号 %s(%s) 同步接入点状态失败: %v", acc.Name, acc.ID, err)
			continue
		}
		if restored > 0 {
			log.Printf("upstreamcheck: 账号 %s(%s) 本轮恢复 %d 个接入点", acc.Name, acc.ID, restored)
		}
	}
}

// truncateForLog 截断上游错误体摘要，避免把长页面整段写进日志。
func truncateForLog(s string) string {
	const maxLog = 300
	if len(s) > maxLog {
		return s[:maxLog] + "…"
	}
	return s
}
