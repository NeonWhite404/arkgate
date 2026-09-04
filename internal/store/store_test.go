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
		Fallback: []string{}, PriceImage: 0.5, PriceInput: 1.5, PriceOutput: 2.5,
		ContextTokens: 4096, MaxOutputTokens: 1024}
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
	if gm.ContextTokens != 4096 || gm.MaxOutputTokens != 1024 {
		t.Fatalf("model limits roundtrip: %+v", gm)
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
		ClientIP: "198.51.100.7",
	}); err != nil {
		t.Fatalf("add log: %v", err)
	}
	logs, err := s.ListUsageLogs(10, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %v (%v)", logs, err)
	}
	if logs[0].Provider != "custom" || logs[0].Modality != model.ModelTypeImage || logs[0].ImageCount != 2 {
		t.Fatalf("log roundtrip: %+v", logs[0])
	}
	if logs[0].Cost != 1.0 {
		t.Fatalf("log cost = %v", logs[0].Cost)
	}
	if logs[0].ClientIP != "198.51.100.7" {
		t.Fatalf("log client ip = %q", logs[0].ClientIP)
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
	// v5 迁移已把 endpoints 的唯一约束放宽到三元组：同账号同模型可再挂一个 ep，
	// 且旧行必须原样保留（重建表不能丢数据）。
	if err := s.UpsertEndpoint(&model.Endpoint{
		ID: "e1", AccountID: "legacy", Model: "old", EP: "ep-legacy-250828", Enabled: true, Weight: 2,
	}); err != nil {
		t.Fatalf("second ep on same account+model: %v", err)
	}
	eps, err := s.ListEndpoints()
	if err != nil || len(eps) != 2 {
		t.Fatalf("endpoints after relax: %d (%v)", len(eps), err)
	}
	byEP := map[string]*model.Endpoint{}
	for _, e := range eps {
		byEP[e.EP] = e
	}
	if old, ok := byEP["ep-legacy"]; !ok || old.ID != "e0" || !old.Enabled {
		t.Fatalf("legacy endpoint row lost or altered: %+v", byEP)
	}
	if fresh, ok := byEP["ep-legacy-250828"]; !ok || fresh.Weight != 2 {
		t.Fatalf("new endpoint not persisted: %+v", byEP)
	}
}

// TestEndpointTupleUnique 锁定映射唯一性口径：唯一键是
// (账号, 模型名, 上游标识) 三元组——同账号同模型的多个 ep 可并存（同模型不同
// 发布版本），完全相同的三元组仍视为同一条（更新而非重复插入）。
func TestEndpointTupleUnique(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	mk := func(id, ep string, weight int) *model.Endpoint {
		return &model.Endpoint{ID: id, AccountID: "a1", Model: "doubao-seed-1-6", EP: ep, Enabled: true, Weight: weight}
	}
	for _, e := range []*model.Endpoint{
		mk("e1", "ep-250615", 1),
		mk("e2", "ep-250828", 3),
	} {
		if err := s.UpsertEndpoint(e); err != nil {
			t.Fatalf("upsert %s: %v", e.EP, err)
		}
	}
	eps, err := s.ListEndpoints()
	if err != nil || len(eps) != 2 {
		t.Fatalf("two versions must coexist: %d (%v)", len(eps), err)
	}

	// 三元组查重：命中返回既有 id，不同 ep 未命中。
	if id, ok := s.EndpointIDByTuple("a1", "doubao-seed-1-6", "ep-250828"); !ok || id != "e2" {
		t.Fatalf("tuple lookup = %q %v", id, ok)
	}
	if _, ok := s.EndpointIDByTuple("a1", "doubao-seed-1-6", "ep-unknown"); ok {
		t.Fatalf("unknown tuple must not match")
	}

	// 同三元组 + 新 id：更新既有行的流控字段，不新增行（否则会出现重复叶子）。
	if err := s.UpsertEndpoint(mk("e-dup", "ep-250828", 9)); err != nil {
		t.Fatalf("same tuple upsert: %v", err)
	}
	eps, _ = s.ListEndpoints()
	if len(eps) != 2 {
		t.Fatalf("same tuple must not duplicate: %d", len(eps))
	}
	got, err := s.GetEndpoint("e2")
	if err != nil || got.Weight != 9 {
		t.Fatalf("same tuple must update existing row: %+v (%v)", got, err)
	}
}

