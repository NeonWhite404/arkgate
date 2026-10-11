/* ArkGate 分发检测 · 图表渲染层（纯函数，无 DOM/Vue 依赖）
 *
 * 本文件是 `share.js`（页面组件）的渲染依赖，必须先于它加载
 * （index.html 中的 script 顺序：ui.js → models.js → share-chart.js → share.js → app.js）。
 *
 * 为什么单独成文件：这些函数**输入数据、输出 SVG 字符串**，是纯函数——
 * 拆出来后可以脱离浏览器直接校验（见 web/share-cart.test.js），
 * 而页面组件（状态、参数抽屉、表格）留在 share.js 里，两者变更节奏不同。
 *
 * 图表总览：
 *   1. renderSDScatter  极值判据散点（主判据：返回期 × 波动 CV）
 *   2. renderSDBox      量级对比箱线
 *   3. renderSDTimeline 日用量蜡烛图（本文件的核心，见下方大段注释）
 *   4. renderSDFit      分布拟合（GPD 外推）
 *
 * 依赖的全局工具（app.js 提供，本文件不自己定义）：
 *   fmtInt / fmtTokens / escHtml / hashIdx / PIE_COLORS / truncText / fmtCost
 * ——这些是**全局约定**，与 share.js 对它们的使用方式一致（经典 script，
 * 顶层 const 不上 window，跨文件按名引用）。
 */
"use strict";

