package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"arkgate/internal/config"
	"arkgate/internal/store"
)

// newRetriesTestAdmin 构造带 store + cfg 的最小 Admin（处理运行时设置够用）。
func newRetriesTestAdmin(t *testing.T) (*Admin, *config.Config) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Load()
	return &Admin{store: st, cfg: cfg}, cfg
}

func putRuntime(t *testing.T, a *Admin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/settings/runtime", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	a.handleRuntimeSettings(rec, req)
	return rec
}

// TestRuntimeSettingsMaxRetriesHotUpdate 锁定跨接入点重试次数的
// 设置页链路：PUT 热改立即生效、持久化到 settings 表、GET 回显一致、
// 越界值拒绝且不落库。
func TestRuntimeSettingsMaxRetriesHotUpdate(t *testing.T) {
	a, cfg := newRetriesTestAdmin(t)

	// 初始回显：未写库时应为内置默认。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/settings/runtime", nil)
	a.handleRuntimeSettings(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if int(body["max_retries"].(float64)) != config.DefaultMaxRetries {
		t.Fatalf("初始 max_retries 应为默认 %d，got %v", config.DefaultMaxRetries, body["max_retries"])
	}
	if int(body["max_retries_hard_limit"].(float64)) != config.MaxRetriesHardLimit {
		t.Fatalf("硬上限回显不一致: %v", body["max_retries_hard_limit"])
	}

	// 热改为 5：cfg 立即生效 + 落库。
	rec = putRuntime(t, a, `{"max_retries":5}`)
	if rec.Code != 200 {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if cfg.Retries.Max() != 5 {
		t.Fatalf("热改未生效: %d", cfg.Retries.Max())
	}
	if v, ok := a.store.GetSetting(keyMaxRetries); !ok || v != "5" {
		t.Fatalf("未持久化: %q %v", v, ok)
	}

	// 0 = 关闭重试，是合法值。
	rec = putRuntime(t, a, `{"max_retries":0}`)
	if rec.Code != 200 {
		t.Fatalf("PUT 0 status = %d", rec.Code)
	}
	if cfg.Retries.Max() != 0 {
		t.Fatalf("0 未生效: %d", cfg.Retries.Max())
	}

	// 越界拒绝：不落库、不改内存值。
	rec = putRuntime(t, a, `{"max_retries":99}`)
	if rec.Code != 400 {
		t.Fatalf("超上限应 400, got %d", rec.Code)
	}
	if cfg.Retries.Max() != 0 {
		t.Fatalf("越界请求不该改动内存值: %d", cfg.Retries.Max())
	}
	if v, _ := a.store.GetSetting(keyMaxRetries); v != "0" {
		t.Fatalf("越界请求不该落库: %q", v)
	}
	rec = putRuntime(t, a, `{"max_retries":-1}`)
	if rec.Code != 400 {
		t.Fatalf("负值应 400, got %d", rec.Code)
	}

	// 部分更新：不带 max_retries 的请求不得清掉它。
	rec = putRuntime(t, a, `{"request_timeout_sec":10}`)
	if rec.Code != 200 {
		t.Fatalf("部分更新 status = %d", rec.Code)
	}
	if cfg.Retries.Max() != 0 {
		t.Fatalf("部分更新不得改动重试次数: %d", cfg.Retries.Max())
	}
}

// TestLoadPersistedRetries 启动回填：DB 值优先于环境变量/默认值。
func TestLoadPersistedRetries(t *testing.T) {
	a, cfg := newRetriesTestAdmin(t)
	if err := a.store.SetSetting(keyMaxRetries, "6"); err != nil {
		t.Fatal(err)
	}
	a.loadPersistedRetries()
	if cfg.Retries.Max() != 6 {
		t.Fatalf("启动回填未生效: %d", cfg.Retries.Max())
	}

	// 脏值（非数字）按未设置处理，保留原值。
	if err := a.store.SetSetting(keyMaxRetries, "abc"); err != nil {
		t.Fatal(err)
	}
	a.loadPersistedRetries()
	if cfg.Retries.Max() != 6 {
		t.Fatalf("脏值不该破坏现有配置: %d", cfg.Retries.Max())
	}
}
