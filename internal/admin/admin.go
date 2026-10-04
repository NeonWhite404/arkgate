// Package admin 提供 /api/* 管理接口与访问令牌鉴权。
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"arkgate/internal/balancer"
	"arkgate/internal/catalog"
	"arkgate/internal/config"
	"arkgate/internal/model"
	"arkgate/internal/provider"
	"arkgate/internal/secure"
	"arkgate/internal/store"
)

// Admin 管理后端。
type Admin struct {
	store   *store.Store
	box     *secure.Box
	bal     *balancer.Balancer
	cfg     *config.Config    // 运行时配置（超时热改写入其原子字段）
	catalog *catalog.Catalog  // 内置模型元数据目录（价格/能力自动补全来源）
	mgr     *provider.Manager // 仅用于「拉取上游模型列表」这类管理侧探测
	handler http.Handler
	// usageCache 用量分析查询结果缓存（含单飞）。前端点 facet 下钻是最高频操作，
	// 同一组参数短时间内会被反复请求，缓存收益最明显。
	usageCache *store.UsageCache
}

const tokenHashKey = "admin_token_hash"

// 运行时设置的持久化键（settings 表，值为秒；"0" = 关闭该超时）。
// maxCurrencyRate 汇率上限。不做精确的经济学约束，只是挡住明显的手滑
// （例如把 7.2 打成 7200000）与负值。
const maxCurrencyRate = 1e6

// maxQuotaPermille 缓存倍率上限（千分比）。100000 = 100x，纯属防手滑，
// 不是经济学约束——真实缓存倍率在 0.004~4.0 之间（见目录快照实测）。
const maxQuotaPermille = 100000

const (
	keyTimeoutRequest    = "timeout_request_sec"
	keyTimeoutFirstToken = "timeout_first_token_sec"
	// 计价币种：单价与成本始终以美元存储，这里只存「展示用」的汇率与符号。
	keyCurrencyRate   = "currency_rate"
	keyCurrencySymbol = "currency_symbol"
	keyCurrencyCode   = "currency_code"
)

// New 构造管理后端，并把已持久化的运行时设置应用到 cfg
// （优先级：DB 持久化值 > 环境变量 > 内置默认）。
func New(st *store.Store, box *secure.Box, bal *balancer.Balancer, cfg *config.Config) *Admin {
	a := &Admin{store: st, box: box, bal: bal, cfg: cfg, catalog: catalog.New(),
		mgr: provider.NewManager(), usageCache: store.NewUsageCache(store.UsageCacheTTL)}
	a.loadPersistedTimeouts()
	a.loadPersistedCurrency()
	a.handler = a.routes()
	return a
}

// loadPersistedTimeouts 启动时把 DB 里的超时设置覆盖到 cfg（未设置过则保留环境变量/默认值）。
func (a *Admin) loadPersistedTimeouts() {
	if v, ok := a.store.GetSetting(keyTimeoutRequest); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			a.cfg.Timeouts.SetRequest(time.Duration(f * float64(time.Second)))
		}
	}
	if v, ok := a.store.GetSetting(keyTimeoutFirstToken); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			a.cfg.Timeouts.SetFirstToken(time.Duration(f * float64(time.Second)))
		}
	}
}

// loadPersistedCurrency 启动时把 DB 里的币种设置覆盖到 cfg（未设置过则保留环境变量/默认值）。
//
// 只覆盖显式设置过的键：若用户只填了汇率没填符号，符号应保持环境变量/默认值，
// 而不是被清空成 ""。
func (a *Admin) loadPersistedCurrency() {
	rate := a.cfg.Currency.Rate()
	symbol := a.cfg.Currency.Symbol()
	code := a.cfg.Currency.Code()
	if v, ok := a.store.GetSetting(keyCurrencyRate); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			rate = f
		}
	}
	if v, ok := a.store.GetSetting(keyCurrencySymbol); ok && v != "" {
		symbol = v
	}
	if v, ok := a.store.GetSetting(keyCurrencyCode); ok && v != "" {
		code = v
	}
	a.cfg.Currency.Set(rate, symbol, code)
}

// Handler 返回 http.Handler。
func (a *Admin) Handler() http.Handler { return a.handler }

func isTokenHash(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

func (a *Admin) isInitialized() bool {
	_, ok := a.store.GetSetting(tokenHashKey)
	return ok
}

func (a *Admin) verifyToken(tok string) bool {
	stored, ok := a.store.GetSetting(tokenHashKey)
	if !ok {
		return false
	}
	// 用**定时安全**比较。
	//
	// 坦白说明这个修改的实际价值（审计时曾把理由写夸大了，实测后修正）：
	// 这不是一个可利用的漏洞——服务端对提交的 token 先做 sha256 再比较，
	// 攻击者只能控制 token、控制不了哈希，想命中哈希前缀就得求 sha256 原像；
	// 且 Go 的字符串 == 先比长度，长度不同根本进不了 memcmp。
	// 实测两种写法在 HTTP 层耗时 255~323µs、波动完全来自网络与调度，
	// 与比较方式无相关性。
	//
	// 保留修改的理由是「防将来」：避免后续有人改成对明文比较、或改成逐字符
	// 循环时引入真实侧信道；且这是敏感比较的社区默认写法，成本为零。
	return subtle.ConstantTimeCompare([]byte(isTokenHash(tok)), []byte(stored)) == 1
}

// EnsureAdminToken 首次运行时若无令牌则生成一个并打印到控制台。
// 返回「是否为新生成」。
func (a *Admin) EnsureAdminToken() (token string, created bool) {
	if a.isInitialized() {
		return "", false
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	token = "ark-" + hex.EncodeToString(buf)
	_ = a.store.SetSetting(tokenHashKey, isTokenHash(token))
	return token, true
}

func (a *Admin) routes() http.Handler {
	mux := http.NewServeMux()

	// 免鉴权。
	mux.HandleFunc("/api/auth/status", a.handleStatus)
	mux.HandleFunc("/api/auth/setup", a.handleSetup)
	mux.HandleFunc("/api/auth/login", a.handleLogin)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok"})
	})

	// 需鉴权。
	type hf func(w http.ResponseWriter, r *http.Request)
	reg := func(path string, h hf) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !a.verifyToken(bearer(r)) {
				writeJSON(w, 401, map[string]any{"detail": "未授权", "code": "UNAUTHORIZED"})
				return
			}
			h(w, r)
		})
	}

	reg("/api/auth/info", a.handleInfo)
	reg("/api/providers", a.handleProviders)
	reg("/api/settings/runtime", a.handleRuntimeSettings)
	reg("/api/accounts", a.handleAccountsCollection)
	reg("/api/accounts/", a.handleAccountItem)
	reg("/api/models", a.handleModelsCollection)
	reg("/api/models/", a.handleModelItem)
	reg("/api/models/metadata-sync", a.handleMetadataSync)
	reg("/api/catalog/lookup", a.handleCatalogLookup)
	reg("/api/upstream/models", a.handleUpstreamModels)
	reg("/api/test/model", a.handleTestModel)
	reg("/api/endpoints", a.handleEndpointsCollection)
	reg("/api/endpoints/", a.handleEndpointItem)
	reg("/api/subkeys", a.handleSubkeysCollection)
	reg("/api/subkeys/", a.handleSubkeyItem)
	reg("/api/logs", a.handleLogs)
	reg("/api/stats", a.handleStats)
	reg("/api/overview", a.handleOverview)
	reg("/api/usage/series", a.handleUsageSeries)
	reg("/api/usage/stats", a.handleUsageStats)
	reg("/api/usage/rollup", a.handleUsageRollup)
	reg("/api/usage/rollup/rebuild", a.handleUsageRollupRebuild)
	reg("/api/usage/cost/backfill", a.handleCostBackfill)

	return mux
}

