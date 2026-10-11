/* ArkGate 分发检测图表 · 离线校验（用法：node web/share-chart.test.js）
 *
 * 位置说明：本文件与它校验的对象 web/share-chart.js 放在一起（而不是 tools/），
 * 因为它是**前端渲染层的一部分**——改图表就要跑它，两者必须一起被看到、
 * 一起进版本控制。`tools/` 是离线分析产物目录（已在 .gitignore 排除），
 * 校验器放那里会被当成一次性脚本而随目录一起被忽略。
 *
 * 为什么需要它：web/share-chart.js 的函数是纯函数（输入数据 → 输出 SVG 字符串），
 * 但项目**没有前端构建链也没有前端测试框架**，图表改动只能靠肉眼看图——
 * 而「按下标取值」这类错误在只有一两条数据的截图里根本看不出来。
 * 实测确实写错过：每个子 Key 的 buckets 是各自稀疏的（只含「有使用的日」），
 * 同一槽下标在不同子 Key 上对应不同日期，按下标取值会把不同日期的值
 * 混进同一根蜡烛。
 *
 * 做法：直接 **eval web/share-chart.js**（它是经典 script，顶层 function 声明
 * 在一个 function 作用域里执行后会挂到该作用域上），取出需要的函数断言输出。
 * 不依赖打包器，也不改动源文件。
 *
 * 运行：仓库根目录执行 `node web/share-chart.test.js`（退出码非 0 = 失败）。
 */
"use strict";
const fs = require("fs");
const path = require("path");

const SRC = path.join(__dirname, "share-chart.js");
const src = fs.readFileSync(SRC, "utf8");

// share-chart.js 依赖的全局工具（由 app.js 提供）。这里以桩件注入——
// 它们只影响展示格式（千分位、颜色、转义），与图表逻辑无关。
const PRELUDE = `
const PIE_COLORS = ["#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ef4444"];
function fmtInt(v) { return String(Math.round(v)); }
function fmtTokens(v) { return String(Math.round(v)); }
function fmtCost(v) { return "$" + Number(v).toFixed(2); }
function hashIdx(name, n) { let h = 5381; for (let i = 0; i < name.length; i++) h = ((h << 5) + h + name.charCodeAt(i)) | 0; return Math.abs(h) % n; }
function escHtml(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }
function truncText(s, n) { const r = Array.from(String(s || "")); return r.length > n ? r.slice(0, n).join("") + "…" : r.join(""); }
`;

let SD;
try {
  SD = new Function(PRELUDE + src + `
    return { renderSDTimeline, renderSDScatter, renderSDBox, renderSDFit,
             sdIsAll, sdFilterTrends, sdSlotStats, sdTrendIndex, sdCandleTitle,
             sdEmptyFrame, fmtReturnPeriod };
  `)();
} catch (e) {
  console.error("✗ share-chart.js 无法求值（语法错误？）：" + e.message);
  process.exit(1);
}

// ── 断言工具 ──
let failed = 0, passed = 0;
function ok(cond, msg) {
  if (cond) { passed++; console.log("  ✓ " + msg); }
  else { failed++; console.log("  ✗ " + msg); }
}
function eq(a, b, msg) { ok(a === b, msg + "（期望 " + JSON.stringify(b) + "，实际 " + JSON.stringify(a) + "）"); }

// dayTs 生成「本地日 00:00 的 UTC 秒」（与后端 usageBucketExpr 的口径一致）。
function dayTs(offsetDays) {
  const d = new Date();
  return Math.floor(new Date(d.getFullYear(), d.getMonth(), d.getDate() - offsetDays).getTime() / 1000);
}

// mkTrend 造一个子 Key 的日序列：[天数偏移, 值] → buckets/days。
function mkTrend(key, pairs) {
  return {
    key: key, label: key,
    buckets: pairs.map((p) => dayTs(p[0])),
    days: pairs.map((p) => p[1]),
  };
}

// axisOf 复刻页面组件 dayAxis 的槽位构造逻辑（含缺日留空）。
// 第 13 组用例会断言它与 share.js 里的真实实现口径一致，防止两边漂移。
function axisOf(trends) {
  const seen = {};
  trends.forEach((t) => t.buckets.forEach((b) => { seen[b] = 1; }));
  const uniq = Object.keys(seen).map(Number).sort((a, b) => a - b);
  if (!uniq.length) return null;
  const slots = [];
  const last = uniq[uniq.length - 1];
  const d = new Date(uniq[0] * 1000);
  let cur = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  for (let g = 0; g < 400; g++) {
    const ts = Math.floor(cur.getTime() / 1000);
    slots.push({ ts: ts, label: (cur.getMonth() + 1) + "/" + cur.getDate(), has: !!seen[ts] });
    if (ts >= last) break;
    cur = new Date(cur.getFullYear(), cur.getMonth(), cur.getDate() + 1);
  }
  return { min: uniq[0], max_t: last, n: slots.length, slots: slots };
}

