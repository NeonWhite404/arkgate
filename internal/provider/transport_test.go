package provider

import (
	"net/http"
	"testing"
)

// TestValidateRequestHeaders 锁定映射级请求头校验边界：认证/传输级/协议头不可
// 覆盖，名称必须是合法 token，值不得含换行/NUL，且大小写冲突要被拒绝。
func TestValidateRequestHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantErr bool
	}{
		{"正常自定义头", map[string]string{"User-Agent": "Hermes/1.0", "x-app": "cli"}, false},
		{"空值合法", map[string]string{"X-Empty": ""}, false},
		{"覆盖认证头", map[string]string{"Authorization": "Bearer x"}, true},
		{"覆盖 x-api-key", map[string]string{"X-Api-Key": "k"}, true},
		{"覆盖协议头", map[string]string{"Content-Type": "text/plain"}, true},
		{"覆盖 anthropic-version", map[string]string{"anthropic-version": "2023-06-01"}, true},
		{"值含换行", map[string]string{"X-Test": "a\r\nb"}, true},
		{"值为 NUL", map[string]string{"X-Test": "a\x00b"}, true},
		{"名含空格", map[string]string{"X Bad": "v"}, true},
		{"名含冒号", map[string]string{"X:Bad": "v"}, true},
		{"大小写冲突", map[string]string{"User-Agent": "A", "user-agent": "B"}, true},
	}
	for _, c := range cases {
		err := ValidateRequestHeaders(c.headers)
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// TestApplyRequestHeaders 锁定实际发送时的行为：自定义头规范化写入；禁止覆盖
// 的头被跳过（第二道防线不依赖 admin 先校验）。
func TestApplyRequestHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer keep")
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	applyRequestHeaders(h, map[string]string{
		"user-agent":        "Hermes/1.0",
		"x-app":             "cli",
		"authorization":     "Bearer evil",
		"content-type":      "text/plain",
		"anthropic-version": "should-not-override",
	})
	if got := h.Get("User-Agent"); got != "Hermes/1.0" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := h.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer keep" {
		t.Fatalf("Authorization 被覆盖: %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type 被覆盖: %q", got)
	}
	if got := h.Get("Anthropic-Version"); got != "" {
		t.Fatalf("Anthropic-Version 应被忽略，实际: %q", got)
	}
}
