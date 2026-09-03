// Package main 是 ArkGate 的入口：火山方舟多账号负载均衡 & OpenAI 兼容网关。
//
// 对外同时提供：
//   - /v1/*    OpenAI 兼容推理接口（chat/completions、models）
//   - /api/*   管理接口（账号、模型映射、子 Key、日志、统计）
//   - /        内嵌 Web 管理界面
//
// 并发：Go net/http 每个请求一个 goroutine；转发链路无全局锁；
// 用量统计/日志写库经 balancer 的有界 channel 异步进行。
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"

	"arkgate/internal/admin"
	"arkgate/internal/balancer"
	"arkgate/internal/config"
	"arkgate/internal/gateway"
	"arkgate/internal/portal"
	"arkgate/internal/secure"
	"arkgate/internal/store"
)

//go:embed all:web
var webFS embed.FS

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	defer st.Close()

	box, err := secure.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化加密失败: %v", err)
	}

	bal := balancer.New(st, cfg.SessionTTL)
	defer bal.Close()

	gw := gateway.New(cfg, st, box, bal)
	adm := admin.New(st, box, bal, cfg)
	pt := portal.New(st, bal)

	// 首次运行自动生成管理令牌。
	if tok, created := adm.EnsureAdminToken(); created {
		log.Printf("=============================================================")
		log.Printf("  检测到首次运行，已生成管理访问令牌：")
		log.Printf("  %s", tok)
		log.Printf("  请妥善保存，登录 Web 管理界面时使用。")
		log.Printf("=============================================================")
	}

	mux := http.NewServeMux()
	// 门户挂载在 /api/portal/ 下；admin 的 /api/ 前缀更宽，但 ServeMux 按最长前缀
	// 匹配，portal 精确接管自己的子树，其余仍归 admin。
	mux.Handle("/api/portal/", pt.Handler())
	mux.Handle("/api/", adm.Handler())
	mux.Handle("/v1/", gw.Handler())
	mux.Handle("/", spaHandler())

	// 端口占用时自动向后寻找可用端口，最多尝试 100 个。
	ln, addr := listenWithFallback(cfg.ListenAddr, 100)
	// 用监听器实际地址打印（含 ARKGATE_ADDR=:0 这类随机端口场景）。
	log.Printf("ArkGate 启动: http://%s", ln.Addr().String())
	log.Printf("  - OpenAI 兼容端点: /v1/chat/completions /v1/responses /v1/images/generations /v1/models")
	log.Printf("  - 管理界面:       /")
	log.Printf("  - 数据目录:       %s", cfg.DataDir)

	srv := &http.Server{
		Addr:    addr,
		Handler: cors(mux),
	}
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}

// listenWithFallback 从 addr 开始尝试监听；端口被占用时自动向后递增，
// 直到找到一个可用端口（最多尝试 maxAttempts 次）。返回监听器与最终地址。
func listenWithFallback(addr string, maxAttempts int) (net.Listener, string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatalf("监听地址格式错误 %q: %v", addr, err)
	}
	base, err := net.LookupPort("tcp", port)
	if err != nil {
		log.Fatalf("端口无效 %q: %v", port, err)
	}

	for i := 0; i < maxAttempts; i++ {
		candidate := net.JoinHostPort(host, fmt.Sprintf("%d", base+i))
		ln, lerr := net.Listen("tcp", candidate)
		if lerr == nil {
			if i > 0 {
				log.Printf("端口 %s 已被占用，已自动切换到 %d", port, base+i)
			}
			return ln, candidate
		}
		if !isAddrInUse(lerr) {
			// 非「端口占用」类错误（如权限不足、地址非法），直接失败。
			log.Fatalf("监听 %s 失败: %v", candidate, lerr)
		}
	}
	log.Fatalf("在 %s 起连续 %d 个端口均被占用，无法启动", addr, maxAttempts)
	return nil, ""
}

// isAddrInUse 判断错误是否由「地址已被占用」导致。
func isAddrInUse(err error) bool {
	opErr, ok := err.(*net.OpError)
	if !ok {
		return strings.Contains(err.Error(), "address already in use")
	}
	if sysErr, ok := opErr.Err.(*os.SyscallError); ok {
		return sysErr.Err == syscall.EADDRINUSE
	}
	return strings.Contains(err.Error(), "address already in use")
}

// spaHandler 提供内嵌 Web 管理界面（单页应用）。
func spaHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Printf("内嵌前端不可用: %v", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "frontend not embedded", http.StatusNotFound)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if data, err := fs.ReadFile(sub, p); err == nil {
			ct := contentType(p)
			w.Header().Set("Content-Type", ct)
			// 前端内嵌进二进制、随版本整体更新：禁止缓存，避免升级后浏览器
			// 还拿旧 app.js/style.css 造成「改了没生效」。
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(data)
			return
		}
		// SPA fallback。
		data, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// cors 允许前端跨域；允许自定义请求头（X-Auth-Token 等）。
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Auth-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