// ── 图表 1（主判据）：极值散点 —— 返回期 × 波动 CV ──
//
// 这是文档里的「图 0」，也是本页唯一带**判定语义**的图：右上角红色区
// （返回期 > 阈值 且 CV < 显著平稳线）= 显著异常区。横轴对数刻度——
// 返回期跨好几个数量级，线性刻度会把所有正常组挤在左边缘。
function renderSDScatter(groups, fit, cvf, params) {
  if (!groups || !groups.length) return '<div class="empty">所选区间暂无数据</div>';
  const W = 820, H = 340, padL = 64, padR = 132, padT = 34, padB = 46;
  const plotW = W - padL - padR, plotH = H - padT - padB;

  // 横轴范围：至少覆盖判据阈值，并留出到最大返回期的空间。
  const critRP = params.return_period_days || 1000;
  let maxRP = critRP * 4;
  for (const g of groups) {
    if (g.return_period_days > maxRP) maxRP = g.return_period_days;
  }
  maxRP *= 1.6;
  const lx = (v) => {
    const lg = Math.log10(Math.max(v, 1));
    return padL + (lg / Math.log10(maxRP)) * plotW;
  };
  // 纵轴范围：覆盖最大 CV 与显著线。
  let maxCV = 0.2;
  for (const g of groups) if (g.cv > maxCV) maxCV = g.cv;
  if (cvf && cvf.valid && cvf.crit_cv > maxCV) maxCV = cvf.crit_cv;
  maxCV *= 1.15;
  const yOf = (v) => padT + plotH * (1 - Math.min(v, maxCV) / maxCV);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';

  // 危险区（右上）：返回期超阈值 且 CV 低于显著平稳线。
  const xc = lx(critRP);
  const critCV = cvf && cvf.valid ? cvf.crit_cv : 0;
  const yc = critCV > 0 ? yOf(critCV) : padT;
  if (critCV > 0) {
    svg += '<rect x="' + xc.toFixed(1) + '" y="' + padT + '" width="' + (W - padR - xc).toFixed(1) +
      '" height="' + (yc - padT).toFixed(1) + '" fill="rgba(185,28,28,.09)"/>';
  }

  // 网格：横轴按 10 的幂，纵轴 4 等分。
  const powLabels = [];
  for (let p = 0; p <= 12; p++) {
    const v = Math.pow(10, p);
    if (v > maxRP) break;
    powLabels.push(v);
  }
  powLabels.forEach((v) => {
    const x = lx(v);
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + padT + '" x2="' + x.toFixed(1) + '" y2="' + (H - padB) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + x.toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' + fmtRPTick(v) + '</text>';
  });
  for (let i = 0; i <= 4; i++) {
    const v = maxCV * (i / 4);
    const y = yOf(v);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" font-size="9" fill="#86909c" text-anchor="end">' + v.toFixed(2) + '</text>';
  }

  // 判定线。
  svg += '<line x1="' + xc.toFixed(1) + '" y1="' + padT + '" x2="' + xc.toFixed(1) + '" y2="' + (H - padB) +
    '" stroke="#b91c1c" stroke-width="1.4" stroke-dasharray="4 3"/>';
  svg += '<text x="' + xc.toFixed(1) + '" y="' + (padT - 6) + '" font-size="10" fill="#b91c1c" text-anchor="middle">' +
    fmtInt(critRP) + ' 天</text>';
  if (critCV > 0) {
    svg += '<line x1="' + padL + '" y1="' + yc.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + yc.toFixed(1) +
      '" stroke="#b91c1c" stroke-width="1.4" stroke-dasharray="4 3"/>';
    svg += '<text x="' + (padL + 4) + '" y="' + (yc - 5).toFixed(1) + '" font-size="10" fill="#b91c1c">显著平稳线 CV=' +
      critCV.toFixed(2) + '</text>';
  }

  // 点：flag 决定半径与光晕，颜色按名称哈希取稳定调色板（刷新不跳变）。
  groups.forEach((g) => {
    let x = lx(g.return_period_days);
    if (x > W - padR) x = W - padR;
    const y = yOf(g.cv);
    const flagged = g.flag === "flagged";
    const color = flagged ? "#b91c1c" : PIE_COLORS[hashIdx(String(g.label || g.key), PIE_COLORS.length)];
    if (flagged) {
      svg += '<circle cx="' + x.toFixed(1) + '" cy="' + y.toFixed(1) + '" r="14" fill="#b91c1c" fill-opacity=".16"/>';
    }
    svg += '<circle cx="' + x.toFixed(1) + '" cy="' + y.toFixed(1) + '" r="' + (flagged ? 7.5 : 5) + '" fill="' + color +
      '" fill-opacity=".88"><title>' + escHtml(sdTitle(g)) + '</title></circle>';
    // 只给命中判据的点标签（全部标注会糊成一团，而没命中的看表格即可）。
    if (flagged) {
      svg += '<text x="' + (x + 11).toFixed(1) + '" y="' + (y + 4).toFixed(1) + '" font-size="11" fill="' + color + '">' +
        escHtml(truncText(g.label || g.key, 12)) + ' ★</text>';
    }
  });

  svg += '<text x="' + (padL + plotW / 2).toFixed(1) + '" y="' + (H - 8) + '" font-size="11" fill="#6b7280" text-anchor="middle">返回期（天，对数刻度；平均多少天才出现一次该量级的日用量）→</text>';
  svg += '<text x="14" y="' + (padT + plotH / 2).toFixed(1) + '" font-size="11" fill="#6b7280" text-anchor="middle" transform="rotate(-90 14 ' +
    (padT + plotH / 2).toFixed(1) + ')">日用量波动 CV（越小越平稳）→</text>';
  if (critCV > 0) {
    svg += '<text x="' + ((xc + W - padR) / 2).toFixed(1) + '" y="' + (padT + 14) + '" font-size="11" fill="#b91c1c" text-anchor="middle">右上 = 显著异常区</text>';
  }
  svg += "</svg>";
  return svg;
}

