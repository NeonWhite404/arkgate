/* ArkGate 前端 —— Vue 3（vendor 全局构建，免 Node 构建；Arco 风格沿用 style.css）
 *
 * 结构：LoginPage（管理端 / 子 Key 门户双模式登录）
 *      ├─ AdminShell（侧边栏 + 管理页：总览/用量分析/账号/模型映射/子Key/日志/设置 + 底部折叠使用说明）
 *      └─ PortalPage（子 Key 自助门户：限额进度、用量、成功率、脱敏调用记录）
 */
"use strict";

const { createApp, reactive } = Vue;

// ── 全局登录态 ──
const state = {
  adminToken: localStorage.getItem("arkgate_token") || "",
  subKey: localStorage.getItem("arkgate_sk") || "",
};

// ── 工具 ──
function fmtTokens(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(n);
}
function fmtTime(unix) {
  if (!unix) return "-";
  const d = new Date(unix * 1000);
  const p = (n) => (n < 10 ? "0" : "") + n;
  return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
}
function fmtCost(c) {
  c = Number(c || 0);
  if (c === 0) return "$0";
  if (c >= 1) return "$" + c.toFixed(2);
  return "$" + c.toFixed(4);
}
function fmtInt(v) {
  v = Number(v || 0);
  return v >= 100 ? fmtTokens(v) : String(Math.round(v));
}
function dayLabel(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => (n < 10 ? "0" : "") + n;
  return p(d.getMonth() + 1) + "-" + p(d.getDate());
}
function toDateInput(d) {
  const p = (n) => (n < 10 ? "0" : "") + n;
  return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate());
}
function fmtPct(x) {
  return Number(x || 0).toFixed(1) + "%";
}

// ── Toast ──
const toasts = reactive([]);
let toastSeq = 0;
function toast(msg, ok = true) {
  const id = ++toastSeq;
  toasts.push({ id, msg, ok });
  setTimeout(() => {
    const i = toasts.findIndex((t) => t.id === id);
    if (i >= 0) toasts.splice(i, 1);
  }, 2600);
}

// ── 本地偏好（仅存本浏览器 localStorage，不影响网关配置） ──
const DEFAULT_BG_URL = "https://img.paulzzh.com/touhou/random";
const prefs = reactive({
  // 背景图：随机图床或固定图片地址均可；空串 = 关闭背景。未设置过时用默认图床。
  bgUrl: localStorage.getItem("arkgate_bg") !== null ? localStorage.getItem("arkgate_bg") : DEFAULT_BG_URL,
  bgBlur: Number(localStorage.getItem("arkgate_bg_blur") ?? 40), // 模糊百分比 0-100
  theme: localStorage.getItem("arkgate_theme") || "light",
  overviewAuto: Number(localStorage.getItem("arkgate_overview_auto") ?? 0), // 总览自动刷新秒数，0=关闭
  helpOpen: localStorage.getItem("arkgate_help_open") !== "0", // 侧栏使用说明默认展开
});
// savePrefs 落盘并即时应用（模糊换算：40% ≈ 12px，纯视觉参数）。
function savePrefs() {
  localStorage.setItem("arkgate_bg", prefs.bgUrl);
  localStorage.setItem("arkgate_bg_blur", String(prefs.bgBlur));
  localStorage.setItem("arkgate_theme", prefs.theme);
  localStorage.setItem("arkgate_overview_auto", String(prefs.overviewAuto));
  localStorage.setItem("arkgate_help_open", prefs.helpOpen ? "1" : "0");
  applyPrefs();
}
function applyPrefs() {
  document.documentElement.setAttribute("data-theme", prefs.theme === "dark" ? "dark" : "");
  document.documentElement.classList.toggle("has-bg", !!prefs.bgUrl);
}

// ── API 封装 ──
// opts.key 显式指定鉴权 Key（门户调用传子 Key）；缺省用管理令牌。
function req(method, path, body, opts = {}) {
  const key = opts.key !== undefined ? opts.key : state.adminToken;
  return fetch(path, {
    method,
    headers: { "Content-Type": "application/json", ...(key ? { Authorization: "Bearer " + key } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  }).then((r) =>
    r.json().catch(() => ({})).then((d) => {
      if (!r.ok) {
        const m = d.detail || (d.error && d.error.message) || r.status + " error";
        const e = new Error(typeof m === "string" ? m : JSON.stringify(m));
        e.status = r.status;
        if (r.status === 401 && window.__arkgateOn401) window.__arkgateOn401(path);
        throw e;
      }
      if (d === null || d === undefined) return [];
      return d;
    })
  );
}

// ── 复选框组（子 Key 白名单） ──
// options 项为字符串（值=标签）或 {v, l}（值与显示分开，如账号 id → 名称）。
const CheckGroup = {
  props: { options: Array, modelValue: Array },
  emits: ["update:modelValue"],
  computed: {
    items() {
      return (this.options || []).map((o) => (typeof o === "string" ? { v: o, l: o } : o));
    },
    checked() { return new Set(this.modelValue || []); },
  },
  methods: {
    toggle(v, on) {
      const s = new Set(this.modelValue || []);
      if (on) s.add(v); else s.delete(v);
      this.$emit("update:modelValue", Array.from(s));
    },
  },
  template: `
    <div class="cb-group">
      <label v-for="o in items" :key="o.v"><input type="checkbox" :checked="checked.has(o.v)" @change="toggle(o.v, $event.target.checked)"/>{{ o.l }}</label>
      <span v-if="!items.length" class="tag tag-gray">暂无可选项</span>
    </div>`,
};

// ── 能力三态选择 ──
const capOptions = [
  { v: 0, label: "继承供应商默认" },
  { v: 1, label: "强制可用" },
  { v: -1, label: "强制禁用" },
];

// ── 登录页（双模式） ──
const LoginPage = {
  emits: ["admin", "portal"],
  data() {
    return { tab: "admin", token: "", key: "", initialized: true, busy: false };
  },
  mounted() {
    req("GET", "/api/auth/status", null, { key: "" })
      .then((d) => { this.initialized = !!d.initialized; })
      .catch(() => {});
  },
  methods: {
    submitAdmin() {
      const tok = this.token.trim();
      if (!tok) { toast("请输入令牌", false); return; }
      this.busy = true;
      const p = this.initialized
        ? req("POST", "/api/auth/login", { token: tok }, { key: "" })
        : req("POST", "/api/auth/setup", { token: tok }, { key: "" });
      p.then(() => { this.$emit("admin", tok); })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.busy = false; });
    },
    submitPortal() {
      const k = this.key.trim();
      if (!k) { toast("请输入子 Key", false); return; }
      this.busy = true;
      // 用输入的子 Key 请求门户概览，成功即视为登录成功。
      req("POST", "/api/portal/overview", null, { key: k })
        .then(() => { this.$emit("portal", k); })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.busy = false; });
    },
  },
  template: `
  <div class="login-wrap"><div class="login-card">
    <div class="logo"><span class="dot" style="width:22px;height:22px;border-radius:7px;background:linear-gradient(135deg,rgb(var(--primary-6)),#4080ff)"></span>ArkGate</div>
    <div class="sub">多供应商多账号负载均衡 · OpenAI 兼容网关</div>
    <div class="login-tabs">
      <div class="lt" :class="{active: tab==='admin'}" @click="tab='admin'">管理端</div>
      <div class="lt" :class="{active: tab==='portal'}" @click="tab='portal'">子 Key 用户</div>
    </div>
    <template v-if="tab==='admin'">
      <div class="form-item"><label>访问令牌</label>
        <input v-model="token" :placeholder="initialized ? '访问令牌' : '首次使用，请设置访问令牌（至少 6 位）'" @keyup.enter="submitAdmin"/></div>
      <button class="btn btn-primary" style="width:100%" :disabled="busy" @click="submitAdmin">{{ initialized ? '登录' : '初始化' }}</button>
    </template>
    <template v-else>
      <div class="form-item"><label>子 API Key</label>
        <input v-model="key" placeholder="sk-xxx" @keyup.enter="submitPortal"/></div>
      <button class="btn btn-primary" style="width:100%" :disabled="busy" @click="submitPortal">查询我的用量</button>
      <div class="form-item" style="margin-top:12px;font-size:12px;color:var(--color-text-3)">
        仅可查看该 Key 自身的用量与调用记录。</div>
    </template>
  </div></div>`,
};

// ── 通用表格空态/标签 ──
function statusTag(s) {
  return s === "active" ? '<span class="tag tag-green">启用</span>' : '<span class="tag tag-gray">禁用</span>';
}

