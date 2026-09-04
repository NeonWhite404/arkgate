package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestListModelsShapes 锁定上游模型列表的解析口径：标准 {"data":[...]}、
// 裸对象数组、裸字符串数组都能识别，空 id 条目被丢掉。
func TestListModelsShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"openai", `{"object":"list","data":[{"id":"gpt-4o","owned_by":"openai"},{"id":"gpt-4o-mini"}]}`,
			[]string{"gpt-4o", "gpt-4o-mini"}},
		{"bare-objects", `[{"id":"ep-250615"},{"id":"ep-250828"}]`, []string{"ep-250615", "ep-250828"}},
		{"bare-strings", `["deepseek-chat","deepseek-reasoner"]`, []string{"deepseek-chat", "deepseek-reasoner"}},
		{"skip-empty-id", `{"data":[{"id":""},{"id":"kept"}]}`, []string{"kept"}},
	}
	for _, c := range cases {
		var gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(c.body))
		}))
		m := NewManager()
		list, err := m.ListModels(context.Background(),
			Route{Def: Def{ID: "custom"}, BaseURL: srv.URL, Key: "opaque-key"}, time.Second)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if gotPath != "/models" {
			t.Fatalf("%s: path = %q", c.name, gotPath)
		}
		if gotAuth != "Bearer opaque-key" {
			t.Fatalf("%s: auth = %q（Key 必须原样进鉴权头）", c.name, gotAuth)
		}
		if len(list) != len(c.want) {
			t.Fatalf("%s: got %d items %+v", c.name, len(list), list)
		}
		for i, id := range c.want {
			if list[i].ID != id {
				t.Fatalf("%s: item %d = %q, want %q", c.name, i, list[i].ID, id)
			}
		}
	}
}

// TestListModelsErrors 上游非 2xx 包成 HTTPError（错误体保留，供管理端提示
// 真实原因）；无法解析的 200 响应必须报错而不是静默返回空列表。
func TestListModelsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()
	m := NewManager()
	_, err := m.ListModels(context.Background(),
		Route{BaseURL: srv.URL, Key: "bad"}, time.Second)
	he, ok := AsHTTPError(err)
	if !ok || he.Code != http.StatusUnauthorized {
		t.Fatalf("want HTTPError 401, got %v", err)
	}
	if string(he.Body) == "" {
		t.Fatalf("upstream error body must be preserved")
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>not json</html>`))
	}))
	defer junk.Close()
	if _, err := m.ListModels(context.Background(),
		Route{BaseURL: junk.URL, Key: "k"}, time.Second); err == nil {
		t.Fatalf("unparsable body must fail loudly")
	}
}
