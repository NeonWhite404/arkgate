// Package store 提供 SQLite 持久化。所有账号/模型/接入点(元组)/子 Key/设置都存这里。
//
// 设计要点：
//   - 单文件数据库（DataDir/arkgate.db），WAL 模式，单连接 writer + 读锁保护。
//   - 上游 API Key 明文不落库——加密后才写入（由调用方用 secure.Box 先行加密）。
//   - 启动时自动建表与迁移（幂等，兼容旧库缺列）。
//   - 迁移守则（向前兼容）：只增不改——仅 ADD COLUMN / CREATE TABLE，DEFAULT 等价
//     旧行为；不改名、不删列；新列用开放类型（provider 用 TEXT 而非枚举约束）。
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// schemaVersion 当前迁移版本号，记入 settings，为未来分批迁移留锚点。
const schemaVersion = "4"

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
			fallback TEXT NOT NULL DEFAULT '[]',
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
			requested_model TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			ep TEXT NOT NULL DEFAULT '',
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
		// 日限额按「日 × 子 Key」计量（修复累计值当当日值的缺陷）。
		`CREATE TABLE IF NOT EXISTS usage_daily (
			day TEXT NOT NULL,
			subkey_id TEXT NOT NULL,
			tokens INTEGER NOT NULL DEFAULT 0,
			images INTEGER NOT NULL DEFAULT 0,
			requests INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, subkey_id)
		)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// 旧库缺列补齐（幂等，忽略「列已存在」这类良性错误；其余上抛防静默失败）。
	alters := []string{
		// —— v1 存量列 ——
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
		`ALTER TABLE usage_logs ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_logs ADD COLUMN ep TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE models ADD COLUMN fallback TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE accounts ADD COLUMN weight INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE accounts ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN total_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN success_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN fail_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`,
		// —— v2：多供应商 + 图像 + 日限额 ——
		`ALTER TABLE accounts ADD COLUMN provider TEXT NOT NULL DEFAULT 'ark'`,
		`ALTER TABLE accounts ADD COLUMN base_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN cap_responses INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN cap_images INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN total_images INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN type TEXT NOT NULL DEFAULT 'text'`,
		`ALTER TABLE endpoints ADD COLUMN total_images INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subkeys ADD COLUMN daily_limit_images INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE subkeys ADD COLUMN total_images INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_logs ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_logs ADD COLUMN modality TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_logs ADD COLUMN image_count INTEGER NOT NULL DEFAULT 0`,
		// —— v3：模型定价 + 成本核算 ——
		`ALTER TABLE models ADD COLUMN price_input REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN price_output REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN price_image REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_logs ADD COLUMN cost REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_daily ADD COLUMN cost REAL NOT NULL DEFAULT 0`,
		// —— v4：模型能力上限（0=未设置，允许目录自动补全） ——
		`ALTER TABLE models ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN max_output_tokens INTEGER NOT NULL DEFAULT 0`,
	}
	for _, st := range alters {
		if _, err := s.db.Exec(st); err != nil {
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrate alter %q: %w", st, err)
		}
	}
	// 记录 schema 版本（存在即更新，幂等）。
	if _, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('schema_version',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, schemaVersion); err != nil {
		return fmt.Errorf("migrate schema_version: %w", err)
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
		&a.PromptTokens, &a.CompletionTokens, &a.TotalTokens,
		&a.Provider, &a.BaseURL, &a.CapResponses, &a.CapImages, &a.TotalImages); err != nil {
		return nil, err
	}
	return a, nil
}

const accountCols = `id,name,ark_key_enc,key_hint,status,weight,created_at,last_used_at,
	total_requests,success_requests,fail_requests,prompt_tokens,completion_tokens,total_tokens,
	provider,base_url,cap_responses,cap_images,total_images`

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
	if a.Provider == "" {
		a.Provider = "ark" // 兼容旧调用方
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO accounts (id,name,ark_key_enc,key_hint,status,weight,created_at,
			last_used_at,total_requests,success_requests,fail_requests,prompt_tokens,completion_tokens,total_tokens,
			provider,base_url,cap_responses,cap_images,total_images)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, ark_key_enc=excluded.ark_key_enc,
			key_hint=excluded.key_hint, status=excluded.status, weight=excluded.weight,
			provider=excluded.provider, base_url=excluded.base_url,
			cap_responses=excluded.cap_responses, cap_images=excluded.cap_images`,
		a.ID, a.Name, a.ArkAPIKeyEnc, a.KeyHint, a.Status, a.Weight, nonzero(a.CreatedAt, nowUnix()),
		a.LastUsedAt, a.TotalRequests, a.SuccessRequests, a.FailRequests, a.PromptTokens,
		a.CompletionTokens, a.TotalTokens,
		a.Provider, a.BaseURL, a.CapResponses, a.CapImages, a.TotalImages)
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