// ── 总览（以统计图表为主） ──
const OverviewPage = {
  data() { return { o: null, series: [], prefs }; },
  computed: {
    // 按小时聚合：tokens / cost / requests 三条趋势共用一套时间桶。
    hourly() {
      const byTs = {};
      for (const p of this.series) {
        if (!byTs[p.ts]) byTs[p.ts] = { t: p.ts, tokens: 0, cost: 0, requests: 0 };
        byTs[p.ts].tokens += p.tokens || 0;
        byTs[p.ts].cost += p.cost || 0;
        byTs[p.ts].requests += p.requests || 0;
      }
      return Object.values(byTs).sort((a, b) => a.t - b.t);
    },
    // 模型 / 子 Key 分布（tokens 降序，颜色与趋势图中的模型色一致）。
    modelColors() {
      const models = [...new Set(this.series.map((p) => p.model || "—"))].sort();
      const colors = ["#3b82f6", "#10b981", "#f59e0b", "#ef4444", "#8b5cf6", "#14b8a6"];
      const map = {};
      models.forEach((m, i) => { map[m] = colors[i % colors.length]; });
      return map;
    },
    modelDist() {
      const byModel = {};
      for (const p of this.series) {
        const m = p.model || "—";
        byModel[m] = (byModel[m] || 0) + (p.tokens || 0);
      }
      return Object.entries(byModel)
        .map(([label, value]) => ({ label, value, color: this.modelColors[label] }))
        .sort((a, b) => b.value - a.value);
    },
    subkeyDist() {
      const bySk = {};
      for (const p of this.series) {
        const k = p.subkey || p.subkey_id || "—";
        bySk[k] = (bySk[k] || 0) + (p.tokens || 0);
      }
      return Object.entries(bySk)
        .map(([label, value]) => ({ label, value, color: "rgb(var(--primary-6))" }))
        .sort((a, b) => b.value - a.value);
    },
    tokenChartHtml() { return renderStackedTokenChart(this.series); },
    costChartHtml() { return renderBarChart(this.hourly, (p) => p.cost, "#f59e0b", fmtCost); },
    reqChartHtml() { return renderBarChart(this.hourly, (p) => p.requests, "#3b82f6", fmtInt); },
  },
  mounted() {
    this.load();
    this.setupAuto();
  },
  beforeUnmount() {
    if (this._timer) clearInterval(this._timer);
  },
  template: `
  <div class="page">
    <div class="page-head">
      <div class="page-title" style="margin:0">总览 <span class="tag tag-green" style="margin-left:8px"><span class="pulse" style="display:inline-block;width:6px;height:6px;border-radius:50%;background:rgb(var(--green-6));margin-right:4px"></span>运行中</span></div>
      <div class="row-actions">
        <button class="btn btn-outline btn-sm" @click="load">↻ 刷新</button>
        <button class="btn btn-outline btn-sm" @click="toggleDark">🌓</button>
      </div>
    </div>
    <div class="stat-row" v-if="o">
      <div class="stat-card"><div class="ic ic-blue">🏛</div><div class="body"><div class="v">{{ o.account_active }}<span style="font-size:13px;color:var(--color-text-3)">/{{ o.account_total }}</span></div><div class="l">启用账号</div></div></div>
      <div class="stat-card"><div class="ic ic-red">⏳</div><div class="body"><div class="v">{{ o.endpoint_circuit }}</div><div class="l">元组熔断</div></div></div>
      <div class="stat-card"><div class="ic ic-purple">🧩</div><div class="body"><div class="v">{{ o.model_count }}</div><div class="l">模型</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">🔑</div><div class="body"><div class="v">{{ o.subkey_count }}</div><div class="l">子 Key</div></div></div>
      <div class="stat-card"><div class="ic ic-blue">⚡</div><div class="body"><div class="v">{{ o.total_requests }}</div><div class="l">总请求</div></div></div>
      <div class="stat-card"><div class="ic ic-green">⬤</div><div class="body"><div class="v">{{ fmtTokens(o.total_tokens) }}</div><div class="l">总 Token</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">💰</div><div class="body"><div class="v">{{ fmtCost(o.total_cost) }}</div><div class="l">总成本（24h {{ fmtCost(o.cost_24h) }}）</div></div></div>
    </div>

    <div class="chart-grid" v-if="o">
      <div class="card wide"><div class="card-head"><div class="card-title">Token 用量趋势（24h，按子 Key × 模型堆叠）</div></div>
        <div class="chart-wrap" v-html="tokenChartHtml"></div></div>
      <div class="card"><div class="card-head"><div class="card-title">成本趋势（24h）</div></div>
        <div class="chart-wrap" v-html="costChartHtml"></div></div>
      <div class="card"><div class="card-head"><div class="card-title">请求趋势（24h）</div></div>
        <div class="chart-wrap" v-html="reqChartHtml"></div></div>
      <div class="card"><div class="card-head"><div class="card-title">模型分布（24h Tokens）</div></div>
        <div class="hbars"><div class="hbar-row" v-for="it in modelDist" :key="it.label">
          <span class="hbar-label" :title="it.label">{{ it.label }}</span>
          <div class="hbar-track"><div class="hbar-fill" :style="{width: hbarPct(it.value, modelDist) + '%', background: it.color}"></div></div>
          <span class="hbar-val">{{ fmtTokens(it.value) }}</span>
        </div><div v-if="!modelDist.length" class="empty">暂无数据</div></div></div>
      <div class="card"><div class="card-head"><div class="card-title">子 Key 分布（24h Tokens）</div></div>
        <div class="hbars"><div class="hbar-row" v-for="it in subkeyDist" :key="it.label">
          <span class="hbar-label" :title="it.label">{{ it.label }}</span>
          <div class="hbar-track"><div class="hbar-fill" :style="{width: hbarPct(it.value, subkeyDist) + '%'}"></div></div>
          <span class="hbar-val">{{ fmtTokens(it.value) }}</span>
        </div><div v-if="!subkeyDist.length" class="empty">暂无数据</div></div></div>
    </div>
  </div>`,
  methods: {
    hbarPct(v, list) {
      const max = Math.max(...list.map((x) => x.value), 1);
      return Math.max(2, (v / max) * 100);
    },
    load() {
      req("GET", "/api/overview").then((d) => { this.o = d; }).catch((e) => toast(e.message, false));
      req("GET", "/api/usage/series?hours=24").then((s) => { this.series = s || []; }).catch(() => {});
    },
    // setupAuto 按设置页的自动刷新间隔起停定时器（进入本页时生效）。
    setupAuto() {
      if (this._timer) clearInterval(this._timer);
      this._timer = null;
      if (this.prefs.overviewAuto > 0) {
        this._timer = setInterval(() => this.load(), this.prefs.overviewAuto * 1000);
      }
    },
  },
};

// 深/浅色切换（挂到 globalProperties 供模板调用；状态并入 prefs 设置页可见）。
function toggleDark() {
  prefs.theme = prefs.theme === "dark" ? "light" : "dark";
  savePrefs();
}

// Token 用量趋势：子 Key × 模型按小时堆叠柱状图（SVG 字符串）。
function renderStackedTokenChart(series) {
  if (!series || !series.length) return '<div class="empty">暂无用量数据</div>';
  const byTs = {}; const subs = {}; const models = {};
  series.forEach((p) => {
    const t = p.ts;
    if (!byTs[t]) byTs[t] = {};
    const key = p.subkey || p.subkey_id;
    const m = p.model || "—";
    if (!byTs[t][key]) byTs[t][key] = {};
    if (!byTs[t][key][m]) byTs[t][key][m] = 0;
    byTs[t][key][m] += p.tokens || 0;
    subs[key] = true; models[m] = true;
  });
  const tsList = Object.keys(byTs).map((x) => parseInt(x, 10)).sort((a, b) => a - b);
  const subList = Object.keys(subs).sort();
  const modelList = Object.keys(models).sort();
  const colors = ["#3b82f6", "#10b981", "#f59e0b", "#ef4444", "#8b5cf6", "#14b8a6"];
  const mColor = {};
  modelList.forEach((m, i) => { mColor[m] = colors[i % colors.length]; });

  const W = 820, H = 240, padL = 48, padR = 12, padT = 12, padB = 28;
  let maxV = 0;
  tsList.forEach((t) => {
    let stackTotal = 0;
    subList.forEach((sk) => modelList.forEach((m) => { stackTotal += (byTs[t] && byTs[t][sk] && byTs[t][sk][m]) || 0; }));
    if (stackTotal > maxV) maxV = stackTotal;
  });
  if (maxV <= 0) maxV = 1;
  const xOf = (i) => padL + (tsList.length > 1 ? (i * (W - padL - padR) / (tsList.length - 1)) : (W - padL - padR) / 2);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H + '">';
  for (let g = 0; g <= 4; g++) {
    const y = padT + (H - padT - padB) * (g / 4);
    svg += '<line x1="' + padL + '" y1="' + y + '" x2="' + (W - padR) + '" y2="' + y + '" stroke="#e5e7eb" />';
  }
  const barW = Math.max(2, Math.min(Math.floor((W - padL - padR) / Math.max(1, tsList.length)) - 2, Math.floor((W - padL - padR) / 3)));
  const plotH = H - padT - padB;
  tsList.forEach((t, i) => {
    const x = xOf(i) - barW / 2;
    let yBase = H - padB;
    let acc = 0;
    subList.forEach((sk) => modelList.forEach((m) => {
      const v = (byTs[t] && byTs[t][sk] && byTs[t][sk][m]) || 0;
      if (v <= 0) return;
      acc += v;
      const y = (H - padB) - Math.round(plotH * (acc / maxV));
      const barH = Math.max(1, yBase - y);
      svg += '<rect x="' + x + '" y="' + y + '" width="' + barW + '" height="' + barH + '" fill="' + mColor[m] + '" opacity="0.9"><title>' + sk + ' · ' + m + ': ' + v + '</title></rect>';
      yBase = y;
    }));
  });
  const step = Math.max(1, Math.floor(tsList.length / 6));
  for (let k = 0; k < tsList.length; k += step) {
    const d = new Date(tsList[k] * 1000);
    const hh = (d.getHours() < 10 ? "0" : "") + d.getHours();
    svg += '<text x="' + xOf(k) + '" y="' + (H - 6) + '" font-size="10" fill="#6b7280" text-anchor="middle">' + hh + '</text>';
  }
  svg += "</svg>";
  const leg = '<div class="chart-legend">' + modelList.map((m) =>
    '<span class="leg-item"><span class="leg-swatch" style="background:' + mColor[m] + '"></span>' + m + "</span>").join("") + "</div>";
  return svg + leg;
}

