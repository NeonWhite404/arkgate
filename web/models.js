/* ArkGate 模型映射工作台（Vue 3 免构建；由 app.js 注册为全局组件 ModelsPage）
 *
 * 交互参照 CLIProxyAPI Management Center 的「Workbench + Sheet」模式：
 *   - 模型目录一张精简表（行内开关启停、搜索过滤），点行进入侧滑抽屉；
 *   - 抽屉内分段编辑：基础信息 / 价格与上限 / 接入点映射；
 *   - 接入点以卡片内联编辑（账号、上游标识、权重/并发/RPM/TPM、请求头），
 *     每张卡片独立「保存」，未保存修改有点标记——不再需要回到映射表逐个开弹窗。
 * 职责边界不变：fallback 链与路由规则仅在「分流配置」页编排。
 */
"use strict";

// epFormOf 把一条接入点记录展开成可编辑表单（数值用 v-model.number 对齐）。
function epFormOf(e) {
  return {
    account_id: e.account_id, ep: e.ep, enabled: !!e.enabled,
    weight: e.weight || 0, max_concurrency: e.max_concurrency || 0,
    rpm_limit: e.rpm_limit || 0, tpm_limit: e.tpm_limit || 0,
    request_headers_text: JSON.stringify(e.request_headers || {}, null, 2),
  };
}

// 请求头预置值来源（勿凭印象改动）：
//   claude  — cc-switch forwarder.rs：CLAUDE_CODE_USER_AGENT / CLAUDE_CODE_BETA / x-app
//   codex   — openai/codex codex-rs/login/src/auth/default_client.rs：
//             originator=codex_cli_rs，UA 格式 {originator}/{版本} ({系统}; {架构}) {终端}
//   hermes  — NousResearch/hermes-agent agent/client_lifecycle.py + codex_headers.py：
//             HermesAgent/{版本} + originator=hermes-agent
const HEADER_PRESETS = {
  blank: {},
  claude: {
    "User-Agent": "claude-cli/1.0.119 (external, cli)",
    "anthropic-beta": "claude-code-20250219",
    "x-app": "cli",
  },
  codex: {
    "User-Agent": "codex_cli_rs/0.154.0 (Mac OS 15.3; arm64) iTerm.app/3.5.11",
    "originator": "codex_cli_rs",
  },
  hermes: {
    "User-Agent": "HermesAgent/0.21.1",
    "originator": "hermes-agent",
  },
};

