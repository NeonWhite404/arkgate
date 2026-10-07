package model

import (
	"time"
)

// 配额周期（v15）。
//
// 设计约束（与需求对齐）：
//   - **同时只存在一个周期**——不做「每天 10 万 且 每周 50 万」的多周期叠加，
//     因此判定只需读一行，不需要 period 维度。
//   - **自然日是最小单位**——不引入小时级或滚动窗口。窗口起点因此永远落在
//     某个自然日的 00:00（可被 ResetHour 平移），于是仍可复用
//     usage_daily.day 存「窗口起始日」，无需新表。
const (
	QuotaPeriodDay   = "day"
	QuotaPeriodWeek  = "week"
	QuotaPeriodMonth = "month"
)

// 缓存倍率默认值（千分比）。基于内嵌目录 868 个含缓存读单价的模型实测：
// 读/输入中位 0.100（Anthropic 系 456 个正好 10%），写/输入中位 1.250。
const (
	DefaultCacheReadPermille  int64 = 100
	DefaultCacheWritePermille int64 = 1250
)

// NormalizeQuotaPeriod 归一化周期值，未知/空值一律回落自然日。
//
// 回落到 day 而不是报错：库里的旧行（v15 之前）该列为空串，
// 必须等价于升级前的「自然日」行为。
func NormalizeQuotaPeriod(p string) string {
	switch p {
	case QuotaPeriodWeek:
		return QuotaPeriodWeek
	case QuotaPeriodMonth:
		return QuotaPeriodMonth
	default:
		return QuotaPeriodDay
	}
}

// QuotaRule 是判定与计费共用的配额参数快照。
//
// 单独抽出来的原因：窗口对齐（本文件）与加权计数（balancer）都需要同一份参数，
// 分散在两处必然发散——例如一处用默认倍率、另一处用配置值。
type QuotaRule struct {
	Period       string // 归一化后的周期
	ResetWeekday int    // ISO 8601：1=周一 .. 7=周日（仅 week 生效）；已解析默认值
	ResetHour    int    // 0-23

	// 加权是否启用，以及已解析的倍率。
	//
	// **Weight 为 false 时倍率无效**，计数必须用原始的 prompt+completion。
	// 这不是默认值问题而是**兼容性**问题：旧子 Key（v15 之前创建）读出来
	// CacheReadPermille=0，若把 0 解释成「默认 0.1x」就会**静默减半它们的
	// 额度消耗**——一条 1000 prompt 的请求从记 1000 变成记…不对，是
	// 从 1000 变成 1000-cacheRead×(1-0.1)：总之配额消耗变小，用户能多发请求。
	// 实测确认过：旧式子 Key 的加权结果是 1170 而旧口径应为 2200。
	//
	// 所以加权是**显式选择加入**：只有管理员在界面上填过倍率（>0）才启用。
	// 这也让「0 = 未设置」在语义上真正等于「保持旧行为」。
	Weight        bool
	ReadPermille  int64 // 仅 Weight 为 true 时有意义
	WritePermille int64
}

// QuotaRuleOf 从子 Key 构造配额参数，解析默认值。
func QuotaRuleOf(sk *SubKey) QuotaRule {
	r := QuotaRule{
		Period:        NormalizeQuotaPeriod(sk.QuotaPeriod),
		ResetHour:     sk.QuotaResetHour,
		ReadPermille:  sk.CacheReadPermille,
		WritePermille: sk.CacheWritePermille,
	}
	// 加权启用条件：**两个倍率至少配置了一个**（>0）。
	// 未配置的那一个才取默认值——这样「只调读取倍率」也能按预期工作，
	// 写入倍率不至于被当成 0（=免费）。
	if r.ReadPermille > 0 || r.WritePermille > 0 {
		r.Weight = true
		if r.ReadPermille <= 0 {
			r.ReadPermille = DefaultCacheReadPermille
		}
		if r.WritePermille <= 0 {
			r.WritePermille = DefaultCacheWritePermille
		}
	}
	// ISO 8601：1=周一 .. 7=周日；0（未设置）与越界值一律回落周一。
	// 用 1..7 而不是 time.Weekday 的 0..6，是为了让「未设置」有一个
	// 不冲突的表示（0），否则零值构造与从库读出的对象语义会不一致。
	if sk.QuotaResetWeekday >= 1 && sk.QuotaResetWeekday <= 7 {
		r.ResetWeekday = sk.QuotaResetWeekday
	} else {
		r.ResetWeekday = 1 // 周一
	}
	// 越界的小时按 0 处理，不 panic——库里可能有手改的脏值。
	if r.ResetHour < 0 || r.ResetHour > 23 {
		r.ResetHour = 0
	}
	return r
}

