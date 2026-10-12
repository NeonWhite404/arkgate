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
 *   3. renderSDCandles 日用量蜡烛图（本文件的核心，见下方大段注释）
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
    const color = flagged ? "#b91c1c" : PIE_COLORS[groupSlot(g.key, groups)];
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
    const color = flagged ? "#b91c1c" : PIE_COLORS[groupSlot(g.key, rows)];
    const label = legendName(g, rows);
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
// renderSDCandles 真正的 MT 蜡烛图：每个自然日一根蜡烛，OHLC 来自**日内小时桶**。
//
// 与旧的「相邻有数据日比较」口径的根本区别（也是旧实现被删掉的原因）：
//   - 旧图：实体 = [前一有数据日合计, 本日合计]，影线 = 各子 Key 当日的 min/max。
//     两者量纲不同（单 Key vs 合计），影线会低垂到蜡烛脚下很远。
//   - 新图：实体 = [开, 收]，影线 = [低, 高]，四个值都来自同一个日内小时序列，
//     同量纲，low <= O,C <= high 必然成立，影线天然包住实体。
//
// 单位 = 请求数（与旧图一致）。多选子 Key 时，把各子 Key 同日的小时桶**逐小相加**
// 再算 OHLC——这样得到的才是「分发总量」的日内形状；若先算各 Key 的 OHLC 再相加，
// 会得到一组互不对应的高低点（第一个 Key 的 High 时刻未必是总量的 High 时刻）。
function renderSDCandles(groups, sgroups, axis, selKeys) {
  const all = groups || [];
  const picked = selKeys === null || selKeys === undefined
    ? all.slice()
    : all.filter((g) => selKeys.indexOf(g.key) >= 0);

  const W = 820, H = 300, padL = 56, padR = 104, padT = 14, padB = 30;
  const plotW = W - padL - padR, plotH = H - padT - padB;
  const slots = (axis && axis.slots) || [];
  const n = slots.length;

  if (!picked.length) {
    return sdEmptyFrame(axis, "未选择任何子 Key", "在上方「选择」里勾选子 Key，或点「全部」");
  }
  if (!n) {
    return sdEmptyFrame(axis, "当前区间没有可画的日", "试试放宽日期区间");
  }

  // 把选中子 Key 的蜡烛按日桶归集，后面在小时粒度上合并。
  const byBucket = {};
  picked.forEach((g) => {
    (g.candles || []).forEach((c) => {
      if (!byBucket[c.bucket]) byBucket[c.bucket] = [];
      byBucket[c.bucket].push(c);
    });
  });

  // 合并口径：把各子 Key 的**日内小时序列逐小时相加**，再从合计序列取 OHLC。
  //
  // 绝不能改成"各 Key 的 High 相加、Low 相加"：两个 Key 的峰值不在同一小时时
  // 会严重高估总量峰值（实测例：A 在 10 时 100/12 时 1，B 在 10 时 1/12 时 100
  // → 逐小时相加的真实峰值 101，而 ΣHigh = 200）。周一并相加同样错：各 Key
  // 的"首个小时"可能是不同的小时，相加后的开/收都没有对应的小时。
  // 所以一律回到小时粒度——只有它在多选时仍然有确切含义。
  const merged = slots.map((s) => {
    const list = byBucket[s.ts];
    if (!list) return null;
    const hourly = new Array(24).fill(0);
    list.forEach((c) => {
      const h = c.hourly || [];
      for (let i = 0; i < 24; i++) hourly[i] += h[i] || 0;
    });
    // 只有确实有请求的小时入样。
    // 不能把 0 当作"当日低点"：凌晨天然没流量的时段会把每根蜡烛的影线
    // 都拉到 0，影线就失去"当日波动"的含义。
    const vals = hourly.filter((v) => v > 0);
    if (!vals.length) return null;
    const firstH = hourly.findIndex((v) => v > 0);
    let lastH = 23;
    while (lastH > 0 && !(hourly[lastH] > 0)) lastH--;
    let sum = 0;
    for (let i = 0; i < vals.length; i++) sum += vals[i];
    return {
      open: hourly[firstH], close: hourly[lastH],
      high: Math.max.apply(null, vals), low: Math.min.apply(null, vals),
      total: sum, hb: vals.length, items: list,
    };
  });

  if (!merged.some((m) => m !== null)) {
    // 区分两种"空"：
    //   - 选中的子 Key 确实在区间内没有任何蜡烛 → 真的没数据；
    //   - 有蜡烛，但没一根落在轴上 → 轴与数据错配（程序 bug，
    //     典型原因是槽 ts 用了与后端不同的时区口径）。
    // 后者必须显式报错：静默画一张"看似正常的空图"（图例照列、网格刻度都在、
    // 零蜡烛、Y 轴还退化成个位数）会让用户无法区分"真没数据"与"程序对不上"，
    // 这正是之前踩过的坑，故在此守住。
    const total = picked.reduce((a, g) => a + (g.candles || []).length, 0);
    if (total > 0) {
      return sdEmptyFrame(axis, "图表数据与时间轴不匹配",
        "选中 " + picked.length + " 个子 Key、共 " + total + " 根蜡烛，但无一落在当前时间轴上");
    }
    return sdEmptyFrame(axis, "当前区间没有用量数据",
      "已选中 " + picked.length + " 个子 Key，但这些天都没有请求记录");
  }

  let maxV = 1;
  merged.forEach((m) => { if (m && m.high > maxV) maxV = m.high; });
  maxV *= 1.06;
  const yOf = (v) => padT + plotH * (1 - Math.min(v, maxV) / maxV);
  const slotW = plotW / n;
  const xCenter = (i) => padL + slotW * (i + 0.5);
  // 蜡烛宽度：真蜡烛的实体只是当日开/收的价格区间，不宜像旧图那样占满槽宽
  // （旧图实体是"合计区间"，接近满宽才像柱状图）。0.62 倍槽宽（上限 18px）
  // 在 28 槽下约 12px，比例接近 MT；影线是 1px 竖线，自然比实体细。
  const candleW = Math.max(2, Math.min(slotW * 0.62, 18));

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let i = 0; i <= 4; i++) {
    const v = maxV * (1 - i / 4);
    const y = padT + plotH * (i / 4);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" font-size="9" fill="#86909c" text-anchor="end">' + sdAxisLabel(v) + '</text>';
  }
  for (let i = 1; i < n; i++) {
    const x = padL + slotW * i;
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + padT + '" x2="' + x.toFixed(1) + '" y2="' + (H - padB) + '" stroke="#f2f3f5"/>';
  }

  merged.forEach((m, i) => {
    if (!m) return;
    const x = xCenter(i);
    const up = m.close > m.open;
    const flat = m.close === m.open;
    const color = flat ? "#86909c" : up ? "#dc2626" : "#10b981";
    // 影线：低~高（同量纲，必包住实体）。
    //
    // 坐标一律先量化到 0.1px（与 SVG 实际写出的一致）再参与几何计算：
    // 直接对未量化的浮点算包含关系、最后才 toFixed(1) 写出，两者会不一致——
    // 实测出现过实体底边精确值 269.69、量化后 269.8 而影线底 269.7，
    // 即"算出来包含、画出来不包含"。量化后再算，写出的图形必然满足不变量。
    const q = (v) => Math.round(v * 10) / 10;
    const wickTop = q(yOf(m.high));
    const wickBot = q(yOf(m.low));
    svg += '<line x1="' + x.toFixed(1) + '" y1="' + wickTop.toFixed(1) + '" x2="' + x.toFixed(1) + '" y2="' + wickBot.toFixed(1) +
      '" stroke="' + color + '" stroke-width="1"/>';
    // 实体：开~收（收>开 涨=红，与 MT 习惯一致）。
    //
    // 硬不变量：实体必须完整落在影线区间 [wickTop, wickBot] 内。
    // 当日只有 1-2 个小时有数据时（open/close/low/high 几乎全等），实体真实高度
    // 可能不足 1px，甚至影线本身也只比 1px 高一点。此时任何"保底 1.2px 高度"
    // 的做法都会把实体撑出影线（实测：固定顶边溢出 0.6px，居中后仍溢出 0.3~0.5px）。
    // 因此最小高度只能作为**上限目标**，实际取值 = min(1.2, 影线高度)：
    // 影线高不足 1.2px 时，实体恰等于影线（退化为一条粗线，形状仍有意义），
    // 而"实体比影线还高"才是真正无意义的形状。
    const wickH = Math.max(0, wickBot - wickTop);
    const yTop0 = q(yOf(Math.max(m.open, m.close)));
    const yBot0 = q(yOf(Math.min(m.open, m.close)));
    // 目标高度：真实开收差与最小可见高度取大，但不超过影线高度。
    let bodyH = q(Math.min(Math.max(1.2, yBot0 - yTop0), wickH));
    // 居中放置后再夹回影线区间。
    let yTop = q((yTop0 + yBot0) / 2 - bodyH / 2);
    if (yTop < wickTop) yTop = wickTop;
    if (yTop + bodyH > wickBot) {
      yTop = wickBot - bodyH;
      if (yTop < wickTop) { yTop = wickTop; bodyH = Math.max(0, wickBot - wickTop); }
    }
    svg += '<rect x="' + (x - candleW / 2).toFixed(1) + '" y="' + yTop.toFixed(1) + '" width="' + candleW.toFixed(1) +
      '" height="' + bodyH.toFixed(1) + '" fill="' + color + '" stroke="' + color + '" stroke-width="0.5" rx="0.5">' +
      '<title>' + escHtml(sdOhlcTitle(slots[i], m, picked.length)) + '</title></rect>';
  });

  const step = Math.max(1, Math.floor(n / 8));
  for (let k = 0; k < n; k += step) {
    svg += '<text x="' + xCenter(k).toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' +
      (slots[k].label || "") + '</text>';
  }

  // 右侧图例：单选时标注该 Key，多选时标注参与的全部 Key。
  const byKey = {};
  (sgroups || []).forEach((g) => { byKey[g.key] = g; });
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
    const color = flagged ? "#b91c1c" : PIE_COLORS[colorSlot(t.key, picked)];
    svg += '<line x1="' + (W - padR + 2) + '" y1="' + (ly - 3) + '" x2="' + (W - padR + 12) + '" y2="' + (ly - 3) + '" stroke="' + color + '" stroke-width="2"/>';
    svg += '<text x="' + (W - padR + 15) + '" y="' + ly + '" font-size="10" fill="' + color + '">' +
      escHtml(legendName(t, picked)) + (flagged ? " ★" : "") + '</text>';
    ly += 14;
  });
  if (labeled.length > maxLabels) {
    svg += '<text x="' + (W - padR + 2) + '" y="' + ly + '" font-size="9" fill="#86909c">+' +
      (labeled.length - maxLabels) + ' 个未标注</text>';
  }
  svg += "</svg>";

  const leg = '<div class="chart-legend">' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#dc2626"></span>涨（收 > 开）</span>' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#10b981"></span>跌（收 < 开）</span>' +
    '<span class="leg-item"><span class="leg-swatch" style="background:#86909c"></span>持平</span>' +
    '<span class="leg-item">实体 = 当日首/末小时桶 · 影线 = 当日最高/最低小时桶 · 单位：请求数' +
    (picked.length > 1 ? ' · 多选时按各子 Key 相加' : '') + '</span>' +
    '</div>';
  return svg + leg;
}

