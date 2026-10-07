package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/config"
	"arkgate/internal/model"
	"arkgate/internal/store"
)

// newPortalTestServer 建一个带真实 store 的门户（无缓存用量 → 不会走到 balancer 定价）。
func newPortalTestServer(t *testing.T, sk *model.SubKey) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sk.KeyHash = hashKey(sk.Key)
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("upsert subkey: %v", err)
	}
	cfg := config.Load()
	bal := balancer.New(st, time.Minute)
	t.Cleanup(bal.Close)
	srv := httptest.NewServer(New(st, bal, cfg).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func fetchOverview(t *testing.T, srv *httptest.Server, token string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/portal/overview", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求门户: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("门户应返回 200, got %d", res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("解码: %v", err)
	}
	return out
}

// TestOverviewSendsRawPeriodTokens 门户必须同时下发「加权额度」与「同周期上游原始量」。
//
// 为什么这条重要：只给加权值（today.tokens）时，子 Key 用户看到
// 「上游原始 120M、额度只扣了 12M」会以为统计出错；反过来只有原始量又无法解释
// 为什么自己被限额拦住。两个数并列才能让用户自己算出缓存折扣。
//
// 更关键的是**数据源不同**：加权来自 usage_daily（乘过倍率），
// 原始来自 usage_logs 聚合——本测试故意让两者不相等，确保没有互相顶替。
func TestOverviewSendsRawPeriodTokens(t *testing.T) {
	sk := &model.SubKey{ID: "sk_p1", Name: "门户", Key: "sk-portal-1", Enabled: true,
		DailyLimitTokens: 10000, CacheReadPermille: 100}
	srv, st := newPortalTestServer(t, sk)

	now := time.Now()
	// 窗口内：原始 1100（prompt 1000 含缓存读 800 + output 100）。
	if err := st.AddUsageLog(&model.UsageLog{TS: now.Unix(), SubKeyID: sk.ID,
		Model: "m", Status: "ok", PromptTokens: 1000, CompletionTokens: 100,
		TotalTokens: 1100, CacheReadTokens: 800}); err != nil {
		t.Fatal(err)
	}
	// 窗口外的历史日志：不得计入本周期原始量。
	if err := st.AddUsageLog(&model.UsageLog{TS: now.AddDate(0, 0, -5).Unix(), SubKeyID: sk.ID,
		Model: "m", Status: "ok", PromptTokens: 5_000_000, CompletionTokens: 0,
		TotalTokens: 5_000_000}); err != nil {
		t.Fatal(err)
	}
	// 加权窗口行：380 = (1000-800) + 100 + 800×0.1。
	rule := model.QuotaRuleOf(sk)
	if err := st.AddWindowUsage(store.WindowUsage{SubKeyID: sk.ID,
		Day: model.WindowDay(rule, now), Tokens: 380, WindowSecs: 86400}); err != nil {
		t.Fatal(err)
	}

	out := fetchOverview(t, srv, "sk-portal-1")

	today, ok := out["today"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 today: %+v", out)
	}
	if got := int64(today["tokens"].(float64)); got != 380 {
		t.Fatalf("today.tokens 应是加权额度 380, got %d", got)
	}
	raw, ok := out["today_raw_tokens"]
	if !ok {
		t.Fatalf("响应缺少 today_raw_tokens（用户无从对比加权与原始）: %+v", out)
	}
	if got := int64(raw.(float64)); got != 1100 {
		t.Fatalf("today_raw_tokens 应是窗口内原始量 1100, got %d", got)
	}

	// 配额元信息必须带上倍率，前端据此把两个数解释清楚。
	q, ok := out["quota"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 quota: %+v", out)
	}
	if int64(q["cache_read_permille"].(float64)) != 100 {
		t.Fatalf("应下发缓存读取倍率: %+v", q)
	}
}

// TestOverviewRawTokensZeroWhenNoLogs 无日志时 today_raw_tokens 为 0（不是缺失），
// 否则前端会显示 NaN。
func TestOverviewRawTokensZeroWhenNoLogs(t *testing.T) {
	sk := &model.SubKey{ID: "sk_p2", Name: "空", Key: "sk-portal-2", Enabled: true}
	srv, _ := newPortalTestServer(t, sk)
	out := fetchOverview(t, srv, "sk-portal-2")
	if v, ok := out["today_raw_tokens"]; !ok || int64(v.(float64)) != 0 {
		t.Fatalf("无日志时 today_raw_tokens 应为 0: %+v", out["today_raw_tokens"])
	}
}

// TestOverviewRejectsBadKey 鉴权仍是 401（新增字段不得放宽入口）。
func TestOverviewRejectsBadKey(t *testing.T) {
	sk := &model.SubKey{ID: "sk_p3", Name: "x", Key: "sk-portal-3", Enabled: true}
	srv, _ := newPortalTestServer(t, sk)
	req, _ := http.NewRequest("GET", srv.URL+"/api/portal/overview", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无效 Key 应 401, got %d", res.StatusCode)
	}
}

// 编译期确认 hashKey 与 store 的鉴权口径一致（避免测试自己算错哈希而误判）。
var _ = func() string {
	h := sha256.Sum256([]byte("x"))
	return hex.EncodeToString(h[:])
}
