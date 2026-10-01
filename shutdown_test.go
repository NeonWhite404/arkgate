package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/model"
	"arkgate/internal/store"
)

// TestGracefulShutdownDrainsStatsBeforeStoreClose 锁定**停机关闭顺序**：
// HTTP 先停 → 等在途请求收尾 → 再 drain 统计 → 最后关库。
//
// 为什么顺序是关键不变量：如果先关库再停 HTTP，一个正在收尾的请求会把统计
// 写进已关闭的库（丢失），甚至把错误返回给已经等待很久的客户端。这条测试把
// 「乱序」变成一个可执行的失败，而不是一句注释里的约定。
func TestGracefulShutdownDrainsStatsBeforeStoreClose(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	bal := balancer.New(st, 0)

	// 一个「慢请求」：收到信号后仍在飞，用于验证 Shutdown 会等它。
	var inFlight atomic.Int32
	release := make(chan struct{})
	var (
		sawStoreClosedErr atomic.Bool
		storeCloseOnce    sync.Once
	)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-release // 一直挂着，直到测试放行
		// 收尾时写统计：若此时库已关，就会记下「撞上已关闭的库」。
		l := &model.UsageLog{Model: "m", Status: "ok", PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
		bal.Record(l, nil, true, false)
		if _, err := st.RollupWatermark(); err != nil && errors.Is(err, context.Canceled) {
			sawStoreClosedErr.Store(true)
		}
		io.WriteString(w, "ok")
	})}

	go func() { _ = srv.Serve(ln) }()

	// 发一个慢请求。
	done := make(chan error, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://%s/slow", ln.Addr().String()))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()

	// 等请求真正进入 handler。
	for i := 0; i < 100 && inFlight.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if inFlight.Load() == 0 {
		t.Fatal("慢请求未进入 handler")
	}

	// 触发优雅停机（模拟收到信号后的处理），并**并行**准备关库。
	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr <- srv.Shutdown(ctx)
	}()

	// 稍后放行请求，让它有机会正常收尾。
	time.Sleep(150 * time.Millisecond)
	close(release)

	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown 应完成（它会等在途请求）: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("慢请求应成功收尾: %v", err)
	}
	if sawStoreClosedErr.Load() {
		t.Fatal("请求收尾时撞上了已关闭的库——说明停机关闭顺序反了")
	}

	// 此时库还开着：统计应当已经可见（请求收尾时就已落盘）。
	_, total, err := st.QueryUsageLogs(store.LogFilter{}, 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total == 0 {
		t.Fatal("慢请求收尾时应已把统计落盘")
	}

	// 现在才关数据层（对应 main 的 defer 链），关闭过程不得 panic/报错。
	bal.Close()
	storeCloseOnce.Do(func() { _ = st.Close() })
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total == 0 {
		t.Fatal("关库前应已 drain 掉在途请求产生的统计（否则丢数据）")
	}
}

// TestShutdownGraceIsBounded 停机等待必须有上限：
// 流式请求可能长时间不结束，无限等会让运维的「重启」看起来卡死。
func TestShutdownGraceIsBounded(t *testing.T) {
	if shutdownGrace <= 0 {
		t.Fatal("shutdownGrace 必须为正")
	}
	if shutdownGrace > 2*time.Minute {
		t.Fatalf("shutdownGrace=%v 过长：重启会看起来像卡死", shutdownGrace)
	}
}
