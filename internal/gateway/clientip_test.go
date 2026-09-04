package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientIP 锁定来源 IP 提取顺序：XFF 首跳 > X-Real-IP > RemoteAddr（去端口）。
// 该值仅用于日志展示，因此解析要稳（脏输入也别 panic 或返回半截内容）。
func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{"remote-addr", "203.0.113.9:53124", nil, "203.0.113.9"},
		{"ipv6-remote", "[2001:db8::1]:443", nil, "2001:db8::1"},
		{"no-port", "203.0.113.9", nil, "203.0.113.9"},
		{"xff-single", "10.0.0.1:8080", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
		{"xff-chain", "10.0.0.1:8080",
			map[string]string{"X-Forwarded-For": "198.51.100.7, 10.0.0.2, 10.0.0.3"}, "198.51.100.7"},
		{"xff-spaces", "10.0.0.1:8080", map[string]string{"X-Forwarded-For": "  198.51.100.7  "}, "198.51.100.7"},
		{"real-ip", "10.0.0.1:8080", map[string]string{"X-Real-IP": "198.51.100.8"}, "198.51.100.8"},
		{"xff-wins", "10.0.0.1:8080",
			map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Real-IP": "198.51.100.8"}, "198.51.100.7"},
		// 空/纯空白的 XFF 不能吃掉后续来源，必须继续回落。
		{"blank-xff-falls-back", "10.0.0.1:8080",
			map[string]string{"X-Forwarded-For": "   ", "X-Real-IP": "198.51.100.8"}, "198.51.100.8"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = c.remoteAddr
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := clientIP(r); got != c.want {
			t.Fatalf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}
}