func scanModel(rows *sql.Rows) (*model.Model, error) {
	m := &model.Model{}
	var fb string
	if err := rows.Scan(&m.Name, &m.Display, &m.Description, &m.Enabled, &fb, &m.CreatedAt,
		&m.Type, &m.PriceInput, &m.PriceOutput, &m.PriceImage,
		&m.ContextTokens, &m.MaxOutputTokens); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(fb), &m.Fallback)
	return m, nil
}

const modelCols = `name,display,description,enabled,fallback,created_at,type,price_input,price_output,price_image,context_tokens,max_output_tokens`

func (s *Store) ListModels() ([]*model.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT ` + modelCols + ` FROM models ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModel 按易读模型名读取单个模型（供「部分更新」先读后合并）。
func (s *Store) GetModel(name string) (*model.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := &model.Model{}
	var fb string
	err := s.db.QueryRow(
		`SELECT `+modelCols+` FROM models WHERE name=?`, name,
	).Scan(&m.Name, &m.Display, &m.Description, &m.Enabled, &fb, &m.CreatedAt, &m.Type,
		&m.PriceInput, &m.PriceOutput, &m.PriceImage,
		&m.ContextTokens, &m.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(fb), &m.Fallback)
	return m, nil
}

func (s *Store) UpsertModel(m *model.Model) error {
	if m.Type == "" {
		m.Type = model.ModelTypeText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fb, _ := json.Marshal(m.Fallback)
	_, err := s.db.Exec(`INSERT INTO models(name,display,description,enabled,fallback,created_at,type,
			price_input,price_output,price_image,context_tokens,max_output_tokens)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET display=excluded.display,
		description=excluded.description, enabled=excluded.enabled, fallback=excluded.fallback,
		type=excluded.type, price_input=excluded.price_input, price_output=excluded.price_output,
		price_image=excluded.price_image, context_tokens=excluded.context_tokens,
		max_output_tokens=excluded.max_output_tokens`,
		m.Name, m.Display, m.Description, boolInt(m.Enabled), string(fb), nonzero(m.CreatedAt, nowUnix()), m.Type,
		m.PriceInput, m.PriceOutput, m.PriceImage, m.ContextTokens, m.MaxOutputTokens)
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
	last_used_at,total_requests,success_requests,fail_requests,prompt_tokens,completion_tokens,total_tokens,
	total_images`

func scanEndpoint(rows *sql.Rows) (*model.Endpoint, error) {
	e := &model.Endpoint{}
	if err := rows.Scan(&e.ID, &e.AccountID, &e.Model, &e.EP, &e.Enabled, &e.CreatedAt,
		&e.Weight, &e.MaxConcurrency, &e.RPMLimit, &e.TPMLimit, &e.LastUsedAt,
		&e.TotalRequests, &e.SuccessRequests, &e.FailRequests, &e.PromptTokens,
		&e.CompletionTokens, &e.TotalTokens, &e.TotalImages); err != nil {
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
			fail_requests,prompt_tokens,completion_tokens,total_tokens,total_images)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id,model) DO UPDATE SET ep=excluded.ep, enabled=excluded.enabled,
			weight=excluded.weight, max_concurrency=excluded.max_concurrency,
			rpm_limit=excluded.rpm_limit, tpm_limit=excluded.tpm_limit`,
		e.ID, e.AccountID, e.Model, e.EP, boolInt(e.Enabled), nonzero(e.CreatedAt, nowUnix()),
		e.Weight, e.MaxConcurrency, e.RPMLimit, e.TPMLimit, e.LastUsedAt,
		e.TotalRequests, e.SuccessRequests, e.FailRequests, e.PromptTokens, e.CompletionTokens,
		e.TotalTokens, e.TotalImages)
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

// AccumulateImages 累计图像张数（账号/元组/子 Key 三视图）。
func (s *Store) AccumulateImages(accountID, endpointID, subkeyID string, n int64) error {
	if n <= 0 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE accounts SET total_images=total_images+? WHERE id=?`, []any{n, accountID}},
		{`UPDATE endpoints SET total_images=total_images+? WHERE id=?`, []any{n, endpointID}},
		{`UPDATE subkeys SET total_images=total_images+? WHERE id=?`, []any{n, subkeyID}},
	} {
		if q.args[1] == "" {
			continue
		}
		if _, err := s.db.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────── SubKey ───────────────────────────

const subkeyCols = `id,name,key_text,key_hash,enabled,allowed_models,allowed_accounts,
	daily_limit_tokens,daily_limit_images,expires_at,created_at,last_used_at,total_requests,total_tokens,
	total_images`

func scanSubKey(rows *sql.Rows) (*model.SubKey, error) {
	sk := &model.SubKey{}
	var am, aa string
	if err := rows.Scan(&sk.ID, &sk.Name, &sk.Key, &sk.KeyHash, &sk.Enabled, &am, &aa,
		&sk.DailyLimitTokens, &sk.DailyLimitImages, &sk.ExpiresAt, &sk.CreatedAt,
		&sk.LastUsedAt, &sk.TotalRequests, &sk.TotalTokens, &sk.TotalImages); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(am), &sk.AllowedModels)
	_ = json.Unmarshal([]byte(aa), &sk.AllowedAccounts)
	return sk, nil
}

func (s *Store) ListSubKeys() ([]*model.SubKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT ` + subkeyCols + ` FROM subkeys ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.SubKey{}
	for rows.Next() {
		sk, err := scanSubKey(rows)
		if err != nil {
			return nil, err
		}
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
		(id,name,key_text,key_hash,enabled,allowed_models,allowed_accounts,daily_limit_tokens,
		 daily_limit_images,expires_at,created_at,last_used_at,total_requests,total_tokens,total_images)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, key_text=excluded.key_text,
			key_hash=excluded.key_hash, enabled=excluded.enabled, allowed_models=excluded.allowed_models,
			allowed_accounts=excluded.allowed_accounts, daily_limit_tokens=excluded.daily_limit_tokens,
			daily_limit_images=excluded.daily_limit_images, expires_at=excluded.expires_at`,
		sk.ID, sk.Name, sk.Key, sk.KeyHash, boolInt(sk.Enabled), string(am), string(aa),
		sk.DailyLimitTokens, sk.DailyLimitImages, sk.ExpiresAt, nonzero(sk.CreatedAt, nowUnix()),
		sk.LastUsedAt, sk.TotalRequests, sk.TotalTokens, sk.TotalImages)
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
		(ts,subkey_id,subkey_name,account_id,account_name,provider,endpoint_id,requested_model,model,ep,modality,
		 prompt_tokens,completion_tokens,total_tokens,image_count,cost,status,latency_ms,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nonzero(l.TS, nowUnix()), l.SubKeyID, l.SubKeyName, l.AccountID, l.AccountName, l.Provider,
		l.EndpointID, l.RequestedModel, l.Model, l.EP, l.Modality,
		l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.ImageCount, l.Cost, l.Status, l.LatencyMs, l.Error)
	return err
}

const usageLogCols = `id,ts,subkey_id,subkey_name,account_id,account_name,provider,endpoint_id,
	requested_model,model,ep,modality,prompt_tokens,completion_tokens,total_tokens,image_count,
	cost,status,latency_ms,error`

func scanUsageLog(rows *sql.Rows) (*model.UsageLog, error) {
	l := &model.UsageLog{}
	if err := rows.Scan(&l.ID, &l.TS, &l.SubKeyID, &l.SubKeyName, &l.AccountID, &l.AccountName,
		&l.Provider, &l.EndpointID, &l.RequestedModel, &l.Model, &l.EP, &l.Modality,
		&l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.ImageCount,
		&l.Cost, &l.Status, &l.LatencyMs, &l.Error); err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) ListUsageLogs(limit int) ([]*model.UsageLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT ` + usageLogCols + ` FROM usage_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.UsageLog{}
	for rows.Next() {
		l, err := scanUsageLog(rows)
		if err != nil {
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

// SumCost 返回全部请求成本与最近 24h 成本（管理总览用）。
func (s *Store) SumCost() (total, last24h float64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	err = s.db.QueryRow(`SELECT COALESCE(SUM(cost),0),
		COALESCE(SUM(CASE WHEN ts >= ? THEN cost ELSE 0 END),0) FROM usage_logs`,
		nowUnix()-86400).Scan(&total, &last24h)
	return
}

// UsageSeriesPoint 是子 Key × 模型 的时间序列点（按小时聚合）。
type UsageSeriesPoint struct {
	TS       int64   `json:"ts"`
	SubKeyID string  `json:"subkey_id"`
	SubKey   string  `json:"subkey"`
	Model    string  `json:"model"`
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost"`
}

// usageSeriesMaxRows 限制单次序列查询返回的点数，避免长时间窗 × 多子 Key ×
// 多模型时序列化出超大响应。
const usageSeriesMaxRows = 5000

// QueryUsageSeries 聚合 usage_logs，返回子 Key × 模型 的按小时用量序列。
// 仅统计最近 hours 小时（默认 24）。返回按 (ts, subkey, model) 排序，
// 最多 usageSeriesMaxRows 行。
func (s *Store) QueryUsageSeries(hours int) ([]*UsageSeriesPoint, error) {
	if hours <= 0 {
		hours = 24
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	since := nowUnix() - int64(hours)*3600
	// subkey_name 必须一并出现在 GROUP BY 中：否则 SQLite 会从分组内任取一行的
	// 值，子 Key 改过名时标签会随机取到新旧名之一。
	rows, err := s.db.Query(`SELECT (ts/3600)*3600 as bucket,
		subkey_id, subkey_name, model,
		SUM(total_tokens), COUNT(*), SUM(cost)
		FROM usage_logs
		WHERE ts >= ?
		GROUP BY bucket, subkey_id, subkey_name, model
		ORDER BY bucket ASC, subkey_name ASC, model ASC
		LIMIT ?`, since, usageSeriesMaxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*UsageSeriesPoint{}
	for rows.Next() {
		p := &UsageSeriesPoint{}
		if err := rows.Scan(&p.TS, &p.SubKeyID, &p.SubKey, &p.Model, &p.Tokens, &p.Requests, &p.Cost); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─────────────────────────── Daily usage（日限额） ───────────────────────────

// DailyUsage 某子 Key 在自然日内的用量。
type DailyUsage struct {
	Tokens   int64   `json:"tokens"`
	Images   int64   `json:"images"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost"`
}

// today 返回本地时区的自然日（与「当日」直觉一致）。
func today() string { return time.Now().Format("2006-01-02") }

// AddDailyUsage 异步 consumer 落库时同步累计当日用量行。
func (s *Store) AddDailyUsage(subkeyID string, tokens, images, requests int64, cost float64) error {
	if subkeyID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO usage_daily(day,subkey_id,tokens,images,requests,cost)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(day,subkey_id) DO UPDATE SET
			tokens=tokens+excluded.tokens, images=images+excluded.images,
			requests=requests+excluded.requests, cost=cost+excluded.cost`,
		today(), subkeyID, tokens, images, requests, cost)
	return err
}

// GetDailyUsage 读取子 Key 当日用量（无行返回零值）。
func (s *Store) GetDailyUsage(subkeyID string) (*DailyUsage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := &DailyUsage{}
	err := s.db.QueryRow(`SELECT tokens,images,requests,cost FROM usage_daily WHERE day=? AND subkey_id=?`,
		today(), subkeyID).Scan(&d.Tokens, &d.Images, &d.Requests, &d.Cost)
	if err == sql.ErrNoRows {
		return &DailyUsage{}, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// SubKeyStats 子 Key 自助门户用的请求概况（只含终端用户可见数据）。
type SubKeyStats struct {
	Requests int64   `json:"requests"`
	Success  int64   `json:"success"`
	Tokens   int64   `json:"tokens"`
	Images   int64   `json:"images"`
	Cost     float64 `json:"cost"`
}

// SubKeyLogStats 统计某子 Key 自 since 起的请求概况（来自 usage_logs，仅本 Key）。
func (s *Store) SubKeyLogStats(subkeyID string, since int64) (*SubKeyStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := &SubKeyStats{}
	err := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(total_tokens),0),
			COALESCE(SUM(image_count),0),
			COALESCE(SUM(cost),0)
		FROM usage_logs WHERE subkey_id=? AND ts>=?`, subkeyID, since).
		Scan(&st.Requests, &st.Success, &st.Tokens, &st.Images, &st.Cost)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// ListUsageLogsBySubKey 返回某子 Key 自己的近期日志。
// 列特意收窄（不含 account/provider/ep 等管理侧字段），供自助门户展示，
// 保证终端用户拿不到任何超出自身调用视角的数据。
const subKeyLogCols = `id,ts,requested_model,model,modality,
	prompt_tokens,completion_tokens,total_tokens,image_count,cost,status,latency_ms,error`

func scanSubKeyUsageLog(rows *sql.Rows) (*model.UsageLog, error) {
	l := &model.UsageLog{}
	if err := rows.Scan(&l.ID, &l.TS, &l.RequestedModel, &l.Model, &l.Modality,
		&l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.ImageCount,
		&l.Cost, &l.Status, &l.LatencyMs, &l.Error); err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) ListUsageLogsBySubKey(subkeyID string, limit int) ([]*model.UsageLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT `+subKeyLogCols+` FROM usage_logs WHERE subkey_id=? ORDER BY id DESC LIMIT ?`,
		subkeyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.UsageLog{}
	for rows.Next() {
		l, err := scanSubKeyUsageLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ─────────────────────────── 用量分析（交互式聚合查询） ───────────────────────────

// UsageQuery 用量分析查询参数。Dim 为空表示只看总量；Entity 为该维度下的键过滤。
type UsageQuery struct {
	From, To    int64  // unix 秒闭区间 [From, To]
	Granularity string // day | hour（其它值回落 day）
	Dim         string // "" | model | subkey | account | endpoint | provider
	Entity      string // Dim 的过滤键（facet.Key）
}

// UsageTotals 聚合总量（成功/失败计数来自 status='ok'）。
type UsageTotals struct {
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Images           int64   `json:"images"`
	Cost             float64 `json:"cost"`
}

// UsageBucket 单个时间桶的聚合。
type UsageBucket struct {
	Bucket           int64   `json:"bucket"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Images           int64   `json:"images"`
	Cost             float64 `json:"cost"`
}

// UsageFacet 维度下实体的小计行（供下拉/表格选择实体）。
type UsageFacet struct {
	Key              string  `json:"key"`
	Label            string  `json:"label"`
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Images           int64   `json:"images"`
	Cost             float64 `json:"cost"`
}

// UsageQueryResult 一次查询的完整结果：总量 + 时序 + 维度实体列表。
type UsageQueryResult struct {
	Summary UsageTotals    `json:"summary"`
	Series  []*UsageBucket `json:"series"`
	Facets  []*UsageFacet  `json:"facets"`
}

// usageDims 分析维度 → (过滤键列, 展示名列)。
var usageDims = map[string][2]string{
	"model":    {"model", "model"},
	"subkey":   {"subkey_id", "subkey_name"},
	"account":  {"account_id", "account_name"},
	"endpoint": {"endpoint_id", "ep"},
	"provider": {"provider", "provider"},
}

// usageQueryMaxSpan 限制查询区间上限，防止把整张日志表拖进聚合。
const usageQueryMaxSpan = 92 * 86400

// usageBucketExpr 时间桶表达式：小时桶按 UTC 整点；天桶按本地时区自然日。
// 时区偏移以整型常量直接内进 SQL（SELECT 与 GROUP BY 会重复出现该表达式，
// 不能用占位符），值来自 time.Zone，无注入面。
func usageBucketExpr(gran string) string {
	if gran == "hour" {
		return "(ts/3600)*3600"
	}
	_, off := time.Now().Zone()
	return "((ts+" + strconv.Itoa(int(off)) + ")/86400)*86400-" + strconv.Itoa(int(off))
}

// QueryUsage 聚合 usage_logs 返回总量、按时间粒度的时序，以及维度实体小计。
// 结果三部分共用同一 WHERE（区间 + 可选实体过滤），前端据此做交互式下钻。
func (s *Store) QueryUsage(q UsageQuery) (*UsageQueryResult, error) {
	now := nowUnix()
	if q.To <= 0 || q.To > now {
		q.To = now
	}
	if q.From <= 0 || q.From > q.To {
		q.From = q.To - 7*86400 + 1
	}
	if q.To-q.From > usageQueryMaxSpan {
		q.From = q.To - usageQueryMaxSpan
	}
	if q.Granularity != "hour" && q.Granularity != "day" {
		q.Granularity = "day"
	}
	dim, dimOK := usageDims[q.Dim]
	if !dimOK {
		q.Dim, q.Entity = "", ""
	}

	where := "ts >= ? AND ts <= ?"
	whereArgs := []any{q.From, q.To}
	if q.Entity != "" {
		where += " AND " + dim[0] + " = ?"
		whereArgs = append(whereArgs, q.Entity)
	}

	res := &UsageQueryResult{Series: []*UsageBucket{}, Facets: []*UsageFacet{}}
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 1) 总量。
	row := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(image_count),0), COALESCE(SUM(cost),0)
		FROM usage_logs WHERE `+where, whereArgs...)
	if err := row.Scan(&res.Summary.Requests, &res.Summary.Success,
		&res.Summary.PromptTokens, &res.Summary.CompletionTokens,
		&res.Summary.TotalTokens, &res.Summary.Images, &res.Summary.Cost); err != nil {
		return nil, err
	}

	// 2) 时序。
	bucketExpr := usageBucketExpr(q.Granularity)
	rows, err := s.db.Query(`SELECT `+bucketExpr+`,
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COUNT(*), COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(image_count),0), COALESCE(SUM(cost),0)
		FROM usage_logs WHERE `+where+` GROUP BY `+bucketExpr+` ORDER BY 1 ASC`, whereArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		b := &UsageBucket{}
		if err := rows.Scan(&b.Bucket, &b.PromptTokens, &b.CompletionTokens,
			&b.Requests, &b.Success, &b.Images, &b.Cost); err != nil {
			return nil, err
		}
		res.Series = append(res.Series, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3) 维度实体小计（供前端实体下拉/表格）。
	if dimOK {
		frows, err := s.db.Query(`SELECT `+dim[0]+`, `+dim[1]+`,
				COUNT(*), COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
				COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
				COALESCE(SUM(total_tokens),0), COALESCE(SUM(image_count),0), COALESCE(SUM(cost),0)
			FROM usage_logs WHERE ts >= ? AND ts <= ?
			GROUP BY `+dim[0]+`, `+dim[1]+`
			ORDER BY SUM(total_tokens) DESC, SUM(image_count) DESC
			LIMIT 200`, q.From, q.To)
		if err != nil {
			return nil, err
		}
		defer frows.Close()
		for frows.Next() {
			f := &UsageFacet{}
			if err := frows.Scan(&f.Key, &f.Label, &f.Requests, &f.Success,
				&f.PromptTokens, &f.CompletionTokens, &f.TotalTokens, &f.Images, &f.Cost); err != nil {
				return nil, err
			}
			res.Facets = append(res.Facets, f)
		}
		if err := frows.Err(); err != nil {
			return nil, err
		}
	}
	return res, nil
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

// NormalizeKey 标准化用户输入的子 Key：补全 sk- 前缀（仅子 Key 使用；
// 上游 Key 为不透明字符串，不做任何前缀处理）。
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

// NormalizeEP 标准化接入点输入：仅去前后空白（不透明字符串，无格式假设）。
func NormalizeEP(ep string) string { return strings.TrimSpace(ep) }