// handleCostBackfill 按当前定价重算历史成本（v14 缓存计费修正后的回填）。
//
// 为什么不放进迁移自动跑：迁移必须快且无副作用，而回填可能扫数百万行。
// 更重要的是「是否重算」是业务决定——历史账单若已对外结算，重算会改变已出账的数字。
// 所以做成显式触发。
//
// 参数：from/to（unix 秒，默认最近 7 天）、dry_run=1 只统计不写。
// 区间上限沿用 92 天（与 rollup 重建一致，防止一次请求把库拖死）。
//
// 返回前后总额对比，便于判断影响面。
func (a *Admin) handleCostBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	qv := r.URL.Query()
	from, _ := strconv.ParseInt(qv.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(qv.Get("to"), 10, 64)
	now := time.Now().Unix()
	if to <= 0 || to > now {
		to = now
	}
	if from <= 0 || from >= to {
		from = to - 7*86400
	}
	const maxSpan = 92 * 86400
	if to-from > maxSpan {
		writeJSON(w, 400, map[string]any{"detail": "区间过大（上限 92 天）"})
		return
	}
	dryRun := qv.Get("dry_run") == "1" || qv.Get("dry_run") == "true"

	res, err := a.store.BackfillCosts(from, to, a.bal.CostForBackfill, dryRun)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	// 非 dry-run 写入后，预聚合表里的成本已过期——必须重算，否则用量分析
	// 默认走聚合表会继续显示旧数字（“回填了但界面没变”的假象）。
	// 失败不当作整体失败：原始表已经正确，只是聚合视图滞后。
	rollupRows, rollupErr := int64(0), ""
	if !dryRun {
		if n, err := a.store.RebuildRollup(from, to); err != nil {
			rollupErr = err.Error()
		} else {
			rollupRows = n
		}
	}
	// 顺带修复日用量表的成本列。
	//
	// 为什么放在同一个端点而不是新开一个：usage_daily.cost 与 usage_logs.cost
	// 是两条独立写入路径，历史上出现过「重构漏填统计字段 → 日成本恒 0，
	// 而日志成本正常」的缺陷（门户「今日成本」显示 $0 但周/总正常）。
	// 运维不需要记住跑两个地方——修成本就该一次修干净。
	//
	// 与主回填的区别：本步**不依赖模型定价**（直接加总日志里的 cost），
	// 因此模型已被删除时也能修；且只改成本列，不动 tokens/images/requests
	// （那些一直是对的，重算反而可能在日志被清理后把数字改小）。
	dailyFixed, dailyErr := a.store.RepairDailyCosts(from, to, dryRun)
	out := map[string]any{
		"success":     rollupErr == "",
		"from":        from,
		"to":          to,
		"scanned":     res.Scanned,
		"updated":     res.Updated,
		"unpriced":    res.Unpriced,
		"old_total":   res.OldTotal,
		"new_total":   res.NewTotal,
		"delta":       res.NewTotal - res.OldTotal,
		"dry_run":     res.DryRun,
		"rollup_rows": rollupRows,
		// daily_fixed：被修正的（日 × 子Key）行数。修成本历史时用得上。
		"daily_fixed": dailyFixed,
	}
	if rollupErr != "" {
		out["rollup_error"] = rollupErr
	}
	if dailyErr != nil {
		out["daily_error"] = dailyErr.Error()
	}
	writeJSON(w, 200, out)
}

// handleUsageRollup 预聚合健康度（watermark / 滞后秒数 / 聚合表行数）。
// 供管理端展示「统计延迟」——滞后持续变大说明聚合器卡住，但查询仍会回落原始表。
func (a *Admin) handleUsageRollup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	h, err := a.store.RollupHealthReport(time.Now().Unix())
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, 200, h)
}

// handleUsageRollupRebuild 在指定区间重算预聚合（修复/回填）。
//
// 用途：改价后重算成本、时区修正、或发现口径问题后重建。**不推进水位**——
// 重建历史不应影响实时聚合进度。
//
// 参数：from/to 为 unix 秒（省略时默认最近 7 天）。区间上限沿用查询上限（92 天），
// 避免一次请求把整库拖死（SQLite 单写者，长事务会阻塞转发热路径的统计落库）。
func (a *Admin) handleUsageRollupRebuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	qv := r.URL.Query()
	from, _ := strconv.ParseInt(qv.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(qv.Get("to"), 10, 64)
	now := time.Now().Unix()
	if to <= 0 || to > now {
		to = now
	}
	if from <= 0 || from >= to {
		from = to - 7*86400
	}
	const maxSpan = 92 * 86400
	if to-from > maxSpan {
		writeJSON(w, 400, map[string]any{"detail": "区间过大（上限 92 天）"})
		return
	}
	rows, err := a.store.RebuildRollup(from, to)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "rows": rows, "from": from, "to": to})
}

// handleUsageStats 交互式用量分析查询：区间 + 粒度（day/hour）+ 维度（模型/子Key/
// 账号/接入点/供应商）+ 实体过滤，一次返回总量、时序与维度实体小计（对齐火山方舟
// 「用量统计」的交互形态）。
func (a *Admin) handleUsageStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	qv := r.URL.Query()
	toInt := func(key string) int64 {
		n, _ := strconv.ParseInt(qv.Get(key), 10, 64)
		return n
	}
	q := store.UsageQuery{
		From:        toInt("from"),
		To:          toInt("to"),
		Granularity: qv.Get("gran"),
		Dim:         qv.Get("dim"),
		Entity:      qv.Get("entity"),
	}
	// nocache=1 绕过缓存（排查「数对不对」时必须能拿到实时值，
	// 否则缓存会让人怀疑是数据问题而不是缓存问题）。
	if qv.Get("nocache") == "1" {
		res, err := a.store.QueryUsage(q)
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		w.Header().Set("X-Usage-Cache", "bypass")
		writeJSON(w, 200, res)
		return
	}
	// 缓存键 = 全部查询参数的规范化拼接。带 entity 与 dim，否则下钻会串味。
	key := fmt.Sprintf("%d|%d|%s|%s|%s", q.From, q.To, q.Granularity, q.Dim, q.Entity)
	res, loaded, err := a.usageCache.GetOrLoad(key, func() (*store.UsageQueryResult, error) {
		return a.store.QueryUsage(q)
	})
	if err != nil || res == nil {
		writeJSON(w, 500, map[string]any{"detail": "用量查询失败"})
		return
	}
	if loaded {
		w.Header().Set("X-Usage-Cache", "miss")
	} else {
		w.Header().Set("X-Usage-Cache", "hit")
	}
	writeJSON(w, 200, res)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return r.Header.Get("X-Auth-Token")
}

// ─────────────────────────── auth ───────────────────────────

func (a *Admin) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"initialized": a.isInitialized()})
}

func (a *Admin) handleSetup(w http.ResponseWriter, r *http.Request) {
	if a.isInitialized() {
		writeJSON(w, 409, map[string]any{"detail": "已初始化"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req); err != nil || len(req.Token) < 6 {
		writeJSON(w, 400, map[string]any{"detail": "令牌长度至少 6 位"})
		return
	}
	_ = a.store.SetSetting(tokenHashKey, isTokenHash(req.Token))
	writeJSON(w, 200, map[string]any{"success": true})
}

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"detail": "无效请求"})
		return
	}
	if !a.isInitialized() {
		writeJSON(w, 412, map[string]any{"detail": "尚未初始化"})
		return
	}
	if !a.verifyToken(req.Token) {
		writeJSON(w, 401, map[string]any{"detail": "令牌不正确"})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "token": req.Token})
}

func (a *Admin) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"initialized": a.isInitialized()})
}

// ─────────────────────────── providers ───────────────────────────

// handleProviders 返回供应商注册表（前端下拉与能力展示用）。
func (a *Admin) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, provider.List())
}

// ─────────────────────────── accounts ───────────────────────────

