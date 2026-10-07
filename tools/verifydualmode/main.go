package main

// 用法加权/原始双口径的端到端验证：
//   假上游（带缓存命中的 usage） → 真实 arkgate 二进制 → 管理 API 配置
//   → /v1/chat/completions → 检查子 Key 列表 / 门户 / 用量分析三处数字。
//
// 使用：
//   go build -o arkgate.exe .        # 或 arkgate（非 Windows）
//   go run ./tools/verifydualmode [二进制路径]
//
// 断言的核心：同一份用量在各界面上分别是
//   加权额度 380（prompt 1000 含缓存读 800、output 100、读倍率 0.1x）
//   上游原始 1100
// 且用量分析页的四项 token 全部是原始值（不因倍率而变）。
//
// 为什么值得单独写一个验证器而不写在单测里：单测能锁定每个函数，但锁不住
// 「三个界面看的是同一个数据源」——加权与原始的错位恰恰只会在全链路上暴露
// （曾经出现过一个界面显示原始量、另一个显示加权值导致用户以为超限）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	upstreamAddr = "127.0.0.1:18998"
	gateAddr     = "127.0.0.1:18997"
)

var failures int

func check(cond bool, format string, args ...any) {
	if cond {
		fmt.Printf("  ok   %s\n", fmt.Sprintf(format, args...))
		return
	}
	failures++
	fmt.Printf("  FAIL %s\n", fmt.Sprintf(format, args...))
}

