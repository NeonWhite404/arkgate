/* ArkGate 前端 —— Vue 3（vendor 全局构建，免 Node 构建）
 *
 * 文件：ui.js（UiDrawer 抽屉 / UiSwitch 开关）→ models.js（模型映射工作台）→ app.js
 * 结构：LoginPage（管理端 / 子 Key 门户双模式登录）
 *      ├─ AdminShell（侧边栏 + 管理页：总览/用量分析/账号/模型映射/分流配置/子Key/日志/设置）
 *      └─ PortalPage（子 Key 自助门户：限额进度、用量、成功率、脱敏调用记录）
 * 交互模式参照 CLIProxyAPI Management Center：复杂实体用「工作台 + 侧滑抽屉」
 * 分段编辑，替代窄弹窗；简单状态（启停）在表格行内直接切换。
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
// ── 计价币种（展示层）──
// 存储里的单价与成本**始终是美元**（与 LiteLLM 目录一致），这里只负责换算展示。
// 汇率由管理端设置页填写，启动时由 /api/settings/runtime 拉取。
// rate = 0 表示不换算（按美元展示）。
const currency = reactive({ rate: 0, symbol: "$", code: "USD" });

// applyCurrency 把服务端返回的币种设置写入本地展示状态。
function applyCurrency(s) {
  if (!s) return;
  currency.rate = Number(s.currency_rate || 0);
  currency.symbol = s.currency_symbol || "$";
  currency.code = s.currency_code || "USD";
}
// currencySuffix 在符号不足以区分币种时补上代码，避免看到 “$” 却以为是美元。
function currencySuffix() {
  if (currency.rate <= 0) return "";
  return currency.symbol === "$" ? " " + currency.code : "";
}
// toDisplay 把存储的美元金额换算为展示币种金额。
function toDisplay(usd) {
  const v = Number(usd || 0);
  return currency.rate > 0 ? v * currency.rate : v;
}
function fmtCost(c) {
  const v = toDisplay(c);
  const sym = currency.symbol;
  const suf = currencySuffix();
  if (v === 0) return sym + "0";
  if (Math.abs(v) >= 1) return sym + v.toFixed(2) + suf;
  return sym + v.toFixed(4) + suf;
}
// fmtUnitPrice 单价专用：单价数量级小（$/1M），固定 4 位避免 0.30 与 0.3000 跳动。
function fmtUnitPrice(c) {
  const v = toDisplay(c);
  return currency.symbol + v.toFixed(4) + currencySuffix();
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

// fmtMs 毫秒展示：小于 1 秒显示整数毫秒，否则显示秒（一位小数）。
// 统计里的首字/总耗时跨越了「几十毫秒」到「几分钟」两个量级，统一带单位更好读。
function fmtMs(ms) {
  const v = Number(ms || 0);
  if (!v) return "—";
  return v < 1000 ? Math.round(v) + "ms" : (v / 1000).toFixed(1) + "s";
}

// fmtAvg 求平均并带单位；无样本时返回「—」而不是 0。
// **不能返回 0**：0ms 会被读成「极快」，而真相是「没有样本」。
function fmtAvg(sum, n, unit) {
  const c = Number(n || 0);
  if (!c) return "—";
  return fmtMs(sum / c);
}

// fmtPctRaw 已是百分数的值（成功率分桶用）；0 样本返回「—」不伪造 0%。
function fmtPctRaw(p, ok, n) {
  if (!n) return "—";
  return Number(p || 0).toFixed(1) + "%";
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

// ── 模型可用性快速测试 ──
// payload 既可为 {model}（按已注册模型解析路由），也可为
// {account_id, ep, protocol}（直接探测一个上游接入点）。
const ProbeTest = {
  props: {
    payload: { type: Object, required: true },
    label: { type: String, default: "测试" },
    disabled: Boolean,
    compact: Boolean,
    btnClass: { type: String, default: "btn btn-outline btn-sm" },
  },
  data() { return { pending: false, result: null }; },
  methods: {
    run() {
      if (this.pending || this.disabled) return;
      this.pending = true;
      this.result = null;
      req("POST", "/api/test/model", this.payload)
        .then((d) => {
          this.result = d;
          if (!d.ok) toast("测试失败：" + (d.error || "未知错误"), false);
        })
        .catch((e) => {
          this.result = { ok: false, error: e.message };
          toast(e.message, false);
        })
        .finally(() => { this.pending = false; });
    },
    resultTitle() {
      const r = this.result;
      if (!r) return "";
      if (!r.ok) return r.error || "测试失败";
      const parts = ["上游可达", (Number(r.latency_ms) || 0) + "ms"];
      if (r.account_name && r.ep) parts.push(r.account_name + " · " + r.ep);
      if (r.resolved) parts.push("路由解析为 " + r.resolved);
      return parts.join(" · ");
    },
    resultText() {
      if (!this.result) return "";
      if (this.compact) return this.result.ok ? "✓" : "✗";
      return this.result.ok ? "✓ " + (Number(this.result.latency_ms) || 0) + "ms" : "✗ " + (this.result.error || "失败");
    },
  },
  template: `
    <span class="probe-test" @click.stop>
      <button :class="btnClass" :disabled="pending || disabled" @click="run">{{ pending ? '测试中…' : label }}</button>
      <span v-if="result" class="probe-result" :class="result.ok ? 'ok' : 'err'" :title="resultTitle()">{{ resultText() }}</span>
    </span>`,
};

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
    // 模型 / 子 Key 分布（tokens 聚合；着色在饼图渲染时按名称哈希固定）。
    modelDist() {
      const byModel = {};
      for (const p of this.series) {
        const m = p.model || "—";
        byModel[m] = (byModel[m] || 0) + (p.tokens || 0);
      }
      return Object.entries(byModel).map(([label, value]) => ({ label, value }));
    },
    subkeyDist() {
      const bySk = {};
      for (const p of this.series) {
        const k = p.subkey || p.subkey_id || "—";
        bySk[k] = (bySk[k] || 0) + (p.tokens || 0);
      }
      return Object.entries(bySk).map(([label, value]) => ({ label, value }));
    },
    modelPieHtml() { return renderPieChart(pieSlices(this.modelDist)); },
    subkeyPieHtml() { return renderPieChart(pieSlices(this.subkeyDist)); },
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
      <div class="stat-card"><div class="ic ic-orange">◎</div><div class="body"><div class="v">{{ o.endpoint_halfopen || 0 }}</div><div class="l">探测中（待验证）</div></div></div>
      <div class="stat-card"><div class="ic ic-purple">🧩</div><div class="body"><div class="v">{{ o.model_count }}</div><div class="l">模型</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">🔑</div><div class="body"><div class="v">{{ o.subkey_count }}</div><div class="l">子 Key</div></div></div>
      <div class="stat-card"><div class="ic ic-blue">⚡</div><div class="body"><div class="v">{{ o.total_requests }}</div><div class="l">总请求</div></div></div>
      <div class="stat-card" title="上游报告的总 token（prompt+completion，含缓存命中）。是计费与展示口径，不是子 Key 额度消耗——额度按缓存倍率加权计量"><div class="ic ic-green">⬤</div><div class="body"><div class="v">{{ fmtTokens(o.total_tokens) }}</div><div class="l">总 Token<span class="stat-sub">上游原始</span></div></div></div>
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
        <div v-html="modelPieHtml"></div></div>
      <div class="card"><div class="card-head"><div class="card-title">子 Key 分布（24h Tokens）</div></div>
        <div v-html="subkeyPieHtml"></div></div>
    </div>
  </div>`,
  methods: {
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

// ── 总览分布饼图（SVG 原生 title + 图例；无第三方依赖） ──
const PIE_COLORS = ["#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ef4444", "#14b8a6", "#ec4899", "#6366f1", "#f97316", "#06b6d4"];
const PIE_OTHER_COLOR = "#9ca3af";
const PIE_MAX_SLICES = 8;
const PIE_SIZE = 168;

// pieSlices 把分布整理成可渲染扇区：过滤非正值、按 Token 降序、超上限合并尾部为
// 「其他」。颜色按分类名称哈希取固定调色板（与数组顺序无关，刷新不跳变），
// 「其他」用固定中性色。空数据返回 []（渲染层画空态）。
function pieSlices(dist) {
  const items = (dist || [])
    .map((d) => ({ label: String(d.label ?? "—"), value: Number(d.value) || 0 }))
    .filter((d) => d.value > 0)
    .sort((a, b) => b.value - a.value);
  if (!items.length) return [];
  const total = items.reduce((s, d) => s + d.value, 0);
  const head = items.slice(0, PIE_MAX_SLICES);
  const rest = items.slice(PIE_MAX_SLICES);
  const slices = head.map((d) => ({ ...d, color: PIE_COLORS[hashIdx(d.label, PIE_COLORS.length)], pct: (d.value / total) * 100 }));
  if (rest.length) {
    slices.push({
      label: "其他（" + rest.length + " 项）",
      value: rest.reduce((s, d) => s + d.value, 0),
      color: PIE_OTHER_COLOR, pct: 0, isOther: true,
    });
    const sum = slices.reduce((s, d) => s + d.value, 0);
    slices.forEach((d) => { d.pct = (d.value / sum) * 100; });
  }
  return slices;
}

// hashIdx 名称哈希 → 稳定调色板下标（djb2，分布均匀、与顺序无关）。
function hashIdx(name, n) {
  let h = 5381;
  for (let i = 0; i < name.length; i++) {
    h = ((h << 5) + h + name.charCodeAt(i)) | 0;
  }
  return Math.abs(h) % n;
}

// pieSlicePath 单扇区 SVG path：单扇区（360°）SVG arc 会退化成零弧，
// 直接画整圆；其余从 12 点方向顺时针累计。
function pieSlicePath(cx, cy, r, startDeg, endDeg) {
  if (endDeg - startDeg >= 359.999) {
    return "M " + cx + " " + (cy - r) + " A " + r + " " + r + " 0 1 1 " + (cx - 0.001) + " " + (cy - r) + " Z";
  }
  const p0 = polar(cx, cy, r, startDeg);
  const p1 = polar(cx, cy, r, endDeg);
  const large = endDeg - startDeg > 180 ? 1 : 0;
  return "M " + p0.x + " " + p0.y + " A " + r + " " + r + " 0 " + large + " 1 " + p1.x + " " + p1.y + " L " + cx + " " + cy + " Z";
}

function polar(cx, cy, r, deg) {
  const rad = ((deg - 90) * Math.PI) / 180;
  return { x: (cx + r * Math.cos(rad)).toFixed(2), y: (cy + r * Math.sin(rad)).toFixed(2) };
}

// escHtml 属性/标签内文本转义（分类名与 tooltip 进入 innerHTML，防注入）。
function escHtml(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// renderPieChart 渲染饼图 + 图例：每个扇区带原生 <title>（分类名、格式化
// Token 数与占比），图例行显示色块、截断名称、Token 数与占比，完整名称保留
// 在 title 属性。空数据返回空态，单分类输出完整圆。
function renderPieChart(slices) {
  if (!slices || !slices.length) return '<div class="empty">暂无数据</div>';
  const cx = PIE_SIZE / 2, cy = PIE_SIZE / 2, r = PIE_SIZE / 2 - 12;
  let svg = '<svg class="pie-svg" width="' + PIE_SIZE + '" height="' + PIE_SIZE + '" viewBox="0 0 ' + PIE_SIZE + " " + PIE_SIZE + '" role="img">';
  let acc = 0;
  slices.forEach((s) => {
    const end = acc + s.pct * 3.6;
    svg += '<path d="' + pieSlicePath(cx, cy, r, acc, end) + '" fill="' + s.color + '">' +
      "<title>" + escHtml(s.label) + "：" + fmtTokens(s.value) + "（" + s.pct.toFixed(1) + "%）</title></path>";
    acc = end;
  });
  svg += "</svg>";
  const total = slices.reduce((sum, s) => sum + s.value, 0);
  const leg = '<ul class="pie-legend">' + slices.map((s) => {
    const cls = s.isOther ? " leg-other" : "";
    return '<li class="pie-leg-row' + cls + '">' +
      '<span class="leg-swatch" style="background:' + s.color + '"></span>' +
      '<span class="pie-leg-label" title="' + escHtml(s.label) + '">' + escHtml(s.label) + "</span>" +
      '<span class="pie-leg-val">' + fmtTokens(s.value) + "</span>" +
      '<span class="pie-leg-pct">' + (s.value > 0 ? ((s.value / total) * 100).toFixed(1) : "0.0") + "%</span>" +
      "</li>";
  }).join("") + "</ul>";
  return '<div class="pie-wrap">' + svg + leg + "</div>";
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
    // 费用按「输入/缓存/输出」堆叠：单色柱看不出钱花在哪，而分项才是定价排查的依据。
    // 注意：三者之和等于成本总额（后台不做 clamp，浮点除法保证不丢分），
    // 所以堆叠总高与旧的单系列柱一致，老读数习惯不被破坏。
    return renderStackedBarChart(buckets,
      (b) => (b.cost || 0) - (b.output_cost || 0),
      (b) => b.output_cost || 0,
      "#f59e0b", "#8b5cf6", "输入+缓存", "输出", fmtCost, labelFn);
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
      // 删除账号会连带清理子 Key 白名单里的该账号（后端同一事务内完成）。
      if (!confirm("确认删除该账号？将同时删除其模型映射。\n子 Key 白名单中的该账号会被移除。")) return;
      req("DELETE", "/api/accounts/" + a.id).then((d) => {
        const off = (d && d.disabled_subkeys) || [];
        if (off.length) {
          // 白名单被清空 = 会变成「不限」，后端 fail-closed 把 Key 停用了。
          toast("已删除；白名单仅含该账号的 " + off.length + " 个子 Key 已自动停用：" + off.join("、"), false);
        } else {
          toast("已删除");
        }
        this.load();
      }).catch((e) => toast(e.message, false));
    },
  },
  template: `
  <div class="page">
    <div class="page-title">上游账号</div>
    <div class="toolbar"><button class="btn btn-primary" @click="openModal(null)">+ 添加账号</button><div class="spacer"></div></div>
    <div class="card"><div class="table-wrap"><table><thead><tr>
      <th>名称</th><th>供应商</th><th>Key</th><th>状态</th><th>权重</th><th>请求/成功/失败</th><th title="上游报告的总 token（prompt+completion，含缓存命中）。账号层不做限流，此列只作容量参考">Token<span class="th-sub">累加·上游原始</span></th><th>图像</th><th>操作</th>
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

    <ui-drawer :open="!!modal" :width="560" :title="modal && modal.id ? '编辑账号' : '添加账号'"
      :subtitle="modal && modal.id ? modal.form.name : '接入点级限流在「模型映射」工作台按映射配置'" @close="modal=null">
      <template v-if="modal">
        <div class="sec">
          <div class="sec-title">基础信息</div>
          <div class="form-row">
            <div class="form-item"><label>名称 <span class="req">*</span></label><input v-model="modal.form.name" placeholder="例如：主账号-北京"/></div>
            <div class="form-item"><label>权重（其下映射 weight=0 时回落）</label><input v-model.number="modal.form.weight" type="number"/></div>
          </div>
          <div class="form-row">
            <div class="form-item"><label>供应商</label>
              <select v-model="modal.form.provider">
                <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.display_name }}（{{ p.id }}）</option>
              </select></div>
            <div class="form-item"><label>状态</label>
              <select v-model="modal.form.status"><option value="active">启用</option><option value="disabled">禁用</option></select></div>
          </div>
        </div>
        <div class="sec">
          <div class="sec-title">连接</div>
          <div class="form-item"><label>Base URL</label>
            <input v-model="modal.form.base_url" :placeholder="providerDef() && providerDef().default_base_url ? '留空使用默认：' + providerDef().default_base_url : '必填：该供应商无默认地址（http(s)://…）'"/></div>
          <div class="form-item"><label>上游 API Key <span class="req">*</span>{{ modal.id ? '（留空表示不修改）' : '' }}</label>
            <input v-model="modal.form.api_key" placeholder="任意字符串，网关不做格式假设"/></div>
        </div>
        <div class="sec">
          <div class="sec-title">能力覆盖（三态）</div>
          <div class="form-row">
            <div class="form-item"><label>Responses 能力</label>
              <select v-model="modal.form.cap_responses"><option v-for="o in capOptions" :key="o.v" :value="o.v">{{ o.label }}</option></select></div>
            <div class="form-item"><label>图像能力</label>
              <select v-model="modal.form.cap_images"><option v-for="o in capOptions" :key="o.v" :value="o.v">{{ o.label }}</option></select></div>
          </div>
          <div class="form-tip">用于纠正自定义供应商的能力声明；并发 / RPM / TPM 限额请到「模型映射」按接入点配置。</div>
        </div>
      </template>
      <template #foot>
        <button class="btn btn-outline" @click="modal=null">取消</button>
        <button class="btn btn-primary" @click="save">保存</button>
      </template>
    </ui-drawer>
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
    // quotaPct 计算额度进度百分比（上限 100 以免进度条溢出）。
    quotaPct(q, kind) {
      if (!q) return 0;
      if (kind === "tokens") {
        if (!q.limit_tokens) return 0;
        return Math.min(100, Math.round((q.used_tokens / q.limit_tokens) * 100));
      }
      if (!q.limit_requests) return 0;
      return Math.min(100, Math.round((q.requests / q.limit_requests) * 100));
    },
    // quotaCls 返回进度条/数字的配色：≥90% 危险，≥70% 警告。
    //
    // 阈值与门户页保持一致：同一个 Key 在管理端和用户端看到的颜色必须同义，
    // 否则「管理员看到绿的、用户看到红的」会被当成两边数据不一致。
    quotaCls(q, kind) {
      const p = this.quotaPct(q, kind);
      return p >= 90 ? "danger" : p >= 70 ? "warn" : "";
    },
    // periodShort 周期简称 + 窗口起始日，让管理员知道进度何时归零。
    periodShort(q) {
      if (!q) return "";
      const label = q.period === "week" ? "本周" : q.period === "month" ? "本月" : "今日";
      return label + "（自 " + q.window_day + " 起）";
    },

    load() {
      Promise.all([req("GET", "/api/subkeys"), req("GET", "/api/models"), req("GET", "/api/accounts")])
        .then((rs) => { this.subs = rs[0] || []; this.models = rs[1] || []; this.accounts = rs[2] || []; })
        .catch((e) => toast(e.message, false));
    },
    openModal(id) {
      this.modal = {
        id,
        form: { name: "", key: "", allowed_models: [], allowed_accounts: [],
          daily_limit_tokens: 0, daily_limit_images: 0, window_limit_requests: 0,
          quota_period: "day", quota_reset_weekday: 1, quota_reset_hour: 0,
          cache_read_permille: 0, cache_write_permille: 0 },
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
            this.modal.form.window_limit_requests = s.window_limit_requests || 0;
            // 空周期在后端等价 day，这里显式归一化，避免下拉框显示空白。
            this.modal.form.quota_period = s.quota_period || "day";
            // 0 = 未设置（后端按周一解析），这里也归一化，否则下拉框选不中。
            this.modal.form.quota_reset_weekday = s.quota_reset_weekday || 1;
            this.modal.form.quota_reset_hour = s.quota_reset_hour || 0;
            // 倍率 0 = 未设置（后端用默认）；表单里也显示 0，让用户看得见「未设置」。
            this.modal.form.cache_read_permille = s.cache_read_permille || 0;
            this.modal.form.cache_write_permille = s.cache_write_permille || 0;
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
        window_limit_requests: Number(f.window_limit_requests) || 0,
        quota_period: f.quota_period || "day",
        quota_reset_weekday: Number(f.quota_reset_weekday) || 0,
        quota_reset_hour: Number(f.quota_reset_hour) || 0,
        cache_read_permille: Number(f.cache_read_permille) || 0,
        cache_write_permille: Number(f.cache_write_permille) || 0,
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
      <th>名称</th><th>Key</th><th>状态</th><th>可访问模型</th><th>请求</th><th>Token<span class="th-sub">累计·上游原始</span></th><th>本周期配额<span class="th-sub">加权，与限额同口径</span></th><th>图像</th><th>操作</th>
    </tr></thead><tbody>
      <tr v-if="!subs.length"><td colspan="9" class="empty">暂无子 Key</td></tr>
      <tr v-for="s in subs" :key="s.id">
        <td><strong>{{ s.name || s.id }}</strong></td>
        <td class="mono"><span class="code-copy" title="点击复制" @click="copy(s.key)">{{ s.key }}</span></td>
        <td><span :class="s.enabled ? 'tag tag-green' : 'tag tag-gray'">{{ s.enabled ? '启用' : '禁用' }}</span></td>
        <td>{{ (s.allowed_models && s.allowed_models.length) ? s.allowed_models.join(', ') : '全部' }}</td>
        <td>{{ s.total_requests }}</td><td>{{ fmtTokens(s.total_tokens) }}</td>
        <td class="quota-cell">
          <template v-if="s.quota && (s.quota.limit_tokens || s.quota.limit_requests)">
            <div class="quota-line" v-if="s.quota.limit_tokens">
              <span class="mono" :class="quotaCls(s.quota, 'tokens')">{{ fmtTokens(s.quota.used_tokens) }}</span>
              <span class="muted"> / {{ fmtTokens(s.quota.limit_tokens) }}</span>
              <span class="pct" :class="quotaCls(s.quota, 'tokens')">{{ quotaPct(s.quota, 'tokens') }}%</span>
            </div>
            <div class="progress progress-sm" v-if="s.quota.limit_tokens">
              <div class="bar" :class="quotaCls(s.quota, 'tokens')" :style="{width: quotaPct(s.quota, 'tokens') + '%'}"></div>
            </div>
            <div class="quota-line" v-if="s.quota.limit_requests">
              <span class="mono" :class="quotaCls(s.quota, 'requests')">{{ s.quota.requests }}</span>
              <span class="muted"> / {{ s.quota.limit_requests }} 次</span>
              <span class="pct" :class="quotaCls(s.quota, 'requests')">{{ quotaPct(s.quota, 'requests') }}%</span>
            </div>
            <div class="quota-meta">
              {{ periodShort(s.quota) }}
              <template v-if="s.quota.weighted"> · 缓存加权 读×{{ (s.quota.cache_read_permille || 1000) / 1000 }} 写×{{ (s.quota.cache_write_permille || 1000) / 1000 }}</template>
              <template v-else> · 未加权（= 上游原始）</template>
            </div>
          </template>
          <span v-else class="muted">不限</span>
        </td>
        <td>{{ s.total_images || 0 }}</td>
        <td><div class="row-actions">
          <button class="btn btn-outline btn-sm" @click="openModal(s.id)">编辑</button>
          <button class="btn btn-danger btn-sm" @click="del(s)">删除</button>
        </div></td>
      </tr>
    </tbody></table></div></div>

    <ui-drawer :open="!!modal" :width="560" :title="modal && modal.id ? '编辑子 Key' : '新建子 Key'" @close="modal=null">
      <template v-if="modal">
        <div class="sec">
          <div class="sec-title">基础信息</div>
          <div class="form-item"><label>名称</label><input v-model="modal.form.name" placeholder="例如：给团队的 Key"/></div>
          <div class="form-item" v-if="!modal.id"><label>自定义 Key（留空自动生成 sk-xxx）</label><input v-model="modal.form.key" placeholder="sk-..."/></div>
        </div>
        <div class="sec">
          <div class="sec-title">访问白名单</div>
          <div class="form-item"><label>可访问模型（不勾选 = 全部）</label>
            <CheckGroup :options="models.map(m => ({v: m.name, l: m.name}))" v-model="modal.form.allowed_models"/></div>
          <div class="form-item"><label>可访问账号（不勾选 = 全部）</label>
            <CheckGroup :options="accounts.map(a => ({v: a.id, l: a.name}))" v-model="modal.form.allowed_accounts"/></div>
        </div>
        <div class="sec">
          <div class="sec-title">限额周期</div>
          <div class="form-row">
            <div class="form-item">
              <label>周期</label>
              <select v-model="modal.form.quota_period">
                <option value="day">每自然日</option>
                <option value="week">每周</option>
                <option value="month">每月</option>
              </select>
            </div>
            <div class="form-item" v-if="modal.form.quota_period === 'week'">
              <label>每周从周几开始</label>
              <select v-model.number="modal.form.quota_reset_weekday">
                <option :value="1">周一</option><option :value="2">周二</option>
                <option :value="3">周三</option><option :value="4">周四</option>
                <option :value="5">周五</option><option :value="6">周六</option>
                <option :value="7">周日</option>
              </select>
            </div>
            <div class="form-item">
              <label>每日重置时刻（本地时区，小时）</label>
              <input v-model.number="modal.form.quota_reset_hour" type="number" min="0" max="23"/>
            </div>
          </div>
          <p class="hint">
            周期起点以自然日为单位，不设滚动窗口。重置时刻用于对齐时区：
            例如填 8 表示每天早上 8 点开始新周期（8 点前的用量计入前一天）。
          </p>
        </div>

        <div class="sec">
          <div class="sec-title">周期限额（0 = 不限）</div>
          <div class="form-row">
            <div class="form-item"><label>Token 限额</label><input v-model.number="modal.form.daily_limit_tokens" type="number"/></div>
            <div class="form-item"><label>请求数限额</label><input v-model.number="modal.form.window_limit_requests" type="number"/></div>
            <div class="form-item"><label>图像张数限额</label><input v-model.number="modal.form.daily_limit_images" type="number"/></div>
          </div>
          <p class="hint">
            Token 限额按「加权配额」计量：缓存读取与缓存写入各自乘下面的倍率。
          </p>
        </div>

        <div class="sec">
          <div class="sec-title">缓存计权倍率（千分比，留空 = 不启用）</div>
          <div class="form-row">
            <div class="form-item">
              <label>缓存读取倍率（默认 100 = 0.1x）</label>
              <input v-model.number="modal.form.cache_read_permille" type="number" min="0"/>
            </div>
            <div class="form-item">
              <label>缓存写入倍率（默认 1250 = 1.25x）</label>
              <input v-model.number="modal.form.cache_write_permille" type="number" min="0"/>
            </div>
          </div>
          <p class="hint">
            上游上报的 prompt_tokens 含缓存命中量。缓存读取单价通常只有输入的 10%
            （OpenAI 系为 50%），按原价计入限额会让「缓存用得多」反而更快撞额度。
          </p>
          <p class="hint">
            <strong>两个都留空 = 不启用加权</strong>（Token 上限仍按上游总数计，
            与升级前一致）。启用后只填一个也可以，另一个按默认值
            （读 0.1x / 写 1.25x）计算，不会当成免费。
          </p>
        </div>
      </template>
      <template #foot>
        <button class="btn btn-outline" @click="modal=null">取消</button>
        <button class="btn btn-primary" @click="save">保存</button>
      </template>
    </ui-drawer>
  </div>`,
};

// ── 请求日志 ──
const LogsPage = {
  data() {
    return {
      logs: [], total: 0, page: 1,
      pageSize: Number(localStorage.getItem("arkgate_log_page_size") || 50),
      sizes: [20, 50, 100, 200, 500],
      filters: { ip: "", model: "", subkey: "", account: "", status: "", error_kind: "" },
      errorKinds: [
        { v: "", label: "全部原因" },
        { v: "upstream_error", label: "上游报错" },
        { v: "upstream_timeout", label: "上游超时" },
        { v: "client_invalid", label: "请求非法（客户端侧）" },
        { v: "client_cancel", label: "下游断开（客户端侧）" },
        { v: "local_error", label: "网关内部错误" },
        { v: "unclassified", label: "未分类（历史数据）" },
      ],
      subkeys: [], accounts: [], models: [], loading: false,
    };
  },
  mounted() {
    Promise.all([req("GET", "/api/subkeys"), req("GET", "/api/accounts"), req("GET", "/api/models")])
      .then((rs) => { this.subkeys = rs[0] || []; this.accounts = rs[1] || []; this.models = rs[2] || []; })
      .catch(() => {});
    this.load();
  },
  computed: {
    pages() { return Math.max(1, Math.ceil(this.total / this.pageSize)); },
    rangeFrom() { return this.total ? (this.page - 1) * this.pageSize + 1 : 0; },
    rangeTo() { return Math.min(this.page * this.pageSize, this.total); },
  },
  methods: {
    load() {
      this.loading = true;
      const offset = (this.page - 1) * this.pageSize;
      const qs = new URLSearchParams({ limit: String(this.pageSize), offset: String(offset) });
      Object.entries(this.filters).forEach(([k, v]) => { if (String(v || "").trim()) qs.set(k, String(v).trim()); });
      req("GET", "/api/logs?" + qs.toString())
        .then((d) => {
          this.logs = (d && d.items) || [];
          this.total = (d && d.total) || 0;
          // 末页被清空/缩短时回退到最后一页，避免停在空白页。
          if (!this.logs.length && this.page > 1 && this.total > 0) {
            this.page = this.pages;
            this.load();
          }
        })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.loading = false; });
    },
    go(p) {
      const next = Math.min(Math.max(1, p), this.pages);
      if (next === this.page) return;
      this.page = next;
      this.load();
    },
    setSize(n) {
      this.pageSize = Number(n) || 50;
      localStorage.setItem("arkgate_log_page_size", String(this.pageSize));
      this.page = 1;
      this.load();
    },
    applyFilters() { this.page = 1; this.load(); },
    kindLabel(k) { return ERROR_KIND_LABELS[k] || "—"; },
    kindColor(k) { return ERROR_KIND_COLORS[k] || "#9ca3af"; },
    resetFilters() {
      this.filters = { ip: "", model: "", subkey: "", account: "", status: "", error_kind: "" };
      this.page = 1;
      this.load();
    },
    clear() {
      if (!confirm("确认清空所有日志？")) return;
      req("DELETE", "/api/logs").then(() => {
        toast("已清空");
        this.page = 1;
        this.load();
      });
    },
  },
  template: `
  <div class="page">
    <div class="page-title">请求日志</div>
    <div class="toolbar">
      <button class="btn btn-outline" :disabled="loading" @click="load">{{ loading ? '加载中…' : '刷新' }}</button>
      <button class="btn btn-danger" @click="clear">清空日志</button>
      <div class="spacer"></div>
      <span class="sh-note">每页</span>
      <select style="width:90px" :value="pageSize" @change="setSize($event.target.value)">
        <option v-for="n in sizes" :key="n" :value="n">{{ n }} 条</option>
      </select>
    </div>
    <div class="toolbar log-filters">
      <input v-model="filters.ip" placeholder="来源 IP" style="width:150px" @keyup.enter="applyFilters"/>
      <input v-model="filters.model" list="log-model-list" placeholder="模型（请求/实际）" style="width:190px" @keyup.enter="applyFilters"/>
      <datalist id="log-model-list"><option v-for="m in models" :key="m.name" :value="m.name"/></datalist>
      <select v-model="filters.subkey" style="width:170px">
        <option value="">全部子 Key</option>
        <option v-for="s in subkeys" :key="s.id" :value="s.id">{{ s.name || s.id }}</option>
      </select>
      <select v-model="filters.account" style="width:170px">
        <option value="">全部账号</option>
        <option v-for="a in accounts" :key="a.id" :value="a.id">{{ a.name }}</option>
      </select>
      <select v-model="filters.status" style="width:110px">
        <option value="">全部状态</option><option value="ok">成功</option><option value="error">失败</option>
      </select>
      <select v-model="filters.error_kind" style="width:190px" title="按失败原因筛选；选「上游报错/超时」可排除客户端侧失败">
        <option v-for="k in errorKinds" :key="k.v" :value="k.v">{{ k.label }}</option>
      </select>
      <button class="btn btn-primary btn-sm" @click="applyFilters">筛选</button>
      <button class="btn btn-outline btn-sm" @click="resetFilters">重置</button>
    </div>
    <div class="card"><div class="table-wrap"><table><thead><tr>
      <th>时间</th><th>来源 IP</th><th>子 Key</th><th>账号</th><th>供应商</th><th>请求模型</th><th>真实模型</th><th title="上游报告的 prompt_tokens，**含缓存命中量**；不是「非缓存输入」。非缓存部分 = 输入 − 缓存读取 − 缓存写入">输入<span class="th-sub">含缓存命中</span></th><th>输出</th><th title="prompt_tokens + completion_tokens，上游原始量">总 Token<span class="th-sub">= 输入 + 输出</span></th><th>图像</th><th>成本</th><th>首字 / 总耗时</th><th>状态</th><th>错误</th>
    </tr></thead><tbody>
      <tr v-if="!logs.length"><td colspan="15" class="empty">暂无日志</td></tr>
      <tr v-for="l in logs" :key="l.id" class="row-in">
        <td>{{ fmtTime(l.ts) }}</td>
        <td class="mono">{{ l.client_ip || '—' }}</td>
        <td>{{ l.subkey_name || l.subkey_id }}</td>
        <td>{{ l.account_name || l.account_id }}</td>
        <td>{{ l.provider || '-' }}</td>
        <td><span class="mono">{{ l.requested_model || l.model }}</span> <span v-if="l.requested_model && l.requested_model !== l.model" class="tag tag-orange" title="实际模型与请求模型不同（fallback 或虚拟路由）">↓</span></td>
        <td class="mono">{{ l.model }}</td>
        <td>{{ l.prompt_tokens }}</td><td>{{ l.completion_tokens }}</td><td>{{ l.total_tokens }}</td>
        <td>{{ l.modality === 'image' ? (l.image_count || 0) + ' 张' : '—' }}</td>
        <td class="cost">{{ fmtCost(l.cost) }}</td>
        <td class="mono">{{ l.first_token_ms ? l.first_token_ms + 'ms' : '—' }} / {{ l.latency_ms }}ms</td>
        <td>
          <span :class="l.status === 'ok' ? 'tag tag-green' : 'tag tag-red'">{{ l.status === 'ok' ? 'OK' : 'ERR' }}</span>
          <span v-if="l.status !== 'ok' && l.error_kind" class="tag tag-gray" :title="'失败原因：' + kindLabel(l.error_kind)">{{ kindLabel(l.error_kind) }}</span>
        </td>
        <td style="max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--color-text-3)" :title="l.error">{{ l.error }}</td>
      </tr>
    </tbody></table></div>
    <div class="pager">
      <span class="sh-note">共 {{ total }} 条<template v-if="total"> · 当前 {{ rangeFrom }}–{{ rangeTo }}</template></span>
      <div class="spacer"></div>
      <button class="btn btn-outline btn-sm" :disabled="page<=1 || loading" @click="go(1)">« 首页</button>
      <button class="btn btn-outline btn-sm" :disabled="page<=1 || loading" @click="go(page-1)">‹ 上一页</button>
      <span class="page-ind">{{ page }} / {{ pages }}</span>
      <button class="btn btn-outline btn-sm" :disabled="page>=pages || loading" @click="go(page+1)">下一页 ›</button>
      <button class="btn btn-outline btn-sm" :disabled="page>=pages || loading" @click="go(pages)">末页 »</button>
    </div>
    </div>
  </div>`,
};

// ── 用量分析（对齐火山方舟「用量统计」交互：区间 + 粒度 + 维度下钻） ──
// 失败原因的可读名与配色。后端 model.ErrorKind* 的取值必须在这里有对应项，
// 否则界面会露出英文枚举（errLabel 有兜底，但配色会退化成灰色）。
const ERROR_KIND_LABELS = {
  upstream_error: "上游报错",
  upstream_timeout: "上游超时",
  client_invalid: "请求非法（客户端侧）",
  client_cancel: "下游断开（客户端侧）",
  local_error: "网关内部错误",
  unclassified: "未分类（历史数据）",
};
const ERROR_KIND_COLORS = {
  upstream_error: "#ef4444",
  upstream_timeout: "#f59e0b",
  client_invalid: "#9ca3af",
  client_cancel: "#a3a3a3",
  local_error: "#8b5cf6",
  unclassified: "#d1d5db",
};

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
      // 展示开关：默认折叠细节（统计卡与分项表），避免首屏过载。
      showDetail: false,
      // 历史成本重算（v14 缓存计费修正后的回填）。分两步：先 dry-run 看影响面，
      // 再由用户确认写入——因为重算会改变已出账的数字，不该一键直接落库。
      bfOpen: false,
      bfBusy: false,
      bfPreview: null,
      bfResult: null,
      bfErr: "",
    };
  },
  computed: {
    summary() { return (this.r && this.r.summary) || {}; },
    facets() { return (this.r && this.r.facets) || []; },
    buckets() { return (this.r && this.r.series) || []; },
    errors() { return (this.r && this.r.errors) || []; },
    // 数据来源：rollup（预聚合）/ raw（原始表）。预聚合区间查得快，但实时数据
    // （水位未覆盖）走原始表——展示出来，避免「刚发的请求没出现」被误判成 bug。
    sourceBadge() {
      const s = this.r && this.r.source;
      if (s === "rollup") return { label: "预聚合", cls: "ic-green", tip: "该区间已聚合，查询走预聚合表" };
      if (s === "raw") return { label: "实时", cls: "ic-blue", tip: "区间含未聚合数据，直接查原始日志表" };
      return null;
    },
    // 平均首字耗时（TTFT）：只对流式成功请求有样本，无样本显示 — 而不是 0。
    avgFirstToken() { return fmtAvg(this.summary.first_token_ms_sum, this.summary.stream_requests); },
    avgLatency() {
      const s = this.summary;
      return fmtAvg(s.latency_ms_sum, s.requests);
    },
    // 上游失败 / 客户端断开分开算。这两者混在一起算「失败率」会误导运维：
    // 客户端断开不是上游故障，不该推动熔断，也不该让人以为要换供应商。
    upstreamFailRate() { return this.rateOf({ requests: this.summary.requests, success: this.summary.requests - (this.summary.upstream_errors || 0) }); },
    clientAbortRate() {
      const s = this.summary;
      if (!s.requests) return "—";
      return fmtPct(((s.client_errors || 0) / s.requests) * 100);
    },
    // 成本分项占比（用于分项条的宽度）。
    costParts() {
      const s = this.summary;
      const total = (s.input_cost || 0) + (s.output_cost || 0) + (s.cache_cost || 0);
      const mk = (v, color, name) => ({ name, color, value: v || 0, pct: total > 0 ? ((v || 0) / total) * 100 : 0 });
      return {
        total,
        items: [
          mk(s.input_cost, "#f59e0b", "输入"),
          mk(s.output_cost, "#8b5cf6", "输出"),
          mk(s.cache_cost, "#14b8a6", "缓存"),
        ],
      };
    },
    // 是否存在「未分类」失败（升级前的历史行）。有则给一句解释，免得被当成数据错误。
    hasUnclassified() { return this.errors.some((e) => e.kind === "unclassified" || !e.kind); },
    // 缓存命中率 = 缓存读取 /（缓存读取 + 输入）。只用输入侧口径：
    // 缓存读是「输入被缓存掉的部分」，与输出无关。
    cacheHitRate() {
      const s = this.summary;
      const read = s.cache_read_tokens || 0;
      const denom = read + (s.prompt_tokens || 0);
      if (!denom) return "—";
      return fmtPct((read / denom) * 100);
    },
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
    // ── 历史成本重算 ──
    // 区间沿用当前页面的日期选择，用户看到什么区间就重算什么区间（避免
    // 「界面上看的是 7 天，重算的却是全部」这种错位）。
    bfRange() {
      return {
        from: Math.floor(new Date(this.from + "T00:00:00").getTime() / 1000),
        to: Math.floor(new Date(this.to + "T23:59:59").getTime() / 1000),
      };
    },
    openBackfill() {
      this.bfOpen = true; this.bfPreview = null; this.bfResult = null; this.bfErr = "";
      this.runBackfill(true);
    },
    runBackfill(dry) {
      const { from, to } = this.bfRange;
      this.bfBusy = true; this.bfErr = "";
      req("POST", "/api/usage/cost/backfill?from=" + from + "&to=" + to + (dry ? "&dry_run=1" : ""))
        .then((d) => {
          if (dry) this.bfPreview = d;
          else { this.bfResult = d; this.load(); }
        })
        .catch((e) => { this.bfErr = e.message; })
        .finally(() => { this.bfBusy = false; });
    },
    pick(key) {
      this.entity = this.entity === key ? "" : key;
      this.load();
    },
    rateOf(s) { return s && s.requests ? fmtPct((s.success / s.requests) * 100) : "—"; },
    fmtInt(v) { return fmtInt(v); },
    errLabel(kind) { return ERROR_KIND_LABELS[kind] || kind || "未知"; },
    errColor(kind) { return ERROR_KIND_COLORS[kind] || "#9ca3af"; },
    // 各失败原因占「全部失败」的比例（分母是失败总数，不是请求总数）。
    errShare(n) {
      const total = this.errors.reduce((a, e) => a + (e.requests || 0), 0);
      if (!total) return "—";
      return ((n / total) * 100).toFixed(1) + "%";
    },
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
      <div class="spacer"></div>
      <button class="btn" :disabled="bfBusy" @click="openBackfill">重算历史成本</button>
    </div>

    <div class="stat-row">
      <div class="stat-card"><div class="ic ic-blue">⚡</div><div class="body"><div class="v">{{ summary.requests || 0 }}</div><div class="l">调用次数</div></div></div>
      <div class="stat-card"><div class="ic" :class="successRateCls">✓</div><div class="body"><div class="v">{{ successRate }}</div><div class="l">成功率</div></div></div>
      <div class="stat-card" title="上游报告的总 token（prompt+completion，含缓存命中），不是子 Key 额度消耗"><div class="ic ic-blue">⬤</div><div class="body"><div class="v">{{ fmtTokens(summary.total_tokens) }}</div><div class="l">总 Tokens<span class="stat-sub">上游原始</span></div></div></div>
      <div class="stat-card" title="上游报告的 prompt_tokens，含缓存命中量"><div class="ic ic-green">↓</div><div class="body"><div class="v">{{ fmtTokens(summary.prompt_tokens) }}</div><div class="l">输入 Tokens<span class="stat-sub">含缓存命中</span></div></div></div>
      <div class="stat-card"><div class="ic ic-purple">↑</div><div class="body"><div class="v">{{ fmtTokens(summary.completion_tokens) }}</div><div class="l">输出 Tokens</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">🖼</div><div class="body"><div class="v">{{ summary.images || 0 }}</div><div class="l">图像（张）</div></div></div>
      <div class="stat-card"><div class="ic ic-orange">💰</div><div class="body"><div class="v">{{ fmtCost(summary.cost) }}</div><div class="l">费用</div></div></div>
    </div>

    <div class="stat-row">
      <div class="stat-card"><div class="ic ic-red">⚠</div><div class="body"><div class="v">{{ fmtInt(summary.upstream_errors || 0) }}</div><div class="l">上游失败</div></div></div>
      <div class="stat-card"><div class="ic ic-gray">✕</div><div class="body"><div class="v">{{ fmtInt(summary.client_errors || 0) }}</div><div class="l">客户端侧失败</div></div></div>
      <div class="stat-card"><div class="ic ic-purple">⇄</div><div class="body"><div class="v">{{ fmtInt(summary.stream_requests || 0) }}</div><div class="l">流式请求</div></div></div>
      <div class="stat-card"><div class="ic ic-blue">⏱</div><div class="body"><div class="v">{{ avgFirstToken }}</div><div class="l">平均首字耗时</div></div></div>
      <div class="stat-card"><div class="ic ic-green">⏳</div><div class="body"><div class="v">{{ avgLatency }}</div><div class="l">平均总耗时</div></div></div>
      <div class="stat-card"><div class="ic ic-teal">♻</div><div class="body"><div class="v">{{ cacheHitRate }}</div><div class="l">缓存命中占输入</div></div></div>
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">{{ chartTitle }}（{{ chartUnit }}） · {{ dimLabel }}<template v-if="dim">：{{ entityLabel }}</template> · 按{{ gran === 'hour' ? '小时' : '天' }}</div>
        <span v-if="sourceBadge" class="src-badge" :class="sourceBadge.cls" :title="sourceBadge.tip">{{ sourceBadge.label }}</span>
      </div>
      <div class="chart-wrap" v-html="chartHtml"></div>
    </div>

    <div class="detail-grid">
      <div class="card" v-if="costParts.total > 0">
        <div class="card-head"><div class="card-title">费用构成</div></div>
        <div class="split-bar">
          <div v-for="it in costParts.items" :key="it.name" class="split-seg"
               :style="{width: it.pct + '%', background: it.color}" :title="it.name + ' ' + fmtCost(it.value)"></div>
        </div>
        <table class="mini-table">
          <tr v-for="it in costParts.items" :key="it.name">
            <td><span class="dot" :style="{background: it.color}"></span>{{ it.name }}</td>
            <td>{{ fmtCost(it.value) }}</td>
            <td class="muted">{{ it.pct.toFixed(1) }}%</td>
          </tr>
          <tr class="total-row"><td>合计</td><td>{{ fmtCost(costParts.total) }}</td><td class="muted">100%</td></tr>
        </table>
      </div>

      <div class="card" v-if="errors.length">
        <div class="card-head"><div class="card-title">失败原因分布</div></div>
        <table class="mini-table">
          <tr v-for="e in errors" :key="e.kind">
            <td><span class="dot" :style="{background: errColor(e.kind)}"></span>{{ errLabel(e.kind) }}</td>
            <td>{{ fmtInt(e.requests) }}</td>
            <td class="muted">{{ errShare(e.requests) }}</td>
          </tr>
        </table>
        <div class="hint">
          客户端侧失败（不合法的请求 / 下游主动断开）不计入上游失败率，也不推动熔断。
          <span v-if="hasUnclassified">「未分类」是升级前写入的历史记录，其失败一律按上游失败统计。</span>
        </div>
      </div>

      <div class="card" v-if="(summary.cache_read_tokens || 0) + (summary.cache_creation_tokens || 0) > 0">
        <div class="card-head"><div class="card-title">缓存 Token</div></div>
        <table class="mini-table">
          <tr><td>缓存读取</td><td>{{ fmtTokens(summary.cache_read_tokens) }}</td><td class="muted">命中占输入 {{ cacheHitRate }}</td></tr>
          <tr><td>缓存写入</td><td>{{ fmtTokens(summary.cache_creation_tokens) }}</td><td class="muted"></td></tr>
        </table>
      </div>
    </div>

    <div class="card" v-if="dim">
      <div class="card-head"><div class="card-title">{{ dimLabel }}拆分（点击行下钻，再点取消）</div></div>
      <div class="table-wrap"><table><thead><tr>
        <th>{{ dimLabel }}</th><th>次数</th><th>成功率</th><th title="上游报告的总 token（prompt+completion，含缓存命中）。计费与展示口径，不是子 Key 额度消耗">Tokens<span class="th-sub">上游原始</span></th><th>缓存读取</th><th>图像</th><th>成本</th><th>上游失败</th>
      </tr></thead><tbody>
        <tr class="clickable" :class="{selected: entity===''}" @click="pick('')">
          <td>全部</td><td>{{ summary.requests || 0 }}</td><td>{{ successRate }}</td>
          <td>{{ fmtTokens(summary.total_tokens) }}</td><td>{{ fmtTokens(summary.cache_read_tokens) }}</td>
          <td>{{ summary.images || '—' }}</td><td class="cost">{{ fmtCost(summary.cost) }}</td>
          <td>{{ summary.upstream_errors || '—' }}</td>
        </tr>
        <tr v-for="f in facets" :key="f.key" class="clickable" :class="{selected: entity===f.key}" @click="pick(f.key)">
          <td class="mono">{{ f.label }}</td><td>{{ f.requests }}</td><td>{{ rateOf(f) }}</td>
          <td>{{ fmtTokens(f.total_tokens) }}</td><td>{{ fmtTokens(f.cache_read_tokens) }}</td>
          <td>{{ f.images || '—' }}</td><td class="cost">{{ fmtCost(f.cost) }}</td>
          <td>{{ f.upstream_errors || '—' }}</td>
        </tr>
        <tr v-if="!facets.length"><td colspan="8" class="empty">所选区间暂无数据</td></tr>
      </tbody></table></div>
    </div>

    <ui-drawer :open="bfOpen" title="重算历史成本" subtitle="按当前模型定价重新计算所选区间的成本" :width="560" @close="bfOpen=false">
      <div class="sec">
        <div class="sec-title">为什么要重算</div>
        <div class="hint" style="padding:0 0 8px">
          缓存计费修正后，<b>新请求</b>已按缓存读取/写入各自单价计算；但<b>修正前已落库</b>
          的成本仍是旧口径（prompt_token 全额按输入价计，缓存命中被多收、缓存写入被漏收）。
          本操作按当前定价重算，仅影响已发生的日志，不改变请求处理行为。
        </div>
        <div class="hint" style="padding:0 0 12px">
          区间取自上方日期选择：<b>{{ from }} ~ {{ to }}</b>。重算会同时刷新预聚合，
          因而界面上显示的数值会随之变化。请避免用与已对账的报表对齐的区间。
        </div>
      </div>

      <div class="sec" v-if="bfPreview && !bfResult">
        <div class="sec-title">影响面（预演，尚未写入）</div>
        <table class="mini-table">
          <tr><td>区间内日志</td><td>{{ fmtInt(bfPreview.scanned) }} 条</td></tr>
          <tr><td>成本将变化</td><td>{{ fmtInt(bfPreview.updated) }} 条</td></tr>
          <tr v-if="bfPreview.unpriced">
            <td>无定价（置 0）</td>
            <td><span style="color:var(--color-danger-6)">{{ fmtInt(bfPreview.unpriced) }} 条</span></td>
          </tr>
          <tr><td>重算前总额</td><td>{{ fmtCost(bfPreview.old_total) }}</td></tr>
          <tr><td>重算后总额</td><td>{{ fmtCost(bfPreview.new_total) }}</td></tr>
          <tr class="total-row">
            <td>差额</td>
            <td :style="{color: bfPreview.delta < 0 ? 'var(--color-success-6)' : 'var(--color-danger-6)'}">
              {{ (bfPreview.delta > 0 ? '+' : '') + fmtCost(bfPreview.delta) }}
            </td>
          </tr>
        </table>
        <div class="hint" v-if="bfPreview.unpriced">
          「无定价」表示该模型当前没有单价配置（通常已被删除或从未定价）。
          这些行的成本会被置 0——若这不是预期结果，请先补齐模型定价再重算。
        </div>
      </div>

      <div class="sec" v-if="bfResult">
        <div class="sec-title">重算完成</div>
        <table class="mini-table">
          <tr><td>已更新</td><td>{{ fmtInt(bfResult.updated) }} / {{ fmtInt(bfResult.scanned) }} 条</td></tr>
          <tr><td>总额</td><td>{{ fmtCost(bfResult.old_total) }} → {{ fmtCost(bfResult.new_total) }}</td></tr>
          <tr><td>预聚合刷新</td><td>{{ fmtInt(bfResult.rollup_rows) }} 行</td></tr>
        </table>
        <div class="hint" v-if="bfResult.rollup_error" style="color:var(--color-danger-6)">
          预聚合刷新失败：{{ bfResult.rollup_error }}（原始日志已重算完成，仅聚合视图滞后）
        </div>
      </div>

      <div class="sec" v-if="bfErr">
        <div class="hint" style="color:var(--color-danger-6)">{{ bfErr }}</div>
      </div>

      <template #foot>
        <button class="btn" @click="bfOpen=false">关闭</button>
        <div class="spacer"></div>
        <button class="btn" :disabled="bfBusy || !bfPreview || bfResult" @click="runBackfill(true)">重新预演</button>
        <button class="btn btn-primary" :disabled="bfBusy || !bfPreview || bfResult || bfPreview.updated === 0"
                @click="runBackfill(false)">
          {{ bfBusy ? '处理中…' : '确认重算' }}
        </button>
      </template>
    </ui-drawer>
  </div>`,
};

// ── 分流配置（把权重与 fallback 链做成可视化编排） ──
// 权重口径与后端 model.Endpoint.EffectiveWeight 一致：叶权重 > 账号权重 > 1；
// 占比只按「启用且未熔断」的叶子折算——这才是真实会承接流量的集合。
const ROUTE_COLORS = ["#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ef4444", "#14b8a6", "#ec4899", "#6366f1"];

const RoutingPage = {
  data() {
    return {
      models: [], accounts: [], eps: [], runtime: {},
      sel: "",            // 当前编辑的模型名
      modelQuery: "",     // 左侧模型搜索
      poolQuery: "",      // fallback 候选搜索
      wDraft: {},          // 权重草稿：endpointID -> 数值
      chain: [],           // fallback 链草稿
      rDraft: { rules: [], default_target: "" }, // 输入长度分流规则草稿（路由模型）
      dragIdx: -1,        // 拖拽编排：正在拖动的链内下标（-1 = 未拖）
      dragPool: "",       // 拖拽编排：来自候选区的候选模型名（空 = 非候选拖拽）
      overIdx: -1,        // 拖拽编排：悬停目标下标（仅作视觉提示）
      savingW: false, savingC: false, savingR: false, loading: false,
    };
  },
  mounted() { this.load(); },
  computed: {
    model() { return this.models.find((m) => m.name === this.sel) || null; },
    // isRouter 当前选中模型是否为路由模型：右侧编排切换为「输入长度分流规则」
    //（路由模型没有接入点，权重与 fallback 两块对它没有意义）。
    isRouter() {
      return ((this.model && this.model.type) || "text") === "router";
    },
    // routerTargets 路由规则的目标候选：文本模型，排除自身。后端也允许指向
    // 路由模型做链式分流，但 UI 收敛到文本目标，避免配置难以排查。
    routerTargets() {
      return this.models
        .filter((m) => (m.type || "text") === "text" && m.name !== this.sel)
        .map((m) => m.name);
    },
    filteredModels() {
      const q = this.modelQuery.trim().toLowerCase();
      return q ? this.models.filter((m) => m.name.toLowerCase().includes(q)) : this.models;
    },
    filteredCandidates() {
      const q = this.poolQuery.trim().toLowerCase();
      return q ? this.candidates.filter((name) => name.toLowerCase().includes(q)) : this.candidates;
    },
    // rDirty 分流规则草稿是否有改动（空目标行视为未改，与保存口径一致）。
    rDirty() {
      const norm = (rc) => JSON.stringify({
        rules: ((rc && rc.rules) || []).filter((r) => r.target)
          .map((r) => ({ max_input_tokens: Number(r.max_input_tokens) || 0, target: r.target })),
        default_target: (rc && rc.default_target) || "",
      });
      return norm(this.model && this.model.router) !== norm(this.rDraft);
    },
    // leaves 当前模型的全部叶子（含停用，停用项只展示不参与占比）。
    leaves() {
      return this.eps
        .filter((e) => e.model === this.sel)
        .map((e, i) => {
          const rt = this.runtime[e.id] || {};
          const w = this.effWeight(e);
          return {
            ...e, color: ROUTE_COLORS[i % ROUTE_COLORS.length],
            eff: w, inherited: !(Number(e.weight) > 0),
            circuit: !!rt.circuit_open, concurrency: rt.concurrency || 0,
            // half_open：冷却已到期但探测尚未成功。此时**不算 live**——它还不能
            // 放心承接流量（最多只有一个探测请求在试）。若把它算作健康，分流页的
            // 占比条会虚高，让人误以为容量已经恢复。
            halfOpen: !!rt.half_open,
            live: e.enabled && !rt.circuit_open && !rt.half_open,
          };
        });
    },
    liveTotal() {
      return this.leaves.filter((l) => l.live).reduce((s, l) => s + l.eff, 0);
    },
    // 候选 fallback：同类型、已存在、非自身、且不在链上。
    candidates() {
      const t = (this.model && this.model.type) || "text";
      return this.models
        .filter((m) => (m.type || "text") === t && m.type !== "router" && m.name !== this.sel && this.chain.indexOf(m.name) < 0)
        .map((m) => m.name);
    },
    chainDirty() {
      const orig = ((this.model && this.model.fallback) || []).join("|");
      return orig !== this.chain.join("|");
    },
    weightDirty() {
      return this.leaves.some((l) => Number(this.wDraft[l.id]) !== Number(l.weight || 0));
    },
  },
  methods: {
    load() {
      this.loading = true;
      Promise.all([
        req("GET", "/api/models"), req("GET", "/api/accounts"),
        req("GET", "/api/endpoints"), req("GET", "/api/stats"),
      ])
        .then((rs) => {
          this.models = rs[0] || [];
          this.accounts = rs[1] || [];
          this.eps = rs[2] || [];
          const rt = {};
          ((rs[3] && rs[3].endpoints) || []).forEach((e) => { rt[e.id] = e.runtime || {}; });
          this.runtime = rt;
          if (!this.sel || !this.models.some((m) => m.name === this.sel)) {
            const first = this.models.find((m) => this.eps.some((e) => e.model === m.name));
            this.sel = (first && first.name) || (this.models[0] && this.models[0].name) || "";
          }
          this.resetDrafts();
        })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.loading = false; });
    },
    resetDrafts() {
      const w = {};
      this.eps.filter((e) => e.model === this.sel).forEach((e) => { w[e.id] = Number(e.weight || 0); });
      this.wDraft = w;
      this.chain = ((this.model && this.model.fallback) || []).slice();
      const rc = this.model && this.model.router;
      this.rDraft = {
        rules: ((rc && rc.rules) || []).map((r) => ({ max_input_tokens: r.max_input_tokens || 0, target: r.target || "" })),
        default_target: (rc && rc.default_target) || "",
      };
    },
    pickModel(name) { this.sel = name; this.resetDrafts(); },
    accName(id) {
      const a = this.accounts.find((x) => x.id === id);
      return (a && a.name) || id;
    },
    accWeight(id) {
      const a = this.accounts.find((x) => x.id === id);
      return (a && a.weight) || 1;
    },
    // effWeight 用草稿值算，让占比随输入实时变化（口径同后端）。
    effWeight(e) {
      const draft = this.wDraft[e.id];
      const own = Number(draft !== undefined ? draft : e.weight || 0);
      return own > 0 ? own : this.accWeight(e.account_id);
    },
    sharePct(l) {
      if (!l.live || this.liveTotal <= 0) return 0;
      return (l.eff / this.liveTotal) * 100;
    },
    epCountOf(name) { return this.eps.filter((e) => e.model === name).length; },
    bump(l, delta) {
      const cur = Number(this.wDraft[l.id] || 0);
      this.wDraft[l.id] = Math.max(0, cur + delta);
    },
    saveWeights() {
      const changed = this.leaves.filter((l) => Number(this.wDraft[l.id]) !== Number(l.weight || 0));
      if (!changed.length) { toast("权重没有改动"); return; }
      this.savingW = true;
      // 映射 PUT 会无条件覆盖四个流控字段，因此必须整组回传，否则并发/RPM/TPM 被清零。
      Promise.all(changed.map((l) => req("PUT", "/api/endpoints/" + l.id, {
        weight: Number(this.wDraft[l.id]) || 0,
        max_concurrency: l.max_concurrency || 0,
        rpm_limit: l.rpm_limit || 0,
        tpm_limit: l.tpm_limit || 0,
      })))
        .then(() => { toast("已保存 " + changed.length + " 条权重"); this.load(); })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.savingW = false; });
    },
    toggleLeaf(l) {
      req("PUT", "/api/endpoints/" + l.id, {
        enabled: !l.enabled,
        weight: l.weight || 0, max_concurrency: l.max_concurrency || 0,
        rpm_limit: l.rpm_limit || 0, tpm_limit: l.tpm_limit || 0,
      })
        .then(() => { toast(l.enabled ? "已停用该接入点" : "已启用该接入点"); this.load(); })
        .catch((e) => toast(e.message, false));
    },
    // ── fallback 链：拖拽编排 ──
    // HTML5 DnD：dataTransfer 只放一个标记串给浏览器（Firefox 起拖必需），
    // 真状态（链内下标 / 候选名）留在组件里。dragend 统一复位，拖放取消也干净。
    chainDragStart(i, e) {
      this.dragIdx = i;
      this.dragPool = "";
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", "chain:" + i);
    },
    // poolDragStart 从候选区起拖：落进链路时按「插入」处理而不是换位。
    poolDragStart(name, e) {
      this.dragPool = name;
      this.dragIdx = -1;
      e.dataTransfer.effectAllowed = "copy";
      e.dataTransfer.setData("text/plain", "pool:" + name);
    },
    chainDragOver(i) {
      if (this.dragIdx >= 0 || this.dragPool) this.overIdx = i;
    },
    chainDragEnd() {
      this.dragIdx = -1;
      this.dragPool = "";
      this.overIdx = -1;
    },
    // chainDrop 把拖动项插入到下标 i：候选拖拽 = 插入；链内拖拽 = 换位
    //（先删后插，从后往前拖时目标左移一位，插到 i 前先校正）。
    chainDrop(i) {
      if (this.dragPool) {
        if (!this.chain.includes(this.dragPool)) this.chain.splice(i, 0, this.dragPool);
        this.chainDragEnd();
        return;
      }
      if (this.dragIdx < 0 || this.dragIdx === i) { this.chainDragEnd(); return; }
      const from = this.dragIdx;
      const [item] = this.chain.splice(from, 1);
      this.chain.splice(from < i ? i - 1 : i, 0, item);
      this.chainDragEnd();
    },
    // chainRowDrop 拖到链路空白处：候选拖到末尾追加，链内拖拽移到末尾。
    chainRowDrop() {
      if (this.dragPool) {
        if (!this.chain.includes(this.dragPool)) this.chain.push(this.dragPool);
      } else if (this.dragIdx >= 0) {
        const [item] = this.chain.splice(this.dragIdx, 1);
        this.chain.push(item);
      }
      this.chainDragEnd();
    },
    // appendFallback 点击候选直接追加到链尾（与拖拽等价的快捷方式）。
    appendFallback(name) {
      if (!name || this.chain.includes(name)) return;
      this.chain.push(name);
    },
    dropFallback(i) { this.chain.splice(i, 1); },
    saveChain() {
      if (!this.sel) return;
      this.savingC = true;
      req("PUT", "/api/models/" + encodeURIComponent(this.sel), { fallback: this.chain })
        .then(() => { toast("已保存 fallback 链"); this.load(); })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.savingC = false; });
    },
    // ── 输入长度分流（路由模型） ──
    addRule() {
      this.rDraft.rules.push({ max_input_tokens: 0, target: "" });
    },
    removeRule(i) { this.rDraft.rules.splice(i, 1); },
    saveRouter() {
      if (!this.sel) return;
      const rules = this.rDraft.rules
        .filter((r) => r.target)
        .map((r) => ({ max_input_tokens: Number(r.max_input_tokens) || 0, target: r.target }));
      if (!rules.length && !this.rDraft.default_target) {
        toast("路由模型需要至少一条分流规则或默认目标", false);
        return;
      }
      this.savingR = true;
      // 模型 PUT 是部分更新：只带 router 键，不会动到该模型的其它字段。
      req("PUT", "/api/models/" + encodeURIComponent(this.sel), { router: { rules, default_target: this.rDraft.default_target || "" } })
        .then(() => { toast("已保存分流规则"); this.load(); })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.savingR = false; });
    },
    rateOfLeaf(l) {
      return l.total_requests ? fmtPct((l.success_requests / l.total_requests) * 100) : "—";
    },
  },
  template: `
  <div class="page">
    <div class="page-head">
      <div class="page-title" style="margin:0">分流配置</div>
      <div class="row-actions">
        <button class="btn btn-outline btn-sm" :disabled="loading" @click="load">↻ 刷新</button>
      </div>
    </div>

    <div class="route-wrap">
      <div class="card route-side">
        <div class="card-head"><div class="card-title">模型</div></div>
        <div class="route-search"><input v-model="modelQuery" placeholder="搜索模型"/></div>
        <div class="route-list">
          <div v-for="m in filteredModels" :key="m.name" class="route-item" :class="{active: sel===m.name}" @click="pickModel(m.name)">
            <span class="mono">{{ m.name }}</span>
            <span v-if="(m.type||'text')==='router'" class="tag tag-orange">路由</span>
            <span v-else :class="epCountOf(m.name) ? 'tag tag-blue' : 'tag tag-gray'">{{ epCountOf(m.name) }}</span>
          </div>
          <div v-if="!filteredModels.length" class="empty">{{ models.length ? '没有匹配的模型' : '暂无模型' }}</div>
        </div>
      </div>

      <div class="route-main">
        <!-- 路由模型：没有接入点，编排入口是「输入长度分流规则」而非权重/fallback -->
        <div class="card" v-if="isRouter">
          <div class="card-head"><div class="card-title">输入长度分流 · {{ sel || '—' }}</div>
            <div class="row-actions">
              <button class="btn btn-outline btn-sm" :disabled="!rDirty" @click="resetDrafts">还原</button>
              <button class="btn btn-primary btn-sm" :disabled="savingR || !rDirty" @click="saveRouter">{{ savingR ? '保存中…' : '保存规则' }}</button>
            </div>
          </div>
          <div style="padding: 12px 16px 16px">
            <div class="router-rule" v-for="(r, i) in rDraft.rules" :key="i">
              <span class="sh-note">输入 ≤</span>
              <input v-model.number="r.max_input_tokens" type="number" min="0" style="width:130px"/>
              <span class="sh-note">tokens →</span>
              <select v-model="r.target" style="width:210px">
                <option value="">选择目标模型…</option>
                <option v-for="t in routerTargets" :key="t" :value="t">{{ t }}</option>
              </select>
              <ProbeTest v-if="r.target" :payload="{model: r.target}"/>
              <button class="btn btn-outline btn-sm" @click="removeRule(i)">移除</button>
            </div>
            <div><button class="btn btn-outline btn-sm" @click="addRule">+ 添加规则</button></div>
            <div class="form-item" style="margin-top:10px;max-width:360px">
              <label>默认目标（输入超过全部阈值时）</label>
              <select v-model="rDraft.default_target" style="width:100%">
                <option value="">（不设默认）</option>
                <option v-for="t in routerTargets" :key="t" :value="t">{{ t }}</option>
              </select>
              <ProbeTest v-if="rDraft.default_target" :payload="{model: rDraft.default_target}" style="margin-top:8px"/>
            </div>
            <div class="sh-note" style="margin-top:8px">估算口径：中日韩 1 字 ≈ 1 token，其它 4 字符 ≈ 1 token；仅用于选路，计费按上游真实用量。</div>
          </div>
        </div>

        <template v-else>
        <div class="card">
          <div class="card-head"><div class="card-title">权重分摊 · {{ sel || '—' }}</div>
            <div class="row-actions">
              <button class="btn btn-outline btn-sm" :disabled="!weightDirty" @click="resetDrafts">还原</button>
              <button class="btn btn-primary btn-sm" :disabled="savingW || !weightDirty" @click="saveWeights">{{ savingW ? '保存中…' : '保存权重' }}</button>
            </div>
          </div>
          <div v-if="leaves.length">
            <div class="share-bar">
              <div v-for="l in leaves.filter(x => x.live)" :key="l.id" class="share-seg"
                :style="{width: sharePct(l) + '%', background: l.color}"
                :title="accName(l.account_id) + ' · ' + l.ep + ' ' + sharePct(l).toFixed(1) + '%'">
                <span v-if="sharePct(l) >= 8">{{ sharePct(l).toFixed(0) }}%</span>
              </div>
              <div v-if="!liveTotal" class="share-empty">当前没有可承接流量的接入点</div>
            </div>
            <div class="table-wrap"><table><thead><tr>
              <th>账号 · 上游标识</th><th>权重</th><th>有效权重</th><th>占比</th><th>状态</th><th>累计请求</th><th>成功率</th><th>操作</th>
            </tr></thead><tbody>
              <tr v-for="l in leaves" :key="l.id">
                <td><span class="dot-color" :style="{background: l.color}"></span>{{ accName(l.account_id) }} · <span class="mono">{{ l.ep }}</span>
                  <span v-if="l.upstream_deleted" class="tag tag-warn" title="该上游标识已不在对应账号的模型列表中：映射保留、路由不受影响；上游恢复后此标记自动消失。">⚠ 上游已删除</span></td>
                <td><div class="w-edit">
                  <button class="btn btn-outline btn-sm" @click="bump(l, -1)">−</button>
                  <input v-model.number="wDraft[l.id]" type="number" min="0"/>
                  <button class="btn btn-outline btn-sm" @click="bump(l, 1)">+</button>
                </div></td>
                <td>{{ l.eff }}<span v-if="l.inherited" class="sh-note">（继承账号）</span></td>
                <td>{{ l.live ? sharePct(l).toFixed(1) + '%' : '—' }}</td>
                <td>
                  <span v-if="!l.enabled" class="tag tag-gray">停用</span>
                  <span v-else-if="l.circuit" class="tag tag-red">熔断中</span>
                  <span v-else-if="l.halfOpen" class="tag tag-warn" title="冷却已到期，正放行一个探测请求验证上游；成功即恢复承接，失败则继续熔断。">探测中</span>
                  <span v-else class="tag tag-green">承接中</span>
                </td>
                <td>{{ l.total_requests || 0 }}</td>
                <td>{{ rateOfLeaf(l) }}</td>
                <td><div class="row-actions">
                  <ProbeTest :payload="{model: sel, account_id: l.account_id, ep: l.ep, protocol: (model && model.provider) || ''}"/>
                  <button class="btn btn-outline btn-sm" @click="toggleLeaf(l)">{{ l.enabled ? '停用' : '启用' }}</button>
                </div></td>
              </tr>
            </tbody></table></div>
            <div class="sh-note" style="margin-top:8px">权重 0 = 继承账号权重（当前账号权重见「有效权重」列）；占比只按启用且未熔断的接入点折算，与网关实际选路口径一致。</div>
          </div>
          <div v-else class="empty">该模型还没有接入点映射，先到「模型映射」页添加</div>
        </div>

        <div class="card">
          <div class="card-head"><div class="card-title">fallback 链 · {{ sel || '—' }}</div>
            <div class="row-actions">
              <button class="btn btn-outline btn-sm" :disabled="!chainDirty" @click="resetDrafts">还原</button>
              <button class="btn btn-primary btn-sm" :disabled="savingC || !chainDirty" @click="saveChain">{{ savingC ? '保存中…' : '保存链路' }}</button>
            </div>
          </div>
          <div class="chain-row" @dragover.prevent @drop.prevent="chainRowDrop">
            <span class="chip chip-self mono">{{ sel || '—' }}</span>
            <template v-for="(f, i) in chain" :key="f">
              <span class="chain-arrow">→</span>
              <span class="chip chip-drag" draggable="true"
                :class="{dragging: dragIdx === i, 'drag-over': overIdx === i && dragIdx !== i}"
                :title="'拖拽调整 ' + f + ' 的位置'"
                @dragstart="chainDragStart(i, $event)" @dragend="chainDragEnd"
                @dragover.prevent="chainDragOver(i)" @drop.prevent.stop="chainDrop(i)">
                <span class="mono">{{ f }}</span>
                <ProbeTest :payload="{model: f}" label="⚡" :compact="true" btn-class="chip-btn"/>
                <button class="chip-btn chip-del" title="移除" @click="dropFallback(i)">×</button>
              </span>
            </template>
            <span v-if="!chain.length" class="sh-note">未配置 fallback：该模型全部接入点不可用时直接返回错误</span>
          </div>
          <div class="sh-note" style="padding: 0 16px 8px">拖拽调整顺序，拖到空白处移到末尾；仅同类型可入链，链不向下传递。</div>
          <div class="chain-add">
            <input v-if="candidates.length" v-model="poolQuery" placeholder="搜索候选模型" style="width:210px"/>
            <div class="chain-pool" v-if="filteredCandidates.length">
              <span v-for="c in filteredCandidates" :key="c" class="ep-chip" draggable="true"
                title="点击追加到链尾，或拖到链上任意位置"
                @dragstart="poolDragStart(c, $event)" @dragend="chainDragEnd"
                @click="appendFallback(c)">{{ c }}</span>
            </div>
            <span v-else class="sh-note">{{ candidates.length ? '没有匹配的候选模型' : '没有可追加的同类型候选模型' }}</span>
            <span class="sh-note">点击候选追加到链尾。</span>
          </div>
        </div>
        </template>
      </div>
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
            currency_rate: d.currency_rate || 0,
            currency_symbol: d.currency_symbol || "$",
            currency_code: d.currency_code || "USD",
          };
          // 币种是全局展示参数，拉到即应用（金额格式化统一走 fmtCost）。
          applyCurrency(d);
        })
        .catch((e) => toast(e.message, false));
    },
    saveRuntime() {
      if (this.savingRt) return;
      this.savingRt = true;
      req("PUT", "/api/settings/runtime", {
        request_timeout_sec: Number(this.rtForm.request_timeout_sec) || 0,
        first_token_timeout_sec: Number(this.rtForm.first_token_timeout_sec) || 0,
        currency_rate: Number(this.rtForm.currency_rate) || 0,
        currency_symbol: this.rtForm.currency_symbol,
        currency_code: this.rtForm.currency_code,
      })
        .then((d) => {
          this.rt = d;
          applyCurrency(d);
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
    <div class="card"><div class="card-head"><div class="card-title">计价币种（展示层，热生效）</div></div>
      <div class="form-row">
        <div class="form-item"><label>汇率（1 美元 = ? 本币；0 = 不换算，按美元展示）</label>
          <input v-model.number="rtForm.currency_rate" type="number" min="0" :max="rt && rt.max_currency_rate || 1000000" step="0.01"/>
          <div class="form-tip">仅影响界面展示的金额。</div></div>
        <div class="form-item"><label>货币符号</label>
          <input v-model="rtForm.currency_symbol" maxlength="4" placeholder="¥"/>
          <div class="form-tip">留空则回落 <span class="mono">$</span>。</div></div>
        <div class="form-item"><label>货币代码（ISO，可选）</label>
          <input v-model="rtForm.currency_code" maxlength="8" placeholder="CNY"/>
          <div class="form-tip">当符号是 <span class="mono">$</span> 时会附在金额后以区分美元。</div></div>
      </div>
      <div class="row-actions">
        <button class="btn btn-primary" :disabled="savingRt" @click="saveRuntime">{{ savingRt ? '保存中…' : '保存' }}</button>
      </div>
      <div class="hint" style="margin-top:12px">
        <b>模型单价与历史成本始终以美元存储</b>（与上游目录一致），这里只做展示换算。
        因此调整汇率不会改写任何已落库的数据，也不需要重算历史成本；
        目录自动补全写入的仍是美元单价。
        <br/>当前展示：{{ fmtCost(1) }} = 1 美元。
      </div>
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
  { key: "overview", label: "总览", icon: '<rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/>', comp: "OverviewPage" },
  { key: "usage", label: "用量分析", icon: '<path d="M3 3v18h18"/><path d="m7 14 4-4 3 3 5-6"/>', comp: "UsagePage" },
  { key: "accounts", label: "上游账号", icon: '<rect x="2" y="4" width="20" height="7" rx="2"/><rect x="2" y="13" width="20" height="7" rx="2"/><path d="M6 7.5h.01M6 16.5h.01"/>', comp: "AccountsPage" },
  { key: "models", label: "模型映射", icon: '<path d="M21 8 12 3 3 8v8l9 5 9-5z"/><path d="m3 8 9 5 9-5"/><path d="M12 13v8"/>', comp: "ModelsPage" },
  { key: "routing", label: "分流配置", icon: '<path d="M16 3h5v5"/><path d="M8 3H3v5"/><path d="m21 3-6.5 6.5"/><path d="m3 3 7 7"/><path d="M16 21h5v-5"/><path d="m21 21-5-5"/>', comp: "RoutingPage" },
  { key: "subkeys", label: "子 Key", icon: '<circle cx="8" cy="15" r="4"/><path d="m10.8 12.2 8.7-8.7"/><path d="m15 8 3 3"/><path d="m18 5 2 2"/>', comp: "SubKeysPage" },
  { key: "logs", label: "请求日志", icon: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M16 13H8"/><path d="M16 17H8"/>', comp: "LogsPage" },
  { key: "settings", label: "设置", icon: '<path d="M4 21v-7"/><path d="M4 10V3"/><path d="M12 21v-9"/><path d="M12 8V3"/><path d="M20 21v-5"/><path d="M20 12V3"/><path d="M2 14h4"/><path d="M10 8h4"/><path d="M18 16h4"/>', comp: "SettingsPage" },
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
      <div class="logo"><span class="dot"></span>ArkGate<span class="ver">v1.2</span></div>
      <div class="nav">
        <div v-for="m in menu" :key="m.key" class="nav-item" :class="{active: view===m.key}" @click="view=m.key">
          <svg class="nav-ic" viewBox="0 0 24 24" v-html="m.icon"></svg>{{ m.label }}
        </div>
      </div>
      <div class="side-help">
        <div class="side-help-head" @click="toggleHelp">
          <span>📖 使用说明</span><span class="arr">{{ prefs.helpOpen ? '▾' : '▸' }}</span>
        </div>
        <div v-show="prefs.helpOpen" class="side-help-body">
          <div class="sh-row">Base URL：<span class="mono linkish" title="点击复制" @click="copy(host + '/v1')">{{ host }}/v1</span></div>
          <div class="sh-row mono">POST /v1/chat/completions</div>
          <div class="sh-row mono">POST /v1/messages <span class="tag tag-orange" title="Anthropic Messages 协议：Claude Code 等客户端直连">Anthropic</span></div>
          <div class="sh-row mono">POST /v1/messages/count_tokens</div>
          <div class="sh-row mono">POST /v1/responses</div>
          <div class="sh-row mono">POST /v1/images/generations</div>
          <div class="sh-row mono">GET /v1/models</div>
          <div class="sh-note">鉴权：Authorization: Bearer sk-你的子Key（/v1/messages 兼容 x-api-key 头）</div>
          <div class="sh-note">Claude Code 接入：环境变量 ANTHROPIC_BASE_URL=<span class="mono linkish" title="点击复制" @click="copy(host)">{{ host }}</span> 与 ANTHROPIC_AUTH_TOKEN=sk-xxx。</div>
        </div>
      </div>
      <div style="padding:12px">
        <button class="btn btn-outline" style="width:100%" @click="logout">退出登录</button>
      </div>
    </aside>
    <main class="main">
      <transition name="page" mode="out-in">
        <component :is="current" :key="view"></component>
      </transition>
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
    // 周期文案：门户标题里的「今日/本周/本月」必须与限额周期一致，
    // 否则用户会以为 100 万额度是「每天的」，而实际是每月的。
    periodLabel() {
      const p = this.d && this.d.quota && this.d.quota.period;
      if (p === "week") return "本周";
      if (p === "month") return "本月";
      return "今日";
    },
    // 重置说明：让用户知道额度何时归零（跨时区部署时尤其重要）。
    resetLabel() {
      const q = (this.d && this.d.quota) || {};
      const h = q.reset_hour || 0;
      const wd = ["", "周一", "周二", "周三", "周四", "周五", "周六", "周日"];
      if (q.period === "week") {
        return "每" + (wd[q.reset_weekday] || "周一") + " " + h + " 点重置";
      }
      if (q.period === "month") {
        return "每月 1 日 " + h + " 点重置";
      }
      return "每日 " + h + " 点重置";
    },
    requestProgress() {
      const lim = this.d && this.d.quota && this.d.quota.limit_requests;
      if (!lim) return null;
      const p = (this.d.today.requests / lim) * 100;
      return { pct: Math.min(100, p), cls: p >= 90 ? "danger" : p >= 70 ? "warn" : "" };
    },
    // quotaVsRaw 把加权后的额度消耗换算回「上游原始 token」的量级，
    // 用于向用户解释「为什么表格里的 Tokens 比额度大」。
    //
    // 注意这是**估算**：加权计数是 plain + output + read×kr + write×kw，
    // 反推需要知道各段的原始构成，而中间那张卡只有 week 的聚合值。
    // 因此这里只在「有缓存用量」时按最简单可靠的关系给出量级：
    // 加权 ≈ 原始 - 缓存读×(1-kr) - 缓存写×(1-kw)。取负则不出示。
    quotaVsRaw() {
      if (!this.d || !this.d.today || !this.d.quota) return null;
      const q = this.d.quota;
      const kr = q.cache_read_permille ? q.cache_read_permille / 1000 : 1;
      const kw = q.cache_write_permille ? q.cache_write_permille / 1000 : 1;
      if (kr === 1 && kw === 1) return null; // 未启用加权，两个数本来就一样
      const w = this.d.week || {};
      // 用本周期加权值 + 7 天缓存量做量级换算（缓存倍率是折扣的来源）。
      const saved = (w.cache_read_tokens || 0) * (1 - kr) + (w.cache_creation_tokens || 0) * (1 - kw);
      if (saved <= 0) return null;
      const approx = this.d.today.tokens + saved;
      return approx > this.d.today.tokens ? approx : null;
    },
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
    // 缓存命中率（输入侧口径）= 缓存读取 /（缓存读取 + 非缓存输入）。
    // 与用量分析页保持同一口径，避免门户与后台数字对不上引发质疑。
    cacheHitRate() {
      const w = this.d && this.d.week;
      if (!w) return null;
      const read = w.cache_read_tokens || 0;
      const plain = Math.max(0, (w.tokens || 0) - read);
      if (read + plain <= 0) return null;
      return (read / (read + plain)) * 100;
    },
    // 缓存节省展示文案。三种「无数据」必须区分开，否则会被误读：
    //   无缓存用量 → 没有可比较的对象；
    //   有模型未定价 → 不知道省了多少（不是省了 $0）；
    //   确有数据 → 显示金额（负值 = 省钱）。
    cacheSavingsText() {
      const d = this.d;
      if (!d) return "—";
      if (!d.cache_has_usage) return "—";
      if (!d.cache_savings_priced) return "—（部分模型未定价）";
      const v = d.cache_savings || 0;
      if (v === 0) return fmtCost(0);
      // saved 为负 = 省钱；正 = 多付（缓存写入溢价超过读取折扣时可能发生）。
      return (v < 0 ? "省 " : "多付 ") + fmtCost(Math.abs(v));
    },
    cacheSavingsClass() {
      const v = (this.d && this.d.cache_savings) || 0;
      return v < 0 ? "cost" : "";
    },
  },
  mounted() { this.load(); },
  methods: {
    load() {
      this.busy = true;
      req("GET", "/api/portal/overview", null, { key: state.subKey })
        .then((d) => {
          this.d = d;
          // 门户的展示币种跟管理端一致，否则管理员与用户看到的金额对不上。
          if (d.currency) {
            currency.rate = Number(d.currency.rate || 0);
            currency.symbol = d.currency.symbol || "$";
            currency.code = d.currency.code || "USD";
          }
        })
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
        <div class="stat-card" :title="d.quota && (d.quota.cache_read_permille || d.quota.cache_write_permille) ? '这是加权配额计数（与限额同口径），不是上游原始 token——缓存读取与写入已各乘自己的倍率' : '未启用缓存加权，本值等于上游原始 token（prompt+completion）'"><div class="ic ic-blue">⬤</div><div class="body"><div class="v">{{ fmtTokens(d.today.tokens) }}</div><div class="l">{{ periodLabel }} Tokens<span class="stat-sub">额度消耗（加权）</span></div></div></div>
        <div class="stat-card"><div class="ic ic-green">⚡</div><div class="body"><div class="v">{{ d.today.requests }}</div><div class="l">{{ periodLabel }}请求数</div></div></div>
        <div class="stat-card"><div class="ic ic-purple">🖼</div><div class="body"><div class="v">{{ d.today.images }}</div><div class="l">{{ periodLabel }}图像（张）</div></div></div>
        <div class="stat-card"><div class="ic ic-orange">💰</div><div class="body"><div class="v">{{ fmtCost(d.today.cost) }}</div><div class="l">{{ periodLabel }}成本（参考）</div></div></div>
        <div class="stat-card"><div class="ic ic-green">✅</div><div class="body"><div class="v">{{ fmtPct(d.success_rate_7d) }}</div><div class="l">7 天成功率</div></div></div>
        <div class="stat-card"><div class="ic ic-teal">♻</div><div class="body"><div class="v">{{ cacheHitRate === null ? '—' : cacheHitRate.toFixed(1) + '%' }}</div><div class="l">7 天缓存命中占输入</div></div></div>
      </div>

      <div class="card" v-if="d.daily_limit_tokens || d.daily_limit_images || (d.quota && d.quota.limit_requests)">
        <div class="card-head">
          <div class="card-title">{{ periodLabel }}限额</div>
          <div class="card-sub">{{ resetLabel }}</div>
        </div>
        <template v-if="d.daily_limit_tokens">
          <div class="kv"><span class="k">Token 限额（加权）</span><span class="v">{{ fmtTokens(d.today.tokens) }} / {{ fmtTokens(d.daily_limit_tokens) }}</span></div>
          <div class="progress" style="margin:8px 0 14px"><div class="bar" :class="tokenProgress.cls" :style="{width: tokenProgress.pct + '%'}"></div></div>
        </template>
        <template v-if="d.quota && d.quota.limit_requests">
          <div class="kv"><span class="k">请求数限额</span><span class="v">{{ d.today.requests }} / {{ d.quota.limit_requests }}</span></div>
          <div class="progress" style="margin:8px 0 14px"><div class="bar" :class="requestProgress.cls" :style="{width: requestProgress.pct + '%'}"></div></div>
        </template>
        <template v-if="d.daily_limit_images">
          <div class="kv"><span class="k">图像张数限额</span><span class="v">{{ d.today.images }} / {{ d.daily_limit_images }}</span></div>
          <div class="progress" style="margin:8px 0 14px"><div class="bar" :class="imageProgress.cls" :style="{width: imageProgress.pct + '%'}"></div></div>
        </template>
        <p class="hint" v-if="d.daily_limit_tokens">
          <strong>额度按「加权配额」计量，与上方表格的 Tokens 列不是同一个数。</strong>
          上方 Tokens 是上游报告的总量（prompt + completion，含缓存命中）；
          额度消耗把缓存读取与写入各乘自己的倍率后再累加
          <template v-if="d.quota && (d.quota.cache_read_permille || d.quota.cache_write_permille)">
            （当前：读 ×{{ ((d.quota.cache_read_permille || 1000) / 1000) }}、写 ×{{ ((d.quota.cache_write_permille || 1000) / 1000) }}）
          </template>。
          <template v-if="quotaVsRaw">{{ periodLabel }}已用额度 {{ fmtTokens(d.today.tokens) }}，
          对应上游原始 {{ fmtTokens(quotaVsRaw) }} —— 两者相差 {{ (quotaVsRaw / Math.max(1, d.today.tokens)).toFixed(1) }} 倍，
          这是缓存命中带来的折扣，不是统计错误。</template>
        </p>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">累计（自开通以来）与最近 7 天</div></div>
        <div class="table-wrap"><table><thead><tr><th>范围</th><th>请求</th><th>成功</th><th title="上游报告的总 token（prompt+completion），其中缓存命中已计入；不等于额度消耗——额度按缓存倍率加权计量">Tokens<span class="th-sub">上游原始</span></th><th>缓存读取</th><th>缓存写入</th><th>图像</th><th>成本</th></tr></thead><tbody>
          <tr><td>最近 7 天</td><td>{{ d.week.requests }}</td><td>{{ d.week.success }}</td><td>{{ fmtTokens(d.week.tokens) }}</td><td>{{ fmtTokens(d.week.cache_read_tokens || 0) }}</td><td>{{ fmtTokens(d.week.cache_creation_tokens || 0) }}</td><td>{{ d.week.images }}</td><td class="cost">{{ fmtCost(d.week.cost) }}</td></tr>
          <tr><td>累计</td><td>{{ d.total.requests }}</td><td>{{ d.total.success }}</td><td>{{ fmtTokens(d.total.tokens) }}</td><td>{{ fmtTokens(d.total.cache_read_tokens || 0) }}</td><td>{{ fmtTokens(d.total.cache_creation_tokens || 0) }}</td><td>{{ d.total.images }}</td><td class="cost">{{ fmtCost(d.total.cost) }}</td></tr>
        </tbody></table></div>
        <!-- 缓存节省：缓存读通常远低于输入价，命中率直接影响用户实际花费。
             展示节省额让用户知道「把稳定前缀放在请求开头」是有回报的。
             无数据/未定价显示 —，绝不显示 $0（会被读成「没有节省」）。 -->
        <div class="kv" style="padding:0 16px 14px">
          <span class="k">缓存节省（最近 7 天，相对全部未命中）</span>
          <span class="v" :class="cacheSavingsClass">{{ cacheSavingsText }}</span>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">可用模型</div></div>
        <div class="chips"><span class="chip" v-for="m in d.models" :key="m">{{ m }}</span></div>
        <div v-if="!d.models.length" class="empty">暂无可用模型</div>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">最近调用（最多 100 条）</div></div>
        <div class="table-wrap"><table><thead><tr>
          <th>时间</th><th>模型</th><th>模态</th><th title="上游报告的 prompt_tokens，含缓存命中量；不是「非缓存输入」">输入<span class="th-sub">含缓存命中</span></th><th>输出</th><th>图像</th><th>成本</th><th>耗时</th><th>状态</th>
        </tr></thead><tbody>
          <tr v-if="!d.logs.length"><td colspan="9" class="empty">暂无调用记录</td></tr>
          <tr v-for="l in d.logs" :key="l.id">
            <td>{{ fmtTime(l.ts) }}</td>
            <td><span class="mono">{{ l.requested_model || l.model }}</span> <span v-if="l.requested_model && l.requested_model !== l.model" class="tag tag-orange" title="实际模型与请求模型不同（fallback 或虚拟路由）">↓</span></td>
            <td>{{ l.modality === 'image' ? '图像' : '文本' }}</td>
            <td>{{ l.prompt_tokens }}</td><td>{{ l.completion_tokens }}</td>
            <td>{{ l.image_count || '—' }}</td>
            <td class="cost">{{ fmtCost(l.cost) }}</td>
            <td>{{ l.latency_ms }}ms</td>
            <td><span :class="l.status === 'ok' ? 'tag tag-green' : 'tag tag-red'">{{ l.status === 'ok' ? 'OK' : 'ERR' }}</span></td>
          </tr>
        </tbody></table></div>
        <div class="sh-note" style="padding:0 16px 14px">失败原因涉及上游细节，不在此展示；如需排查请联系管理员。</div>
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
  beforeMount() {
    // 计价币种必须在任何页面渲染前拉到：否则先打开「用量分析」再进「设置」的话，
    // 第一屏会按美元显示，进设置页才变成人民币——看起来像数据被改了。
    // 只对管理会话拉（该接口需管理鉴权）；失败静默，保持美元展示。
    if (state.adminToken) {
      req("GET", "/api/settings/runtime").then(applyCurrency).catch(() => {});
    }
  },
  template: `
    <div v-if="prefs.bgUrl" class="app-bg" :style="bgStyle"></div>
    <div v-if="prefs.bgUrl" class="app-bg-shade"></div>
    <transition name="shell" mode="out-in">
      <LoginPage v-if="mode==='login'" @admin="onAdmin" @portal="onPortal"/>
      <AdminShell v-else-if="mode==='admin'" @logout="logoutAdmin"/>
      <PortalPage v-else-if="mode==='portal'" @logout="logoutPortal"/>
      <div v-else class="login-wrap"><div class="login-card sub">加载中…</div></div>
    </transition>
    <transition-group name="toast" tag="div" class="toasts">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="t.ok ? '' : 'err'">{{ t.msg }}</div>
    </transition-group>`,
};

const app = createApp(App);
// 模板表达式只能访问组件实例与全局属性：把工具函数/常量挂到 globalProperties，
// 模板里的 {{ fmtTokens(..) }}、@click="toggleDark"、v-for="o in capOptions" 才可见。
app.config.globalProperties.fmtTokens = fmtTokens;
app.config.globalProperties.fmtTime = fmtTime;
app.config.globalProperties.fmtCost = fmtCost;
app.config.globalProperties.fmtUnitPrice = fmtUnitPrice;
// currencyLabel：表单里「单价（¥ / 1M）」这类标签用；不换算时显示 $。
app.config.globalProperties.currencyLabel = () => currency.symbol + currencySuffix();
// currency 本体挂进 globalProperties，设置页需要读写它。
app.config.globalProperties.currency = currency;
app.config.globalProperties.fmtPct = fmtPct;
app.config.globalProperties.fmtMs = fmtMs;
app.config.globalProperties.fmtInt = fmtInt;
app.config.globalProperties.toggleDark = toggleDark;
app.config.globalProperties.capOptions = capOptions;
app.component("ProbeTest", ProbeTest)
  .component("UiDrawer", UiDrawer)   // ui.js：侧滑抽屉（工作台编辑容器）
  .component("UiSwitch", UiSwitch)   // ui.js：开关
  .component("OverviewPage", OverviewPage)
  .component("UsagePage", UsagePage)
  .component("AccountsPage", AccountsPage)
  .component("ModelsPage", ModelsPage)
  .component("RoutingPage", RoutingPage)
  .component("SubKeysPage", SubKeysPage)
  .component("LogsPage", LogsPage)
  .component("SettingsPage", SettingsPage)
  .mount("#app");