// sdOhlcTitle 蜡烛 tooltip：日期 + OHLC + 合计 + 采样小时数。
//
// HourBuckets 一并列出：只有 1-2 个小时有数据的日子，实体与影线必然退化成
// 一条线——写明采样数，用户才能分辨"当日真的很平稳"与"当天只有 2 小时有数据"。
function sdOhlcTitle(slot, m, keyCount) {
  const day = slot ? slot.label : "";
  const s = [day,
    "开 " + fmtInt(Math.round(m.open)) + " · 高 " + fmtInt(Math.round(m.high)) +
    " · 低 " + fmtInt(Math.round(m.low)) + " · 收 " + fmtInt(Math.round(m.close)),
    "合计 " + fmtInt(Math.round(m.total)) + " 次",
    "日内采样 " + m.hb + " 个小时桶" + (keyCount > 1 ? " · " + keyCount + " 个子 Key 相加" : "")];
  return s.join("\n");
}

// groupSlot 按**在数组中的次序**分配调色板下标（不重复，除非超出调色板长度）。
//
// 为什么不用 hashIdx(label)：djb2 对短名字聚集严重——实测同屏 7 个子 Key 中 3 个
// 拿到同一颜色、两个同名项完全无法区分，图例形同虚设。groupSlot 保证同一集合内
// 每个项颜色互异；输入数组次序稳定（后端按 key 序返回），故颜色也稳定。
function groupSlot(key, arr) {
  const idx = (arr || []).findIndex((g) => g.key === key);
  return (idx < 0 ? 0 : idx) % PIE_COLORS.length;
}

