package store

import (
	"path/filepath"
	"testing"

	"database/sql"
)

// 审计：旧库（v1 基线）升级后，v13 新增的索引与 rollup 表是否建齐？
//
// 为什么单独测这个：只验证「迁移不报错」不够——**漏建索引不会报错**，
// 只会让查询悄悄退化成全表扫（数据量小时完全看不出来，上线后才暴露）。
//
// 注意 schema 用的是**真实 v1 基线**（与 TestMigrateFromLegacySchema 同一份），
// 不是精简臆造版：一开始我用了只含 id/ts 的最小 schema，结果迁移因缺列失败，
// 差点误判成「旧库升级不了」——问题出在测试夹具不真实。
func TestAuditLegacyGetsAllIndexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arkgate.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
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
			t.Fatalf("建旧库失败: %v / %s", err, st)
		}
	}
	_ = db.Close()

	st, err := New(dir)
	if err != nil {
		t.Fatalf("旧库升级失败: %v", err)
	}
	defer st.Close()

	// v13 起新增、且**必须**在旧库上补齐的索引。缺任何一个都意味着查询退化。
	want := []string{
		"idx_usage_ts", "idx_endpoints_account", "idx_subkeys_hash", "idx_usage_subkey",
		"idx_usage_ts_model", "idx_usage_ts_subkey", "idx_usage_ts_account",
		"idx_usage_status_ts", "idx_usage_kind_ts",
		"idx_rollup_hourly_kind", "idx_rollup_daily_kind",
	}
	have := map[string]bool{}
	rows, err := st.db.Query(`SELECT name FROM sqlite_master WHERE type='index'`)
	if err != nil {
		t.Fatalf("查索引: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		have[n] = true
	}
	rows.Close()
	for _, w := range want {
		if !have[w] {
			t.Errorf("旧库升级后缺少索引: %s", w)
		}
	}

	// 旧的两列唯一约束应已放宽为三元组（否则同账号同模型只能挂一个 ep）。
	var ddl string
	if err := st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='endpoints'`).Scan(&ddl); err != nil {
		t.Fatalf("读 endpoints ddl: %v", err)
	}
	if !contains(ddl, "account_id, model, ep") {
		t.Errorf("endpoints 唯一约束未放宽为三元组: %s", ddl)
	}

	// rollup 表应可用（旧库升级后必须也能预聚合）。
	var cnt int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_rollup_hourly`).Scan(&cnt); err != nil {
		t.Errorf("rollup 小时表不可用: %v", err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM usage_rollup_state`).Scan(&cnt); err != nil {
		t.Errorf("rollup 水位表不可用: %v", err)
	}

	// 旧数据必须还在（迁移不能悄悄丢数据）。
	var n int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM endpoints`).Scan(&n); err != nil {
		t.Fatalf("查 endpoints: %v", err)
	}
	if n != 1 {
		t.Errorf("旧接入点应在迁移后保留, got %d", n)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("查 accounts: %v", err)
	}
	if n != 1 {
		t.Errorf("旧账号应在迁移后保留, got %d", n)
	}
	// settings 数量会因迁移**增加**（写入 schema_version 等内部键），所以不能断言
	// 总数不变——改为断言「旧键的值原样保留」。
	var tok string
	if err := st.db.QueryRow(`SELECT value FROM settings WHERE key='admin_token_hash'`).Scan(&tok); err != nil {
		t.Fatalf("旧设置丢失: %v", err)
	}
	if tok != "abc" {
		t.Errorf("旧设置值被改动: %q", tok)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
