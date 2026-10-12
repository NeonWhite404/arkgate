/* ArkGate 分发检测页（下游二次分发，仅管理可见）
 *
 * 模型定稿见 docs/downstream-sharing-detection.md：把「分发」定义为
 *   「量级显著超出个人使用」 ∧ 「波动显著低于个人使用」
 * 两个条件缺一不可——只看量级会误报重度个人用户（他们也有繁忙期），
 * 只看波动会误报低频机器人（量小且规律）。
 *
 * 量级用极值理论（EVT/POT）外推的**返回期**给出绝对判据：「这个日用量由个人使用
 * 产生的概率」＝ 1/返回期。这是本页存在的理由——早期版本用组间相对排名，
 * 永远会选出一个「最高」的组，无法表达「显著」。
 *
 * 本文件只含**页面组件与偏好读写**；四张图的渲染函数在 share-chart.js
 * （先于本文件加载，见 index.html 的 script 顺序）。
 * 本文件不注册组件，由 app.js 负责 app.component("ShareDetectPage", ...)。
 */
"use strict";

// 参数默认值与后端 shareanalytics.DefaultParams 必须一致（后端会回显实际生效值，
// 前端只用于表单初值；真正权威的是响应里的 result.params）。
const SD_DEFAULTS = {
  q_threshold: 0.75,
  return_period_days: 1000,
  cv_alpha: 0.05,
  exclude_k: 3,
  min_days: 5,
  reliable_days: 15,
  max_rounds: 4,
};
// min_req_per_day 不参与统计模型，只决定「哪些天算作使用日」，因此单独放。
const SD_DEFAULT_MINREQ = 5;

const SD_FLAGS = {
  flagged: { label: "显著异常", cls: "tag-red", tip: "量级与波动双判据同时命中" },
  excluded: { label: "粗筛偏高", cls: "tag-orange", tip: "量级明显高于正常组被前置剔除，但未过主判据；粗筛刻意宽松，不等于分发" },
  observe: { label: "观察", cls: "tag-blue", tip: "只命中一个判据：量大但抖，或量小但极稳" },
  normal: { label: "正常", cls: "tag-gray", tip: "两个判据都未命中" },
  insufficient: { label: "样本不足", cls: "tag-gray", tip: "天数不足，未做有效检验" },
};


