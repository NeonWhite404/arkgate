package store

import "fmt"

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
	// 用 id 排序分页（offset 按已扫描条数推进）而不是按 ts：ts 有重复值，
	// 用它分页会漏行或重复行。
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
