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
 * 重构说明：原先的「相邻日比较」蜡烛渲染器（renderSDTimeline）已被
 * **日内 OHLC 真蜡烛**（renderSDCandles）取代并删除，指向它的用例一并移除。
 * 保留的用例都是对**当前实现**仍然成立的契约（跨时区槽 ts 口径、轴/数据错配
 * 必须报错、Y 轴刻度不得退化、图例不撞色等），这些是踩过坑的地方。
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
const PIE_COLORS = ["#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ef4444", "#14b8a6", "#ec4899", "#6366f1", "#f97316", "#06b6d4"];
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
    return { renderSDCandles, renderSDScatter, renderSDBox, renderSDFit,
             sdOhlcTitle, sdEmptyFrame, sdAxisLabel, fmtReturnPeriod,
             colorSlot, groupSlot, legendName, PIE_COLORS };
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

// mkCandle 造一根日内蜡烛：hours 形如 {10:100, 12:300}（本地小时 → 请求数）。
// OHLC 按后端口径从小时序列推出，与 store.ShareCandleSeries 一致。
function mkCandle(bucket, hours) {
  const hourly = new Array(24).fill(0);
  Object.keys(hours).forEach((h) => { hourly[+h] = hours[h]; });
  const nz = hourly.filter((v) => v > 0);
  const fh = hourly.findIndex((v) => v > 0);
  let lh = 23;
  while (lh > 0 && !(hourly[lh] > 0)) lh--;
  return {
    bucket: bucket, hourly: hourly,
    open: hourly[fh], close: hourly[lh],
    high: Math.max.apply(null, nz), low: Math.min.apply(null, nz),
    total: nz.reduce((a, b) => a + b, 0), hour_buckets: nz.length,
  };
}

// mkGroup 造一个子 Key 的蜡烛组。
function mkGroup(key, label, entries) {
  return { key: key, label: label, candles: entries.map((e) => mkCandle(e[0], e[1])) };
}

// axisOf 复刻 share.js 里 dayAxis 的槽位构造逻辑（含缺日留空），步长固定 86400。
// 第 13 组用例会断言它与 share.js 里的真实实现口径一致，防止两边漂移。
function axisOf(candles) {
  const seen = {};
  candles.forEach((c) => { seen[c.bucket] = 1; });
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

console.log("\n1) 防漂移：页面组件的 dayAxis 与本校验器的构造口径一致");
{
  // 校验器里的 axisOf 是 share.js 里 dayAxis 的复刻（源数据从 trends 换成 candles，
  // 口径不变）。若真实实现改成别的口径，这里会失败——否则会出现「测试全绿但页面
  // 仍是旧行为」的假安全感。
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
    "dayAxis **不**按选中集合过滤（轴按全部蜡烛建，清空选中不拆图框）");
  ok(seg.indexOf("const cd = this.cd;") > 0,
    "dayAxis 用蜡烛桶构造槽位（图与轴同源，不漏不重）");
}