// WindowStart 返回 at 所属窗口的起始自然日（本地时区）。
//
// 语义要点（三处易错，逐条说明）：
//
//  1. **ResetHour 先切分，再按周期回退**。顺序不能反：
//     若先回退到本周一、再比较 ResetHour，那么「周一 07:00」（ResetHour=8）
//     会先落到本周一、再因未到 8 点而回退一周 → 得到上周一，而正确答案是
//     上周一。看起来一样？不——对「周三 07:00」，先回退得周一、再判断 7<8
//     会再退一周得到**上周一**，但它其实属于「本周一 08:00 起」的窗口。
//     正确顺序：先用 ResetHour 求出「业务日」（7:00 → 前一天），再回退周期。
//
//  2. **month 的 ResetHour 回退可能跨月**。10 月 1 日 03:00（ResetHour=8）
//     的业务日是 9 月 30 日，窗口起点应是 9 月 1 日而非 10 月 1 日。
//     用 time.Date(y, m, 1, ...) 直接取当月 1 号会错。
//
//  3. **用 time.Date 归一化而非 AddDate 手算**。month 回退到「上月 1 号」
//     时，若用 AddDate(0,-1,0) 从 3 月 31 日出发会得到 3 月 3 日之类的结果。
//     统一用 time.Date(y, m, 1) 让标准库处理进位。
//
// 返回值为本地时区的零时刻日期，调用方用 formatDay 转成 "2006-01-02"。
func WindowStart(rule QuotaRule, at time.Time) time.Time {
	// 步骤 1：应用 ResetHour 得到「业务日」。
	biz := at
	if at.Hour() < rule.ResetHour {
		biz = at.AddDate(0, 0, -1)
	}
	y, m, d := biz.Date()
	loc := biz.Location()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, loc)

	switch rule.Period {
	case QuotaPeriodWeek:
		// 回退到最近的 ResetWeekday（含当天）。
		// time.Weekday 是 0=周日..6=周六，本字段是 ISO 8601 的 1=周一..7=周日，
		// 换算：iso = (int(wd)+6)%7 + 1。只在这一处做映射，避免散落各处走样。
		cur := (int(dayStart.Weekday())+6)%7 + 1
		delta := (cur - rule.ResetWeekday + 7) % 7
		return dayStart.AddDate(0, 0, -delta)
	case QuotaPeriodMonth:
		// 注意：dayStart 可能因 ResetHour 回退已跨月，这里取**它所在月**的 1 号。
		return time.Date(dayStart.Year(), dayStart.Month(), 1, 0, 0, 0, 0, loc)
	default:
		return dayStart
	}
}

// WindowDay 返回 at 所属窗口的起始日字符串（"2006-01-02"），即 usage_daily.day 的取值。
func WindowDay(rule QuotaRule, at time.Time) string {
	return WindowStart(rule, at).Format("2006-01-02")
}

// WindowStartTime 返回窗口的**精确起始时刻**（本地时区，含 ResetHour 偏移）。
//
// 与 WindowStart 的区别：WindowStart 返回的是「业务日的 00:00」，那是 usage_daily.day
// 的键值（行归属标记）；而真实窗口是在该业务日的 ResetHour 才切分的。
// 例如 ResetHour=8 时，周三 09:00 与周三 07:00 分别属于「周三 00:00」与「周二 00:00」
// 两个 day 键，但真实窗口起点分别是周三 08:00 与周二 08:00。
//
// 为什么需要它：要把同一周期的**上游原始用量**（按 usage_logs.ts 聚合）与
// usage_daily 的加权值放在一起对比，聚合下界必须是真实窗口起点。
// 直接用 WindowStart 会把业务日 00:00~ResetHour 之间的请求算进来，
// 而那些请求在 usage_daily 里归属的是**上一个窗口**——两边口径就错位了。
// ResetHour=0（默认）时两者完全相等。
func WindowStartTime(rule QuotaRule, at time.Time) time.Time {
	return WindowStart(rule, at).Add(time.Duration(rule.ResetHour) * time.Hour)
}

