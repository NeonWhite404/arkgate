// 用量分析预聚合（rollup）。
//
// 背景：QueryUsage 每次都在 usage_logs 上跑实时 GROUP BY。数据量上去后，前端每点
// 一次 facet、切一次指标都要全表扫一遍。这里用「小时 + 天」两级滚动聚合表把它
// 变成小表查询（对标 sub2api 的 usage_dashboard_hourly/daily）。
//
// 三条设计红线（改这里前先读）：
//
//  1. **rollup 只是加速器，不是真源**。任何 rollup 覆盖不了的查询都必须回落原始表
//     （QueryUsage 里的 rollupCovers 判断）。宁可走慢路径，也不能给出错的数。
//
//  2. **迟到日志窗口**。balancer.Record 是异步落库的（statCh/logCh 是 4096 缓冲
//     channel，由独立 goroutine 消费），所以一条 ts=T 的日志可能在 T+数秒才进表。
//     聚合器的推进区间必须回看足够长（rollupLookback），否则「尚未落库」会被误判
//     成「该时段无用量」，watermark 一旦越过就**永久少计**——这是本项目比 sub2api
//     更危险的地方（sub2api 是同步写 PG，无此问题）。
//
//  3. **幂等重放**。聚合全部走 INSERT ... ON CONFLICT DO UPDATE（upsert），同一区间
//     重复聚合结果一致。因此 watermark 落后时直接重跑即可自愈，不需要补偿逻辑。
package store

import (
	"database/sql"
	"fmt"
	"time"

	"arkgate/internal/model"
)

// rollupLookback 聚合回看窗口。必须显著大于「日志从产生到落库」的最坏延迟。
//
// 为什么不只考虑延迟：AggregateHourly 会把起点向前对齐到整点并**重算整个小时桶**
// （否则同一小时会被切成两段、upsert 互相覆盖）。因此只要水位落后不足一小时，
// 当前小时的迟到日志还能被下一轮兜住；但一旦水位越过某小时——该小时就再也不会
// 被重算。所以回看窗口至少要是「一小时 + 延迟余量」，才能保证「上一个完整小时」
// 始终在重算范围内（TestLateLogNotLost 锁定该不变量）。
const rollupLookback = 65 * time.Minute

// rollupDims 参与预聚合的维度（含全局合计）。
// 与 usageDims 的区别：这里多一个 "" 表示全局行（不带实体），
// 且顺序固定——聚合器按此顺序逐维度跑一次 GROUP BY。
var rollupDims = []struct {
	kind string // dim_kind 值（与 UsageQuery.Dim 同名）
	expr string // SQL 表达式（选出 dim_key）
}{
	{"", "''"}, // 全局合计
	{"model", "model"},
	{"subkey", "subkey_id"},
	{"account", "account_id"},
	{"endpoint", "endpoint_id"},
	{"provider", "provider"},
}

// rollupAggCols 聚合列清单（小时表与天表结构一致，共用插入语句模板）。
const rollupAggCols = `requests, success, upstream_errors, client_errors,
	prompt_tokens, completion_tokens, total_tokens, images,
	cost, input_cost, output_cost, cache_cost,
	cache_creation_tokens, cache_read_tokens,
	stream_requests, first_token_ms_sum, latency_ms_sum`

// rollupAggExpr 与 rollupAggCols 一一对应的聚合表达式。
//
// 上游/客户端错误的分母口径必须与 QueryUsage 的实时查询完全一致，否则「预聚合命中」
// 与「回落原始表」会给出不同的成功率（同一批数据两种答案是最难排查的 bug）：
//   - 上游错误 = 非 ok 且（无分类的历史行 或 upstream_error/upstream_timeout）
//   - 客户端错误 = 非 ok 且（client_invalid/client_cancel/local_error）
const rollupAggExpr = `COUNT(*),
	COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN status<>'ok' AND (error_kind='' OR error_kind IN ('upstream_error','upstream_timeout')) THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN status<>'ok' AND error_kind IN ('client_invalid','client_cancel','local_error') THEN 1 ELSE 0 END),0),
	COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0),
	COALESCE(SUM(image_count),0),
	COALESCE(SUM(cost),0), COALESCE(SUM(input_cost),0), COALESCE(SUM(output_cost),0), COALESCE(SUM(cache_cost),0),
	COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
	COALESCE(SUM(is_stream),0),
	COALESCE(SUM(CASE WHEN status='ok' AND is_stream=1 THEN first_token_ms ELSE 0 END),0),
	COALESCE(SUM(latency_ms),0)`