const ModelsPage = {
  data() {
    return {
      models: [], accounts: [], eps: [],
      query: "",
      mDrawer: null,   // 模型工作台抽屉 {name|null, originalType, form, catHint, epRows[]}
      iDrawer: null,   // 从上游导入 {account_id, loading, saving, list, picked, query}
      syncing: false,
      savingModel: false,
    };
  },
  mounted() { this.load(); },
  methods: {
    load() {
      return Promise.all([req("GET", "/api/models"), req("GET", "/api/accounts"), req("GET", "/api/endpoints")])
        .then((rs) => {
          this.models = rs[0] || []; this.accounts = rs[1] || []; this.eps = rs[2] || [];
        })
        .catch((e) => toast(e.message, false));
    },
    accName(id) {
      const a = this.accounts.find((x) => x.id === id);
      return (a && a.name) || id;
    },
    epCount(name) {
      return this.eps.filter((e) => e.model === name).length;
    },
    filteredModels() {
      const q = (this.query || "").trim().toLowerCase();
      if (!q) return this.models;
      return this.models.filter((m) =>
        (m.name || "").toLowerCase().includes(q) ||
        (m.display || "").toLowerCase().includes(q) ||
        (m.description || "").toLowerCase().includes(q));
    },
    typeTag(m) {
      return m.type === "image" ? ["tag-purple", "图像"]
        : m.type === "router" ? ["tag-orange", "路由"] : ["tag-blue", "文本"];
    },

    // ── 模型工作台抽屉 ──
    openDrawer(m) {
      this.mDrawer = m ? {
        name: m.name,
        originalType: m.type || "text",
        form: { type: m.type || "text", provider: m.provider || "", display: m.display || "",
          description: m.description || "", enabled: !!m.enabled,
          price_input: m.price_input || 0, price_output: m.price_output || 0,
          price_image: m.price_image || 0, context_tokens: m.context_tokens || 0,
          max_output_tokens: m.max_output_tokens || 0 },
        catHint: "",
        epRows: this.eps.filter((e) => e.model === m.name).map((e) => this.rowFromEp(e)),
      } : {
        name: null,
        originalType: "text",
        form: { type: "text", provider: "", display: "", description: "", enabled: true,
          price_input: 0, price_output: 0, price_image: 0,
          context_tokens: 0, max_output_tokens: 0 },
        catHint: "",
        epRows: [],
      };
      if (m && m.type !== "router") this.lookupCat(m.name);
    },
    drawerTitle() {
      return this.mDrawer && this.mDrawer.name ? "模型：" + this.mDrawer.name : "新建模型";
    },
    rowFromEp(e) {
      return { id: e.id, snap: JSON.stringify(epFormOf(e)), form: epFormOf(e),
        headersOpen: false, opts: [], optsLoading: false, saving: false };
    },
    dirtyRow(row) {
      return JSON.stringify(row.form) !== row.snap;
    },
    headerCount(row) {
      try { return Object.keys(JSON.parse(row.form.request_headers_text || "{}") || {}).length; }
      catch (_) { return 0; }
    },
    sibEps(row) {
      if (!this.mDrawer || !this.mDrawer.name) return [];
      return this.eps.filter((e) => e.account_id === row.form.account_id &&
        e.model === this.mDrawer.name && e.id !== row.id);
    },
    applyHeaderPreset(name, row) {
      row.form.request_headers_text = JSON.stringify(HEADER_PRESETS[name] || {}, null, 2);
    },
    addEpRow() {
      if (!this.mDrawer || !this.mDrawer.name) return;
      const f = epFormOf({ account_id: this.accounts.length ? this.accounts[0].id : "", ep: "", enabled: true });
      this.mDrawer.epRows.push({ id: null, snap: JSON.stringify(f), form: f,
        headersOpen: false, opts: [], optsLoading: false, saving: false });
    },
    fetchRowEp(row) {
      const acc = row.form.account_id;
      if (!acc || row.optsLoading) return;
      row.optsLoading = true;
      req("GET", "/api/upstream/models?account_id=" + encodeURIComponent(acc))
        .then((d) => {
          row.opts = (d.models || []).map((m) => m.id);
          toast(row.opts.length ? "已拉取 " + row.opts.length + " 个上游模型" : "上游返回空列表");
        })
        .catch((e) => toast(e.message, false))
        .finally(() => { row.optsLoading = false; });
    },
    // refreshEps 轻量刷新映射列表，并同步已保存行的快照（保留其它行的未保存修改）。
    refreshEps() {
      return req("GET", "/api/endpoints").then((d) => {
        this.eps = d || [];
        if (!this.mDrawer) return;
        this.mDrawer.epRows.forEach((row) => {
          if (!row.id) return;
          const fresh = this.eps.find((x) => x.id === row.id);
          if (fresh) row.snap = JSON.stringify(epFormOf(fresh));
        });
      }).catch(() => {});
    },
    saveRow(row) {
      if (!this.mDrawer || !this.mDrawer.name) return;
      const f = row.form;
      if (!f.account_id || !f.ep.trim()) { toast("请选择账号并填写上游标识", false); return; }
      let requestHeaders;
      try { requestHeaders = JSON.parse(f.request_headers_text || "{}"); }
      catch (_) { toast("请求头必须是合法 JSON 对象", false); return; }
      if (!requestHeaders || Array.isArray(requestHeaders) || typeof requestHeaders !== "object" ||
        Object.values(requestHeaders).some((v) => typeof v !== "string")) {
        toast("请求头必须是字符串到字符串的 JSON 对象", false); return;
      }
      const payload = { account_id: f.account_id, model: this.mDrawer.name, ep: f.ep.trim(),
        enabled: f.enabled, request_headers: requestHeaders,
        weight: Number(f.weight) || 0, max_concurrency: Number(f.max_concurrency) || 0,
        rpm_limit: Number(f.rpm_limit) || 0, tpm_limit: Number(f.tpm_limit) || 0 };
      row.saving = true;
      const p = row.id ? req("PUT", "/api/endpoints/" + row.id, payload)
        : req("POST", "/api/endpoints", payload);
      p.then((d) => {
        if (!row.id && d && d.id) row.id = d.id;
        row.snap = JSON.stringify(row.form);
        toast("接入点已保存");
        return this.refreshEps();
      }).catch((e) => toast(e.message, false))
        .finally(() => { row.saving = false; });
    },
    removeEpRow(row) {
      if (!row.id) {
        this.mDrawer.epRows.splice(this.mDrawer.epRows.indexOf(row), 1);
        return;
      }
      if (!confirm("确认删除该接入点映射？")) return;
      req("DELETE", "/api/endpoints/" + row.id)
        .then(() => {
          this.mDrawer.epRows.splice(this.mDrawer.epRows.indexOf(row), 1);
          toast("已删除");
          return this.refreshEps();
        })
        .catch((e) => toast(e.message, false));
    },

    // ── 模型元数据 ──
    lookupCat(name) {
      if (!name || !this.mDrawer) return;
      req("GET", "/api/catalog/lookup?name=" + encodeURIComponent(name))
        .then((d) => {
          if (!this.mDrawer) return;
          this.mDrawer.catHint = d.found ? this.catText(d.entry) : "";
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
      const d = this.mDrawer;
      const f = d.form;
      const name = d.name;
      // fallback 与 router 配置只在「分流配置」页维护；普通编辑不携带这两个键，
      // 完整保留既有编排。只有类型切换时才清理不兼容的旧配置。
      const payload = {
        type: f.type, provider: f.type === "text" ? (f.provider || "") : "",
        display: f.display.trim() || name, description: f.description.trim(),
        enabled: f.enabled, price_input: Number(f.price_input) || 0,
        price_output: Number(f.price_output) || 0, price_image: Number(f.price_image) || 0,
        context_tokens: Number(f.context_tokens) || 0, max_output_tokens: Number(f.max_output_tokens) || 0,
      };
      if (f.type === "router" || (name && d.originalType !== f.type)) payload.fallback = [];
      if (d.originalType === "router" && f.type !== "router") payload.router = null;
      if (!name && (!f.name0 || !f.name0.trim())) { toast("请输入模型名", false); return; }
      this.savingModel = true;
      const p = name
        ? req("PUT", "/api/models/" + encodeURIComponent(name), payload)
        : req("POST", "/api/models", { ...payload, name: (f.name0 || "").trim() });
      p.then((r) => {
        const filled = r && r.auto_filled;
        toast(filled && filled.length ? "已保存，目录自动补全：" + filled.join("、") : "已保存");
        if (!name) {
          d.name = (f.name0 || "").trim();
          d.originalType = f.type;
        }
        return this.load();
      })
        .catch((e) => toast(e.message, false))
        .finally(() => { this.savingModel = false; });
    },
    toggleModel(m, v) {
      req("PUT", "/api/models/" + encodeURIComponent(m.name), { enabled: v })
        .then(() => { m.enabled = v; toast(v ? "已启用 " + m.name : "已停用 " + m.name); })
        .catch((e) => toast(e.message, false));
    },
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
      req("DELETE", "/api/models/" + encodeURIComponent(m.name))
        .then(() => {
          toast("已删除");
          if (this.mDrawer && this.mDrawer.name === m.name) this.mDrawer = null;
          return this.load();
        })
        .catch((e) => toast(e.message, false));
    },

    // ── 从上游导入 ──
    openImport() {
      this.iDrawer = {
        account_id: this.accounts.length ? this.accounts[0].id : "",
        loading: false, saving: false, list: [], picked: {}, query: "",
      };
    },
    fetchImport() {
      const m = this.iDrawer;
      if (!m || !m.account_id || m.loading) return;
      m.loading = true;
      req("GET", "/api/upstream/models?account_id=" + encodeURIComponent(m.account_id))
        .then((d) => {
          m.list = d.models || [];
          m.picked = {};
          const mapped = {};
          this.eps.filter((e) => e.account_id === m.account_id).forEach((e) => { mapped[e.ep] = true; });
          m.list.forEach((it) => { m.picked[it.id] = !mapped[it.id]; });
          toast("上游返回 " + m.list.length + " 个模型");
        })
        .catch((e) => toast(e.message, false))
        .finally(() => { m.loading = false; });
    },
    importMapped(id) {
      const m = this.iDrawer;
      return this.eps.some((e) => e.account_id === m.account_id && e.ep === id);
    },
    filteredImportList() {
      const m = this.iDrawer;
      if (!m) return [];
      const q = (m.query || "").trim().toLowerCase();
      return q ? m.list.filter((it) => it.id.toLowerCase().includes(q)) : m.list;
    },
    togglePick(id) { this.iDrawer.picked[id] = !this.iDrawer.picked[id]; },
    pickAll(v) { this.filteredImportList().forEach((it) => { this.iDrawer.picked[it.id] = v; }); },
    doImport() {
      const m = this.iDrawer;
      const ids = m.list.map((x) => x.id).filter((id) => m.picked[id]);
      if (!ids.length) { toast("请先选择要导入的模型", false); return; }
      m.saving = true;
      const known = {};
      this.models.forEach((x) => { known[x.name] = true; });
      let added = 0, skipped = 0, failed = 0;
      const step = (i) => {
        if (i >= ids.length) {
          m.saving = false;
          this.iDrawer = null;
          toast("导入完成：新增 " + added + " 个映射，跳过 " + skipped + "，失败 " + failed);
          this.load();
          return;
        }
        const id = ids[i];
        const ensureModel = known[id]
          ? Promise.resolve()
          : req("POST", "/api/models", { name: id, type: "text", display: id }).catch(() => {});
        ensureModel
          .then(() => req("POST", "/api/endpoints", { account_id: m.account_id, model: id, ep: id, weight: 0 }))
          .then(() => { added++; })
          .catch((e) => { if (String(e.message).indexOf("已存在") >= 0) skipped++; else failed++; })
          .finally(() => step(i + 1));
      };
      step(0);
    },
  },
  template: `
  <div class="page">
    <div class="page-head">
      <div>
        <div class="page-title">模型映射</div>
        <div class="page-sub">点行进入模型工作台：基础信息、价格上限与接入点在一个抽屉里完成编辑</div>
      </div>
    </div>
    <div class="toolbar">
      <div class="search-box">
        <svg class="s-ic" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>
        <input v-model="query" placeholder="搜索模型名 / 显示名 / 描述"/>
      </div>
      <span class="sh-note">共 {{ filteredModels().length }} 个模型 · {{ eps.length }} 个接入点</span>
      <div class="spacer"></div>
      <button class="btn btn-outline" :disabled="syncing" @click="syncCatalog">{{ syncing ? '补全中…' : '⟳ 从目录补全' }}</button>
      <button class="btn btn-outline" @click="openImport">⇩ 从上游导入</button>
      <button class="btn btn-primary" @click="openDrawer(null)">+ 新建模型</button>
    </div>
    <div class="card">
      <div class="table-wrap"><table><thead><tr>
        <th>模型</th><th>类型 / 协议</th><th>接入点</th><th>价格</th><th>上下文 / 输出上限</th><th>分流</th><th>状态</th><th style="width:120px">操作</th>
      </tr></thead><tbody>
        <tr v-if="!filteredModels().length"><td colspan="8" class="empty">{{ query ? '没有匹配的模型' : '暂无模型，点右上角新建或从上游导入' }}</td></tr>
        <tr v-for="m in filteredModels()" :key="m.name" class="clickable" @click="openDrawer(m)">
          <td><div class="cell-main mono">{{ m.name }}</div><div class="cell-sub">{{ m.display || m.description }}</div></td>
          <td><span :class="'tag ' + typeTag(m)[0]">{{ typeTag(m)[1] }}</span>
            <span v-if="m.type==='text' && m.provider==='anthropic'" class="tag tag-gray" title="上游使用 Anthropic /v1/messages 协议，网关自动转换">Anthropic</span></td>
          <td><span v-if="m.type==='router'" class="tag tag-out">虚拟</span>
            <span v-else :class="epCount(m.name) ? 'tag tag-blue' : 'tag tag-gray'">{{ epCount(m.name) }} 接入点</span></td>
          <td class="cost">
            <template v-if="m.type==='image'">{{ fmtCost(m.price_image) }} / 张</template>
            <template v-else-if="m.type==='router'">—</template>
            <template v-else-if="m.price_input || m.price_output">{{ fmtCost(m.price_input) }} / {{ fmtCost(m.price_output) }} per 1M</template>
            <span v-else class="tag tag-gray">未定价</span>
          </td>
          <td class="mono">
            <template v-if="m.context_tokens || m.max_output_tokens">{{ m.context_tokens ? fmtTokens(m.context_tokens) : '—' }} / {{ m.max_output_tokens ? fmtTokens(m.max_output_tokens) : '—' }}</template>
            <span v-else>—</span>
          </td>
          <td class="mono">
            <template v-if="m.type==='router'">{{ (m.router && m.router.rules.length) || 0 }} 条规则</template>
            <template v-else-if="m.fallback && m.fallback.length">{{ m.fallback.length }} 级 fallback</template>
            <span v-else class="sh-note">—</span>
          </td>
          <td @click.stop><ui-switch :model-value="m.enabled" size="sm" @update:model-value="toggleModel(m, $event)"/></td>
          <td @click.stop><div class="row-actions">
            <button class="btn btn-outline btn-sm" @click="openDrawer(m)">工作台</button>
            <button class="btn btn-danger btn-sm" @click="delModel(m)">删除</button>
          </div></td>
        </tr>
      </tbody></table></div>
    </div>

    <!-- 模型工作台抽屉 -->
    <ui-drawer :open="!!mDrawer" :width="880" :title="drawerTitle()"
      :subtitle="mDrawer && mDrawer.name ? '类型 ' + typeTag(mDrawer.form)[1] + (mDrawer.form.type==='text' && mDrawer.form.provider==='anthropic' ? ' · Anthropic 协议' : '') : '保存后即可在下方添加接入点'"
      @close="mDrawer=null">
      <template v-if="mDrawer">
        <div class="sec">
          <div class="sec-title">基础信息</div>
          <div class="form-row">
            <div class="form-item"><label>模型名（下游调用用，唯一）<span class="req" v-if="!mDrawer.name">*</span></label>
              <input v-if="!mDrawer.name" v-model="mDrawer.form.name0" placeholder="例如 doubao-seed-1-6" @blur="lookupCat(mDrawer.form.name0 && mDrawer.form.name0.trim())"/>
              <input v-else :value="mDrawer.name" disabled/></div>
            <div class="form-item"><label>类型</label>
              <select v-model="mDrawer.form.type">
                <option value="text">文本（chat / responses）</option>
                <option value="image">图像（images/generations）</option>
                <option value="router">路由（虚拟分流，按输入长度）</option>
              </select></div>
          </div>
          <div class="form-item" v-if="mDrawer.form.type==='text'"><label>上游协议</label>
            <select v-model="mDrawer.form.provider">
              <option value="">OpenAI 兼容（默认：chat/completions 透传）</option>
              <option value="anthropic">Anthropic（/v1/messages，网关自动转换）</option>
            </select>
            <div class="form-tip">仅 /v1/responses 需要 OpenAI 协议上游。</div></div>
          <div class="form-row">
            <div class="form-item"><label>显示名</label><input v-model="mDrawer.form.display"/></div>
            <div class="form-item"><label>状态</label>
              <div style="display:flex;align-items:center;gap:10px;height:34px">
                <ui-switch v-model="mDrawer.form.enabled"/>
                <span class="sh-note">{{ mDrawer.form.enabled ? '启用中' : '已停用（不承接任何流量）' }}</span>
              </div></div>
          </div>
          <div class="form-item"><label>描述</label><input v-model="mDrawer.form.description"/></div>
          <div v-if="mDrawer.catHint" class="form-tip blue">目录命中：{{ mDrawer.catHint }}（保存时自动补全空缺字段，人工填写优先）</div>
        </div>

        <div class="sec" v-if="mDrawer.form.type!=='router'">
          <div class="sec-title">价格与能力上限</div>
          <div class="form-row three" v-if="mDrawer.form.type==='image'">
            <div class="form-item"><label>图像单价（$ / 张）</label><input v-model.number="mDrawer.form.price_image" type="number" step="0.0001"/></div>
          </div>
          <div class="form-row" v-else>
            <div class="form-item"><label>输入单价（$ / 1M tokens）</label><input v-model.number="mDrawer.form.price_input" type="number" step="0.0001"/></div>
            <div class="form-item"><label>输出单价（$ / 1M tokens）</label><input v-model.number="mDrawer.form.price_output" type="number" step="0.0001"/></div>
          </div>
          <div class="form-row" v-if="mDrawer.form.type==='text'">
            <div class="form-item"><label>上下文窗口（tokens，0 = 不校验）</label><input v-model.number="mDrawer.form.context_tokens" type="number"/></div>
            <div class="form-item"><label>最大输出（tokens，0 = 不裁剪）</label><input v-model.number="mDrawer.form.max_output_tokens" type="number"/></div>
          </div>
          <div class="form-tip" v-if="mDrawer.form.type!=='image'">0 表示未设置；目录补全只填空缺字段，人工填写优先。</div>
        </div>

        <div class="sec" v-if="mDrawer.form.type==='router'">
          <div class="sec-title">虚拟路由模型</div>
          <div class="form-tip">路由模型不承接上游流量：保存基本信息后，请到「分流配置」页设置输入长度分流规则。</div>
        </div>

        <div class="sec" v-else>
          <div class="sec-title">接入点映射 <span class="cnt">{{ mDrawer.epRows.length }}</span>
            <div class="spacer" style="flex:1"></div>
            <button class="btn btn-outline btn-sm" :disabled="!mDrawer.name" @click="addEpRow">+ 添加接入点</button>
          </div>
          <div class="sec-tip" v-if="!mDrawer.name">模型尚未保存：先保存基础信息，再添加接入点。</div>
          <div class="sec-tip" v-else>同一账号 × 同一模型可挂多个接入点（如同模型的不同发布版本），按权重分摊；fallback 链在「分流配置」页编排。</div>

          <div v-for="(row, ri) in mDrawer.epRows" :key="row.id || 'new' + ri" class="ep-card" :class="{off: !row.form.enabled}">
            <div class="ep-card-head">
              <select v-model="row.form.account_id" @change="row.opts=[]">
                <option v-for="a in accounts" :key="a.id" :value="a.id">{{ a.name }}</option>
              </select>
              <input class="ep-input mono" v-model="row.form.ep" placeholder="上游模型标识，如 ep-2025xxx / gpt-4o"/>
              <button class="btn btn-outline btn-sm" :disabled="row.optsLoading || !row.form.account_id" @click="fetchRowEp(row)">{{ row.optsLoading ? '拉取中…' : '⇩ 拉取' }}</button>
              <ui-switch v-model="row.form.enabled" size="sm"/>
            </div>
            <div class="ep-card-body">
              <div v-if="row.opts.length" class="ep-picker">
                <span v-for="o in row.opts" :key="o" class="ep-chip" :class="{active: row.form.ep === o}" @click="row.form.ep = o">{{ o }}</span>
              </div>
              <div class="ep-metrics">
                <div><label>权重（0 = 继承账号）</label><input v-model.number="row.form.weight" type="number"/></div>
                <div><label>并发上限</label><input v-model.number="row.form.max_concurrency" type="number"/></div>
                <div><label>RPM</label><input v-model.number="row.form.rpm_limit" type="number"/></div>
                <div><label>TPM</label><input v-model.number="row.form.tpm_limit" type="number"/></div>
              </div>
              <div class="ep-headers">
                <span class="ep-headers-toggle" @click="row.headersOpen = !row.headersOpen">
                  上游请求头
                  <span v-if="headerCount(row)" class="headers-badge">{{ headerCount(row) }} 项</span>
                  <span class="arr">{{ row.headersOpen ? '▾' : '▸' }}</span>
                </span>
                <div v-show="row.headersOpen" style="margin-top:8px">
                  <div class="row-actions" style="margin-bottom:6px">
                    <button class="btn btn-outline btn-sm" type="button" @click="applyHeaderPreset('blank', row)">白板</button>
                    <button class="btn btn-outline btn-sm" type="button" @click="applyHeaderPreset('claude', row)">Claude Code</button>
                    <button class="btn btn-outline btn-sm" type="button" @click="applyHeaderPreset('codex', row)">Codex</button>
                    <button class="btn btn-outline btn-sm" type="button" @click="applyHeaderPreset('hermes', row)">Hermes</button>
                  </div>
                  <textarea v-model="row.form.request_headers_text" rows="5" spellcheck="false" placeholder='{"User-Agent":"Hermes/1.0"}'></textarea>
                  <div class="form-tip">使上游认为特定客户端在调用；认证、传输级与协议相关敏感头不可覆盖。</div>
                </div>
              </div>
              <div class="ep-card-foot">
                <span v-if="sibEps(row).length" class="sh-note" :title="sibEps(row).map(x => x.ep).join('、')">同账号同模型另有 {{ sibEps(row).length }} 个接入点</span>
                <div class="spacer"></div>
                <probe-test v-if="row.id && row.form.account_id && row.form.ep" :payload="{model: mDrawer.name, account_id: row.form.account_id, ep: row.form.ep}" label="测试"/>
                <span v-if="dirtyRow(row)" class="ep-dirty-dot" title="有未保存的修改"></span>
                <button class="btn btn-danger btn-sm" @click="removeEpRow(row)">{{ row.id ? '删除' : '取消' }}</button>
                <button class="btn btn-primary btn-sm" :disabled="!dirtyRow(row) || row.saving" @click="saveRow(row)">{{ row.saving ? '保存中…' : '保存接入点' }}</button>
              </div>
            </div>
          </div>
          <div v-if="mDrawer.name && !mDrawer.epRows.length" class="ep-empty">该模型还没有接入点映射，点上方「+ 添加接入点」</div>
        </div>
      </template>
      <template #foot>
        <button class="btn btn-outline" @click="mDrawer=null">关闭</button>
        <button class="btn btn-primary" :disabled="savingModel" @click="saveModel">{{ savingModel ? '保存中…' : (mDrawer && mDrawer.name ? '保存模型信息' : '创建模型') }}</button>
      </template>
    </ui-drawer>

    <!-- 从上游导入抽屉 -->
    <ui-drawer :open="!!iDrawer" :width="600" title="从上游导入模型" @close="iDrawer=null">
      <template v-if="iDrawer">
        <div class="sec">
          <div class="sec-title">选择账号</div>
          <div class="form-item"><label>账号（用其凭据请求上游 GET /models）</label>
            <div style="display:flex;gap:8px">
              <select v-model="iDrawer.account_id">
                <option v-for="a in accounts" :key="a.id" :value="a.id">{{ a.name }}</option>
              </select>
              <button class="btn btn-outline" style="white-space:nowrap" :disabled="iDrawer.loading" @click="fetchImport">{{ iDrawer.loading ? '拉取中…' : '⇩ 拉取列表' }}</button>
            </div>
            <div class="form-tip">导入会为每个选中项建立「同名模型 + 映射（上游标识 = 模型 id）」，价格与能力上限走目录自动补全；已存在的自动跳过。类型默认文本，图像模型请导入后到工作台里改。</div></div>
        </div>
        <div class="sec" v-if="iDrawer.list.length">
          <div class="sec-title">上游模型 <span class="cnt">{{ filteredImportList().length }} / {{ iDrawer.list.length }}</span></div>
          <div class="row-actions" style="margin-bottom:10px">
            <input v-model="iDrawer.query" placeholder="搜索上游模型" style="width:210px"/>
            <button class="btn btn-outline btn-sm" @click="pickAll(true)">全选</button>
            <button class="btn btn-outline btn-sm" @click="pickAll(false)">全不选</button>
          </div>
          <div class="import-list">
            <label v-for="it in filteredImportList()" :key="it.id" class="import-row">
              <input type="checkbox" :checked="iDrawer.picked[it.id]" @change="togglePick(it.id)"/>
              <span class="mono import-model">{{ it.id }}</span>
              <span v-if="importMapped(it.id)" class="tag tag-gray">已映射</span>
              <div class="spacer" style="flex:1"></div>
              <probe-test :payload="{account_id: iDrawer.account_id, ep: it.id}"/>
            </label>
            <div v-if="!filteredImportList().length" class="empty">没有匹配的上游模型</div>
          </div>
        </div>
        <div class="sec" v-else><div class="empty">先选择账号并拉取列表</div></div>
      </template>
      <template #foot>
        <button class="btn btn-outline" @click="iDrawer=null">取消</button>
        <button class="btn btn-primary" :disabled="iDrawer.saving || !iDrawer.list.length" @click="doImport">{{ iDrawer.saving ? '导入中…' : '导入选中' }}</button>
      </template>
    </ui-drawer>
  </div>`,
};
