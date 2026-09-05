// Package portal 实现子 Key 自助门户：终端用户用自己的 sk-xxx 登录，
// 查询自己的用量、限额与调用记录。
//
// 越权边界（设计红线）：门户只暴露「该子 Key 自己」的调用视角数据——
//   - 自身限额、当日用量、最近 7 天概况与成功率；
//   - 自己的请求日志（脱敏列：不含账号/供应商/上游模型标识等管理侧字段）；
//   - 按其白名单过滤后的可用模型名（与 /v1/models 同口径）。
//
// 绝不返回：管理令牌、上游账号信息（名称/Key/base_url）、其它子 Key 的任何
// 数据、上游模型标识（ep）、端点运行态。后端列级白名单兜底，前端只做展示。
package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/model"
	"arkgate/internal/store"
)

// Portal 子 Key 自助门户。
type Portal struct {
	store   *store.Store
	bal     *balancer.Balancer
	handler http.Handler
}

// New 构造门户。
func New(st *store.Store, bal *balancer.Balancer) *Portal {
	p := &Portal{store: st, bal: bal}
	p.handler = p.routes()
	return p
}

// Handler 返回 http.Handler。
func (p *Portal) Handler() http.Handler { return p.handler }

func (p *Portal) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/portal/overview", p.handleOverview)
	mux.HandleFunc("/api/portal/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "不支持的接口"})
	})
	return mux
}

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// authSubKey 校验请求者身份：与 /v1 调用同一鉴权口径（Bearer sk-xxx）。
// 门户新增请求头 X-Sub-Key 作为兼容备选。
func (p *Portal) authSubKey(r *http.Request) (*model.SubKey, error) {
	token := bearerToken(r)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Sub-Key"))
	}
	if token == "" {
		return nil, errors.New("缺少 API Key（Authorization: Bearer sk-xxx）")
	}
	sk, err := p.store.GetSubKeyByHash(hashKey(token))
	if err != nil {
		return nil, errors.New("无效的 API Key")
	}
	if !sk.Enabled {
		return nil, errors.New("该 API Key 已被禁用")
	}
	if sk.ExpiresAt > 0 && sk.ExpiresAt < time.Now().Unix() {
		return nil, errors.New("该 API Key 已过期")
	}
	return sk, nil
}

// modelAllowed 判断子 Key 是否可访问模型（与 gateway 同语义：空 = 全部）。
func modelAllowed(sk *model.SubKey, name string) bool {
	if len(sk.AllowedModels) == 0 {
		return true
	}
	for _, m := range sk.AllowedModels {
		if m == name {
			return true
		}
	}
	return false
}

// logEntry 门户日志条目：列级白名单，只含终端用户可见数据。
// 刻意**不含 error**——上游错误体可能带上游模型标识（ep-xxx）、账号线索、
// 请求 id 等运维信息，属于越权数据；终端用户看 status 判断成败即可，
// 具体原因找管理员（store.subKeyLogCols 已在查询层先拦一道）。
type logEntry struct {
	TS               int64   `json:"ts"`
	RequestedModel   string  `json:"requested_model"`
	Model            string  `json:"model"`
	Modality         string  `json:"modality"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	ImageCount       int64   `json:"image_count"`
	Cost             float64 `json:"cost"`
	Status           string  `json:"status"`
	LatencyMs        int64   `json:"latency_ms"`
}

// handleOverview 返回登录子 Key 的完整自助视图。
func (p *Portal) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "仅支持 GET/POST"})
		return
	}
	sk, err := p.authSubKey(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": err.Error()})
		return
	}

	// 可用模型：启用目录 ∩ 子 Key 白名单（与 /v1/models 同口径）。
	allModels, _ := p.store.ListModels()
	models := make([]string, 0, len(allModels))
	for _, m := range allModels {
		if !m.Enabled || !modelAllowed(sk, m.Name) {
			continue
		}
		models = append(models, m.Name)
	}

	// 当日用量（含成本）、最近 7 天概况、累计概况。
	today, err := p.store.GetDailyUsage(sk.ID)
	if err != nil {
		today = &store.DailyUsage{}
	}
	week, _ := p.store.SubKeyLogStats(sk.ID, time.Now().Unix()-7*86400)
	total, _ := p.store.SubKeyLogStats(sk.ID, 0)

	var successRate float64
	if week.Requests > 0 {
		successRate = float64(week.Success) / float64(week.Requests) * 100
	}

	// 近期调用记录（脱敏列）。
	rawLogs, _ := p.store.ListUsageLogsBySubKey(sk.ID, 100)
	logs := make([]logEntry, 0, len(rawLogs))
	for _, l := range rawLogs {
		logs = append(logs, logEntry{
			TS:               l.TS,
			RequestedModel:   l.RequestedModel,
			Model:            l.Model,
			Modality:         l.Modality,
			PromptTokens:     l.PromptTokens,
			CompletionTokens: l.CompletionTokens,
			TotalTokens:      l.TotalTokens,
			ImageCount:       l.ImageCount,
			Cost:             l.Cost,
			Status:           l.Status,
			LatencyMs:        l.LatencyMs,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":               sk.Name,
		"expires_at":         sk.ExpiresAt,
		"daily_limit_tokens": sk.DailyLimitTokens,
		"daily_limit_images": sk.DailyLimitImages,
		"models":             models,
		"today":              today,
		"week":               week,
		"total":              total,
		"success_rate_7d":    successRate,
		"logs":               logs,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 客户端提前断开等场景，忽略即可。
		_ = err
	}
}