// sdTitle 散点/趋势图共用：一个点的完整读数 + 判读依据。
function sdTitle(g) {
  const seg = [];
  seg.push((g.label || g.key) + (g.flag === "flagged" ? "【显著异常】" : g.flag === "excluded" ? "【粗筛偏高】" : ""));
  seg.push("天数 " + g.days);
  seg.push("日量 中位 " + fmtInt(Math.round(g.level_median)) + " / P90 " + fmtInt(Math.round(g.level_p90)) + " / 最大 " + fmtInt(Math.round(g.level_max)));
  seg.push("波动 CV " + (g.cv || 0).toFixed(3) + (g.cv_pct ? "（p=" + g.cv_pct.toFixed(3) + "）" : ""));
  seg.push("返回期 " + fmtReturnPeriod(g) + " 天");
  return seg.join(" · ");
}

// ── 图表 2（量级视角）：各子 Key 日量箱线对比 ──
//
// 横条 = P10~P90 范围、竖线 = 中位数、圆点 = 极值。比散点更直观地回答
// 「谁的日常水平高」——但注意它**只是描述**：中位数高不等于分发（重度个人
// 用户的中位数同样高），判定仍看散点图的两个判据。
function renderSDBox(groups, fit) {
  const rows = (groups || []).slice().sort((a, b) => b.level_median - a.level_median);
  if (!rows.length) return '<div class="empty">暂无数据</div>';
  const H = Math.max(120, rows.length * 26 + 26);
  const W = 820, padL = 118, padR = 74, padT = 12, padB = 26;
  const plotW = W - padL - padR;
  let maxV = 1;
  rows.forEach((g) => { if (g.level_max > maxV) maxV = g.level_max; });
  if (fit && fit.valid && fit.upper_endpoint > 0 && fit.upper_endpoint > maxV) maxV = fit.upper_endpoint;
  maxV *= 1.06;
  const xOf = (v) => padL + (v / maxV) * plotW;

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let i = 0; i <= 4; i++) {
    const v = maxV * (i / 4);
    const x = xOf(v);
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + padT + '" x2="' + x.toFixed(1) + '" y2="' + (H - padB) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + x.toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' + fmtInt(Math.round(v)) + '</text>';
  }
  rows.forEach((g, i) => {
    const y = padT + i * 26 + 13;
    const flagged = g.flag === "flagged";
    const color = flagged ? "#b91c1c" : PIE_COLORS[hashIdx(String(g.label || g.key), PIE_COLORS.length)];
    const label = truncText(g.label || g.key, 13);
    svg += '<text x="' + (padL - 10) + '" y="' + (y + 4) + '" font-size="11" fill="#4e5969" text-anchor="end">' +
      escHtml(label) + (flagged ? " ★" : "") + '</text>';
    // P10~P90 范围条（用 level_p90 与中位数对称近似下限，避免后端再传一列）。
    const p10 = Math.max(0, g.level_median - (g.level_p90 - g.level_median));
    svg += '<rect x="' + xOf(p10).toFixed(1) + '" y="' + (y - 4) + '" width="' + Math.max(1, xOf(g.level_p90) - xOf(p10)).toFixed(1) +
      '" height="8" rx="2" fill="' + color + '" fill-opacity=".26"/>';
    svg += '<line x1="' + xOf(g.level_median).toFixed(1) + '" y1="' + (y - 7) + '" x2="' + xOf(g.level_median).toFixed(1) + '" y2="' + (y + 7) +
      '" stroke="' + color + '" stroke-width="2"/>';
    svg += '<circle cx="' + xOf(g.level_max).toFixed(1) + '" cy="' + y + '" r="3" fill="' + color +
      '"><title>' + escHtml(sdTitle(g)) + '</title></circle>';
    // 右侧标注最大日量与其倍数（相对正常组中位数），回答「高多少倍」。
    svg += '<text x="' + (W - padR + 6) + '" y="' + (y + 4) + '" font-size="10" fill="#86909c">' +
      fmtInt(Math.round(g.level_max)) + '</text>';
  });
  svg += '<text x="' + (padL + plotW / 2).toFixed(1) + '" y="' + (H - 8) + '" font-size="11" fill="#6b7280" text-anchor="middle">日请求量（条 = 中位两侧范围，竖线 = 中位数，点 = 最大）→</text>';
  svg += "</svg>";
  return svg;
}

