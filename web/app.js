/* ArkGate 前端 —— 无框架单页应用，模仿 Arco 设计语言 */
(function () {
  "use strict";

  var token = localStorage.getItem("arkgate_token") || "";

  function h(html) {
    var t = document.createElement("template");
    t.innerHTML = html.trim();
    return t.content.firstElementChild;
  }

  function esc(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function toast(msg, ok) {
    var t = h('<div class="toast ' + (ok ? "ok" : "err") + '">' + esc(msg) + "</div>");
    document.body.appendChild(t);
    setTimeout(function () { t.remove(); }, 2600);
  }

  // ── API 封装 ──
  function req(method, path, body) {
    return fetch(path, {
      method: method,
      headers: {
        "Content-Type": "application/json",
        "Authorization": token ? "Bearer " + token : "",
      },
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (d) {
        if (!r.ok) {
          var m = d.detail || d.error || (r.status + " error");
          var e = new Error(typeof m === "string" ? m : JSON.stringify(m));
          e.status = r.status;
          throw e;
        }
        // 后端空列表可能序列化为 null，统一兜底为 []，避免前端 .map/.forEach 崩溃。
        if (d === null || d === undefined) return [];
        return d;
      });
    });
  }

  function authStatus() { return req("GET", "/api/auth/status"); }

  // ── 视图注册 ──
  var routes = {};
  function register(name, render, sidebar) {
    routes[name] = { render: render, sidebar: sidebar !== false };
  }

  function fmtTokens(n) {
    n = Number(n || 0);
    if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
    return String(n);
  }
  function fmtTime(unix) {
    if (!unix) return "-";
    var d = new Date(unix * 1000);
    function p(n) { return (n < 10 ? "0" : "") + n; }
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  }
  function statusTag(s) {
    var map = {
      "active": '<span class="tag tag-green">启用</span>',
      "disabled": '<span class="tag tag-gray">禁用</span>',
    };
    return map[s] || '<span class="tag tag-gray">' + esc(s) + "</span>";
  }

  // ── 总览 ──
  register("overview", function (root) {
    req("GET", "/api/overview").then(function (d) {
      var accs = d.accounts || [];
      var totalReq = 0, totalTok = 0;
      accs.forEach(function (a) { totalReq += a.total_requests; totalTok += a.total_tokens; });
      root.innerHTML = "";
      root.appendChild(h(
        '<div class="page">' +
          '<div class="banner"><div>' +
            '<div class="badge"><span class="pulse"></span>运行中</div>' +
            '<h2>ArkGate · 火山方舟多账号网关</h2>' +
            '<p>多账号加权轮询 · 熔断 · 限流 · OpenAI 兼容 /v1 端点</p>' +
          '</div><div class="row-actions">' +
            '<button class="btn btn-outline" id="btn-dark">🌓 深色</button>' +
          '</div></div>' +
          '<div class="stat-row">' +
            stat('账号总数', d.account_total, 'ic-blue', '🏛') +
            stat('启用中', d.account_active, 'ic-green', '✅') +
            stat('熔断中', d.account_circuit, 'ic-red', '⏳') +
            stat('模型目录', d.model_count, 'ic-purple', '🧩') +
            stat('子 Key', d.subkey_count, 'ic-orange', '🔑') +
            stat('总请求', totalReq, 'ic-blue', '⚡') +
            stat('总 Token', fmtTokens(totalTok), 'ic-green', '⬤') +
          '</div>' +
          '<div class="card"><div class="card-head"><div class="card-title">账号状态</div></div>' +
            '<div class="table-wrap"><table><thead><tr>' +
            '<th>账号</th><th>状态</th><th>权重</th><th>请求</th><th>成功</th><th>失败</th><th>Token</th><th>最后使用</th>' +
            '</tr></thead><tbody>' + (accs.length ? accs.map(function (a) {
              var circuit = a.Runtime && a.Runtime.CircuitOpenUntil > Date.now() / 1e6 ? " (熔断)" : "";
              return "<tr><td>" + esc(a.name) + "</td><td>" + statusTag(a.status) + circuit +
                "</td><td>" + esc(a.weight) + "</td><td>" + a.total_requests +
                "</td><td class='v' style='color:rgb(var(--green-6))'>" + a.success_requests +
                "</td><td class='v' style='color:rgb(var(--red-6))'>" + a.fail_requests +
                "</td><td>" + fmtTokens(a.total_tokens) + "</td><td>" + fmtTime(a.last_used_at) + "</td></tr>";
            }).join("") : '<tr><td colspan="8" class="empty">暂无账号，请先添加</td></tr>') +
            "</tbody></table></div></div>" +
        "</div>"
      ));
      bindDark(root);
    }).catch(function (e) { root.innerHTML = ""; });
  });

  function stat(label, value, icCls, icGlyph) {
    return '<div class="stat-card"><div class="ic ' + icCls + '">' + icGlyph + '</div>' +
      '<div class="body"><div class="v">' + esc(value) + '</div><div class="l">' + esc(label) + '</div></div></div>';
  }

  function bindDark(root) {
    var b = root.querySelector("#btn-dark");
    if (b) b.addEventListener("click", function () {
      var cur = document.documentElement.getAttribute("data-theme");
      var next = cur === "dark" ? "" : "dark";
      document.documentElement.setAttribute("data-theme", next);
      localStorage.setItem("arkgate_theme", next);
    });
  }

  // ── 账号 ──
  register("accounts", function (root) {
    function load() {
      req("GET", "/api/accounts").then(function (accs) {
        root.innerHTML = "";
        root.appendChild(h(
          '<div class="page">' +
            '<div class="page-title">火山账号</div><div class="page-sub">每个账号对应一个火山方舟长效 API Key，网关会在请求时动态替换。</div>' +
            '<div class="toolbar"><button class="btn btn-primary" id="add">+ 添加账号</button><div class="spacer"></div></div>' +
            '<div class="card"><div class="table-wrap"><table><thead><tr>' +
            '<th>名称</th><th>Key</th><th>状态</th><th>权重</th><th>请求/成功/失败</th><th>Token</th><th>操作</th>' +
            '</tr></thead><tbody>' + (accs.length ? accs.map(function (a) {
              return "<tr><td><strong>" + esc(a.name) + "</strong></td><td class='mono'>" + esc(a.key_hint) +
                "</td><td>" + statusTag(a.status) + "</td><td>" + esc(a.weight) +
                "</td><td>" + a.total_requests + "/" + a.success_requests + "/" + a.fail_requests +
                "</td><td>" + fmtTokens(a.total_tokens) +
                "</td><td><div class='row-actions'>" +
                '<button class="btn btn-outline btn-sm" data-edit="' + esc(a.id) + '">编辑</button>' +
                '<button class="btn btn-danger btn-sm" data-del="' + esc(a.id) + '">删除</button>' +
                "</div></td></tr>";
            }).join("") : '<tr><td colspan="7" class="empty">暂无账号</td></tr>') +
            "</tbody></table></div></div>" +
          "</div>"
        ));

        root.querySelector("#add").onclick = function () { showAccountModal(null, load); };
        root.querySelectorAll("[data-edit]").forEach(function (b) {
          b.onclick = function () { showAccountModal(b.getAttribute("data-edit"), load); };
        });
        root.querySelectorAll("[data-del]").forEach(function (b) {
          b.onclick = function () {
            if (!confirm("确认删除该账号？将同时删除其模型映射。")) return;
            req("DELETE", "/api/accounts/" + b.getAttribute("data-del")).then(function () { toast("已删除"); load(); });
          };
        });
      });
    }
    load();
  });

  function showAccountModal(id, done) {
    var isEdit = !!id;
    var title = isEdit ? "编辑账号" : "添加账号";
    var modal = h(
      '<div class="modal-mask"><div class="modal"><div class="modal-head"><h3>' + title + '</h3>' +
      '<button class="modal-close">×</button></div><div class="modal-body">' +
      '<div class="form-item"><label>名称 <span class="req">*</span></label><input id="f-name" placeholder="例如：主账号-北京"/></div>' +
      '<div class="form-item"><label>方舟 API Key <span class="req">*</span>' + (isEdit ? "（留空表示不修改）" : "") + '</label><input id="f-key" placeholder="火山方舟长效 API Key"/></div>' +
      '<div class="form-row">' +
      '<div class="form-item"><label>权重</label><input id="f-weight" type="number" value="1"/></div>' +
      '<div class="form-item"><label>状态</label><select id="f-status"><option value="active">启用</option><option value="disabled">禁用</option></select></div>' +
      '</div>' +
      '<div class="form-item"><label style="color:var(--color-text-3)">并发 / RPM / TPM 限额请到「模型映射」按接入点配置。</label></div>' +
      '</div><div class="modal-foot">' +
      '<button class="btn btn-outline" id="cancel">取消</button>' +
      '<button class="btn btn-primary" id="save">保存</button>' +
      '</div></div></div>'
    );
    document.body.appendChild(modal);
    function close() { modal.remove(); }
    modal.querySelector(".modal-close").onclick = close;
    modal.querySelector("#cancel").onclick = close;
    modal.addEventListener("click", function (e) { if (e.target === modal) close(); });

    if (isEdit) {
      req("GET", "/api/accounts").then(function (accs) {
        var a = accs.find(function (x) { return x.id === id; });
        if (!a) return;
        modal.querySelector("#f-name").value = a.name;
        modal.querySelector("#f-weight").value = a.weight;
        modal.querySelector("#f-status").value = a.status;
      });
    }

    modal.querySelector("#save").onclick = function () {
      var payload = {
        name: modal.querySelector("#f-name").value.trim(),
        ark_api_key: modal.querySelector("#f-key").value.trim(),
        weight: parseInt(modal.querySelector("#f-weight").value, 10) || 1,
        status: modal.querySelector("#f-status").value,
      };
      if (!payload.name) { toast("请输入名称", false); return; }
      if (!isEdit && !payload.ark_api_key) { toast("请输入 API Key", false); return; }
      var p = isEdit
        ? req("PUT", "/api/accounts/" + id, payload)
        : req("POST", "/api/accounts", payload);
      p.then(function () { toast("已保存"); close(); done && done(); })
       .catch(function (e) { toast(e.message, false); });
    };
  }

  // ── 模型 & 映射 ──
  register("models", function (root) {
    renderModels();
    function renderModels() {
      Promise.all([req("GET", "/api/models"), req("GET", "/api/accounts"), req("GET", "/api/endpoints")])
        .then(function (rs) {
          var models = rs[0], accounts = rs[1], eps = rs[2];
          var accName = {};
          accounts.forEach(function (a) { accName[a.id] = a.name; });

          root.innerHTML = "";
          root.appendChild(h(
            '<div class="page">' +
              '<div class="page-title">模型映射</div>' +
              '<div class="page-sub">把可读性差的 ep-xxx… 接入点映射为易读模型名；下游用它调用，网关调用火山时自动换回 ep-。每个账号的接入点天然不同，因此映射按「账号 × 模型」维护。</div>' +
              '<div class="toolbar"><button class="btn btn-primary" id="add-model">+ 新建模型</button>' +
              '<button class="btn btn-outline" id="add-ep">+ 添加映射</button><div class="spacer"></div></div>' +
              '<div class="card"><div class="card-head"><div class="card-title">模型目录</div></div>' +
              '<div class="table-wrap"><table><thead><tr><th>模型名</th><th>显示名</th><th>描述</th><th>状态</th><th>操作</th></tr></thead><tbody>' +
              (models.length ? models.map(function (m) {
                return "<tr><td class='mono'>" + esc(m.name) + "</td><td>" + esc(m.display) + "</td><td>" + esc(m.description) +
                  "</td><td>" + (m.enabled ? '<span class="tag tag-green">启用</span>' : '<span class="tag tag-gray">停用</span>') +
                  "</td><td><div class='row-actions'>" +
                  '<button class="btn btn-danger btn-sm" data-del-model="' + esc(m.name) + '">删除</button></div></td></tr>';
              }).join("") : '<tr><td colspan="5" class="empty">暂无模型</td></tr>') +
              "</tbody></table></div></div>" +
              '<div class="card"><div class="card-head"><div class="card-title">接入点映射（元组级流控）</div></div>' +
              '<div class="table-wrap"><table><thead><tr><th>账号</th><th>模型名</th><th>真实接入点 (ep-)</th><th>权重</th><th>并发</th><th>RPM</th><th>TPM</th><th>状态</th><th>操作</th></tr></thead><tbody>' +
              (eps.length ? eps.map(function (e) {
                return "<tr><td>" + esc(e.account_name || e.account_id) + "</td><td class='mono'>" + esc(e.model) +
                  "</td><td class='mono'>" + esc(e.ep) + "</td><td>" + (e.weight || "继承") +
                  "</td><td>" + (e.max_concurrency || "不限") + "</td><td>" + (e.rpm_limit || "不限") + "</td><td>" + (e.tpm_limit || "不限") +
                  "</td><td>" + (e.enabled ? '<span class="tag tag-green">启用</span>' : '<span class="tag tag-gray">停用</span>') +
                  "</td><td><div class='row-actions'><button class='btn btn-outline btn-sm' data-edit-ep='" + esc(e.id) + "'>编辑</button>" +
                  '<button class="btn btn-danger btn-sm" data-del-ep="' + esc(e.id) + '">删除</button></div></td></tr>';
              }).join("") : '<tr><td colspan="9" class="empty">暂无映射</td></tr>') +
              "</tbody></table></div></div>" +
            "</div>"
          ));

          root.querySelector("#add-model").onclick = function () {
            var modal = h(
              '<div class="modal-mask"><div class="modal"><div class="modal-head"><h3>新建模型</h3><button class="modal-close">×</button></div>' +
              '<div class="modal-body">' +
              '<div class="form-item"><label>模型名（下游调用用，唯一）<span class="req">*</span></label><input id="m-name" placeholder="例如 doubao-seed-1-6"/></div>' +
              '<div class="form-item"><label>显示名</label><input id="m-display" placeholder="豆包 Seed 1.6"/></div>' +
              '<div class="form-item"><label>描述</label><input id="m-desc"/></div>' +
              '</div><div class="modal-foot"><button class="btn btn-outline" id="m-cancel">取消</button><button class="btn btn-primary" id="m-save">保存</button></div></div></div>'
            );
            document.body.appendChild(modal);
            function close() { modal.remove(); }
            modal.querySelector(".modal-close").onclick = close;
            modal.querySelector("#m-cancel").onclick = close;
            modal.querySelector("#m-save").onclick = function () {
              var name = modal.querySelector("#m-name").value.trim();
              if (!name) { toast("请输入模型名", false); return; }
              req("POST", "/api/models", { name: name, display: modal.querySelector("#m-display").value.trim() || name, description: modal.querySelector("#m-desc").value.trim() })
                .then(function () { toast("已保存"); close(); renderModels(); });
            };
          };

          root.querySelector("#add-ep").onclick = function () {
            var opts = accounts.map(function (a) {
              return '<option value="' + esc(a.id) + '">' + esc(a.name) + "</option>";
            }).join("");
            var mopts = models.map(function (m) {
              return '<option value="' + esc(m.name) + '">' + esc(m.name) + "</option>";
            }).join("");
            var modal = h(
              '<div class="modal-mask"><div class="modal"><div class="modal-head"><h3>添加接入点映射</h3><button class="modal-close">×</button></div>' +
              '<div class="modal-body">' +
              '<div class="form-item"><label>账号 <span class="req">*</span></label><select id="e-acc">' + opts + "</select></div>" +
              '<div class="form-item"><label>模型名 <span class="req">*</span></label><select id="e-model">' + mopts + "</select></div>" +
              '<div class="form-item"><label>真实接入点 <span class="req">*</span></label><input id="e-ep" placeholder="ep-2025xxxxxxx-xxxxx"/></div>' +
              '<div class="form-row three">' +
              '<div class="form-item"><label>权重</label><input id="e-weight" type="number" value="0"/></div>' +
              '<div class="form-item"><label>并发上限</label><input id="e-conc" type="number" value="0"/></div>' +
              '<div class="form-item"><label>RPM</label><input id="e-rpm" type="number" value="0"/></div>' +
              '</div>' +
              '<div class="form-item"><label>TPM</label><input id="e-tpm" type="number" value="0"/></div>' +
              '</div><div class="modal-foot"><button class="btn btn-outline" id="e-cancel">取消</button><button class="btn btn-primary" id="e-save">保存</button></div></div></div>'
            );
            document.body.appendChild(modal);
            function close() { modal.remove(); }
            modal.querySelector(".modal-close").onclick = close;
            modal.querySelector("#e-cancel").onclick = close;
            modal.querySelector("#e-save").onclick = function () {
              var acc = modal.querySelector("#e-acc").value;
              var model = modal.querySelector("#e-model").value;
              var ep = modal.querySelector("#e-ep").value.trim();
              if (!acc || !model || !ep) { toast("请完整填写", false); return; }
              req("POST", "/api/endpoints", {
                account_id: acc, model: model, ep: ep,
                weight: parseInt(modal.querySelector("#e-weight").value, 10) || 0,
                max_concurrency: parseInt(modal.querySelector("#e-conc").value, 10) || 0,
                rpm_limit: parseInt(modal.querySelector("#e-rpm").value, 10) || 0,
                tpm_limit: parseInt(modal.querySelector("#e-tpm").value, 10) || 0,
              }).then(function () { toast("已保存"); close(); renderModels(); });
            };
          };

          root.querySelectorAll("[data-edit-ep]").forEach(function (b) {
            b.onclick = function () {
              var id = b.getAttribute("data-edit-ep");
              req("GET", "/api/endpoints").then(function (eps) {
                var e = eps.find(function (x) { return x.id === id; });
                if (!e) return;
                var modal = h(
                  '<div class="modal-mask"><div class="modal"><div class="modal-head"><h3>编辑接入点映射</h3><button class="modal-close">×</button></div>' +
                  '<div class="modal-body">' +
                  '<div class="form-item"><label>真实接入点</label><input id="x-ep" value="' + esc(e.ep) + '"/></div>' +
                  '<div class="form-row three">' +
                  '<div class="form-item"><label>权重</label><input id="x-weight" type="number" value="' + (e.weight || 0) + '"/></div>' +
                  '<div class="form-item"><label>并发上限</label><input id="x-conc" type="number" value="' + (e.max_concurrency || 0) + '"/></div>' +
                  '<div class="form-item"><label>RPM</label><input id="x-rpm" type="number" value="' + (e.rpm_limit || 0) + '"/></div>' +
                  '</div>' +
                  '<div class="form-item"><label>TPM</label><input id="x-tpm" type="number" value="' + (e.tpm_limit || 0) + '"/></div>' +
                  '<div class="form-item"><label>状态</label><select id="x-enabled"><option value="true"' + (e.enabled ? " selected" : "") + '>启用</option><option value="false"' + (!e.enabled ? " selected" : "") + '>停用</option></select></div>' +
                  '</div><div class="modal-foot"><button class="btn btn-outline" id="x-cancel">取消</button><button class="btn btn-primary" id="x-save">保存</button></div></div></div>'
                );
                document.body.appendChild(modal);
                function close() { modal.remove(); }
                modal.querySelector(".modal-close").onclick = close;
                modal.querySelector("#x-cancel").onclick = close;
                modal.querySelector("#x-save").onclick = function () {
                  req("PUT", "/api/endpoints/" + id, {
                    ep: modal.querySelector("#x-ep").value.trim() || e.ep,
                    weight: parseInt(modal.querySelector("#x-weight").value, 10) || 0,
                    max_concurrency: parseInt(modal.querySelector("#x-conc").value, 10) || 0,
                    rpm_limit: parseInt(modal.querySelector("#x-rpm").value, 10) || 0,
                    tpm_limit: parseInt(modal.querySelector("#x-tpm").value, 10) || 0,
                    enabled: modal.querySelector("#x-enabled").value === "true",
                  }).then(function () { toast("已保存"); close(); renderModels(); });
                };
              });
            };
          });

          root.querySelectorAll("[data-del-model]").forEach(function (b) {
            b.onclick = function () {
              if (!confirm("确认删除该模型及其所有映射？")) return;
              req("DELETE", "/api/models/" + b.getAttribute("data-del-model")).then(function () { toast("已删除"); renderModels(); });
            };
          });
          root.querySelectorAll("[data-del-ep]").forEach(function (b) {
            b.onclick = function () {
              if (!confirm("确认删除该映射？")) return;
              req("DELETE", "/api/endpoints/" + b.getAttribute("data-del-ep")).then(function () { toast("已删除"); renderModels(); });
            };
          });
        });
    }
  });

  // ── 子 Key ──
  register("subkeys", function (root) {
    function load() {
      req("GET", "/api/subkeys").then(function (subs) {
        root.innerHTML = "";
        root.appendChild(h(
          '<div class="page">' +
            '<div class="page-title">子 API Key</div>' +
            '<div class="page-sub">下发给客户端使用的 OpenAI 兼容 Key（sk-xxx）。真正的火山 Key 不出网，网关按白名单与限额路由。</div>' +
            '<div class="toolbar"><button class="btn btn-primary" id="add">+ 新建子 Key</button><div class="spacer"></div></div>' +
            '<div class="card"><div class="table-wrap"><table><thead><tr>' +
            '<th>名称</th><th>Key</th><th>状态</th><th>可访问模型</th><th>请求</th><th>Token</th><th>操作</th>' +
            '</tr></thead><tbody>' + (subs.length ? subs.map(function (s) {
              return "<tr><td><strong>" + esc(s.name || s.id) + "</strong></td>" +
                "<td class='mono'><span class='code-copy' title='点击复制' data-copy='" + esc(s.key) + "'>" + esc(s.key) + "</span></td>" +
                "<td>" + (s.enabled ? '<span class="tag tag-green">启用</span>' : '<span class="tag tag-gray">禁用</span>') + "</td>" +
                "<td>" + (s.allowed_models && s.allowed_models.length ? s.allowed_models.join(", ") : "全部") + "</td>" +
                "<td>" + s.total_requests + "</td><td>" + fmtTokens(s.total_tokens) + "</td>" +
                "<td><div class='row-actions'>" +
                '<button class="btn btn-outline btn-sm" data-edit="' + esc(s.id) + '" data-now=\'' + JSON.stringify({ enabled: s.enabled, name: s.name }).replace(/'/g, "&#39;") + '\'>编辑</button>' +
                '<button class="btn btn-danger btn-sm" data-del="' + esc(s.id) + '">删除</button>' +
                "</div></td></tr>";
            }).join("") : '<tr><td colspan="7" class="empty">暂无子 Key</td></tr>') +
            "</tbody></table></div></div>" +
          "</div>"
        ));
        root.querySelector("#add").onclick = function () { showSubkeyModal(null, load); };
        root.querySelectorAll("[data-edit]").forEach(function (b) {
          b.onclick = function () { showSubkeyModal(b.getAttribute("data-edit"), load); };
        });
        root.querySelectorAll("[data-del]").forEach(function (b) {
          b.onclick = function () {
            if (!confirm("确认删除该子 Key？")) return;
            req("DELETE", "/api/subkeys/" + b.getAttribute("data-del")).then(function () { toast("已删除"); load(); });
          };
        });
        root.querySelectorAll("[data-copy]").forEach(function (el) {
          el.onclick = function () {
            navigator.clipboard.writeText(el.getAttribute("data-copy")).then(function () { toast("已复制"); });
          };
        });
      });
    }
    load();
  });

  function showSubkeyModal(id, done) {
    var isEdit = !!id;
    Promise.all([req("GET", "/api/models"), req("GET", "/api/accounts")]).then(function (rs) {
      var models = rs[0], accounts = rs[1];
      var mopts = models.map(function (m) { return '<label style="display:inline-flex;gap:4px;margin-right:12px"><input type="checkbox" value="' + esc(m.name) + '">' + esc(m.name) + "</label>"; }).join("");
      var aopts = accounts.map(function (a) { return '<label style="display:inline-flex;gap:4px;margin-right:12px"><input type="checkbox" value="' + esc(a.id) + '">' + esc(a.name) + "</label>"; }).join("");
      var modal = h(
        '<div class="modal-mask"><div class="modal"><div class="modal-head"><h3>' + (isEdit ? "编辑子 Key" : "新建子 Key") + '</h3><button class="modal-close">×</button></div>' +
        '<div class="modal-body">' +
        '<div class="form-item"><label>名称</label><input id="sk-name" placeholder="例如：给团队的 Key"/></div>' +
        (isEdit ? "" : '<div class="form-item"><label>自定义 Key（留空自动生成 sk-xxx）</label><input id="sk-key" placeholder="sk-..."/></div>') +
        '<div class="form-item"><label>可访问模型（不勾选 = 全部）</label><div class="form-item" style="max-height:120px;overflow:auto;border:1px solid var(--color-border-2);border-radius:6px;padding:8px;">' + (mopts || '<span class="tag tag-gray">暂无模型</span>') + "</div></div>" +
        '<div class="form-item"><label>可访问账号（不勾选 = 全部）</label><div class="form-item" style="max-height:120px;overflow:auto;border:1px solid var(--color-border-2);border-radius:6px;padding:8px;">' + (aopts || '<span class="tag tag-gray">暂无账号</span>') + "</div></div>" +
        '<div class="form-item"><label>当日 Token 限额（0 = 不限）</label><input id="sk-limit" type="number" value="0"/></div>' +
        '</div><div class="modal-foot"><button class="btn btn-outline" id="sk-cancel">取消</button><button class="btn btn-primary" id="sk-save">保存</button></div></div></div>'
      );
      document.body.appendChild(modal);
      function close() { modal.remove(); }
      modal.querySelector(".modal-close").onclick = close;
      modal.querySelector("#sk-cancel").onclick = close;
      modal.addEventListener("click", function (e) { if (e.target === modal) close(); });

      if (isEdit) {
        req("GET", "/api/subkeys").then(function (subs) {
          var s = subs.find(function (x) { return x.id === id; });
          if (!s) return;
          modal.querySelector("#sk-name").value = s.name;
          modal.querySelector("#sk-limit").value = s.daily_limit_tokens;
          modal.querySelectorAll('input[type=checkbox]').forEach(function (cb) {
            var allowed = (cb.value.length > 12 ? s.allowed_accounts : s.allowed_models) || [];
            cb.checked = allowed.indexOf(cb.value) >= 0;
          });
        });
      }

      modal.querySelector("#sk-save").onclick = function () {
        var allowedModels = [], allowedAccounts = [];
        modal.querySelectorAll('input[type=checkbox]:checked').forEach(function (cb) {
          // 用名称/ID 长度粗分：账号 id 以 acc_ 开头，模型无此前缀。
          if (cb.value.indexOf("acc_") === 0) allowedAccounts.push(cb.value);
          else allowedModels.push(cb.value);
        });
        var payload = {
          name: modal.querySelector("#sk-name").value.trim() || "未命名",
          allowed_models: allowedModels,
          allowed_accounts: allowedAccounts,
          daily_limit_tokens: parseInt(modal.querySelector("#sk-limit").value, 10) || 0,
        };
        var keyEl = modal.querySelector("#sk-key");
        if (keyEl && keyEl.value.trim()) payload.key = keyEl.value.trim();
        var p;
        if (isEdit) {
          p = req("PUT", "/api/subkeys/" + id, { name: payload.name, allowed_models: payload.allowed_models, allowed_accounts: payload.allowed_accounts, daily_limit_tokens: payload.daily_limit_tokens });
        } else {
          p = req("POST", "/api/subkeys", payload);
        }
        p.then(function (d) {
          if (d.key) { toast("已创建，Key：" + d.key); } else { toast("已保存"); }
          close(); done && done();
        }).catch(function (e) { toast(e.message, false); });
      };
    });
  }

  // ── 请求日志 ──
  register("logs", function (root) {
    function load() {
      req("GET", "/api/logs?limit=300").then(function (logs) {
        root.innerHTML = "";
        root.appendChild(h(
          '<div class="page">' +
            '<div class="page-title">请求日志</div><div class="page-sub">子 Key 维度的用量与错误记录（不含火山上游同步）。</div>' +
            '<div class="toolbar"><button class="btn btn-outline" id="refresh">刷新</button>' +
            '<button class="btn btn-danger" id="clear">清空日志</button><div class="spacer"></div></div>' +
            '<div class="card"><div class="table-wrap"><table><thead><tr>' +
            '<th>时间</th><th>子 Key</th><th>账号</th><th>模型</th><th>输入</th><th>输出</th><th>总 Token</th><th>耗时</th><th>状态</th><th>错误</th>' +
            '</tr></thead><tbody>' + (logs.length ? logs.map(function (l) {
              return "<tr><td>" + fmtTime(l.ts) + "</td><td>" + esc(l.subkey_name || l.subkey_id) +
                "</td><td>" + esc(l.account_name || l.account_id) + "</td><td class='mono'>" + esc(l.model) +
                "</td><td>" + l.prompt_tokens + "</td><td>" + l.completion_tokens + "</td><td>" + l.total_tokens +
                "</td><td>" + l.latency_ms + "ms</td><td>" + (l.status === "ok" ? '<span class="tag tag-green">OK</span>' : '<span class="tag tag-red">ERR</span>') +
                "</td><td style='max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--color-text-3)' title='" + esc(l.error) + "'>" + esc(l.error) + "</td></tr>";
            }).join("") : '<tr><td colspan="10" class="empty">暂无日志</td></tr>') +
            "</tbody></table></div></div>" +
          "</div>"
        ));
        root.querySelector("#refresh").onclick = load;
        root.querySelector("#clear").onclick = function () {
          if (!confirm("确认清空所有日志？")) return;
          req("DELETE", "/api/logs").then(function () { toast("已清空"); load(); });
        };
      });
    }
    load();
  });

  // ── 设置 ──
  register("settings", function (root) {
    req("GET", "/api/overview").then(function (d) {
      var host = location.origin;
      root.innerHTML = "";
      root.appendChild(h(
        '<div class="page">' +
          '<div class="page-title">使用说明</div><div class="page-sub">把 ArkGate 当作 OpenAI 兼容端点接入你的客户端。</div>' +
          '<div class="card"><div class="card-head"><div class="card-title">OpenAI 兼容端点</div></div>' +
          '<div class="kv"><span class="k">Base URL</span><span class="v mono code-copy" data-copy="' + host + '/v1">' + host + "/v1</span></div>" +
          '<div class="kv"><span class="k">Chat Completions</span><span class="v mono">POST /v1/chat/completions</span></div>' +
          '<div class="kv"><span class="k">模型列表</span><span class="v mono">GET /v1/models</span></div>' +
          "</div>" +
          '<div class="card"><div class="card-head"><div class="card-title">调用示例</div></div>' +
          '<pre style="background:var(--color-fill-1);border-radius:8px;padding:16px;overflow:auto;font-size:12px;font-family:ui-monospace,Menlo,monospace">' + esc(
            'curl ' + host + '/v1/chat/completions \\\n' +
            '  -H "Authorization: Bearer sk-你的子Key" \\\n' +
            '  -H "Content-Type: application/json" \\\n' +
            '  -d \'{\n' +
            '    "model": "doubao-seed-1-6",\n' +
            '    "messages": [{"role":"user","content":"你好"}],\n' +
            '    "stream": false\n' +
            '  }\''
          ) + "</pre></div>" +
          '<div class="card"><div class="card-head"><div class="card-title">环境变量</div></div>' +
          '<div class="kv"><span class="k">ARKGATE_ADDR</span><span class="v mono">监听地址（默认 0.0.0.0:8002）</span></div>' +
          '<div class="kv"><span class="k">ARKGATE_BASE_URL</span><span class="v mono">火山 Base URL（默认 cn-beijing）</span></div>' +
          '<div class="kv"><span class="k">ARKGATE_DATA_DIR</span><span class="v mono">数据目录（默认 ~/.arkgate）</span></div>' +
          "</div>" +
        "</div>"
      ));
      root.querySelectorAll("[data-copy]").forEach(function (el) {
        el.onclick = function () { navigator.clipboard.writeText(el.getAttribute("data-copy")).then(function () { toast("已复制"); }); };
      });
    });
  });

  // ── 路由与外壳 ──
  var MENU = [
    { key: "overview", label: "总览", icon: "📊" },
    { key: "accounts", label: "火山账号", icon: "🏛" },
    { key: "models", label: "模型映射", icon: "🧩" },
    { key: "subkeys", label: "子 Key", icon: "🔑" },
    { key: "logs", label: "请求日志", icon: "📄" },
    { key: "settings", label: "使用说明", icon: "⚙️" },
  ];

  var ctx = { view: "overview", main: null };

  function shell() {
    var nav = MENU.map(function (m) {
      return '<div class="nav-item' + (ctx.view === m.key ? " active" : "") + '" data-view="' + m.key + '">' +
        '<span class="ic">' + m.icon + "</span>" + m.label + "</div>";
    }).join("");
    var el = h(
      '<div class="layout"><aside class="sidebar">' +
        '<div class="logo"><span class="dot"></span>ArkGate<span class="ver">v1.0</span></div>' +
        '<div class="nav">' + nav + "</div>" +
      '</aside><main class="main" id="main"></main></div>'
    );
    el.querySelectorAll("[data-view]").forEach(function (n) {
      n.onclick = function () { go(n.getAttribute("data-view")); };
    });
    return el;
  }

  function go(view) {
    var r = routes[view];
    if (!r) view = "overview";
    ctx.view = view;
    document.getElementById("app").innerHTML = "";
    document.getElementById("app").appendChild(shell());
    ctx.main = document.getElementById("main");
    routes[view].render(ctx.main);
  }

  function renderLogin() {
    authStatus().then(function (d) {
      var doc = document.getElementById("app");
      doc.innerHTML = "";
      doc.appendChild(h(
        '<div class="login-wrap"><div class="login-card">' +
          '<div class="logo"><span class="dot"></span>ArkGate</div>' +
          (d.initialized
            ? '<div class="sub">请输入访问令牌登录</div>'
            : '<div class="sub">首次使用，请设置访问令牌（至少 6 位）</div>') +
          '<div class="form-item"><label>访问令牌</label><input id="login-token" placeholder="访问令牌" autofocus/></div>' +
          '<button class="btn btn-primary" style="width:100%" id="login-btn">' + (d.initialized ? "登录" : "初始化") + "</button>" +
          "</div></div>"
      ));
      var input = doc.querySelector("#login-token");
      function submit() {
        var tok = input.value.trim();
        if (!tok) { toast("请输入令牌", false); return; }
        var p = d.initialized
          ? req("POST", "/api/auth/login", { token: tok })
          : req("POST", "/api/auth/setup", { token: tok });
        p.then(function (r) {
          token = tok;
          localStorage.setItem("arkgate_token", tok);
          go("overview");
        }).catch(function (e) { toast(e.message, false); });
      }
      doc.querySelector("#login-btn").onclick = submit;
      input.addEventListener("keydown", function (e) { if (e.key === "Enter") submit(); });
    });
  }

  function boot() {
    var theme = localStorage.getItem("arkgate_theme");
    if (theme) document.documentElement.setAttribute("data-theme", theme);
    // 未初始化时用 auth/status 判断；有 token 时直接进，由 API 401 兜底。
    if (!token) {
      renderLogin();
      return;
    }
    req("GET", "/api/auth/status").then(function (d) {
      if (!d.initialized) { renderLogin(); return; }
      req("GET", "/api/overview").then(function () { go("overview"); })
        .catch(function () { go("overview"); });
    }).catch(function () { renderLogin(); });
  }

  // 全局 401 兜底。
  var _origFetch = window.fetch;
  window.fetch = function (input, init) {
    return _origFetch(input, init).then(function (r) {
      if (r.status === 401 && (String(input).indexOf("/api/") >= 0) && token) {
        localStorage.removeItem("arkgate_token");
        token = "";
        renderLogin();
      }
      return r;
    });
  };

  document.addEventListener("DOMContentLoaded", boot);
})();