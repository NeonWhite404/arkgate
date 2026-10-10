// Package shareanalytics 实现「下游二次分发」的极值理论（EVT）判据。
//
// 模型定稿见 docs/downstream-sharing-detection.md。本包是该文档主判据（§2 极值理论 /
// §3 波动 CV）的**在线可调用**版本：把离线工具 tools/shareanalyze 里的纯数学部分
// 提取出来，供管理端页面按需查询（可调参数），不做任何 IO。
//
// 为什么单独成包而不是直接 import tools/shareanalyze：后者是 package main，
// 无法被网关引用；且它是只读离线工具（自带报告与图表渲染），与在线查询的
// 关注点不同。本包只保留**计算**，不含格式化与渲染。
//
// ── 判据（缺一不可） ──
//
//	分发 ⇔ 量级的返回期 T(max) > 阈值（默认 1000 天） ∧ 波动的 p 值 < α（默认 0.05）
//
// 只看量级会误报「重度个人用户」（他们也有繁忙期）；只看波动会误报「低频机器人」
// （量小且规律）。分发 = 量大且稳。
//
// ── 与前几版的根本差别（不要退回相对排名） ──
//
// 早期版本（v9/v10/v11）用**组间相对排名**，永远会选出一个「最高」的组，无法表达
// 「显著」。EVT 给出的是「这个量级由个人使用产生的概率」这一**绝对判据**。
package shareanalytics

import (
	"math"
	"sort"
)

// Params 是模型的可调参数。全部字段零值由 Normalize 收敛到文档定稿值，
// 因此调用方可以只覆盖关心的几项。
type Params struct {
	// QThreshold 是 GPD 的阈值分位（u = 样本的该分位）。
	// 标准做法要求 u 足够高以保证渐近性，同时留够超额点做估计；
	// P75 在 7~50 点样本上通常给出 6~12 个超额点，是可用下限。
	QThreshold float64 `json:"q_threshold"`
	// ReturnPeriodDays 是量级判据：返回期超过该值（天）视为显著异常。
	// 1000 天 ≈ 三年一遇，个人使用几乎不可能产生。
	ReturnPeriodDays float64 `json:"return_period_days"`
	// CVAlpha 是波动判据的单侧显著性水平（p 值上限，越小越严格）。
	CVAlpha float64 `json:"cv_alpha"`
	// ExcludeK 是粗筛的稳健上界倍数（中位数 + k×MAD）。
	ExcludeK float64 `json:"exclude_k"`
	// MinDays 是参与统计的最少天数（低于则弃用该组）。
	MinDays int `json:"min_days"`
	// ReliableDays 是「样本充足」门槛：低于该天数标注外推不可靠。
	ReliableDays int `json:"reliable_days"`
	// MinRounds/MaxRounds 控制迭代剔除（粗筛 → 拟合 → 精筛）的轮数上限。
	MaxRounds int `json:"max_rounds"`
}

// DefaultParams 返回文档定稿的默认参数。
func DefaultParams() Params {
	return Params{
		QThreshold:       0.75,
		ReturnPeriodDays: 1000,
		CVAlpha:          0.05,
		ExcludeK:         3.0,
		MinDays:          5,
		ReliableDays:     15,
		MaxRounds:        4,
	}
}

// Normalize 把越界/零值参数收敛到可用范围。
//
// 为什么收敛而不是报错：这些参数是**分析视角**而非网关行为，一个手滑的值
// （如 q_threshold=0）不该让整个页面查不出结果。收敛后在响应里回显实际生效值，
// 前端据此提示「参数已调整」。
func (p Params) Normalize() Params {
	d := DefaultParams()
	if p.QThreshold < 0.5 || p.QThreshold > 0.95 {
		p.QThreshold = d.QThreshold
	}
	if p.ReturnPeriodDays <= 0 {
		p.ReturnPeriodDays = d.ReturnPeriodDays
	}
	if p.ReturnPeriodDays > 1e9 {
		p.ReturnPeriodDays = 1e9
	}
	// α=0 会让「p < α」永假（波动判据静默失效），视同未设置。
	if p.CVAlpha <= 0 || p.CVAlpha > 0.5 {
		p.CVAlpha = d.CVAlpha
	}
	if p.ExcludeK <= 0 || p.ExcludeK > 20 {
		p.ExcludeK = d.ExcludeK
	}
	if p.MinDays < 3 || p.MinDays > 200 {
		p.MinDays = d.MinDays
	}
	if p.ReliableDays < 3 || p.ReliableDays > 400 {
		p.ReliableDays = d.ReliableDays
	}
	if p.MaxRounds < 1 || p.MaxRounds > 12 {
		p.MaxRounds = d.MaxRounds
	}
	return p
}