// ── 图表 3（趋势视角）：多子 Key 日用量蜡烛图 ──
//
// 文档「图 1」。这张图回答的是散点图回答不了的问题：**偶发尖峰 vs 持续高位**。
// 分发必然是后者（整条线抬升且平稳），单点突起很常见、不必紧张。
//
// 形态（2026-10 改造，替换原先的多折线）：每个时间槽一根**蜡烛**——
//   实体底 = 前一个「有数据的槽」的合计值、实体顶 = 本槽合计值，影线到本槽的 min/max。
//   涨（顶 > 底）用红、跌用绿（MetaTrader 习惯：红涨绿跌）。
// 为什么不是多折线：子 Key 多时线数超过调色板容量必然撞色，且细线互相压盖，
// 「哪条是谁的」在图上根本读不出来。蜡烛把「当日总量」变成一个可比的形状，
// 子 Key 的数量不再影响可读性。
//
// 点（每个选中子 Key 在该槽的原始日量）：
//   - 单选时画在蜡烛**右侧**（此时用户就是要看这个 Key 的逐日形态，实体当参照线）；
//   - 多选时不画散点，改为把每个子 Key 的值以**极淡的竖线**画在实体内部
//     （构成「实体内部的取值分布」，不额外占用横向空间、也不产生第二套配色）。
//
// 合计口径：该槽所有「选中子 Key」当日请求数之和。选中集合变化 → 蜡烛形状与
// Y 轴量程同步重算（这是需求「随选中的子 key 刷新」的落点）。
//
// selKeys 为 null 表示全选（默认视角）；否则是选中集合（数组）。
// axis 由调用方（页面组件 dayAxis）按选中集合预计算，故本函数是纯函数。
function renderSDTimeline(trends, groups, axis, selKeys) {
  const picked = sdFilterTrends(trends, selKeys);
  if (!picked.length) {
    return '<div class="empty">' + (sdIsAll(selKeys, trends) ? '所选区间暂无数据' : '未选择任何子 Key——请在上方选择要显示的子 Key') + '</div>';
  }
  if (!axis || !axis.slots.length) return '<div class="empty">所选区间暂无数据</div>';
  const byKey = {};
  (groups || []).forEach((g) => { byKey[g.key] = g; });

  const W = 820, H = 300, padL = 56, padR = 24, padT = 14, padB = 30;
  const plotW = W - padL - padR, plotH = H - padT - padB;
  const slots = axis.slots;
  const n = slots.length;
  // 槽位宽度：固定网格（缺日留空槽），蜡烛不超过槽宽 62%，也不太胖。
  const slotW = plotW / n;
  const candleW = Math.max(2, Math.min(slotW * 0.62, 16));
  const xCenter = (i) => padL + slotW * (i + 0.5);

  // 每个槽的合计值（选中集合）与各子 Key 明细。无数据的槽为 null（不画）。
  const idx = sdTrendIndex(picked, slots);
  const stats = slots.map((_, i) => sdSlotStats(picked, idx, i));
  let maxV = 1;
  stats.forEach((s) => { if (s && s.max > maxV) maxV = s.max; });
  maxV *= 1.06;
  const yOf = (v) => padT + plotH * (1 - Math.min(v, maxV) / maxV);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let i = 0; i <= 4; i++) {
    const v = maxV * (1 - i / 4);
    const y = padT + plotH * (i / 4);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" font-size="9" fill="#86909c" text-anchor="end">' + fmtInt(Math.round(v)) + '</text>';
  }

  // 槽位分隔（极淡竖线）：没有它，缺日留空的槽看起来像蜡烛间随机间距。
  for (let i = 1; i < n; i++) {
    const x = padL + slotW * i;
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + padT + '" x2="' + x.toFixed(1) + '" y2="' + (H - padB) + '" stroke="#f2f3f5"/>';
  }

  // 蜡烛：实体底 = 上一个「有数据的槽」的合计，实体顶 = 本槽合计。
  // prev 只在遇到有数据的槽时推进——中间的空档不参与比较（否则「隔了 3 天」
  // 的涨跌会被当成相邻两日的涨跌，语义是错的）。
  let prev = null;
  const single = picked.length === 1;
  const hit = []; // 右侧标注：命中判据的关键组
  stats.forEach((s, i) => {
    if (!s) return;
    const topV = s.total;
    const botV = prev === null ? s.total : prev;
    prev = s.total;
    const up = topV > botV;                       // 涨：红（MT 习惯红涨绿跌）
    const flat = topV === botV;
    const color = flat ? "#86909c" : up ? "#dc2626" : "#10b981";
    const x = xCenter(i);
    const yTop = yOf(Math.max(topV, botV));
    const yBot = yOf(Math.min(topV, botV));
    const bodyH = Math.max(1.2, yBot - yTop);      // 平盘时给 1.2px，否则实体消失

    // 影线：到该槽 min/max（选中集合口径）。
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + yOf(s.max).toFixed(1) + '" x2="' + x.toFixed(1) + '" y2="' + yOf(s.min).toFixed(1) +
      '" stroke="' + color + '" stroke-width="1"/>';
    // 实体（半透明，让体内的分布竖线可见）。
    svg += '<rect x="' + (x - candleW / 2).toFixed(1) + '" y="' + yTop.toFixed(1) + '" width="' + candleW.toFixed(1) +
      '" height="' + bodyH.toFixed(1) + '" fill="' + color + '" fill-opacity="0.55" stroke="' + color + '" stroke-width="1">' +
      '<title>' + escHtml(sdCandleTitle(slots[i], s, botV, single)) + '</title></rect>';

    if (single) {
      // 单选：把该 Key 的当日值画成实体右侧的点（此时值 == 实体顶，点只是强调）。
      svg += '<circle cx="' + (x + candleW / 2 + 3).toFixed(1) + '" cy="' + yOf(s.total).toFixed(1) + '" r="2" fill="' + color +
        '"><title>' + escHtml(sdCandleTitle(slots[i], s, botV, true)) + '</title></circle>';
    } else {
      // 多选：实体内部的取值分布竖线（每子 Key 一条，极淡；重叠即「值集中」）。
      s.items.forEach((it) => {
        svg += '<line x1="' + (x - candleW / 2).toFixed(1) + '" y1="' + yOf(it.v).toFixed(1) + '" x2="' + (x + candleW / 2).toFixed(1) +
          '" y2="' + yOf(it.v).toFixed(1) + '" stroke="#111827" stroke-opacity="0.22"/>';
      });
    }
  });

  // 日刻度：最多 8 个（槽位化后 tick 落在槽心）。
  const step = Math.max(1, Math.floor(n / 8));
  for (let k = 0; k < n; k += step) {
    svg += '<text x="' + xCenter(k).toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' +
      (slots[k].label || "") + '</text>';
  }

  // 右侧标注：命中判据 / 粗筛偏高的组 + 最多 4 个其它组（图例式，省掉独立图例块）。
  const labeled = picked.filter((t) => {
    const g = byKey[t.key];
    return g && (g.flag === "flagged" || g.flag === "excluded");
  }).concat(picked.filter((t) => {
    const g = byKey[t.key];
    return !g || (g.flag !== "flagged" && g.flag !== "excluded");
  }));
  let ly = padT + 6;
  const maxLabels = Math.floor((H - padT - padB) / 14);
  labeled.slice(0, maxLabels).forEach((t) => {
    const g = byKey[t.key] || {};
    const flagged = g.flag === "flagged";
    const color = flagged ? "#b91c1c" : PIE_COLORS[hashIdx(String(t.label || t.key), PIE_COLORS.length)];
    svg += '<line x1="' + (W - padR + 2) + '" y1="' + (ly - 3) + '" x2="' + (W - padR + 12) + '" y2="' + (ly - 3) + '" stroke="' + color + '" stroke-width="2"/>';
    svg += '<text x="' + (W - padR + 15) + '" y="' + ly + '" font-size="10" fill="' + color + '">' +
      escHtml(truncText(t.label || t.key, 7)) + (flagged ? " ★" : "") + '</text>';
    ly += 14;
  });
  if (labeled.length > maxLabels) {
    svg += '<text x="' + (W - padR + 2) + '" y="' + ly + '" font-size="9" fill="#86909c">+' +
      (labeled.length - maxLabels) + ' 个</text>';
  }
  svg += "</svg>";

  const leg = '<div class="chart-legend">' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#dc2626"></span>涨（合计高于上一个有数据的日）</span>' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#10b981"></span>跌</span>' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#86909c"></span>持平</span>' +
    '<span class="leg-item">实体 = 合计（相邻有数据日比较）· 影线 = 当日 min/max' +
    (picked.length > 1 ? ' · 体内横线 = 各子 Key 取值' : ' · 右侧点 = 该子 Key 当日值') + '</span>' +
    '</div>';
  return svg + leg;
}