// 通用单系列柱状图：points [{t, ...}]，取值与格式化由调用方注入（成本/请求趋势共用）。
// labelFn 缺省用小时标签；天粒度图表传 dayLabel。big=true 用量分析主图规格
// （820×240、5 格网格），与堆叠图/折线图同框；缺省总览小图（400×190）。
function renderBarChart(points, pick, color, format, labelFn, big) {
  labelFn = labelFn || hourLabel;
  if (!points || !points.length) return '<div class="empty">暂无数据</div>';
  // 时间字段兼容两种形态：总览 hourly 用 .t，用量分析桶用 .bucket。
  const tf = (p) => (p.t !== undefined ? p.t : p.bucket);
  const W = big ? 820 : 400, H = big ? 240 : 190;
  const padL = big ? 56 : 44, padR = big ? 12 : 8, padT = big ? 12 : 10, padB = big ? 28 : 24;
  const grids = big ? 4 : 3; // 网格分段数
  const vals = points.map(pick);
  const maxV = Math.max(...vals, 1);
  const plotH = H - padT - padB;
  const n = points.length;
  // 单桶时柱子收窄到绘图区 1/3，避免撑成整块色条。
  const barW = Math.max(2, Math.min(Math.floor((W - padL - padR) / n) - 2, Math.floor((W - padL - padR) / 3)));
  const xOf = (i) => padL + (n > 1 ? (i * (W - padL - padR) / (n - 1)) : (W - padL - padR) / 2);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let g = 0; g <= grids; g++) {
    const y = padT + plotH * (g / grids);
    const v = maxV * (1 - g / grids);
    svg += '<line x1="' + padL + '" y1="' + y + '" x2="' + (W - padR) + '" y2="' + y + '" stroke="#e5e7eb" />';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 4) + '" font-size="9" fill="#86909c" text-anchor="end">' + format(v) + '</text>';
  }
  points.forEach((p, i) => {
    const v = pick(p);
    const h = Math.max(v > 0 ? 2 : 0, plotH * (v / maxV));
    const x = xOf(i) - barW / 2;
    const y = H - padB - h;
    svg += '<rect x="' + x + '" y="' + y + '" width="' + barW + '" height="' + h + '" rx="2" fill="' + color + '" opacity="0.9"><title>' + labelFn(tf(p)) + ' · ' + format(v) + '</title></rect>';
  });
  const step = Math.max(1, Math.floor(n / (big ? 8 : 6)));
  for (let k = 0; k < n; k += step) {
    svg += '<text x="' + xOf(k) + '" y="' + (H - 6) + '" font-size="10" fill="#6b7280" text-anchor="middle">' + labelFn(tf(points[k])) + '</text>';
  }
  svg += "</svg>";
  return svg;
}

// 成功率折线图（用量分析主图规格）：pick(p) 返回 {pct, ok, n} 或 null（该桶无请求，
// 折线断开）。Y 轴固定 0-100%，与柱状图同一绘图框，切指标时布局不跳变。
function renderLineChart(points, pick, labelFn) {
  if (!points || !points.length) return '<div class="empty">所选区间暂无数据</div>';
  const W = 820, H = 240, padL = 56, padR = 12, padT = 12, padB = 28;
  const plotH = H - padT - padB;
  const n = points.length;
  const xOf = (i) => padL + (n > 1 ? (i * (W - padL - padR)) / (n - 1) : (W - padL - padR) / 2);
  const yOf = (pct) => padT + plotH * (1 - Math.min(100, Math.max(0, pct)) / 100);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let g = 0; g <= 4; g++) {
    const y = padT + plotH * (g / 4);
    svg += '<line x1="' + padL + '" y1="' + y + '" x2="' + (W - padR) + '" y2="' + y + '" stroke="#e5e7eb" />';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 4) + '" font-size="9" fill="#86909c" text-anchor="end">' + Math.round(100 - g * 25) + '%</text>';
  }
  // 分段折线：有效点连 L，遇 null 重起 M（空桶断线，不伪造 0%）。
  let d = "", hasPoint = false, penDown = false;
  points.forEach((p, i) => {
    const v = pick(p);
    if (!v) { penDown = false; return; }
    hasPoint = true;
    const cmd = penDown ? "L" : "M";
    d += cmd + xOf(i).toFixed(1) + " " + yOf(v.pct).toFixed(1) + " ";
    penDown = true;
  });
  svg += '<path d="' + d + '" fill="none" stroke="#10b981" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>';
  points.forEach((p, i) => {
    const v = pick(p);
    if (!v) return;
    svg += '<circle cx="' + xOf(i).toFixed(1) + '" cy="' + yOf(v.pct).toFixed(1) + '" r="2.6" fill="#10b981"><title>' +
      labelFn(p.bucket) + " · 成功率 " + v.pct.toFixed(1) + "%（" + v.ok + "/" + v.n + " 次）</title></circle>";
  });
  if (!hasPoint) return '<div class="empty">所选区间暂无数据</div>';
  const step = Math.max(1, Math.floor(n / 8));
  for (let k = 0; k < n; k += step) {
    svg += '<text x="' + xOf(k) + '" y="' + (H - 6) + '" font-size="10" fill="#6b7280" text-anchor="middle">' + labelFn(points[k].bucket) + '</text>';
  }
  svg += "</svg>";
  return svg;
}

// 双系列堆叠柱状图（用量分析：输入/输出 tokens 按时间桶堆叠）。
function renderStackedBarChart(points, pickA, pickB, colorA, colorB, nameA, nameB, fmt, labelFn) {
  if (!points || !points.length) return '<div class="empty">暂无数据</div>';
  const W = 820, H = 240, padL = 56, padR = 12, padT = 12, padB = 28;
  const plotH = H - padT - padB;
  const n = points.length;
  const maxV = Math.max(...points.map((p) => pickA(p) + pickB(p)), 1);
  const barW = Math.max(2, Math.min(Math.floor((W - padL - padR) / n) - 2, Math.floor((W - padL - padR) / 3)));
  const xOf = (i) => padL + (n > 1 ? (i * (W - padL - padR)) / (n - 1) : (W - padL - padR) / 2);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let g = 0; g <= 4; g++) {
    const y = padT + plotH * (g / 4);
    svg += '<line x1="' + padL + '" y1="' + y + '" x2="' + (W - padR) + '" y2="' + y + '" stroke="#e5e7eb" />';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 4) + '" font-size="9" fill="#86909c" text-anchor="end">' + fmt(maxV * (1 - g / 4)) + '</text>';
  }
  points.forEach((p, i) => {
    const a = pickA(p), b = pickB(p);
    const ha = plotH * (a / maxV);
    const hb = plotH * (b / maxV);
    const x = xOf(i) - barW / 2;
    if (a > 0) {
      svg += '<rect x="' + x + '" y="' + (H - padB - ha) + '" width="' + barW + '" height="' + Math.max(1, ha) + '" rx="2" fill="' + colorA + '" opacity="0.9"><title>' + labelFn(p.bucket) + ' · ' + nameA + ': ' + fmt(a) + '</title></rect>';
    }
    if (b > 0) {
      svg += '<rect x="' + x + '" y="' + (H - padB - ha - hb) + '" width="' + barW + '" height="' + Math.max(1, hb) + '" rx="2" fill="' + colorB + '" opacity="0.9"><title>' + labelFn(p.bucket) + ' · ' + nameB + ': ' + fmt(b) + '</title></rect>';
    }
  });
  const step = Math.max(1, Math.floor(n / 8));
  for (let k = 0; k < n; k += step) {
    svg += '<text x="' + xOf(k) + '" y="' + (H - 6) + '" font-size="10" fill="#6b7280" text-anchor="middle">' + labelFn(points[k].bucket) + '</text>';
  }
  svg += "</svg>";
  const leg = '<div class="chart-legend"><span class="leg-item"><span class="leg-swatch" style="background:' + colorA + '"></span>' + nameA +
    '</span><span class="leg-item"><span class="leg-swatch" style="background:' + colorB + '"></span>' + nameB + "</span></div>";
  return svg + leg;
}

// 用量分析图：按指标切换单系列/堆叠/折线，按粒度切换 X 轴标签。
// 四种指标共用同一主图框（820×240），切换时布局不跳变。
function renderUsageChart(buckets, gran, metric) {
  const labelFn = gran === "hour" ? hourLabel : dayLabel;
  if (!buckets || !buckets.length) return '<div class="empty">所选区间暂无数据</div>';
  if (metric === "cost") {
    return renderBarChart(buckets, (b) => b.cost, "#f59e0b", fmtCost, labelFn, true);
  }
  if (metric === "requests") {
    return renderBarChart(buckets, (b) => b.requests, "#3b82f6", fmtInt, labelFn, true);
  }
  if (metric === "success") {
    return renderLineChart(buckets,
      (b) => (b.requests > 0 ? { pct: (b.success / b.requests) * 100, ok: b.success, n: b.requests } : null),
      labelFn);
  }
  return renderStackedBarChart(buckets,
    (b) => b.prompt_tokens, (b) => b.completion_tokens,
    "#3b82f6", "#10b981", "输入 tokens", "输出 tokens", fmtTokens, labelFn);
}

function hourLabel(ts) {
  const d = new Date(ts * 1000);
  return (d.getHours() < 10 ? "0" : "") + d.getHours() + ":00";
}

