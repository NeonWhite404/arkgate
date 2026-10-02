package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"arkgate/internal/model"
)

// 审计：v12 → 当前版本 的升级路径。这是**紧邻的旧版本**，此前没有任何测试覆盖
// （现有 TestMigrateFromLegacySchema 覆盖的是 v1 基线，跨度太大，无法发现
// 「v13 新增列没写进 ALTER 列表」这类遗漏）。
//
// 做法：手工构造一个「缺失 v13 列」的库 + 写入历史数据，然后走正常 Open 升级，
// 断言：① 升级成功；② 历史数据一字不改；③ 新列取默认值（= 旧行为）；
//       ④ 重复升级幂等。
func TestAuditMigrateV12ToV13(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arkgate.db")

	// ── 构造 v12 库：usage_logs 缺 v13 的 8 个列 ──
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pre := []string{
		`CREATE TABLE accounts (id TEXT PRIMARY KEY, name TEXT NOT NULL, ark_key_enc TEXT NOT NULL,
			key_hint TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active',
			weight INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0, total_requests INTEGER NOT NULL DEFAULT 0,
			success_requests INTEGER NOT NULL DEFAULT 0, fail_requests INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0, provider TEXT NOT NULL DEFAULT 'ark',
			base_url TEXT NOT NULL DEFAULT '', cap_responses INTEGER NOT NULL DEFAULT 0,
			cap_images INTEGER NOT NULL DEFAULT 0, total_images INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE models (name TEXT PRIMARY KEY, display TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
			fallback TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL DEFAULT 0,
			type TEXT NOT NULL DEFAULT 'text', price_input REAL NOT NULL DEFAULT 0,
			price_output REAL NOT NULL DEFAULT 0, price_image REAL NOT NULL DEFAULT 0,
			context_tokens INTEGER NOT NULL DEFAULT 0, max_output_tokens INTEGER NOT NULL DEFAULT 0,
			router TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '')`,
		// 注意：v12 的 endpoints 已放宽为三元组唯一键。
		`CREATE TABLE endpoints (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, model TEXT NOT NULL,
			ep TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL DEFAULT 0,
			weight INTEGER NOT NULL DEFAULT 0, max_concurrency INTEGER NOT NULL DEFAULT 0,
			rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0, total_requests INTEGER NOT NULL DEFAULT 0,
			success_requests INTEGER NOT NULL DEFAULT 0, fail_requests INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0, total_images INTEGER NOT NULL DEFAULT 0,
			request_headers TEXT NOT NULL DEFAULT '{}', upstream_deleted INTEGER NOT NULL DEFAULT 0,
			skip_upstream_check INTEGER NOT NULL DEFAULT 0, UNIQUE(account_id, model, ep))`,
		`CREATE TABLE subkeys (id TEXT PRIMARY KEY, name TEXT NOT NULL, key_text TEXT NOT NULL,
			key_hash TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
			allowed_models TEXT NOT NULL DEFAULT '[]', allowed_accounts TEXT NOT NULL DEFAULT '[]',
			daily_limit_tokens INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0, last_used_at INTEGER NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			daily_limit_images INTEGER NOT NULL DEFAULT 0, total_images INTEGER NOT NULL DEFAULT 0)`,
		// v12 的 usage_logs：有 first_token_ms/client_ip，但**没有** v13 的 8 列。
		`CREATE TABLE usage_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
			subkey_id TEXT NOT NULL DEFAULT '', subkey_name TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '', account_name TEXT NOT NULL DEFAULT '',
			endpoint_id TEXT NOT NULL DEFAULT '', requested_model TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '', ep TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0, first_token_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
			modality TEXT NOT NULL DEFAULT '', image_count INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0, client_ip TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE usage_daily (day TEXT NOT NULL, subkey_id TEXT NOT NULL,
			tokens INTEGER NOT NULL DEFAULT 0, images INTEGER NOT NULL DEFAULT 0,
			requests INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (day, subkey_id))`,
	}
	for _, st := range pre {
		if _, err := db.Exec(st); err != nil {
			t.Fatalf("建 v12 表失败: %v\n%s", err, st)
		}
	}
	// 历史数据：一条成功、一条失败。
	if _, err := db.Exec(`INSERT INTO usage_logs(id, ts, subkey_id, model, status, total_tokens,
		cost, first_token_ms, client_ip, error) VALUES
		(1, 1700000000, 'sk1', 'gpt-4o', 'ok', 100, 0.5, 120, '1.2.3.4', ''),
		(2, 1700000001, 'sk1', 'gpt-4o', 'error', 0, 0, 0, '1.2.3.4', '上游 500')`); err != nil {
		t.Fatalf("插入历史数据失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key, value) VALUES('schema_version','12')`); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	_ = db.Close()

	// ── 正常 Open：应触发迁移到 v13 ──
	st, err := New(dir)
	if err != nil {
		t.Fatalf("v12 库升级失败: %v", err)
	}
	defer st.Close()

	// ① 版本已是 13
	v, okv := st.GetSetting("schema_version")
	if !okv {
		t.Fatal("读版本失败")
	}
	// 不写死版本号：升级目标就是当前 schemaVersion，断言它避免每次加迁移都要改测试。
	if v != schemaVersion {
		t.Fatalf("版本应升到 %s, got %q", schemaVersion, v)
	}

	// ② 历史数据完整保留（一字不改）
	logs, total, err := st.QueryUsageLogs(LogFilter{}, 10, 0)
	if err != nil {
		t.Fatalf("查日志: %v", err)
	}
	if total != 2 || len(logs) != 2 {
		t.Fatalf("历史日志应保留 2 条, got total=%d len=%d", total, len(logs))
	}
	var ok1 bool
	for _, l := range logs {
		if l.ID == 1 {
			ok1 = true
			if l.TotalTokens != 100 || l.Cost != 0.5 || l.FirstTokenMs != 120 || l.ClientIP != "1.2.3.4" {
				t.Fatalf("历史行 1 被改动: %+v", l)
			}
			// ③ 新列取默认值（= 旧行为）
			if l.ErrorKind != "" || l.IsStream || l.CacheReadTokens != 0 ||
				l.InputCost != 0 || l.OutputCost != 0 || l.CacheCost != 0 || l.UserAgent != "" {
				t.Fatalf("新列应为零值, got %+v", l)
			}
		}
	}
	if !ok1 {
		t.Fatal("历史行 1 丢失")
	}

	// ④ 幂等：再次 Open 不应报错，数据仍 2 条
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("重复升级失败（幂等性破坏）: %v", err)
	}
	defer st2.Close()
	_, total2, err := st2.QueryUsageLogs(LogFilter{}, 10, 0)
	if err != nil {
		t.Fatalf("重复升级后查日志: %v", err)
	}
	if total2 != 2 {
		t.Fatalf("重复升级后日志数变了: %d", total2)
	}

	// ⑤ 升级后新写入的数据能带上 v13 列
	if err := st2.AddUsageLog(&model.UsageLog{
		TS: 1700000100, Model: "gpt-4o", Status: "ok", TotalTokens: 10,
		ErrorKind: "", IsStream: true, CacheReadTokens: 7, InputCost: 0.1,
		UserAgent: "audit/1.0",
	}); err != nil {
		t.Fatalf("写入新日志: %v", err)
	}
	newLogs, _, err := st2.QueryUsageLogs(LogFilter{}, 1, 0)
	if err != nil {
		t.Fatalf("查新日志: %v", err)
	}
	if len(newLogs) == 0 || !newLogs[0].IsStream || newLogs[0].CacheReadTokens != 7 ||
		newLogs[0].UserAgent != "audit/1.0" {
		t.Fatalf("v13 列未生效: %+v", newLogs[0])
	}
}

// AuditMigrateV13ToV14：v13 → v14（缓存单价列）升级路径。
//
// 与 v12→当前 的测试互补：那一个验证「缺 v13 列」的库，这一个验证「已有 v13、
// 只缺 v14 列」的库。断言重点是**旧模型的价格一字不改**——v14 只往 models 表加
// 两列，若迁移写成「重建表」就可能悄悄丢掉已有定价（那是直接的账单错误）。
func TestAuditMigrateV13ToV14(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "arkgate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 构造 v13 models 表：有 price_input/output/image，缺 price_cache_*。
	for _, st := range []string{
		`CREATE TABLE models (name TEXT PRIMARY KEY, display TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
			fallback TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL DEFAULT 0,
			type TEXT NOT NULL DEFAULT 'text', provider TEXT NOT NULL DEFAULT '',
			price_input REAL NOT NULL DEFAULT 0, price_output REAL NOT NULL DEFAULT 0,
			price_image REAL NOT NULL DEFAULT 0, context_tokens INTEGER NOT NULL DEFAULT 0,
			max_output_tokens INTEGER NOT NULL DEFAULT 0, router TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO models(name,display,enabled,price_input,price_output,price_image,context_tokens)
			VALUES('gpt-4o','GPT-4o',1,2.5,10,0,128000)`,
	} {
		if _, err := db.Exec(st); err != nil {
			t.Fatalf("build v13: %v", err)
		}
	}
	_ = db.Close()

	st, err := New(dir)
	if err != nil {
		t.Fatalf("v13 库升级失败: %v", err)
	}
	defer st.Close()

	m, err := st.GetModel("gpt-4o")
	if err != nil {
		t.Fatalf("读回模型: %v", err)
	}
	// 已有定价必须原样保留（迁移绝不能动 models 的既有列）。
	if m.PriceInput != 2.5 || m.PriceOutput != 10 || m.ContextTokens != 128000 {
		t.Fatalf("旧模型定价被改动: %+v", m)
	}
	// 新列取零值 = 「未设置」，计费回落到输入单价（等价旧行为）。
	if m.PriceCacheRead != 0 || m.PriceCacheWrite != 0 {
		t.Fatalf("新增缓存单价列应为 0（未设置）, got read=%v write=%v",
			m.PriceCacheRead, m.PriceCacheWrite)
	}

	// 升级后能正常写入并读回缓存单价。
	m.PriceCacheRead, m.PriceCacheWrite = 0.25, 3.125
	if err := st.UpsertModel(m); err != nil {
		t.Fatalf("保存缓存单价: %v", err)
	}
	got, err := st.GetModel("gpt-4o")
	if err != nil {
		t.Fatalf("再读: %v", err)
	}
	if got.PriceCacheRead != 0.25 || got.PriceCacheWrite != 3.125 {
		t.Fatalf("缓存单价未持久化: read=%v write=%v", got.PriceCacheRead, got.PriceCacheWrite)
	}
}