// GroupSeries 是一个观察单元（= 一个子 Key）的日用量序列。
//
// Days 是「按自然日（本地时区）的请求数」，由调用方聚合（store 侧复用
// usageBucketExpr 的 DST-safe 分桶，避免两套时间口径）。空值日不补 0：
// 需求里的「波动」指**使用日之间**的起伏，把「没用的日子」算成 0 会把
// 间歇性使用误判成高波动。
type GroupSeries struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Days  []float64 `json:"-"`
}

// GPDFit 是一次 GPD 拟合的结果。
type GPDFit struct {
	Threshold float64 `json:"threshold"` // 阈值 u
	N         int     `json:"n"`         // 样本总数
	Exceed    int     `json:"exceed"`    // 超额点数
	Zeta      float64 `json:"zeta"`      // ζ_u = P(X > u) 的经验估计
	Xi        float64 `json:"xi"`        // 形状参数 ξ
	Beta      float64 `json:"beta"`      // 尺度参数 β
	Valid     bool    `json:"valid"`
	Reason    string  `json:"reason,omitempty"`
	// Adjusted 表示 ξ 因病态估计被回退到 0（见 fitGPD 内注释）。
	Adjusted bool `json:"adjusted"`
	// UpperEndpoint 是分布支撑的上端点（ξ<0 时有限）。ξ≥0（重尾）时无有限上界，
	// 此处为 0 并由 Unbounded 标记——**刻意不返回 +Inf**：
	// encoding/json 拒绝序列化 Inf/NaN，一个 Inf 字段会让整个响应体变成空
	// （net/http 已发出 200 头，错误只能写进日志），前端只看到「无数据」
	// 而服务端毫无提示，是最难排查的一类静默失败。
	UpperEndpoint float64 `json:"upper_endpoint"`
	// Unbounded = ξ ≥ 0，尾部不衰减到 0（无有限上界），外推需谨慎。
	Unbounded bool `json:"unbounded"`
}

// CVFit 是波动（变异系数）分布的拟合结果。
type CVFit struct {
	Mu    float64 `json:"mu"`    // 正常组 CV 的对数均值
	Sigma float64 `json:"sigma"` // 对数标准差
	N     int     `json:"n"`
	Valid bool    `json:"valid"`
	// CritCV 是「显著平稳线」= exp(μ - 1.645σ)（单侧 p=0.05）。
	CritCV float64 `json:"crit_cv"`
}

// GroupResult 是单个子 Key 的分析结果。
type GroupResult struct {
	Key   string `json:"key"`
	Label string `json:"label"`

	// 量级维度
	Days        int     `json:"days"`
	LevelMedian float64 `json:"level_median"`
	LevelP90    float64 `json:"level_p90"`
	LevelMax    float64 `json:"level_max"`
	// 量级判据：最大日量对应的返回期（天）。
	//
	// 分布支撑上界被超过时（P=0）真实返回期为 +Inf，此处封顶为
	// maxReturnPeriod 并在 Infinite 标记——理由同 GPDFit.UpperEndpoint：
	// 不能把 +Inf 交给 encoding/json。封顶不影响判读（阈值默认 1000 天，
	// 封顶值远高于它），只影响排序与展示。
	ReturnPeriodDays float64 `json:"return_period_days"`
	Infinite         bool    `json:"infinite"`
	LevelPValue      float64 `json:"level_p_value"`

	// 波动维度
	CV    float64 `json:"cv"`
	CVPct float64 `json:"cv_pct"`

	// Flag 是该组的判读：flagged | observe | normal | insufficient。
	Flag string `json:"flag"`
	// Reliable 表示样本量足够（天数 ≥ ReliableDays）。
	Reliable bool `json:"reliable"`
	// Excluded 表示该组已被判为异常、剔出阈值估计（两阶段剔除的产物）。
	Excluded bool     `json:"excluded"`
	Notes    []string `json:"notes,omitempty"`
}

// maxReturnPeriod 是返回期的封顶值（天）。10^12 天远超任何判据阈值，
// 只用于把 +Inf 变成可序列化的有限值（见 GroupResult.ReturnPeriodDays）。
const maxReturnPeriod = 1e12

