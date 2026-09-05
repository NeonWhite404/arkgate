// Package admin 提供 /api/* 管理接口与访问令牌鉴权。
package admin

import (
	"crypto/rand"
	"crypto/sha256"
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
}

const tokenHashKey = "admin_token_hash"

// 运行时设置的持久化键（settings 表，值为秒；"0" = 关闭该超时）。
const (
	keyTimeoutRequest    = "timeout_request_sec"
	keyTimeoutFirstToken = "timeout_first_token_sec"
)

// New 构造管理后端，并把已持久化的运行时设置应用到 cfg
// （优先级：DB 持久化值 > 环境变量 > 内置默认）。
func New(st *store.Store, box *secure.Box, bal *balancer.Balancer, cfg *config.Config) *Admin {
	a := &Admin{store: st, box: box, bal: bal, cfg: cfg, catalog: catalog.New(), mgr: provider.NewManager()}
	a.loadPersistedTimeouts()
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
	return isTokenHash(tok) == stored
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
	reg("/api/endpoints", a.handleEndpointsCollection)
	reg("/api/endpoints/", a.handleEndpointItem)
	reg("/api/subkeys", a.handleSubkeysCollection)
	reg("/api/subkeys/", a.handleSubkeyItem)
	reg("/api/logs", a.handleLogs)
	reg("/api/stats", a.handleStats)
	reg("/api/overview", a.handleOverview)
	reg("/api/usage/series", a.handleUsageSeries)
	reg("/api/usage/stats", a.handleUsageStats)

	return mux
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
	res, err := a.store.QueryUsage(q)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
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
		if err := a.store.DeleteAccount(id); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
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
	if m.Router == nil || (len(m.Router.Rules) == 0 && m.Router.DefaultTarget == "") {
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
	def, ok := provider.Get(acc.Provider)
	if !ok {
		def = provider.FallbackDef(acc.Provider)
	}
	baseURL, err := def.ResolveBaseURL(acc.BaseURL)
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": "该账号未配置 base URL"})
		return
	}
	key, err := a.box.Decrypt(acc.ArkAPIKeyEnc)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": "账号密钥解密失败"})
		return
	}
	rt := provider.Route{Def: def, BaseURL: baseURL, Key: key}
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
	writeJSON(w, 200, map[string]any{"base_url": baseURL, "count": len(list), "models": list})
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
		if err := a.store.DeleteModel(name); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
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
		if e.AccountID == "" || e.Model == "" || e.EP == "" {
			writeJSON(w, 400, map[string]any{"detail": "account_id、model、ep 必填"})
			return
		}
		e.EP = strings.TrimSpace(e.EP)
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
		// 一次性解码到 map，兼顾「指针字段」与「字段覆盖」。
		var probe map[string]any
		if err := decode(r, &probe); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
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
		existing.Weight = intField(probe["weight"])
		existing.MaxConcurrency = intField(probe["max_concurrency"])
		existing.RPMLimit = intField(probe["rpm_limit"])
		existing.TPMLimit = int64(intField(probe["tpm_limit"]))
		existing.EP = strings.TrimSpace(existing.EP)
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
		}
		if err := decode(r, &p); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		key := strings.TrimSpace(p.Key)
		if key == "" {
			key = "sk-" + randHex(24)
		}
		key = store.NormalizeKey(key)
		sk := &model.SubKey{
			ID:               "sk_" + randHex(6),
			Name:             p.Name,
			Key:              key,
			KeyHash:          isTokenHash(key),
			Enabled:          true,
			AllowedModels:    p.AllowedModels,
			AllowedAccounts:  p.AllowedAccounts,
			DailyLimitTokens: p.DailyLimitTokens,
			DailyLimitImages: p.DailyLimitImages,
			CreatedAt:        time.Now().Unix(),
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
	logs, err := a.store.ListUsageLogs(limit, offset)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	total, err := a.store.CountUsageLogs()
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
	// 元组级健康度：启用数 / 熔断数（熔断按 ID 查内存运行态，
	// 快照副本不带 Runtime，不能直接判可用性）。
	var epEnabled, epCircuit int64
	for _, e := range eps {
		if e.Enabled {
			epEnabled++
		}
		if e.Enabled && a.bal.CircuitOpen(e.ID) {
			epCircuit++
		}
	}
	// 成本核算（来自 usage_logs.cost 聚合）。
	totalCost, cost24h, _ := a.store.SumCost()
	writeJSON(w, 200, map[string]any{
		"account_total":    len(accs),
		"account_active":   active,
		"account_disabled": disabled,
		"endpoint_total":   len(eps),
		"endpoint_enabled": epEnabled,
		"endpoint_circuit": epCircuit,
		"model_count":      modelCount,
		"subkey_count":     subkeyCount,
		"total_requests":   totalReq,
		"total_tokens":     totalTokens,
		"total_cost":       totalCost,
		"cost_24h":         cost24h,
		"accounts":         accs,
		"endpoints":        eps,
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
