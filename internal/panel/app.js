/* marvis2api 管理面板前端。零依赖 vanilla JS，设计系统对齐 workbuddy2api-gui。
 * 鉴权：登录页（密码 = api_key，默认 marvis）→ 7 天 HttpOnly 会话 Cookie。 */
"use strict";

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

/* ---------- 基础设施：登录会话 / API 调用 / Toast ---------- */

function toast(msg, isErr) {
  const el = document.createElement("div");
  el.className = "toast" + (isErr ? " err" : "");
  el.textContent = msg;
  $("toast").appendChild(el);
  setTimeout(() => el.remove(), 4200);
}

async function api(method, path, body) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const resp = await fetch(path, {
    method, headers, body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  let data = null;
  try { data = await resp.json(); } catch { /* 非 JSON 错误体 */ }
  if (resp.status === 401) {
    // 会话过期/失效：展示登录页（幂等，不影响其他在途请求）。
    showLogin("登录状态已失效，请重新登录");
    throw new Error((data && data.error) || "未登录");
  }
  if (!resp.ok) throw new Error((data && (data.error || data.err)) || ("HTTP " + resp.status));
  return data;
}

/* 登录页：默认密码 marvis；成功后 7 天会话 Cookie。 */

let loginShown = false;

function showLogin(errMsg) {
  if (!loginShown) {
    $("app").classList.add("hidden");
    loginShown = true;
    // 默认密码横幅（对齐 GUI 的安全提示）：仅未登录时由 session 探测填充。
    fetch("/panel/api/session").then((r) => r.json()).then((s) => {
      $("login-default").classList.toggle("hidden", !s.using_default_password);
    }).catch(() => {});
  }
  $("login-err").innerHTML = errMsg
    ? `<div class="alert alert-error" style="margin:0 0 12px">${esc(errMsg)}</div>` : "";
  $("login").classList.remove("hidden");
  setTimeout(() => $("login-pass").focus(), 30);
}
function hideLogin() {
  $("login").classList.add("hidden");
  $("app").classList.remove("hidden");
  $("login-err").innerHTML = "";
  $("login-default").classList.add("hidden");
  loginShown = false;
}
async function doLogin() {
  const pass = $("login-pass").value;
  if (!pass) { showLoginErr("请输入密码"); return; }
  $("login-btn").disabled = true;
  try {
    const resp = await fetch("/panel/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: pass }),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      showLoginErr(data.error || ("登录失败 HTTP " + resp.status));
      return;
    }
    $("login-pass").value = "";
    hideLogin();
    route(); // 重新加载当前视图
  } finally {
    $("login-btn").disabled = false;
  }
}
function showLoginErr(msg) {
  $("login-err").innerHTML = `<div class="alert alert-error" style="margin:0 0 12px">${esc(msg)}</div>`;
}
async function doLogout() {
  try { await api("POST", "/panel/api/logout"); } catch { /* 会话已无效也无妨 */ }
  showLogin();
}

/* ---------- 路由 ---------- */

const VIEWS = ["dashboard", "accounts", "keys", "stats", "chat", "models", "config", "logs"];
const TITLES = { dashboard: "仪表盘", accounts: "账号", keys: "密钥", stats: "请求统计", chat: "聊天测试", models: "模型", config: "网关配置", logs: "运行日志" };
const SUBS = {
  dashboard: "使用中账号的健康度和剩余额度合计",
  accounts: "添加、切换和查看账号。不指定请求头时，新对话按剩余额度分配，同一段对话固定走同一个号。",
  keys: "登录密码和 API Key 分开。密码只打开面板，Key 用来调用接口。",
  stats: "按模型聚合的请求统计（/v1/stats 数据源）",
  chat: "经网关代理的在线对话验证",
  models: "上游模型目录（动态拉取 + 静态兜底）",
  config: "在线编辑 config.json（部分字段热生效）",
  logs: "网关运行日志（环形缓冲，最新 500 条）",
};
let current = "dashboard";

function route() {
  const name = (location.hash.replace(/^#\//, "") || "dashboard").split("?")[0];
  if (name === "credential") {
    location.hash = "#/accounts";
    return;
  }
  current = VIEWS.includes(name) ? name : "dashboard";
  for (const v of VIEWS) $("view-" + v).classList.toggle("hidden", v !== current);
  for (const a of document.querySelectorAll("#nav .nav-item"))
    a.classList.toggle("active", a.dataset.view === current);
  $("viewtitle").textContent = TITLES[current];
  $("viewsub").textContent = SUBS[current] || "";
  if (current === "dashboard") { refreshOverview(); loadDashUsage(); }
  if (current === "accounts") loadAccountsPage();
  if (current === "keys") loadKeys();
  if (current === "stats") loadStats();
  if (current === "chat") loadChatModels();
  if (current === "models") loadModels();
  if (current === "config") loadConfig();
  if (logTimer && current !== "logs") { clearInterval(logTimer); logTimer = null; }
  if (current === "logs" && !logTimer) {
    logTimer = setInterval(loadLogs, 3000);
    loadLogs();
  } else if (current === "logs") {
    loadLogs();
  }
}

/* ---------- 仪表盘 ---------- */

function fmtQuota(n) {
  if (n == null || n === "" || Number.isNaN(Number(n))) return "—";
  const v = Number(n);
  const abs = Math.abs(v);
  const unit = abs >= 1e9 ? [1e9, "B"] : abs >= 1e6 ? [1e6, "M"] : abs >= 1e3 ? [1e3, "K"] : null;
  if (!unit) return String(Math.round(v));
  const scaled = v / unit[0];
  const digits = Math.abs(scaled) >= 100 ? 0 : Math.abs(scaled) >= 10 ? 1 : 2;
  return scaled.toFixed(digits).replace(/\.0+$/, "").replace(/(\.\d)0$/, "$1") + unit[1];
}

function accountInUse(a) {
  return a && !a.dormant;
}

function accountHealthy(a) {
  if (!accountInUse(a)) return false;
  if (a.kernel === "error" || a.error) return false;
  if (a.state === "cooling" || a.state === "breaker" || a.state === "disabled") return false;
  if (a.expires_at > 0 && a.expires_at * 1000 < Date.now()) return false;
  return true;
}

async function refreshOverview() {
  try {
    const [o, accts] = await Promise.all([
      api("GET", "/panel/api/overview"),
      api("GET", "/panel/api/accounts").catch(() => ({ accounts: [] })),
    ]);
    const all = accts.accounts || [];
    const using = all.filter(accountInUse);
    const healthy = using.filter(accountHealthy).length;
    const remain = using.reduce((sum, a) => sum + (Number(a.quota_remaining) || 0), 0);
    const known = using.filter((a) => Number(a.quota_total) > 0 || Number(a.quota_remaining) > 0).length;
    const ratio = using.length ? `${healthy}/${using.length}` : "0/0";
    const ratioCls = !using.length ? "" : healthy === using.length ? "text-ok" : healthy === 0 ? "text-danger" : "text-warn";
    const stat = (lbl, num, cls) =>
      `<div class="stat"><div class="stat-label">${lbl}</div>
       <div class="stat-value ${cls || ""}">${num}</div></div>`;
    $("dash-cards").innerHTML =
      stat("健康", ratio, ratioCls) +
      stat("剩余额度", fmtQuota(remain)) +
      stat("使用中", using.length);
    $("dash-info").innerHTML =
      `休眠 ${all.length - using.length} 个` +
      (known < using.length ? ` · ${using.length - known} 个还没有额度` : "") +
      ` · 版本 <b>${esc(o.version)}</b> · 运行 ${Math.floor(o.uptime_sec / 60)} 分钟`;
    $("quickstart").innerHTML =
      esc(`curl http://127.0.0.1:${location.port || "18620"}/v1/chat/completions \\`) +
      "<br>" + esc(`  -H "Authorization: Bearer <api_key>" \\`) +
      "<br>" + esc(`  -H "Content-Type: application/json" \\`) +
      "<br>" + esc(`  -d '{"model":"deepseek-v4-pro-external","messages":[{"role":"user","content":"hi"}],"stream":true}'`);
    $("foot-version").textContent = "v" + o.version;
  } catch (e) { toast("总览加载失败: " + e.message, true); }
}

/* ---------- 账号 ---------- */

let accountRows = [];
let selectedIds = new Set();

function tokenText(exp) {
  if (!exp) return `<span class="muted">未知</span>`;
  const left = exp * 1000 - Date.now();
  const when = new Date(exp * 1000).toLocaleString();
  if (left <= 0) return `<span class="text-danger">已过期</span><div class="muted">${esc(when)}</div>`;
  if (left < 10 * 60 * 1000) return `<span class="text-warn">即将过期</span><div class="muted">${esc(when)}</div>`;
  return `<span class="text-ok">有效</span><div class="muted">${esc(when)}</div>`;
}

function visibleAccounts() {
  const q = (($("acct-q") && $("acct-q").value) || "").trim().toLowerCase();
  return accountRows.filter((a) => !q || `${a.name || ""} ${a.uid || ""} ${a.note || ""}`.toLowerCase().includes(q));
}

function targetAccountIds() {
  const rows = visibleAccounts();
  const picked = rows.filter((a) => selectedIds.has(a.id));
  return (picked.length ? picked : rows).map((a) => a.id);
}

function renderAccounts() {
  const box = $("acct-table");
  if (!box) return;
  const hint = $("acct-batch-hint");
  if (hint) hint.textContent = selectedIds.size ? `作用于已选 ${selectedIds.size} 个账号` : "未选择时作用于全部账号";
  const rows = visibleAccounts();
  if (!accountRows.length) {
    box.innerHTML = `<p class="muted">还没有账号。用上面的扫码或手工填写添加。</p>`;
    return;
  }
  if (!rows.length) {
    box.innerHTML = `<p class="muted">没有匹配的账号。</p>`;
    return;
  }
  const allOn = rows.every((a) => selectedIds.has(a.id));
  box.innerHTML = `<div class="table-wrap"><table><thead><tr>
    <th style="width:34px"><input type="checkbox" data-action="toggle-all-accounts" ${allOn ? "checked" : ""} aria-label="全选"></th>
    <th>账号</th><th>渠道</th><th>状态</th><th class="num">每日额度</th><th>Token</th><th>报错</th><th>操作</th>
  </tr></thead><tbody>${rows.map((a) => {
    const kind = a.login_type === "QC" ? "QQ" : "微信";
    const kernel = a.kernel === "ready" ? "内核可用" : a.kernel === "starting" ? "启动中" : a.kernel === "error" ? "内核失败" : "";
    const state = a.dormant ? "休眠" : "已启用";
    const bad = a.kernel === "error" || a.state === "cooling" || a.state === "breaker" || a.err_total > 0;
    const badge = a.dormant ? "badge-dim" : bad ? "badge-warn" : "badge-ok";
    const monitor = a.success_count || a.err_total || a.latency_ms ? `<div class="muted">成功 ${a.success_count || 0} · 失败 ${a.err_total || 0}${a.latency_ms ? " · 探活 " + a.latency_ms + "ms" : ""}</div>` : "";
    const quota = a.quota_total ? `${Number(a.quota_remaining).toLocaleString()} / ${Number(a.quota_total).toLocaleString()}` : "—";
    const err = a.error || a.reason || a.last_error || "";
    const title = a.name || "未获取名称";
    const nameErr = !a.name && a.name_error ? `<div class="muted">${esc(a.name_error)}</div>` : "";
    return `<tr>
      <td><input type="checkbox" data-action="toggle-account" data-id="${esc(a.id)}" ${selectedIds.has(a.id) ? "checked" : ""} aria-label="选择"></td>
      <td><b>${esc(title)}</b><div class="muted">${kind}</div>${nameErr}</td>
      <td>${kind}</td>
      <td><span class="badge ${badge}">${esc([state, kernel].filter(Boolean).join(" · "))}</span>${monitor}</td>
      <td class="num">${esc(quota)}</td>
      <td>${tokenText(a.expires_at)}</td>
      <td>${err ? `<span class="text-danger">${esc(String(err).slice(0, 80))}</span>` : `<span class="muted">—</span>`}</td>
      <td><div class="page-actions" style="gap:5px">
        <button type="button" class="btn btn-sm" data-action="toggle-dormant" data-id="${esc(a.id)}" data-dormant="${a.dormant ? "1" : "0"}">${a.dormant ? "启用" : "休眠"}</button>
        <button type="button" class="btn btn-sm" data-action="account-probe" data-id="${esc(a.id)}">探活</button>
        <button type="button" class="btn btn-sm" data-action="account-quota" data-id="${esc(a.id)}">额度</button>
        <button type="button" class="btn btn-sm" data-action="account-refresh" data-id="${esc(a.id)}" ${a.has_refresh ? "" : "disabled"}>刷新</button>
        <button type="button" class="btn btn-sm btn-danger" data-action="delete-account" data-id="${esc(a.id)}">移除</button>
      </div></td>
    </tr>`;
  }).join("")}</tbody></table></div>`;
}

async function loadAccountsPage() {
  const box = $("acct-table");
  if (!box) return;
  try {
    const d = await api("GET", "/panel/api/accounts");
    const hint = $("acct-hint");
    if (hint) hint.textContent = "默认全部启用。休眠的账号不参与新对话分配。名称来自微信或 QQ。";
    accountRows = d.accounts || [];
    renderAccounts();
  } catch (e) {
    box.innerHTML = `<p class="text-danger">账号列表加载失败: ${esc(e.message)}</p>`;
  }
}

async function accountAct(id, path, okText) {
  const r = await api("POST", path + "?id=" + encodeURIComponent(id));
  if (r && r.ok === false) throw new Error(r.error || "失败");
  toast(okText(r));
  loadAccountsPage();
}

async function loadKeys() {
  const box = $("key-table");
  if (!box) return;
  try {
    const d = await api("GET", "/panel/api/keys");
    const rows = d.keys || [];
    box.innerHTML = `<div class="table-wrap"><table><thead><tr>
      <th>名称</th><th>密钥</th><th>状态</th><th>最近校验</th><th>操作</th>
    </tr></thead><tbody>${rows.map((k) => `<tr>
      <td>${esc(k.name || k.id)}</td>
      <td class="mono">${esc(k.mask || "****")}</td>
      <td>${k.disabled ? "已停用" : "启用"}</td>
      <td>${k.last_check ? (k.last_ok ? `<span class="text-ok">通过</span>` : `<span class="text-danger">失败</span>`) + `<div class="muted">${esc(k.last_check)}</div>` : `<span class="muted">—</span>`}</td>
      <td><div class="page-actions" style="gap:5px">
        <button type="button" class="btn btn-sm" data-action="copy-key" data-id="${esc(k.id)}">复制</button>
        <button type="button" class="btn btn-sm" data-action="check-key" data-id="${esc(k.id)}">校验</button>
        <button type="button" class="btn btn-sm" data-action="disable-key" data-id="${esc(k.id)}" data-disabled="${k.disabled ? "1" : "0"}">${k.disabled ? "启用" : "停用"}</button>
        <button type="button" class="btn btn-sm btn-danger" data-action="delete-key" data-id="${esc(k.id)}">删除</button>
      </div></td>
    </tr>`).join("")}</tbody></table></div>`;
  } catch (e) {
    box.innerHTML = `<p class="text-danger">${esc(e.message)}</p>`;
  }
}

async function writeClipboard(text) {
  if (!text) return false;
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

async function copyKey(id, config) {
  try {
    const r = await api("POST", "/panel/api/keys/reveal", { id: id || "", config: !!config });
    const ok = await writeClipboard(r.secret || "");
    toast(ok ? "已复制" : "复制失败", !ok);
  } catch (e) { toast(e.message, true); }
}

let pendingKeySecret = "";

function maskKey(secret) {
  if (!secret || secret.length <= 8) return "****";
  return secret.slice(0, 4) + "****" + secret.slice(-4);
}

function openKeyModal() {
  pendingKeySecret = "";
  $("key-modal-name").value = "";
  $("key-modal-err").textContent = "";
  $("key-modal-form").classList.remove("hidden");
  $("key-modal-result").classList.add("hidden");
  $("key-modal").classList.remove("hidden");
  $("key-modal-name").focus();
}

function closeKeyModal() {
  pendingKeySecret = "";
  $("key-modal").classList.add("hidden");
}

async function confirmCreateKey() {
  const name = ($("key-modal-name").value || "").trim();
  if (!name) { $("key-modal-err").textContent = "请填写名称"; return; }
  $("key-modal-err").textContent = "";
  try {
    const r = await api("POST", "/panel/api/keys", { name });
    pendingKeySecret = r.secret || "";
    $("key-result-name").textContent = r.name || name;
    $("key-result-mask").textContent = maskKey(pendingKeySecret);
    $("key-modal-form").classList.add("hidden");
    $("key-modal-result").classList.remove("hidden");
    loadKeys();
  } catch (e) {
    $("key-modal-err").textContent = e.message;
  }
}

async function copyNewKey() {
  const ok = await writeClipboard(pendingKeySecret);
  toast(ok ? "已复制" : "复制失败", !ok);
}

async function checkKey(id, config) {
  toast("正在校验…");
  try {
    const r = await api("POST", "/panel/api/keys/check", { id: id || "", config: !!config });
    toast(r.ok ? "校验通过" : "校验失败: " + (r.error || r.status), !r.ok);
    loadKeys();
  } catch (e) { toast(e.message, true); }
}

async function savePassword() {
  try {
    await api("POST", "/panel/api/password", {
      current: $("pw-current").value,
      password: $("pw-new").value,
    });
    $("pw-current").value = "";
    $("pw-new").value = "";
    toast("登录密码已修改");
  } catch (e) { toast(e.message, true); }
}

async function selectAccount(id) {
  try {
    await api("POST", "/panel/api/accounts/select", { id });
    toast("已切换账号");
    loadAccountsPage();
  } catch (e) { toast(e.message, true); }
}

async function deleteAccount(id) {
  if (!confirm("移除这个账号？")) return;
  try {
    await api("DELETE", "/panel/api/accounts?id=" + encodeURIComponent(id));
    toast("已移除");
    loadAccountsPage();
  } catch (e) { toast(e.message, true); }
}

function toggleManualAccount() {
  const box = $("acct-form");
  if (!box) return;
  box.classList.toggle("hidden");
}

async function saveCredential() {
  const body = {
    name: $("f-name").value.trim(),
    token: $("f-token").value.trim(),
    refresh_token: $("f-refresh").value.trim(),
    uid: $("f-openid").value.trim(),
    guid: $("f-guid").value.trim(),
    base_url: $("f-url").value.trim(),
    login_type: $("f-login-type").value,
    note: $("f-note").value.trim(),
  };
  if (!body.token) { toast("请填写 access token", true); return; }
  try {
    const r = await api("PUT", "/panel/api/credential", body);
    toast(r.revived ? "已保存，并成为当前账号" : "已保存");
    $("f-token").value = "";
    $("f-refresh").value = "";
    loadAccountsPage();
  } catch (e) { toast("保存失败: " + e.message, true); }
}

async function batchAccounts(kind) {
  const ids = targetAccountIds();
  if (!ids.length) { toast("没有账号", true); return; }
  const path = kind === "probe" ? "/panel/api/accounts/probe" : kind === "quota" ? "/panel/api/accounts/quota" : "/panel/api/accounts/refresh-token";
  const msg = $("acct-msg");
  let ok = 0, fail = 0, skip = 0;
  for (let i = 0; i < ids.length; i++) {
    const a = accountRows.find((x) => x.id === ids[i]);
    if (kind === "refresh" && a && !a.has_refresh) { skip++; continue; }
    if (msg) msg.textContent = `正在处理 ${i + 1} / ${ids.length}`;
    try {
      const r = await api("POST", path + "?id=" + encodeURIComponent(ids[i]));
      if (r && r.ok === false) fail++;
      else ok++;
    } catch { fail++; }
  }
  if (msg) msg.textContent = `完成：成功 ${ok}，失败 ${fail}${skip ? "，跳过 " + skip + "（没有 refresh token）" : ""}`;
  loadAccountsPage();
}

async function probeCredential() {
  toast("探活中…");
  try {
    const r = await api("POST", "/panel/api/credential/probe");
    if (r.ok) toast(`✓ 可达，${r.latency_ms}ms，${(r.models || []).length} 个模型`);
    else toast(`✗ 探活失败: ${r.error}`, true);
    loadAccountsPage();
  } catch (e) { toast("探活失败: " + e.message, true); }
}

async function quotaCredential() {
  try {
    const r = await api("POST", "/panel/api/credential/quota");
    if (r.ok) toast(`✓ 配额：剩余 ${fmtQuota(r.remaining)}${r.total ? " / " + fmtQuota(r.total) : ""}`);
    else toast(`✗ 配额查询失败: ${r.error}`, true);
    loadAccountsPage();
  } catch (e) { toast("配额查询失败: " + e.message, true); }
}

async function refreshCredentialToken() {
  toast("正在刷新 token…");
  try {
    const r = await api("POST", "/panel/api/credential/refresh-token");
    if (r.ok) toast("token 已刷新并落盘");
    else toast("刷新失败: " + (r.error || "未知错误"), true);
    loadAccountsPage();
  } catch (e) { toast("刷新失败: " + e.message, true); }
}

async function reviveCredential() {
  try {
    await api("POST", "/panel/api/credential/revive");
    toast("已清除冷却");
    loadAccountsPage();
  } catch (e) { toast("操作失败: " + e.message, true); }
}

async function deleteCredential() {
  if (!confirm("确认删除已保存的凭证？网关将立即转为未配置状态。")) return;
  try {
    await api("DELETE", "/panel/api/credential");
    toast("已删除");
    loadAccountsPage();
  } catch (e) { toast("删除失败: " + e.message, true); }
}

async function refreshQuota() {
  toast("配额刷新已触发…");
  try {
    await api("POST", "/panel/api/refresh");
    setTimeout(() => { current === "accounts" ? loadAccountsPage() : refreshOverview(); }, 3000);
  } catch (e) { toast("触发失败: " + e.message, true); }
}

/* ---------- 用量 ---------- */

async function loadDashUsage() {
  try {
    const u = await api("GET", "/panel/api/usage?hours=168");
    const t = u.totals || {};
    let html = `<p class="muted" style="margin:0 0 10px">近 7 天：<b>${t.requests || 0}</b> 次请求 · 入 ${fmtQuota(t.prompt_tokens || 0)} tok · 出 ${fmtQuota(t.completion_tokens || 0)} tok</p>`;
    if ((u.recent || []).length) {
      html += `<div class="table-wrap"><table><thead><tr><th>时间</th><th>模型</th><th>入/出 tok</th><th>耗时</th><th>状态</th></tr></thead><tbody>`;
      for (const r of u.recent.slice(-12).reverse()) {
        html += `<tr><td class="mono">${esc(new Date(r.time).toLocaleString())}</td><td>${esc(r.model)}</td>
          <td class="num">${fmtQuota(r.prompt_tokens)} / ${fmtQuota(r.completion_tokens)}</td>
          <td class="num">${(r.duration_ms / 1000).toFixed(2)}s</td>
          <td>${r.status}${r.stream ? " · 流式" : ""}</td></tr>`;
      }
      html += `</tbody></table></div>`;
    } else {
      html += `<div class="empty">（暂无请求记录）</div>`;
    }
    $("dash-usage").innerHTML = html;
  } catch (e) { /* 静默 */ }
}

/* ---------- 模型 ---------- */

async function loadModels() {
  $("models-msg").textContent = "加载中…";
  try {
    const d = await api("GET", "/panel/api/models");
    const list = d.models || [];
    let rows = "";
    for (const m of list) {
      rows += `<tr><td class="mono"><b>${esc(m.id)}</b></td><td class="mono muted">${esc(JSON.stringify(m, null, 0).slice(0, 200))}</td></tr>`;
    }
    $("models-table").innerHTML = rows
      ? `<div class="table-wrap"><table><thead><tr><th>模型 ID</th><th>字段</th></tr></thead><tbody>${rows}</tbody></table></div>`
      : `<div class="empty">（目录为空）</div>`;
    $("models-msg").textContent = `共 ${list.length} 个模型`;
  } catch (e) {
    $("models-table").innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`;
    $("models-msg").textContent = "";
  }
}

/* ---------- 请求统计 ---------- */

async function loadStats() {
  $("stats-msg").textContent = "加载中…";
  try {
    const s = await api("GET", "/panel/api/stats");
    const row = (m) => `<tr>
      <td class="mono"><b>${esc(m.model)}</b></td>
      <td class="num">${m.requests}</td>
      <td class="num text-ok">${m.success}</td>
      <td class="num text-danger">${m.failed}</td>
      <td class="num">${m.streaming}</td>
      <td class="num">${m.avg_latency_ms ? m.avg_latency_ms.toFixed(0) + "ms" : "—"}</td>
      <td class="num">${m.tokens_per_sec ? m.tokens_per_sec.toFixed(1) : "—"}</td>
      <td class="num">${fmtQuota(m.prompt_tokens)} / ${fmtQuota(m.completion_tokens)}</td>
      <td>${m.last_seen ? esc(new Date(m.last_seen).toLocaleTimeString()) : "—"}</td>
    </tr>`;
    const head = `<div class="table-wrap"><table><thead><tr><th>模型</th><th>请求</th><th>成功</th><th>失败</th><th>流式</th>
      <th>平均时延</th><th>tok/s</th><th>入/出 tok</th><th>最近</th></tr></thead><tbody>`;
    const rows = (s.models || []).map(row).join("");
    const total = s.total ? `<tr><td class="mono"><b>合计</b></td><td class="num">${s.total.requests}</td><td class="num">${s.total.success}</td>
      <td class="num">${s.total.failed}</td><td class="num">${s.total.streaming}</td><td class="num">${s.total.avg_latency_ms ? s.total.avg_latency_ms.toFixed(0)+"ms" : "—"}</td>
      <td class="num">—</td><td class="num">${fmtQuota(s.total.prompt_tokens)} / ${fmtQuota(s.total.completion_tokens)}</td><td>—</td></tr>` : "";
    $("stats-table").innerHTML = (s.models || []).length ? head + rows + total + `</tbody></table></div><p class="muted" style="margin:10px 0 0">累计自 ${esc(new Date(s.since).toLocaleString())}（运行 ${(s.uptime_sec/60).toFixed(0)} 分钟）</p>`
      : `<div class="empty">（暂无请求）</div>`;
    $("stats-msg").textContent = "";
  } catch (e) {
    $("stats-table").innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`;
    $("stats-msg").textContent = "";
  }
}

async function resetStats() {
  if (!confirm("确认清空请求统计累计？")) return;
  try {
    await api("POST", "/panel/api/stats/reset");
    toast("统计已清空");
    loadStats();
  } catch (e) { toast("操作失败: " + e.message, true); }
}

/* ---------- 聊天测试 ---------- */

async function loadChatModels() {
  try {
    const d = await api("GET", "/panel/api/models");
    const sel = $("c-model");
    const prev = sel.value;
    sel.innerHTML = "";
    for (const m of (d.models || [])) {
      const opt = document.createElement("option");
      opt.value = m.id; opt.textContent = m.id;
      sel.appendChild(opt);
    }
    if (prev && [...sel.options].some((o) => o.value === prev)) sel.value = prev;
  } catch (e) { /* 未配置凭证/拉取失败时静默：聊天页仍可用手填模型 */ }
}

async function sendChat() {
  const out = $("c-out");
  const btn = $("c-send");
  const model = $("c-model").value || $("c-model").options[0]?.value || "deepseek-v4-pro-external";
  const msgs = [];
  const sys = $("c-sys").value.trim();
  if (sys) msgs.push({ role: "system", content: sys });
  msgs.push({ role: "user", content: $("c-input").value });
  const body = { model, messages: msgs, stream: $("c-stream").checked };
  if ($("c-temp").value !== "") body.temperature = parseFloat($("c-temp").value);

  btn.disabled = true;
  $("c-status").textContent = "请求中…";
  out.textContent = "";
  const started = performance.now();
  try {
    const resp = await fetch("/panel/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (resp.status === 401) {
      showLogin("登录状态已失效，请重新登录");
      throw new Error("未登录");
    }
    if (!resp.ok) {
      let msg = "HTTP " + resp.status;
      try {
        const j = await resp.json();
        const err = j && j.error;
        msg = (err && (err.message || (typeof err === "string" ? err : ""))) || msg;
      } catch { /* 非 JSON 错误体 */ }
      throw new Error(msg);
    }
    if (body.stream) {
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "", usage = null, reasoning = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const lines = buf.split("\n");
        buf = lines.pop();
        for (const line of lines) {
          const t = line.trim();
          if (!t.startsWith("data:")) continue;
          const payload = t.slice(5).trim();
          if (payload === "[DONE]") continue;
          try {
            const j = JSON.parse(payload);
            if (j.usage) usage = j.usage;
            const delta = j.choices?.[0]?.delta;
            if (delta?.reasoning_content) { reasoning += delta.reasoning_content; }
            if (delta?.content) out.textContent += delta.content;
          } catch { /* 忽略非 JSON 行 */ }
        }
      }
      if (reasoning) out.textContent = `「推理」${reasoning}\n\n` + out.textContent;
      const secs = ((performance.now() - started) / 1000).toFixed(2);
      $("c-meta").textContent = `流式 · 总耗时 ${secs}s` + (usage ? ` · in ${usage.prompt_tokens} / out ${usage.completion_tokens} tok` : "");
    } else {
      const j = await resp.json();
      const msg = j.choices?.[0]?.message;
      out.textContent = (msg?.reasoning_content ? "「推理」" + msg.reasoning_content + "\n\n" : "") + (msg?.content ?? JSON.stringify(j, null, 2));
      const secs = ((performance.now() - started) / 1000).toFixed(2);
      $("c-meta").textContent = `非流式 · ${secs}s` + (j.usage ? ` · in ${j.usage.prompt_tokens} / out ${j.usage.completion_tokens} tok` : "");
    }
    $("c-status").textContent = "完成";
  } catch (e) {
    out.textContent = "请求失败: " + e.message;
    $("c-status").textContent = "失败";
  } finally { btn.disabled = false; }
}

/* ---------- 配置 ---------- */

async function loadConfig() {
  try {
    const d = await api("GET", "/panel/api/config");
    $("cfg-json").value = JSON.stringify(d.config, null, 2);
    $("cfg-msg").textContent = "";
  } catch (e) { toast("配置加载失败: " + e.message, true); }
}

async function saveConfig() {
  let obj;
  try { obj = JSON.parse($("cfg-json").value); }
  catch (e) { toast("非法 JSON: " + e.message, true); return; }
  try {
    const r = await api("POST", "/panel/api/config", obj);
    const n = (r.restart_required || []).length;
    $("cfg-msg").textContent = n ? `已保存。${n} 个字段需重启生效。` : "已保存，全部字段热生效。";
    toast("配置已保存");
  } catch (e) { toast("保存失败: " + e.message, true); }
}

/* ---------- 日志 ---------- */

let logTimer = null;

async function loadLogs() {
  try {
    const d = await api("GET", "/panel/api/logs");
    const lines = (d.entries || []).map((e) => {
      const color = e.channel === "chat" ? "text-accent" : e.channel === "probe" ? "text-ok" : "text-dim";
      return `<span class="${color}">[${esc(e.channel)}]</span> ${esc(e.text)}`;
    });
    $("logs-out").innerHTML = lines.join("<br>") || `<span class="muted">（暂无日志）</span>`;
    $("logs-out").scrollTop = $("logs-out").scrollHeight;
  } catch (e) { /* 静默（视图不可见时轮询照跑） */ }
}

/* ---------- 微信扫码登录（无 App 授权） ---------- */

let wxQrSession = null;   // 当前会话 id
let wxQrAborted = false;  // 关闭弹窗后终止轮询
let qrPollPath = "/panel/api/login/wechat/poll";

function startWechatQR() { startQR("/panel/api/login/wechat/start", "/panel/api/login/wechat/poll", "微信扫码登录", "请用微信扫码并在手机上确认"); }
function startQQQR() { startQR("/panel/api/login/qq/start", "/panel/api/login/qq/poll", "QQ扫码登录", "请用手机 QQ 扫码并确认"); }

async function startQR(startPath, pollPath, title, hint) {
  wxQrAborted = false;
  qrPollPath = pollPath;
  $("wx-qr-title").textContent = title;
  $("wx-qr-img").removeAttribute("src");
  $("wx-qr-status").textContent = "正在获取二维码…";
  $("wx-qr-mask").classList.remove("hidden");
  try {
    const r = await api("POST", startPath);
    if (wxQrAborted) return;
    wxQrSession = r.session_id;
    $("wx-qr-img").src = r.qr;
    $("wx-qr-status").textContent = hint;
    pollWechatQR();
  } catch (e) {
    $("wx-qr-status").textContent = "发起失败: " + e.message;
  }
}

async function pollWechatQR() {
  while (!wxQrAborted && wxQrSession) {
    try {
      const r = await api("GET", qrPollPath + "?id=" + encodeURIComponent(wxQrSession));
      if (wxQrAborted) return;
      const s = r.status;
      if (s === "scanned") $("wx-qr-status").textContent = "已扫码，请在手机上确认…";
      if (s === "waiting") $("wx-qr-status").textContent = "等待扫码…";
      if (s === "expired" || s === "timed_out") { $("wx-qr-status").textContent = "二维码已失效，请关闭后重新发起"; return; }
      if (s === "error") { $("wx-qr-status").textContent = "授权失败: " + (r.error || "未知错误"); return; }
      if (s === "done") {
        $("wx-qr-status").textContent = "登录成功，凭证已保存";
        toast("已加入账号，内核正在启动");
        closeWechatQR(true);
        loadAccountsPage().catch(() => {});
        refreshOverview().catch(() => {});
        return;
      }
    } catch (e) {
      if (wxQrAborted) return;
      $("wx-qr-status").textContent = "轮询失败: " + e.message;
      return;
    }
  }
}

function closeWechatQR(silent) {
  wxQrAborted = true;
  wxQrSession = null;
  $("wx-qr-mask").classList.add("hidden");
  if (!silent) return;
}

/* ---------- 启动 ---------- */

/* CSP 的 script-src 'self' 会拦截 HTML 里的 onclick，所以按钮一律用 data-action。 */
document.addEventListener("click", (ev) => {
  const el = ev.target.closest("[data-action]");
  if (!el || el.disabled) return;
  switch (el.dataset.action) {
    case "logout": doLogout(); break;
    case "save-credential": saveCredential(); break;
    case "wechat-qr": startWechatQR(); break;
    case "qq-qr": startQQQR(); break;
    case "wechat-qr-close": closeWechatQR(); break;
    case "probe-credential": probeCredential(); break;
    case "quota-credential": quotaCredential(); break;
    case "refresh-credential-token": refreshCredentialToken(); break;
    case "revive-credential": reviveCredential(); break;
    case "delete-credential": deleteCredential(); break;
    case "select-account": selectAccount(el.dataset.id); break;
    case "toggle-dormant":
      api("POST", "/panel/api/accounts/dormant", { id: el.dataset.id, dormant: el.dataset.dormant !== "1" })
        .then(() => loadAccountsPage())
        .catch((e) => toast(e.message, true));
      break;
    case "reload-keys": loadKeys(); break;
    case "create-key": openKeyModal(); break;
    case "confirm-create-key": confirmCreateKey(); break;
    case "copy-new-key": copyNewKey(); break;
    case "close-key-modal": closeKeyModal(); break;
    case "save-password": savePassword(); break;
    case "check-key": checkKey(el.dataset.id, el.dataset.config === "1"); break;
    case "copy-key": copyKey(el.dataset.id, el.dataset.config === "1"); break;
    case "disable-key":
      api("POST", "/panel/api/keys/disable", { id: el.dataset.id, disabled: el.dataset.disabled !== "1" })
        .then(() => loadKeys()).catch((e) => toast(e.message, true));
      break;
    case "delete-key":
      if (!confirm("删除这把 API Key？")) break;
      api("DELETE", "/panel/api/keys?id=" + encodeURIComponent(el.dataset.id))
        .then(() => loadKeys()).catch((e) => toast(e.message, true));
      break;
    case "delete-account": deleteAccount(el.dataset.id); break;
    case "reload-accounts": loadAccountsPage(); break;
    case "toggle-manual-account": toggleManualAccount(); break;
    case "toggle-account": {
      const id = el.dataset.id;
      if (selectedIds.has(id)) selectedIds.delete(id); else selectedIds.add(id);
      renderAccounts();
      break;
    }
    case "toggle-all-accounts": {
      const rows = visibleAccounts();
      const allOn = rows.every((a) => selectedIds.has(a.id));
      rows.forEach((a) => allOn ? selectedIds.delete(a.id) : selectedIds.add(a.id));
      renderAccounts();
      break;
    }
    case "clear-account-selection": selectedIds.clear(); renderAccounts(); break;
    case "batch-probe": batchAccounts("probe"); break;
    case "batch-quota": batchAccounts("quota"); break;
    case "batch-refresh": batchAccounts("refresh"); break;
    case "account-probe":
      accountAct(el.dataset.id, "/panel/api/accounts/probe", (r) => "探活成功，" + r.latency_ms + " 毫秒" + (r.name ? "，" + r.name : ""))
        .catch((e) => { toast(e.message, true); loadAccountsPage(); });
      break;
    case "account-quota":
      accountAct(el.dataset.id, "/panel/api/accounts/quota", (r) => "剩余 " + Number(r.remaining).toLocaleString() + " / " + Number(r.total).toLocaleString()).catch((e) => toast(e.message, true));
      break;
    case "account-refresh":
      accountAct(el.dataset.id, "/panel/api/accounts/refresh-token", () => "Token 已刷新").catch((e) => toast(e.message, true));
      break;
    case "refresh-quota": refreshQuota(); break;
    case "reset-stats": resetStats(); break;
    case "send-chat": sendChat(); break;
    case "save-config": saveConfig(); break;
  }
});
$("login-form").addEventListener("submit", (e) => {
  e.preventDefault();
  doLogin();
});
const acctQ = $("acct-q");
if (acctQ) acctQ.addEventListener("input", renderAccounts);
window.addEventListener("hashchange", route);

// 首屏探测：免鉴权模式或已有会话 → 直接进面板；否则停留登录页。
(async () => {
  try {
    const s = await fetch("/panel/api/session").then((r) => r.json());
    if (s.auth_required === false || s.authenticated === true) {
      hideLogin();
      $("foot-version").textContent = s.version ? "v" + s.version : "";
    } else {
      showLogin();
      $("login-default").classList.toggle("hidden", !s.using_default_password);
    }
  } catch {
    showLogin("无法连接网关，请确认服务已启动");
  }
  route();
})();