// ═══════════════ 用例 ═══════════════

console.log("\n1) 轴槽位：缺日留空槽，且槽序列连续");
{
  // A 在 3 天前/今天有量，B 在 2 天前有量 → 轴应为 4 个连续槽。
  const trends = [mkTrend("A", [[3, 100], [0, 400]]), mkTrend("B", [[2, 200]])];
  const ax = axisOf(trends);
  eq(ax.n, 4, "槽数 = 跨 4 个自然日");
  // 槽序：0=3天前(A) 1=2天前(B) 2=1天前(空) 3=今天(A)
  eq(ax.slots[0].has, true, "槽 0（3 天前）命中 A");
  eq(ax.slots[1].has, true, "槽 1（2 天前）命中 B");
  eq(ax.slots[2].has, false, "槽 2（无人使用的一天）has=false，仍占位");
  eq(ax.slots[3].has, true, "末槽命中");
}

console.log("\n2) 稀疏 buckets 必须按时间戳对齐（按下标取值会串日期）");
{
  const A = mkTrend("A", [[2, 111]]);
  const B = mkTrend("B", [[0, 999]]);
  const trends = [A, B];
  const ax = axisOf(trends);
  const idx = SD.sdTrendIndex(trends, ax.slots);
  const s0 = SD.sdSlotStats(trends, idx, 0);
  const s2 = SD.sdSlotStats(trends, idx, 2);
  eq(s0.total, 111, "槽 0（2 天前）只有 A 的 111");
  eq(s2.total, 999, "槽 2（今天）只有 B 的 999，未被 A 的下标错位污染");
  eq(s0.items[0].label, "A", "槽 0 的明细归属 A");
  eq(s2.items[0].label, "B", "槽 2 的明细归属 B");
}

console.log("\n3) 空槽返回 null（不补零，不画蜡烛）");
{
  const trends = [mkTrend("A", [[2, 10], [0, 20]])];
  const ax = axisOf(trends);
  const idx = SD.sdTrendIndex(trends, ax.slots);
  eq(SD.sdSlotStats(trends, idx, 1), null, "中间空槽 = null");
}

console.log("\n4) 合计与 min/max 口径");
{
  const trends = [mkTrend("A", [[0, 100]]), mkTrend("B", [[0, 300]]), mkTrend("C", [[0, 200]])];
  const ax = axisOf(trends);
  const idx = SD.sdTrendIndex(trends, ax.slots);
  const s = SD.sdSlotStats(trends, idx, 0);
  eq(s.total, 600, "合计 = 100+300+200");
  eq(s.min, 100, "min = 100");
  eq(s.max, 300, "max = 300");
  eq(s.subkeys, 3, "命中 3 个子 Key");
  eq(s.items[0].label, "B", "明细按值降序，首项是最大的 B");
}

console.log("\n5) 选中过滤：单选 / 空选 / 全选");
{
  const trends = [mkTrend("A", [[0, 100]]), mkTrend("B", [[0, 300]]), mkTrend("C", [[0, 200]])];
  const ax = axisOf(trends);
  eq(SD.sdFilterTrends(trends, null).length, 3, "null = 全选");
  eq(SD.sdFilterTrends(trends, []).length, 0, "空数组 = 一个都不选");
  eq(SD.sdFilterTrends(trends, ["B"]).length, 1, "单选 B");
  ok(SD.sdIsAll(null, trends), "null 判定为全选");
  ok(SD.sdIsAll(["A", "B", "C"], trends), "覆盖全部 → 等价全选");
  ok(!SD.sdIsAll(["A", "B"], trends), "少一个 → 非全选");

  const picked = SD.sdFilterTrends(trends, ["B"]);
  const idx = SD.sdTrendIndex(picked, ax.slots);
  const s = SD.sdSlotStats(picked, idx, 0);
  eq(s.total, 300, "单选 B 后合计 = 300（不再含 A/C）");
  eq(s.subkeys, 1, "单选 B 命中 1 个");
}

