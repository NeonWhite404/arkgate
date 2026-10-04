package store

import (
	"database/sql"
	"encoding/json"
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

// TestAuditMigrateV14ToV15：v14 → v15（子 Key 配额周期 + 缓存倍率）升级路径。
//
// 这是本仓库最需要警惕的一类迁移：**改动的是「限额」字段**。
// 若新列默认值不是「等价旧行为」，升级瞬间所有存量子 Key 的额度语义会变，
// 表现为「升级后用户突然被限流」或「额度突然翻倍」——两者都是生产事故。
//
// 因此断言三点：
//  1. 旧行的限额字段一字不改（daily_limit_tokens/images）；
//  2. 新列取零值，且零值经 model.QuotaRuleOf 解析后 == 旧行为（自然日 + 未加权）；
//  3. usage_daily.window_secs 默认 0（历史行，等价「日」），旧用量不被改写。
func TestAuditMigrateV14ToV15(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "arkgate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 构造 v14 subkeys 表：有 daily_limit_*，缺 quota_* / cache_*_permille /
	// window_limit_requests。
	for _, st := range []string{
		`CREATE TABLE subkeys (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '',
			key_text TEXT NOT NULL DEFAULT '', key_hash TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1, allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_accounts TEXT NOT NULL DEFAULT '[]',
			daily_limit_tokens INTEGER NOT NULL DEFAULT 0,
			daily_limit_images INTEGER NOT NULL DEFAULT 0,
			expires_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			total_images INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX idx_subkeys_hash ON subkeys(key_hash)`,
		`INSERT INTO subkeys(id,name,key_text,key_hash,enabled,daily_limit_tokens,daily_limit_images)
			VALUES('sk_old','旧Key','sk-aaa','hash-a',1,1000000,50)`,
		// v14 的 usage_daily（没有 window_secs 列）。
		`CREATE TABLE usage_daily (day TEXT NOT NULL, subkey_id TEXT NOT NULL,
			tokens INTEGER NOT NULL DEFAULT 0, images INTEGER NOT NULL DEFAULT 0,
			requests INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (day, subkey_id))`,
		`INSERT INTO usage_daily(day,subkey_id,tokens,images,requests,cost)
			VALUES('2026-01-01','sk_old',12345,3,7,0.5)`,
	} {
		if _, err := db.Exec(st); err != nil {
			t.Fatalf("build v14: %v", err)
		}
	}
	_ = db.Close()

	st, err := New(dir)
	if err != nil {
		t.Fatalf("v14 库升级失败: %v", err)
	}
	defer st.Close()

	sk, err := st.GetSubKeyByHash("hash-a")
	if err != nil {
		t.Fatalf("读回子 Key: %v", err)
	}
	// ① 旧限额字段一字不改。
	if sk.DailyLimitTokens != 1000000 || sk.DailyLimitImages != 50 {
		t.Fatalf("旧限额被改动: tokens=%d images=%d", sk.DailyLimitTokens, sk.DailyLimitImages)
	}
	// ② 新列取零值（= 未设置）。
	if sk.QuotaPeriod != "" || sk.QuotaResetWeekday != 0 || sk.QuotaResetHour != 0 ||
		sk.CacheReadPermille != 0 || sk.CacheWritePermille != 0 || sk.WindowLimitRequests != 0 {
		t.Fatalf("新增配额列应为零值: %+v", sk)
	}
	// ③ 零值经规则解析后必须等价旧行为。
	//    最关键的是 weekday 默认：DB 默认 0（未设置）→ 解析为 ISO 周一(1)。
	//    若这里写成 time.Weekday 的 0=周日，周周期会在升级后悄悄偏移一天。
	rule := model.QuotaRuleOf(sk)
	if rule.Period != model.QuotaPeriodDay {
		t.Fatalf("零值周期应解析为 day, got %q", rule.Period)
	}
	// **零值倍率必须解析为「不加权」**，而不是默认倍率。
	//
	// 这是升级安全的关键：若零值解析成 0.1x/1.25x，旧子 Key 的配额消耗
	// 会静默变小（实测 1170 vs 2200），等于扩大额度。加权必须是显式选择。
	if rule.Weight {
		t.Fatalf("零值倍率不应启用加权（会静默放宽旧子 Key 的额度）")
	}
	// 旧口径计数的直接证据：即使上游上报了缓存 token，也必须按总数算。
	if got := model.QuotaCount(rule, 1000, 100, 600, 100); got != 1100 {
		t.Fatalf("迁移后旧子 Key 应仍按原口径记 1100, got %d", got)
	}
	if rule.ResetWeekday != 1 {
		t.Fatalf("零值 weekday 应解析为周一(1), got %d", rule.ResetWeekday)
	}

	// ④ 历史用量行不被改写，且 window_secs 默认 0（等价「日」）。
	//
	// 必须用 GetWindowUsage 指定那一行的 day 去读——GetDailyUsage 固定读「今天」，
	// 而 fixture 的历史行在 2026-01-01，用它只会读到空行（初版就是这么写错的，
	// 得到「历史用量被改写」的假失败）。
	du, err := st.GetWindowUsage("sk_old", "2026-01-01")
	if err != nil {
		t.Fatalf("读历史用量: %v", err)
	}
	if du.Tokens != 12345 || du.Images != 3 || du.Requests != 7 {
		t.Fatalf("历史用量被改写: %+v", du)
	}
	// window_secs 对历史行应为 0（迁移默认值），表示「等价于日」。
	var ws int64
	if err := func() error {
		st.mu.RLock()
		defer st.mu.RUnlock()
		return st.db.QueryRow(`SELECT window_secs FROM usage_daily WHERE day='2026-01-01' AND subkey_id='sk_old'`).Scan(&ws)
	}(); err != nil {
		t.Fatalf("读 window_secs: %v", err)
	}
	if ws != 0 {
		t.Fatalf("历史行 window_secs 应为 0（等价日）, got %d", ws)
	}
	// ⑤ 升级后能写入并读回新字段。
	sk.QuotaPeriod = model.QuotaPeriodMonth
	sk.CacheReadPermille = 500
	sk.WindowLimitRequests = 999
	if err := st.UpsertSubKey(sk); err != nil {
		t.Fatalf("保存新配额字段: %v", err)
	}
	got, err := st.GetSubKeyByHash("hash-a")
	if err != nil {
		t.Fatalf("再读: %v", err)
	}
	if got.QuotaPeriod != model.QuotaPeriodMonth || got.CacheReadPermille != 500 ||
		got.WindowLimitRequests != 999 {
		t.Fatalf("配额字段未持久化: %+v", got)
	}
	// 旧字段仍保留（部分更新不得互相清零）。
	if got.DailyLimitTokens != 1000000 || got.DailyLimitImages != 50 {
		t.Fatalf("保存新字段后旧限额被清零: %+v", got)
	}
}

// TestAuditMigrateV15ToV16 v15 → v16：新增 endpoints.default_body_params 列。
//
// 升级安全要求（与历次迁移一致）：
//   - 旧行的配置一个字都不变（尤其 request_headers / 流控字段不能被清零）；
//   - 新列取零值（空对象），语义等价「未配置默认参数」= 升级前行为；
//   - 升级后能正常读写新列。
func TestAuditMigrateV15ToV16(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "arkgate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// v15 的 endpoints：有 request_headers，缺 default_body_params。
	for _, st := range []string{
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
		// 一行「配得很满」的旧接入点：升级后这些值必须一字不变。
		`INSERT INTO endpoints(id,account_id,model,ep,enabled,weight,max_concurrency,rpm_limit,
			tpm_limit,request_headers,total_requests,total_tokens)
			VALUES('ep_old','acc_1','m','ep-upstream',1,7,11,120,90000,
			'{"User-Agent":"claude-cli/1.0.119"}',42,12345)`,
	} {
		if _, err := db.Exec(st); err != nil {
			t.Fatalf("build v15: %v", err)
		}
	}
	_ = db.Close()

	st, err := New(dir)
	if err != nil {
		t.Fatalf("v15 库升级失败: %v", err)
	}
	defer st.Close()

	ep, err := st.GetEndpoint("ep_old")
	if err != nil {
		t.Fatalf("读回接入点: %v", err)
	}
	// ① 既有配置逐项不变。
	if ep.Weight != 7 || ep.MaxConcurrency != 11 || ep.RPMLimit != 120 || ep.TPMLimit != 90000 {
		t.Fatalf("流控字段被迁移改动: %+v", ep)
	}
	if ep.RequestHeaders["User-Agent"] != "claude-cli/1.0.119" {
		t.Fatalf("request_headers 被改动: %+v", ep.RequestHeaders)
	}
	if ep.TotalRequests != 42 || ep.TotalTokens != 12345 {
		t.Fatalf("统计被改动: req=%d tok=%d", ep.TotalRequests, ep.TotalTokens)
	}
	// ② 新列取零值（= 未配置 → 不注入任何参数，行为与升级前一致）。
	if len(ep.DefaultBodyParams) != 0 {
		t.Fatalf("新列应为空（等价未配置）, got %+v", ep.DefaultBodyParams)
	}
	// ③ 升级后能写入并读回（保真）。
	ep.DefaultBodyParams = map[string]json.RawMessage{
		"persona": json.RawMessage(`"Virginia Woolf"`),
		"big":     json.RawMessage(`12345678901234567890`),
	}
	if err := st.UpsertEndpoint(ep); err != nil {
		t.Fatalf("保存新字段: %v", err)
	}
	got, err := st.GetEndpoint("ep_old")
	if err != nil {
		t.Fatalf("再读: %v", err)
	}
	if string(got.DefaultBodyParams["persona"]) != `"Virginia Woolf"` {
		t.Fatalf("新字段未持久化: %+v", got.DefaultBodyParams)
	}
	if string(got.DefaultBodyParams["big"]) != `12345678901234567890` {
		t.Fatalf("大整数精度丢失: %s", got.DefaultBodyParams["big"])
	}
	// ④ 保存新字段不得连带改动既有配置（部分更新语义）。
	if got.RequestHeaders["User-Agent"] != "claude-cli/1.0.119" || got.TPMLimit != 90000 {
		t.Fatalf("保存新字段后既有配置被改动: %+v", got)
	}
}

// TestAuditMigrateV16Idempotent 重复打开同一库不改变任何东西（幂等）。
func TestAuditMigrateV16Idempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("首次打开: %v", err)
	}
	ep := &model.Endpoint{
		ID: "e1", AccountID: "a", Model: "m", EP: "p", Enabled: true,
		DefaultBodyParams: map[string]json.RawMessage{"persona": json.RawMessage(`"x"`)},
	}
	if err := st.UpsertEndpoint(ep); err != nil {
		t.Fatalf("写入: %v", err)
	}
	st.Close()

	// 再次打开（迁移会重跑一遍判重逻辑）。
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("二次打开: %v", err)
	}
	defer st2.Close()
	got, err := st2.GetEndpoint("e1")
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if string(got.DefaultBodyParams["persona"]) != `"x"` {
		t.Fatalf("幂等重跑把数据搞坏了: %+v", got.DefaultBodyParams)
	}
}