const ShareDetectPage = {
  components: {},
  data() {
    const saved = sdLoadPrefs();
    return {
      from: toDateInput(new Date(Date.now() - 29 * 86400000)),
      to: toDateInput(new Date()),
      minReqPerDay: saved.minReqPerDay,
      form: saved.form,
      snap: JSON.stringify(saved.form),
      pOpen: false,
      r: null,
      // cd = 蜡烛数据（日 OHLC，单位 = 请求数）。与 r.trends 分开保存：
      // 前者是"日内采样得到的 OHLC"，后者是"日桶序列 + EVT 分析结果"，
      // 两者聚合粒度不同（见 admin.handleShareCandles）。
      cd: null,
      loading: false,
      tab: "scatter",
      // 趋势图的子 Key 参与开关：null = 全选（升级前后的默认视角一致）。
      // 只作用于「日用量趋势」——其余三张图的阈值是全局口径，收窄选中集合
      // 会让「显著平稳线」看起来变了，是两套语义，不能混。
      sel: saved.sel,
      selOpen: false,
      selQuery: "",
      tabs: [
        { v: "scatter", label: "极值判据", tip: "主判据：返回期 × 波动 CV，右上红区 = 显著异常" },
        { v: "timeline", label: "日用量趋势", tip: "日蜡烛：实体 = 当日首/末小时桶，影线 = 当日最高/最低小时桶（单位：请求数）；可勾选参与的子 Key" },
        { v: "box", label: "量级对比", tip: "各子 Key 日量的中位数、范围与最大值" },
        { v: "fit", label: "分布拟合", tip: "返回期是怎么算出来的：GPD 外推与判定位置" },
      ],
    };
  },
  computed: {
    res() { return (this.r && this.r.result) || null; },
    groups() { return (this.res && this.res.groups) || []; },
    fit() { return (this.res && this.res.fit) || {}; },
    cvf() { return (this.res && this.res.cv) || {}; },
    params() { return (this.res && this.res.params) || SD_DEFAULTS; },
    trends() { return (this.r && this.r.trends) || []; },
    // 蜡烛按子 Key 分组（保持后端排序；前端按 Bucket 对齐共享时间轴）。
    // 用 Map 保序：Object 的整数键会被重排，桶值是大整数，会打乱图的先后。
    candleGroups() {
      const m = new Map();
      ((this.cd && this.cd.candles) || []).forEach((c) => {
        if (!m.has(c.key)) m.set(c.key, { key: c.key, label: c.label || c.key, candles: [] });
        m.get(c.key).candles.push(c);
      });
      return Array.from(m.values());
    },
    // 选中集合（数组形式；null = 全选）。模板里到处要判断「是否选中」，
    // 统一走 selSet（避免每个调用点都把 null 特判一遍）。
    selAll() { return this.sel === null; },
    selSet() {
      const s = {};
      if (this.sel !== null) (this.sel || []).forEach((k) => { s[k] = 1; });
      return s;
    },
    selCount() { return this.selAll ? this.candleGroups.length : (this.sel || []).length; },
    // 选择器列表：按当前返回期降序（与明细表同序），带判定与搜索过滤。
    // 列表项来自**蜡烛分组**（趋势图的数据源），而非 trends——两边的子 Key
    // 集合可能不同（蜡烛只含区间内真的有过请求的 Key），若用 trends 作列表会
    // 出现"能勾选但没有蜡烛"或反之的错位。分组之外的 Key 一律不出现在列表里。
    selList() {
      const byKey = {};
      this.groups.forEach((g) => { byKey[g.key] = g; });
      const q = this.selQuery.trim().toLowerCase();
      return this.candleGroups
        .map((t) => {
          const g = byKey[t.key] || null;
          return {
            key: t.key, label: t.label || t.key,
            flag: g ? g.flag : "insufficient",
            // 返回期可能是「外推上界」（后端用 1e12 这类哨兵值表示无限），
            // 直接格式化会得到「1000000000000 天」这种读数。超过 1e9 视为无限。
            rp: g && isFinite(g.return_period_days) && g.return_period_days < 1e9 ? g.return_period_days : -1,
            median: g ? g.level_median : 0,
            days: t.candles.length,
          };
        })
        .filter((x) => !q || String(x.label).toLowerCase().indexOf(q) >= 0)
        .sort((a, b) => b.rp - a.rp);
    },
    // 趋势图的时间轴：**槽位化**——以区间内每个自然日为一个槽，缺日留空槽不补零。
    //
    // 时间轴按**全部蜡烛**构造（不受选中集合影响），这是有意的：
    //   - 勾选/取消子 Key 时 X 轴不应跳动（轴跟着选择变，就没有「对比前后两张图」的基准）；
    //   - 清空选中后轴仍存在，图框不会消失——否则点「清空」会看到整张图消失，
    //     看上去像功能坏了（实测反馈的「图为空」就是这个）。
    // Y 轴量程仍按选中集合算（那是「看谁就按谁定标」），两者职责不同。
    dayAxis() {
      // 时间轴取**蜡烛**的桶（不是 trends）：蜡烛是这张图唯一的数据源，
      // 轴的职责就是给每根蜡烛一个位置，两者同源才能保证不漏不重。
      const cd = this.cd;
      const seen = {};
      ((cd && cd.candles) || []).forEach((c) => { seen[c.bucket] = 1; });
      const uniq = Object.keys(seen).map((x) => parseInt(x, 10)).sort((a, b) => a - b);
      if (!uniq.length) return null;
      // 槽序列补齐：从首桶所在日逐自然日推进到尾桶所在日，中间空白日也占一个槽
      // （缺日留空不补零——「那天没人用」本身就是信息）。
      //
      // 关键：槽的 ts 必须**沿用后端给过的桶值口径**（后端用 time.Local 构造当日
      // 本地 00:00 的 UTC 秒），绝不能自己用 new Date(y,m,d) 重算：后者按**浏览器**
      // 时区构造，浏览器与服务器时区不一致时（服务器 UTC+8、浏览器 UTC 等）每个槽
      // 都会错开几小时，槽 ts 与桶值永远对不上——表现为「图例列出了子 Key、却一根
      // 蜡烛都没有」（实测踩坑）。
      //
      // 做法：起点直接用首桶 ts，步长固定 86400。日号（label）仅用于显示，由
      // 本地时间取月/日得出，不参与 ts 计算，因此它错了也不影响蜡烛能否命中。
      const seenSet = {};
      uniq.forEach((u) => { seenSet[u] = 1; });
      const slots = [];
      const last = uniq[uniq.length - 1];
      // guard 上限 400 天：与后端的区间上限（92 天）同量级，防脏数据把页面卡死。
      for (let guard = 0; guard < 400; guard++) {
        const ts = uniq[0] + guard * 86400;
        const cur = new Date(ts * 1000);
        slots.push({
          ts: ts,
          label: (cur.getMonth() + 1) + "/" + cur.getDate(),
          has: !!seenSet[ts],
        });
        if (ts >= last) break;
      }
      if (!slots.length) return null;
      return { min: uniq[0], max_t: last, n: slots.length, slots: slots };
    },
    counts() {
      const c = { flagged: 0, excluded: 0, observe: 0, normal: 0, insufficient: 0 };
      this.groups.forEach((g) => { c[g.flag] = (c[g.flag] || 0) + 1; });
      return c;
    },
    dirty() { return JSON.stringify(this.form) !== this.snap; },
    chartHtml() {
      if (!this.res) return "";
      if (this.tab === "box") return renderSDBox(this.groups, this.fit);
      if (this.tab === "timeline") return renderSDCandles(this.candleGroups, this.groups, this.dayAxis, this.sel);
      if (this.tab === "fit") return renderSDFit(this.groups, this.fit, this.params);
      return renderSDScatter(this.groups, this.fit, this.cvf, this.params);
    },
    // 趋势图卡片的标题：标出「已选 N / 共 M」，否则用户选完子 Key 再看图
    // 会以为是自己看错了（图里的形状变了却没有任何文字说明）。
    timelineTitle() {
      const tot = this.candleGroups.length;
      if (this.selAll) return "日用量蜡烛图（全部 " + tot + " 个子 Key）";
      return "日用量蜡烛图（已选 " + this.selCount + " / " + tot + " 个子 Key）";
    },
    fitSummary() {
      const f = this.fit;
      if (!f.valid) return f.reason || "拟合不可用";
      return "阈值 u=" + fmtInt(Math.round(f.threshold)) + " · 超额点 " + f.exceed + "/" + f.n +
        " · ζ_u=" + (f.zeta || 0).toFixed(3) + " · ξ=" + (f.xi >= 0 ? "+" : "") + (f.xi || 0).toFixed(3) +
        " · β=" + (f.beta || 0).toFixed(1);
    },
    upperBoundText() {
      const f = this.fit;
      if (!f.valid) return "";
      if (f.unbounded) return "ξ ≥ 0 → 重尾分布（无有限上界），外推需谨慎";
      return "ξ < 0 → 分布有上界：个人使用强度理论上限 ≈ " + fmtInt(Math.round(f.upper_endpoint)) + " 次/天";
    },
  },
  mounted() { this.load(); },
  methods: {
    flagMeta(g) { return SD_FLAGS[g.flag] || SD_FLAGS.normal; },
    fmtReturnPeriod,
    // ── 趋势图的子 Key 参与开关 ──
    // 内部统一存**数组**（null 表示全选），全选/空选/单选都走同一条路径，
    // 避免「null 与 [] 的语义分叉」。
    // sdPruneSel 清理已不存在的 key（改区间/换参数后可选集合会变）。
    // 少了这一步，选中集合残留旧 key 会表现为「选着 5 个但只画 3 条」，
    // 没有任何提示——是最难自证的一类 UI bug。
    sdPruneSel() {
      if (this.sel === null) return;
      const live = {};
      this.candleGroups.forEach((t) => { live[t.key] = 1; });
      const next = this.sel.filter((k) => live[k]);
      if (next.length !== this.sel.length) {
        this.sel = next.length ? next : null;
        sdSavePrefs({ form: this.form, minReqPerDay: this.minReqPerDay, sel: this.sel });
      }
    },
    sdToggle(key) {
      const cur = this.sel === null ? this.candleGroups.map((t) => t.key) : this.sel.slice();
      const i = cur.indexOf(key);
      if (i >= 0) cur.splice(i, 1); else cur.push(key);
      // 全部选中等价于「全选」（存储为 null）：后续新出现的子 Key 自动参与，
      // 不必再手动去勾一次。
      this.sel = cur.length === this.candleGroups.length ? null : cur;
      this.sdSaveSel();
    },
    sdSelectAll() { this.sel = null; this.sdSaveSel(); },
    sdSelectNone() { this.sel = []; this.sdSaveSel(); },
    // 仅显著：把命中主判据/粗筛的组一键选中（排查时最常用的一条捷径）。
    sdSelectFlagged() {
      const keys = this.selList.filter((x) => x.flag === "flagged" || x.flag === "excluded").map((x) => x.key);
      if (!keys.length) { toast("当前区间没有命中的子 Key", false); return; }
      this.sel = keys;
      this.sdSaveSel();
    },
    // Top N：列表已按返回期降序，取前 N 个（量级最高的几个）。
    sdSelectTop(n) {
      const keys = this.selList.slice(0, n).map((x) => x.key);
      this.sel = keys.length === this.candleGroups.length ? null : keys;
      this.sdSaveSel();
    },
    sdSaveSel() {
      sdSavePrefs({ form: this.form, minReqPerDay: this.minReqPerDay, sel: this.sel });
    },
    selColor(key) {
      const g = this.groups.find((x) => x.key === key);
      if (g && g.flag === "flagged") return "#b91c1c";
      if (g && g.flag === "excluded") return "#f59e0b";
      return PIE_COLORS[hashIdx(String(key), PIE_COLORS.length)];
    },
    load() {
      this.loading = true;
      const from = Math.floor(new Date(this.from + "T00:00:00").getTime() / 1000);
      const to = Math.floor(new Date(this.to + "T23:59:59").getTime() / 1000);
      const f = this.form;
      const qs = new URLSearchParams({
        from: String(from), to: String(to),
        min_req_per_day: String(this.minReqPerDay),
        q_threshold: String(f.q_threshold), return_period_days: String(f.return_period_days),
        cv_alpha: String(f.cv_alpha), exclude_k: String(f.exclude_k),
        min_days: String(f.min_days), reliable_days: String(f.reliable_days),
        max_rounds: String(f.max_rounds),
      });
      req("GET", "/api/share-detection?" + qs.toString())
        .then((d) => { this.r = d; this.sdPruneSel(); })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.loading = false; });
      // 蜡烛单独取一次：OHLC 需要**日内多小时采样**，与 EVT 所需的「日用量的
      // 分布」聚合粒度不同，不能从同一个查询里兼得（见 admin.handleShareCandles）。
      // 失败不当作整体失败：趋势图空着，其它三个图照常可用，且提示可分辨。
      req("GET", "/api/share-detection/candles?" + new URLSearchParams({
        from: String(from), to: String(to),
      }).toString())
        .then((d) => { this.cd = d; })
        .catch((e) => { this.cd = null; toast("蜡烛数据加载失败：" + e.message, false); });
    },
    openParams() { this.form = Object.assign({}, this.form); this.pOpen = true; },
    saveParams() {
      this.snap = JSON.stringify(this.form);
      sdSavePrefs({ form: this.form, minReqPerDay: this.minReqPerDay, sel: this.sel });
      this.pOpen = false;
      this.load();
    },
    resetParams() {
      this.form = Object.assign({}, SD_DEFAULTS);
      this.minReqPerDay = SD_DEFAULT_MINREQ;
    },
    onMinReqChange() {
      sdSavePrefs({ form: this.form, minReqPerDay: this.minReqPerDay, sel: this.sel });
      this.load();
    },
    presetRange(days) {
      this.from = toDateInput(new Date(Date.now() - (days - 1) * 86400000));
      this.to = toDateInput(new Date());
      this.load();
    },
  },
  template: `
  <div class="page">
    <div class="page-title">分发检测</div>

    <div class="toolbar">
      <input type="date" v-model="from" style="width:150px"/>
      <span class="sh-note">至</span>
      <input type="date" v-model="to" style="width:150px"/>
      <button class="btn btn-outline btn-sm" @click="presetRange(7)">7 天</button>
      <button class="btn btn-outline btn-sm" @click="presetRange(30)">30 天</button>
      <button class="btn btn-outline btn-sm" @click="presetRange(90)">90 天</button>
      <button class="btn btn-primary" :disabled="loading" @click="load">{{ loading ? '分析中…' : '重新分析' }}</button>
      <div class="spacer"></div>
      <button class="btn btn-outline" @click="openParams">⚙ 模型参数</button>
    </div>

    <div class="stat-row" v-if="res">
      <div class="stat-card"><div class="ic ic-red">★</div><div class="body">
        <div class="v">{{ counts.flagged }}</div><div class="l">显著异常</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">◐</div><div class="body">
        <div class="v">{{ counts.observe }}</div><div class="l">观察</div></div></div>
      <div class="stat-card"><div class="ic ic-blue">Σ</div><div class="body">
        <div class="v">{{ res.evaluated }}</div><div class="l">参与分析<span class="stat-sub">样本充足的 {{ res.reliable }} 个</span></div></div></div>
      <div class="stat-card" title="正常组 CV 分布的对数正态下界"><div class="ic ic-purple">σ</div><div class="body">
        <div class="v">{{ cvf.valid ? cvf.crit_cv.toFixed(2) : '—' }}</div>
        <div class="l">显著平稳线 CV<span class="stat-sub">{{ cvf.valid ? '中位 CV ' + Math.exp(cvf.mu).toFixed(2) : '未拟合' }}</span></div></div></div>
      <div class="stat-card"><div class="ic ic-green">n</div><div class="body">
        <div class="v">{{ fmtInt(res.total_days) }}</div><div class="l">正常样本·日量</div></div></div>
      <div class="stat-card" v-if="r && r.groups"><div class="ic ic-gray">⚠</div><div class="body">
        <div class="v">{{ counts.insufficient }}</div><div class="l">样本不足</div></div></div>
    </div>

    <div class="card">
      <div class="tabs">
        <button v-for="t in tabs" :key="t.v" class="tab-btn" :class="{active: tab === t.v}" :title="t.tip" @click="tab = t.v">{{ t.label }}</button>
      </div>

      <!-- 子 Key 参与开关：只管「日用量趋势」。散点/量级对比/拟合图的判据是
           全局口径（阈值由全体样本估出），收窄选中集合会让「显著平稳线」
           看起来变了——那是两套语义，因此那三张图不接这个开关。 -->
      <div class="sd-pick" v-if="tab === 'timeline' && candleGroups.length">
        <div class="sd-pick-bar">
          <button class="btn btn-outline btn-sm" @click="sdSelectAll" :disabled="selAll">全部</button>
          <button class="btn btn-outline btn-sm" @click="sdSelectNone" :disabled="!selCount && !selAll">清空</button>
          <button class="btn btn-outline btn-sm" @click="sdSelectFlagged">仅命中</button>
          <button class="btn btn-outline btn-sm" @click="sdSelectTop(5)">Top 5</button>
          <span class="sd-pick-count">
            已选 <b>{{ selCount }}</b> / {{ candleGroups.length }} 个子 Key
            <span v-if="!selAll" class="sd-pick-warn">（仅影响本图，其余三张图仍为全局口径）</span>
          </span>
          <div class="spacer"></div>
          <input class="sd-pick-search" v-model="selQuery" placeholder="搜索子 Key" v-if="selOpen"/>
          <button class="btn btn-outline btn-sm" @click="selOpen = !selOpen">{{ selOpen ? '收起 ▴' : '选择 ▾' }}</button>
        </div>
        <div class="sd-pick-list" v-if="selOpen">
          <button v-for="s in selList" :key="s.key" class="sd-pick-item"
                  :class="{on: selAll || selSet[s.key]}" @click="sdToggle(s.key)"
                  :title="s.label + ' · ' + (flagMeta({flag:s.flag}).label) + ' · 返回期 ' + (s.rp >= 0 ? fmtInt(s.rp) + ' 天' : '外推上界（等同于无限）') + ' · 中位 ' + fmtInt(Math.round(s.median)) + '/天'">
            <span class="sd-pick-dot" :style="{background: selColor(s.key)}"></span>
            <span class="sd-pick-name">{{ s.label }}</span>
            <span class="sd-pick-rp" v-if="s.flag === 'flagged' || s.flag === 'excluded'">★</span>
          </button>
          <div class="empty" v-if="!selList.length">没有匹配的子 Key</div>
        </div>
      </div>

      <div class="card-head" v-if="tab === 'timeline'">
        <div class="card-title">{{ timelineTitle }}</div>
        <span class="src-badge ic-gray" title="蜡烛实体 = 当日首/末个小时桶的请求数（开/收）；影线 = 当日最高/最低小时桶（高/低）。日内多小时采样来自 usage_logs 的逐次请求。缺日留空槽，不补零">日内 OHLC</span>
      </div>
      <div class="chart-wrap" v-html="chartHtml"></div>
    </div>

    <div class="detail-grid" v-if="res">
      <div class="card">
        <div class="card-head"><div class="card-title">拟合参数</div></div>
        <table class="mini-table">
          <tr><td>阈值分位</td><td>P{{ Math.round((params.q_threshold || 0.75) * 100) }}</td><td class="muted">u</td></tr>
          <tr><td>返回期判据</td><td>{{ fmtInt(params.return_period_days) }} 天</td><td class="muted">量级线</td></tr>
          <tr><td>波动判据</td><td>p &lt; {{ params.cv_alpha }}</td><td class="muted">单侧</td></tr>
          <tr><td>样本门槛</td><td>{{ params.min_days }} 天</td><td class="muted">参与统计</td></tr>
        </table>
        <div class="hint">{{ fitSummary }}</div>
        <div class="hint" v-if="upperBoundText">{{ upperBoundText }}</div>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">判定分布</div></div>
        <table class="mini-table">
          <tr v-for="k in ['flagged','excluded','observe','normal','insufficient']" :key="k">
            <td><span class="tag" :class="flagMeta({flag:k}).cls">{{ flagMeta({flag:k}).label }}</span></td>
            <td>{{ counts[k] || 0 }}</td>
            <td class="muted" style="text-align:left;width:auto"></td>
          </tr>
        </table>
      </div>
    </div>

    <div class="card" v-if="res">
      <div class="card-head"><div class="card-title">明细（共 {{ groups.length }} 个子 Key，按返回期降序）</div></div>
      <div class="table-wrap"><table><thead><tr>
        <th>子 Key</th><th>判定</th><th>天数</th>
        <th title="该子 Key 有使用的日子的请求数中位数">中位/天</th>
        <th>P90/天</th>
        <th>最大/天</th>
        <th title="日用量变异系数 = 标准差/均值">波动 CV</th>
        <th title="该 CV 在正常组 CV 分布中的分位">CV p 值</th>
        <th title="最大日量对应的返回期">返回期(天)<span class="th-sub">量级判据</span></th>
        <th>量级 p 值</th>
        <th>样本</th>
      </tr></thead><tbody>
        <tr v-if="!groups.length"><td colspan="11" class="empty">区间内无日志，或各组天数均低于门槛（≥ {{ params.min_days }} 天，正常样本池需 ≥ 8 个日量）</td></tr>
        <tr v-for="g in groups" :key="g.key" :class="{ 'sd-row-flagged': g.flag === 'flagged' }">
          <td>{{ g.label || g.key }}</td>
          <td><span class="tag" :class="flagMeta(g).cls" :title="flagMeta(g).tip">{{ flagMeta(g).label }}</span></td>
          <td>{{ g.days }}</td>
          <td>{{ fmtInt(Math.round(g.level_median)) }}</td>
          <td>{{ fmtInt(Math.round(g.level_p90)) }}</td>
          <td>{{ fmtInt(Math.round(g.level_max)) }}</td>
          <td :class="{ 'sd-hot': cvf.valid && g.cv_pct > 0 && g.cv_pct < params.cv_alpha }">{{ (g.cv || 0).toFixed(2) }}</td>
          <td>{{ g.cv_pct ? g.cv_pct.toFixed(3) : '—' }}</td>
          <td :class="{ 'sd-hot': g.return_period_days > params.return_period_days }">
            <span v-html="fmtReturnPeriod(g)"></span>{{ g.infinite ? ' 以上' : '' }}
          </td>
          <td>{{ g.level_p_value != null ? g.level_p_value.toExponential(2) : '—' }}</td>
          <td>
            <span v-if="g.reliable" class="tag tag-green">充足</span>
            <span v-else class="tag tag-gray" :title="(g.notes || []).join('；')">不足</span>
          </td>
        </tr>
      </tbody></table></div>
    </div>

    <ui-drawer :open="pOpen" title="模型参数" :width="620" @close="pOpen=false">
      <div class="sec">
        <div class="sec-title">判据阈值</div>
        <div class="form-row">
          <div class="form-item">
            <label>返回期判据（天）</label>
            <input v-model.number="form.return_period_days" type="number" min="1" step="100"/>
          </div>
          <div class="form-item">
            <label>波动显著性 α</label>
            <input v-model.number="form.cv_alpha" type="number" min="0.001" max="0.5" step="0.01"/>
          </div>
        </div>
        <p class="hint">α 是单侧 p 值上限，越小越严格。</p>
      </div>

      <div class="sec">
        <div class="sec-title">分布估计</div>
        <div class="form-row">
          <div class="form-item">
            <label>GPD 阈值分位</label>
            <input v-model.number="form.q_threshold" type="number" min="0.5" max="0.95" step="0.05"/>
          </div>
          <div class="form-item">
            <label>粗筛倍数 k（中位数 + k×MAD）</label>
            <input v-model.number="form.exclude_k" type="number" min="0.5" max="20" step="0.5"/>
          </div>
        </div>
        <div class="form-row">
          <div class="form-item">
            <label>迭代剔除轮数上限</label>
            <input v-model.number="form.max_rounds" type="number" min="1" max="12"/>
          </div>
        </div>
        <p class="hint">阈值分位越高超额点越少，少于 5 个无法拟合。</p>
      </div>

      <div class="sec">
        <div class="sec-title">样本门槛</div>
        <div class="form-row">
          <div class="form-item">
            <label>参与统计的最少天数</label>
            <input v-model.number="form.min_days" type="number" min="3" max="200"/>
          </div>
          <div class="form-item">
            <label>「样本充足」门槛（天）</label>
            <input v-model.number="form.reliable_days" type="number" min="3" max="400"/>
          </div>
          <div class="form-item">
            <label>使用日最少请求数</label>
            <input v-model.number="minReqPerDay" type="number" min="1" @change="onMinReqChange"/>
          </div>
        </div>
        <p class="hint">「使用日最少请求数」滤掉零星请求的日子（一两条不代表当日使用强度）。</p>
      </div>

      <template #foot>
        <button class="btn btn-outline" @click="resetParams">恢复默认</button>
        <div class="spacer"></div>
        <button class="btn btn-outline" @click="pOpen=false">取消</button>
        <button class="btn btn-primary" :disabled="!dirty" @click="saveParams">{{ dirty ? '保存并重新分析' : '无改动' }}</button>
      </template>
    </ui-drawer>
  </div>`,
};