type accountPayload struct {
	ID string `json:"id"`
	// 上游 API Key 为不透明字符串：仅 TrimSpace，不校验前缀/格式。
	// api_key 是新字段名；ark_api_key 为兼容旧前端的别名。
	APIKey    string `json:"api_key"`
	ArkAPIKey string `json:"ark_api_key"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Weight    int    `json:"weight"`

	Provider string `json:"provider"` // ark | openai | custom…
	BaseURL  string `json:"base_url"` // 覆盖 provider 默认 base
	// 能力三态覆盖：0 继承 provider 默认，1 强制是，-1 强制否。
	// 指针：未出现的字段不覆盖（默认 0 继承）。
	CapResponses *int `json:"cap_responses"`
	CapImages    *int `json:"cap_images"`
}

func (p *accountPayload) key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	return p.ArkAPIKey
}

// validateProvider 校验 payload 的 provider/base_url/能力覆盖，返回规范化后的值。
func (p *accountPayload) validateProvider() (def provider.Def, baseURL string, caps [2]int, err error) {
	id := strings.TrimSpace(p.Provider)
	if id == "" {
		id = "ark" // 兼容旧前端
	}
	d, ok := provider.Get(id)
	if !ok {
		return d, "", [2]int{}, errors.New("未知的供应商 " + id)
	}
	baseURL = provider.NormalizeBaseURL(p.BaseURL)
	if baseURL != "" && !provider.IsHTTPURL(baseURL) {
		return d, "", [2]int{}, errors.New("base_url 必须是 http(s) 地址")
	}
	if d.DefaultBaseURL == "" && baseURL == "" {
		return d, "", [2]int{}, errors.New("该供应商需要填写 base_url")
	}
	for i, c := range []*int{p.CapResponses, p.CapImages} {
		v := 0
		if c != nil {
			v = *c
		}
		if v != -1 && v != 0 && v != 1 {
			return d, "", [2]int{}, errors.New("能力覆盖仅支持 0（继承）/ 1（是）/ -1（否）")
		}
		caps[i] = v
	}
	return d, baseURL, caps, nil
}

func (a *Admin) handleAccountsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := a.store.ListAccounts()
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, all)
	case http.MethodPost:
		var p accountPayload
		if err := decode(r, &p); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		if p.Name == "" || p.key() == "" {
			writeJSON(w, 400, map[string]any{"detail": "name 与 api_key 必填"})
			return
		}
		def, baseURL, caps, verr := p.validateProvider()
		if verr != nil {
			writeJSON(w, 400, map[string]any{"detail": verr.Error()})
			return
		}
		id := p.ID
		if id == "" {
			id = "acc_" + randHex(6)
		}
		key := strings.TrimSpace(p.key())
		enc, err := a.box.Encrypt(key)
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		acc := &model.Account{
			ID: id, Name: p.Name, ArkAPIKeyEnc: enc,
			KeyHint:      secure.Mask(key),
			Status:       defaultStr(p.Status, model.AccountActive),
			Weight:       defaultInt(p.Weight, 1),
			Provider:     def.ID,
			BaseURL:      baseURL,
			CapResponses: caps[0],
			CapImages:    caps[1],
		}
		if err := a.store.UpsertAccount(acc); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true, "id": id})
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

func (a *Admin) handleAccountItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	if id == "" {
		writeJSON(w, 400, map[string]any{"detail": "缺少 id"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		var p accountPayload
		if err := decode(r, &p); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		acc, err := a.store.GetAccount(id)
		if err != nil {
			writeJSON(w, 404, map[string]any{"detail": "账号不存在"})
			return
		}
		if p.Name != "" {
			acc.Name = p.Name
		}
		if p.Status != "" {
			acc.Status = p.Status
		}
		if p.Weight > 0 {
			acc.Weight = p.Weight
		}
		// 供应商相关：任一字段变化都需整体过一遍校验（provider 决定 base_url 是否必填）。
		if (p.Provider != "" && p.Provider != acc.Provider) || p.BaseURL != "" {
			probe := accountPayload{Provider: p.Provider, BaseURL: p.BaseURL}
			if _, _, _, verr := probe.validateProvider(); verr != nil {
				writeJSON(w, 400, map[string]any{"detail": verr.Error()})
				return
			}
			if p.Provider != "" {
				acc.Provider = strings.TrimSpace(p.Provider)
			}
			if p.BaseURL != "" {
				acc.BaseURL = provider.NormalizeBaseURL(p.BaseURL)
			}
		}
		if p.CapResponses != nil {
			if v := *p.CapResponses; v == -1 || v == 0 || v == 1 {
				acc.CapResponses = v
			} else {
				writeJSON(w, 400, map[string]any{"detail": "能力覆盖仅支持 0（继承）/ 1（是）/ -1（否）"})
				return
			}
		}
		if p.CapImages != nil {
			if v := *p.CapImages; v == -1 || v == 0 || v == 1 {
				acc.CapImages = v
			} else {
				writeJSON(w, 400, map[string]any{"detail": "能力覆盖仅支持 0（继承）/ 1（是）/ -1（否）"})
				return
			}
		}
		if k := p.key(); k != "" {
			key := strings.TrimSpace(k)
			enc, err := a.box.Encrypt(key)
			if err != nil {
				writeJSON(w, 500, map[string]any{"detail": err.Error()})
				return
			}
			acc.ArkAPIKeyEnc = enc
			acc.KeyHint = secure.Mask(key)
		}
		// base_url 可能被清空（如从 custom 迁回 ark），需要复核是否满足 provider 要求。
		if def, ok := provider.Get(acc.Provider); ok && def.DefaultBaseURL == "" && provider.NormalizeBaseURL(acc.BaseURL) == "" {
			writeJSON(w, 400, map[string]any{"detail": "该供应商需要填写 base_url"})
			return
		}
		if err := a.store.UpsertAccount(acc); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
	case http.MethodDelete:
		disabled, err := a.store.DeleteAccount(id)
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		// 白名单被清空的子 Key 会被自动停用（空白名单 = 不限，必须 fail closed）。
		body := map[string]any{"success": true}
		if len(disabled) > 0 {
			body["disabled_subkeys"] = disabled
		}
		writeJSON(w, 200, body)
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

// ─────────────────────────── models ───────────────────────────

// validateModelType 校验模型类型字段，空值回落为 text。
func validateModelType(t string) (string, error) {
	if t == "" {
		return model.ModelTypeText, nil
	}
	if t != model.ModelTypeText && t != model.ModelTypeImage && t != model.ModelTypeRouter {
		return "", errors.New("模型类型仅支持 text / image / router")
	}
	return t, nil
}

// validateModelProvider 校验模型的上游协议字段（供应商类型已从账号下沉到模型）。
// 空值 = OpenAI 兼容（默认，覆盖 ark/openai/custom 三类账号供应商）；
// 目前仅支持 anthropic 一个转换协议。注册表里的 ark/openai/custom 是账号层
// 概念（决定默认 base URL），在模型层等价于 OpenAI 兼容协议，不作为合法取值。
func validateModelProvider(p string) error {
	switch p {
	case model.ModelProtocolOpenAI, model.ModelProtocolAnthropic:
		return nil
	}
	return errors.New("上游协议仅支持空（OpenAI 兼容）/ anthropic")
}

// validateFallbackChain 校验 fallback 链：所有名字必须已存在且与模型同类型；
// 路由模型不能作为 fallback 目标（它是虚拟名字，没有接入点，进了链也必失败）。
func (a *Admin) validateFallbackChain(chain []string, modelType string) error {
	for _, f := range chain {
		fm, err := a.store.GetModel(f)
		if err != nil {
			return errors.New("fallback 模型 " + f + " 不存在")
		}
		ft := fm.Type
		if ft == "" {
			ft = model.ModelTypeText
		}
		if ft == model.ModelTypeRouter {
			return errors.New("fallback 模型 " + f + " 是路由模型，不能作为 fallback 目标")
		}
		if ft != modelType {
			return errors.New("fallback 模型 " + f + " 与该模型类型不同（仅允许同类型退避）")
		}
	}
	return nil
}

// validateRouterModel 校验路由模型（type=router）的整体约束：
//   - 不使用 fallback 链（分流目标即路由规则，退避语义由目标模型自己的链承担）；
//   - 必须至少配置一条规则或默认目标，否则该名字解析不出任何真实模型；
//   - 规则/默认目标的模型必须存在且是文本或路由模型（不能指向图像模型或自身）；
//   - 规则按阈值升序整理（运行时取第一条满足的），空目标规则剔除；
//   - 沿路由目标走下去不能绕回自身（成环）。
func (a *Admin) validateRouterModel(m *model.Model) error {
	if len(m.Fallback) > 0 {
		return errors.New("路由模型不使用 fallback 链（请改用分流规则）")
	}
	// 路由规则独立在「分流配置」页维护；新建模型时允许暂时没有配置，
	// 未配置模型运行时会返回明确的「未配置可用分流规则」错误。
	if m.Router == nil {
		return nil
	}
	if len(m.Router.Rules) == 0 && m.Router.DefaultTarget == "" {
		return errors.New("路由模型需要至少一条分流规则或默认目标")
	}
	if len(m.Router.Rules) > model.MaxRouterRules {
		return fmt.Errorf("分流规则最多 %d 条", model.MaxRouterRules)
	}
	for _, r := range m.Router.Rules {
		if r.MaxInputTokens < 0 {
			return errors.New("分流阈值不能为负")
		}
		if err := a.validateRouterTarget(r.Target, m.Name); err != nil {
			return err
		}
	}
	if m.Router.DefaultTarget != "" {
		if err := a.validateRouterTarget(m.Router.DefaultTarget, m.Name); err != nil {
			return err
		}
	}
	// 规则整理：剔除空目标，按阈值升序稳定排序（运行时语义 = 第一条满足的）。
	rules := make([]model.RouterRule, 0, len(m.Router.Rules))
	for _, r := range m.Router.Rules {
		if r.Target != "" {
			rules = append(rules, r)
		}
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].MaxInputTokens < rules[j].MaxInputTokens })
	m.Router.Rules = rules
	return a.checkRouterCycle(m.Name)
}

// validateRouterTarget 校验单个路由目标：必须存在、非自身、且是文本/路由模型
// （图像模型没有输入长度语义，不能作为分流目标）。
func (a *Admin) validateRouterTarget(t, self string) error {
	if t == "" {
		return errors.New("分流规则缺少目标模型")
	}
	if t == self {
		return errors.New("路由目标不能是自身")
	}
	tm, err := a.store.GetModel(t)
	if err != nil {
		return errors.New("路由目标模型 " + t + " 不存在")
	}
	tt := tm.Type
	if tt == "" {
		tt = model.ModelTypeText
	}
	if tt == model.ModelTypeImage {
		return errors.New("路由目标 " + t + " 是图像模型（仅允许文本/路由模型）")
	}
	return nil
}

// checkRouterCycle 从 start 出发沿「路由目标」边做 DFS：任何路径绕回 start
// 即成环（运行时 ResolveRouter 还有深度/环兜底，这里在保存时就拦下）。
func (a *Admin) checkRouterCycle(start string) error {
	all, err := a.store.ListModels()
	if err != nil {
		return err
	}
	rcOf := map[string]*model.RouterConfig{}
	for _, m := range all {
		if m.Type == model.ModelTypeRouter && m.Router != nil {
			rcOf[m.Name] = m.Router
		}
	}
	visited := map[string]bool{}
	var walk func(node string) bool
	walk = func(node string) bool {
		rc, ok := rcOf[node]
		if !ok || visited[node] {
			return false // 走到非路由模型（链终止）或已访问过
		}
		visited[node] = true
		for _, r := range rc.Rules {
			if r.Target == start {
				return true
			}
			if walk(r.Target) {
				return true
			}
		}
		if rc.DefaultTarget == start {
			return true
		}
		return walk(rc.DefaultTarget)
	}
	if walk(start) {
		return errors.New("路由配置成环（沿分流目标会绕回 " + start + "）")
	}
	return nil
}

// ─────────────────────────── 上游模型列表探测 ───────────────────────────

// upstreamProbeTimeout 探测上游 /models 的上限：借用请求超时，但不跟随
// 「0 = 不限」——管理端点击不该无限等待，缺省压到 30s。
func (a *Admin) upstreamProbeTimeout() time.Duration {
	t := a.cfg.Timeouts.Request()
	if t <= 0 || t > 30*time.Second {
		return 30 * time.Second
	}
	return t
}

// handleUpstreamModels 用指定账号的凭据拉取其上游的 OpenAI 兼容模型列表
// （GET {base}/models），供「从上游导入」减少手工录入。
// 上游错误原样透出状态码与错误体摘要，便于区分「未授权」与「不支持该接口」。
func (a *Admin) handleUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	accountID := r.URL.Query().Get("account_id")
	if accountID == "" {
		writeJSON(w, 400, map[string]any{"detail": "缺少 account_id"})
		return
	}
	acc, err := a.store.GetAccount(accountID)
	if err != nil {
		writeJSON(w, 404, map[string]any{"detail": "账号不存在"})
		return
	}
	rt, err := a.routeForAccount(acc)
	if err != nil {
		if errors.Is(err, provider.ErrNoBaseURL) {
			writeJSON(w, 400, map[string]any{"detail": "该账号未配置 base URL"})
			return
		}
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	list, err := a.mgr.ListModels(r.Context(), rt, a.upstreamProbeTimeout())
	if err != nil {
		if he, ok := provider.AsHTTPError(err); ok {
			writeJSON(w, 502, map[string]any{
				"detail": fmt.Sprintf("上游返回 %d：%s", he.Code, truncateText(string(he.Body), 300)),
			})
			return
		}
		writeJSON(w, 502, map[string]any{"detail": "拉取失败：" + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"base_url": rt.BaseURL, "count": len(list), "models": list})
}

// routeForAccount 解析账号的可发送路由（供应商定义、最终 base URL 与解密后的 Key）。
func (a *Admin) routeForAccount(acc *model.Account) (provider.Route, error) {
	def, ok := provider.Get(acc.Provider)
	if !ok {
		def = provider.FallbackDef(acc.Provider)
	}
	baseURL, err := def.ResolveBaseURL(acc.BaseURL)
	if err != nil {
		return provider.Route{}, err
	}
	key, err := a.box.Decrypt(acc.ArkAPIKeyEnc)
	if err != nil {
		return provider.Route{}, errors.New("账号密钥解密失败")
	}
	return provider.Route{Def: def, BaseURL: baseURL, Key: key}, nil
}

// testModelRequest 快速测试请求。account_id + ep 直接探测上游标识；model
// 按已注册模型解析接入点（路由模型会先解析到真实目标）。
type testModelRequest struct {
	Model     string `json:"model"`
	AccountID string `json:"account_id"`
	EP        string `json:"ep"`
	Protocol  string `json:"protocol"`
}

type testModelResult struct {
	OK          bool   `json:"ok"`
	LatencyMS   int64  `json:"latency_ms"`
	Model       string `json:"model,omitempty"`
	Resolved    string `json:"resolved,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`
	EP          string `json:"ep,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Status      int    `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

// handleTestModel 用最小 chat 请求快速探测模型/接入点可用性。
// 探测是管理侧诊断，不进入限流、熔断与用量统计；上游失败也返回 200 + ok=false，
// 让前端可以把失败结果显示在对应模型旁边。
func (a *Admin) handleTestModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	var p testModelRequest
	if err := decode(r, &p); err != nil {
		writeJSON(w, 400, map[string]any{"detail": "无效请求"})
		return
	}
	p.Model = strings.TrimSpace(p.Model)
	p.AccountID = strings.TrimSpace(p.AccountID)
	p.EP = strings.TrimSpace(p.EP)
	p.Protocol = strings.TrimSpace(p.Protocol)
	if p.Protocol != "" && p.Protocol != model.ModelProtocolAnthropic && p.Protocol != model.ModelProtocolOpenAI {
		writeJSON(w, 400, map[string]any{"detail": "上游协议仅支持空（OpenAI 兼容）/ anthropic"})
		return
	}
	if p.AccountID == "" || p.EP == "" {
		if p.Model == "" {
			writeJSON(w, 400, map[string]any{"detail": "缺少 model 或 account_id + ep"})
			return
		}
		result := a.testRegisteredModel(r.Context(), p.Model)
		writeJSON(w, 200, result)
		return
	}

	acc, err := a.store.GetAccount(p.AccountID)
	if err != nil {
		writeJSON(w, 404, map[string]any{"detail": "账号不存在"})
		return
	}
	result := testModelResult{Model: p.Model, AccountID: acc.ID, AccountName: acc.Name, EP: p.EP, Protocol: p.Protocol}
	if p.Model != "" {
		if registered, merr := a.store.GetModel(p.Model); merr == nil {
			if registered.Type == model.ModelTypeImage {
				result.Error = "图像模型不支持快速测试（避免产生真实生成费用）"
				writeJSON(w, 200, result)
				return
			}
			if result.Protocol == "" {
				result.Protocol = registered.Provider
			}
		}
	}
	rt, err := a.routeForAccount(acc)
	if err != nil {
		result.Error = err.Error()
		writeJSON(w, 200, result)
		return
	}
	// account_id+ep 直测：若该元组恰有已登记映射（account×model×ep），带上它的
	// 请求头，保证探测与真实转发环境一致（依赖 UA / beta 头的接入点也能被测准）。
	if p.Model != "" {
		if eps, lerr := a.store.ListEndpoints(); lerr == nil {
			for _, ep := range eps {
				if ep.AccountID == acc.ID && ep.Model == p.Model && ep.EP == p.EP {
					rt.Headers = ep.RequestHeaders
					rt.BodyDefaults = ep.DefaultBodyParams
					break
				}
			}
		}
	}
	a.finishModelProbe(r.Context(), &result, rt, p.EP, p.Protocol)
	writeJSON(w, 200, result)
}