// WindowSecs 返回窗口的标称长度（秒），随行落库到 usage_daily.window_secs。
//
// 为什么落库而不是靠子 Key 当前配置反推：周期可以随时改，历史行的 day 对应的是
// **当时**配置下的窗口起点。把长度随行存储，行才自解释——否则清理/回填/审计
// 都无法判断某一行到底是日、周还是月。
//
// 用「标称值」而非真实天数差：month 长度随月份变化（28~31 天），
// 但调用方只需要知道「这是哪一档周期」，不需要精确秒数。
func WindowSecs(period string) int64 {
	switch NormalizeQuotaPeriod(period) {
	case QuotaPeriodWeek:
		return 7 * 86400
	case QuotaPeriodMonth:
		return 30 * 86400
	default:
		return 86400
	}
}

// QuotaCount 按倍率把上游用量折算为「配额计数」。
//
// 这是与计费口径（v14 splitCostOf）对称的设计：两者都先把 prompt 拆成
// 普通/缓存读/缓存写三段，只是权重不同——计费用单价，配额用倍率。
// 复用同一套夹紧逻辑（billablePrompt），避免「上游数据自相矛盾」时
// 配额与计费对同一份用量给出不同判断。
//
// **未启用加权时（rule.Weight == false）不拆分段，直接返回 prompt+completion。**
// 这是向后兼容的硬要求：旧子 Key 没配过倍率，必须与升级前逐字一致。
// 即使上游报告了缓存 token 也不能参与加权，否则同样一份请求在升级后会
// 突然变得「更省额度」。
//
// 返回值刻意是 int64：配额是整数额度，半个 token 无意义，且避免浮点累计误差。
func QuotaCount(rule QuotaRule, prompt, completion, cacheRead, cacheWrite int64) int64 {
	if !rule.Weight {
		total := prompt + completion
		if total < 0 {
			return 0
		}
		return total
	}
	plain, read, write := SplitPrompt(prompt, cacheRead, cacheWrite)
	total := plain + completion
	// 倍率是千分比，四舍五入。读/写的权重通常 <1 与 >1，方向相反，
	// 因此不能先合并再取整（会放大误差）。
	total += roundDiv(read*rule.ReadPermille, 1000)
	total += roundDiv(write*rule.WritePermille, 1000)
	if total < 0 {
		return 0
	}
	return total
}

// SplitPrompt 把 prompt_tokens 拆成「普通 / 缓存读 / 缓存写」三段，保证三者之和
// 恰好等于 prompt（不重不漏、不出负值）。
//
// 夹紧策略：**按比例收敛**（而非「优先保读」）。
//
// 为什么按比例：脏数据（缓存量之和 > prompt）时，优先保读会让 split 出来的
// 缓存读偏多、缓存写偏少。而缓存读便宜（典型 0.1x）、缓存写贵（典型 1.25x），
// 于是「偏多算读」系统性地**少计费/少扣配额**——方向对运营方不利，
// 且是静默的。按比例不偏袒任何一侧，不引入这种系统性偏差。
//
// 溢出：read * prompt 量级约为 (1e6)^2 = 1e12，int64 安全
//（真实 token 数远小于此，上游 single response 不可能到百万级缓存量）。
func SplitPrompt(prompt, cacheRead, cacheWrite int64) (plain, read, write int64) {
	if prompt < 0 {
		prompt = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	read, write = cacheRead, cacheWrite
	if read+write > prompt {
		total := read + write
		read = read * prompt / total
		write = prompt - read
	}
	return prompt - read - write, read, write
}

// roundDiv 计算 round(a / b)，用于千分比折算。
func roundDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	if a >= 0 {
		return (a + b/2) / b
	}
	return -((-a + b/2) / b)
}