console.log("\n6) 蜡烛实体：相邻有数据的槽比较，空档不参与");
{
  // A 在 4 天前=100、2 天前=100（3 天前空）、今天=400。
  // 今天应与「2 天前」比较（涨 300），而不是与空槽比较。
  const trends = [mkTrend("A", [[4, 100], [2, 100], [0, 400]])];
  const ax = axisOf(trends);
  const idx = SD.sdTrendIndex(trends, ax.slots);
  let prev = null;
  const seq = [];
  for (let i = 0; i < ax.slots.length; i++) {
    const s = SD.sdSlotStats(trends, idx, i);
    if (!s) { seq.push(null); continue; }
    seq.push({ top: s.total, bot: prev === null ? s.total : prev });
    prev = s.total;
  }
  // 槽序：0=4天前 1=3天前(空) 2=2天前 3=1天前(空) 4=今天
  eq(seq[1], null, "3 天前的空档不画");
  eq(seq[4].bot, 100, "今天的实体底 = 上一个有数据日（2 天前）的 100，跳过空档");
  eq(seq[4].top, 400, "今天的实体顶 = 400");
}

console.log("\n7) SVG 输出：多选不画散点（改为体内分布线），单选画点");
{
  const trends = [mkTrend("A", [[0, 100]]), mkTrend("B", [[0, 300]])];
  const ax = axisOf(trends);
  const multi = SD.renderSDTimeline(trends, [], ax, null);
  ok(multi.indexOf("<svg") === 0, "多选输出 SVG");
  eq((multi.match(/<circle/g) || []).length, 0, "多选：无散点圆");
  ok(multi.indexOf("<rect") > 0, "多选：有蜡烛实体");
  ok(multi.indexOf("体内横线") > 0, "多选：图例说明体内横线");
  // 实体必须是不透明色块：半透明在毛玻璃背景/深色主题下会被稀成「空心框」，
  // 看起来就像图没画出来（实测截图确认）。
  ok(!/fill-opacity/.test(multi), "实体不使用 fill-opacity（避免被背景稀成空心）");

  const single = SD.renderSDTimeline(trends, [], ax, ["A"]);
  eq((single.match(/<circle/g) || []).length, 1, "单选：该槽画 1 个点");
  ok(single.indexOf("右侧点") > 0, "单选：图例说明右侧点");
}

console.log("\n8) 空选中集合保留图框（不再整块消失），且文案可操作");
{
  const trends = [mkTrend("A", [[0, 100]]), mkTrend("B", [[1, 50]])];
  const ax = axisOf(trends);
  const none = SD.renderSDTimeline(trends, [], ax, []);
  // 早期版本 return 一句 .empty 文字，页面上表现为「图为空」（实测反馈）。
  // 现在必须仍是一张完整的图：有 svg、有网格、有日期刻度。
  ok(none.indexOf("<svg") === 0, "空选仍输出 SVG 图框（不是一句文字）");
  ok(none.indexOf("未选择任何子 Key") > 0, "图内提示「未选择任何子 Key」");
  ok(none.indexOf("勾选") > 0, "提示告知如何恢复（可操作）");
  eq((none.match(/<line/g) || []).length >= 5, true, "网格线仍在（图框完整）");
  ok(none.indexOf("/") > 0, "X 轴日期刻度仍在（区间有效的可见证据）");

  // 真无数据（区间本身没数据）也不该只剩一行字。
  const noData = SD.renderSDTimeline([], [], ax, null);
  ok(noData.indexOf("<svg") === 0, "区间无数据也保留图框");
  ok(noData.indexOf("所选区间暂无数据") > 0, "图内提示「所选区间暂无数据」");
}

console.log("\n9) 单槽（区间只有一天）不除零、不崩");
{
  const trends = [mkTrend("A", [[0, 42]])];
  const ax = axisOf(trends);
  eq(ax.n, 1, "只有一个槽");
  const html = SD.renderSDTimeline(trends, [], ax, null);
  ok(html.indexOf("<svg") === 0, "单槽仍输出合法 SVG");
  ok(!/NaN|Infinity/.test(html), "输出不含 NaN/Infinity");
}

console.log("\n10) 大量子 Key 不产生 NaN，且尺寸稳定");
{
  const trends = [];
  for (let i = 0; i < 30; i++) trends.push(mkTrend("K" + i, [[i % 5, 100 + i * 7], [0, 200 + i * 3]]));
  const ax = axisOf(trends);
  const html = SD.renderSDTimeline(trends, [], ax, null);
  ok(html.indexOf("<svg") === 0, "30 个子 Key 输出合法 SVG");
  ok(!/NaN|Infinity/.test(html), "30 个子 Key 无 NaN/Infinity");
  const w = (html.match(/<rect x="([-\d.]+)"/) || [])[1];
  ok(Number(w) > 0, "蜡烛 x 坐标为有效正数（实体宽度未溢出槽宽）");
}