// testRegisteredModel 从启用账号与接入点中挑一条，对已注册模型执行探测。
func (a *Admin) testRegisteredModel(ctx context.Context, name string) testModelResult {
	result := testModelResult{Model: name}
	m, err := a.store.GetModel(name)
	if err != nil {
		result.Error = "模型不存在"
		return result
	}
	if m.Type == model.ModelTypeImage {
		result.Error = "图像模型不支持快速测试（避免产生真实生成费用）"
		return result
	}
	target := name
	if m.Type == model.ModelTypeRouter {
		resolved, rerr := a.bal.ResolveRouter(name, 0)
		if rerr != nil {
			result.Error = rerr.Error()
			return result
		}
		result.Resolved = resolved
		target = resolved
		m, err = a.store.GetModel(target)
		if err != nil {
			result.Error = "路由目标模型不存在"
			return result
		}
		if m.Type == model.ModelTypeImage {
			result.Error = "路由目标是图像模型，不支持快速测试"
			return result
		}
	}
	result.Model = name
	result.Protocol = m.Provider

	endpoints, err := a.store.ListEndpoints()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	accounts, err := a.store.ListAccounts()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	accOf := make(map[string]*model.Account, len(accounts))
	for _, acc := range accounts {
		accOf[acc.ID] = acc
	}
	var chosen *model.Endpoint
	var chosenAcc *model.Account
	for _, ep := range endpoints {
		if ep.Model != target || !ep.Enabled {
			continue
		}
		acc := accOf[ep.AccountID]
		if acc == nil || acc.Status != model.AccountActive {
			continue
		}
		chosen, chosenAcc = ep, acc
		break
	}
	if chosen == nil {
		result.Error = "该模型没有可用接入点（未配置或全部停用）"
		return result
	}
	result.AccountID, result.AccountName, result.EP = chosenAcc.ID, chosenAcc.Name, chosen.EP
	rt, err := a.routeForAccount(chosenAcc)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	// 测试要与该映射的真实调用环境一致：带上叶节点的请求头（UA / beta 等）
	// 与默认请求体参数（上游方言要求的额外字段），否则会出现
	// 「探测失败但实转成功」的误报。
	rt.Headers = chosen.RequestHeaders
	rt.BodyDefaults = chosen.DefaultBodyParams
	a.finishModelProbe(ctx, &result, rt, chosen.EP, m.Provider)
	return result
}

