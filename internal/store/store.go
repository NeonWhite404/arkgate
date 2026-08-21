// Package store 提供 SQLite 持久化。所有账号/模型/接入点(元组)/子 Key/设置都存这里。
//
// 设计要点：
//   - 单文件数据库（DataDir/arkgate.db），WAL 模式，单连接 writer + 读锁保护。
//   - API Key 明文不落库——加密后才写入（由调用方用 secure.Box 先行加密）。
//   - 启动时自动建表与迁移（幂等，兼容旧库缺列）。
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"arkgate/internal/model"
)

// Store 封装数据库访问。
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// New 打开（必要时创建）数据目录下的 SQLite 数据库并建表。
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "arkgate.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ─────────────────────────── 建表 & 迁移 ───────────────────────────

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
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
		`CREATE TABLE IF NOT EXISTS models (
			name TEXT PRIMARY KEY,
			display TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS endpoints (
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
		`CREATE TABLE IF NOT EXISTS subkeys (
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
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			subkey_id TEXT NOT NULL DEFAULT '',
			subkey_name TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			account_name TEXT NOT NULL DEFAULT '',
			endpoint_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_logs(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_endpoints_account ON endpoints(account_id)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// 旧库缺列补齐（幂等，忽略「列已存在」错误）。
	alters := []string{
		`ALTER TABLE endpoints ADD COLUMN weight INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN max_concurrency INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN tpm_limit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN total_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN success_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN fail_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE endpoints ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_logs ADD COLUMN endpoint_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN weight INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE accounts ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN total_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN success_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN fail_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`,
	}
	for _, st := range alters {
		// 只忽略「列已存在」这类良性幂等错误；其余错误向上抛，避免迁移静默失败后
		// 到查询期才以 "no such column" 崩溃。
		if _, err := s.db.Exec(st); err != nil {
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrate alter %q: %w", st, err)
		}
	}
	return nil
}

func nowUnix() int64 { return time.Now().Unix() }
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func nonzero(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// ─────────────────────────── Account ───────────────────────────

func scanAccount(rows *sql.Rows) (*model.Account, error) {
	a := &model.Account{}
	if err := rows.Scan(&a.ID, &a.Name, &a.ArkAPIKeyEnc, &a.KeyHint, &a.Status, &a.Weight,
		&a.CreatedAt, &a.LastUsedAt, &a.TotalRequests, &a.SuccessRequests, &a.FailRequests,
		&a.PromptTokens, &a.CompletionTokens, &a.TotalTokens); err != nil {
		return nil, err
	}
	return a, nil
}

const accountCols = `id,name,ark_key_enc,key_hint,status,weight,created_at,last_used_at,
	total_requests,success_requests,fail_requests,prompt_tokens,completion_tokens,total_tokens`

func (s *Store) ListAccounts() ([]*model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*model.Account, 0)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccount(id string) (*model.Account, error) {
	all, err := s.ListAccounts()
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *Store) UpsertAccount(a *model.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO accounts (id,name,ark_key_enc,key_hint,status,weight,created_at,
			last_used_at,total_requests,success_requests,fail_requests,prompt_tokens,completion_tokens,total_tokens)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, ark_key_enc=excluded.ark_key_enc,
			key_hint=excluded.key_hint, status=excluded.status, weight=excluded.weight`,
		a.ID, a.Name, a.ArkAPIKeyEnc, a.KeyHint, a.Status, a.Weight, nonzero(a.CreatedAt, nowUnix()),
		a.LastUsedAt, a.TotalRequests, a.SuccessRequests, a.FailRequests, a.PromptTokens,
		a.CompletionTokens, a.TotalTokens)
	return err
}

func (s *Store) DeleteAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM endpoints WHERE account_id=?`, id); err != nil {
		return err
	}
	return nil
}

// AccumulateAccount 累计账号用量/请求计数。
func (s *Store) AccumulateAccount(id string, ok bool, pt, ct int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	col := "success_requests"
	if !ok {
		col = "fail_requests"
	}
	_, err := s.db.Exec(`UPDATE accounts SET last_used_at=?, total_requests=total_requests+1,
		`+col+"="+col+`+1, prompt_tokens=prompt_tokens+?, completion_tokens=completion_tokens+?,
		total_tokens=total_tokens+? WHERE id=?`,
		nowUnix(), pt, ct, pt+ct, id)
	return err
}

// ─────────────────────────── Model ───────────────────────────

func (s *Store) ListModels() ([]*model.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT name,display,description,enabled,created_at FROM models ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Model{}
	for rows.Next() {
		m := &model.Model{}
		if err := rows.Scan(&m.Name, &m.Display, &m.Description, &m.Enabled, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpsertModel(m *model.Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO models(name,display,description,enabled,created_at)
		VALUES(?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET display=excluded.display,
		description=excluded.description, enabled=excluded.enabled`,
		m.Name, m.Display, m.Description, boolInt(m.Enabled), nonzero(m.CreatedAt, nowUnix()))
	return err
}

func (s *Store) DeleteModel(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM models WHERE name=?`, name); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM endpoints WHERE model=?`, name); err != nil {
		return err
	}
	return nil
}

// ─────────────────────────── Endpoint（元组） ───────────────────────────

const endpointCols = `id,account_id,model,ep,enabled,created_at,weight,max_concurrency,rpm_limit,tpm_limit,
	last_used_at,total_requests,success_requests,fail_requests,prompt_tokens,completion_tokens,total_tokens`

func scanEndpoint(rows *sql.Rows) (*model.Endpoint, error) {
	e := &model.Endpoint{}
	if err := rows.Scan(&e.ID, &e.AccountID, &e.Model, &e.EP, &e.Enabled, &e.CreatedAt,
		&e.Weight, &e.MaxConcurrency, &e.RPMLimit, &e.TPMLimit, &e.LastUsedAt,
		&e.TotalRequests, &e.SuccessRequests, &e.FailRequests, &e.PromptTokens,
		&e.CompletionTokens, &e.TotalTokens); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Store) ListEndpoints() ([]*model.Endpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT ` + endpointCols + ` FROM endpoints ORDER BY account_id, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetEndpoint(id string) (*model.Endpoint, error) {
	all, err := s.ListEndpoints()
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *Store) UpsertEndpoint(e *model.Endpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 冲突键是 (account_id, model)，但 id 是主键。为避免「编辑改归属字段时静默
	// 改到他人行/产生陈旧孤儿行」，先按 id 定位：若该 id 已存在，则直接覆盖该行
	// （account_id/model 同步更新，触发冲突时同把 id 一起写回，保证前端手里的
	// id 与库中行一致）；若 id 不存在但 (account_id,model) 撞上既有行，则拒绝并报错。
	var existingID string
	if e.ID != "" {
		err := s.db.QueryRow(`SELECT id FROM endpoints WHERE id=?`, e.ID).Scan(&existingID)
		if err == nil {
			// 存在：按 id 定位行，覆盖所有字段。
			return s.updateEndpointByID(e, existingID)
		}
		// id 不存在：落到下面的「插入或冲突」逻辑。
		existingID = ""
	}
	_, err := s.db.Exec(`INSERT INTO endpoints (id,account_id,model,ep,enabled,created_at,weight,
			max_concurrency,rpm_limit,tpm_limit,last_used_at,total_requests,success_requests,
			fail_requests,prompt_tokens,completion_tokens,total_tokens)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id,model) DO UPDATE SET ep=excluded.ep, enabled=excluded.enabled,
			weight=excluded.weight, max_concurrency=excluded.max_concurrency,
			rpm_limit=excluded.rpm_limit, tpm_limit=excluded.tpm_limit`,
		e.ID, e.AccountID, e.Model, e.EP, boolInt(e.Enabled), nonzero(e.CreatedAt, nowUnix()),
		e.Weight, e.MaxConcurrency, e.RPMLimit, e.TPMLimit, e.LastUsedAt,
		e.TotalRequests, e.SuccessRequests, e.FailRequests, e.PromptTokens, e.CompletionTokens, e.TotalTokens)
	return err
}

// updateEndpointByID 按主键 id 覆盖一行（含 account_id/model 归属的变更）。
func (s *Store) updateEndpointByID(e *model.Endpoint, id string) error {
	_, err := s.db.Exec(`UPDATE endpoints SET account_id=?, model=?, ep=?, enabled=?, weight=?,
			max_concurrency=?, rpm_limit=?, tpm_limit=? WHERE id=?`,
		e.AccountID, e.Model, e.EP, boolInt(e.Enabled), e.Weight, e.MaxConcurrency,
		e.RPMLimit, e.TPMLimit, id)
	return err
}

func (s *Store) DeleteEndpoint(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM endpoints WHERE id=?`, id)
	return err
}

// AccumulateEndpoint 累计元组用量/请求计数。
func (s *Store) AccumulateEndpoint(id string, ok bool, pt, ct int64) error {
	if id == "" || strings.HasPrefix(id, "passthrough:") {
		return nil // 透传元组无持久化统计
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	col := "success_requests"
	if !ok {
		col = "fail_requests"
	}
	_, err := s.db.Exec(`UPDATE endpoints SET last_used_at=?, total_requests=total_requests+1,
		`+col+"="+col+`+1, prompt_tokens=prompt_tokens+?, completion_tokens=completion_tokens+?,
		total_tokens=total_tokens+? WHERE id=?`,
		nowUnix(), pt, ct, pt+ct, id)
	return err
}

// ─────────────────────────── SubKey ───────────────────────────

func (s *Store) ListSubKeys() ([]*model.SubKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id,name,key_text,key_hash,enabled,allowed_models,allowed_accounts,
		daily_limit_tokens,expires_at,created_at,last_used_at,total_requests,total_tokens
		FROM subkeys ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.SubKey{}
	for rows.Next() {
		sk := &model.SubKey{}
		var am, aa string
		if err := rows.Scan(&sk.ID, &sk.Name, &sk.Key, &sk.KeyHash, &sk.Enabled, &am, &aa,
			&sk.DailyLimitTokens, &sk.ExpiresAt, &sk.CreatedAt, &sk.LastUsedAt,
			&sk.TotalRequests, &sk.TotalTokens); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(am), &sk.AllowedModels)
		_ = json.Unmarshal([]byte(aa), &sk.AllowedAccounts)
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *Store) GetSubKeyByHash(hash string) (*model.SubKey, error) {
	all, err := s.ListSubKeys()
	if err != nil {
		return nil, err
	}
	for _, sk := range all {
		if sk.KeyHash == hash {
			return sk, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *Store) UpsertSubKey(sk *model.SubKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	am, _ := json.Marshal(sk.AllowedModels)
	aa, _ := json.Marshal(sk.AllowedAccounts)
	_, err := s.db.Exec(`INSERT INTO subkeys
		(id,name,key_text,key_hash,enabled,allowed_models,allowed_accounts,daily_limit_tokens,expires_at,created_at,
		 last_used_at,total_requests,total_tokens)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, key_text=excluded.key_text,
			key_hash=excluded.key_hash, enabled=excluded.enabled, allowed_models=excluded.allowed_models,
			allowed_accounts=excluded.allowed_accounts, daily_limit_tokens=excluded.daily_limit_tokens,
			expires_at=excluded.expires_at`,
		sk.ID, sk.Name, sk.Key, sk.KeyHash, boolInt(sk.Enabled), string(am), string(aa),
		sk.DailyLimitTokens, sk.ExpiresAt, nonzero(sk.CreatedAt, nowUnix()),
		sk.LastUsedAt, sk.TotalRequests, sk.TotalTokens)
	return err
}

func (s *Store) DeleteSubKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM subkeys WHERE id=?`, id)
	return err
}

func (s *Store) AccumulateSubKey(id string, ok bool, pt, ct int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE subkeys SET last_used_at=?, total_requests=total_requests+1,
		total_tokens=total_tokens+? WHERE id=?`, nowUnix(), pt+ct, id)
	return err
}

// ─────────────────────────── Usage log ───────────────────────────

func (s *Store) AddUsageLog(l *model.UsageLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO usage_logs
		(ts,subkey_id,subkey_name,account_id,account_name,endpoint_id,model,prompt_tokens,completion_tokens,total_tokens,status,latency_ms,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nonzero(l.TS, nowUnix()), l.SubKeyID, l.SubKeyName, l.AccountID, l.AccountName, l.EndpointID,
		l.Model, l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Status, l.LatencyMs, l.Error)
	return err
}

func (s *Store) ListUsageLogs(limit int) ([]*model.UsageLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id,ts,subkey_id,subkey_name,account_id,account_name,endpoint_id,model,
		prompt_tokens,completion_tokens,total_tokens,status,latency_ms,error
		FROM usage_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.UsageLog{}
	for rows.Next() {
		l := &model.UsageLog{}
		if err := rows.Scan(&l.ID, &l.TS, &l.SubKeyID, &l.SubKeyName, &l.AccountID, &l.AccountName,
			&l.EndpointID, &l.Model, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens,
			&l.Status, &l.LatencyMs, &l.Error); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClearUsageLogs 清空日志表。
func (s *Store) ClearUsageLogs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM usage_logs`)
	return err
}

// ─────────────────────────── Settings ───────────────────────────

func (s *Store) GetSetting(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ─────────────────────────── 工具 ───────────────────────────

// NormalizeKey 标准化用户输入的子 Key：补全 sk- 前缀。
func NormalizeKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if !strings.HasPrefix(k, "sk-") {
		return "sk-" + k
	}
	return k
}

// NormalizeEP 标准化接入点输入：去除前后空白。
func NormalizeEP(ep string) string { return strings.TrimSpace(ep) }