// 参数与展示偏好只存本浏览器（localStorage）：这些是**分析视角**而非网关行为，
// 不同管理员可以有不同视角，不该互相覆盖，也不该占用 settings 表。
function sdLoadPrefs() {
  try {
    const raw = JSON.parse(localStorage.getItem("arkgate_sd_prefs") || "{}");
    const form = Object.assign({}, SD_DEFAULTS, raw.form || {});
    const minReqPerDay = Number(raw.minReqPerDay) > 0 ? Number(raw.minReqPerDay) : SD_DEFAULT_MINREQ;
    // sel 缺省 = null（全选）。数组才认（空数组 = 明确清空，与未设置不同）。
    const sel = Array.isArray(raw.sel) ? raw.sel.map(String) : null;
    return { form, minReqPerDay, sel };
  } catch (e) {
    return { form: Object.assign({}, SD_DEFAULTS), minReqPerDay: SD_DEFAULT_MINREQ, sel: null };
  }
}

function sdSavePrefs(v) {
  try {
    localStorage.setItem("arkgate_sd_prefs", JSON.stringify({
      form: v.form, minReqPerDay: v.minReqPerDay,
      sel: Array.isArray(v.sel) ? v.sel : null,
    }));
  } catch (e) { /* 隐私模式下写不进去，忽略 */ }
}
