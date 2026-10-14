/* LLM API admin console — vanilla JS SPA */
(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  const state = {
    token: localStorage.getItem("admin_token") || "",
    route: "dashboard",
    cache: {},
  };

  /* ---------- helpers ---------- */
  const esc = (s) =>
    String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  const fmt = (n) => {
    if (n === null || n === undefined) return "-";
    if (typeof n === "number") return n.toLocaleString("zh-CN", { maximumFractionDigits: 2 });
    return String(n);
  };

  const fmtTime = (iso) => {
    if (!iso) return "-";
    const d = new Date(iso);
    const p = (x) => String(x).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  };

  async function api(path, opts = {}) {
    const headers = { "Content-Type": "application/json", ...(opts.headers || {}) };
    if (state.token) headers["Authorization"] = "Bearer " + state.token;
    const res = await fetch(path, { ...opts, headers });
    let body = null;
    try { body = await res.json(); } catch (_) {}
    if (res.status === 401 && !path.includes("/login")) {
      logout();
      throw new Error("登录已过期");
    }
    if (!res.ok) {
      const msg = body && (body.error || body.message) ? JSON.stringify(body.error || body.message) : `HTTP ${res.status}`;
      throw new Error(msg);
    }
    return body;
  }

  /* ---------- modal ---------- */
  function openModal(title, bodyHTML, opts = {}) {
    const root = $("#modal-root");
    root.innerHTML = `
      <div class="overlay" id="overlay">
        <div class="modal ${opts.wide ? "wide" : ""}">
          <button class="modal-x" data-close aria-label="关闭">&times;</button>
          <h3>${esc(title)}</h3>
          <div class="modal-body">${bodyHTML}</div>
          <div class="modal-actions">
            <button class="btn" data-close>取消</button>
            ${opts.actionsHTML || `<button class="btn primary" data-submit>保存</button>`}
          </div>
        </div>
      </div>`;
    const close = () => { root.innerHTML = ""; };
    // only the × / 取消 buttons close the modal; clicking outside does not
    root.querySelectorAll("[data-close]").forEach((b) => b.addEventListener("click", close));
    return { el: $(".modal"), close, root: $("#overlay") };
  }
  function closeModal() { $("#modal-root").innerHTML = ""; }

  function toast(msg, color = "var(--accent)") {
    const t = document.createElement("div");
    t.textContent = msg;
    Object.assign(t.style, { position: "fixed", bottom: "24px", left: "50%", transform: "translateX(-50%)", background: "var(--bg3)", border: `1px solid ${color}`, color: color, padding: "10px 18px", borderRadius: "10px", zIndex: 99, fontSize: "13px" });
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 2600);
  }

  const bad = (m) => `<span class="badge ${m ? "ok" : "bad"}">${m ? "启用" : "停用"}</span>`;

  /* ---------- auth ---------- */
  function logout() {
    state.token = "";
    localStorage.removeItem("admin_token");
    showLogin();
  }

  function showLogin() {
    $("#main-view").classList.add("hidden");
    const lv = $("#login-view");
    lv.classList.remove("hidden");
    $("#login-user").focus();
  }

  function showMain() {
    $("#login-view").classList.add("hidden");
    $("#main-view").classList.remove("hidden");
  }

  /* ---------- router ---------- */
  const routes = { dashboard, channels, quotas, models, users, logs };

  function router() {
    const h = location.hash.replace(/^#\//, "") || "dashboard";
    const name = routes[h] ? h : "dashboard";
    state.route = name;
    $$(".sidebar nav a").forEach((a) => a.classList.toggle("active", a.dataset.route === name));
    const view = $("#view");
    view.innerHTML = '<div class="muted">加载中…</div>';
    routes[name](view);
  }

  /* ---------- dashboard ---------- */
  async function dashboard(view) {
    const s = await api("/api/admin/stats");
    const today = s.today || {}, all = s.allTime || {};
    view.innerHTML = `
      <div class="page-head"><h2>仪表盘</h2></div>
      <div class="grid stat-grid" style="margin-bottom:18px">
        <div class="stat"><div class="label">今日请求</div><div class="value">${fmt(today.requests)}</div></div>
        <div class="stat"><div class="label">今日 Token</div><div class="value">${fmt(today.tokens)}</div></div>
        <div class="stat"><div class="label">今日消耗额度</div><div class="value amber">${fmt(today.charge)}</div></div>
        <div class="stat"><div class="label">今日成功/失败</div><div class="value"><span class="green" style="color:var(--green)">${fmt(today.success)}</span> / <span style="color:var(--red)">${fmt(today.errors)}</span></div></div>
        <div class="stat"><div class="label">累计请求</div><div class="value">${fmt(all.requests)}</div></div>
        <div class="stat"><div class="label">累计 Token</div><div class="value">${fmt(all.tokens)}</div></div>
      </div>
      <div class="grid stat-grid" style="margin-bottom:18px">
        <div class="stat"><div class="label">渠道数</div><div class="value">${fmt(s.channelCount)}</div></div>
        <div class="stat"><div class="label">模型路由</div><div class="value">${fmt(s.virtualModelCount)}</div></div>
        <div class="stat"><div class="label">额度配置</div><div class="value">${fmt(s.quotaCount)}</div></div>
        <div class="stat"><div class="label">用户数</div><div class="value">${fmt(s.userCount)}</div></div>
      </div>
      <div class="grid two">
        <div class="card"><h3>Top 渠道</h3>${topTable(s.topChannels)}</div>
        <div class="card"><h3>Top 模型</h3>${topTable(s.topModels)}</div>
      </div>
      <div class="card"><h3>低额度提醒</h3>${lowQuotaTable(s.lowQuotas)}</div>`;
  }

  function topTable(rows = []) {
    if (!rows.length) return '<div class="muted">暂无数据</div>';
    return `<table><tr><th>名称</th><th>请求</th><th>Token</th><th>消耗</th></tr>
      ${rows.map((r) => `<tr><td>${esc(r.name)}</td><td>${fmt(r.count)}</td><td>${fmt(r.tokens)}</td><td>${fmt(r.charge)}</td></tr>`).join("")}</table>`;
  }

  function lowQuotaTable(rows = []) {
    if (!rows || !rows.length) return '<div class="ok-text">暂无低额度配额</div>';
    return `<table><tr><th>渠道</th><th>模型</th><th>模式</th><th>剩余</th></tr>
      ${rows.map((q) => `<tr><td>${esc(q.channelId)}</td><td>${esc(q.model)}</td><td>${q.mode === "calls" ? "按次" : "按token"}</td><td class="${q.remaining <= 0 ? "red" : "amber"}" style="color:${q.remaining <= 0 ? "var(--red)" : "var(--amber)"}">${fmt(q.remaining)}</td></tr>`).join("")}</table>`;
  }

  /* ---------- channels ---------- */
  async function channels(view) {
    const list = await api("/api/admin/channels");
    state.cache.channels = list;
    view.innerHTML = `
      <div class="page-head"><h2>渠道管理</h2><button class="btn primary" id="add-channel">+ 新建渠道</button></div>
      <div class="card">
        <table><tr><th>名称</th><th>Base URL</th><th>鉴权</th><th>密钥数</th><th>额度配置</th><th>状态</th><th></th></tr>
        ${list.map((c) => `<tr>
          <td><b>${esc(c.name)}</b></td>
          <td class="muted">${esc(c.baseUrl)}</td>
          <td>${esc(c.authType)}${c.apiKeyHeader ? ` (${esc(c.apiKeyHeader)})` : ""}</td>
          <td>${c.keys.length}</td>
          <td>${c.quotas.length}</td>
          <td>${c.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge bad">停用</span>'}</td>
          <td class="actions">
            <button class="btn small" data-edit="${c.id}">编辑</button>
            <button class="btn small danger" data-del="${c.id}" data-name="${esc(c.name)}">删除</button>
          </td></tr>`).join("")}
        </table>
      </div>`;

    $("#add-channel").onclick = () => channelModal(null);

    view.querySelectorAll("[data-edit]").forEach((b) => (b.onclick = () => channelModal(b.dataset.edit)));
    view.querySelectorAll("[data-del]").forEach((b) => (b.onclick = async () => {
      if (!confirm(`确定删除渠道「${b.dataset.name}」？其密钥、额度配置和路由引用都会被移除。`)) return;
      await api(`/api/admin/channels/${b.dataset.del}`, { method: "DELETE" });
      toast("已删除", "var(--red)");
      channels(view);
    }));
  }

  function channelModal(id) {
    const channelsCache = state.cache.channels || [];
    const src = channelsCache.find((c) => c.id === id) || {};
    const isEdit = !!id;
    const m = openModal(isEdit ? `编辑渠道 ${src.name}` : "新建渠道", `
      <form id="channel-form">
        <label>名称</label><input name="name" value="${esc(src.name || "")}" required />
        <label>Base URL（如 https://api.openai.com/v1）</label><input name="baseUrl" value="${esc(src.baseUrl || "")}" placeholder="https://api.openai.com/v1" required />
        <div class="form-row">
          <div><label>鉴权方式</label>
            <select name="authType" id="authType">
              <option value="bearer" ${src.authType === "bearer" || !src.authType ? "selected" : ""}>Authorization: Bearer</option>
              <option value="header" ${src.authType === "header" ? "selected" : ""}>自定义请求头</option>
              <option value="none" ${src.authType === "none" ? "selected" : ""}>无鉴权</option>
            </select></div>
          <div><label>请求头名称（header 方式）</label><input name="apiKeyHeader" value="${esc(src.apiKeyHeader || "")}" placeholder="x-api-key" /></div>
        </div>
        <div class="form-row">
          <div><label>超时（秒）</label><input name="timeoutSec" type="number" value="${src.timeoutSec || 120}" /></div>
          <div><label>状态</label>
            <select name="enabled"><option value="true" ${src.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${src.enabled === false ? "selected" : ""}>停用</option></select>
          </div>
        </div>
        ${isEdit ? "" : `<label>API Key（每行一个）</label><textarea name="keys" placeholder="sk-xxx&#10;sk-yyy"></textarea>`}
      </form>`, { actionsHTML: `<button class="btn primary" data-submit>保存</button>` });

    // edit mode: key ops are staged here and only persisted on 保存
    const keyRows = (src.keys || []).map((k) => ({
      id: k.id, key: k.key, enabled: k.enabled, origEnabled: k.enabled, isNew: false, pendingDelete: false,
    }));

    const submit = async () => {
      const f = new FormData($("#channel-form"));
      const req = {
        name: f.get("name"), baseUrl: f.get("baseUrl"), authType: f.get("authType"),
        apiKeyHeader: f.get("apiKeyHeader"), timeoutSec: Number(f.get("timeoutSec")) || 120,
        enabled: f.get("enabled") === "true",
      };
      if (!isEdit) req.keys = String(f.get("keys") || "").split("\n").map((s) => s.trim()).filter(Boolean);
      await api(isEdit ? `/api/admin/channels/${id}` : "/api/admin/channels", {
        method: isEdit ? "PUT" : "POST", body: JSON.stringify(req),
      });
      if (isEdit) {
        for (const k of keyRows) {
          if (k.isNew) {
            if (!k.pendingDelete) {
              await api(`/api/admin/channels/${id}/keys`, { method: "POST", body: JSON.stringify({ key: k.key }) });
            }
          } else if (k.pendingDelete) {
            await api(`/api/admin/keys/${k.id}`, { method: "DELETE" });
          } else if (k.enabled !== k.origEnabled) {
            await api(`/api/admin/keys/${k.id}`, { method: "PUT", body: JSON.stringify({ enabled: k.enabled }) });
          }
        }
      }
      closeModal();
      toast("已保存");
      channels(viewRef());
    };

    m.root.querySelector("[data-submit]").onclick = submit;

    if (isEdit) {
      const body = m.root.querySelector(".modal-body");
      const section = document.createElement("div");
      body.appendChild(section);

      const renderKeys = () => {
        section.innerHTML = `<div class="card" style="margin-top:14px"><h3>API 密钥</h3>
          <table><tr><th>Key</th><th>状态</th><th></th></tr>
          ${keyRows.map((k, i) => `<tr style="${k.pendingDelete ? "opacity:.5" : ""}">
            <td class="muted" style="font-family:monospace">${esc(k.key.slice(0, 14))}…${k.isNew ? ' <span class="badge blue">待保存</span>' : ""}${k.pendingDelete ? ' <span class="badge bad">待删除</span>' : ""}</td>
            <td>${k.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge bad">停用</span>'}</td>
            <td class="actions">
              ${k.isNew
                ? `<button class="btn small danger" data-key-remove="${i}">移除</button>`
                : `<button class="btn small" data-key-toggle="${i}">${k.enabled ? "停用" : "启用"}</button>
                   <button class="btn small ${k.pendingDelete ? "" : "danger"}" data-key-del="${i}">${k.pendingDelete ? "恢复" : "删除"}</button>`}
            </td></tr>`).join("")}
          <tr><td colspan="3"><div style="display:flex;gap:8px;margin-top:6px"><input id="new-key" placeholder="sk-..." style="flex:1"/><button type="button" class="btn small primary" id="add-key">添加</button></div></td></tr>
          </table>
          <div class="form-hint" style="margin-top:8px">新增 / 停用 / 删除仅在列表中标记，点击弹窗底部「保存」后才会写入数据库。</div></div>`;

        section.querySelectorAll("[data-key-toggle]").forEach((b) => (b.onclick = () => {
          const k = keyRows[+b.dataset.keyToggle];
          k.enabled = !k.enabled;
          renderKeys();
        }));
        section.querySelectorAll("[data-key-del]").forEach((b) => (b.onclick = () => {
          const k = keyRows[+b.dataset.keyDel];
          k.pendingDelete = !k.pendingDelete;
          renderKeys();
        }));
        section.querySelectorAll("[data-key-remove]").forEach((b) => (b.onclick = () => {
          keyRows.splice(+b.dataset.keyRemove, 1);
          renderKeys();
        }));
        section.querySelector("#add-key").onclick = () => {
          const inp = section.querySelector("#new-key");
          const v = inp.value.trim();
          if (!v) return;
          keyRows.push({ id: null, key: v, enabled: true, origEnabled: true, isNew: true, pendingDelete: false });
          inp.value = "";
          renderKeys();
        };
      };
      renderKeys();
    }
  }

  function viewRef() { return $("#view"); }

  /* ---------- quotas ---------- */
  const MODEL_TYPES = [
    ["text", "语言"], ["speech", "语音"], ["image", "图片"],
    ["video", "视频"], ["embed", "向量"], ["other", "其他"],
  ];
  const typeLabel = (t) => (MODEL_TYPES.find((x) => x[0] === t) || ["text", "语言"])[1];
  const typeBadge = (t) => {
    const cls = { text: "blue", speech: "ok", image: "amber", video: "bad" }[t] || "";
    return `<span class="badge ${cls}">${typeLabel(t)}</span>`;
  };
  const statusBadge = (q) => {
    if (!q.enabled) return '<span class="badge bad">停用</span>';
    const st = q.status || "ok";
    if (st === "unavailable") return `<span class="badge bad" title="${esc(q.statusReason || "")}">不可用</span>`;
    if (st === "insufficient") return '<span class="badge amber">额度不足</span>';
    return '<span class="badge ok">正常</span>';
  };

  async function quotas(view) {
    const [qs, chs] = await Promise.all([api("/api/admin/quotas"), api("/api/admin/channels")]);
    state.cache.channels = chs;
    state.cache.quotas = qs;
    const filter = state.cache.quotaTypeFilter || "all";
    const shown = filter === "all" ? qs : qs.filter((q) => (q.type || "text") === filter);
    const counts = (t) => (t === "all" ? qs.length : qs.filter((q) => (q.type || "text") === t).length);
    const chName = (id) => { const c = chs.find((c) => c.id === id); return c ? c.name : id; };
    const sumToken = shown.filter((q) => q.mode !== "calls").reduce((s, q) => s + (q.remaining || 0), 0);
    const sumCalls = shown.filter((q) => q.mode === "calls").reduce((s, q) => s + (q.remaining || 0), 0);
    const resvSum = shown.reduce((s, q) => s + (q.reserved || 0), 0);
    view.innerHTML = `
      <div class="page-head"><h2>额度管理</h2><button class="btn primary" id="add-quota">+ 新建额度</button></div>
      <div class="muted" style="margin-bottom:12px">每个「渠道 × 模型」一条额度记录。按 token 模式根据 usage 扣减；按次模式每次调用扣 1（图片按张数 × 单张消耗）。</div>
      <div class="filter-row" style="display:flex;gap:8px;margin-bottom:12px;flex-wrap:wrap">
        <button class="btn small ${filter === "all" ? "primary" : ""}" data-tfilter="all">全部 (${counts("all")})</button>
        ${MODEL_TYPES.map(([v, l]) => `<button class="btn small ${filter === v ? "primary" : ""}" data-tfilter="${v}">${l} (${counts(v)})</button>`).join("")}
      </div>
      <div class="card" style="padding:12px 18px;margin-bottom:12px;display:flex;gap:28px;align-items:center;flex-wrap:wrap">
        <span class="muted">剩余合计（${filter === "all" ? "全部" : typeLabel(filter)} · ${shown.length} 条）</span>
        <span>token：<b style="color:var(--accent)">${fmt(sumToken)}</b></span>
        <span>按次：<b style="color:var(--accent)">${fmt(sumCalls)}</b></span>
        <span class="muted">预扣中：<b>${fmt(resvSum)}</b></span>
      </div>
      <div class="card">
        <table><tr><th>类型</th><th>渠道</th><th>模型</th><th>模式</th><th>剩余</th><th>预扣中</th><th>单次/张消耗</th><th>系数</th><th>状态</th><th></th></tr>
        ${shown.map((q) => `<tr>
          <td>${typeBadge(q.type)}</td>
          <td>${esc(chName(q.channelId))}</td><td><b>${esc(q.model)}</b></td>
          <td>${q.mode === "calls" ? '<span class="badge blue">按次</span>' : '<span class="badge">按token</span>'}</td>
          <td class="${q.remaining <= 0 ? "red" : ""}" style="${q.remaining < 10000 && q.mode === "token" ? "color:var(--amber)" : ""}">${fmt(q.remaining)}</td>
          <td class="muted">${fmt(q.reserved)}</td>
          <td>${q.chargePerCall || 1}</td><td>${q.multiplier || 1}</td>
          <td>${statusBadge(q)}</td>
          <td class="actions">
            ${q.status && q.status !== "ok" ? `<button class="btn small" data-resetst="${q.id}" title="手动恢复为正常状态">恢复</button>` : ""}
            <button class="btn small" data-adjust="${q.id}">调整</button>
            <button class="btn small" data-edit="${q.id}">编辑</button>
            <button class="btn small danger" data-del="${q.id}" data-name="${esc(chName(q.channelId))}/${esc(q.model)}">删除</button>
          </td></tr>`).join("") || '<tr><td colspan="10" class="muted">暂无额度记录</td></tr>'}
        </table>
      </div>`;

    view.querySelectorAll("[data-tfilter]").forEach((b) => (b.onclick = () => {
      state.cache.quotaTypeFilter = b.dataset.tfilter; quotas(view);
    }));
    $("#add-quota").onclick = () => quotaModal(null, chs);
    view.querySelectorAll("[data-edit]").forEach((b) => (b.onclick = () => quotaModal(b.dataset.edit, chs)));
    view.querySelectorAll("[data-del]").forEach((b) => (b.onclick = async () => {
      if (!confirm(`删除额度「${b.dataset.name}」？`)) return;
      await api(`/api/admin/quotas/${b.dataset.del}`, { method: "DELETE" });
      toast("已删除", "var(--red)"); quotas(view);
    }));
    view.querySelectorAll("[data-adjust]").forEach((b) => (b.onclick = () => adjustModal(b.dataset.adjust)));
    view.querySelectorAll("[data-resetst]").forEach((b) => (b.onclick = async () => {
      await api(`/api/admin/quotas/${b.dataset.resetst}/reset-status`, { method: "POST" });
      toast("已恢复为正常"); quotas(view);
    }));
  }

  function quotaModal(id, chs) {
    const all = state.cache.quotas || [];
    const src = all.find((q) => q.id === id) || {};
    const isEdit = !!id;
    const lastCh = localStorage.getItem("lastQuotaChannel");
    const presetCh = src.channelId || (chs.some((c) => c.id === lastCh) ? lastCh : (chs[0] && chs[0].id));
    const m = openModal(isEdit ? "编辑额度" : "新建额度", `
      <form id="quota-form">
        <div class="form-row">
          <div><label>渠道</label><select name="channelId">${chs.map((c) => `<option value="${c.id}" ${presetCh === c.id ? "selected" : ""}>${esc(c.name)}</option>`).join("")}</select></div>
          <div><label>模型 ID</label><input name="model" value="${esc(src.model || "")}" placeholder="gpt-4o-mini" required /></div>
        </div>
        <div class="form-row">
          <div><label>模型类型</label><select name="type">
            ${MODEL_TYPES.map(([v, l]) => `<option value="${v}" ${(src.type || "text") === v ? "selected" : ""}>${l}</option>`).join("")}
          </select></div>
          <div><label>扣减模式</label><select name="mode">
            <option value="token" ${src.mode !== "calls" ? "selected" : ""}>按 token 数（usage）</option>
            <option value="calls" ${src.mode === "calls" ? "selected" : ""}>按调用次数</option>
          </select></div>
        </div>
        <div class="form-row">
          <div><label>剩余额度</label><input name="remaining" type="number" step="any" value="${src.remaining ?? 0}" /></div>
          <div><label>单次/每张消耗（按次模式）</label><input name="chargePerCall" type="number" step="any" value="${src.chargePerCall || 1}" /></div>
        </div>
        <div class="form-row">
          <div><label>消耗系数（按 token 模式）</label><input name="multiplier" type="number" step="any" value="${src.multiplier || 1}" /></div>
          <div><label>状态</label><select name="enabled"><option value="true" ${src.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${src.enabled === false ? "selected" : ""}>停用</option></select></div>
        </div>
        <div class="form-row">
          <div style="flex:1"><label>备注</label><input name="note" value="${esc(src.note || "")}" /></div>
        </div>
      </form>`);

    const chSel = m.root.querySelector("[name=channelId]");
    chSel.onchange = () => localStorage.setItem("lastQuotaChannel", chSel.value);

    m.root.querySelector("[data-submit]").onclick = async () => {
      const f = new FormData($("#quota-form"));
      localStorage.setItem("lastQuotaChannel", f.get("channelId"));
      const req = {
        channelId: f.get("channelId"), model: f.get("model"), type: f.get("type"), mode: f.get("mode"),
        remaining: Number(f.get("remaining")) || 0, chargePerCall: Number(f.get("chargePerCall")) || 1,
        multiplier: Number(f.get("multiplier")) || 1, enabled: f.get("enabled") === "true", note: f.get("note"),
      };
      await api(isEdit ? `/api/admin/quotas/${id}` : "/api/admin/quotas", { method: isEdit ? "PUT" : "POST", body: JSON.stringify(req) });
      closeModal(); toast("已保存"); quotas(viewRef());
    };
  }

  function adjustModal(id) {
    const m = openModal("调整额度", `
      <form id="adjust-form">
        <label>变动量（正数增加、负数扣减）</label>
        <input name="delta" type="number" step="any" placeholder="如 +10000 或 -500" required />
        <div class="form-hint">例：新充 10000 token 填 10000；手动扣 500 填 -500。</div>
      </form>`);
    m.root.querySelector("[data-submit]").onclick = async () => {
      const delta = Number($("#adjust-form").querySelector("[name=delta]").value);
      if (isNaN(delta)) return toast("请输入有效数字", "var(--red)");
      const r = await api(`/api/admin/quotas/${id}/adjust`, { method: "POST", body: JSON.stringify({ delta }) });
      closeModal(); toast(`调整完成，剩余 ${fmt(r.remaining)}`); quotas(viewRef());
    };
  }

  /* ---------- virtual models ---------- */
  async function models(view) {
    const [vms, chs, quotasList] = await Promise.all([api("/api/admin/virtual-models"), api("/api/admin/channels"), api("/api/admin/quotas")]);
    state.cache.channels = chs;
    state.cache.quotas = quotasList;
    state.cache.vms = vms;
    const chName = (id) => { const c = chs.find((c) => c.id === id); return c ? c.name : id; };
    const errCell = (e) => { e = e || "失败"; return `<span class="error-text" style="font-size:12px" title="${esc(e)}">${esc(e.slice(0, 36))}${e.length > 36 ? "…" : ""}</span>`; };

    view.innerHTML = `
      <div class="page-head"><h2>模型路由</h2><button class="btn primary" id="add-model">+ 新建路由</button></div>
      <div class="muted" style="margin-bottom:12px">对外暴露的模型名按优先级依次尝试候选渠道；额度不足或上游失败时自动切换。轻测只探测接口连通性（不耗 token）；测速为真实小请求，按量结算额度。</div>
      ${vms.map((vm) => {
        const sp = vm.speedTest;
        const pr = vm.probe;
        return `
        <div class="card">
          <div style="display:flex;justify-content:space-between;align-items:center">
            <div><b style="font-size:15px">${esc(vm.name)}</b>
              ${vm.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge bad">停用</span>'}
              ${vm.description ? `<span class="muted">— ${esc(vm.description)}</span>` : ""}
              ${pr ? `<span class="muted">· 轻测于 ${fmtTime(pr.at)}</span>` : ""}
              ${sp ? `<span class="muted">· 测速于 ${fmtTime(sp.at)}</span>` : ""}
            </div>
            <div><button class="btn small" data-probe="${vm.id}" data-name="${esc(vm.name)}" title="GET /models 探测接口连通性，不消耗 token">${pr ? "重新轻测" : "轻测"}</button>
            <button class="btn small" data-speedtest="${vm.id}" data-name="${esc(vm.name)}" title="真实小请求（max_tokens=8），按量结算额度">${sp ? "重新测速" : "测速"}</button>
            <button class="btn small" data-edit="${vm.id}">编辑</button>
            <button class="btn small danger" data-del="${vm.id}" data-name="${esc(vm.name)}">删除</button></div>
          </div>
          <table style="margin-top:12px"><tr><th>优先级</th><th>状态</th><th>渠道</th><th>上游模型</th><th>额度</th><th>轻测 / 测速</th><th></th></tr>
          ${vm.candidates.length === 0 ? '<tr><td colspan="7" class="muted">未配置候选渠道</td></tr>' :
            vm.candidates.map((c, i) => {
              const off = c.enabled === false;
              const q = quotasList.find((x) => x.channelId === c.channelId && x.model === c.model);
              const spv = sp && sp.results && sp.results.find((x) => x.channelId === c.channelId && x.model === c.model);
              const prv = pr && pr.results && pr.results.find((x) => x.channelId === c.channelId && x.model === c.model);
              const parts = [];
              if (spv) {
                parts.push(spv.ok
                  ? `<b>${spv.latencyMs}</b> ms <span class="muted">${spv.tokens || 0}tk${spv.charge ? " · 扣" + fmt(spv.charge) : ""}</span>`
                  : errCell(spv.error));
              }
              if (prv) {
                parts.push(prv.ok
                  ? `<span class="muted">接口 ${prv.latencyMs} ms</span>`
                  : errCell(prv.error));
              }
              const spCell = parts.length ? parts.join("<br>") : '<span class="muted">—</span>';
              const qBad = q && q.status && q.status !== "ok";
              return `<tr${off ? ' style="opacity:.55"' : ""}><td>${c.priority}</td>
                <td>${off ? '<span class="badge bad">停用</span>' : '<span class="badge ok">启用</span>'}</td>
                <td>${esc(chName(c.channelId))}</td><td>${esc(c.model)}</td>
                <td>${q ? `${fmt(q.remaining)}（${q.mode === "calls" ? "按次" : "按token"}）${qBad ? " " + statusBadge(q) : ""}` : '<span class="badge bad">无额度配置</span>'}</td>
                <td>${spCell}</td>
                <td class="actions">
                  <button class="btn small" data-cand-toggle="${vm.id}" data-cand-idx="${i}" data-on="${!off}">${off ? "启用" : "停用"}</button>
                  ${qBad ? `<button class="btn small" data-resetst="${q.id}" title="手动恢复额度状态为正常">恢复</button>` : ""}
                </td></tr>`;
            }).join("")}
          </table>
        </div>`;
      }).join("")}
    `;

    $("#add-model").onclick = () => modelModal(null, chs);
    view.querySelectorAll("[data-edit]").forEach((b) => (b.onclick = () => modelModal(b.dataset.edit, chs)));
    view.querySelectorAll("[data-probe]").forEach((b) => (b.onclick = async () => {
      const orig = b.textContent;
      b.disabled = true; b.textContent = "轻测中…";
      try {
        const r = await api(`/api/admin/virtual-models/${b.dataset.probe}/probe`, { method: "POST" });
        void r;
        toast("轻测完成（未耗 token）");
        models(view);
      } catch (e) {
        toast(e.message || "轻测失败", "var(--red)");
        b.disabled = false; b.textContent = orig;
      }
    }));
    view.querySelectorAll("[data-speedtest]").forEach((b) => (b.onclick = async () => {
      const orig = b.textContent;
      b.disabled = true; b.textContent = "测速中…";
      try {
        const r = await api(`/api/admin/virtual-models/${b.dataset.speedtest}/speedtest`, { method: "POST" });
        void r;
        toast("测速完成（已按量结算额度）");
        models(view);
      } catch (e) {
        toast(e.message || "测速失败", "var(--red)");
        b.disabled = false; b.textContent = orig;
      }
    }));
    view.querySelectorAll("[data-cand-toggle]").forEach((b) => (b.onclick = async () => {
      const vm = vms.find((v) => v.id === b.dataset.candToggle);
      if (!vm) return;
      const idx = Number(b.dataset.candIdx);
      const cands = vm.candidates.map((c, i) => (i === idx ? { ...c, enabled: b.dataset.on !== "true" } : c));
      await api(`/api/admin/virtual-models/${vm.id}`, { method: "PUT", body: JSON.stringify({ name: vm.name, description: vm.description, enabled: vm.enabled, candidates: cands }) });
      toast("已更新"); models(view);
    }));
    view.querySelectorAll("[data-resetst]").forEach((b) => (b.onclick = async () => {
      await api(`/api/admin/quotas/${b.dataset.resetst}/reset-status`, { method: "POST" });
      toast("已恢复为正常"); models(view);
    }));
    view.querySelectorAll("[data-del]").forEach((b) => (b.onclick = async () => {
      if (!confirm(`删除路由「${b.dataset.name}」？`)) return;
      await api(`/api/admin/virtual-models/${b.dataset.del}`, { method: "DELETE" });
      toast("已删除", "var(--red)"); models(view);
    }));
  }

  function modelModal(id, chs) {
    const all = state.cache.vms || [];
    const src = all.find((v) => v.id === id) || { candidates: [] };
    const isEdit = !!id;
    const m = openModal(isEdit ? `编辑路由 ${src.name}` : "新建路由", `
      <form id="model-form">
        <div class="form-row">
          <div><label>对外模型名</label><input name="name" value="${esc(src.name || "")}" placeholder="gpt-4o" required /></div>
          <div><label>描述</label><input name="description" value="${esc(src.description || "")}" /></div>
        </div>
        <label>状态</label>
        <select name="enabled"><option value="true" ${src.enabled !== false ? "selected" : ""}>启用</option><option value="false" ${src.enabled === false ? "selected" : ""}>停用</option></select>
        <label style="margin-top:16px">候选渠道（优先级数字越小越先尝试，可拖动调整顺序）</label>
        <div id="cand-rows"></div>
        <button type="button" class="btn small" id="add-cand" style="margin-top:8px">+ 添加候选</button>
        <div style="margin-top:14px;border:1px dashed var(--border);border-radius:8px;padding:10px 12px">
          <div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
            <b style="font-size:13px">批量添加候选</b>
            <select id="ba-channel" style="flex:1;min-width:140px">${chs.map((ch) => `<option value="${ch.id}">${esc(ch.name)}</option>`).join("")}</select>
            <button type="button" class="btn small" id="ba-load">加载该渠道模型</button>
          </div>
          <div id="ba-list" class="muted" style="margin-top:8px;font-size:13px">选择渠道后点击「加载该渠道模型」，勾选多个模型一次性加入候选。</div>
          <button type="button" class="btn small primary" id="ba-add" style="margin-top:8px" disabled>加入候选（0）</button>
        </div>
      </form>`, { wide: true });

    const rows = m.root.querySelector("#cand-rows");
    let cands = (src.candidates || []).slice().sort((a, b) => a.priority - b.priority);

    function renderRows() {
      cands.sort((a, b) => a.priority - b.priority);
      const quotas = state.cache.quotas || [];
      const dls = chs.map((ch) => {
        const ms = quotas.filter((q) => q.channelId === ch.id);
        return `<datalist id="models-${ch.id}">${ms.map((q) => `<option value="${esc(q.model)}" label="剩 ${fmt(q.remaining)} · ${q.mode === "calls" ? "按次" : "按token"}"></option>`).join("")}</datalist>`;
      }).join("");
      rows.innerHTML = dls + cands.map((c, i) => `
        <div style="display:flex;gap:8px;margin-top:8px;align-items:center;${c.enabled === false ? "opacity:.55" : ""}">
          <select style="flex:2" data-c-channel="${i}">${chs.map((ch) => `<option value="${ch.id}" ${c.channelId === ch.id ? "selected" : ""}>${esc(ch.name)}</option>`).join("")}</select>
          <input style="flex:2" data-c-model="${i}" list="models-${c.channelId}" value="${esc(c.model)}" placeholder="上游模型 ID（点击可下拉选择）" />
          <input style="flex:1;width:70px" data-c-priority="${i}" type="number" value="${c.priority}" />
          <span class="muted" style="font-size:12px">优先级</span>
          <label class="muted" style="font-size:12px;display:flex;align-items:center;gap:4px;white-space:nowrap"><input type="checkbox" data-c-enabled="${i}" ${c.enabled === false ? "" : "checked"} />启用</label>
          <button type="button" class="btn small danger" data-c-del="${i}">×</button>
        </div>`).join("");
      rows.querySelectorAll("[data-c-del]").forEach((b) => (b.onclick = () => { cands.splice(Number(b.dataset.cDel), 1); renderRows(); }));
      rows.querySelectorAll("[data-c-enabled]").forEach((s) => (s.onchange = () => {
        const i = Number(s.dataset.cEnabled);
        cands[i].enabled = s.checked;
        renderRows();
      }));
      rows.querySelectorAll("[data-c-channel]").forEach((s) => (s.onchange = () => {
        const i = Number(s.dataset.cChannel);
        cands[i].channelId = s.value;
        const inp = rows.querySelector(`[data-c-model="${i}"]`);
        if (inp) inp.setAttribute("list", "models-" + s.value);
      }));
      rows.querySelectorAll("[data-c-model]").forEach((s) => (s.oninput = () => { cands[Number(s.dataset.cModel)].model = s.value; }));
      rows.querySelectorAll("[data-c-priority]").forEach((s) => (s.oninput = () => { cands[Number(s.dataset.cPriority)].priority = Number(s.value) || 0; }));
    }
    renderRows();
    m.root.querySelector("#add-cand").onclick = () => {
      const nextP = cands.length ? Math.max(...cands.map((c) => c.priority)) + 1 : 1;
      cands.push({ channelId: chs[0] ? chs[0].id : "", model: "", priority: nextP, enabled: true });
      renderRows();
    };

    // batch-add candidates: pick a channel, multi-select its models
    const baList = m.root.querySelector("#ba-list");
    const baAdd = m.root.querySelector("#ba-add");
    const baChecked = new Set();
    const syncBa = () => {
      baAdd.disabled = baChecked.size === 0;
      baAdd.textContent = `加入候选（${baChecked.size}）`;
    };
    m.root.querySelector("#ba-load").onclick = () => {
      const chId = m.root.querySelector("#ba-channel").value;
      const quotas = (state.cache.quotas || []).filter((q) => q.channelId === chId);
      const modelsMap = new Map();
      quotas.forEach((q) => { if (q.model && !modelsMap.has(q.model)) modelsMap.set(q.model, q); });
      if (!modelsMap.size) {
        baList.innerHTML = '<span class="muted">该渠道暂无已配置额度的模型，可先到「额度管理」添加。</span>';
        baChecked.clear(); syncBa();
        return;
      }
      const ch = chs.find((c) => c.id === chId);
      baList.innerHTML = [...modelsMap.values()].map((q) => {
        const already = cands.some((c) => c.channelId === chId && c.model === q.model);
        return `<label style="display:flex;align-items:center;gap:6px;padding:3px 0;${already ? "opacity:.5" : ""}">
          <input type="checkbox" data-ba-model="${esc(q.model)}" ${already ? "checked disabled" : ""} />
          <b>${esc(q.model)}</b>
          <span class="muted">剩 ${fmt(q.remaining)} · ${q.mode === "calls" ? "按次" : "按token"}</span>
          ${already ? '<span class="badge">已在候选</span>' : ""}
        </label>`;
      }).join("");
      baChecked.clear(); syncBa();
      baList.querySelectorAll("[data-ba-model]").forEach((cb) => (cb.onchange = () => {
        if (cb.checked) baChecked.add(cb.dataset.baModel);
        else baChecked.delete(cb.dataset.baModel);
        syncBa();
      }));
    };
    baAdd.onclick = () => {
      const chId = m.root.querySelector("#ba-channel").value;
      let nextP = cands.length ? Math.max(...cands.map((c) => c.priority)) : 0;
      let added = 0;
      baChecked.forEach((model) => {
        if (cands.some((c) => c.channelId === chId && c.model === model)) return;
        cands.push({ channelId: chId, model, priority: ++nextP, enabled: true });
        added++;
      });
      baChecked.clear(); syncBa();
      renderRows();
      toast(`已加入 ${added} 个候选`);
    };

    m.root.querySelector("[data-submit]").onclick = async () => {
      const f = new FormData($("#model-form"));
      const valid = cands.filter((c) => c.channelId && c.model);
      if (!valid.length) return toast("请至少配置一个候选渠道", "var(--red)");
      const req = { name: f.get("name"), description: f.get("description"), enabled: f.get("enabled") === "true", candidates: valid };
      await api(isEdit ? `/api/admin/virtual-models/${id}` : "/api/admin/virtual-models", { method: isEdit ? "PUT" : "POST", body: JSON.stringify(req) });
      state.cache.vms = null;
      closeModal(); toast("已保存"); models(viewRef());
    };
  }

  /* ---------- users ---------- */
  async function users(view) {
    const [us, toks] = await Promise.all([api("/api/admin/users"), api("/api/admin/tokens")]);
    const origin = location.origin;
    view.innerHTML = `
      <div class="page-head"><h2>用户与令牌</h2><button class="btn primary" id="add-user">+ 新建用户</button></div>
      <details class="card help">
        <summary>调用方法（OpenAI 兼容接口）</summary>
        <div class="help-body">
          <ul>
            <li><b>Base URL</b>：<code>${esc(origin)}/v1</code></li>
            <li><b>鉴权</b>：请求头 <code>Authorization: Bearer sk-xxx</code>，sk-xxx 为创建令牌时展示的密钥（仅显示一次，请妥善保存）</li>
            <li><b>模型名</b>：填「模型路由」页配置的对外模型名；调用 <code>GET /v1/models</code> 可列出全部可用模型</li>
            <li><b>接口</b>：<code>POST /v1/chat/completions</code>（对话）、<code>POST /v1/images/generations</code>（图片）</li>
          </ul>
          <div class="code-title">curl</div>
          <pre>curl ${esc(origin)}/v1/chat/completions \\
  -H "Authorization: Bearer sk-xxx" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "你好"}]}'</pre>
          <div class="code-title">Python（openai SDK）</div>
          <pre>from openai import OpenAI

client = OpenAI(
    api_key="sk-xxx",
    base_url="${esc(origin)}/v1",
)

resp = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)</pre>
        </div>
      </details>
      ${us.map((u) => `
        <div class="card">
          <div style="display:flex;justify-content:space-between;align-items:center">
            <div><b style="font-size:15px">${esc(u.name)}</b>
              ${u.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge bad">停用</span>'}
              ${u.description ? `<span class="muted">— ${esc(u.description)}</span>` : ""}
              <span class="muted">· ${u.tokenCount} 个令牌</span></div>
            <div>
              <button class="btn small primary" data-newtok="${u.id}" data-name="${esc(u.name)}">+ 新令牌</button>
              <button class="btn small" data-edit="${u.id}">编辑</button>
              <button class="btn small danger" data-del="${u.id}" data-name="${esc(u.name)}">删除</button>
            </div>
          </div>
          <table style="margin-top:12px"><tr><th>令牌名</th><th>状态</th><th>上次使用</th><th></th></tr>
          ${toks.filter((t) => t.userId === u.id).map((t) => `<tr>
            <td>${esc(t.name)}</td>
            <td>${t.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge bad">停用</span>'}</td>
            <td class="muted">${fmtTime(t.lastUsedAt)}</td>
            <td class="actions">
              <button class="btn small" data-tok-toggle="${t.id}" data-on="${t.enabled}">${t.enabled ? "停用" : "启用"}</button>
              <button class="btn small danger" data-tok-del="${t.id}">删除</button>
            </td></tr>`).join("") || '<tr><td colspan="4" class="muted">无令牌，点击「+ 新令牌」创建</td></tr>'}
          </table>
        </div>`).join("")}
    `;

    $("#add-user").onclick = () => userModal(null);
    view.querySelectorAll("[data-edit]").forEach((b) => (b.onclick = () => userModal(b.dataset.edit)));
    view.querySelectorAll("[data-del]").forEach((b) => (b.onclick = async () => {
      if (!confirm(`删除用户「${b.dataset.name}」及其全部令牌？`)) return;
      await api(`/api/admin/users/${b.dataset.del}`, { method: "DELETE" });
      toast("已删除", "var(--red)"); users(view);
    }));
    view.querySelectorAll("[data-newtok]").forEach((b) => (b.onclick = () => tokenModal(b.dataset.newtok, b.dataset.name)));
    view.querySelectorAll("[data-tok-toggle]").forEach((b) => (b.onclick = async () => {
      await api(`/api/admin/tokens/${b.dataset.tokToggle}`, { method: "PUT", body: JSON.stringify({ enabled: b.dataset.on !== "true" }) });
      toast("已更新"); users(view);
    }));
    view.querySelectorAll("[data-tok-del]").forEach((b) => (b.onclick = async () => {
      if (!confirm("删除该令牌？所有使用该密钥的调用将立即失效。")) return;
      await api(`/api/admin/tokens/${b.dataset.tokDel}`, { method: "DELETE" });
      toast("已删除", "var(--red)"); users(view);
    }));
  }

  function userModal(id) {
    const m = openModal(id ? "编辑用户" : "新建用户", `
      <form id="user-form">
        <label>用户名</label><input name="name" required placeholder="如 team-a" />
        <label>描述</label><input name="description" placeholder="可选" />
        ${id ? `<label>状态</label><select name="enabled"><option value="true">启用</option><option value="false">停用</option></select>` : ""}
      </form>`);
    m.root.querySelector("[data-submit]").onclick = async () => {
      const f = new FormData($("#user-form"));
      const req = { name: f.get("name"), description: f.get("description") };
      if (id) req.enabled = f.get("enabled") === "true";
      await api(id ? `/api/admin/users/${id}` : "/api/admin/users", { method: id ? "PUT" : "POST", body: JSON.stringify(req) });
      closeModal(); toast("已保存"); users(viewRef());
    };
  }

  function tokenModal(userId, userName) {
    const m = openModal(`为用户 ${userName} 创建令牌`, `
      <form id="token-form"><label>令牌名称</label><input name="name" placeholder="如 production-web" required /></form>
      <div class="key-copy hidden" id="key-show"></div>
      <div class="form-hint">创建后请立即复制并妥善保存密钥，密钥仅显示一次。</div>`);
    m.root.querySelector("[data-submit]").onclick = async () => {
      const f = new FormData($("#token-form"));
      const r = await api(`/api/admin/users/${userId}/tokens`, { method: "POST", body: JSON.stringify({ name: f.get("name") }) });
      const k = m.root.querySelector("#key-show");
      k.classList.remove("hidden");
      k.innerHTML = `密钥：<b>${esc(r.key)}</b>`;
      toast("令牌已创建");
      users(viewRef());
    };
  }

  /* ---------- logs ---------- */
  const logF = { page: 1, limit: 20, status: "", model: "", userId: "" };

  async function logs(view) {
    const q = new URLSearchParams({ page: logF.page, limit: logF.limit });
    if (logF.status) q.set("status", logF.status);
    if (logF.model) q.set("model", logF.model);
    if (logF.userId) q.set("userId", logF.userId);
    const r = await api(`/api/admin/logs?${q}`);
    const totalPages = Math.max(1, Math.ceil(r.total / logF.limit));

    view.innerHTML = `
      <div class="page-head"><h2>调用日志</h2></div>
      <div class="filter-row">
        <div class="field"><label>状态</label><select id="f-status">
          <option value="">全部</option>
          <option value="success" ${logF.status === "success" ? "selected" : ""}>成功</option>
          <option value="error" ${logF.status === "error" ? "selected" : ""}>失败</option>
        </select></div>
        <div class="field"><label>虚拟模型</label><input id="f-model" value="${esc(logF.model)}" placeholder="模型名" /></div>
        <button class="btn primary" id="f-apply">筛选</button>
        <button class="btn" id="f-reset">重置</button>
      </div>
      <div class="card">
        <table><tr><th>时间</th><th>虚拟模型</th><th>渠道</th><th>上游模型</th><th>模式</th><th>思考</th><th>Token</th><th>消耗</th><th>状态</th></tr>
        ${r.items.map((l) => `<tr>
          <td class="muted">${fmtTime(l.createdAt)}</td>
          <td><b>${esc(l.virtualModel)}</b></td>
          <td>${esc(l.channelName || l.channelId)} </td>
          <td class="muted">${esc(l.actualModel || "")}${l.images ? ` <span class="badge blue">${l.images}图</span>` : ""}</td>
          <td>${l.mode === "calls" ? "按次" : "按token"}</td>
          <td>${l.thinking === "off" ? '<span class="badge bad">关</span>' : l.thinking === "on" ? '<span class="badge ok">开</span>' : '<span class="muted">-</span>'}</td>
          <td>${fmt(l.totalTokens)}</td>
          <td>${fmt(l.charge)}</td>
          <td>${l.status === "success" ? '<span class="badge ok">成功</span>' : `<span class="badge bad">失败</span>`}</td></tr>
          ${l.error ? `<tr><td colspan="9" class="error-text" style="font-size:12px">${esc(l.error)}</td></tr>` : ""}`).join("") || '<tr><td colspan="9" class="muted">暂无日志</td></tr>'}
        </table>
        <div class="pagination">
          <button class="btn small" id="prev-page" ${logF.page <= 1 ? "disabled" : ""}>上一页</button>
          <span>第 ${logF.page} / ${totalPages} 页（共 ${fmt(r.total)} 条）</span>
          <button class="btn small" id="next-page" ${logF.page >= totalPages ? "disabled" : ""}>下一页</button>
        </div>
      </div>`;

    $("#f-apply").onclick = () => { logF.status = $("#f-status").value; logF.model = $("#f-model").value.trim(); logF.page = 1; logs(view); };
    $("#f-reset").onclick = () => { Object.assign(logF, { status: "", model: "", userId: "", page: 1 }); logs(view); };
    $("#prev-page").onclick = () => { if (logF.page > 1) { logF.page--; logs(view); } };
    $("#next-page").onclick = () => { if (logF.page < totalPages) { logF.page++; logs(view); } };
  }

  /* ---------- init ---------- */
  $("#login-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const errEl = $("#login-error");
    errEl.classList.add("hidden");
    try {
      const r = await api("/api/admin/login", {
        method: "POST",
        body: JSON.stringify({ username: $("#login-user").value, password: $("#login-pass").value }),
      });
      state.token = r.token;
      localStorage.setItem("admin_token", r.token);
      showMain();
      router();
    } catch (err) {
      errEl.textContent = err.message;
      errEl.classList.remove("hidden");
    }
  });

  $("#logout-btn").onclick = logout;
  window.addEventListener("hashchange", () => { if (state.token) router(); });

  if (state.token) {
    showMain();
    router();
  } else {
    showLogin();
  }
})();