// ── 上游账号 ──
const AccountsPage = {
  data() {
    return {
      accs: [],
      providers: [],
      modal: null, // {id|null, form:{...}}
    };
  },
  mounted() { this.load(); },
  methods: {
    load() {
      req("GET", "/api/accounts").then((d) => { this.accs = d || []; }).catch((e) => toast(e.message, false));
    },
    openModal(id) {
      const p = req("GET", "/api/providers");
      const q = id ? req("GET", "/api/accounts") : Promise.resolve([]);
      Promise.all([p, q]).then((rs) => {
        this.providers = rs[0] || [];
        const acc = id ? (rs[1] || []).find((x) => x.id === id) : null;
        this.modal = {
          id: id || null,
          form: acc ? {
            name: acc.name, provider: acc.provider || "ark", base_url: acc.base_url || "",
            api_key: "", cap_responses: acc.cap_responses || 0, cap_images: acc.cap_images || 0,
            weight: acc.weight, status: acc.status,
          } : {
            name: "", provider: "ark", base_url: "", api_key: "",
            cap_responses: 0, cap_images: 0, weight: 1, status: "active",
          },
        };
      });
    },
    providerDef() {
      return this.providers.find((x) => x.id === (this.modal && this.modal.form.provider));
    },
    save() {
      const f = this.modal.form;
      if (!f.name.trim()) { toast("请输入名称", false); return; }
      if (!this.modal.id && !f.api_key.trim()) { toast("请输入 API Key", false); return; }
      const payload = {
        name: f.name.trim(), provider: f.provider, base_url: f.base_url.trim(),
        api_key: f.api_key.trim(), cap_responses: Number(f.cap_responses),
        cap_images: Number(f.cap_images), weight: Number(f.weight) || 1, status: f.status,
      };
      const p = this.modal.id
        ? req("PUT", "/api/accounts/" + this.modal.id, payload)
        : req("POST", "/api/accounts", payload);
      p.then(() => { toast("已保存"); this.modal = null; this.load(); })
        .catch((e) => toast(e.message, false));
    },
    del(a) {
      if (!confirm("确认删除该账号？将同时删除其模型映射。")) return;
      req("DELETE", "/api/accounts/" + a.id).then(() => { toast("已删除"); this.load(); });
    },
  },
  template: `
  <div class="page">
    <div class="page-title">上游账号</div>
    <div class="toolbar"><button class="btn btn-primary" @click="openModal(null)">+ 添加账号</button><div class="spacer"></div></div>
    <div class="card"><div class="table-wrap"><table><thead><tr>
      <th>名称</th><th>供应商</th><th>Key</th><th>状态</th><th>权重</th><th>请求/成功/失败</th><th>Token</th><th>图像</th><th>操作</th>
    </tr></thead><tbody>
      <tr v-if="!accs.length"><td colspan="9" class="empty">暂无账号</td></tr>
      <tr v-for="a in accs" :key="a.id">
        <td><strong>{{ a.name }}</strong></td><td>{{ a.provider || 'ark' }}</td>
        <td class="mono">{{ a.key_hint }}</td>
        <td><span :class="a.status==='active' ? 'tag tag-green' : 'tag tag-gray'">{{ a.status==='active' ? '启用' : '禁用' }}</span></td>
        <td>{{ a.weight }}</td>
        <td>{{ a.total_requests }}/{{ a.success_requests }}/{{ a.fail_requests }}</td>
        <td>{{ fmtTokens(a.total_tokens) }}</td><td>{{ a.total_images || 0 }}</td>
        <td><div class="row-actions">
          <button class="btn btn-outline btn-sm" @click="openModal(a.id)">编辑</button>
          <button class="btn btn-danger btn-sm" @click="del(a)">删除</button>
        </div></td>
      </tr>
    </tbody></table></div></div>

    <div v-if="modal" class="modal-mask" @click.self="modal=null">
      <div class="modal"><div class="modal-head"><h3>{{ modal.id ? '编辑账号' : '添加账号' }}</h3><button class="modal-close" @click="modal=null">×</button></div>
      <div class="modal-body">
        <div class="form-item"><label>名称 <span class="req">*</span></label><input v-model="modal.form.name" placeholder="例如：主账号-北京"/></div>
        <div class="form-row">
          <div class="form-item"><label>供应商</label>
            <select v-model="modal.form.provider">
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.display_name }}（{{ p.id }}）</option>
            </select></div>
          <div class="form-item"><label>权重</label><input v-model="modal.form.weight" type="number"/></div>
        </div>
        <div class="form-row">
          <div class="form-item"><label>状态</label>
            <select v-model="modal.form.status"><option value="active">启用</option><option value="disabled">禁用</option></select></div>
          <div class="form-item"><label>Base URL</label>
            <input v-model="modal.form.base_url" :placeholder="providerDef() && providerDef().default_base_url ? '留空使用默认：' + providerDef().default_base_url : '必填：该供应商无默认地址（http(s)://…）'"/></div>
        </div>
        <div class="form-item"><label>上游 API Key <span class="req">*</span>{{ modal.id ? '（留空表示不修改）' : '' }}</label>
          <input v-model="modal.form.api_key" placeholder="任意字符串，网关不做格式假设"/></div>
        <div class="form-row">
          <div class="form-item"><label>Responses 能力</label>
            <select v-model="modal.form.cap_responses"><option v-for="o in capOptions" :key="o.v" :value="o.v">{{ o.label }}</option></select></div>
          <div class="form-item"><label>图像能力</label>
            <select v-model="modal.form.cap_images"><option v-for="o in capOptions" :key="o.v" :value="o.v">{{ o.label }}</option></select></div>
        </div>
        <div class="form-item" style="color:var(--color-text-3)">并发 / RPM / TPM 限额请到「模型映射」按接入点配置；能力覆盖用于纠正自定义供应商的能力声明。</div>
      </div>
      <div class="modal-foot"><button class="btn btn-outline" @click="modal=null">取消</button>
        <button class="btn btn-primary" @click="save">保存</button></div>
      </div></div>
  </div>`,
};

