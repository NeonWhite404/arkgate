package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteAuthErr 配额超限回 429、其余回 401。
//
// 这个测试的由来：实现时用批量文本替换把「authSubKey 的 401 分支」换成
// writeAuthErr(w, err)，但替换**同时命中了 writeAuthErr 函数体内部的
// 那一行**，于是它变成自我递归——**每个无效 Key 请求都会把进程打成
// stack overflow 崩溃**（实测：发一个错 Key 服务就没了）。
//
// 教训：批量替换后必须检查目标函数**自身**有没有被改到。
// 本测试用「能正常返回」这个最朴素的性质把这类无限递归钉死：
// 若 writeAuthErr 再次自我递归，测试会因栈溢出而失败（而不是静默通过）。
func TestWriteAuthErr(t *testing.T) {
	// ① 配额超限 → 429 + quota_exceeded，且带 Retry-After。
	rec := httptest.NewRecorder()
	writeAuthErr(rec, &quotaError{"该 API Key 已达到当前周期请求数上限"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("配额超限应回 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 应带 Retry-After 头（告诉客户端退避多久）")
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体: %v", err)
	}
	// code 必须机器可读：客户端不该去匹配中文文案。
	if body.Error.Code != "quota_exceeded" {
		t.Errorf("code 应为 quota_exceeded, got %q", body.Error.Code)
	}
	if body.Error.Type != "rate_limit_error" {
		t.Errorf("type 应为 rate_limit_error, got %q", body.Error.Type)
	}
	if body.Error.Message == "" {
		t.Error("message 不应为空（终端用户需要看懂）")
	}

	// ② 鉴权类错误 → 401，且不得带 quota_exceeded。
	rec = httptest.NewRecorder()
	writeAuthErr(rec, errors.New("无效的 API Key"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("鉴权失败应回 401, got %d", rec.Code)
	}
	// 必须**重新解析**：复用上面的 body 变量会残留第一次响应，
	// 让这条断言恒真（初版就是这么写的，误报了一次失败）。
	body = struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code == "quota_exceeded" {
		t.Error("鉴权失败不应被标成配额超限（会让客户端误以为是限流）")
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Error("401 不应带 Retry-After（那是限流语义）")
	}
}

// TestQuotaErrorIs 锁定 errors.Is 语义——调用点靠它分流 429/401。
func TestQuotaErrorIs(t *testing.T) {
	err := error(&quotaError{"已达上限"})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatal("quotaError 必须能被 errors.Is 识别为 ErrQuotaExceeded")
	}
	if errors.Is(errors.New("无效的 API Key"), ErrQuotaExceeded) {
		t.Fatal("普通错误不应被误判为配额超限")
	}
	// 文案要原样透出（用户看的是这句话，不是 code）。
	if err.Error() != "已达上限" {
		t.Errorf("Error() 应返回原始文案, got %q", err.Error())
	}
}