// sdIsAll 判断选中集合是否等价于「全部」（null 或覆盖所有 trend）。
function sdIsAll(selKeys, trends) {
  if (selKeys === null || selKeys === undefined) return true;
  return sdFilterTrends(trends, selKeys).length === (trends || []).length;
}

// sdFilterTrends 按选中集合过滤 trends（null/未选中任何项都返回空数组）。
function sdFilterTrends(trends, selKeys) {
  const all = trends || [];
  if (selKeys === null || selKeys === undefined) return all.slice();
  const set = {};
  (selKeys || []).forEach((k) => { set[k] = 1; });
  return all.filter((t) => set[t.key]);
}

// sdSlotStats 汇总一个槽位上所有选中子 Key 的当日值。
// 返回 null 表示该槽无数据（全部选中组当日都没用量）——缺日留空，不补零。
//
// ⚠ 按**桶值（时间戳）查值**，不能按下标：每个子 Key 的 buckets 是各自稀疏的、
// 只含「有使用的日」，同一个槽下标在不同子 Key 上对应不同的日期。按下标取会把
// 不同日期的值混到同一根蜡烛里（实测会表现为「形状对未来预知」的错误读图）。
// 索引由调用方建好（trendIndex），一次 O(n·m) 而不是每个槽都线性扫一遍。
function sdSlotStats(picked, idx, i) {
  const items = [];
  let total = 0, mn = Infinity, mx = 0;
  picked.forEach((t) => {
    const v = idx[t.key] ? idx[t.key][i] : undefined;
    if (v === undefined || v <= 0) return;
    items.push({ label: t.label || t.key, v: v });
    total += v;
    if (v < mn) mn = v;
    if (v > mx) mx = v;
  });
  if (!items.length) return null;
  items.sort((a, b) => b.v - a.v);
  return { items: items, total: total, min: mn, max: mx, subkeys: items.length };
}

