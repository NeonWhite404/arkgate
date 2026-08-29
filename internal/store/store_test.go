package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"arkgate/internal/model"

	_ "modernc.org/sqlite"
)

// TestStoreBasics 新字段读写回环 + 迁移幂等（同库重复打开）。
func TestStoreBasics(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	acc := &model.Account{
		ID: "a1", Name: "n", ArkAPIKeyEnc: "enc", KeyHint: "....9999",
		Status: model.AccountActive, Weight: 2,
		Provider: "custom", BaseURL: "http://x/v1", CapResponses: 1, CapImages: -1,
	}
	if err := s.UpsertAccount(acc); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	got, err := s.GetAccount("a1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Provider != "custom" || got.BaseURL != "http://x/v1" || got.CapResponses != 1 || got.CapImages != -1 {
		t.Fatalf("account roundtrip: %+v", got)
	}
	if got.CreatedAt == 0 {
		t.Fatal("CreatedAt should be defaulted")
	}

	m := &model.Model{Name: "img", Type: model.ModelTypeImage, Display: "图像", Enabled: true,
		Fallback: []string{}, PriceImage: 0.5, PriceInput: 1.5, PriceOutput: 2.5}
	if err := s.UpsertModel(m); err != nil {
		t.Fatalf("upsert model: %v", err)
	}
	gm, err := s.GetModel("img")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if gm.Type != model.ModelTypeImage {
		t.Fatalf("model type = %q", gm.Type)
	}
	if gm.PriceImage != 0.5 || gm.PriceInput != 1.5 || gm.PriceOutput != 2.5 {
		t.Fatalf("model prices roundtrip: %+v", gm)
	}

	ep := &model.Endpoint{ID: "e1", AccountID: "a1", Model: "img", EP: "doubao-seedream-4-0", Enabled: true, Weight: 3}
	if err := s.UpsertEndpoint(ep); err != nil {
		t.Fatalf("upsert endpoint: %v", err)
	}
	ge, err := s.GetEndpoint("e1")
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if ge.EP != "doubao-seedream-4-0" || ge.Weight != 3 {
		t.Fatalf("endpoint roundtrip: %+v", ge)
	}

	sk := &model.SubKey{ID: "s1", Key: "sk-x", KeyHash: "h", Enabled: true, DailyLimitImages: 100}
	if err := s.UpsertSubKey(sk); err != nil {
		t.Fatalf("upsert subkey: %v", err)
	}
	gs, err := s.GetSubKeyByHash("h")
	if err != nil {
		t.Fatalf("get subkey: %v", err)
	}
	if gs.DailyLimitImages != 100 {
		t.Fatalf("subkey images limit = %d", gs.DailyLimitImages)
	}

	if err := s.AddUsageLog(&model.UsageLog{
		SubKeyID: "s1", AccountID: "a1", Provider: "custom", EndpointID: "e1",
		RequestedModel: "img", Model: "img", EP: "doubao-seedream-4-0",
		Modality: model.ModelTypeImage, ImageCount: 2, Cost: 1.0, Status: "ok",
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}
	logs, err := s.ListUsageLogs(10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %v (%v)", logs, err)
	}
	if logs[0].Provider != "custom" || logs[0].Modality != model.ModelTypeImage || logs[0].ImageCount != 2 {
		t.Fatalf("log roundtrip: %+v", logs[0])
	}
	if logs[0].Cost != 1.0 {
		t.Fatalf("log cost = %v", logs[0].Cost)
	}

	// 日限额累计：两次 +3/+2 → tokens=5, images=2, requests=2, cost=0.75。
	if err := s.AddDailyUsage("s1", 3, 1, 1, 0.25); err != nil {
		t.Fatalf("daily: %v", err)
	}
	if err := s.AddDailyUsage("s1", 2, 1, 1, 0.5); err != nil {
		t.Fatalf("daily: %v", err)
	}
	du, err := s.GetDailyUsage("s1")
	if err != nil {
		t.Fatalf("get daily: %v", err)
	}
	if du.Tokens != 5 || du.Images != 2 || du.Requests != 2 {
		t.Fatalf("daily = %+v", du)
	}
	if diff := du.Cost - 0.75; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("daily cost = %v", du.Cost)
	}
	if empty, err := s.GetDailyUsage("nope"); err != nil || empty.Tokens != 0 {
		t.Fatalf("missing subkey daily should be zero: %+v %v", empty, err)
	}

	// 图像张数累计（三视图）。
	if err := s.AccumulateImages("a1", "e1", "s1", 2); err != nil {
		t.Fatalf("accumulate images: %v", err)
	}
	if ga, _ := s.GetAccount("a1"); ga.TotalImages != 2 {
		t.Fatalf("account images = %d", ga.TotalImages)
	}
	if ge2, _ := s.GetEndpoint("e1"); ge2.TotalImages != 2 {
		t.Fatalf("endpoint images = %d", ge2.TotalImages)
	}
	if gs2, _ := s.GetSubKeyByHash("h"); gs2.TotalImages != 2 {
		t.Fatalf("subkey images = %d", gs2.TotalImages)
	}

	// 迁移幂等：关闭重开不报错，数据仍在。
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if accs, err := s2.ListAccounts(); err != nil || len(accs) != 1 {
		t.Fatalf("after reopen: %d accounts (%v)", len(accs), err)
	}
	if v, ok := s2.GetSetting("schema_version"); !ok || v != schemaVersion {
		t.Fatalf("schema_version = %q %v", v, ok)
	}
}

