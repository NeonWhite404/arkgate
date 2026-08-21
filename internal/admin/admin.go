// Package admin 提供 /api/* 管理接口与访问令牌鉴权。
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/model"
	"arkgate/internal/secure"
	"arkgate/internal/store"
)

// Admin 管理后端。
type Admin struct {
	store   *store.Store
	box     *secure.Box
	bal     *balancer.Balancer
	handler http.Handler
}

const tokenHashKey = "admin_token_hash"

// New 构造管理后端。
func New(st *store.Store, box *secure.Box, bal *balancer.Balancer) *Admin {
	a := &Admin{store: st, box: box, bal: bal}
	a.handler = a.routes()
	return a
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
	reg("/api/accounts", a.handleAccountsCollection)
	reg("/api/accounts/", a.handleAccountItem)
	reg("/api/models", a.handleModelsCollection)
	reg("/api/models/", a.handleModelItem)
	reg("/api/endpoints", a.handleEndpointsCollection)
	reg("/api/endpoints/", a.handleEndpointItem)
	reg("/api/subkeys", a.handleSubkeysCollection)
	reg("/api/subkeys/", a.handleSubkeyItem)
	reg("/api/logs", a.handleLogs)
	reg("/api/stats", a.handleStats)
	reg("/api/overview", a.handleOverview)

	return mux
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

// ─────────────────────────── accounts ───────────────────────────

type accountPayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ArkAPIKey string `json:"ark_api_key"` // 明文入参，仅在新增/更新时
	Status    string `json:"status"`
	Weight    int    `json:"weight"`
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
		if p.Name == "" || p.ArkAPIKey == "" {
			writeJSON(w, 400, map[string]any{"detail": "name 与 ark_api_key 必填"})
			return
		}
		id := p.ID
		if id == "" {
			id = "acc_" + randHex(6)
		}
		enc, err := a.box.Encrypt(strings.TrimSpace(p.ArkAPIKey))
		if err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		acc := &model.Account{
			ID: id, Name: p.Name, ArkAPIKeyEnc: enc,
			KeyHint: secure.Mask(strings.TrimSpace(p.ArkAPIKey)),
			Status:  defaultStr(p.Status, model.AccountActive),
			Weight:  defaultInt(p.Weight, 1),
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
		if p.ArkAPIKey != "" {
			enc, err := a.box.Encrypt(strings.TrimSpace(p.ArkAPIKey))
			if err != nil {
				writeJSON(w, 500, map[string]any{"detail": err.Error()})
				return
			}
			acc.ArkAPIKeyEnc = enc
			acc.KeyHint = secure.Mask(strings.TrimSpace(p.ArkAPIKey))
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
		m.Enabled = true // 新建默认启用
		if err := a.store.UpsertModel(&m); err != nil {
			writeJSON(w, 500, map[string]any{"detail": err.Error()})
			return
		}
		a.bal.Refresh()
		writeJSON(w, 200, map[string]any{"success": true})
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
		var m model.Model
		if err := decode(r, &m); err != nil {
			writeJSON(w, 400, map[string]any{"detail": err.Error()})
			return
		}
		m.Name = name
		if err := a.store.UpsertModel(&m); err != nil {
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
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		_ = json.Unmarshal([]byte(v), &limit)
	}
	logs, err := a.store.ListUsageLogs(limit)
	if err != nil {
		writeJSON(w, 500, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, 200, logs)
}

func (a *Admin) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"accounts":  a.bal.SnapshotAccounts(),
		"endpoints": a.bal.SnapshotEndpoints(),
	})
}

// handleOverview 返回总览数据：账号/元组运行态 + 模型数 + 子 Key 数。
func (a *Admin) handleOverview(w http.ResponseWriter, r *http.Request) {
	accs := a.bal.SnapshotAccounts()
	eps := a.bal.SnapshotEndpoints()
	models, _ := a.store.ListModels()
	subs, _ := a.store.ListSubKeys()

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
	// 元组级健康度：启用数 / 熔断数。
	var epEnabled, epCircuit int64
	for _, e := range eps {
		if e.Enabled {
			epEnabled++
		}
		if !a.bal.EndpointUsable(e) && e.Enabled {
			epCircuit++
		}
	}
	writeJSON(w, 200, map[string]any{
		"account_total":    len(accs),
		"account_active":   active,
		"account_disabled": disabled,
		"endpoint_total":   len(eps),
		"endpoint_enabled": epEnabled,
		"endpoint_circuit": epCircuit,
		"model_count":      len(models),
		"subkey_count":     len(subs),
		"total_requests":   totalReq,
		"total_tokens":     totalTokens,
		"accounts":         accs,
		"endpoints":        eps,
	})
}

// ─────────────────────────── 工具 ───────────────────────────

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
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