func main() {
	dir, _ := os.MkdirTemp("", "arkverify")
	defer os.RemoveAll(dir)
	go fakeUpstream()
	time.Sleep(300 * time.Millisecond)

	bin := os.Getenv("ARKGATE_BIN")
	if bin == "" {
		bin = "./arkgate.exe"
		if _, err := os.Stat(bin); err != nil {
			bin = "./arkgate" // 非 Windows
		}
	}
	if _, err := os.Stat(bin); err != nil {
		fmt.Printf("未找到二进制 %s，请先 go build（或用 ARKGATE_BIN 指定）\n", bin)
		os.Exit(1)
	}
	bin, _ = filepath.Abs(bin)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"ARKGATE_ADDR="+gateAddr,
		"ARKGATE_DATA_DIR="+dir,
		"ARKGATE_SESSION_TTL=0")
	logf, _ := os.Create(filepath.Join(dir, "log.txt"))
	cmd.Stderr = logf
	cmd.Stdout = logf
	if err := cmd.Start(); err != nil {
		fmt.Println("启动失败:", err)
		os.Exit(1)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// 等端口就绪并取管理令牌。
	token := ""
	for i := 0; i < 60; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "log.txt")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				// 日志行带时间戳前缀，从 "ark-" 起截取。
				if i := strings.Index(line, "ark-"); i >= 0 {
					cand := strings.TrimSpace(line[i:])
					// 只认「整行只含令牌」的行（行内可能有说明文字），长度足够即可。
					if len(cand) >= 8 && !strings.ContainsAny(cand, " \t") {
						token = cand
					}
				}
			}
		}
		if token != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if token == "" {
		b, _ := os.ReadFile(filepath.Join(dir, "log.txt"))
		fmt.Println("未取到管理令牌，日志如下:\n" + string(b))
		os.Exit(1)
	}
	time.Sleep(500 * time.Millisecond)

	admin := func(method, path string, body any) map[string]any {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "http://"+gateAddr+path, rdr)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println("管理请求失败:", path, err)
			os.Exit(1)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		if res.StatusCode >= 400 {
			fmt.Printf("管理请求 %s %s → %d %s\n", method, path, res.StatusCode, string(raw))
		}
		return out
	}
	adminList := func(path string) []map[string]any {
		req, _ := http.NewRequest("GET", "http://"+gateAddr+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var out []map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}

	fmt.Println("── 配置 ──")
	admin("POST", "/api/accounts", map[string]any{
		"id": "acc_local", "name": "local", "api_key": "up-key", "provider": "custom",
		"base_url": "http://" + upstreamAddr + "/v1"})
	admin("POST", "/api/models", map[string]any{"name": "m", "type": "text"})
	admin("POST", "/api/endpoints", map[string]any{
		"account_id": "acc_local", "model": "m", "ep": "m"})
	skOut := admin("POST", "/api/subkeys", map[string]any{
		"name": "v", "key": "sk-verify", "daily_limit_tokens": 100000,
		"cache_read_permille": 100})
	_ = skOut

	fmt.Println("── 发一次请求（上游回报 prompt 1000 其中缓存读 800、output 100）──")
	req, _ := http.NewRequest("POST", "http://"+gateAddr+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-verify")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("转发失败:", err)
		os.Exit(1)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	check(res.StatusCode == 200 && strings.Contains(string(body), "hi"),
		"转发成功 (%d) %.80s", res.StatusCode, string(body))

	// 统计异步落库，轮询等待。
	time.Sleep(1200 * time.Millisecond)

	fmt.Println("── 子 Key 列表：加权额度与同周期原始量并列 ──")
	var quota map[string]any
	for i := 0; i < 20; i++ {
		for _, s := range adminList("/api/subkeys") {
			if s["id"] == "sk_verify" || s["key"] == "sk-verify" {
				quota, _ = s["quota"].(map[string]any)
			}
		}
		if quota != nil && num(quota["raw_tokens"]) > 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if quota == nil {
		check(false, "子 Key 列表未返回 quota")
	} else {
		check(num(quota["used_tokens"]) == 380,
			"本周期加权额度 = 380 (got %v)", quota["used_tokens"])
		check(num(quota["raw_tokens"]) == 1100,
			"本周期上游原始 = 1100 (got %v)", quota["raw_tokens"])
		check(quota["weighted"] == true, "标记为已启用加权")
	}

	fmt.Println("── 门户：加权额度与同周期原始量并列 ──")
	pReq, _ := http.NewRequest("GET", "http://"+gateAddr+"/api/portal/overview", nil)
	pReq.Header.Set("Authorization", "Bearer sk-verify")
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		fmt.Println("门户请求失败:", err)
		os.Exit(1)
	}
	pRaw, _ := io.ReadAll(pRes.Body)
	pRes.Body.Close()
	var portal map[string]any
	_ = json.Unmarshal(pRaw, &portal)
	today, _ := portal["today"].(map[string]any)
	if today == nil {
		check(false, "门户未返回 today: %.200s", string(pRaw))
	} else {
		check(num(today["tokens"]) == 380,
			"门户本周期加权 = 380 (got %v)", today["tokens"])
		check(num(portal["today_raw_tokens"]) == 1100,
			"门户本周期原始 = 1100 (got %v)", portal["today_raw_tokens"])
	}
	// 门户日志仍不得下发错误文本（越权红线）。
	check(!strings.Contains(string(pRaw), "\"error\":"), "门户响应不含 error 字段")

	fmt.Println("── 用量分析：四项 token 全为上游原始值 ──")
	from := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	to := time.Now().Format("2006-01-02")
	q := fmt.Sprintf("/api/usage/stats?from=%s&to=%s&granularity=hour&dim=", from, to)
	st := admin("GET", q, nil)
	sum, _ := st["summary"].(map[string]any)
	if sum == nil {
		check(false, "用量分析未返回 summary")
	} else {
		check(num(sum["total_tokens"]) == 1100, "总 Token = 1100 原始 (got %v)", sum["total_tokens"])
		check(num(sum["prompt_tokens"]) == 1000, "输入 Token = 1000 含缓存 (got %v)", sum["prompt_tokens"])
		check(num(sum["completion_tokens"]) == 100, "输出 Token = 100 (got %v)", sum["completion_tokens"])
		check(num(sum["cache_read_tokens"]) == 800, "缓存命中 Token = 800 (got %v)", sum["cache_read_tokens"])
		// 三个口径互不污染：额度(380) 与原始(1100) 必须不同。
		check(num(sum["total_tokens"]) != num(quota["used_tokens"]),
			"用量分析原始量(%v) 不等于加权额度(%v)", sum["total_tokens"], quota["used_tokens"])
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("失败 %d 项\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过")
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// fakeUpstream 最小 OpenAI 兼容上游，用于端点探测与 chat 转发。
// usage 固定回报 prompt 1000（其中缓存命中 800）+ completion 100。
func fakeUpstream() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m","object":"model"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,
			"prompt_tokens_details":{"cached_tokens":800}}}`))
	})
	_ = http.ListenAndServe(upstreamAddr, mux)
}