console.log("\n2) 依赖边界：图表层不自带全局工具，页面层不重复定义图表函数");
{
  // 拆分后两文件的职责必须干净，否则会出现「同名函数两处定义、加载顺序决定用哪个」
  // 这种极难排查的问题。
  const page = fs.readFileSync(path.join(__dirname, "share.js"), "utf8");
  ["renderSDCandles", "renderSDScatter", "renderSDBox", "renderSDFit"].forEach((fn) => {
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

console.log("\n3) 语法与完整性自检");
{
  // 本文件把 share-chart.js 当**字符串**求值，如果源文件语法坏了，
  // 上面的 new Function 会直接抛错（已有 try/catch 报错并退出）。
  // 这里额外确认关键函数都真的存在（防被误删后仍“恰好”能求值）。
  ["renderSDCandles", "renderSDScatter", "renderSDBox", "renderSDFit", "sdEmptyFrame"].forEach((fn) => {
    ok(typeof SD[fn] === "function", fn + " 可调用");
  });
  // 空图框必须自带 svg 包裹并对成对标签（它现在直接返回整张图）。
  const f = SD.sdEmptyFrame({ slots: [{ ts: 0, label: "1/1" }] }, "msg", "hint");
  ok(f.indexOf("<svg") === 0 && f.indexOf("</svg>") > 0, "sdEmptyFrame 输出完整 svg");
  ok(f.indexOf("msg") > 0 && f.indexOf("hint") > 0, "sdEmptyFrame 带上提示与引导文案");
}

console.log("\n4) 坐标轴刻度不得退化成「1/1/1/0/0」（量级小时仍可读数）");
{
  // 实测发生的 bug：刻度值是 maxV 的四等分，用 fmtInt 四舍五入后
  // maxV=1 时得到 1/1/1/0/0——五个刻度看起来一样，图好像没画出来。
  eq(SD.sdAxisLabel(0), "0", "数值 0 → \"0\"");
  ok(SD.sdAxisLabel(0.25) !== SD.sdAxisLabel(0.5), "0.25 与 0.5 不同（不再被舍入成同一个数）");
  ok(SD.sdAxisLabel(0.5) !== SD.sdAxisLabel(1), "0.5 与 1 不同");
  eq(SD.sdAxisLabel(1824), "1824", "大整数不带小数");
  eq(SD.sdAxisLabel(45.5), "45.5", "中间量级保留一位小数");

  // 端到端：maxV=1 的蜡烛图，五个刻度必须**互不相同**。
  const g = [mkGroup("A", "A", [[dayTs(0), { 5: 1 }]])];
  const ax = axisOf(g[0].candles);
  const h = SD.renderSDCandles(g, [], ax, null);
  const labels = (h.match(/text-anchor="end">([^<]*)</g) || []).map((s) => s.replace(/.*>/, ""));
  eq(labels.length, 5, "Y 轴 5 个刻度");
  eq(new Set(labels).size, 5, "5 个刻度文案互不相同（旧实现会退化成 1/1/1/0/0）");
}

console.log("\n5) 蜡烛宽度随槽宽伸缩（不写死小值）");
{
  // 旧实现上限写死 16px，实测 31 槽时蜡烛仅 13px，叠在网格里几乎看不出实体。
  const b0 = dayTs(30);
  const wide = [mkGroup("A", "A", [[b0, { 12: 100 }], [b0 + 86400, { 12: 90 }]])];
  // 构造 31 个连续日的单 Key 序列
  const many = [mkGroup("A", "A", Array.from({ length: 31 }, (_, i) => [b0 + i * 86400, { 12: 100 + i }]))];
  const hMany = SD.renderSDCandles(many, [], axisOf(many[0].candles), null);
  const wMany = Number((hMany.match(/<rect x="[\d.]+" y="[\d.]+" width="([\d.]+)"/) || [])[1]);
  const hFew = SD.renderSDCandles(wide, [], axisOf(wide[0].candles), null);
  const wFew = Number((hFew.match(/<rect x="[\d.]+" y="[\d.]+" width="([\d.]+)"/) || [])[1]);
  ok(wMany > 0 && wFew > 0, "两种密度都取到蜡烛宽度");
  ok(wMany < wFew, "槽多时蜡烛更窄（宽度跟槽宽挂钩），而非固定值");
}

console.log("\n6) 跨时区：槽 ts 必须与后端桶值精确相等（否则零蜡烛）");
{
  // 实测 bug：后端用 time.Local 构造「当日本地 00:00 的 UTC 秒」作为桶值，
  // 而旧 dayAxis 用 new Date(y,m,d) 按**浏览器**时区重建槽 ts。
  // 服务器 UTC+8 / 浏览器 UTC 时每个槽错开 8 小时，全部对不上 →
  // 「图例列出了子 Key、却一根蜡烛都没有」（用户看到的现象）。
  //
  // 锁定行为：槽 ts 序列必须逐字包含全部桶值，且末槽 ts === 尾桶 ts。
  const buckets = [1789142400, 1789228800, 1791475200]; // 9/12, 9/13, 10/9（UTC+8 口径）
  const g = [mkGroup("a", "A", buckets.map((b) => [b, { 12: 5 }]))];
  const ax = axisOf(g[0].candles);
  const tsSet = ax.slots.map((s) => s.ts);
  buckets.forEach((b) => ok(tsSet.indexOf(b) >= 0, "桶 " + b + " 在槽序列中（跨时区仍命中）"));
  eq(tsSet[tsSet.length - 1], buckets[buckets.length - 1], "末槽 ts === 尾桶");
  // 命中桶数必须等于真实桶数（缺日只会在中间留空，不会把真数据丢掉）
  eq(ax.slots.filter((s) => s.has).length, buckets.length, "命中桶数 == 真实桶数");
  eq(ax.slots.length, 28, "9/12→10/9 补齐为 28 槽（中间空白日占位）");
}

console.log("\n7) 数据与轴错配时必须显式报错，不得静默画空图");
{
  // 旧行为：错配时画一张「看似正常的空图」——图例照列、网格刻度都在、
  // 零蜡烛、Y 轴还退化成个位数。用户无法区分「真没数据」与「程序对不上」。
  const g = [mkGroup("a", "A", [[1000000, { 3: 5 }], [2000000, { 3: 9 }]])];
  const alien = { slots: [{ ts: 1, label: "x" }, { ts: 2, label: "y" }], n: 2 };
  const h = SD.renderSDCandles(g, [], alien, null);
  ok(h.indexOf("不匹配") > 0, "错配时提示「数据与时间轴不匹配」");
  eq((h.match(/<rect/g) || []).length, 0, "错配时不画任何蜡烛");
  // 对照：正常轴必须真的画出来（防止上面的断言因为「永远不会画」而假通过）
  const good = SD.renderSDCandles(g, [], axisOf(g[0].candles), null);
  ok((good.match(/<rect/g) || []).length > 0, "正常轴下确实画出蜡烛（对照）");
}

console.log("\n8) 图例颜色不得撞色、重名子 Key 必须可区分");
{
  // 实测 bug：颜色用 hashIdx(label, n) 取（djb2 对短名字聚集严重），
  // 库里 7 个子 Key 中有 3 个（「三重奏」/「dev」/「bot」）拿到同一颜色，
  // 两个同名「test」完全无法区分——图例失去意义。
  const mk = (key, label) => ({ key, label, buckets: [1789142400], days: [10] });
  const picked = [
    mk("sk_22df8a1ea014", "test"), mk("sk_31b860000001", "三重奏"),
    mk("sk_5e74e0000002", "dev"), mk("sk_5f8210000003", "bot"),
    mk("sk_6825b0000004", "cat"), mk("sk_6ee1b0000005", "zcode自用"),
    mk("sk_70e2939c491a", "test"),
  ];
  const cols = picked.map((t) => SD.PIE_COLORS[SD.colorSlot(t.key, picked)]);
  eq(new Set(cols).size, picked.length, "每个子 Key 颜色互不相同（实测旧版仅 4 种）");

  // 重名：两个 test 必须能分辨
  const n1 = SD.legendName(picked[0], picked), n2 = SD.legendName(picked[6], picked);
  ok(n1 !== n2, "同名子 Key 的图例名不同（" + n1 + " vs " + n2 + "）");
  // 不重名的项不加后缀（不加噪音）
  eq(SD.legendName(picked[1], picked), "三重奏", "不重名时不加 key 后缀");
}

console.log("\n9) 真蜡烛（日内 OHLC）：影线必包实体、多选逐小时相加");
{
  // 每组：某子 Key 的若干天，每天给一个 hourly(24) 序列
  const mkG = (key, label, days) => ({
    key, label,
    candles: days.map((d) => {
      const hourly = new Array(24).fill(0);
      Object.keys(d.hours).forEach((h) => { hourly[+h] = d.hours[h]; });
      const nz = hourly.filter((v) => v > 0);
      const fh = hourly.findIndex((v) => v > 0);
      let lh = 23; while (lh > 0 && !(hourly[lh] > 0)) lh--;
      return {
        key, label, bucket: d.bucket, hourly,
        open: hourly[fh], close: hourly[lh],
        high: Math.max.apply(null, nz), low: Math.min.apply(null, nz),
        total: nz.reduce((a, b) => a + b, 0), hour_buckets: nz.length,
      };
    }),
  });
  const B0 = 1789142400, day = 86400;
  const ax = {
    slots: [0, 1, 2].map((i) => ({ ts: B0 + i * day, label: "d" + i, has: true })), n: 3,
  };

  // 单选：OHLC 就是该 Key 的真实值
  const one = [mkG("k1", "A", [
    { bucket: B0, hours: { 10: 100, 12: 300, 20: 50 } },
    { bucket: B0 + day, hours: { 9: 400, 11: 200 } },
  ])];
  const h1 = SD.renderSDCandles(one, [], ax, null);
  ok(h1.indexOf("<svg") === 0, "输出 svg");
  eq((h1.match(/<rect/g) || []).length, 2, "两根蜡烛");

  // 多选：两 Key 的峰值在不同小时 → 必须先逐小时相加再取高/低
  const a = mkG("ka", "A", [{ bucket: B0, hours: { 10: 100, 12: 1 } }]);
  const b = mkG("kb", "B", [{ bucket: B0, hours: { 10: 1, 12: 100 } }]);
  const h2 = SD.renderSDCandles([a, b], [], ax.slice ? ax : ax, null);
  // 逐小时相加: 10时101, 12时101 → 高=低=101（而不是 ΣHigh=200）
  ok(h2.indexOf("高 101") > 0, "多选时逐小时相加（高=101 而非 200），实际含：" +
    (h2.match(/高 [\d,]+/) || [""])[0]);

  // 不变量：每根蜡烛必须 low <= open,close <= high（影线包住实体）
  const real = [mkG("kz", "Z", [
    { bucket: B0, hours: { 3: 5, 4: 900, 5: 7 } },
    { bucket: B0 + day, hours: { 1: 1, 2: 2 } },
    { bucket: B0 + 2 * day, hours: { 22: 800, 23: 10 } },
  ])];
  const h3 = SD.renderSDCandles(real, [], ax, null);
  const rects = [...h3.matchAll(/<rect x="[\d.]+" y="([\d.]+)" width="[\d.]+" height="([\d.]+)"/g)]
    .map((m) => ({ y: +m[1], h: +m[2] }));
  const wicks = [...h3.matchAll(/<line x1="[\d.]+" y1="([\d.]+)" x2="[\d.]+" y2="([\d.]+)" stroke="#(?:dc2626|10b981|86909c)"/g)]
    .map((m) => ({ y1: +m[1], y2: +m[2] }));
  eq(rects.length, 3, "三根实体");
  eq(wicks.length, 3, "三根影线");
  let okb = true;
  for (let i = 0; i < 3; i++) {
    // SVG y 向下：影线上端(y1) 必须 <= 实体顶端(y)，下端(y2) 必须 >= 实体底端(y+h)
    if (!(wicks[i].y1 <= rects[i].y + 0.01 && wicks[i].y2 >= rects[i].y + rects[i].h - 0.01)) okb = false;
  }
  ok(okb, "影线包住实体（不再低垂到蜡烛脚下）");

  // 清空选中：保留图框而不是整块消失
  const h4 = SD.renderSDCandles(real, [], ax, []);
  ok(h4.indexOf("<svg") === 0 && h4.indexOf("未选择") > 0, "清空选中时保留图框 + 提示");
}

console.log("\n" + (failed ? "✗ " + failed + " 项失败" : "✓ 全部通过") + "（通过 " + passed + " 项）");
process.exit(failed ? 1 : 0);