// sdTrendIndex 把各 trend 的 (bucket → days 值) 建成「槽下标 → 值」的查表。
// 入参 slots 是共享时间轴（含空白日的槽），故未命中的槽下标天然为 undefined。
function sdTrendIndex(picked, slots) {
  const pos = {};
  slots.forEach((s, i) => { pos[s.ts] = i; });
  const out = {};
  picked.forEach((t) => {
    const row = {};
    (t.buckets || []).forEach((b, j) => {
      const i = pos[b];
      if (i === undefined) return;
      row[i] = (t.days || [])[j] || 0;
    });
    out[t.key] = row;
  });
  return out;
}

// sdCandleTitle 蜡烛 tooltip：日期 + 合计/上个有数据日 + 涨跌 + 各子 Key 读数。
function sdCandleTitle(slot, s, prevTotal, single) {
  const seg = [slot.label || ""];
  seg.push("合计 " + fmtInt(Math.round(s.total)) + " 次");
  if (prevTotal !== null && prevTotal !== undefined) {
    const d = s.total - prevTotal;
    seg.push("较上一有数据日 " + (d >= 0 ? "+" : "") + fmtInt(Math.round(d)) +
      "（" + (d >= 0 ? "涨" : "跌") + "，基准 " + fmtInt(Math.round(prevTotal)) + "）");
  } else {
    seg.push("首个有数据日（无比较基准，按持平处理）");
  }
  seg.push("区间 " + fmtInt(Math.round(s.min)) + " ~ " + fmtInt(Math.round(s.max)) + "（" + s.subkeys + " 个子 Key）");
  // 子 Key 明细：最多列 8 条（悬停提示过长反而读不了），其余折叠为计数。
  const head = s.items.slice(0, 8).map((it) => it.label + " " + fmtInt(Math.round(it.v)));
  seg.push(head.join(" · ") + (s.items.length > 8 ? " · 等 " + s.items.length + " 个" : ""));
  if (single) seg.push("（单选中视图）");
  return seg.join("\n");
}