// TestQueryUsage 锁定用量分析聚合：小时/天分桶（本地时区）、维度 facet、
// 实体过滤、成功计数。
func TestQueryUsage(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	mk := func(ts int64, name, sub, acc string, pt, ct int64, ok bool, cost float64) {
		st := "ok"
		if !ok {
			st = "error"
		}
		l := modelLog(ts, name, sub, acc, pt, ct, st, cost)
		if err := s.AddUsageLog(&l); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}
	mk(now-3600, "m1", "s1", "a1", 100, 50, true, 0.10)
	mk(now-60, "m2", "s2", "a1", 10, 0, false, 0)
	mk(now-30, "m1", "s1", "a2", 200, 25, true, 0.20)

	q := UsageQuery{From: now - 86400, To: now, Granularity: "hour"}
	res, err := s.QueryUsage(q)
	if err != nil {
		t.Fatalf("query hour: %v", err)
	}
	if res.Summary.Requests != 3 || res.Summary.Success != 2 {
		t.Fatalf("summary: %+v", res.Summary)
	}
	if res.Summary.TotalTokens != 385 || res.Summary.PromptTokens != 310 || res.Summary.CompletionTokens != 75 {
		t.Fatalf("summary tokens: %+v", res.Summary)
	}
	if diff := res.Summary.Cost - 0.30; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("summary cost: %v", res.Summary.Cost)
	}
	// 小时桶：now-3600 与 now-30/now-60 至少分属两桶（跨整点时为三桶）。
	if len(res.Series) < 2 {
		t.Fatalf("hour series buckets = %d, want >= 2", len(res.Series))
	}
	var gotTok int64
	for _, b := range res.Series {
		gotTok += b.PromptTokens + b.CompletionTokens
	}
	if gotTok != 385 {
		t.Fatalf("series tokens sum = %d", gotTok)

	}
	// 天桶：三个时间点落在的不同本地自然日数量。
	daySet := map[string]bool{}
	for _, ts := range []int64{now - 3600, now - 60, now - 30} {
		daySet[time.Unix(ts, 0).Format("2006-01-02")] = true
	}
	resD, err := s.QueryUsage(UsageQuery{From: now - 86400, To: now, Granularity: "day"})
	if err != nil {
		t.Fatalf("query day: %v", err)
	}
	if len(resD.Series) != len(daySet) {
		t.Fatalf("day buckets = %d, want %d", len(resD.Series), len(daySet))
	}

	// 模型维度 facet + 实体过滤。
	resM, err := s.QueryUsage(UsageQuery{From: now - 86400, To: now, Granularity: "day", Dim: "model"})
	if err != nil {
		t.Fatalf("query dim: %v", err)
	}
	if len(resM.Facets) != 2 {
		t.Fatalf("model facets = %d, want 2", len(resM.Facets))
	}
	if resM.Facets[0].Key != "m1" { // 按 tokens 降序：m1(375) > m2(10)
		t.Fatalf("facet order: %+v", resM.Facets)
	}
	resF, err := s.QueryUsage(UsageQuery{From: now - 86400, To: now, Dim: "subkey", Entity: "s1"})
	if err != nil {
		t.Fatalf("query entity: %v", err)
	}
	if resF.Summary.Requests != 2 || resF.Summary.Success != 2 || resF.Summary.TotalTokens != 375 {
		t.Fatalf("entity summary: %+v", resF.Summary)
	}
	if len(resF.Facets) == 0 || resF.Facets[0].Key != "s1" {
		t.Fatalf("entity facets: %+v", resF.Facets)
	}

	// 非法参数回落：gran/dim 未知时不报错、dim 被清空。
	resBad, err := s.QueryUsage(UsageQuery{From: now - 86400, To: now, Granularity: "week", Dim: "hack"})
	if err != nil {
		t.Fatalf("query bad: %v", err)
	}
	if len(resBad.Series) == 0 || len(resBad.Facets) != 0 {
		t.Fatalf("bad params result: series=%d facets=%d", len(resBad.Series), len(resBad.Facets))
	}
}

// TestUsageLogPagination 锁定日志分页：按 id 倒序、offset 翻页不重不漏，
// 总数独立于分页参数；越界 offset 返回空页而不是报错。
func TestUsageLogPagination(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	for i := 0; i < 25; i++ {
		if err := s.AddUsageLog(&model.UsageLog{
			SubKeyID: "s1", Model: "m", Status: "ok", PromptTokens: int64(i),
		}); err != nil {
			t.Fatalf("add log %d: %v", i, err)
		}
	}
	total, err := s.CountUsageLogs()
	if err != nil || total != 25 {
		t.Fatalf("count = %d (%v)", total, err)
	}

	first, err := s.ListUsageLogs(10, 0)
	if err != nil || len(first) != 10 {
		t.Fatalf("page 1 = %d (%v)", len(first), err)
	}
	// 倒序：第一页首条是最后写入的那条（prompt_tokens=24）。
	if first[0].PromptTokens != 24 {
		t.Fatalf("page 1 must start at newest, got %d", first[0].PromptTokens)
	}
	second, err := s.ListUsageLogs(10, 10)
	if err != nil || len(second) != 10 || second[0].PromptTokens != 14 {
		t.Fatalf("page 2 = %d first=%v (%v)", len(second), second[0].PromptTokens, err)
	}
	last, err := s.ListUsageLogs(10, 20)
	if err != nil || len(last) != 5 {
		t.Fatalf("page 3 = %d (%v)", len(last), err)
	}
	// 页间不重叠。
	seen := map[int64]bool{}
	for _, l := range append(append(first, second...), last...) {
		if seen[l.ID] {
			t.Fatalf("duplicate row across pages: id=%d", l.ID)
		}
		seen[l.ID] = true
	}
	if len(seen) != 25 {
		t.Fatalf("pages covered %d rows, want 25", len(seen))
	}
	// 越界 offset / 非法参数：空页 + 不报错。
	if out, err := s.ListUsageLogs(10, 999); err != nil || len(out) != 0 {
		t.Fatalf("offset past end: %d (%v)", len(out), err)
	}
	if out, err := s.ListUsageLogs(0, -5); err != nil || len(out) != 25 {
		t.Fatalf("illegal params must fall back: %d (%v)", len(out), err)
	}
}

// modelLog 仅为 TestQueryUsage 服务的构造辅助（返回一条日志）。
func modelLog(ts int64, name, sub, acc string, pt, ct int64, status string, cost float64) model.UsageLog {
	return model.UsageLog{
		TS: ts, SubKeyID: sub, SubKeyName: sub + "-name",
		AccountID: acc, AccountName: acc + "-name", Provider: "ark",
		EndpointID: "e-" + acc, EP: "ep-" + acc,
		RequestedModel: name, Model: name, Modality: "text",
		PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct,
		Cost: cost, Status: status,
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