// RollupWatermark 返回当前已聚合到的时间点（0 = 从未聚合过）。
func (s *Store) RollupWatermark() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var wm int64
	err := s.db.QueryRow(`SELECT watermark_ts FROM usage_rollup_state WHERE id=1`).Scan(&wm)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return wm, err
}

// AggregateHourly 把 [from, to) 的原始日志聚合进小时表，覆盖全部维度。
//
// 幂等：每行都是 upsert，重复调用同一区间结果一致。返回写入的维度行数（仅用于观测）。
// 注意 hourFrom 取整点：否则同一小时会被 from 切成两段，upsert 时互相覆盖导致少计
//（不是相加）——这是本函数最容易被改错的地方。
func (s *Store) AggregateHourly(from, to int64) (int64, error) {
	if from >= to {
		return 0, nil
	}
	// 对齐到整点：让每个桶的区间端点稳定，保证重复聚合能覆盖同一批行。
	from = from - from%3600

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64
	for _, d := range rollupDims {
		res, err := tx.Exec(`INSERT INTO usage_rollup_hourly
			(bucket_start, dim_kind, dim_key, `+rollupAggCols+`)
			SELECT (ts/3600)*3600, ?, `+d.expr+`, `+rollupAggExpr+`
			FROM usage_logs WHERE ts >= ? AND ts < ?
			GROUP BY (ts/3600)*3600, `+d.expr+`
			ON CONFLICT(bucket_start, dim_kind, dim_key) DO UPDATE SET
				requests=excluded.requests, success=excluded.success,
				upstream_errors=excluded.upstream_errors, client_errors=excluded.client_errors,
				prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens,
				total_tokens=excluded.total_tokens, images=excluded.images,
				cost=excluded.cost, input_cost=excluded.input_cost,
				output_cost=excluded.output_cost, cache_cost=excluded.cache_cost,
				cache_creation_tokens=excluded.cache_creation_tokens,
				cache_read_tokens=excluded.cache_read_tokens,
				stream_requests=excluded.stream_requests,
				first_token_ms_sum=excluded.first_token_ms_sum,
				latency_ms_sum=excluded.latency_ms_sum`,
			d.kind, from, to)
		if err != nil {
			return 0, fmt.Errorf("rollup hourly dim=%q: %w", d.kind, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// AggregateDailyFromHourly 由小时表重算 [from, to) 覆盖的本地自然日，写入天表。
//
// 为什么从天表不从原始表：天表要按本地日分组，而本地日不是固定 86400 秒（DST），
// 直接在原始表上按天分桶需要为每个日边界算偏移（见 usageBucketExpr）。既然小时表
// 已经有每小时的量，这里改成「按本地时区把小时桶归到所属日」——小时是 UTC 整点，
// 归属本地日只需看该小时起点的本地日期，DST 自动正确。
//
// 注意：dayFrom/dayTo 必须是「本地日字符串」范围，且端点按日对齐（含边界当天整天），
// 因为天表的主键是日期，重算必须覆盖整日才能保证幂等（半个日会把同日另一半抹掉）。
func (s *Store) AggregateDailyFromHourly(from, to int64) (int64, error) {
	if from >= to {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 起止日各自归一到当地 00:00，再扩到「结束日的次日 00:00」，保证整日覆盖。
	loc := time.Local
	start := time.Unix(from, 0).In(loc)
	dayStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	end := time.Unix(to, 0).In(loc)
	// +1 天：to 当天也要整天重算（小时表里该日可能已有新数据）。
	dayEnd := time.Date(end.Year(), end.Month(), end.Day()+1, 0, 0, 0, 0, loc)

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64
	for _, d := range rollupDims {
		// 日期字符串由 SQLite 按本地偏移换算：date(bucket_start, 'unixepoch', 'localtime')
		// 与 Go 侧的 time.Local 一致（同一进程，时区相同）。
		res, err := tx.Exec(`INSERT INTO usage_rollup_daily
			(bucket_date, dim_kind, dim_key, `+rollupAggCols+`)
			SELECT date(bucket_start, 'unixepoch', 'localtime'), ?, dim_key,
				SUM(requests), SUM(success), SUM(upstream_errors), SUM(client_errors),
				SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens), SUM(images),
				SUM(cost), SUM(input_cost), SUM(output_cost), SUM(cache_cost),
				SUM(cache_creation_tokens), SUM(cache_read_tokens),
				SUM(stream_requests), SUM(first_token_ms_sum), SUM(latency_ms_sum)
			FROM usage_rollup_hourly
			WHERE dim_kind = ? AND bucket_start >= ? AND bucket_start < ?
			GROUP BY date(bucket_start, 'unixepoch', 'localtime'), dim_key
			ON CONFLICT(bucket_date, dim_kind, dim_key) DO UPDATE SET
				requests=excluded.requests, success=excluded.success,
				upstream_errors=excluded.upstream_errors, client_errors=excluded.client_errors,
				prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens,
				total_tokens=excluded.total_tokens, images=excluded.images,
				cost=excluded.cost, input_cost=excluded.input_cost,
				output_cost=excluded.output_cost, cache_cost=excluded.cache_cost,
				cache_creation_tokens=excluded.cache_creation_tokens,
				cache_read_tokens=excluded.cache_read_tokens,
				stream_requests=excluded.stream_requests,
				first_token_ms_sum=excluded.first_token_ms_sum,
				latency_ms_sum=excluded.latency_ms_sum`,
			d.kind, d.kind, dayStart.Unix(), dayEnd.Unix())
		if err != nil {
			return 0, fmt.Errorf("rollup daily dim=%q: %w", d.kind, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// RunRollupOnce 推进一次预聚合：聚合 [watermark-lookback, now) 后推进水位。
//
// 水位推进规则：**只推进到 now - lookback**，而不是 now。这样即使有日志迟到
//（ts 早于 now 但落库晚于本轮），下一轮的回看窗口仍能覆盖到它。这是防「永久少计」
// 的关键：水位永远落后于「可能还在路上的日志」的下界。
//
// 返回 (写入行数, 新水位)。
func (s *Store) RunRollupOnce(now int64) (int64, int64, error) {
	wm, err := s.RollupWatermark()
	if err != nil {
		return 0, 0, err
	}
	lookback := int64(rollupLookback / time.Second)
	// 首轮（wm=0）从原始表最早一条日志开始，避免漏掉全部历史。
	start := wm - lookback
	if wm == 0 {
		start = s.earliestUsageTS()
		if start == 0 {
			start = now - lookback // 无任何日志：从 now 起算，等价于空跑
		}
	}
	if start >= now {
		return 0, wm, nil
	}

	n, err := s.AggregateHourly(start, now)
	if err != nil {
		return 0, wm, err
	}
	dn, err := s.AggregateDailyFromHourly(start, now)
	if err != nil {
		return 0, wm, err
	}

	// 新水位 = now - lookback，保证下一轮仍能覆盖「现在可能仍在途」的日志。
	newWM := now - lookback
	if newWM < wm {
		newWM = wm // 单调不回退
	}
	if err := s.SetRollupWatermark(newWM, now); err != nil {
		return n + dn, wm, err
	}
	return n + dn, newWM, nil
}

// SetRollupWatermark 写入水位（单调：不会回退，避免并发调用把进度拉回去）。
func (s *Store) SetRollupWatermark(ts, updatedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO usage_rollup_state(id, watermark_ts, updated_at) VALUES(1,?,?)
		ON CONFLICT(id) DO UPDATE SET
			watermark_ts = MAX(usage_rollup_state.watermark_ts, excluded.watermark_ts),
			updated_at = excluded.updated_at`, ts, updatedAt)
	return err
}

// earliestUsageTS 返回最早一条日志的 ts（无日志返回 0）。
func (s *Store) earliestUsageTS() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ts int64
	if err := s.db.QueryRow(`SELECT COALESCE(MIN(ts),0) FROM usage_logs`).Scan(&ts); err != nil {
		return 0
	}
	return ts
}

// RebuildRollup 在 [from, to) 上重算预聚合（供管理端修复/回填）。
// 用于：改价后重算成本、时区修正、或发现口径问题后重建。
//
// 与 RunRollupOnce 的区别：范围显式给定，且**不推进水位**（重建历史不影响实时进度）。
func (s *Store) RebuildRollup(from, to int64) (int64, error) {
	if from >= to {
		return 0, fmt.Errorf("重建区间非法: from=%d to=%d", from, to)
	}
	n, err := s.AggregateHourly(from, to)
	if err != nil {
		return 0, err
	}
	dn, err := s.AggregateDailyFromHourly(from, to)
	if err != nil {
		return n, err
	}
	return n + dn, nil
}

// rollupCovers 判断查询区间是否已被预聚合完全覆盖。
//
// 条件：
//   - 水位非 0 且覆盖到 q.To（水位是「已聚合上界」）；
//   - 查询维度是受支持的枚举（否则回落原始表）；
//   - 实体过滤也支持（rollup 按 dim_key 存了每个实体）。
//
// 注意：水位语义是「now - lookback」，所以刚发生的数据总是走原始表——这正是
// 想要的（实时数据要准，历史数据要快）。
func (s *Store) rollupCovers(q UsageQuery) bool {
	wm, err := s.RollupWatermark()
	if err != nil || wm <= 0 {
		return false
	}
	// 水位覆盖到 to 才可用：to 在水位之后意味着有尚未聚合的数据。
	if q.To > wm {
		return false
	}
	if q.Dim != "" {
		found := false
		for _, d := range rollupDims {
			if d.kind == q.Dim {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// rollupDimKind 把 UsageQuery.Dim 映射到 dim_kind（"" 即全局行）。
func rollupDimKind(dim string) string {
	if dim == "" {
		return ""
	}
	for _, d := range rollupDims {
		if d.kind == dim {
			return d.kind
		}
	}
	return ""
}

// queryUsageFromRollup 从预聚合表回答查询。
// 语义必须与 queryUsageRaw 完全一致（同样的总量/时序/facet/错误分布口径），
// 否则「同一批数据两种答案」。任何一步出错都由调用方回落到原始表。
func (s *Store) queryUsageFromRollup(q UsageQuery) (*UsageQueryResult, error) {
	kind := rollupDimKind(q.Dim)
	res := &UsageQueryResult{Series: []*UsageBucket{}, Facets: []*UsageFacet{}, Errors: []*UsageErrorKind{}, Source: "rollup"}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 实体过滤：dim_key = entity。"全局行"（dim=""）不接受实体过滤（语义上无实体）。
	entityWhere := ""
	var entityArg []any
	if q.Dim != "" && q.Entity != "" {
		entityWhere = " AND dim_key = ?"
		entityArg = append(entityArg, q.Entity)
	}

	// 1) 总量（该维度下的合计；不带实体过滤时就是该维度的总和汇总）。
	// 注意：全局行（dim_kind=''）在所有维度下都唯一存在，因此总量查询无需 GROUP BY。
	row := s.db.QueryRow(`SELECT COALESCE(SUM(requests),0), COALESCE(SUM(success),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(images),0), COALESCE(SUM(cost),0),
			COALESCE(SUM(input_cost),0), COALESCE(SUM(output_cost),0), COALESCE(SUM(cache_cost),0),
			COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(upstream_errors),0), COALESCE(SUM(client_errors),0),
			COALESCE(SUM(stream_requests),0), COALESCE(SUM(first_token_ms_sum),0),
			COALESCE(SUM(latency_ms_sum),0)
		FROM usage_rollup_hourly
		WHERE dim_kind=?`+entityWhere+` AND bucket_start >= ? AND bucket_start < ?`,
		append(append([]any{kind}, entityArg...), q.From, q.To)...)
	if err := row.Scan(&res.Summary.Requests, &res.Summary.Success,
		&res.Summary.PromptTokens, &res.Summary.CompletionTokens,
		&res.Summary.TotalTokens, &res.Summary.Images, &res.Summary.Cost,
		&res.Summary.InputCost, &res.Summary.OutputCost, &res.Summary.CacheCost,
		&res.Summary.CacheCreationTokens, &res.Summary.CacheReadTokens,
		&res.Summary.UpstreamErrors, &res.Summary.ClientErrors,
		&res.Summary.StreamRequests, &res.Summary.FirstTokenMsSum,
		&res.Summary.LatencyMsSum); err != nil {
		return nil, err
	}

	// 2) 时序。天粒度走天表（按本地日，DST 安全），小时粒度走小时表。
	if q.Granularity == "day" {
		if err := s.rollupSeriesDaily(res, q, kind, entityWhere, entityArg); err != nil {
			return nil, err
		}
	} else {
		if err := s.rollupSeriesHourly(res, q, kind, entityWhere, entityArg); err != nil {
			return nil, err
		}
	}

	// 3) 维度实体分布（与原始表路径的 facets 同义）。
	//
	// 关键：**不受实体过滤影响**——原始表路径的 facets 查询只用区间、不带 entity
	// 条件（前端靠它渲染「当前维度下各实体的构成」再做下钻）。若这里带上 entity，
	// 下钻时列表会只剩当前选中项一行，下钻就没法继续往下走了。
	if q.Dim != "" {
		rows, err := s.db.Query(`SELECT dim_key, dim_key,
				COALESCE(SUM(requests),0), COALESCE(SUM(success),0),
				COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
				COALESCE(SUM(total_tokens),0), COALESCE(SUM(images),0), COALESCE(SUM(cost),0),
				COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(upstream_errors),0),
				COALESCE(SUM(input_cost),0), COALESCE(SUM(output_cost),0), COALESCE(SUM(cache_cost),0)
			FROM usage_rollup_hourly
			WHERE dim_kind=? AND bucket_start >= ? AND bucket_start < ?
			GROUP BY dim_key
			ORDER BY SUM(total_tokens) DESC, SUM(images) DESC
			LIMIT 200`, kind, q.From, q.To)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		pending := make([]*UsageFacet, 0, 16)
		for rows.Next() {
			f := &UsageFacet{}
			if err := rows.Scan(&f.Key, &f.Label, &f.Requests, &f.Success,
				&f.PromptTokens, &f.CompletionTokens, &f.TotalTokens, &f.Images, &f.Cost,
				&f.CacheReadTokens, &f.UpstreamErrors,
				&f.InputCost, &f.OutputCost, &f.CacheCost); err != nil {
				return nil, err
			}
			pending = append(pending, f)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// 先读完并关闭 rows，再查可读名：SQLite 是单连接（MaxOpenConns(1)），
		// 在遍历 rows 期间发起嵌套查询会拿不到连接而永久阻塞（曾把本包测试挂死）。
		_ = rows.Close()
		for _, f := range pending {
			if q.Dim == "subkey" || q.Dim == "account" {
				f.Label = s.resolveDimLabel(q.Dim, f.Key)
			}
			res.Facets = append(res.Facets, f)
		}
	}

	// 4) 错误分布：rollup 只存了「上游/客户端」两大类，不足以还原细分枚举。
	// 为保证与原始表口径一致，这里**不编造**细分，只给出两类小计（kind 用枚举名）。
	if res.Summary.UpstreamErrors > 0 {
		res.Errors = append(res.Errors, &UsageErrorKind{
			Kind: model.ErrorKindUpstreamError, Requests: res.Summary.UpstreamErrors})
	}
	if res.Summary.ClientErrors > 0 {
		res.Errors = append(res.Errors, &UsageErrorKind{
			Kind: model.ErrorKindClientInvalid, Requests: res.Summary.ClientErrors})
	}
	return res, nil
}

// rollupSeriesHourly 小时粒度的时序（直接从小时表取，天然对齐 UTC 整点）。
func (s *Store) rollupSeriesHourly(res *UsageQueryResult, q UsageQuery, kind, entityWhere string, entityArg []any) error {
	rows, err := s.db.Query(`SELECT bucket_start,
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(requests),0), COALESCE(SUM(success),0),
			COALESCE(SUM(images),0), COALESCE(SUM(cost),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(upstream_errors),0),
			COALESCE(SUM(input_cost),0), COALESCE(SUM(output_cost),0), COALESCE(SUM(cache_cost),0),
			COALESCE(SUM(first_token_ms_sum),0), COALESCE(SUM(stream_requests),0)
		FROM usage_rollup_hourly
		WHERE dim_kind=?`+entityWhere+` AND bucket_start >= ? AND bucket_start < ?
		GROUP BY bucket_start ORDER BY bucket_start ASC`,
		append(append([]any{kind}, entityArg...), q.From, q.To)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		b := &UsageBucket{}
		if err := rows.Scan(&b.Bucket, &b.PromptTokens, &b.CompletionTokens,
			&b.Requests, &b.Success, &b.Images, &b.Cost,
			&b.CacheReadTokens, &b.UpstreamErrors,
			&b.InputCost, &b.OutputCost, &b.CacheCost,
			&b.FirstTokenMsSum, &b.StreamRequests); err != nil {
			return err
		}
		res.Series = append(res.Series, b)
	}
	return rows.Err()
}

// rollupSeriesDaily 天粒度的时序。
//
// bucket 值统一转换成「本地日 00:00 的 UTC 秒」，与原始表路径（usageBucketExpr）
// 的输出口径一致——否则前端在同一张图上切换数据源时横轴会跳。
func (s *Store) rollupSeriesDaily(res *UsageQueryResult, q UsageQuery, kind, entityWhere string, entityArg []any) error {
	loc := time.Local
	// 日期范围按本地日边界转成字符串（含头含尾，因 bucket_date 是日粒度）。
	fromDay := time.Unix(q.From, 0).In(loc).Format("2006-01-02")
	// to 是右开区间：若 to 恰好是某日 00:00，则该日不应包含，日期上界要退一天。
	toT := time.Unix(q.To, 0).In(loc)
	if toT.Hour() == 0 && toT.Minute() == 0 && toT.Second() == 0 && q.To > q.From {
		toT = toT.AddDate(0, 0, -1)
	}
	toDay := toT.Format("2006-01-02")

	rows, err := s.db.Query(`SELECT bucket_date,
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(requests),0), COALESCE(SUM(success),0),
			COALESCE(SUM(images),0), COALESCE(SUM(cost),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(upstream_errors),0),
			COALESCE(SUM(input_cost),0), COALESCE(SUM(output_cost),0), COALESCE(SUM(cache_cost),0),
			COALESCE(SUM(first_token_ms_sum),0), COALESCE(SUM(stream_requests),0)
		FROM usage_rollup_daily
		WHERE dim_kind=?`+entityWhere+` AND bucket_date >= ? AND bucket_date <= ?
		GROUP BY bucket_date ORDER BY bucket_date ASC`,
		append(append([]any{kind}, entityArg...), fromDay, toDay)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		b := &UsageBucket{}
		if err := rows.Scan(&day, &b.PromptTokens, &b.CompletionTokens,
			&b.Requests, &b.Success, &b.Images, &b.Cost,
			&b.CacheReadTokens, &b.UpstreamErrors,
			&b.InputCost, &b.OutputCost, &b.CacheCost,
			&b.FirstTokenMsSum, &b.StreamRequests); err != nil {
			return err
		}
		t, err := time.ParseInLocation("2006-01-02", day, loc)
		if err != nil {
			continue // 脏日期跳过而不是整查询失败
		}
		b.Bucket = t.Unix()
		res.Series = append(res.Series, b)
	}
	return rows.Err()
}

// resolveDimLabel 把维度实体的内部 ID 还原成可读名（与原始表路径的展示口径一致）。
// 查不到就回落成 ID 本身——rollup 里的历史实体可能已被删除，不能因此报错。
//
// 调用方必须已持有 s.mu（读锁）：本函数**不再自锁**。Go 的 RWMutex 不允许递归读锁
// ——若有写者在两次 RLock 之间排队，第二次 RLock 会永久阻塞（实测会让本包测试挂死）。
func (s *Store) resolveDimLabel(dim, key string) string {
	if key == "" {
		return ""
	}
	var label string
	switch dim {
	case "subkey":
		if err := s.db.QueryRow(`SELECT name FROM subkeys WHERE id=?`, key).Scan(&label); err == nil && label != "" {
			return label
		}
	case "account":
		if err := s.db.QueryRow(`SELECT name FROM accounts WHERE id=?`, key).Scan(&label); err == nil && label != "" {
			return label
		}
	}
	return key
}

// RollupHealth 预聚合健康度（供管理端展示「统计延迟」）。
type RollupHealth struct {
	Watermark int64 `json:"watermark_ts"`
	UpdatedAt int64 `json:"updated_at"`
	// LagSeconds = now - watermark，反映统计落后实时多久。
	// 正常情况下应稳定在 rollupLookback 附近（5 分钟）；持续变大说明聚合器卡住。
	LagSeconds int64 `json:"lag_seconds"`
	// HourlyRows / DailyRows 便于判断聚合是否真的在写数据。
	HourlyRows int64 `json:"hourly_rows"`
	DailyRows  int64 `json:"daily_rows"`
}

// RollupHealthReport 读取预聚合健康度。
func (s *Store) RollupHealthReport(now int64) (*RollupHealth, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := &RollupHealth{}
	var updated int64
	err := s.db.QueryRow(`SELECT watermark_ts, updated_at FROM usage_rollup_state WHERE id=1`).
		Scan(&h.Watermark, &updated)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	h.UpdatedAt = updated
	if h.Watermark > 0 {
		h.LagSeconds = now - h.Watermark
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_rollup_hourly`).Scan(&h.HourlyRows); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_rollup_daily`).Scan(&h.DailyRows); err != nil {
		return nil, err
	}
	return h, nil
}

// RollupBucketForTest 读取聚合表中某个小时桶的行（dim_kind/dim_key 定位）。
// 仅供测试断言「迟到日志确实进了聚合表」，避免测试只看 QueryUsage 结果时
// 被「回落原始表」路径蒙混通过。生产代码不应调用。
func (s *Store) RollupBucketForTest(bucketStart int64, dimKind, dimKey string) (requests, promptTokens int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_ = s.db.QueryRow(`SELECT requests, prompt_tokens FROM usage_rollup_hourly
		WHERE bucket_start=? AND dim_kind=? AND dim_key=?`,
		bucketStart, dimKind, dimKey).Scan(&requests, &promptTokens)
	return
}