// Result 是一次完整分析的结果。
type Result struct {
	Params Params        `json:"params"`
	Fit    GPDFit        `json:"fit"`
	CV     CVFit         `json:"cv"`
	Groups []GroupResult `json:"groups"`
	// TotalDays 是参与拟合的日量样本总数（正常样本池）。
	TotalDays int `json:"total_days"`
	// Flagged 是按主判据（量级 ∧ 波动）判出的显著异常数；
	// Excluded 是粗筛颚出、仅用于保护拟合的组数。两者**不得混计**：
	// 粗筛刻意宽松，混在一起会让人误以为模型抓到了更多分发。
	Flagged  int `json:"flagged"`
	Excluded int `json:"excluded"`
	// Reliable 是样本充足的子 Key 数。
	Reliable int `json:"reliable"`
	// Evaluated 是参与分析的子 Key 数。
	Evaluated int `json:"evaluated"`
}

// Analyze 对全部子 Key 做极值分析。
//
// 关键设计：**阈值来自「个人使用」的分布，由全体组的日量样本合并估计**。
// 这样判据是「绝对量级」，不是「组间排名」——即使所有组都正常，也不会有组被判异常
// （除非其量级真的超出了分布的外推范围）。
//
// ⚠ 迭代剔除（实测必需）：若数据中已混入分发组，它们会把池子的上尾完全占满，
// 使 L-矩解出病态参数（实测 ξ=-2.154，外推完全失效）。因此先粗筛掉明显异常的组，
// 用**剔除后的纯正常样本**重新拟合，迭代至稳定。
func Analyze(series []GroupSeries, params Params) Result {
	p := params.Normalize()
	res := Result{Params: p, Groups: []GroupResult{}}

	// 只保留天数达标的组。
	type grp struct {
		key, label string
		days       []float64
	}
	var infos []grp
	for _, s := range series {
		if len(s.Days) < p.MinDays {
			continue
		}
		d := append([]float64(nil), s.Days...)
		sort.Float64s(d)
		infos = append(infos, grp{key: s.Key, label: s.Label, days: d})
	}
	if len(infos) == 0 {
		res.Fit = GPDFit{Reason: "无可用的组·日样本（各组天数均低于门槛）"}
		return res
	}
	res.Evaluated = len(infos)

	// 波动分布先算：CV 是组内归一化量，不受跨组量级差异污染。
	var allCVs []float64
	for _, inf := range infos {
		if len(inf.days) >= p.MinDays {
			if m := mean(inf.days); m > 0 {
				allCVs = append(allCVs, stddev(inf.days)/m)
			}
		}
	}
	res.CV = fitCV(allCVs, 1.645)

	// ── 粗筛（两阶段的第一阶段）──
	//
	// ⚠ 为何不能直接用「返回期 > 阈值」剔除（循环依赖）：
	// 剔除依赖返回期，返回期依赖 GPD 拟合，而拟合又被异常组污染 →
	// 返回期偏小、不触发剔除 → 拟合永远被污染（死循环）。
	//
	// 修法：粗筛用**不受少数大值影响的量**——组日量的中位数。
	// 分发组天天 5000，其组内中位就是 5000；正常组中位 ~300 → 明显可辨。
	var gmeds []float64
	for _, inf := range infos {
		gmeds = append(gmeds, median(inf.days))
	}
	coarseHi := 0.0
	if len(gmeds) >= 3 {
		coarseHi = robustUpper(gmeds, p.ExcludeK)
	}

	exclude := map[string]bool{}
	if coarseHi > 0 {
		for _, inf := range infos {
			if median(inf.days) > coarseHi {
				exclude[inf.key] = true
			}
		}
	}

	var fit GPDFit
	for round := 0; round < p.MaxRounds; round++ {
		var pool []float64
		for _, inf := range infos {
			if exclude[inf.key] {
				continue
			}
			pool = append(pool, inf.days...)
		}
		if len(pool) < 8 {
			fit = GPDFit{Reason: "正常样本不足 8 个日量，无法做极值外推"}
			break
		}
		fit = fitGPD(pool, p.QThreshold)
		if !fit.Valid {
			break
		}
		// 精筛：返回期极长 **且** 波动显著低才剔除。
		newly := 0
		for _, inf := range infos {
			if exclude[inf.key] {
				continue
			}
			rp := fit.ReturnPeriod(maxOf(inf.days))
			cvp := 1.0
			if res.CV.Valid {
				if m := mean(inf.days); m > 0 {
					cvp = res.CV.LogNormalPValue(stddev(inf.days) / m)
				}
			}
			if rp > p.ReturnPeriodDays && cvp < p.CVAlpha {
				exclude[inf.key] = true
				newly++
			}
		}
		if newly == 0 {
			break
		}
	}
	res.Fit = fit
	res.TotalDays = fit.N

	// ── 逐组汇总 ──
	for _, inf := range infos {
		r := GroupResult{
			Key:         inf.key,
			Label:       inf.label,
			Days:        len(inf.days),
			LevelMedian: median(inf.days),
			LevelMax:    maxOf(inf.days),
			LevelP90:    percentile(inf.days, 0.90),
			Excluded:    exclude[inf.key],
		}
		if m := mean(inf.days); m > 0 {
			r.CV = stddev(inf.days) / m
			r.CVPct = res.CV.LogNormalPValue(r.CV)
		}
		if fit.Valid {
			r.LevelPValue = fit.ExceedProb(r.LevelMax)
			r.ReturnPeriodDays = fit.ReturnPeriod(r.LevelMax)
			if math.IsInf(r.ReturnPeriodDays, 0) {
				r.ReturnPeriodDays = maxReturnPeriod
				r.Infinite = true
			}
		}
		r.Reliable = len(inf.days) >= p.ReliableDays
		if !r.Reliable {
			r.Notes = append(r.Notes, "仅 "+itoa(len(inf.days))+" 天样本，EVT 外推不可靠（建议 ≥ "+itoa(p.ReliableDays)+" 天）")
		}
		if fit.Adjusted {
			r.Notes = append(r.Notes, "ξ 估计病态已回退到 0（指数尾，保守假设）")
		}
		r.Flag = judge(r, fit, res.CV, p)
		if r.Flag == "flagged" {
			res.Flagged++
		}
		if r.Excluded {
			res.Excluded++
		}
		if r.Reliable {
			res.Reliable++
		}
		res.Groups = append(res.Groups, r)
	}
	// 返回期降序：值得看的排前面。+Inf 排最前。
	sort.SliceStable(res.Groups, func(i, j int) bool {
		return res.Groups[i].ReturnPeriodDays > res.Groups[j].ReturnPeriodDays
	})
	return res
}

