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
	"log"
	"net/http"
	"strings"
	"time"

	"arkgate/internal/balancer"
	"arkgate/internal/config"
	"arkgate/internal/model"
	"arkgate/internal/store"
)

// Portal 子 Key 自助门户。
type Portal struct {
	store   *store.Store
	bal     *balancer.Balancer
	cfg     *config.Config
	handler http.Handler
}

// New 构造门户。cfg 用于下发展示币种（单价与成本仍是美元存储，只做展示换算）。
func New(st *store.Store, bal *balancer.Balancer, cfg *config.Config) *Portal {
	p := &Portal{store: st, bal: bal, cfg: cfg}
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

	// 当前**配额周期**用量。
	//
	// 为什么不再用 GetDailyUsage：周期可为周/月，day 列存的是窗口起始日，
	// 直接取「今天」在周/月周期下永远读不到行 → 限额进度条永远是 0。
	// 门户必须与限额判定同一口径，否则用户看到的进度与实际拦截不一致。
	quotaRule := model.QuotaRuleOf(sk)
	winDay := model.WindowDay(quotaRule, time.Now())
	today, err := p.store.GetWindowUsage(sk.ID, winDay)
	if err != nil {
		today = &store.DailyUsage{}
	}
	// 同一周期的**上游原始 token**：与 today.tokens（加权配额）并列下发。
	// 两个数派生自不同数据源（usage_daily / usage_logs），只有真实的原始值
	// 才能让用户看懂「额度为什么扣得比总量少」。
	// 下界用真实窗口起点（含 ResetHour 偏移），与 usage_daily 的行归属一致。
	winRawTokens := int64(0)
	if raw, rerr := p.store.SubKeyRawTokensSince(map[string]int64{
		sk.ID: model.WindowStartTime(quotaRule, time.Now()).Unix(),
	}); rerr == nil {
		winRawTokens = raw[sk.ID]
	} else {
		// 不阻断门户：缺失时前端会回落为不展示该对比行。
		log.Printf("portal: 读取子 Key %s 周期原始 token 失败: %v", sk.ID, rerr)
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

	// 缓存节省量：让用户能看出「缓存命中降了多少成本」。
	//
	// **必须按 same 区间做聚合查询，不能拿 rawLogs 算**：rawLogs 只有最近 100 条
	// 且以时间为序，而 week/total 是全量聚合——用 rawLogs 会让「7 天缓存读取 600」
	// 与「节省 $0」同时出现（历史上真出现过这个自相矛盾的响应）。
	//
	// 按模型分组是必要的：不同模型单价不同，拿总量乘单一单价算出来的节省是错的。
	// 单价取自**当前**模型配置；模型已删或未定价时不能当作 0，而是标记
	// cache_savings_priced=false，让界面显示「—」而不是「省了 $0」。
	cacheSavings, allPriced, anyCache := 0.0, true, false
	byModel, err := p.store.SubKeyCacheByModel(sk.ID, time.Now().Unix()-7*86400)
	if err == nil {
		for _, mc := range byModel {
			if mc.Read == 0 && mc.Write == 0 {
				continue
			}
			anyCache = true
			saved, priced := p.bal.CacheSavings(mc.Model, mc.Read, mc.Write)
			if !priced {
				allPriced = false
				continue
			}
			cacheSavings += saved
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":               sk.Name,
		"expires_at":         sk.ExpiresAt,
		"daily_limit_tokens": sk.DailyLimitTokens,
		"daily_limit_images": sk.DailyLimitImages,
		// 配额周期信息：界面据此把「今日」文案改成「本周/本月」，
		// 并展示重置时间与缓存倍率（让用户看得懂自己的额度怎么扣的）。
		"quota": map[string]any{
			"period":                  quotaRule.Period,
			"window_day":              winDay,
			"reset_hour":              quotaRule.ResetHour,
			"reset_weekday":           quotaRule.ResetWeekday,
			"limit_requests":          sk.WindowLimitRequests,
			"cache_read_permille":     quotaRule.ReadPermille,
			"cache_write_permille":    quotaRule.WritePermille,
		},
		"models":          models,
		"today":              today,
		// 本周期上游原始 token（与 today.tokens 的加权额度同周期），
		// 供前端直接对比，不再用缓存量估算。
		"today_raw_tokens":   winRawTokens,
		"week":               week,
		"total":              total,
		"success_rate_7d":    successRate,
		"logs":               logs,
		// 缓存节省：负值 = 省钱。anyCache=false 表示区间内无缓存用量，
		// allPriced=false 表示有模型未定价——两种情况下界面都应显示「—」而非 $0。
		"cache_savings":        cacheSavings,
		"cache_has_usage":      anyCache,
		"cache_savings_priced": allPriced,
		// 展示币种：子 Key 用户看的是自己的账单，必须和管理端同一口径，
		// 否则「管理员看到 ￥21、用户看到 $3」会被当成计费错误。金额本身
		// 仍是美元数值，换算由前端按汇率做。
		"currency": p.currency(),
	})
}

// currency 下发展示币种设置。非敏感信息（汇率与符号），可安全给子 Key 用户。
func (p *Portal) currency() map[string]any {
	if p.cfg == nil || p.cfg.Currency == nil {
		return map[string]any{"rate": 0, "symbol": "$", "code": "USD"}
	}
	return map[string]any{
		"rate":   p.cfg.Currency.Rate(),
		"symbol": p.cfg.Currency.Symbol(),
		"code":   p.cfg.Currency.Code(),
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 客户端提前断开等场景，忽略即可。
		_ = err
	}
}