console.log("\n11) tooltip 含合计/涨跌/区间/明细");
{
  const slot = { ts: 0, label: "10/1" };
  const s = { total: 600, min: 100, max: 300, subkeys: 2, items: [{ label: "B", v: 300 }, { label: "A", v: 300 }] };
  const t = SD.sdCandleTitle(slot, s, 400, false);
  ok(t.indexOf("10/1") === 0, "首行是日期");
  ok(t.indexOf("合计 600") > 0, "含合计");
  ok(t.indexOf("+200") > 0, "含相对上一有数据日的涨跌");
  ok(t.indexOf("100 ~ 300") > 0, "含区间 min~max");
  ok(t.indexOf("B 300") > 0, "含子 Key 明细");
  ok(SD.sdCandleTitle(slot, s, null, false).indexOf("首个有数据日") > 0,
    "无比较基准时明确说明，而不是显示「+600」");
}

console.log("\n12) 首日无基准时按持平处理（实体极小但存在，不画成暴涨）");
{
  const trends = [mkTrend("A", [[2, 500], [0, 900]])];
  const ax = axisOf(trends);
  const html = SD.renderSDTimeline(trends, [], ax, null);
  ok(html.indexOf("#86909c") > 0, "首日使用持平色（灰），不是红色暴涨");
}

console.log("\n13) 防漂移：页面组件的 dayAxis 与本校验器的构造口径一致");
{
  // 校验器里的 axisOf 是 share.js 里 dayAxis 的复刻。若真实实现改成别的口径
  // （比如按 ts+86400 推进，那会跨 DST 算错日界线），这里会失败——否则会出现
  // 「测试全绿但页面仍是旧行为」的假安全感。
  const page = fs.readFileSync(path.join(__dirname, "share.js"), "utf8");
  const i = page.indexOf("dayAxis() {");
  ok(i > 0, "share.js 里 dayAxis 存在");
  const seg = page.slice(i, i + 1600);
  ok(seg.indexOf("cur.getFullYear(), cur.getMonth(), cur.getDate() + 1") > 0,
    "dayAxis 用「本地 00:00 + 日号+1」推进（DST 安全），不是 ts+86400");
  ok(seg.indexOf("sdFilterTrends(this.trends, this.sel)") < 0,
    "dayAxis **不**按选中集合过滤（轴按全部 trends 建，清空选中不拆图框）");
  ok(seg.indexOf("const trends = this.trends;") > 0,
    "dayAxis 用全部 trends 构造槽位");
}

console.log("\n14) 依赖边界：图表层不自带全局工具，页面层不重复定义图表函数");
{
  // 拆分后两文件的职责必须干净，否则会出现「同名函数两处定义、加载顺序决定用哪个」
  // 这种极难排查的问题。
  const page = fs.readFileSync(path.join(__dirname, "share.js"), "utf8");
  ["renderSDTimeline", "renderSDScatter", "renderSDBox", "renderSDFit"].forEach((fn) => {
    ok(src.indexOf("function " + fn + "(") > 0, "share-chart.js 定义 " + fn);
    ok(page.indexOf("function " + fn + "(") < 0, "share.js 不重复定义 " + fn);
  });
  // 图表层所需的全局工具应由 app.js 提供，chart 层自己不该再定义一遍。
  ["function fmtInt", "function escHtml", "function hashIdx"].forEach((d) => {
    ok(src.indexOf(d) < 0, "share-chart.js 不自己定义 " + d.replace("function ", "") + "（依赖 app.js）");
  });
  const idx = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  const pChart = idx.indexOf("/share-chart.js");
  const pPage = idx.indexOf("/share.js");
  ok(pChart > 0 && pPage > 0, "index.html 同时加载两个文件");
  ok(pChart < pPage, "share-chart.js 先于 share.js 加载（后者依赖前者）");
}

console.log("\n15) 语法与完整性自检");
{
  // 本文件把 share-chart.js 当**字符串**求值，如果源文件语法坏了，
  // 上面的 new Function 会直接抛错（已有 try/catch 报错并退出）。
  // 这里额外确认关键函数都真的存在（防被误删后仍“恰好”能求值）。
  ["renderSDTimeline", "renderSDScatter", "renderSDBox", "renderSDFit", "sdEmptyFrame"].forEach((fn) => {
    ok(typeof SD[fn] === "function", fn + " 可调用");
  });
  // 空图框必须自带 svg 包裹并对成对标签（它现在直接返回整张图）。
  const f = SD.sdEmptyFrame({ slots: [{ ts: 0, label: "1/1" }] }, "msg", "hint");
  ok(f.indexOf("<svg") === 0 && f.indexOf("</svg>") > 0, "sdEmptyFrame 输出完整 svg");
  ok(f.indexOf("msg") > 0 && f.indexOf("hint") > 0, "sdEmptyFrame 带上提示与引导文案");
}

console.log("\n" + (failed ? "✗ " + failed + " 项失败" : "✓ 全部通过") + "（通过 " + passed + " 项）");
process.exit(failed ? 1 : 0);
