package store

import (
	"fmt"
	"time"
)

// 历史成本回填（billing backfill）。
//
// 背景：v14 修正了缓存计费——此前 prompt_tokens 是「含缓存命中的总量」，却被
// 整块乘输入单价，导致缓存读取被多收（Anthropic 约 10× 于应计）而缓存写入被漏收。
// 已落库的 cost 仍是旧口径。是否重算取决于用途：
//
//   - 若历史成本只用于「看趋势」，可不重算（新旧口径在同一天内不会混用，
//     跨升级点会有一个台阶）；
//   - 若历史成本会用于结算/对账，必须重算，否则账单虚高。
//
// 因此这是**显式触发**的操作，不在迁移里自动跑（迁移必须快且无副作用）。
//
// 幂等性：重算结果只依赖 usage_logs 的 token 列与当前模型定价，重复执行结果相同。
// 若模型定价以后又改了，再跑一次即可刷新。

// CostBackfillResult 回填结果（供管理端展示与核对）。
type CostBackfillResult struct {
	// Scanned 是区间内被检查的日志条数。
	Scanned int64 `json:"scanned"`
	// Updated 是成本确实发生变化的条数。
	Updated int64 `json:"updated"`
	// Unpriced 是命中「模型无定价」的条数——这些行的 cost 被置 0。
	// 单独报出来，因为把有 token 的记录算成 0 元比保留旧值更值得警惕：
	// 通常意味着模型已被删除或从未定价。
	Unpriced int64 `json:"unpriced"`
	// OldTotal / NewTotal 是区间内成本总额的前后对比，便于直观看到差异。
	OldTotal float64 `json:"old_total"`
	NewTotal float64 `json:"new_total"`
	// DryRun 为 true 时只统计不写入。
	DryRun bool `json:"dry_run"`
}

// CostFunc 按模型名与日志计算成本三拆分。返回 priced=false 表示该模型无定价。
//
// 由调用方（balancer）注入：定价表在 balancer 内存里，store 不应反向依赖它。
// 这样 store 保持「只负责读写」的职责，计价口径只有一处实现。
type CostFunc func(modelName string, prompt, completion, images, cacheRead, cacheWrite int64) (in, out, cache float64, priced bool)

// costBackfillBatch 是回填时分批读取的行。
type costBackfillRow struct {
	id                                 int64
	modelName                          string
	prompt, completion, images         int64
	cacheRead, cacheWrite              int64
	oldIn, oldOut, oldCache, oldTotal  float64
}

// BackfillCosts 按当前定价重算 [from, to) 区间内 usage_logs 的成本。
//
// 实现要点：
//   - **分批读取**（batch 条一事务）：历史日志可能上百万条，一次性读进内存会 OOM。
//   - **逐行比较后再写**：值没变的行不产生 UPDATE，重复执行几乎零成本。
//   - **单事务写入**：中途失败不会留下「算了一半」的区间（配合幂等可安全重跑）。
//   - 不触碰 rollup 表：调用方在回填后重算预聚合即可（见 admin 端点）。
func (s *Store) BackfillCosts(from, to int64, cost CostFunc, dryRun bool) (*CostBackfillResult, error) {
	if from >= to {
		return nil, fmt.Errorf("回填区间非法: from=%d to=%d", from, to)
	}
	if cost == nil {
		return nil, fmt.Errorf("缺少计价函数")
	}
	res := &CostBackfillResult{DryRun: dryRun}

	const batch = 5000
	for {
		rows, err := s.queryCostBackfillBatch(from, to, batch, res.Scanned)
		if err != nil {
			return res, err
		}
		if len(rows) == 0 {
			break
		}
		// 先在内存里算完整批，再开事务写——避免长事务持有写锁阻塞转发热路径。
		var updates []costUpdate
		for _, r := range rows {
			res.Scanned++
			in, out, cache, priced := cost(r.modelName, r.prompt, r.completion,
				r.images, r.cacheRead, r.cacheWrite)
			newTotal := in + out + cache
			if !priced {
				res.Unpriced++
				newTotal = 0
				in, out, cache = 0, 0, 0
			}
			res.OldTotal += r.oldTotal
			res.NewTotal += newTotal
			// 浮点比较用极小的容差：避免因精度噪声产生无意义的 UPDATE。
			if !costEqual(in, r.oldIn) || !costEqual(out, r.oldOut) ||
				!costEqual(cache, r.oldCache) || !costEqual(newTotal, r.oldTotal) {
				updates = append(updates, costUpdate{r.id, in, out, cache, newTotal})
			}
		}
		if !dryRun && len(updates) > 0 {
			if err := s.applyCostUpdates(updates); err != nil {
				return res, err
			}
		}
		res.Updated += int64(len(updates))
		if len(rows) < batch {
			break
		}
	}
	return res, nil
}