// TestMigrateFromLegacySchema 锁定向前兼容：v1 之前的旧库（缺全部新增列）
// 打开时自动补列，存量数据可见且新列取等价旧行为的默认值。
func TestMigrateFromLegacySchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "arkgate.db"))
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	// 旧库 = v1 基线 schema（迁移前的 CREATE 语句，缺 v2 全部新增列）。
	legacy := []string{
		`CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			ark_key_enc TEXT NOT NULL,
			key_hint TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			weight INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0,
			success_requests INTEGER NOT NULL DEFAULT 0,
			fail_requests INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE models (
			name TEXT PRIMARY KEY,
			display TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			fallback TEXT NOT NULL DEFAULT '[]',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE endpoints (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			model TEXT NOT NULL,
			ep TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0,
			weight INTEGER NOT NULL DEFAULT 0,
			max_concurrency INTEGER NOT NULL DEFAULT 0,
			rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0,
			success_requests INTEGER NOT NULL DEFAULT 0,
			fail_requests INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			UNIQUE(account_id, model)
		)`,
		`CREATE TABLE subkeys (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			key_text TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_accounts TEXT NOT NULL DEFAULT '[]',
			daily_limit_tokens INTEGER NOT NULL DEFAULT 0,
			expires_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE usage_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			subkey_id TEXT NOT NULL DEFAULT '',
			subkey_name TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			account_name TEXT NOT NULL DEFAULT '',
			requested_model TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO accounts (id,name,ark_key_enc,key_hint,status)
			VALUES ('legacy','旧账号','enc','....8888','active')`,
		`INSERT INTO models (name,display,enabled) VALUES ('old','旧模型',1)`,
		`INSERT INTO endpoints (id,account_id,model,ep,enabled)
			VALUES ('e0','legacy','old','ep-legacy',1)`,
		`INSERT INTO settings (key,value) VALUES ('admin_token_hash','abc')`,
	}
	for _, st := range legacy {
		if _, err := db.Exec(st); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	s, err := New(dir)
	if err != nil {
		t.Fatalf("migrate open: %v", err)
	}
	defer s.Close()

	accs, err := s.ListAccounts()
	if err != nil || len(accs) != 1 {
		t.Fatalf("accounts after migrate: %d (%v)", len(accs), err)
	}
	a := accs[0]
	// 新列默认值 = 等价旧行为：ark 供应商、零能力覆盖（0=继承）、零统计。
	if a.Provider != "ark" || a.BaseURL != "" || a.CapResponses != 0 || a.CapImages != 0 {
		t.Fatalf("legacy account defaults: %+v", a)
	}
	if a.Name != "旧账号" {
		t.Fatalf("legacy data intact: %+v", a)
	}
	ms, err := s.ListModels()
	if err != nil || len(ms) != 1 || ms[0].Type != model.ModelTypeText {
		t.Fatalf("legacy model defaults: %+v (%v)", ms, err)
	}
	// 存量设置不受迁移影响。
	if v, ok := s.GetSetting("admin_token_hash"); !ok || v != "abc" {
		t.Fatalf("settings preserved: %q %v", v, ok)
	}
	// 旧库可继续正常写入。
	if err := s.UpsertModel(&model.Model{Name: "new", Type: model.ModelTypeImage, Enabled: true}); err != nil {
		t.Fatalf("write after migrate: %v", err)
	}
}