// judge 给出单组的判读标签。
//
// 注意「样本不足」不是「正常」：把两者混为一谈会让管理员以为已经排查过了。
// 样本不足的组即使量级平淡也应显式标出——它只是**没被检验**。
func judge(r GroupResult, fit GPDFit, cvf CVFit, p Params) string {
	if !r.Reliable || !fit.Valid {
		return "insufficient"
	}
	// 量级达标（返回期超阈值）**且** 波动显著低 = 主判据命中。
	// 该判定对「粗筛已踢出」的组同样成立（返回期是对**清洗后的正常样本拟合**
	// 算的），所以不能因为被粗筛踢过就直接标成 excluded——那样反而把真正的
	// 分发样本降级成「仅粗筛」。
	levelHit := r.ReturnPeriodDays > p.ReturnPeriodDays
	cvHit := cvf.Valid && r.CVPct < p.CVAlpha && r.CV > 0
	if levelHit && cvHit {
		return "flagged"
	}
	// 粗筛踢出但未过主判据：量级明显高于 + k×MAD 但外推后并不极端。
	// 单独成标签——粗筛是刻意宽松的，把它当定论会高估模型。
	if r.Excluded {
		return "excluded"
	}
	// 量级达标但波动不够稳 → 「观察」而非「正常」：这类组多半是重度个人
	// 用户或批量评测，是最需要人工看一眼的灰区。
	if levelHit {
		return "observe"
	}
	// 波动显著低但量级不够 → 同样是灰区（低频规律机器人）。
	// 这正是「只看波动会误报」的对照：bot 类组会被标在这里，
	// 而不是当作分发结论。
	if cvHit {
		return "observe"
	}
	return "normal"
}

// ═══════════ GPD 拟合 ═══════════