// colorSlot 给选中集合里的每个子 Key 分配**不重复**的调色板下标。
//
// 为什么不能用 hashIdx(label)：djb2 对短名字聚集得很厉害，实测库里 7 个子 Key
// 里有 3 个（「三重奏」/「dev」/「bot」）恰好都落进同一个颜色，两个同名的
// 「test」也完全分不出——图例于是失去意义（用户截图为证）。
//
// 改为「按选中集合内的稳定次序依次取色」：同一集合下颜色稳定（刷新不变、
// 勾选顺序不影响）；集合变化时颜色可能重排，但**永不撞色**是关键。
// 超过调色板长度时循环复用（此时颜色必然重复，但至少相邻项不同）。
function colorSlot(key, picked) {
  const idx = (picked || []).findIndex((t) => t.key === key);
  return (idx < 0 ? 0 : idx) % PIE_COLORS.length;
}

// legendName 图例显示名：同名子 Key 加上 key 后缀以区分。
//
// 库里真有两个都叫「test」的子 Key（不同 key），只显示 label 时图例会列出
// 两行一模一样的「test」，无法判断哪条是哪条。只对**确实重名**的项加后缀，
// 不重名时不加噪音。
function legendName(t, picked) {
  const name = t.label || t.key;
  const dup = (picked || []).filter((x) => (x.label || x.key) === name).length > 1;
  if (!dup) return truncText(name, 7);
  // 后缀取 key 末 4 位（sk_ 前缀之后的部分），足够区分且不占宽度。
  return truncText(name, 4) + "·" + String(t.key || "").slice(-4);
}// sdAxisLabel 坐标轴刻度文案：按量级自适应精度。
//
// 为什么不能直接用 fmtInt：刻度值是 maxV 的四等分，量级小时必然是小数。
// 旧实现一律四舍五入到整数，maxV=1 时会得到「1/1/1/0/0」——五个刻度看起来
// 一样（实测发生过），图看起来就像没画出来，也完全无法读数。
// 规则：数值 >= 100 取整；>= 10 取一位小数；否则两位小数。
// 千分位仍交给 fmtInt（>=100 的分支），小数分支自己补位。
function sdAxisLabel(v) {
  if (!isFinite(v)) return "";
  const a = Math.abs(v);
  if (a >= 100) return fmtInt(Math.round(v));
  if (a >= 10) return v.toFixed(1).replace(/\.0$/, "");
  if (a >= 1) return v.toFixed(1);
  if (a === 0) return "0";
  return v.toFixed(2);
}