// TestSubKeyScopedQueries 锁定门户数据面：按子 Key 的统计与日志聚合，
// 日志列收窄（不含账号/供应商/接入点），SumCost 全局成本聚合。
func TestSubKeyScopedQueries(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	for i, sk := range []string{"s1", "s1", "s2"} {
		if err := s.AddUsageLog(&model.UsageLog{
			TS: now - int64(i)*10, SubKeyID: sk, SubKeyName: "name-" + sk,
			AccountID: "a1", AccountName: "账号甲", Provider: "ark", EndpointID: "e1", EP: "ep-1",
			RequestedModel: "m", Model: "m", Modality: model.ModelTypeText,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			Cost: 0.25, Status: "ok", LatencyMs: 42,
		}); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}
	// 一条失败日志（s1）。
	if err := s.AddUsageLog(&model.UsageLog{
		TS: now, SubKeyID: "s1", RequestedModel: "m", Model: "m",
		Modality: model.ModelTypeText, Status: "error", Error: "boom",
	}); err != nil {
		t.Fatalf("add fail log: %v", err)
	}

	st, err := s.SubKeyLogStats("s1", 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Requests != 3 || st.Success != 2 || st.Tokens != 300 {
		t.Fatalf("s1 stats: %+v", st)
	}
	if diff := st.Cost - 0.5; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("s1 cost = %v", st.Cost)
	}

	// 时间窗过滤：只统计最近 5 秒内的两条（fail + 最新 ok），旧 ok 与 s2 被排除。
	st2, err := s.SubKeyLogStats("s1", now-5)
	if err != nil {
		t.Fatalf("stats window: %v", err)
	}
	if st2.Requests != 2 || st2.Success != 1 || st2.Tokens != 150 {
		t.Fatalf("s1 windowed stats: %+v", st2)
	}

	logs, err := s.ListUsageLogsBySubKey("s1", 100)
	if err != nil || len(logs) != 3 {
		t.Fatalf("s1 logs = %d (%v)", len(logs), err)
	}
	for _, l := range logs {
		if l.AccountID != "" || l.AccountName != "" || l.Provider != "" || l.EndpointID != "" || l.EP != "" || l.SubKeyID != "" {
			t.Fatalf("portal log must be sanitized, got %+v", l)
		}
		if l.Cost != 0.25 && l.Status != "error" {
			t.Fatalf("log fields: %+v", l)
		}
	}
	if logs[0].RequestedModel != "m" {
		t.Fatalf("portal log fields: %+v", logs[0])
	}

	total, last24h, err := s.SumCost()
	if err != nil {
		t.Fatalf("sum cost: %v", err)
	}
	if diff := total - 0.75; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("total cost = %v", total)
	}
	if last24h <= 0 {
		t.Fatalf("24h cost = %v", last24h)
	}
}