// fitGPD 用 L-moments 估计 GPD 参数。
//
// 阈值选择是 EVT 最难的部分。这里用两条互补手段：
//  1. u = 样本的 QThreshold 分位（默认 P75）——足够高以保证渐近性，
//     同时留够超额点做估计。
//  2. 参数用 **L-moments 而非 MLE**（Hosking & Wallis 1987）：
//     L-矩对异常值稳健，小样本偏差远小于 MLE（MLE 在 ξ<-0.5 时甚至不存在）。
//     每组只有 7~50 天，稳健性比渐近效率重要。
func fitGPD(xs []float64, qThreshold float64) GPDFit {
	n := len(xs)
	f := GPDFit{N: n}
	if n < 8 {
		f.Reason = "样本仅 " + itoa(n) + " 个，EVT 外推不可靠（需 ≥8）"
		return f
	}
	s := append([]float64{}, xs...)
	sort.Float64s(s)

	u := percentile(s, qThreshold)
	var exc []float64
	for _, x := range s {
		if x > u {
			exc = append(exc, x-u)
		}
	}
	f.Threshold = u
	f.Exceed = len(exc)
	f.Zeta = float64(len(exc)) / float64(n)
	if len(exc) < 5 {
		f.Reason = "超额点仅 " + itoa(len(exc)) + " 个（阈值偏高），无法可靠估计"
		return f
	}

	// 样本 L-矩（无偏估计量，Hosking & Wallis 1987）：
	//   b0 = (1/n) Σ x_i
	//   b1 = (1/n) Σ ((i-1)/(n-1)) x_i        (i 从 1 计序)
	//   λ1 = b0,  λ2 = 2·b1 - b0
	m := len(exc)
	se := append([]float64{}, exc...)
	sort.Float64s(se)
	var b0, b1 float64
	for i, x := range se {
		b0 += x
		b1 += float64(i) / float64(m-1) * x
	}
	b0 /= float64(m)
	b1 /= float64(m)
	lambda1 := b0
	lambda2 := 2*b1 - b0
	if lambda2 <= 0 {
		f.Reason = "L-尺度 ≤ 0（样本退化）"
		return f
	}
	// GPD 的 L-矩反解：对 GPD(ξ,β) 有 λ1 = β/(1-ξ)、λ2 = β/((1-ξ)(2-ξ))，
	// 故 λ1/λ2 = 2-ξ → ξ = 2 - λ1/λ2，β = λ1(1-ξ)。
	xi := 2 - lambda1/lambda2
	beta := lambda1 * (1 - xi)

	// ⚠ ξ 的合理性约束（实测踩坑）：
	// L-矩反解在小样本 + 混合分布下会解出病态值。实测（4 正常 + 1 分发）：
	// ξ=-2.154、β=11434、上端点被压到 5712 → 返回期仅 12.7 天，外推完全失效。
	// GPD 的理论要求是 ξ > -1（ξ ≤ -1 时支撑上界 u - β/ξ ≤ u，与「超额点都 > u」
	// 矛盾）。处理：回退到 ξ = 0（指数尾）——这是**最保守的重尾假设**，
	// 即假设尾部衰减不比指数快，宁可低估上界（保守 = 宁可漏报）。
	const xiFloor = -1.0
	if xi <= xiFloor {
		xi = 0
		beta = lambda1 // ξ=0 时 λ1 = β
		f.Adjusted = true
	}
	if math.IsNaN(xi) || math.IsInf(xi, 0) || beta <= 0 {
		f.Reason = "参数估计退化（L-矩反解失败）"
		return f
	}
	f.Xi = xi
	f.Beta = beta
	f.Valid = true
	f.Unbounded = xi >= 0
	// 只在有有限上界时写该字段。ξ≥0 时 upperEndpoint 为 +Inf，
	// 直接赋值会让整个 JSON 响应序列化失败（见字段注释）。
	if up := f.upperEndpoint(); !math.IsInf(up, 0) && !math.IsNaN(up) {
		f.UpperEndpoint = up
	}
	return f
}

// ExceedProb 计算 P(X > x)（x 需 > 阈值）：
//
//	P(X > x) = ζ_u · [1 + ξ(x-u)/β]^(-1/ξ)
func (f GPDFit) ExceedProb(x float64) float64 {
	if !f.Valid || x <= f.Threshold {
		return 1
	}
	y := x - f.Threshold
	var p float64
	if math.Abs(f.Xi) < 1e-9 {
		p = math.Exp(-y / f.Beta)
	} else {
		base := 1 + f.Xi*y/f.Beta
		if base <= 0 {
			// 超出 GPD 支撑上界 → 该量级不可能由正常分布产生。
			return 0
		}
		p = math.Pow(base, -1/f.Xi)
	}
	return f.Zeta * p
}

// ReturnPeriod 返回期（天）：平均多少天出现一次这么大的量。
func (f GPDFit) ReturnPeriod(x float64) float64 {
	p := f.ExceedProb(x)
	if p <= 0 {
		return math.Inf(1)
	}
	return 1 / p
}