// sdEmptyFrame 画一个「空但完整」的图框：网格、日期刻度都在，中心是一句提示。
// 用于「区间有效但当前没有可画的数据 / 未选任何子 Key」。
//
// 与直接 return 一句文字的区别：后者让整张图在页面上消失，用户无法区分
// 「功能坏了」、「区间真没数据」、「我把子 Key 全取消了」三种情况。
function sdEmptyFrame(axis, msg, hint) {
  const W = 820, H = 300, padL = 56, padR = 24, padT = 14, padB = 30;
  const plotW = W - padL - padR, plotH = H - padT - padB;
  const slots = (axis && axis.slots) || [];
  const n = slots.length || 1;
  const slotW = plotW / n;
  const xCenter = (i) => padL + slotW * (i + 0.5);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let i = 0; i <= 4; i++) {
    const y = padT + plotH * (i / 4);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) + '" stroke="#e5e7eb"/>';
  }
  // 日期刻度照旧画：它是「区间有效、只是没东西可画」的可见证据。
  const step = Math.max(1, Math.floor(n / 8));
  for (let k = 0; k < n; k += step) {
    svg += '<text x="' + xCenter(k).toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' +
      ((slots[k] && slots[k].label) || "") + '</text>';
  }
  const cx = padL + plotW / 2, cy = padT + plotH / 2;
  svg += '<text x="' + cx.toFixed(1) + '" y="' + (cy - 4).toFixed(1) + '" font-size="13" fill="#86909c" text-anchor="middle">' + escHtml(msg) + '</text>';
  if (hint) {
    svg += '<text x="' + cx.toFixed(1) + '" y="' + (cy + 16).toFixed(1) + '" font-size="11" fill="#a9aeb8" text-anchor="middle">' + escHtml(hint) + '</text>';
  }
  svg += "</svg>";
  return svg;
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
    const color = flagged ? "#b91c1c" : PIE_COLORS[groupSlot(g.key, groups)];
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
