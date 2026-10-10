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
 * 本文件在 app.js 之前加载（同 models.js 的约定），依赖但不注册任何全局组件；
 * 由 app.js 负责 app.component("ShareDetectPage", ...)。
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

// ── 图表 3（趋势视角）：多子 Key 日用量时间线 ──
//
// 文档「图 1」。这条线回答的是散点图回答不了的问题：**偶发尖峰 vs 持续高位**。
// 分发必然是后者（整条线抬升且平稳），单点突起很常见、不必紧张。
function renderSDTimeline(trends, groups, days) {
  if (!trends || !trends.length || !days) return '<div class="empty">暂无数据</div>';
  const byKey = {};
  (groups || []).forEach((g) => { byKey[g.key] = g; });

  const W = 820, H = 300, padL = 56, padR = 96, padT = 14, padB = 30;
  const plotW = W - padL - padR, plotH = H - padT - padB;
  const maxV = Math.max(1, days.max);
  const xOf = (t) => padL + (days.n > 1 ? ((t - days.min) / (days.max_t - days.min)) * plotW : plotW / 2);
  const yOf = (v) => padT + plotH * (1 - Math.min(v, maxV) / maxV);

  let svg = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H +
    '" preserveAspectRatio="xMidYMid meet" style="max-width:100%">';
  for (let i = 0; i <= 4; i++) {
    const v = maxV * (1 - i / 4);
    const y = padT + plotH * (i / 4);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) + '" stroke="#e5e7eb"/>';
    svg += '<text x="' + (padL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" font-size="9" fill="#86909c" text-anchor="end">' + fmtInt(Math.round(v)) + '</text>';
  }
  // 日刻度：最多 8 个。
  const step = Math.max(1, Math.floor(days.n / 8));
  for (let k = 0; k < days.n; k += step) {
    const x = xOf(days.min + k * 86400);
    svg += '<text x="' + x.toFixed(1) + '" y="' + (H - padB + 15) + '" font-size="9" fill="#86909c" text-anchor="middle">' +
      (days.labels[k] || "") + '</text>';
  }

  // 折线：只画「天数 ≥ minDays」的组（与参与统计的组一致），其余画淡。
  const drawn = [];
  (trends || []).forEach((t) => {
    const g = byKey[t.key];
    if (!g) return;
    const flagged = g.flag === "flagged";
    const excluded = g.flag === "excluded";
    const color = flagged ? "#b91c1c" : PIE_COLORS[hashIdx(String(t.label || t.key), PIE_COLORS.length)];
    const dim = !flagged && !excluded;
    let d = "";
    (t.buckets || []).forEach((b, i) => {
      const v = (t.days || [])[i] || 0;
      const x = xOf(b);
      const y = yOf(v);
      d += (i === 0 ? "M" : "L") + x.toFixed(1) + " " + y.toFixed(1) + " ";
    });
    if (!d) return;
    svg += '<path d="' + d + '" fill="none" stroke="' + color + '" stroke-width="' + (flagged ? 2.2 : 1.4) +
      '" stroke-opacity="' + (dim ? 0.5 : 0.95) + '" stroke-linejoin="round"><title>' +
      escHtml((t.label || t.key) + "：" + (SD_FLAGS[g.flag] || {}).label) + '</title></path>';
    drawn.push({ t: t, g: g, color: color, flagged: flagged });
  });

  // 右侧直接标注高量级组（图例式）：线条颜色 + 名称，省掉单独的图例块。
  const labeled = drawn.filter((x) => x.flagged || x.g.flag === "excluded" || x.g.excluded)
    .concat(drawn.filter((x) => !x.flagged && x.g.flag !== "excluded").slice(0, 5));
  let ly = padT + 6;
  const seen = {};
  labeled.forEach((x) => {
    if (seen[x.t.key] || ly > H - padB) return;
    seen[x.t.key] = 1;
    svg += '<line x1="' + (W - padR + 4) + '" y1="' + (ly - 3) + '" x2="' + (W - padR + 18) + '" y2="' + (ly - 3) + '" stroke="' + x.color + '" stroke-width="2"/>';
    svg += '<text x="' + (W - padR + 22) + '" y="' + ly + '" font-size="10" fill="' + x.color + '">' +
      escHtml(truncText(x.t.label || x.t.key, 9)) + (x.flagged ? " ★" : "") + '</text>';
    ly += 14;
  });
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
      loading: false,
      tab: "scatter",
      tabs: [
        { v: "scatter", label: "极值判据", tip: "主判据：返回期 × 波动 CV，右上红区 = 显著异常" },
        { v: "timeline", label: "日用量趋势", tip: "偶发尖峰 vs 持续高位——分布看不出的时间形态" },
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
    // 时间轴公共范围：所有子 Key 的桶合并取 min/max，保证各条线横向可比
    // （每组各自从 0 起画会让「后开始的组」看起来像在剧烈波动）。
    dayAxis() {
      let mn = Infinity, mx = -Infinity, n = 0;
      const labels = [];
      let first = true;
      this.trends.forEach((t) => {
        (t.buckets || []).forEach((b) => {
          if (b < mn) mn = b;
          if (b > mx) mx = b;
          labels.push(b);
          n++;
        });
      });
      if (!n) return null;
      const uniq = Array.from(new Set(labels)).sort((a, b) => a - b);
      return {
        min: uniq[0], max_t: uniq[uniq.length - 1], n: uniq.length,
        min_: mn, max: Math.max(1, ...this.trends.flatMap((t) => t.days || [])),
        labels: uniq.map((u) => {
          const d = new Date(u * 1000);
          return (d.getMonth() + 1) + "/" + d.getDate();
        }),
      };
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
      if (this.tab === "timeline") return renderSDTimeline(this.trends, this.groups, this.dayAxis);
      if (this.tab === "fit") return renderSDFit(this.groups, this.fit, this.params);
      return renderSDScatter(this.groups, this.fit, this.cvf, this.params);
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
        .then((d) => { this.r = d; })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.loading = false; });
    },
    openParams() { this.form = Object.assign({}, this.form); this.pOpen = true; },
    saveParams() {
      this.snap = JSON.stringify(this.form);
      sdSavePrefs({ form: this.form, minReqPerDay: this.minReqPerDay });
      this.pOpen = false;
      this.load();
    },
    resetParams() {
      this.form = Object.assign({}, SD_DEFAULTS);
      this.minReqPerDay = SD_DEFAULT_MINREQ;
    },
    onMinReqChange() {
      sdSavePrefs({ form: this.form, minReqPerDay: this.minReqPerDay });
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
    return { form, minReqPerDay };
  } catch (e) {
    return { form: Object.assign({}, SD_DEFAULTS), minReqPerDay: SD_DEFAULT_MINREQ };
  }
}

function sdSavePrefs(v) {
  try {
    localStorage.setItem("arkgate_sd_prefs", JSON.stringify({ form: v.form, minReqPerDay: v.minReqPerDay }));
  } catch (e) { /* 隐私模式下写不进去，忽略 */ }
}