// ── 模型映射 ──
const ModelsPage = {
  data() {
    return {
      models: [], accounts: [], eps: [],
      mModal: null, // 模型弹窗 {name|null, form, catHint}
      eModal: null, // 映射弹窗 {id|null, form}
      syncing: false, // 目录补全进行中
    };
  },
  mounted() { this.load(); },
  methods: {
    load() {
      Promise.all([req("GET", "/api/models"), req("GET", "/api/accounts"), req("GET", "/api/endpoints")])
        .then((rs) => {
          this.models = rs[0] || []; this.accounts = rs[1] || []; this.eps = rs[2] || [];
        })
        .catch((e) => toast(e.message, false));
    },
    accName(id) {
      const a = this.accounts.find((x) => x.id === id);
      return (a && a.name) || id;
    },
    // ── 模型 ──
    openModel(m) {
      this.mModal = m ? {
        name: m.name,
        form: { type: m.type || "text", display: m.display, description: m.description || "",
          fallback: (m.fallback || []).join(", "), enabled: m.enabled,
          price_input: m.price_input || 0, price_output: m.price_output || 0, price_image: m.price_image || 0,
          context_tokens: m.context_tokens || 0, max_output_tokens: m.max_output_tokens || 0 },
        catHint: "",
      } : {
        name: null,
        form: { type: "text", display: "", description: "", fallback: "", enabled: true,
          price_input: 0, price_output: 0, price_image: 0,
          context_tokens: 0, max_output_tokens: 0 },
        catHint: "",
      };
      if (m) this.lookupCat(m.name); // 编辑态：即时显示目录命中情况
    },
    // lookupCat 按模型名查内置目录，提示将自动补全的价格/能力（人工填写优先）。
    lookupCat(name) {
      if (!name || !this.mModal) return;
      req("GET", "/api/catalog/lookup?name=" + encodeURIComponent(name))
        .then((d) => {
          if (!this.mModal) return;
          this.mModal.catHint = d.found ? this.catText(d.entry) : "";
        })
        .catch(() => {});
    },
    catText(e) {
      if (!e) return "";
      const parts = [];
      if (e.max_input) parts.push("上下文 " + fmtTokens(e.max_input));
      if (e.max_output) parts.push("最大输出 " + fmtTokens(e.max_output));
      if (e.cost_in) parts.push("输入 $" + e.cost_in + "/1M");
      if (e.cost_out) parts.push("输出 $" + e.cost_out + "/1M");
      if (e.cost_image) parts.push("图像 $" + e.cost_image + "/张");
      return parts.join(" · ");
    },
    saveModel() {
      const f = this.mModal.form;
      const name = this.mModal.name;
      const fallback = f.fallback.split(",").map((s) => s.trim()).filter((s) => s.length > 0);
      const payload = {
        type: f.type, display: f.display.trim() || name, description: f.description.trim(),
        fallback, enabled: f.enabled,
        price_input: Number(f.price_input) || 0, price_output: Number(f.price_output) || 0,
        price_image: Number(f.price_image) || 0,
        context_tokens: Number(f.context_tokens) || 0, max_output_tokens: Number(f.max_output_tokens) || 0,
      };
      if (!name) {
        if (!f.name0 || !f.name0.trim()) { toast("请输入模型名", false); return; }
      }
      const p = name
        ? req("PUT", "/api/models/" + name, payload)
        : req("POST", "/api/models", { ...payload, name: (f.name0 || "").trim() });
      p.then((d) => {
        const filled = d && d.auto_filled;
        toast(filled && filled.length ? "已保存，目录自动补全：" + filled.join("、") : "已保存");
        this.mModal = null; this.load();
      })
        .catch((e) => toast(e.message, false));
    },
    // syncCatalog 在线刷新目录（失败回落内嵌快照）并为所有模型补全空缺字段。
    syncCatalog() {
      if (this.syncing) return;
      this.syncing = true;
      req("POST", "/api/models/metadata-sync")
        .then((d) => {
          const src = d.fetch_ok ? "在线目录" : "内嵌快照";
          toast(d.updated > 0 ? "已从" + src + "补全 " + d.updated + " 个模型" : "目录已是最新（来源：" + src + "）");
          this.load();
        })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.syncing = false; });
    },
    delModel(m) {
      if (!confirm("确认删除该模型及其所有映射？")) return;
      req("DELETE", "/api/models/" + m.name).then(() => { toast("已删除"); this.load(); });
    },
    // ── 映射 ──
    openEp(e) {
      this.eModal = e ? {
        id: e.id,
        form: { account_id: e.account_id, model: e.model, ep: e.ep, enabled: e.enabled,
          weight: e.weight || 0, max_concurrency: e.max_concurrency || 0,
          rpm_limit: e.rpm_limit || 0, tpm_limit: e.tpm_limit || 0 },
      } : {
        id: null,
        form: { account_id: this.accounts.length ? this.accounts[0].id : "", model: this.models.length ? this.models[0].name : "",
          ep: "", enabled: true, weight: 0, max_concurrency: 0, rpm_limit: 0, tpm_limit: 0 },
      };
    },
    saveEp() {
      const f = this.eModal.form;
      if (!f.account_id || !f.model || !f.ep.trim()) { toast("请完整填写", false); return; }
      const payload = { account_id: f.account_id, model: f.model, ep: f.ep.trim(), enabled: f.enabled,
        weight: Number(f.weight) || 0, max_concurrency: Number(f.max_concurrency) || 0,
        rpm_limit: Number(f.rpm_limit) || 0, tpm_limit: Number(f.tpm_limit) || 0 };
      const p = this.eModal.id
        ? req("PUT", "/api/endpoints/" + this.eModal.id, payload)
        : req("POST", "/api/endpoints", payload);
      p.then(() => { toast("已保存"); this.eModal = null; this.load(); })
        .catch((e) => toast(e.message, false));
    },
    delEp(e) {
      if (!confirm("确认删除该映射？")) return;
      req("DELETE", "/api/endpoints/" + e.id).then(() => { toast("已删除"); this.load(); });
    },
  },
  template: `
  <div class="page">
    <div class="page-title">模型映射</div>
    <div class="toolbar">
      <button class="btn btn-primary" @click="openModel(null)">+ 新建模型</button>
      <button class="btn btn-outline" @click="openEp(null)">+ 添加映射</button>
      <div class="spacer"></div>
      <button class="btn btn-outline" :disabled="syncing" @click="syncCatalog">{{ syncing ? '补全中…' : '⟳ 从目录补全' }}</button>
    </div>
    <div class="card"><div class="card-head"><div class="card-title">模型目录（可配置跨模型 fallback 链，仅限同类型）</div></div>
      <div class="table-wrap"><table><thead><tr>
        <th>模型名</th><th>类型</th><th>显示名</th><th>上下文 / 最大输出</th><th>价格</th><th>fallback</th><th>描述</th><th>状态</th><th>操作</th>
      </tr></thead><tbody>
        <tr v-if="!models.length"><td colspan="9" class="empty">暂无模型</td></tr>
        <tr v-for="m in models" :key="m.name">
          <td class="mono">{{ m.name }}</td>
          <td><span :class="m.type==='image' ? 'tag tag-purple' : 'tag tag-blue'">{{ m.type==='image' ? '图像' : '文本' }}</span></td>
          <td>{{ m.display }}</td>
          <td class="mono">
            <template v-if="m.context_tokens || m.max_output_tokens">{{ m.context_tokens ? fmtTokens(m.context_tokens) : '—' }} / {{ m.max_output_tokens ? fmtTokens(m.max_output_tokens) : '—' }}</template>
            <span v-else>—</span>
          </td>
          <td class="cost">
            <template v-if="m.type==='image'">{{ fmtCost(m.price_image) }} / 张</template>
            <template v-else-if="m.price_input || m.price_output">{{ fmtCost(m.price_input) }} / {{ fmtCost(m.price_output) }} per 1M</template>
            <span v-else class="tag tag-gray">未定价</span>
          </td>
          <td class="mono">{{ (m.fallback && m.fallback.length) ? m.fallback.join(' → ') : '—' }}</td>
          <td>{{ m.description }}</td>
          <td><span :class="m.enabled ? 'tag tag-green' : 'tag tag-gray'">{{ m.enabled ? '启用' : '停用' }}</span></td>
          <td><div class="row-actions">
            <button class="btn btn-outline btn-sm" @click="openModel(m)">编辑</button>
            <button class="btn btn-danger btn-sm" @click="delModel(m)">删除</button>
          </div></td>
        </tr>
      </tbody></table></div></div>
    <div class="card"><div class="card-head"><div class="card-title">映射表（元组级流控）</div></div>
      <div class="table-wrap"><table><thead><tr>
        <th>账号</th><th>模型名</th><th>上游模型 / 接入点</th><th>权重</th><th>并发</th><th>RPM</th><th>TPM</th><th>状态</th><th>操作</th>
      </tr></thead><tbody>
        <tr v-if="!eps.length"><td colspan="9" class="empty">暂无映射</td></tr>
        <tr v-for="e in eps" :key="e.id">
          <td>{{ accName(e.account_id) }}</td><td class="mono">{{ e.model }}</td><td class="mono">{{ e.ep }}</td>
          <td>{{ e.weight || '继承' }}</td><td>{{ e.max_concurrency || '不限' }}</td>
          <td>{{ e.rpm_limit || '不限' }}</td><td>{{ e.tpm_limit || '不限' }}</td>
          <td><span :class="e.enabled ? 'tag tag-green' : 'tag tag-gray'">{{ e.enabled ? '启用' : '停用' }}</span></td>
          <td><div class="row-actions">
            <button class="btn btn-outline btn-sm" @click="openEp(e)">编辑</button>
            <button class="btn btn-danger btn-sm" @click="delEp(e)">删除</button>
          </div></td>
        </tr>
      </tbody></table></div></div>

    <!-- 模型弹窗 -->
    <div v-if="mModal" class="modal-mask" @click.self="mModal=null">
      <div class="modal"><div class="modal-head"><h3>{{ mModal.name ? '编辑模型' : '新建模型' }}</h3><button class="modal-close" @click="mModal=null">×</button></div>
      <div class="modal-body">
        <div class="form-item"><label>模型名（下游调用用，唯一）<span class="req" v-if="!mModal.name">*</span></label>
          <input v-if="!mModal.name" v-model="mModal.form.name0" placeholder="例如 doubao-seed-1-6" @blur="lookupCat(mModal.form.name0 && mModal.form.name0.trim())"/>
          <input v-else :value="mModal.name" disabled/>
          <div v-if="mModal.catHint" class="form-tip">目录命中：{{ mModal.catHint }}（保存时自动补全空缺字段，人工填写优先）</div></div>
        <div class="form-item"><label>类型</label>
          <select v-model="mModal.form.type">
            <option value="text">文本（chat / responses）</option>
            <option value="image">图像（images/generations）</option>
          </select></div>
        <div class="form-item"><label>显示名</label><input v-model="mModal.form.display"/></div>
        <div class="form-row three" v-if="mModal.form.type==='image'">
          <div class="form-item"><label>图像单价（$ / 张）</label><input v-model="mModal.form.price_image" type="number" step="0.0001"/></div>
        </div>
        <div class="form-row" v-else>
          <div class="form-item"><label>输入单价（$ / 1M tokens）</label><input v-model="mModal.form.price_input" type="number" step="0.0001"/></div>
          <div class="form-item"><label>输出单价（$ / 1M tokens）</label><input v-model="mModal.form.price_output" type="number" step="0.0001"/></div>
        </div>
        <div class="form-row" v-if="mModal.form.type!=='image'">
          <div class="form-item"><label>上下文窗口（tokens，0=不校验）</label><input v-model="mModal.form.context_tokens" type="number"/></div>
          <div class="form-item"><label>最大输出（tokens，0=不裁剪）</label><input v-model="mModal.form.max_output_tokens" type="number"/></div>
        </div>
        <div class="form-item"><label>fallback 链（逗号分隔，按顺序尝试；仅限同类型且已存在的模型）</label>
          <input v-model="mModal.form.fallback" placeholder="例如 doubao-seed-1-5, doubao-lite"/></div>
        <div class="form-item"><label>描述</label><input v-model="mModal.form.description"/></div>
        <div class="form-item"><label>状态</label>
          <select v-model="mModal.form.enabled"><option :value="true">启用</option><option :value="false">停用</option></select></div>
      </div>
      <div class="modal-foot"><button class="btn btn-outline" @click="mModal=null">取消</button>
        <button class="btn btn-primary" @click="saveModel">保存</button></div>
      </div></div>

    <!-- 映射弹窗 -->
    <div v-if="eModal" class="modal-mask" @click.self="eModal=null">
      <div class="modal"><div class="modal-head"><h3>{{ eModal.id ? '编辑接入点映射' : '添加接入点映射' }}</h3><button class="modal-close" @click="eModal=null">×</button></div>
      <div class="modal-body">
        <div class="form-item"><label>账号 <span class="req">*</span></label>
          <select v-model="eModal.form.account_id"><option v-for="a in accounts" :key="a.id" :value="a.id">{{ a.name }}</option></select></div>
        <div class="form-item"><label>模型名 <span class="req">*</span></label>
          <select v-model="eModal.form.model"><option v-for="m in models" :key="m.name" :value="m.name">{{ m.name }}</option></select></div>
        <div class="form-item"><label>上游模型 / 接入点 <span class="req">*</span></label>
          <input v-model="eModal.form.ep" placeholder="如 ep-2025xxxxxxx（Ark）或 gpt-4o（OpenAI），按账号供应商填写"/></div>
        <div class="form-row three">
          <div class="form-item"><label>权重</label><input v-model="eModal.form.weight" type="number"/></div>
          <div class="form-item"><label>并发上限</label><input v-model="eModal.form.max_concurrency" type="number"/></div>
          <div class="form-item"><label>RPM</label><input v-model="eModal.form.rpm_limit" type="number"/></div>
        </div>
        <div class="form-item"><label>TPM</label><input v-model="eModal.form.tpm_limit" type="number"/></div>
        <div class="form-item"><label>状态</label>
          <select v-model="eModal.form.enabled"><option :value="true">启用</option><option :value="false">停用</option></select></div>
      </div>
      <div class="modal-foot"><button class="btn btn-outline" @click="eModal=null">取消</button>
        <button class="btn btn-primary" @click="saveEp">保存</button></div>
      </div></div>
  </div>`,
};