// finishModelProbe 发送探测请求并写入结果。探测请求体保持最小：不设
// max_tokens / reasoning_effort 等易被上游拒绝或影响响应的参数，只要求模型
// 以固定短语回应，验证链路与模型能否正常对话。不做用量统计。
func (a *Admin) finishModelProbe(ctx context.Context, result *testModelResult, rt provider.Route, ep, protocol string) {
	const probeBody = `{"messages":[{"role":"user","content":"Reply with exactly PONG and nothing else."}]}`
	started := time.Now()
	var err error
	if protocol == model.ModelProtocolAnthropic {
		// Anthropic 协议的 max_tokens 是协议必填项（不能省略），用兜底默认值。
		_, _, err = a.mgr.AnthropicChat(ctx, rt, []byte(probeBody), ep, provider.DefaultAnthropicMaxTokens, a.upstreamProbeTimeout())
	} else {
		_, _, err = a.mgr.Chat(ctx, rt, []byte(probeBody), ep, a.upstreamProbeTimeout())
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	if err == nil {
		result.OK = true
		return
	}
	if he, ok := provider.AsHTTPError(err); ok {
		result.Status = he.Code
		result.Error = fmt.Sprintf("上游返回 %d：%s", he.Code, truncateText(string(he.Body), 300))
		return
	}
	result.Error = err.Error()
}

// truncateText 截断上游错误体，避免把长 HTML 页面整页塞进管理端提示。
func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ─────────────────────────── 运行时设置（上游超时） ───────────────────────────

// 超时可设区间（秒）：上限 1 小时够覆盖长推理，0 表示关闭该项超时。
const maxTimeoutSec = 3600

// handleRuntimeSettings GET 读当前上游超时，PUT 热改并持久化。
// 语义：秒（支持小数），0 = 关闭该项；只写请求里显式带上的键（部分更新）。
// 改动立即对后续请求生效（cfg.Timeouts 原子字段），无需重启。
func (a *Admin) handleRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, a.runtimeSettingsBody())
	case http.MethodPut:
		var probe map[string]any
		if err := decode(r, &probe); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		apply := func(key string, set func(time.Duration), storeKey string) error {
			raw, ok := probe[key]
			if !ok {
				return nil
			}
			sec := floatField(raw)
			if sec < 0 || sec > maxTimeoutSec {
				return fmt.Errorf("%s 需在 0~%d 秒之间（0 = 关闭）", key, maxTimeoutSec)
			}
			set(time.Duration(sec * float64(time.Second)))
			return a.store.SetSetting(storeKey, strconv.FormatFloat(sec, 'f', -1, 64))
		}
		if err := apply("request_timeout_sec", a.cfg.Timeouts.SetRequest, keyTimeoutRequest); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		if err := apply("first_token_timeout_sec", a.cfg.Timeouts.SetFirstToken, keyTimeoutFirstToken); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		// 计价币种（展示层）。刻意允许很大的汇率上限——兑换率本身没有天然上界
		// （历史上出现过 1:10000+ 的币种），卡得太死只会妨碍使用；但要挡住负值
		// 与非数字，否则界面会显示莫名其妙的金额。
		if raw, ok := probe["currency_rate"]; ok {
			rate := floatField(raw)
			if rate < 0 || rate > maxCurrencyRate {
				writeJSON(w, 400, map[string]any{"detail": fmt.Sprintf("汇率需在 0~%g 之间（0 = 不换算）", maxCurrencyRate)})
				return
			}
			if err := a.store.SetSetting(keyCurrencyRate, strconv.FormatFloat(rate, 'f', -1, 64)); err != nil {
				writeJSON(w, 500, map[string]any{"detail": err.Error()})
				return
			}
			a.cfg.Currency.Set(rate, a.cfg.Currency.Symbol(), a.cfg.Currency.Code())
		}
		if raw, ok := probe["currency_symbol"]; ok {
			sym := strings.TrimSpace(stringField(raw))
			// 必须按**字符数**而非字节数校验：¥ € £ ₹ ₽ 这些符号在 UTF-8 里都是
			// 2~3 字节，用 len() 会把它们全部误拒——而人民币符号正是本功能的目标场景。
			if utf8.RuneCountInString(sym) > 4 {
				writeJSON(w, 400, map[string]any{"detail": "货币符号最多 4 个字符"})
				return
			}
			if err := a.store.SetSetting(keyCurrencySymbol, sym); err != nil {
				writeJSON(w, 500, map[string]any{"detail": err.Error()})
				return
			}
			a.cfg.Currency.Set(a.cfg.Currency.Rate(), sym, a.cfg.Currency.Code())
		}
		if raw, ok := probe["currency_code"]; ok {
			code := strings.ToUpper(strings.TrimSpace(stringField(raw)))
			if utf8.RuneCountInString(code) > 8 {
				writeJSON(w, 400, map[string]any{"detail": "货币代码最多 8 个字符"})
				return
			}
			if err := a.store.SetSetting(keyCurrencyCode, code); err != nil {
				writeJSON(w, 500, map[string]any{"detail": err.Error()})
				return
			}
			a.cfg.Currency.Set(a.cfg.Currency.Rate(), a.cfg.Currency.Symbol(), code)
		}
		writeJSON(w, 200, a.runtimeSettingsBody())
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

func (a *Admin) runtimeSettingsBody() map[string]any {
	return map[string]any{
		"request_timeout_sec":     a.cfg.Timeouts.Request().Seconds(),
		"first_token_timeout_sec": a.cfg.Timeouts.FirstToken().Seconds(),
		"session_ttl_sec":         a.cfg.SessionTTL.Seconds(),
		"max_retries":             a.cfg.MaxRetriesAvailable,
		"max_timeout_sec":         maxTimeoutSec,
		"max_currency_rate":       maxCurrencyRate,
		// 计价币种：单价与成本以美元存储，这两个值只影响展示换算。
		"currency_rate":   a.cfg.Currency.Rate(),
		"currency_symbol": a.cfg.Currency.Symbol(),
		"currency_code":   a.cfg.Currency.Code(),
		"defaults": map[string]any{
			"request_timeout_sec":     config.DefaultRequestTimeout.Seconds(),
			"first_token_timeout_sec": config.DefaultFirstTokenTimeout.Seconds(),
		},
	}
}

// ─────────────────────────── 模型目录自动补全 ───────────────────────────

// catalogDataURL LiteLLM 社区维护的模型元数据目录（统一来源，按模型名查询）。
const catalogDataURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// fillModelFromCatalog 按模型名查内置目录，补全空缺（零值）字段；
// 人工填写的非零值一律不动。返回被补全的字段名列表。
// 路由模型不参与补全：它不承接请求，价格/上下文都没有语义。
func fillModelFromCatalog(c *catalog.Catalog, m *model.Model) []string {
	if m.Type == model.ModelTypeRouter {
		return nil
	}
	e, ok := c.Lookup(m.Name)
	if !ok {
		return nil
	}
	var filled []string
	if m.ContextTokens == 0 && e.MaxInput > 0 {
		m.ContextTokens = e.MaxInput
		filled = append(filled, "context_tokens")
	}
	if m.MaxOutputTokens == 0 && e.MaxOutput > 0 {
		m.MaxOutputTokens = e.MaxOutput
		filled = append(filled, "max_output_tokens")
	}
	if m.PriceInput == 0 && e.CostIn > 0 {
		m.PriceInput = e.CostIn
		filled = append(filled, "price_input")
	}
	if m.PriceOutput == 0 && e.CostOut > 0 {
		m.PriceOutput = e.CostOut
		filled = append(filled, "price_output")
	}
	if m.PriceImage == 0 && e.CostImage > 0 {
		m.PriceImage = e.CostImage
		filled = append(filled, "price_image")
	}
	// 缓存单价：同样只补空缺。目录里缓存读/写通常与输入价不同
	// （读约 10%、写约 125%），补全后计费才能反映真实折扣与溢价。
	if m.PriceCacheRead == 0 && e.CostCacheRead > 0 {
		m.PriceCacheRead = e.CostCacheRead
		filled = append(filled, "price_cache_read")
	}
	if m.PriceCacheWrite == 0 && e.CostCacheWrite > 0 {
		m.PriceCacheWrite = e.CostCacheWrite
		filled = append(filled, "price_cache_write")
	}
	return filled
}

// fetchLatestCatalog 在线拉取最新目录原文（失败返回 nil，由调用方回落内嵌快照）。
func fetchLatestCatalog() []byte {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(catalogDataURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil
	}
	return data
}

// handleMetadataSync 全量补全：优先在线拉取最新目录（失败静默回落内嵌快照），
// 然后为所有模型补全空缺（零值）字段；人工填写的值不动。
func (a *Admin) handleMetadataSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	fetchOK := false
	if data := fetchLatestCatalog(); data != nil {
		if err := a.catalog.Reload(data); err == nil {
			fetchOK = true
		}
	}
	type entry struct {
		Name   string   `json:"name"`
		Fields []string `json:"fields"`
	}
	details := []entry{}
	models, err := a.store.ListModels()
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	for _, m := range models {
		if m.Type == model.ModelTypeRouter {
			continue // 路由模型不承接请求，无价格/上下文可补
		}
		fields := fillModelFromCatalog(a.catalog, m)
		if len(fields) == 0 {
			continue
		}
		if err := a.store.UpsertModel(m); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		details = append(details, entry{Name: m.Name, Fields: fields})
	}
	a.bal.Refresh()
	writeJSON(w, 200, map[string]any{
		"updated":  len(details),
		"details":  details,
		"fetch_ok": fetchOK,
		"source":   a.catalog.Source(),
		"entries":  a.catalog.Count(),
	})
}