// upperEndpoint 分布支撑的上端点（ξ<0 时有限，ξ≥0 时无穷）。
func (f GPDFit) upperEndpoint() float64 {
	if f.Xi >= 0 {
		return math.Inf(1)
	}
	return f.Threshold - f.Beta/f.Xi
}

// ═══════════ 波动维度 ═══════════

// fitCV 用对数正态拟合正常组的 CV 分布（波动判据的下界来源）。
//
// 为何取对数：CV 为正数且右偏（个别组可能很高），对数变换后近似对称，
// 「均值减 kσ」才有意义；也不因单个极端 CV 把整个下界拉垮。
func fitCV(cvs []float64, z float64) CVFit {
	var logs []float64
	for _, c := range cvs {
		if c > 0 {
			logs = append(logs, math.Log(c))
		}
	}
	if len(logs) < 3 {
		return CVFit{}
	}
	mu := mean(logs)
	var sd float64
	for _, v := range logs {
		sd += (v - mu) * (v - mu)
	}
	sd = math.Sqrt(sd / float64(len(logs)-1))
	f := CVFit{Mu: mu, Sigma: sd, N: len(logs), Valid: true}
	f.CritCV = math.Exp(mu - z*sd)
	return f
}

// LogNormalPValue 返回「观测到 CV ≤ c」在正常分布下的概率。
// 极小值（如 <0.05）= 显著地比正常组平稳。
func (f CVFit) LogNormalPValue(c float64) float64 {
	if !f.Valid || c <= 0 || f.Sigma <= 0 {
		return 1
	}
	z := (math.Log(c) - f.Mu) / f.Sigma
	return normalCDF(z)
}

// ═══════════ 稳健统计工具 ═══════════

// robustUpper 用「中位数 + k×MAD」的稳健上界估计，并迭代剔除超出者。
//
// 为何用中位数 + MAD 而非均值 + 标准差：均值与标准差本身会被待检出的异常组拉动
// （一个 5000 的组能把均值抬到几百），而中位数与 MAD 的崩溃点约 50%、
// 不必假设分布形状。迭代剔除的理由：异常组占比较高时它们会把 MAD 抬高，
// 从而「自己定义正常范围」；每轮剔除超出者后重算，直到稳定。
//
// 这是**粗筛**（第一阶段）：只负责把量级明显异常的组挑出来，避免污染 GPD 拟合。
// 精度要求不高，宁可宽松。
func robustUpper(xs []float64, k float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cur := append([]float64{}, xs...)
	for round := 0; round < 5; round++ {
		if len(cur) < 3 {
			break
		}
		hi := median(cur) + k*scaleOf(cur)
		var keep []float64
		for _, v := range cur {
			if v <= hi {
				keep = append(keep, v)
			}
		}
		if len(keep) == len(cur) {
			return hi
		}
		cur = keep
	}
	return median(cur) + k*scaleOf(cur)
}

// scaleOf 是稳健尺度：MAD → IQR/1.349 → 1 逐级回退。
//
// IQR/1.349 是正态下的标准差估计（1.349≈2×0.6745），用于尺度对齐；
// 全部相同时回退到 1，避免上界退化成中位数本身。
func scaleOf(xs []float64) float64 {
	if s := medianAbsDev(xs); s >= 1e-9 {
		return s
	}
	if s := iqrOf(xs) / 1.349; s >= 1e-9 {
		return s
	}
	return 1
}

func medianAbsDev(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	med := median(xs)
	devs := make([]float64, 0, len(xs))
	for _, x := range xs {
		devs = append(devs, math.Abs(x-med))
	}
	return median(devs)
}

func iqrOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return percentile(xs, 0.75) - percentile(xs, 0.25)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func stddev(xs []float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	m := mean(xs)
	var s float64
	for _, x := range xs {
		d := x - m
		s += d * d
	}
	return math.Sqrt(s / float64(n-1))
}

func maxOf(xs []float64) float64 {
	m := math.Inf(-1)
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	if math.IsInf(m, -1) {
		return 0
	}
	return m
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	if len(cp) == 1 {
		return cp[0]
	}
	pos := p * float64(len(cp)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo < 0 {
		lo = 0
	}
	if hi >= len(cp) {
		hi = len(cp) - 1
	}
	if lo == hi {
		return cp[lo]
	}
	return cp[lo] + (cp[hi]-cp[lo])*(pos-float64(lo))
}

// normalCDF 标准正态分布函数（Go 标准库 erf，精度优于多项式近似）。
func normalCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