// ── 子 Key ──
const SubKeysPage = {
  components: { CheckGroup },
  data() {
    return { subs: [], models: [], accounts: [], modal: null };
  },
  mounted() { this.load(); },
  methods: {
    load() {
      Promise.all([req("GET", "/api/subkeys"), req("GET", "/api/models"), req("GET", "/api/accounts")])
        .then((rs) => { this.subs = rs[0] || []; this.models = rs[1] || []; this.accounts = rs[2] || []; })
        .catch((e) => toast(e.message, false));
    },
    openModal(id) {
      this.modal = {
        id,
        form: { name: "", key: "", allowed_models: [], allowed_accounts: [],
          daily_limit_tokens: 0, daily_limit_images: 0 },
      };
      if (id) {
        req("GET", "/api/subkeys").then((subs) => {
          const s = (subs || []).find((x) => x.id === id);
          if (s) {
            this.modal.form.name = s.name;
            this.modal.form.allowed_models = s.allowed_models || [];
            this.modal.form.allowed_accounts = s.allowed_accounts || [];
            this.modal.form.daily_limit_tokens = s.daily_limit_tokens;
            this.modal.form.daily_limit_images = s.daily_limit_images || 0;
          }
        });
      }
    },
    save() {
      const f = this.modal.form;
      const payload = {
        name: f.name.trim() || "未命名",
        allowed_models: f.allowed_models,
        allowed_accounts: f.allowed_accounts,
        daily_limit_tokens: Number(f.daily_limit_tokens) || 0,
        daily_limit_images: Number(f.daily_limit_images) || 0,
      };
      let p;
      if (this.modal.id) {
        p = req("PUT", "/api/subkeys/" + this.modal.id, payload);
      } else {
        if (f.key.trim()) payload.key = f.key.trim();
        p = req("POST", "/api/subkeys", payload);
      }
      p.then((d) => {
        toast(d && d.key ? "已创建，Key：" + d.key : "已保存");
        this.modal = null; this.load();
      }).catch((e) => toast(e.message, false));
    },
    del(s) {
      if (!confirm("确认删除该子 Key？")) return;
      req("DELETE", "/api/subkeys/" + s.id).then(() => { toast("已删除"); this.load(); });
    },
    copy(text) {
      navigator.clipboard.writeText(text).then(() => toast("已复制"));
    },
  },
  template: `
  <div class="page">
    <div class="page-title">子 API Key</div>
    <div class="toolbar"><button class="btn btn-primary" @click="openModal(null)">+ 新建子 Key</button><div class="spacer"></div></div>
    <div class="card"><div class="table-wrap"><table><thead><tr>
      <th>名称</th><th>Key</th><th>状态</th><th>可访问模型</th><th>请求</th><th>Token</th><th>图像</th><th>操作</th>
    </tr></thead><tbody>
      <tr v-if="!subs.length"><td colspan="8" class="empty">暂无子 Key</td></tr>
      <tr v-for="s in subs" :key="s.id">
        <td><strong>{{ s.name || s.id }}</strong></td>
        <td class="mono"><span class="code-copy" title="点击复制" @click="copy(s.key)">{{ s.key }}</span></td>
        <td><span :class="s.enabled ? 'tag tag-green' : 'tag tag-gray'">{{ s.enabled ? '启用' : '禁用' }}</span></td>
        <td>{{ (s.allowed_models && s.allowed_models.length) ? s.allowed_models.join(', ') : '全部' }}</td>
        <td>{{ s.total_requests }}</td><td>{{ fmtTokens(s.total_tokens) }}</td><td>{{ s.total_images || 0 }}</td>
        <td><div class="row-actions">
          <button class="btn btn-outline btn-sm" @click="openModal(s.id)">编辑</button>
          <button class="btn btn-danger btn-sm" @click="del(s)">删除</button>
        </div></td>
      </tr>
    </tbody></table></div></div>

    <div v-if="modal" class="modal-mask" @click.self="modal=null">
      <div class="modal"><div class="modal-head"><h3>{{ modal.id ? '编辑子 Key' : '新建子 Key' }}</h3><button class="modal-close" @click="modal=null">×</button></div>
      <div class="modal-body">
        <div class="form-item"><label>名称</label><input v-model="modal.form.name" placeholder="例如：给团队的 Key"/></div>
        <div class="form-item" v-if="!modal.id"><label>自定义 Key（留空自动生成 sk-xxx）</label><input v-model="modal.form.key" placeholder="sk-..."/></div>
        <div class="form-item"><label>可访问模型（不勾选 = 全部）</label>
          <CheckGroup :options="models.map(m => ({v: m.name, l: m.name}))" v-model="modal.form.allowed_models"/></div>
        <div class="form-item"><label>可访问账号（不勾选 = 全部）</label>
          <CheckGroup :options="accounts.map(a => ({v: a.id, l: a.name}))" v-model="modal.form.allowed_accounts"/></div>
        <div class="form-item"><label>当日 Token 限额（0 = 不限）</label><input v-model="modal.form.daily_limit_tokens" type="number"/></div>
        <div class="form-item"><label>当日图像张数限额（0 = 不限）</label><input v-model="modal.form.daily_limit_images" type="number"/></div>
      </div>
      <div class="modal-foot"><button class="btn btn-outline" @click="modal=null">取消</button>
        <button class="btn btn-primary" @click="save">保存</button></div>
      </div></div>
  </div>`,
};

// ── 请求日志 ──
const LogsPage = {
  data() { return { logs: [] }; },
  mounted() { this.load(); },
  methods: {
    load() {
      req("GET", "/api/logs?limit=300").then((d) => { this.logs = d || []; }).catch((e) => toast(e.message, false));
    },
    clear() {
      if (!confirm("确认清空所有日志？")) return;
      req("DELETE", "/api/logs").then(() => { toast("已清空"); this.load(); });
    },
  },
  template: `
  <div class="page">
    <div class="page-title">请求日志</div>
    <div class="toolbar">
      <button class="btn btn-outline" @click="load">刷新</button>
      <button class="btn btn-danger" @click="clear">清空日志</button>
      <div class="spacer"></div>
    </div>
    <div class="card"><div class="table-wrap"><table><thead><tr>
      <th>时间</th><th>子 Key</th><th>账号</th><th>供应商</th><th>请求模型</th><th>真实模型</th><th>输入</th><th>输出</th><th>总 Token</th><th>图像</th><th>成本</th><th>耗时</th><th>状态</th><th>错误</th>
    </tr></thead><tbody>
      <tr v-if="!logs.length"><td colspan="14" class="empty">暂无日志</td></tr>
      <tr v-for="l in logs" :key="l.id">
        <td>{{ fmtTime(l.ts) }}</td>
        <td>{{ l.subkey_name || l.subkey_id }}</td>
        <td>{{ l.account_name || l.account_id }}</td>
        <td>{{ l.provider || '-' }}</td>
        <td><span class="mono">{{ l.requested_model || l.model }}</span> <span v-if="l.requested_model && l.requested_model !== l.model" class="tag tag-orange" title="fallback">↓</span></td>
        <td class="mono">{{ l.model }}</td>
        <td>{{ l.prompt_tokens }}</td><td>{{ l.completion_tokens }}</td><td>{{ l.total_tokens }}</td>
        <td>{{ l.modality === 'image' ? (l.image_count || 0) + ' 张' : '—' }}</td>
        <td class="cost">{{ fmtCost(l.cost) }}</td>
        <td>{{ l.latency_ms }}ms</td>
        <td><span :class="l.status === 'ok' ? 'tag tag-green' : 'tag tag-red'">{{ l.status === 'ok' ? 'OK' : 'ERR' }}</span></td>
        <td style="max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--color-text-3)" :title="l.error">{{ l.error }}</td>
      </tr>
    </tbody></table></div></div>
  </div>`,
};

// ── 用量分析（对齐火山方舟「用量统计」交互：区间 + 粒度 + 维度下钻） ──
const USAGE_DIMS = [
  { v: "", label: "全部" },
  { v: "model", label: "模型" },
  { v: "subkey", label: "子 Key" },
  { v: "account", label: "账号" },
  { v: "endpoint", label: "接入点" },
  { v: "provider", label: "供应商" },
];

const UsagePage = {
  data() {
    return {
      from: toDateInput(new Date(Date.now() - 6 * 86400000)),
      to: toDateInput(new Date()),
      gran: "day",
      dim: "",
      entity: "",
      metric: "tokens",
      dims: USAGE_DIMS,
      metrics: [
        { v: "tokens", label: "Token" },
        { v: "cost", label: "费用" },
        { v: "requests", label: "次数" },
        { v: "success", label: "成功率" },
      ],
      r: null,
      loading: false,
    };
  },
  computed: {
    summary() { return (this.r && this.r.summary) || {}; },
    facets() { return (this.r && this.r.facets) || []; },
    buckets() { return (this.r && this.r.series) || []; },
    successRate() {
      const s = this.summary;
      return s.requests ? fmtPct((s.success / s.requests) * 100) : "—";
    },
    successRateCls() {
      const s = this.summary;
      if (!s.requests) return "ic-gray";
      const p = (s.success / s.requests) * 100;
      return p >= 99 ? "ic-green" : p >= 90 ? "ic-orange" : "ic-red";
    },
    dimLabel() {
      const d = USAGE_DIMS.find((x) => x.v === this.dim);
      return d ? d.label : "";
    },
    entityLabel() {
      if (!this.entity) return "全部";
      const f = this.facets.find((x) => x.key === this.entity);
      return (f && f.label) || this.entity;
    },
    chartTitle() { return { tokens: "Token 用量", cost: "费用", requests: "调用次数", success: "成功率" }[this.metric]; },
    chartUnit() { return { tokens: "tokens", cost: "USD", requests: "次", success: "%" }[this.metric]; },
    chartHtml() { return renderUsageChart(this.buckets, this.gran, this.metric); },
  },
  mounted() { this.load(); },
  methods: {
    load() {
      this.loading = true;
      const from = Math.floor(new Date(this.from + "T00:00:00").getTime() / 1000);
      const to = Math.floor(new Date(this.to + "T23:59:59").getTime() / 1000);
      req("GET", "/api/usage/stats?from=" + from + "&to=" + to +
        "&gran=" + this.gran + "&dim=" + this.dim + "&entity=" + encodeURIComponent(this.entity))
        .then((d) => { this.r = d; })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.loading = false; });
    },
    onDimChange() { this.entity = ""; this.load(); },
    pick(key) {
      this.entity = this.entity === key ? "" : key;
      this.load();
    },
    rateOf(s) { return s && s.requests ? fmtPct((s.success / s.requests) * 100) : "—"; },
  },
  template: `
  <div class="page">
    <div class="page-head">
      <div class="page-title" style="margin:0">用量分析</div>
      <div class="row-actions">
        <button class="btn btn-outline btn-sm" @click="load" :disabled="loading">↻ 刷新</button>
        <button class="btn btn-outline btn-sm" @click="toggleDark">🌓</button>
      </div>
    </div>

    <div class="toolbar">
      <input type="date" v-model="from" style="width:150px" @change="load"/>
      <span style="color:var(--color-text-3)">~</span>
      <input type="date" v-model="to" style="width:150px" @change="load"/>
      <div class="seg">
        <div class="seg-item" :class="{active: gran==='day'}" @click="gran='day'; load()">天</div>
        <div class="seg-item" :class="{active: gran==='hour'}" @click="gran='hour'; load()">小时</div>
      </div>
      <select v-model="dim" style="width:140px" @change="onDimChange">
        <option v-for="d in dims" :key="d.v" :value="d.v">{{ d.label }}</option>
      </select>
      <select v-if="dim" v-model="entity" style="width:230px" @change="load">
        <option value="">全部</option>
        <option v-for="f in facets" :key="f.key" :value="f.key">{{ f.label }}</option>
      </select>
      <div class="seg">
        <div class="seg-item" v-for="m in metrics" :key="m.v" :class="{active: metric===m.v}" @click="metric=m.v; load()">{{ m.label }}</div>
      </div>
    </div>

    <div class="stat-row">
      <div class="stat-card"><div class="ic ic-blue">⚡</div><div class="body"><div class="v">{{ summary.requests || 0 }}</div><div class="l">调用次数</div></div></div>
      <div class="stat-card"><div class="ic" :class="successRateCls">✓</div><div class="body"><div class="v">{{ successRate }}</div><div class="l">成功率</div></div></div>
      <div class="stat-card"><div class="ic ic-blue">⬤</div><div class="body"><div class="v">{{ fmtTokens(summary.total_tokens) }}</div><div class="l">总 Tokens</div></div></div>
      <div class="stat-card"><div class="ic ic-green">↓</div><div class="body"><div class="v">{{ fmtTokens(summary.prompt_tokens) }}</div><div class="l">输入 Tokens</div></div></div>
      <div class="stat-card"><div class="ic ic-purple">↑</div><div class="body"><div class="v">{{ fmtTokens(summary.completion_tokens) }}</div><div class="l">输出 Tokens</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">🖼</div><div class="body"><div class="v">{{ summary.images || 0 }}</div><div class="l">图像（张）</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">💰</div><div class="body"><div class="v">{{ fmtCost(summary.cost) }}</div><div class="l">费用</div></div></div>
    </div>

    <div class="card">
      <div class="card-head"><div class="card-title">{{ chartTitle }}（{{ chartUnit }}） · {{ dimLabel }}<template v-if="dim">：{{ entityLabel }}</template> · 按{{ gran === 'hour' ? '小时' : '天' }}</div></div>
      <div class="chart-wrap" v-html="chartHtml"></div>
    </div>

    <div class="card" v-if="dim">
      <div class="card-head"><div class="card-title">{{ dimLabel }}拆分（点击行下钻，再点取消）</div></div>
      <div class="table-wrap"><table><thead><tr>
        <th>{{ dimLabel }}</th><th>次数</th><th>成功率</th><th>Tokens</th><th>图像</th><th>成本</th>
      </tr></thead><tbody>
        <tr class="clickable" :class="{selected: entity===''}" @click="pick('')">
          <td>全部</td><td>{{ summary.requests || 0 }}</td><td>{{ successRate }}</td>
          <td>{{ fmtTokens(summary.total_tokens) }}</td><td>{{ summary.images || '—' }}</td><td class="cost">{{ fmtCost(summary.cost) }}</td>
        </tr>
        <tr v-for="f in facets" :key="f.key" class="clickable" :class="{selected: entity===f.key}" @click="pick(f.key)">
          <td class="mono">{{ f.label }}</td><td>{{ f.requests }}</td><td>{{ rateOf(f) }}</td>
          <td>{{ fmtTokens(f.total_tokens) }}</td><td>{{ f.images || '—' }}</td><td class="cost">{{ fmtCost(f.cost) }}</td>
        </tr>
        <tr v-if="!facets.length"><td colspan="6" class="empty">所选区间暂无数据</td></tr>
      </tbody></table></div>
    </div>
  </div>`,
};