// handleCatalogLookup 按模型名查目录，供前端表单即时提示与预填参考。
func (a *Admin) handleCatalogLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	e, ok := a.catalog.Lookup(r.URL.Query().Get("name"))
	writeJSON(w, 200, map[string]any{"found": ok, "entry": e})
}

func (a *Admin) handleModelsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := a.store.ListModels()
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, all)
	case http.MethodPost:
		var m model.Model
		if err := decode(r, &m); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		if m.Name == "" {
			writeJSON(w, 400, map[string]any{"detail": "name 必填"})
			return
		}
		if m.Display == "" {
			m.Display = m.Name
		}
		t, terr := validateModelType(m.Type)
		if terr != nil {
			writeJSON(w, 400, map[string]any{"detail": terr.Error()})
			return
		}
		m.Type = t
		if perr := validateModelProvider(m.Provider); perr != nil {
			writeJSON(w, 400, map[string]any{"detail": perr.Error()})
			return
		}
		if m.Type == model.ModelTypeImage && m.Provider == model.ModelProtocolAnthropic {
			writeJSON(w, 400, map[string]any{"detail": "图像模型不支持 Anthropic 协议（其上游无图像生成 API）"})
			return
		}
		if m.Fallback == nil {
			m.Fallback = []string{} // 统一存 []，避免落库成 null（与「清空链路」口径一致）
		}
		if m.Type == model.ModelTypeRouter {
			// 路由模型：走自己的约束（不使用 fallback 链；目标存在且非环）。
			if err := a.validateRouterModel(&m); err != nil {
				writeJSON(w, 400, map[string]any{"detail": err.Error()})
				return
			}
		} else {
			if err := a.validateFallbackChain(m.Fallback, t); err != nil {
				writeJSON(w, 400, map[string]any{"detail": err.Error()})
				return
			}
		}
		// 目录自动补全：按模型名查内置目录，只填空缺（零值）字段，人工填写优先。
		filled := fillModelFromCatalog(a.catalog, &m)
		m.Enabled = true // 新建默认启用
		if err := a.store.UpsertModel(&m); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true, "auto_filled": filled})
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