// costEqual 成本比较容差（1e-12 美元，远小于任何有意义的金额差异）。
func costEqual(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}

func (s *Store) queryCostBackfillBatch(from, to int64, limit int, offset int64) ([]costBackfillRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 分页用 OFFSET，按 id 排序。
	//
	// 为什么这样是安全的（曾误写成「必须用 id，用 ts 会漏行」，那是错的）：
	// UPDATE 只改 input_cost/output_cost/cache_cost/cost 列，**不碰排序键与
	// WHERE 条件（ts 区间）**，所以后续批次的排序结果与首批完全一致，
	// OFFSET 能精确跳过已处理行。只要这个前提成立，任何稳定排序都安全；
	// id 只是最自然的选择（唯一、不可变）。
	//
	// 反面情形：若将来改成按 cost 排序并同时改写 cost，排序会在批次间
	// 重排，OFFSET 就会漏行/重复行。已用 TestBackfillPaginationNoSkipNoDup
	// 锁定「每行恰好被处理一次」。
	rows, err := s.db.Query(`SELECT id, model, prompt_tokens, completion_tokens, image_count,
			cache_read_tokens, cache_creation_tokens, input_cost, output_cost, cache_cost, cost
		FROM usage_logs
		WHERE ts >= ? AND ts < ?
		ORDER BY id
		LIMIT ? OFFSET ?`, from, to, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []costBackfillRow
	for rows.Next() {
		var r costBackfillRow
		if err := rows.Scan(&r.id, &r.modelName, &r.prompt, &r.completion, &r.images,
			&r.cacheRead, &r.cacheWrite, &r.oldIn, &r.oldOut, &r.oldCache, &r.oldTotal); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// costUpdate 是一行待写入的成本（命名类型，避免匿名结构体在两处签名漂移）。
type costUpdate struct {
	id                    int64
	in, out, cache, total float64
}

func (s *Store) applyCostUpdates(updates []costUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`UPDATE usage_logs
		SET input_cost=?, output_cost=?, cache_cost=?, cost=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, u := range updates {
		if _, err := stmt.Exec(u.in, u.out, u.cache, u.total, u.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 编译期断言：CostFunc 与 balancer 注入的函数签名一致（避免两侧静默漂移）。
var _ CostFunc = func(string, int64, int64, int64, int64, int64) (float64, float64, float64, bool) {
	return 0, 0, 0, false
}

// RepairDailyCosts 按 usage_logs 重算 usage_daily 的成本列。
//
// 为什么需要单独修：usage_daily.cost 与 usage_logs.cost 是两个独立写入路径。
// 某次重构漏了把成本填进统计投递结构体，于是 usage_daily.cost 恒为 0，
// 而 usage_logs.cost 完全正常——门户「今日成本」卡片直接显示 $0，
// 但用户的周/总费用（取自日志聚合）是对的，同一页面自相矛盾。
// 修复代码只保证**今后**写对，已落库的 0 不会自己变，所以要能重算。
//
// 为什么与 BackfillCosts 分开：本函数不依赖模型定价（直接把日志里的 cost 加总），
// 因此即使模型已被删除也能修正——而 BackfillCosts 对未定价模型会置 0。
//
// 时区：窗口起点是**本地时区**的自然日（与 model.WindowDay 写入口径一致），
// 所以按本地时区分组，不能用 UTC（跨时区会把凌晨的请求算到前一天）。
//
// v15 后 usage_daily.day 存的是**窗口起始日**（可能是本周一/本月 1 号），
// 因此必须先按「行自身的窗口」聚合日志，不能用日志的自然日去对齐——
// 否则周周期下要么匹配不到行（WHERE day='周三的日期'），要么把整周成本
// 写成单日成本（当窗口起点恰好等于某个自然日时）。
//
// 窗口长度取**行上记录的 window_secs** 而不是子 Key 当前配置：
// 周期可以随时改，历史行的 day 对应的是当时配置下的窗口；
// 用当前配置反推会算错改过周期的那些历史行。
// window_secs=0 的历史行（v15 之前）按自然日 86400 处理（等价旧行为）。
func (s *Store) RepairDailyCosts(from, to int64, dryRun bool) (int64, error) {
	if from >= to {
		return 0, fmt.Errorf("区间非法: from=%d to=%d", from, to)
	}
	// 第一步：读出待修的行（day + window_secs），并算出每个窗口的时间区间。
	//
	// 单连接约束：必须在遍历 rows 之前把结果读完再执行后续查询——
	// 在 rows 未关闭时发起新的查询会因拿不到连接而永久阻塞。
	type winRow struct {
		day        string
		subkeyID   string
		windowSecs int64
	}
	var wins []winRow
	func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		rows, err := s.db.Query(`SELECT day, subkey_id, window_secs FROM usage_daily
			WHERE day >= ? AND day <= ?`, dayOf(from), dayOf(to))
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var w winRow
			if err := rows.Scan(&w.day, &w.subkeyID, &w.windowSecs); err != nil {
				return
			}
			wins = append(wins, w)
		}
	}()
	if len(wins) == 0 {
		return 0, nil
	}

	// 第二步：逐个窗口聚合日志成本。
	//
	// 按 (day, subkey) 单独聚合，而不是一次 GROUP BY day——因为每个子 Key 的
	// 窗口长度可能不同（有的按周、有的按日），不能用同一个分桶表达式。
	type winCost struct {
		day      string
		subkeyID string
		cost     float64
	}
	costs := make([]winCost, 0, len(wins))
	for _, w := range wins {
		secs := w.windowSecs
		if secs <= 0 {
			secs = 86400 // 历史行：等价自然日
		}
		start, err := time.ParseInLocation("2006-01-02", w.day, time.Local)
		if err != nil {
			continue // 脏 day 值：跳过而不是中断整次修复
		}
		end := start.Add(time.Duration(secs) * time.Second)
		var sum float64
		func() {
			s.mu.RLock()
			defer s.mu.RUnlock()
			_ = s.db.QueryRow(`SELECT COALESCE(SUM(cost),0) FROM usage_logs
				WHERE subkey_id=? AND ts >= ? AND ts < ?`,
				w.subkeyID, start.Unix(), end.Unix()).Scan(&sum)
		}()
		costs = append(costs, winCost{day: w.day, subkeyID: w.subkeyID, cost: sum})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 第三步：逐行对齐成本（只改成本，不动 tokens/images/requests）。
	//
	// 「不动其它列」是刻意的：tokens/images 一直是对的，重算它们反而有风险
	// （例如日志被清理后会把计数改小）。成本这一列才是本次要修的对象。
	//
	// 注意 v15 后 tokens 列存的是**加权配额计数**（不是真实 token），
	// 更不能拿日志重算。
	var fixed int64
	for _, c := range costs {
		if dryRun {
			// 预演：只统计差异，不写。
			var cur float64
			if err := tx.QueryRow(`SELECT COALESCE(cost,0) FROM usage_daily WHERE day=? AND subkey_id=?`,
				c.day, c.subkeyID).Scan(&cur); err == nil {
				if !costEqual(cur, c.cost) {
					fixed++
				}
			}
			continue
		}
		res, err := tx.Exec(`UPDATE usage_daily SET cost=? WHERE day=? AND subkey_id=?`,
			c.cost, c.day, c.subkeyID)
		if err != nil {
			return fixed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fixed += n
		}
	}
	if dryRun {
		return fixed, tx.Rollback()
	}
	if err := tx.Commit(); err != nil {
		return fixed, err
	}
	return fixed, nil
}

// dayOf 把 unix 秒转成本地时区的日期字符串，与 model.WindowDay 同一格式。
func dayOf(ts int64) string {
	return time.Unix(ts, 0).In(time.Local).Format("2006-01-02")
}
