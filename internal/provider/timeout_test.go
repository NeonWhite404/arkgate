package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPostRespectsTimeout 锁定「超时按调用传入」：Manager 不再持有 Client.Timeout，
// 每次调用用 context 施加时限，因此管理端热改后下一次请求就生效。
func TestPostRespectsTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 一直挂住，直到测试放行
		w.Write([]byte(`{"ok":true}`))
	}))
	defer func() { close(release); srv.Close() }()

	m := NewManager()
	rt := Route{Def: Def{ID: "openai"}, BaseURL: srv.URL, Key: "k"}

	// 短超时：应在 timeout 附近失败，而不是等到上游返回。
	start := time.Now()
	if _, _, err := m.Chat(context.Background(), rt, []byte(`{"model":"m"}`), "ep", 150*time.Millisecond); err == nil {
		t.Fatalf("expected timeout error")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("timeout not honored, elapsed %v", el)
	}

	// timeout=0 表示不限时：用带超时的父 ctx 验证「不是 Manager 施加的时限」。
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, _, err := m.Chat(ctx, rt, []byte(`{"model":"m"}`), "ep", 0); err == nil {
		t.Fatalf("expected parent ctx deadline error")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("parent ctx deadline not honored, elapsed %v", el)
	}
}