func (a *Admin) handleModelItem(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/models/")
	if name == "" {
		writeJSON(w, 400, map[string]any{"detail": "缺少 name"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		// 解码到 map 做「部分更新」：直接反序列化成 model.Model 会把请求里未出现的
		// 字段当成零值写回（描述被清空、enabled 被置 false、fallback 被清链），
		// 与「编辑」语义不符。这里只覆盖请求显式带上的键。
		var probe map[string]any
		if err := decode(r, &probe); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		existing, err := a.store.GetModel(name)
		if err != nil {
			writeJSON(w, 404, map[string]any{"detail": "模型不存在"})
			return
		}
		if v, ok := probe["display"].(string); ok {
			existing.Display = v
		}
		if v, ok := probe["description"].(string); ok {
			existing.Description = v
		}
		if v, ok := probe["enabled"].(bool); ok {
			existing.Enabled = v
		}
		if v, ok := probe["type"].(string); ok {
			t, terr := validateModelType(v)
			if terr != nil {
				writeJSON(w, 400, map[string]any{"detail": terr.Error()})
				return
			}
			existing.Type = t
		}
		if raw, ok := probe["fallback"]; ok {
			existing.Fallback = stringSlice(raw)
		}
		// 上游协议（供应商类型下沉到模型）：空 = OpenAI 兼容。
		if v, ok := probe["provider"].(string); ok {
			existing.Provider = strings.TrimSpace(v)
		}
		// 路由配置（仅 type=router 有语义）：显式带 router 键才更新——
		// null / 空对象清空，对象则整体替换（规则是数组，无法按条合并）。
		if raw, ok := probe["router"]; ok {
			if raw == nil {
				existing.Router = nil
			} else {
				rb, merr := json.Marshal(raw)
				rc := &model.RouterConfig{}
				if merr != nil || json.Unmarshal(rb, rc) != nil {
					writeJSON(w, 400, map[string]any{"detail": "router 配置无法解析"})
					return
				}
				existing.Router = rc
			}
		}
		// 价格字段（成本核算）：0 表示未定价。
		if v, ok := probe["price_input"]; ok {
			existing.PriceInput = floatField(v)
		}
		if v, ok := probe["price_output"]; ok {
			existing.PriceOutput = floatField(v)
		}
		if v, ok := probe["price_image"]; ok {
			existing.PriceImage = floatField(v)
		}
		// 缓存单价（0 = 未设置，计费回落到输入单价）。
		if v, ok := probe["price_cache_read"]; ok {
			existing.PriceCacheRead = floatField(v)
		}
		if v, ok := probe["price_cache_write"]; ok {
			existing.PriceCacheWrite = floatField(v)
		}
		// 能力上限：0 = 未设置（不校验，允许目录自动补全）。
		if v, ok := probe["context_tokens"]; ok {
			existing.ContextTokens = int64Field(v)
		}
		if v, ok := probe["max_output_tokens"]; ok {
			existing.MaxOutputTokens = int64Field(v)
		}
		et := existing.Type
		if et == "" {
			et = model.ModelTypeText
		}
		if perr := validateModelProvider(existing.Provider); perr != nil {
			writeJSON(w, 400, map[string]any{"detail": perr.Error()})
			return
		}
		if et == model.ModelTypeImage && existing.Provider == model.ModelProtocolAnthropic {
			writeJSON(w, 400, map[string]any{"detail": "图像模型不支持 Anthropic 协议（其上游无图像生成 API）"})
			return
		}
		// 类型感知校验：路由模型走分流规则约束，其余走 fallback 链约束。
		if et == model.ModelTypeRouter {
			if err := a.validateRouterModel(existing); err != nil {
				writeJSON(w, 400, map[string]any{"detail": err.Error()})
				return
			}
		} else {
			if err := a.validateFallbackChain(existing.Fallback, et); err != nil {
				writeJSON(w, 400, map[string]any{"detail": err.Error()})
				return
			}
		}
		if err := a.store.UpsertModel(existing); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
	case http.MethodDelete:
		disabled, err := a.store.DeleteModel(name)
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		// 白名单被清空的子 Key 会被自动停用（空白名单 = 不限，必须 fail closed）；
		// 把名字回给前端提示，避免管理员以为 Key 无故失效。
		body := map[string]any{"success": true}
		if len(disabled) > 0 {
			body["disabled_subkeys"] = disabled
		}
		writeJSON(w, 200, body)
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

// ─────────────────────────── endpoints ───────────────────────────

func (a *Admin) handleEndpointsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 附带账号名，方便 UI 展示。
		type epOut struct {
			*model.Endpoint
			AccountName string `json:"account_name"`
		}
		all, err := a.store.ListEndpoints()
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		accs, _ := a.store.ListAccounts()
		nameOf := map[string]string{}
		for _, x := range accs {
			nameOf[x.ID] = x.Name
		}
		out := make([]epOut, 0, len(all))
		for _, e := range all {
			out = append(out, epOut{Endpoint: e, AccountName: nameOf[e.AccountID]})
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var e model.Endpoint
		if err := decode(r, &e); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		e.EP = strings.TrimSpace(e.EP)
		if e.AccountID == "" || e.Model == "" || e.EP == "" {
			writeJSON(w, 400, map[string]any{"detail": "account_id、model、ep 必填"})
			return
		}
		if err := provider.ValidateRequestHeaders(e.RequestHeaders); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		if err := provider.ValidateBodyParams(e.DefaultBodyParams); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		// 同一账号 + 同一模型可挂多个不同 ep（同模型的不同发布版本），
		// 但完全相同的三元组是重复配置：明确拒绝，避免静默改到既有行。
		if _, dup := a.store.EndpointIDByTuple(e.AccountID, e.Model, e.EP); dup {
			writeJSON(w, 400, map[string]any{"detail": "该账号下该模型已存在相同的上游标识 " + e.EP})
			return
		}
		if e.ID == "" {
			e.ID = "ep_" + randHex(6)
		}
		e.Enabled = true // 新建默认启用
		if err := a.store.UpsertEndpoint(&e); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true, "id": e.ID})
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

func (a *Admin) handleEndpointItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/endpoints/")
	if id == "" {
		writeJSON(w, 400, map[string]any{"detail": "缺少 id"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		// 请求体先整块读进内存：下面要用两种方式解它——
		// ① map[string]any 做部分更新；② RawMessage 取 default_body_params 的原始字节。
		// （顺序不能反：decode 会读干 r.Body，之后再读就是空的。）
		bodyBytes, err := readAllBody(r)
		if err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		var probe map[string]any
		if err := json.Unmarshal(bodyBytes, &probe); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		// default_body_params 需要**原始 JSON 字节**：probe 是 map[string]any，
		// 数字会被解成 float64，12345678901234567890 这种大整数在进入业务逻辑
		// 之前就已经变成 1.2345678901234567e+19（实测确认，且会静默落库）。
		rawBodyParams, hasRawBodyParams := rawFieldOf(bodyBytes, "default_body_params")
		existing, err := a.store.GetEndpoint(id)
		if err != nil {
			writeJSON(w, 404, map[string]any{"detail": "映射不存在"})
			return
		}
		existing.ID = id
		if v, ok := probe["ep"].(string); ok && v != "" {
			existing.EP = v
		}
		if v, ok := probe["model"].(string); ok && v != "" {
			existing.Model = v
		}
		if v, ok := probe["account_id"].(string); ok && v != "" {
			existing.AccountID = v
		}
		if v, ok := probe["enabled"].(bool); ok {
			existing.Enabled = v
		}
		// 接入点级上游检查豁免：开启时即时清除本条的「上游已删除」标记——
		// 该标记来自检查器且携带「本条参与检查」的前提，豁免后必须一并失效，
		// 否则会出现「已豁免却仍标红」的矛盾状态（检查器下一轮也会清，这里不等它）。
		if v, ok := probe["skip_upstream_check"].(bool); ok {
			existing.SkipUpstreamCheck = v
			if v {
				if err := a.store.ClearUpstreamDeleted(id); err != nil {
					writeJSON(w, 500, map[string]any{"detail": err.Error()})
					return
				}
			}
		}
		// 流控整数字段保持部分更新语义：只覆盖请求里显式携带的键。
		// （此前无条件覆盖，只发 request_headers 会把四个限流值一起清零。）
		type intFieldSpec struct {
			key string
			set func(int64)
		}
		for _, f := range []intFieldSpec{
			{"weight", func(v int64) { existing.Weight = int(v) }},
			{"max_concurrency", func(v int64) { existing.MaxConcurrency = int(v) }},
			{"rpm_limit", func(v int64) { existing.RPMLimit = int(v) }},
			{"tpm_limit", func(v int64) { existing.TPMLimit = v }},
		} {
			v, present := probe[f.key]
			if !present || v == nil {
				continue
			}
			num, isNum := v.(float64)
			if !isNum {
				writeJSON(w, 400, map[string]any{"detail": f.key + " 必须是数字"})
				return
			}
			f.set(int64(num))
		}
		if raw, ok := probe["request_headers"]; ok {
			if raw == nil {
				existing.RequestHeaders = map[string]string{} // 显式 null 视作清空
			} else {
				b, err := json.Marshal(raw)
				if err != nil || json.Unmarshal(b, &existing.RequestHeaders) != nil {
					writeJSON(w, 400, map[string]any{"detail": "request_headers 必须是字符串到字符串的对象"})
					return
				}
			}
		}
		if hasRawBodyParams {
			if len(bytes.TrimSpace(rawBodyParams)) == 0 || string(bytes.TrimSpace(rawBodyParams)) == "null" {
				existing.DefaultBodyParams = map[string]json.RawMessage{} // 显式 null 视作清空
			} else {
				// 直接从原始字节解到 map[string]json.RawMessage：值保持原始
				// 字节，大整数不走 float64。用 probe 里的 any 会丢精度。
				var parsed map[string]json.RawMessage
				if err := json.Unmarshal(rawBodyParams, &parsed); err != nil || parsed == nil {
					writeJSON(w, 400, map[string]any{"detail": "default_body_params 必须是 JSON 对象"})
					return
				}
				existing.DefaultBodyParams = parsed
			}
		}
		existing.EP = strings.TrimSpace(existing.EP)
		if err := provider.ValidateRequestHeaders(existing.RequestHeaders); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		if err := provider.ValidateBodyParams(existing.DefaultBodyParams); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		// 改归属/标识后若与「另一条」映射三元组相同，明确拒绝（自身不算冲突）。
		if other, dup := a.store.EndpointIDByTuple(existing.AccountID, existing.Model, existing.EP); dup && other != id {
			writeJSON(w, 400, map[string]any{"detail": "该账号下该模型已存在相同的上游标识 " + existing.EP})
			return
		}
		if err := a.store.UpsertEndpoint(existing); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
	case http.MethodDelete:
		if err := a.store.DeleteEndpoint(id); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

// ─────────────────────────── subkeys ───────────────────────────

func (a *Admin) handleSubkeysCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := a.store.ListSubKeys()
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, all)
	case http.MethodPost:
		var p struct {
			Name             string   `json:"name"`
			Key              string   `json:"key"`
			AllowedModels    []string `json:"allowed_models"`
			AllowedAccounts  []string `json:"allowed_accounts"`
			DailyLimitTokens int64    `json:"daily_limit_tokens"`
			DailyLimitImages int64    `json:"daily_limit_images"`
			QuotaPeriod      string   `json:"quota_period"`
			// 指针：0 是「未设置」（→ 默认周一），必须与「显式传 0」区分不了时
			// 才不报错；但缺字段与传 0 在这里等价，都是「用默认」，故无需区分。
			QuotaResetWeekday   int   `json:"quota_reset_weekday"`
			QuotaResetHour      int   `json:"quota_reset_hour"`
			CacheReadPermille   int64 `json:"cache_read_permille"`
			CacheWritePermille  int64 `json:"cache_write_permille"`
			WindowLimitRequests int64 `json:"window_limit_requests"`
		}
		if err := decode(r, &p); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		if msg := validateQuotaFields(p.QuotaPeriod, p.QuotaResetWeekday, p.QuotaResetHour,
			p.CacheReadPermille, p.CacheWritePermille); msg != "" {
			writeJSON(w, 400, map[string]any{"detail": msg})
			return
		}
		key := strings.TrimSpace(p.Key)
		if key == "" {
			key = "sk-" + randHex(24)
		}
		key = store.NormalizeKey(key)
		sk := &model.SubKey{
			ID:                  "sk_" + randHex(6),
			Name:                p.Name,
			Key:                 key,
			KeyHash:             isTokenHash(key),
			Enabled:             true,
			AllowedModels:       p.AllowedModels,
			AllowedAccounts:     p.AllowedAccounts,
			DailyLimitTokens:    p.DailyLimitTokens,
			DailyLimitImages:    p.DailyLimitImages,
			QuotaPeriod:         model.NormalizeQuotaPeriod(p.QuotaPeriod),
			QuotaResetWeekday:   p.QuotaResetWeekday,
			QuotaResetHour:      p.QuotaResetHour,
			CacheReadPermille:   p.CacheReadPermille,
			CacheWritePermille:  p.CacheWritePermille,
			WindowLimitRequests: p.WindowLimitRequests,
			CreatedAt:           time.Now().Unix(),
		}
		if err := a.store.UpsertSubKey(sk); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"success": true, "id": sk.ID, "key": key})
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

func (a *Admin) handleSubkeyItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/subkeys/")
	if id == "" {
		writeJSON(w, 400, map[string]any{"detail": "缺少 id"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		var p struct {
			Name             *string  `json:"name"`
			Enabled          *bool    `json:"enabled"`
			AllowedModels    []string `json:"allowed_models"`
			AllowedAccounts  []string `json:"allowed_accounts"`
			DailyLimitTokens *int64   `json:"daily_limit_tokens"`
			DailyLimitImages *int64   `json:"daily_limit_images"`
			// 配额字段全部用指针：部分更新语义，只发一个字段不得清零其余字段
			// （与 24 号不变量同源的配置）。
			QuotaPeriod         *string `json:"quota_period"`
			QuotaResetWeekday   *int    `json:"quota_reset_weekday"`
			QuotaResetHour      *int    `json:"quota_reset_hour"`
			CacheReadPermille   *int64  `json:"cache_read_permille"`
			CacheWritePermille  *int64  `json:"cache_write_permille"`
			WindowLimitRequests *int64  `json:"window_limit_requests"`
		}
		if err := decode(r, &p); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		all, err := a.store.ListSubKeys()
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		var sk *model.SubKey
		for _, x := range all {
			if x.ID == id {
				sk = x
				break
			}
		}
		if sk == nil {
			writeJSON(w, 404, map[string]any{"detail": "子 Key 不存在"})
			return
		}
		if p.Name != nil {
			sk.Name = *p.Name
		}
		if p.Enabled != nil {
			sk.Enabled = *p.Enabled
		}
		if p.AllowedModels != nil {
			sk.AllowedModels = p.AllowedModels
		}
		if p.AllowedAccounts != nil {
			sk.AllowedAccounts = p.AllowedAccounts
		}
		if p.DailyLimitTokens != nil {
			sk.DailyLimitTokens = *p.DailyLimitTokens
		}
		if p.DailyLimitImages != nil {
			sk.DailyLimitImages = *p.DailyLimitImages
		}
		// 配额字段的校验要在赋值前做，让非法值整体拒绝而不是写一半。
		{
			period := sk.QuotaPeriod
			if p.QuotaPeriod != nil {
				period = *p.QuotaPeriod
			}
			wd, rh := sk.QuotaResetWeekday, sk.QuotaResetHour
			if p.QuotaResetWeekday != nil {
				wd = *p.QuotaResetWeekday
			}
			if p.QuotaResetHour != nil {
				rh = *p.QuotaResetHour
			}
			rd, rw := sk.CacheReadPermille, sk.CacheWritePermille
			if p.CacheReadPermille != nil {
				rd = *p.CacheReadPermille
			}
			if p.CacheWritePermille != nil {
				rw = *p.CacheWritePermille
			}
			if msg := validateQuotaFields(period, wd, rh, rd, rw); msg != "" {
				writeJSON(w, 400, map[string]any{"detail": msg})
				return
			}
		}
		if p.QuotaPeriod != nil {
			sk.QuotaPeriod = model.NormalizeQuotaPeriod(*p.QuotaPeriod)
		}
		if p.QuotaResetWeekday != nil {
			sk.QuotaResetWeekday = *p.QuotaResetWeekday
		}
		if p.QuotaResetHour != nil {
			sk.QuotaResetHour = *p.QuotaResetHour
		}
		if p.CacheReadPermille != nil {
			sk.CacheReadPermille = *p.CacheReadPermille
		}
		if p.CacheWritePermille != nil {
			sk.CacheWritePermille = *p.CacheWritePermille
		}
		if p.WindowLimitRequests != nil {
			sk.WindowLimitRequests = *p.WindowLimitRequests
		}
		if err := a.store.UpsertSubKey(sk); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"success": true})
	case http.MethodDelete:
		if err := a.store.DeleteSubKey(id); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"success": true})
	default:
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
	}
}