// ── 设置（网关运行时 + 本浏览器偏好） ──
const SettingsPage = {
  data() {
    return {
      prefs,
      rt: null,          // 网关运行时设置（服务端）
      rtForm: { request_timeout_sec: 0, first_token_timeout_sec: 0 },
      savingRt: false,
    };
  },
  mounted() { this.loadRuntime(); },
  methods: {
    save() { savePrefs(); },
    resetBg() {
      this.prefs.bgUrl = DEFAULT_BG_URL;
      this.prefs.bgBlur = 40;
      savePrefs();
      toast("已恢复默认背景");
    },
    loadRuntime() {
      req("GET", "/api/settings/runtime")
        .then((d) => {
          this.rt = d;
          this.rtForm = {
            request_timeout_sec: d.request_timeout_sec,
            first_token_timeout_sec: d.first_token_timeout_sec,
          };
        })
        .catch((e) => toast(e.message, false));
    },
    saveRuntime() {
      if (this.savingRt) return;
      this.savingRt = true;
      req("PUT", "/api/settings/runtime", {
        request_timeout_sec: Number(this.rtForm.request_timeout_sec) || 0,
        first_token_timeout_sec: Number(this.rtForm.first_token_timeout_sec) || 0,
      })
        .then((d) => {
          this.rt = d;
          toast("已保存，对后续请求立即生效");
        })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.savingRt = false; });
    },
    resetRuntime() {
      if (!this.rt || !this.rt.defaults) return;
      this.rtForm = {
        request_timeout_sec: this.rt.defaults.request_timeout_sec,
        first_token_timeout_sec: this.rt.defaults.first_token_timeout_sec,
      };
      this.saveRuntime();
    },
  },
  template: `
  <div class="page">
    <div class="page-title">设置</div>
    <div class="page-sub">上游超时保存在网关，对所有客户端生效；外观与总览偏好只存本浏览器。</div>
    <div class="card"><div class="card-head"><div class="card-title">上游超时（网关级，热生效无需重启）</div></div>
      <div class="form-row">
        <div class="form-item"><label>请求超时（秒，0 = 不限）</label>
          <input v-model.number="rtForm.request_timeout_sec" type="number" min="0" :max="rt && rt.max_timeout_sec || 3600" step="1"/>
          <div class="form-tip">非流式 chat / responses / images 单次调用的整体上限（含读完响应体）。</div></div>
        <div class="form-item"><label>流式首字节超时（秒，0 = 关闭）</label>
          <input v-model.number="rtForm.first_token_timeout_sec" type="number" min="0" :max="rt && rt.max_timeout_sec || 3600" step="1"/>
          <div class="form-tip">上游建连后多久没吐出第一个字节就换下一个接入点重试（首字节前重试对客户端无感）。</div></div>
      </div>
      <div class="row-actions">
        <button class="btn btn-primary" :disabled="savingRt" @click="saveRuntime">{{ savingRt ? '保存中…' : '保存' }}</button>
        <button class="btn btn-outline" :disabled="savingRt" @click="resetRuntime">恢复默认</button>
      </div>
      <div class="kv" v-if="rt" style="margin-top:12px">
        <span class="k">会话粘性 TTL</span><span class="v mono">{{ rt.session_ttl_sec }}s（ARKGATE_SESSION_TTL）</span></div>
      <div class="kv" v-if="rt"><span class="k">跨接入点重试上限</span><span class="v mono">{{ rt.max_retries }} 次</span></div>
    </div>
    <div class="card"><div class="card-head"><div class="card-title">外观（仅本浏览器）</div></div>
      <div class="form-item"><label>背景图地址（留空关闭；可填随机图床或固定图片 URL）</label>
        <div style="display:flex;gap:8px">
          <input v-model="prefs.bgUrl" placeholder="https://img.paulzzh.com/touhou/random" @input="save"/>
          <button class="btn btn-outline" style="white-space:nowrap" @click="resetBg">恢复默认</button>
        </div></div>
      <div class="form-item"><label>背景模糊：{{ prefs.bgBlur }}%</label>
        <input type="range" min="0" max="100" v-model.number="prefs.bgBlur" @input="save" style="width:100%"/></div>
      <div class="form-item"><label>主题</label>
        <select v-model="prefs.theme" @change="save">
          <option value="light">浅色</option>
          <option value="dark">深色</option>
        </select></div>
    </div>
    <div class="card"><div class="card-head"><div class="card-title">总览（仅本浏览器）</div></div>
      <div class="form-item"><label>自动刷新间隔</label>
        <select v-model.number="prefs.overviewAuto" @change="save">
          <option :value="0">关闭（手动刷新）</option>
          <option :value="10">每 10 秒</option>
          <option :value="30">每 30 秒</option>
          <option :value="60">每 60 秒</option>
        </select></div>
    </div>
  </div>`,
};

// ── 管理端外壳 ──
const MENU = [
  { key: "overview", label: "总览", icon: "📊", comp: "OverviewPage" },
  { key: "usage", label: "用量分析", icon: "📈", comp: "UsagePage" },
  { key: "accounts", label: "上游账号", icon: "🏛", comp: "AccountsPage" },
  { key: "models", label: "模型映射", icon: "🧩", comp: "ModelsPage" },
  { key: "subkeys", label: "子 Key", icon: "🔑", comp: "SubKeysPage" },
  { key: "logs", label: "请求日志", icon: "📄", comp: "LogsPage" },
  { key: "settings", label: "设置", icon: "⚙️", comp: "SettingsPage" },
];

const AdminShell = {
  emits: ["logout"],
  data() { return { view: "overview", prefs, host: location.origin }; },
  methods: {
    logout() {
      state.adminToken = "";
      localStorage.removeItem("arkgate_token");
      this.$emit("logout");
    },
    toggleHelp() {
      prefs.helpOpen = !prefs.helpOpen;
      savePrefs();
    },
    copy(text) { navigator.clipboard.writeText(text).then(() => toast("已复制")); },
  },
  template: `
  <div class="layout">
    <aside class="sidebar">
      <div class="logo"><span class="dot"></span>ArkGate<span class="ver">v1.1</span></div>
      <div class="nav">
        <div v-for="m in menu" :key="m.key" class="nav-item" :class="{active: view===m.key}" @click="view=m.key">
          <span class="ic">{{ m.icon }}</span>{{ m.label }}
        </div>
      </div>
      <div class="side-help">
        <div class="side-help-head" @click="toggleHelp">
          <span>📖 使用说明</span><span class="arr">{{ prefs.helpOpen ? '▾' : '▸' }}</span>
        </div>
        <div v-show="prefs.helpOpen" class="side-help-body">
          <div class="sh-row">Base URL：<span class="mono linkish" title="点击复制" @click="copy(host + '/v1')">{{ host }}/v1</span></div>
          <div class="sh-row mono">POST /v1/chat/completions</div>
          <div class="sh-row mono">POST /v1/responses</div>
          <div class="sh-row mono">POST /v1/images/generations</div>
          <div class="sh-row mono">GET /v1/models</div>
          <div class="sh-note">鉴权：Authorization: Bearer sk-你的子Key</div>
        </div>
      </div>
      <div style="padding:12px">
        <button class="btn btn-outline" style="width:100%" @click="logout">退出登录</button>
      </div>
    </aside>
    <main class="main">
      <component :is="current"></component>
    </main>
  </div>`,
  computed: {
    menu() { return MENU; },
    current() {
      const m = MENU.find((x) => x.key === this.view) || MENU[0];
      return m.comp;
    },
  },
};

