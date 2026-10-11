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
             sdEmptyFrame, sdAxisLabel, fmtReturnPeriod };
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

// axisOf 复刻 share.js 里 dayAxis 的槽位构造逻辑（含缺日留空），步长固定 86400。
// 第 13 组用例会断言它与 share.js 里的真实实现口径一致，防止两边漂移。
function axisOf(trends) {
  const seen = {};
  trends.forEach((t) => t.buckets.forEach((b) => { seen[b] = 1; }));
  const uniq = Object.keys(seen).map(Number).sort((a, b) => a - b);
  if (!uniq.length) return null;
  const slots = [];
  const last = uniq[uniq.length - 1];
  for (let g = 0; g < 400; g++) {
    const ts = uniq[0] + g * 86400;
    const cur = new Date(ts * 1000);
    slots.push({ ts: ts, label: (cur.getMonth() + 1) + "/" + cur.getDate(), has: !!seen[ts] });
    if (ts >= last) break;
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
  // 校验器里的 axisOf 是 share.js 里 dayAxis 的复刻。若真实实现改成别的口径，
  // 这里会失败——否则会出现「测试全绿但页面仍是旧行为」的假安全感。
  const page = fs.readFileSync(path.join(__dirname, "share.js"), "utf8");
  const i = page.indexOf("dayAxis() {");
  ok(i > 0, "share.js 里 dayAxis 存在");
  const seg = page.slice(i, i + 1800);
  // 核心契约：槽 ts = 首桶 ts + n*86400（沿用后端桶值口径）。
  // 绝不能改回 new Date(y,m,d) 重建——那是导致「图例有子 Key 但零蜡烛」的旧 bug。
  ok(seg.indexOf("uniq[0] + guard * 86400") > 0,
    "dayAxis 用「首桶 ts + n*86400」推进（与后端桶值同源，跨时区才对得上）");
  ok(seg.indexOf("new Date(d.getFullYear(), d.getMonth(), d.getDate())") < 0,
    "dayAxis **不**用 new Date(y,m,d) 重建槽 ts（按浏览器时区重算会与桶值全部错开）");
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

console.log("\n16) 坐标轴刻度不得退化成「1/1/1/0/0」（量级小时仍可读数）");
{
  // 实测发生的 bug：刻度值是 maxV 的四等分，用 fmtInt 四舍五入后
  // maxV=1 时得到 1/1/1/0/0——五个刻度看起来一样，图好像没画出来。
  eq(SD.sdAxisLabel(0), "0", "数值 0 → \"0\"");
  ok(SD.sdAxisLabel(0.25) !== SD.sdAxisLabel(0.5), "0.25 与 0.5 不同（不再被舍入成同一个数）");
  ok(SD.sdAxisLabel(0.5) !== SD.sdAxisLabel(1), "0.5 与 1 不同");
  eq(SD.sdAxisLabel(1824), "1824", "大整数不带小数");
  eq(SD.sdAxisLabel(45.5), "45.5", "中间量级保留一位小数");

  // 端到端：maxV=1 的蜡烛图，五个刻度必须**互不相同**。
  const trends = [mkTrend("A", [[0, 1]])];
  const ax = axisOf(trends);
  const h = SD.renderSDTimeline(trends, [], ax, null);
  const labels = (h.match(/text-anchor="end">([^<]*)</g) || []).map((s) => s.replace(/.*>/, ""));
  eq(labels.length, 5, "Y 轴 5 个刻度");
  eq(new Set(labels).size, 5, "5 个刻度文案互不相同（旧实现会退化成 1/1/1/0/0）");
}

console.log("\n17) 蜡烛宽度随槽宽伸缩（不写死小值）");
{
  // 旧实现上限写死 16px，实测 31 槽时蜡烛仅 13px，叠在网格里几乎看不出实体。
  const wide = [mkTrend("A", [[0, 100], [1, 90]])];
  const narrow = [];
  const day = 86400;
  for (let i = 0; i < 2; i++) narrow.push(mkTrend("A" + i, [[i, 50 + i]]));
  // 构造 31 个连续日的单 Key 序列
  const many = [mkTrend("A", Array.from({ length: 31 }, (_, i) => [30 - i, 100 + i]))];
  const hMany = SD.renderSDTimeline(many, [], axisOf(many), null);
  const wMany = Number((hMany.match(/<rect x="[\d.]+" y="[\d.]+" width="([\d.]+)"/) || [])[1]);
  const hFew = SD.renderSDTimeline(wide, [], axisOf(wide), null);
  const wFew = Number((hFew.match(/<rect x="[\d.]+" y="[\d.]+" width="([\d.]+)"/) || [])[1]);
  ok(wMany > 0 && wFew > 0, "两种密度都取到蜡烛宽度");
  ok(wMany < wFew, "槽多时蜡烛更窄（宽度跟槽宽挂钩），而非固定值");
}

console.log("\n18) 跨时区：槽 ts 必须与后端桶值精确相等（否则零蜡烛）");
{
  // 实测 bug：后端用 time.Local 构造「当日本地 00:00 的 UTC 秒」作为桶值，
  // 而旧 dayAxis 用 new Date(y,m,d) 按**浏览器**时区重建槽 ts。
  // 服务器 UTC+8 / 浏览器 UTC 时每个槽错开 8 小时，全部对不上 →
  // 「图例列出了子 Key、却一根蜡烛都没有」（用户看到的现象）。
  //
  // 锁定行为：槽 ts 序列必须逐字包含全部桶值，且末槽 ts === 尾桶 ts。
  const buckets = [1789142400, 1789228800, 1791475200]; // 9/12, 9/13, 10/9（UTC+8 口径）
  const trends = [{ key: "a", label: "A", buckets: buckets, days: [5, 9, 7] }];
  const ax = axisOf(trends);
  const tsSet = ax.slots.map((s) => s.ts);
  buckets.forEach((b) => ok(tsSet.indexOf(b) >= 0, "桶 " + b + " 在槽序列中（跨时区仍命中）"));
  eq(tsSet[tsSet.length - 1], buckets[buckets.length - 1], "末槽 ts === 尾桶");
  // 命中桶数必须等于真实桶数（缺日只会在中间留空，不会把真数据丢掉）
  eq(ax.slots.filter((s) => s.has).length, buckets.length, "命中桶数 == 真实桶数");
  eq(ax.slots.length, 28, "9/12→10/9 补齐为 28 槽（中间空白日占位）");
}

console.log("\n19) 数据与轴错配时必须显式报错，不得静默画空图");
{
  // 旧行为：错配时画一张「看似正常的空图」——图例照列、网格刻度都在、
  // 零蜡烛、Y 轴还退化成个位数。用户无法区分「真没数据」与「程序对不上」。
  const trends = [{ key: "a", label: "A", buckets: [1000000, 2000000], days: [5, 9] }];
  const alien = { slots: [{ ts: 1, label: "x" }, { ts: 2, label: "y" }], n: 2 };
  const h = SD.renderSDTimeline(trends, [], alien, null);
  ok(h.indexOf("不匹配") > 0, "错配时提示「数据与时间轴不匹配」");
  eq((h.match(/<rect/g) || []).length, 0, "错配时不画任何蜡烛");
  // 对照：正常轴必须真的画出来（防止上面的断言因为「永远不会画」而假通过）
  const good = SD.renderSDTimeline(trends, [], axisOf(trends), null);
  ok((good.match(/<rect/g) || []).length > 0, "正常轴下确实画出蜡烛（对照）");
}

console.log("\n20) 蜡烛实体必须在真实量程内（不得塌在顶部）");
{
  // 实测 bug：实体纵向跨度是 [本槽 total, 上一个有数据槽 prev]，但 maxV 只按
  // s.max 算。当 prev 大于区间内任何 s.max 时 yOf(prev) 被夹到顶部，实体塌成
  // 1.2px 贴在 y=padT，而影线仍按 s.min/s.max 正确定位 → 看起来「柱子跑到线外面」。
  const day = 86400, b0 = 1789142400;
  const mk = (pairs) => ({
    key: "k", label: "K",
    buckets: pairs.map((p) => b0 + p[0] * day),
    days: pairs.map((p) => p[1]),
  });
  // 三日量：首日 1.0K、次日 2.0K（prev 高峰）、末日 1.2K（跌）
  const t = [mk([[0, 1000], [1, 2000], [2, 1200]])];
  const ax = { slots: [0, 1, 2].map((i) => ({ ts: b0 + i * day, label: "d" + i })), n: 3 };
  const h = SD.renderSDTimeline(t, [], ax, null);
  const rects = [...h.matchAll(/<rect x="[\d.]+" y="([\d.]+)" width="[\d.]+" height="([\d.]+)"/g)]
    .map((m) => ({ y: Number(m[1]), h: Number(m[2]) }));
  eq(rects.length, 3, "画出 3 根蜡烛");
  // 第二天 1000→2000 是涨（实体跨 1.0K~2.0K），应明显高于 1.2px
  ok(rects[1].h > 20, "涨日实体高度按真实差值（不被夹成 1.2px），实际 h=" + rects[1].h.toFixed(1));
  // 第三日 2000→1200 是跌，实体底就是 2000（区间最高）→ 实体必须同样可见
  ok(rects[2].h > 20, "跌日实体高度按真实差值，实际 h=" + rects[2].h.toFixed(1));
  // 且跌日实体的顶端应低于涨日（同一量程下 1200 < 2000）
  ok(rects[2].y >= rects[1].y, "跌日实体顶端不低于涨日（同一把尺）");
}

console.log("\n" + (failed ? "✗ " + failed + " 项失败" : "✓ 全部通过") + "（通过 " + passed + " 项）");
process.exit(failed ? 1 : 0);