// ─────────────────────────── logs / stats / overview ───────────────────────────

func (a *Admin) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		if err := a.store.ClearUsageLogs(); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"success": true})
		return
	}
	// 分页：limit 为每页条数（store 会把超范围值回落到 200），offset 为偏移。
	// 返回 {items,total,limit,offset}，让前端能画出「第 x / y 页」。
	limit, offset := 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	// 要素筛选（零值 = 不过滤）：ip/model 模糊子串，subkey/account/status 精确。
	qv := r.URL.Query()
	f := store.LogFilter{
		IP:        strings.TrimSpace(qv.Get("ip")),
		Model:     strings.TrimSpace(qv.Get("model")),
		SubKeyID:  qv.Get("subkey"),
		AccountID: qv.Get("account"),
		Status:    qv.Get("status"),
		ErrorKind: qv.Get("error_kind"),
	}
	if f.Status != "" && f.Status != "ok" && f.Status != "error" {
		writeJSON(w, 400, map[string]any{"detail": "status 仅支持 ok / error"})
		return
	}
	if !validErrorKind(f.ErrorKind) {
		writeJSON(w, 400, map[string]any{"detail": "error_kind 取值非法"})
		return
	}
	logs, total, err := a.store.QueryUsageLogs(f, limit, offset)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"items": logs, "total": total, "limit": limit, "offset": offset})
}

func (a *Admin) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"accounts":  a.bal.SnapshotAccounts(),
		"endpoints": a.bal.SnapshotEndpoints(),
	})
}

// handleOverview 返回总览数据：账号/元组运行态 + 模型数 + 子 Key 数。
//
// 统计由 balancer.applyStat 在落库时同步进内存，这里直接读快照即可拿到最新值，
// 无需 Refresh()（那会做三次全表扫描并抢写锁，阻塞所有在途 Select）。
func (a *Admin) handleOverview(w http.ResponseWriter, r *http.Request) {
	accs := a.bal.SnapshotAccounts()
	eps := a.bal.SnapshotEndpoints()
	modelCount, _ := a.store.CountModels()
	subkeyCount, _ := a.store.CountSubKeys()

	var totalReq, totalTokens int64
	var active, disabled int64
	for _, acc := range accs {
		totalReq += acc.TotalRequests
		totalTokens += acc.TotalTokens
		if acc.Status == model.AccountActive {
			active++
		} else {
			disabled++
		}
	}
	// 元组级健康度：启用数 / 熔断数 / 探测数（熔断按 ID 查内存运行态，
	// 快照副本不带 Runtime，不能直接判可用性）。
	//
	// 探测数必须与熔断数分开报：两者都“不承接流量”，但含义完全不同——
	// 熔断中是等冷却，探测中是差一个成功请求就能恢复。合在一起会让运维
	// 无法区分「上游还坏着」与「正在恢复中」。
	var epEnabled, epCircuit, epHalfOpen int64
	for _, e := range eps {
		if e.Enabled {
			epEnabled++
		}
		if !e.Enabled {
			continue
		}
		if a.bal.CircuitOpen(e.ID) {
			epCircuit++
		} else if a.bal.CircuitHalfOpen(e.ID) {
			epHalfOpen++
		}
	}
	// 成本核算（来自 usage_logs.cost 聚合）。
	totalCost, cost24h, _ := a.store.SumCost()
	writeJSON(w, 200, map[string]any{
		"account_total":     len(accs),
		"account_active":    active,
		"account_disabled":  disabled,
		"endpoint_total":    len(eps),
		"endpoint_enabled":  epEnabled,
		"endpoint_circuit":  epCircuit,
		"endpoint_halfopen": epHalfOpen,
		"model_count":       modelCount,
		"subkey_count":      subkeyCount,
		"total_requests":    totalReq,
		"total_tokens":      totalTokens,
		"total_cost":        totalCost,
		"cost_24h":          cost24h,
		"accounts":          accs,
		"endpoints":         eps,
	})
}

// handleUsageSeries 返回子 Key × 模型 的按小时用量序列（最近 24 小时），供总览页绘图。
func (a *Admin) handleUsageSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}
	series, err := a.store.QueryUsageSeries(hours)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, 200, series)
}

// ─────────────────────────── 工具 ───────────────────────────

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// rawFieldOf 读取请求体里某个字段的**原始 JSON 字节**（第三个返回值表示字段是否存在）。
//
// 为什么需要它：handler 为了做「部分更新」把请求体解成 map[string]any，
// 而 any 里的数字是 float64——大整数（如 12345678901234567890）会被舍入，
// 且这种丢失发生在进入业务逻辑之前，业务层再用 RawMessage 也救不回来。
// 对「值必须保真」的字段（默认请求体参数）要单独按原始字节取一次。
//
// 代价：请求体被读一遍到内存（管理端请求体很小，可接受）。
func rawFieldOf(body []byte, field string) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	raw, ok := m[field]
	return raw, ok
}

// readAllBody 把请求体读进内存（受 MaxBytesReader 保护的上游已限制大小）。
//
// 存在的理由：同一个请求体要用两种方式解（any 做部分更新、RawMessage 保精度），
// 而 r.Body 只能读一次。
func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(r.Body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if code >= 400 {
		log.Printf("admin: error %d: %+v", code, v)
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("admin: write json: %v", err)
	}
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// intField 把 JSON 解码得到的数字（float64/json.Number）安全转 int。
func intField(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

// int64Field 把 JSON 解码得到的数字安全转 int64（上限字段用）。
func int64Field(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// floatField 把 JSON 解码得到的数字安全转 float64（价格字段用）。
func floatField(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// stringField 取字符串字段（非字符串类型返回空串，由调用方决定是否视为非法）。
func stringField(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// stringSlice 把 JSON 解码得到的 []any 安全转成 []string，跳过非字符串与空串。
// 显式返回非 nil 空切片，使「清空 fallback 链」能被正确持久化为 []。
func stringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// validErrorKind 校验日志筛选的失败原因取值（空 = 不筛）。
// 用白名单而不是直接拼进 SQL：错误分类是枚举，非法值应明确报 400 而不是静默返回空结果
// ——后者会让「筛出来是空的」被误读成「没有这类失败」。
func validErrorKind(k string) bool {
	switch k {
	case "", "upstream_error", "upstream_timeout", "client_invalid", "client_cancel",
		"local_error", "unclassified":
		return true
	}
	return false
}

// validateQuotaFields 校验配额字段，返回错误文案（空串 = 通过）。
//
// 为什么要在写库前整体校验而不是逐字段静默夹紧：配额的三个部分
// （周期 / 重置时刻 / 缓存倍率）互相影响——若只写一半（例如接受了非法周期
// 但拒绝了倍率），子 Key 会处于管理员没预期的组合状态，且限额会立刻按这个
// 组合生效。整体拒绝让「保存失败 = 什么都没变」成立。
func validateQuotaFields(period string, weekday, hour int, readPM, writePM int64) string {
	// 周期：空串等价 day（旧行不带该字段），只拒绝真正未知的值。
	switch period {
	case "", model.QuotaPeriodDay, model.QuotaPeriodWeek, model.QuotaPeriodMonth:
	default:
		return "配额周期只支持 day / week / month"
	}
	// ISO 8601：1=周一 .. 7=周日；0 = 未设置（等价周一）。
	if weekday < 0 || weekday > 7 {
		return "重置星期需在 0~7 之间（1=周一 .. 7=周日，0=默认周一）"
	}
	if hour < 0 || hour > 23 {
		return "重置时刻需在 0~23 之间"
	}
	// 倍率上限 100x：不设经济学约束，只挡住明显的手滑（例如把 1000 当成 1.0x
	// 却多打了一个 0）。下限允许 0（= 未设置 → 用默认）。
	if readPM < 0 || readPM > maxQuotaPermille {
		return "缓存读取倍率需在 0~100000 之间（千分比，1000 = 1.0x；0 = 用默认 0.1x）"
	}
	if writePM < 0 || writePM > maxQuotaPermille {
		return "缓存写入倍率需在 0~100000 之间（千分比，1000 = 1.0x；0 = 用默认 1.25x）"
	}
	return ""
}