// ── 子 Key 自助门户 ──
const PortalPage = {
  emits: ["logout"],
  data() { return { d: null, busy: false }; },
  computed: {
    tokenProgress() {
      if (!this.d || !this.d.daily_limit_tokens) return null;
      const p = (this.d.today.tokens / this.d.daily_limit_tokens) * 100;
      return { pct: Math.min(100, p), cls: p >= 90 ? "danger" : p >= 70 ? "warn" : "" };
    },
    imageProgress() {
      if (!this.d || !this.d.daily_limit_images) return null;
      const p = (this.d.today.images / this.d.daily_limit_images) * 100;
      return { pct: Math.min(100, p), cls: p >= 90 ? "danger" : p >= 70 ? "warn" : "" };
    },
  },
  mounted() { this.load(); },
  methods: {
    load() {
      this.busy = true;
      req("GET", "/api/portal/overview", null, { key: state.subKey })
        .then((d) => { this.d = d; })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.busy = false; });
    },
    logout() {
      state.subKey = "";
      localStorage.removeItem("arkgate_sk");
      this.$emit("logout");
    },
  },
  template: `
  <div>
    <div class="portal-bar">
      <div class="logo"><span class="dot"></span>ArkGate 用量门户</div>
      <div class="row-actions">
        <span class="tag tag-blue">{{ d && d.name ? d.name : '我的 Key' }}</span>
        <button class="btn btn-outline btn-sm" @click="load" :disabled="busy">↻ 刷新</button>
        <button class="btn btn-outline btn-sm" @click="toggleDark">🌓</button>
        <button class="btn btn-outline btn-sm" @click="logout">退出</button>
      </div>
    </div>
    <div class="page" v-if="d">
      <div class="stat-row">
        <div class="stat-card"><div class="ic ic-blue">⬤</div><div class="body"><div class="v">{{ fmtTokens(d.today.tokens) }}</div><div class="l">今日 Tokens</div></div></div>
        <div class="stat-card"><div class="ic ic-green">⚡</div><div class="body"><div class="v">{{ d.today.requests }}</div><div class="l">今日请求数</div></div></div>
        <div class="stat-card"><div class="ic ic-purple">🖼</div><div class="body"><div class="v">{{ d.today.images }}</div><div class="l">今日图像（张）</div></div></div>
        <div class="stat-card"><div class="ic ic-orange">💰</div><div class="body"><div class="v">{{ fmtCost(d.today.cost) }}</div><div class="l">今日成本</div></div></div>
        <div class="stat-card"><div class="ic ic-green">✅</div><div class="body"><div class="v">{{ fmtPct(d.success_rate_7d) }}</div><div class="l">7 天成功率</div></div></div>
      </div>

      <div class="card" v-if="d.daily_limit_tokens || d.daily_limit_images">
        <div class="card-head"><div class="card-title">今日限额</div></div>
        <template v-if="d.daily_limit_tokens">
          <div class="kv"><span class="k">Token 限额</span><span class="v">{{ d.today.tokens }} / {{ fmtTokens(d.daily_limit_tokens) }}</span></div>
          <div class="progress" style="margin:8px 0 14px"><div class="bar" :class="tokenProgress.cls" :style="{width: tokenProgress.pct + '%'}"></div></div>
        </template>
        <template v-if="d.daily_limit_images">
          <div class="kv"><span class="k">图像张数限额</span><span class="v">{{ d.today.images }} / {{ d.daily_limit_images }}</span></div>
          <div class="progress" style="margin:8px 0 14px"><div class="bar" :class="imageProgress.cls" :style="{width: imageProgress.pct + '%'}"></div></div>
        </template>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">累计（自开通以来）与最近 7 天</div></div>
        <div class="table-wrap"><table><thead><tr><th>范围</th><th>请求</th><th>成功</th><th>Tokens</th><th>图像</th><th>成本</th></tr></thead><tbody>
          <tr><td>最近 7 天</td><td>{{ d.week.requests }}</td><td>{{ d.week.success }}</td><td>{{ fmtTokens(d.week.tokens) }}</td><td>{{ d.week.images }}</td><td class="cost">{{ fmtCost(d.week.cost) }}</td></tr>
          <tr><td>累计</td><td>{{ d.total.requests }}</td><td>{{ d.total.success }}</td><td>{{ fmtTokens(d.total.tokens) }}</td><td>{{ d.total.images }}</td><td class="cost">{{ fmtCost(d.total.cost) }}</td></tr>
        </tbody></table></div>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">可用模型</div></div>
        <div class="chips"><span class="chip" v-for="m in d.models" :key="m">{{ m }}</span></div>
        <div v-if="!d.models.length" class="empty">暂无可用模型</div>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">最近调用（最多 100 条）</div></div>
        <div class="table-wrap"><table><thead><tr>
          <th>时间</th><th>模型</th><th>模态</th><th>输入</th><th>输出</th><th>图像</th><th>成本</th><th>耗时</th><th>状态</th><th>错误</th>
        </tr></thead><tbody>
          <tr v-if="!d.logs.length"><td colspan="10" class="empty">暂无调用记录</td></tr>
          <tr v-for="l in d.logs" :key="l.id">
            <td>{{ fmtTime(l.ts) }}</td>
            <td><span class="mono">{{ l.requested_model || l.model }}</span> <span v-if="l.requested_model && l.requested_model !== l.model" class="tag tag-orange" title="fallback">↓</span></td>
            <td>{{ l.modality === 'image' ? '图像' : '文本' }}</td>
            <td>{{ l.prompt_tokens }}</td><td>{{ l.completion_tokens }}</td>
            <td>{{ l.image_count || '—' }}</td>
            <td class="cost">{{ fmtCost(l.cost) }}</td>
            <td>{{ l.latency_ms }}ms</td>
            <td><span :class="l.status === 'ok' ? 'tag tag-green' : 'tag tag-red'">{{ l.status === 'ok' ? 'OK' : 'ERR' }}</span></td>
            <td style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--color-text-3)" :title="l.error">{{ l.error }}</td>
          </tr>
        </tbody></table></div>
      </div>
    </div>
  </div>`,
};

// ── 根组件 ──
const App = {
  components: { LoginPage, AdminShell, PortalPage },
  // toasts/prefs：模块级 reactive（toast() 写入、设置页读写），挂进 data 供模板渲染。
  data() { return { mode: "loading", toasts, prefs }; },
  computed: {
    // 背景层样式：模糊百分比换算 px（40% ≈ 12px），inset 负边距消除模糊边缘发虚。
    bgStyle() {
      return {
        backgroundImage: "url(\"" + this.prefs.bgUrl + "\")",
        filter: "blur(" + Math.round(this.prefs.bgBlur * 0.3) + "px)",
      };
    },
  },
  methods: {
    onAdmin(tok) {
      state.adminToken = tok;
      localStorage.setItem("arkgate_token", tok);
      this.mode = "admin";
    },
    onPortal(sk) {
      state.subKey = sk;
      localStorage.setItem("arkgate_sk", sk);
      this.mode = "portal";
    },
    logoutAdmin() { state.adminToken = ""; localStorage.removeItem("arkgate_token"); this.mode = "login"; },
    logoutPortal() { state.subKey = ""; localStorage.removeItem("arkgate_sk"); this.mode = "login"; },
  },
  mounted() {
    applyPrefs();
    // 401 统一兜底：门户会话与管理会话分别退回登录页。
    window.__arkgateOn401 = (path) => {
      if (path && path.indexOf("/api/portal/") === 0) {
        if (state.subKey) this.logoutPortal();
      } else if (state.adminToken) {
        this.logoutAdmin();
      }
    };
    // 已有门户会话优先恢复（子 Key 用户通常不持有管理令牌）。
    const bootPortal = state.subKey
      ? req("POST", "/api/portal/overview", null, { key: state.subKey }).then(() => true).catch(() => false)
      : Promise.resolve(false);
    bootPortal.then((ok) => {
      if (ok) { this.mode = "portal"; return; }
      localStorage.removeItem("arkgate_sk");
      if (state.adminToken) {
        req("GET", "/api/auth/status", null, { key: state.adminToken })
          .then((d) => {
            if (!d.initialized) { this.logoutAdmin(); this.mode = "login"; return; }
            this.mode = "admin";
          })
          .catch(() => { this.mode = "login"; });
        return;
      }
      this.mode = "login";
    });
  },
  template: `
    <div v-if="prefs.bgUrl" class="app-bg" :style="bgStyle"></div>
    <div v-if="prefs.bgUrl" class="app-bg-shade"></div>
    <LoginPage v-if="mode==='login'" @admin="onAdmin" @portal="onPortal"/>
    <AdminShell v-else-if="mode==='admin'" @logout="logoutAdmin"/>
    <PortalPage v-else-if="mode==='portal'" @logout="logoutPortal"/>
    <div v-else class="login-wrap"><div class="login-card sub">加载中…</div></div>
    <div class="toasts"><div v-for="t in toasts" :key="t.id" class="toast" :class="t.ok ? '' : 'err'">{{ t.msg }}</div></div>`,
};

const app = createApp(App);
// 模板表达式只能访问组件实例与全局属性：把工具函数/常量挂到 globalProperties，
// 模板里的 {{ fmtTokens(..) }}、@click="toggleDark"、v-for="o in capOptions" 才可见。
app.config.globalProperties.fmtTokens = fmtTokens;
app.config.globalProperties.fmtTime = fmtTime;
app.config.globalProperties.fmtCost = fmtCost;
app.config.globalProperties.fmtPct = fmtPct;
app.config.globalProperties.toggleDark = toggleDark;
app.config.globalProperties.capOptions = capOptions;
app.component("OverviewPage", OverviewPage)
  .component("UsagePage", UsagePage)
  .component("AccountsPage", AccountsPage)
  .component("ModelsPage", ModelsPage)
  .component("SubKeysPage", SubKeysPage)
  .component("LogsPage", LogsPage)
  .component("SettingsPage", SettingsPage)
  .mount("#app");
