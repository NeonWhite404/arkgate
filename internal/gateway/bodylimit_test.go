package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReadBodyRejectsOversized 请求体超限必须返回 413 且不把整个体读进内存。
//
// 原先 5 处入口都是裸 io.ReadAll(r.Body)：单个客户端发一个超大请求体就能把网关
// 内存吃光，而网关是集群单点。这条测试锁定「上限存在且状态码语义正确」——
// 413 而非 400：请求体过大是重试无用的错误，不该诱导客户端重试。
func TestReadBodyRejectsOversized(t *testing.T) {
	big := strings.Repeat("x", maxRequestBody+1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()

	_, err := readBody(rec, req)
	if err == nil {
		t.Fatal("超限请求体应报错")
	}
	if !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("应识别为超限错误, got %v", err)
	}
}

// TestReadBodyOrFailWrites413 超限时写 413 并返回 ok=false。
func TestReadBodyOrFailWrites413(t *testing.T) {
	big := strings.Repeat("y", maxRequestBody+1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(big))
	rec := httptest.NewRecorder()

	body, ok := readBodyOrFail(rec, req)
	if ok || body != nil {
		t.Fatal("超限时应返回 ok=false 且不带 body")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("应为 413, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "请求体过大") {
		t.Fatalf("错误信息应说明原因, got %s", rec.Body.String())
	}
}

// TestReadBodyAllowsNormalSize 正常大小的请求体不受影响（不能因加限长而误伤）。
func TestReadBodyAllowsNormalSize(t *testing.T) {
	const payload = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	body, ok := readBodyOrFail(rec, req)
	if !ok {
		t.Fatalf("正常请求不应失败: %d %s", rec.Code, rec.Body.String())
	}
	if string(body) != payload {
		t.Fatalf("请求体应原样返回（字节透传是既有约束）, got %q", body)
	}
}

// TestReadBodyExactLimitAllowed 恰好等于上限应通过（边界不能偏一位）。
func TestReadBodyExactLimitAllowed(t *testing.T) {
	exact := strings.Repeat("z", maxRequestBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(exact))
	rec := httptest.NewRecorder()

	body, err := readBody(rec, req)
	if err != nil {
		t.Fatalf("恰好等于上限应被允许: %v", err)
	}
	if len(body) != maxRequestBody {
		t.Fatalf("长度应为 %d, got %d", maxRequestBody, len(body))
	}
}