// ── 图表 4（模型视角）：拟合分布与判定位置 ──
//
// 把模型本身画出来：正常组日量的经验生存函数（对数纵轴）叠上拟合的 GPD 外推，
// 再标出各子 Key 的最大日量落在哪里。这张图回答「返回期是怎么算出来的」——
// 让人能看出外推是否可信（点离拟合线越远/越靠右，外推越不可信）。
function renderSDFit(groups, fit, params) {
  if (!fit || !fit.valid || !groups.length) {
    return '<div class="empty">' + escHtml((fit && fit.reason) || "拟合不可用") + '</div>';
  }
  const W = 820, H = 300, padL = 58, padR = 120, padT = 14, padB = 32;
  const plotW = W - padL - padR, plotH = H - padT - padB;

  // 横轴：日量（线性，覆盖到所有组的最大值）。纵轴：超过该量的概率（对数）。
  let maxX = fit.threshold * 2;
  groups.forEach((g) => { if (g.level_max > maxX) maxX = g.level_max; });
  if (fit.upper_endpoint > 0 && fit.upper_endpoint > maxX) maxX = fit.upper_endpoint;
  maxX *= 1.08;
  const xOf = (v) => padL + (Math.min(v, maxX) / maxX) * plotW;
  // 概率下界取「最小可分辨」= 1/(N+1)，再往下是纯外推。
  const pMin = Math.max(1e-8, 1 / (fit.n + 1) / 50);
  const yOf = (p) => {
    const lp = Math.log10(Math.max(p, pMin));
    const lo = Math.log10(pMin), hi = 0;
    return padT + plotH * (1 - (lp - lo) / (hi - lo));
  };

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  // 网格：横轴 4 等分，纵轴按 10 的幂。
  for (let i = 0; i <= 4; i++) {
    const v = maxX * (i / 4);
    const x = xOf(v);
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + padT + '" x2="' + x.toFixed(1) + '" y2="' + (H - padB) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + x.toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' + fmtInt(Math.round(v)) + '</text>';
  }
  for (let e = 0; e >= -12; e--) {
    const p = Math.pow(10, e);
    if (p < pMin) break;
    const y = yOf(p);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" font-size="9" fill="#86909c" text-anchor="end">1e' + e + '</text>';
  }

  // 阈值 u 的竖线。
  const xu = xOf(fit.threshold);
  svg += '<line x1="' + xu.toFixed(1) + '" y1="' + padT + '" x2="' + xu.toFixed(1) + '" y2="' + (H - padB) +
    '" stroke="#86909c" stroke-dasharray="3 3"/>';
  svg += '<text x="' + xu.toFixed(1) + '" y="' + (padT - 3) + '" font-size="9" fill="#86909c" text-anchor="middle">阈值 u</text>';

  // GPD 外推曲线：P(X > x) = ζ_u · [1 + ξ(x-u)/β]^(-1/ξ)
  let d = "", started = false;
  for (let i = 0; i <= 160; i++) {
    const x = fit.threshold + (maxX - fit.threshold) * (i / 160);
    let p;
    if (Math.abs(fit.xi) < 1e-9) {
      p = fit.zeta * Math.exp(-(x - fit.threshold) / fit.beta);
    } else {
      const base = 1 + fit.xi * (x - fit.threshold) / fit.beta;
      if (base <= 0) break;
      p = fit.zeta * Math.pow(base, -1 / fit.xi);
    }
    if (p < pMin) break;
    d += (started ? "L" : "M") + xOf(x).toFixed(1) + " " + yOf(p).toFixed(1) + " ";
    started = true;
  }
  if (d) {
    svg += '<path d="' + d + '" fill="none" stroke="#8b5cf6" stroke-width="1.8" stroke-dasharray="5 3"/>';
  }

  // 各组最大日量：横轴位置 + 其返回期对应的纵轴位置。
  groups.forEach((g) => {
    const x = xOf(g.level_max);
    const p = g.return_period_days > 0 ? 1 / g.return_period_days : pMin;
    const y = yOf(Math.max(p, pMin));
    const flagged = g.flag === "flagged";
    const color = flagged ? "#b91c1c" : PIE_COLORS[hashIdx(String(g.label || g.key), PIE_COLORS.length)];
    svg += '<circle cx="' + x.toFixed(1) + '" cy="' + y.toFixed(1) + '" r="' + (flagged ? 6 : 4) + '" fill="' + color +
      '" fill-opacity=".9"><title>' + escHtml(sdTitle(g)) + '</title></circle>';
    if (flagged) {
      svg += '<text x="' + (x + 9).toFixed(1) + '" y="' + (y - 6).toFixed(1) + '" font-size="10" fill="' + color + '">' +
        escHtml(truncText(g.label || g.key, 10)) + '</text>';
    }
  });

  svg += '<text x="' + (padL + plotW / 2).toFixed(1) + '" y="' + (H - 8) + '" font-size="11" fill="#6b7280" text-anchor="middle">日请求量 →</text>';
  svg += '<text x="14" y="' + (padT + plotH / 2).toFixed(1) + '" font-size="11" fill="#6b7280" text-anchor="middle" transform="rotate(-90 14 ' +
    (padT + plotH / 2).toFixed(1) + ')">超过该量的概率 P(X&gt;x)（对数）→</text>';
  svg += "</svg>";
  const leg = '<div class="chart-legend">' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#8b5cf6"></span>GPD 拟合外推（虚线）</span>' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#b91c1c"></span>显著异常子 Key 的最大日量</span>' +
    '<span class="leg-item">其余点为各子 Key 最大日量（纵轴位置 = 1/返回期）</span></div>';
  return svg + leg;
}

// ── 辅助 ──

function fmtRPTick(v) {
  if (v >= 1e6) return (v / 1e6) + "M";
  if (v >= 1e3) return (v / 1e3) + "K";
  return String(v);
}

// fmtReturnPeriod 返回期展示：封顶值（后端对 +Inf 的表示）显示为「∞ 以上」，
// 而不是印一个 1e12 让人误以为是真实外推结果。
function fmtReturnPeriod(g) {
  if (g.infinite) return "∞";
  if (g.return_period_days >= 1e9) return "&gt;1e9";
  if (!g.return_period_days || g.return_period_days < 1) return "1";
  return fmtInt(Math.round(g.return_period_days));
}

function truncText(s, n) {
  const r = Array.from(String(s || ""));
  return r.length > n ? r.slice(0, n).join("") + "…" : r.join("");
}

// ═══════════════ 页面 ═══════════════
