const state = {
  me: null,
  view: "overview",
  dashboard: null,
  accounts: [],
  accountSummary: null,
  prices: [],
  pricingSync: null,
  billing: null,
  usage: [],
  audits: [],
  daily: [],
  authorization: null,
  purposes: [],
  proxyPools: [],
  proxies: [],
  users: [],
  keys: [],
  accountGroup: "",
  accountSearch: "",
  accountStatus: "",
  accountsLoadedAt: 0,
  breakdown: "group",
  proxyPoolFilter: "",
  oauthSessionID: "",
  batchResults: [],
  paginationPages: {},
  paginationSizes: {},
  serverPagination: {},
  selectedAccountIDs: new Set(),
};

const viewMeta = {
  overview: ["CONTROL / OVERVIEW", "运行概览", "新增用途"],
  accounts: ["POOL / ACCOUNTS", "账号池", "新增账号"],
  proxies: ["NETWORK / PROXIES", "代理池", "批量导入"],
  access: ["ACCESS / USERS & KEYS", "用户与 SK", "生成 SK"],
  pricing: ["BILLING / MODEL PRICES", "模型价格", "手动价格"],
  billing: ["LEDGER / BILLING", "计费中心", "记录用量"],
  audit: ["SECURITY / AUDIT TRAIL", "操作日志", ""],
  dead: ["LIFECYCLE / DEAD ACCOUNTS", "死亡账户", ""],
  onboarding: ["POOL / BATCH ONBOARDING", "批量上号", ""],
  daily: ["ANALYTICS / DAILY", "每日统计", ""],
  authorization: ["SECURITY / AUTHORIZATION", "授权统计", ""],
};
const rolePageDefaults = {
  admin: Object.keys(viewMeta),
  readonly_admin: [
    "overview",
    "accounts",
    "dead",
    "daily",
    "authorization",
    "proxies",
    "pricing",
    "billing",
    "audit",
  ],
  user: ["access"],
};
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const isAdmin = () => state.me?.role === "admin";
const isManager = () =>
  state.me?.role === "admin" || state.me?.role === "readonly_admin";
const canView = (page) =>
  state.me?.role === "admin" || state.me?.visible_pages?.includes(page);
const uiChoices = new Map();
const sidebarStorageKey = "ccmax.sidebar.collapsed";
const paginationTables = {
  accounts: { body: "accounts-body", size: 20, render: renderAccounts },
  dead: { body: "dead-accounts-body", size: 20, render: renderDeadAccounts },
  batch: { body: "batch-result-body", size: 20, render: renderBatchResults },
  daily: { body: "daily-body", size: 10, render: renderDaily },
  authorization: {
    body: "authorization-body",
    size: 20,
    render: renderAuthorization,
  },
  proxies: { body: "proxies-body", size: 20, render: renderProxies },
  users: { body: "users-body", size: 10, render: renderAccess },
  keys: { body: "keys-body", size: 20, render: renderAccess },
  prices: { body: "prices-body", size: 20, render: renderPriceTable },
  usage: { body: "usage-body", size: 20, render: renderBilling },
  audit: { body: "audit-body", size: 20, render: renderAudit },
};

function resetPagination(key) {
  state.paginationPages[key] = 1;
}
function renderPagination(key, total, page, pageSize, totalPages) {
  const config = paginationTables[key];
  const tableWrap = document.getElementById(config.body)?.closest(".table-wrap");
  if (!tableWrap) return;
  let footer = tableWrap.parentElement.querySelector(
    `.table-pagination[data-pagination="${key}"]`,
  );
  if (!footer) {
    footer = document.createElement("div");
    footer.className = "table-pagination";
    footer.dataset.pagination = key;
    tableWrap.insertAdjacentElement("afterend", footer);
  }
  footer.hidden = total === 0;
  footer.innerHTML = `
    <span class="pagination-summary">共 <b>${total.toLocaleString("zh-CN")}</b> 条</span>
    <div class="pagination-controls">
      <label class="pagination-size"><span>每页</span><select data-pagination-size="${key}" aria-label="${key} 每页条数">${[10, 20, 50, 100]
        .map(
          (size) =>
            `<option value="${size}" ${size === pageSize ? "selected" : ""}>${size}</option>`,
        )
        .join("")}</select></label>
      <button type="button" data-pagination-key="${key}" data-page-step="-1" title="上一页" aria-label="上一页" ${page <= 1 ? "disabled" : ""}><i data-lucide="chevron-left"></i></button>
      <span class="pagination-page" aria-live="polite">${page} / ${totalPages}</span>
      <button type="button" data-pagination-key="${key}" data-page-step="1" title="下一页" aria-label="下一页" ${page >= totalPages ? "disabled" : ""}><i data-lucide="chevron-right"></i></button>
    </div>`;
  refreshIcons();
}
function paginatedItems(key, items) {
  const config = paginationTables[key];
  const server = state.serverPagination[key];
  if (server) {
    state.paginationPages[key] = server.page;
    state.paginationSizes[key] = server.pageSize;
    renderPagination(
      key,
      server.total,
      server.page,
      server.pageSize,
      server.totalPages,
    );
    return items;
  }
  const pageSize = Number(state.paginationSizes[key] || config.size);
  const totalPages = Math.max(1, Math.ceil(items.length / pageSize));
  const page = Math.min(
    Math.max(1, Number(state.paginationPages[key] || 1)),
    totalPages,
  );
  state.paginationPages[key] = page;
  state.paginationSizes[key] = pageSize;
  renderPagination(key, items.length, page, pageSize, totalPages);
  return items.slice((page - 1) * pageSize, page * pageSize);
}

function setServerPagination(key, payload) {
  state.serverPagination[key] = {
    total: Number(payload.total ?? payload.summary?.total ?? 0),
    page: Number(payload.page || 1),
    pageSize: Number(payload.page_size || paginationTables[key].size),
    totalPages: Number(payload.total_pages || 1),
  };
}

function paginationParams(key) {
  return {
    page: String(state.paginationPages[key] || 1),
    page_size: String(
      state.paginationSizes[key] || paginationTables[key].size,
    ),
  };
}

async function loadServerPage(key) {
  if (key === "usage") return loadBilling();
  if (key === "audit") return loadAudit();
  if (key === "authorization") return loadAuthorization();
}

function refreshIcons() {
  if (window.lucide)
    window.lucide.createIcons({
      attrs: { "aria-hidden": "true", "stroke-width": 1.8 },
    });
}
function initUIComponents() {
  $$(".icon-button[data-close-dialog]").forEach((button) => {
    button.innerHTML = '<i data-lucide="x"></i>';
  });
  if (window.Choices)
    $$("select[data-choice]").forEach((select) => {
      if (!uiChoices.has(select.id))
        uiChoices.set(
          select.id,
          new window.Choices(select, {
            searchEnabled: false,
            itemSelectText: "",
            shouldSort: false,
            allowHTML: false,
          }),
        );
    });
  refreshIcons();
}
function resetDialogViewport(dialog) {
  dialog.scrollTop = 0;
  $$("form, .form-grid, .form-stack, .secret-body", dialog).forEach((node) => {
    node.scrollTop = 0;
  });
}
function showInitializedDialog(selector) {
  const dialog = $(selector);
  resetDialogViewport(dialog);
  dialog.showModal();
  requestAnimationFrame(() => resetDialogViewport(dialog));
}
function setSidebarCollapsed(collapsed) {
  const shell = $("#app-shell");
  const button = $("#sidebar-toggle");
  shell.classList.toggle("sidebar-collapsed", collapsed);
  button.setAttribute("aria-expanded", String(!collapsed));
  button.setAttribute("aria-label", collapsed ? "展开侧栏" : "收起侧栏");
  button.title = collapsed ? "展开侧栏" : "收起侧栏";
  button.innerHTML = `<i data-lucide="${collapsed ? "panel-left-open" : "panel-left-close"}"></i>`;
  refreshIcons();
}
function initializeSidebar() {
  let collapsed = false;
  try {
    collapsed = localStorage.getItem(sidebarStorageKey) === "true";
  } catch {
    // Storage can be unavailable in locked-down browser contexts.
  }
  setSidebarCollapsed(collapsed);
}
function setChoiceValue(selector, value) {
  const element = $(selector);
  element.value = value;
  uiChoices.get(element.id)?.setChoiceByValue(String(value));
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  if (response.status === 204) return null;
  const payload = await response.json().catch(() => ({}));
  if (response.status === 401 && path !== "/api/auth/login") showLogin();
  if (!response.ok)
    throw new Error(payload.error || `请求失败 (${response.status})`);
  return payload;
}

function escapeHTML(value) {
  return String(value ?? "").replace(
    /[&<>'"]/g,
    (char) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" })[
        char
      ],
  );
}
function groupMark(id, modifier = "") {
  const group = String(id).toLowerCase() === "b" ? "b" : "a";
  return `<span class="group-mark ${group} ${modifier}" title="${group.toUpperCase()} 分组"><svg aria-hidden="true"><use href="#claude-group-icon"></use></svg><span>${group.toUpperCase()}</span></span>`;
}
function money(value) {
  const number = Number(value || 0);
  return number === 0
    ? "$0.00"
    : number < 0.01
      ? `$${number.toFixed(6)}`
      : `$${number.toFixed(2)}`;
}
function compact(value) {
  return new Intl.NumberFormat("zh-CN", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(Number(value || 0));
}
function dateTime(value) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? value
    : date.toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
      });
}
function dateTimeInput(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000)
    .toISOString()
    .slice(0, 16);
}
function isoFromInput(value) {
  return value ? new Date(value).toISOString() : "";
}
function toast(message, type = "success") {
  const node = document.createElement("div");
  node.className = `toast ${type === "error" ? "error" : ""}`;
  node.textContent = message;
  $("#toast-region").append(node);
  setTimeout(() => node.remove(), 3200);
}
function confirmAction(title, message, confirmLabel = "确认") {
  const dialog = $("#confirm-dialog");
  dialog.returnValue = "";
  $("#confirm-title").textContent = title;
  $("#confirm-message").textContent = message;
  $("#confirm-submit").textContent = confirmLabel;
  showInitializedDialog("#confirm-dialog");
  return new Promise((resolve) => {
    dialog.addEventListener(
      "close",
      () => resolve(dialog.returnValue === "confirm"),
      { once: true },
    );
  });
}
function roleName(role) {
  return (
    { admin: "管理员", readonly_admin: "只读管理员", user: "普通用户" }[role] ||
    role
  );
}

function showLogin() {
  $("#app-shell").hidden = true;
  $("#login-screen").hidden = false;
}
function showApp() {
  $("#login-screen").hidden = true;
  $("#app-shell").hidden = false;
}

async function boot() {
  initUIComponents();
  initializeSidebar();
  try {
    const session = await api("/api/auth/session");
    if (!session.authenticated) {
      showLogin();
      return;
    }
    state.me = session.user;
    showApp();
    configureRole();
    initializeDates();
    await loadCore();
  } catch {
    showLogin();
  }
}

function configureRole() {
  state.selectedAccountIDs.clear();
  $("#app-shell").dataset.role = state.me.role;
  $("#identity-name").textContent = state.me.name || state.me.username;
  $("#identity-role").textContent = roleName(state.me.role);
  $("#readonly-banner").hidden = state.me.role !== "readonly_admin";
  $$(".nav-item[data-view]").forEach((node) => {
    node.hidden = !canView(node.dataset.view);
  });
  $$(".write-action").forEach((node) => {
    node.hidden = !isAdmin();
  });
  $("#add-api-key").hidden =
    state.me.role === "readonly_admin" || !canView("access");
  const initial =
    $$(".nav-item[data-view]").find((node) => !node.hidden)?.dataset.view ||
    "access";
  setView(initial);
}

async function loadCore() {
  $("#connection-status").textContent = "同步中";
  try {
    if (canView("overview")) {
      state.dashboard = await api("/api/dashboard");
      state.purposes = state.dashboard.purposes;
      renderDashboard();
    }
    if (canView("billing") && !canView("overview"))
      state.purposes = await api("/api/purposes");
    if (canView("accounts") || canView("dead") || canView("onboarding")) {
      await loadAccounts();
    }
    if (canView("proxies") || canView("onboarding")) {
      [state.proxyPools, state.proxies] = await Promise.all([
        api("/api/proxy-pools"),
        api("/api/proxies"),
      ]);
      if (canView("proxies")) renderProxies();
    }
    if (canView("pricing")) {
      [state.prices, state.pricingSync] = await Promise.all([
        api("/api/prices"),
        api("/api/prices/sync-status"),
      ]);
      renderPriceTable();
      renderPriceSync();
    }
    if (canView("access")) {
      state.keys = await api("/api/api-keys");
      if (isAdmin()) state.users = await api("/api/users");
      renderAccess();
    }
    populateSelects();
    $("#connection-status").textContent = "运行正常";
    refreshIcons();
  } catch (error) {
    $("#connection-status").textContent = "连接异常";
    toast(error.message, "error");
  }
}

async function loadAccounts() {
  if (!canView("accounts") && !canView("dead") && !canView("onboarding"))
    return;
  state.accounts = await api("/api/accounts");
  const accountIDs = new Set(state.accounts.map((item) => item.id));
  state.selectedAccountIDs = new Set(
    [...state.selectedAccountIDs].filter((id) => accountIDs.has(id)),
  );
  state.accountsLoadedAt = performance.now();
  renderAccounts();
  renderDeadAccounts();
  if (canView("accounts")) await loadAccountSummary();
}

async function loadBilling() {
  if (!canView("billing")) return;
  const params = new URLSearchParams(paginationParams("usage"));
  for (const [key, selector] of [
    ["from", "#billing-from"],
    ["to", "#billing-to"],
    ["group_id", "#billing-group"],
    ["purpose_key", "#billing-purpose"],
  ])
    if ($(selector).value) params.set(key, $(selector).value);
  try {
    const [billing, usage] = await Promise.all([
      api(`/api/billing?${params}`),
      api(`/api/usage?${params}`),
    ]);
    state.billing = billing;
    state.usage = usage.items;
    setServerPagination("usage", usage);
    renderBilling();
  } catch (error) {
    toast(error.message, "error");
  }
}

async function loadAccountSummary() {
  if (!canView("accounts")) return;
  const params = new URLSearchParams();
  if (state.accountSearch) params.set("search", state.accountSearch);
  if (state.accountGroup) params.set("group_id", state.accountGroup);
  if (state.accountStatus) params.set("status", state.accountStatus);
  if ($("#account-from").value) params.set("from", $("#account-from").value);
  if ($("#account-to").value) params.set("to", $("#account-to").value);
  try {
    state.accountSummary = await api(`/api/accounts/summary?${params}`);
    renderAccountSummary();
  } catch (error) {
    toast(error.message, "error");
  }
}

async function loadAudit() {
  if (!canView("audit")) return;
  const params = new URLSearchParams(paginationParams("audit"));
  if ($("#audit-actor").value) params.set("actor", $("#audit-actor").value);
  if ($("#audit-action").value) params.set("action", $("#audit-action").value);
  if ($("#audit-from").value) params.set("from", $("#audit-from").value);
  if ($("#audit-to").value) params.set("to", $("#audit-to").value);
  try {
    const data = await api(`/api/audit-logs?${params}`);
    state.audits = data.items;
    setServerPagination("audit", data);
    renderAudit();
  } catch (error) {
    toast(error.message, "error");
  }
}

async function loadDaily() {
  if (!canView("daily")) return;
  try {
    state.daily = await api(`/api/stats/daily?days=${$("#daily-days").value}`);
    renderDaily();
  } catch (error) {
    toast(error.message, "error");
  }
}

async function loadAuthorization() {
  if (!canView("authorization")) return;
  const params = new URLSearchParams(paginationParams("authorization"));
  if ($("#authorization-status").value)
    params.set("status", $("#authorization-status").value);
  if ($("#authorization-from").value)
    params.set("from", $("#authorization-from").value);
  if ($("#authorization-to").value)
    params.set("to", $("#authorization-to").value);
  try {
    state.authorization = await api(`/api/authorization-logs?${params}`);
    setServerPagination("authorization", state.authorization);
    renderAuthorization();
  } catch (error) {
    toast(error.message, "error");
  }
}

function metric(label, value, note, tone = "") {
  return `<article class="metric ${tone}"><span class="metric-label">${escapeHTML(label)}</span><strong>${escapeHTML(value)}</strong><small>${escapeHTML(note)}</small></article>`;
}

function renderDashboard() {
  const data = state.dashboard;
  if (!data) return;
  $("#nav-account-count").textContent = data.accounts_total;
  $("#nav-dead-count").textContent = data.accounts_dead;
  $("#overview-metrics").innerHTML = [
    metric(
      "ACTIVE ACCOUNTS",
      `${data.accounts_active} / ${data.accounts_total}`,
      `可参与调度 · 暂不可调度 ${data.accounts_unavailable} · 错误 ${data.accounts_dead}`,
      "a",
    ),
    metric(
      "TODAY BILLED",
      money(data.today.billed_cost),
      `${data.today.requests} 次请求`,
    ),
    metric("MONTH ACTUAL", money(data.month.actual_cost), "账号成本", "b"),
    metric(
      "MONTH MARGIN",
      money(data.month.margin),
      `收入 ${money(data.month.billed_cost)}`,
    ),
  ].join("");
  $("#purpose-list").innerHTML = data.purposes
    .map(
      (item) =>
        `<div class="purpose-row"><div class="purpose-name"><strong>${escapeHTML(item.name)}</strong><small>${escapeHTML(item.key)}</small></div><span class="purpose-description">${escapeHTML(item.description || "—")}</span><div class="purpose-actions"><div class="segmented"><button class="group-a ${item.active_group_id === "a" ? "active" : ""}" data-purpose-switch="${item.id}" data-group="a" ${isAdmin() ? "" : "disabled"}>${groupMark("a", "button-mark")}</button><button class="group-b ${item.active_group_id === "b" ? "active" : ""}" data-purpose-switch="${item.id}" data-group="b" ${isAdmin() ? "" : "disabled"}>${groupMark("b", "button-mark")}</button></div>${isAdmin() ? `<button class="icon-button compact" data-edit-purpose="${item.id}" title="编辑用途">···</button>` : ""}</div></div>`,
    )
    .join("");
  $("#group-cards").innerHTML = data.groups
    .map((item) => {
      const ratio = item.total_accounts
        ? Math.round((item.active_accounts / item.total_accounts) * 100)
        : 0;
      return `<article class="group-card ${item.id}"><div class="group-card-head">${groupMark(item.id, "large")}${isAdmin() ? `<button class="icon-button group-settings" data-edit-group="${item.id}">···</button>` : ""}</div><h3>${escapeHTML(item.name)}</h3><p>${escapeHTML(item.description || "—")}</p><div class="group-stat-line"><span>可用账号</span><strong>${item.active_accounts} / ${item.total_accounts}</strong></div><div class="capacity-bar"><span style="width:${ratio}%"></span></div><div class="group-stat-line"><span>本月计费</span><strong>${money(item.month_billed_cost)}</strong></div><div class="group-stat-line"><span>计费倍率</span><strong>× ${Number(item.rate_multiplier).toFixed(2)}</strong></div></article>`;
    })
    .join("");
  $("#recent-usage-body").innerHTML = usageRows(data.recent_usage, true);
}

function accountStatus(account) {
  if (account.dispatch_status === "error") return ["错误", "error"];
  if (account.dispatch_status === "unavailable")
    return ["暂不可调度", "off"];
  return ["正常", "ok"];
}
function accountAuthStatus(account) {
  if (account.auth_status === "reauth_required")
    return account.has_credentials
      ? ["需重新授权", "error"]
      : ["待授权", "off"];
  if (account.auth_status === "valid") return ["授权有效", "ok"];
  return ["待验证", "off"];
}
function quotaCell(value, resetAt) {
  const used = Math.max(0, Math.min(Number(value || 0), 100));
  return `<div class="quota-cell"><div><strong>${used.toFixed(1)}%</strong><small>${resetAt ? dateTime(resetAt) : "等待刷新"}</small></div><span><i style="width:${used}%"></i></span></div>`;
}
function subscriptionName(value) {
  return (
    {
      enterprise: "Enterprise",
      team: "Team",
      max: "Max",
      pro: "Pro",
      free: "Free",
    }[String(value || "").toLowerCase()] ||
    value ||
    "未识别"
  );
}
function durationText(seconds) {
  const value = Math.max(0, Number(seconds || 0));
  if (!value) return "—";
  const days = Math.floor(value / 86400);
  const hours = Math.floor((value % 86400) / 3600);
  const minutes = Math.floor((value % 3600) / 60);
  if (days) return `${days}天 ${hours}小时`;
  if (hours) return `${hours}小时 ${minutes}分`;
  return `${Math.max(1, minutes)}分钟`;
}
function durationClockText(minutes, started = true) {
  if (!started) return "—";
  const value = Math.max(0, Math.floor(Number(minutes || 0)));
  const days = Math.floor(value / 1440);
  const hours = Math.floor((value % 1440) / 60);
  const remaining = value % 60;
  const clock = [hours, remaining]
    .map((part) => String(part).padStart(2, "0"))
    .join(":");
  return days ? `${days}天 ${clock}` : clock;
}
function liveSurvivalMinutes(item) {
  if (!item.onboarded_at) return null;
  const baseSeconds = Math.max(0, Number(item.survival_seconds || 0));
  if (item.invalidated_at || !state.accountsLoadedAt)
    return Math.floor(baseSeconds / 60);
  const elapsedSeconds = Math.max(
    0,
    (performance.now() - state.accountsLoadedAt) / 1000,
  );
  return Math.floor((baseSeconds + elapsedSeconds) / 60);
}
function survivalCell(item) {
  const minutes = liveSurvivalMinutes(item);
  const status = item.invalidated_at ? "已停止计时" : "每分钟更新";
  return `<time class="live-duration mono" data-account-survival="${item.id}" title="${status}">${durationClockText(minutes, minutes !== null)}</time>`;
}
function updateSurvivalClocks() {
  $$('[data-account-survival]').forEach((node) => {
    const item = state.accounts.find(
      (account) => account.id === Number(node.dataset.accountSurvival),
    );
    if (!item) return;
    const minutes = liveSurvivalMinutes(item);
    node.textContent = durationClockText(minutes, minutes !== null);
  });
}
function accountUsageCell(item) {
  const row = (label, value, resetAt) => {
    const used = Math.max(0, Math.min(Number(value || 0), 100));
    return `<div class="usage-window-row"><b>${label}</b><span><i style="width:${used}%"></i></span><strong>${used.toFixed(0)}%</strong><small>${resetAt ? dateTime(resetAt) : "等待刷新"}</small></div>`;
  };
  return `<div class="usage-window">${row("5h", item.quota_5h_utilization, item.quota_5h_reset_at)}${row("7d", item.quota_7d_utilization, item.quota_7d_reset_at)}<small>${item.request_count.toLocaleString("zh-CN")} 请求 · ${compact(item.input_tokens + item.output_tokens)} Token</small></div>`;
}
function renderAccountSummary() {
  const item = state.accountSummary;
  if (!item) return;
  $("#account-summary-metrics").innerHTML = [
    metric(
      "FILTERED ACCOUNTS",
      item.accounts,
      `${item.active_accounts} 个可调度`,
      "a",
    ),
    metric("BILLED", money(item.billed_cost), `${item.requests} 次请求`),
    metric("ACTUAL COST", money(item.actual_cost), "筛选区间账号成本", "b"),
    metric(
      "TOKENS",
      compact(item.input_tokens + item.output_tokens),
      `输入 ${compact(item.input_tokens)} / 输出 ${compact(item.output_tokens)}`,
    ),
  ].join("");
}
function renderAccounts() {
  const search = state.accountSearch.toLowerCase();
  const scoped = state.accounts.filter(
    (item) =>
      (!state.accountGroup || item.group_ids.includes(state.accountGroup)) &&
      (!search ||
        `${item.name} ${item.notes} ${item.credential_hint}`
          .toLowerCase()
          .includes(search)),
  );
  const counts = {
    normal: scoped.filter((item) => item.dispatch_status === "normal").length,
    unavailable: scoped.filter(
      (item) => item.dispatch_status === "unavailable",
    ).length,
    error: scoped.filter((item) => item.dispatch_status === "error").length,
  };
  $("#status-all-count").textContent = scoped.length;
  $("#status-normal-count").textContent = counts.normal;
  $("#status-unavailable-count").textContent = counts.unavailable;
  $("#status-error-count").textContent = counts.error;
  $$("#account-status-tabs button").forEach((node) => {
    node.classList.toggle(
      "active",
      node.dataset.accountStatus === state.accountStatus,
    );
  });
  const filtered = scoped.filter(
    (item) =>
      !state.accountStatus || item.dispatch_status === state.accountStatus,
  );
  $("#account-list-count").textContent = `${filtered.length} 个账号`;
  $("#nav-account-count").textContent = state.accounts.length;
  $("#accounts-empty").hidden = filtered.length > 0;
  const pageItems = paginatedItems("accounts", filtered);
  $("#accounts-body").innerHTML = pageItems
    .map((item) => {
      const [statusText, statusClass] = accountStatus(item);
      const [authText] = accountAuthStatus(item);
      const groups = item.group_ids.map((id) => groupMark(id, "pill")).join("");
      const actions = isAdmin()
        ? `<span class="row-actions"><button data-refresh-quota="${item.id}" title="刷新配额"><i data-lucide="gauge"></i></button><button data-auth-account="${item.id}" class="${item.auth_status === "reauth_required" ? "attention" : ""}" title="更新授权"><i data-lucide="key-round"></i></button><button data-toggle-account="${item.id}" title="${item.schedulable ? "暂停调度" : "恢复调度"}"><i data-lucide="${item.schedulable ? "pause" : "play"}"></i></button><button data-edit-account="${item.id}" title="编辑账号"><i data-lucide="square-pen"></i></button><button class="danger" data-delete-account="${item.id}" title="删除账号"><i data-lucide="trash-2"></i></button></span>`
        : '<span class="muted">只读</span>';
      return `<tr><td class="select-column"><input type="checkbox" data-account-select="${item.id}" aria-label="选择 ${escapeHTML(item.name)}" ${state.selectedAccountIDs.has(item.id) ? "checked" : ""} ${isAdmin() ? "" : "disabled"} /></td><td><span class="row-title">${escapeHTML(item.name)}</span><span class="row-subtitle account-meta">${groups}<span class="mono">${escapeHTML(item.proxy_hint || "未绑定代理")}</span></span></td><td><span class="pill ${statusClass}">${statusText}</span><span class="row-subtitle">${escapeHTML(item.auth_error || authText)}</span></td><td><span class="subscription-badge">${escapeHTML(subscriptionName(item.subscription_type))}</span></td><td class="num money-cell">${money(item.account_price)}</td><td class="num money-cell emphasis">${money(item.total_billed_cost)}</td><td>${accountUsageCell(item)}</td><td class="num mono request-count">${Number(item.request_count).toLocaleString("zh-CN")}</td><td class="mono">${dateTime(item.onboarded_at)}</td><td>${survivalCell(item)}</td><td class="mono">${dateTime(item.last_used_at)}</td><td class="actions">${actions}</td></tr>`;
    })
    .join("");
  syncAccountSelection();
  refreshIcons();
}

function selectedAccountIDs() {
  return [...state.selectedAccountIDs];
}
function syncAccountSelection() {
  const search = state.accountSearch.toLowerCase();
  const selectable = state.accounts.filter(
    (item) =>
      (!state.accountGroup || item.group_ids.includes(state.accountGroup)) &&
      (!search ||
        `${item.name} ${item.notes} ${item.credential_hint}`
          .toLowerCase()
          .includes(search)) &&
      (!state.accountStatus || item.dispatch_status === state.accountStatus),
  );
  const selected = selectable.filter((item) =>
    state.selectedAccountIDs.has(item.id),
  ).length;
  const selectAll = $("#select-all-accounts");
  selectAll.disabled = !isAdmin() || selectable.length === 0;
  selectAll.checked = selectable.length > 0 && selected === selectable.length;
  selectAll.indeterminate = selected > 0 && selected < selectable.length;
  $("#selected-account-count").textContent = state.selectedAccountIDs.size;
  $("#delete-selected-accounts").disabled =
    !isAdmin() || state.selectedAccountIDs.size === 0;
}

function renderDeadAccounts() {
  const dead = state.accounts.filter(
    (item) => item.dispatch_status === "error",
  );
  $("#nav-dead-count").textContent = dead.length;
  $("#dead-list-count").textContent = `${dead.length} 个账号`;
  $("#dead-empty").hidden = dead.length > 0;
  const average =
    dead.length > 0
      ? dead.reduce((sum, item) => sum + item.survival_seconds, 0) / dead.length
      : 0;
  $("#dead-metrics").innerHTML = [
    metric("DEAD ACCOUNTS", dead.length, "授权失效或账号错误", "b"),
    metric(
      "TOTAL REQUESTS",
      compact(dead.reduce((sum, item) => sum + item.request_count, 0)),
      "死亡账号历史请求",
    ),
    metric(
      "TOTAL BILLED",
      money(dead.reduce((sum, item) => sum + item.total_billed_cost, 0)),
      "死亡账号历史计费",
    ),
    metric("AVERAGE LIFETIME", durationText(average), "平均存活时间", "a"),
  ].join("");
  $("#dead-accounts-body").innerHTML = paginatedItems("dead", dead)
    .map((item) => {
      const actions = isAdmin()
        ? `<span class="row-actions"><button data-auth-account="${item.id}" class="attention" title="重新授权"><i data-lucide="key-round"></i></button><button data-edit-account="${item.id}" title="编辑账号"><i data-lucide="square-pen"></i></button></span>`
        : '<span class="muted">只读</span>';
      return `<tr><td><span class="row-title">${escapeHTML(item.name)}</span><span class="row-subtitle">${escapeHTML(item.credential_hint)}</span></td><td>${escapeHTML(subscriptionName(item.subscription_type))}</td><td><span class="row-title">${escapeHTML(item.proxy_name || "未绑定")}</span><span class="row-subtitle mono">${escapeHTML(item.proxy_hint || "—")}</span></td><td class="mono">${dateTime(item.onboarded_at)}</td><td class="mono">${dateTime(item.invalidated_at)}</td><td>${survivalCell(item)}</td><td class="num mono">${Number(item.request_count).toLocaleString("zh-CN")}</td><td class="num money-cell">${money(item.total_billed_cost)}</td><td class="error-copy">${escapeHTML(item.auth_error || item.error_message || "账号状态错误")}</td><td class="actions">${actions}</td></tr>`;
    })
    .join("");
  refreshIcons();
}

function renderDaily() {
  $("#daily-body").innerHTML = paginatedItems("daily", state.daily)
    .map(
      (item) =>
        `<tr><td class="mono">${escapeHTML(item.date)}</td><td class="num mono">${Number(item.requests).toLocaleString("zh-CN")}</td><td class="num mono">${compact(item.input_tokens + item.output_tokens)}</td><td class="num money-cell">${money(item.billed_cost)}</td><td class="num money-cell">${money(item.actual_cost)}</td><td class="num">${item.accounts_onboarded}</td><td class="num">${item.accounts_died}</td><td class="num mono">${item.auth_successful} / ${item.authorizations}</td></tr>`,
    )
    .join("");
}

function renderAuthorization() {
  const data = state.authorization;
  if (!data) return;
  $("#authorization-count").textContent = `${data.summary.total} 条记录`;
  $("#authorization-metrics").innerHTML = [
    metric("AUTHORIZATIONS", data.summary.total, "筛选区间授权次数"),
    metric("SUCCESSFUL", data.summary.successful, "授权成功", "a"),
    metric("FAILED", data.summary.failed, "授权失败", "b"),
    metric(
      "SUCCESS RATE",
      `${Number(data.summary.success_rate).toFixed(1)}%`,
      "授权成功率",
    ),
  ].join("");
  $("#authorization-body").innerHTML = paginatedItems(
    "authorization",
    data.items,
  )
    .map(
      (item) =>
        `<tr><td class="mono">${dateTime(item.created_at)}</td><td><span class="row-title">${escapeHTML(item.account_name || "未创建账号")}</span><span class="row-subtitle">${item.account_id ? `#${item.account_id}` : "—"}</span></td><td class="mono">${escapeHTML(item.proxy_ip || "未分配")}</td><td>${escapeHTML(item.method.replaceAll("_", " "))}</td><td>${escapeHTML(subscriptionName(item.subscription_type))}</td><td><span class="pill ${item.success ? "ok" : "error"}">${item.success ? "成功" : "失败"}</span></td><td class="mono">${escapeHTML(item.client_ip || "—")}</td><td class="${item.success ? "" : "error-copy"}">${escapeHTML(item.status_message || "—")}</td></tr>`,
    )
    .join("");
}

function renderProxies() {
  $("#nav-proxy-count").textContent = state.proxies.length;
  $("#proxy-pool-list").innerHTML = state.proxyPools
    .map(
      (pool) =>
        `<article class="pool-row ${String(pool.id) === String(state.proxyPoolFilter) ? "selected" : ""}" data-select-pool="${pool.id}"><div><strong>${escapeHTML(pool.name)}</strong><small>${pool.source_type === "api" ? "API 自动同步" : "手动维护"} · ${pool.available_count}/${pool.proxy_count} 可用</small></div><div class="pool-meter"><span style="width:${pool.proxy_count ? (pool.available_count / pool.proxy_count) * 100 : 0}%"></span></div><div class="pool-meta"><span>${pool.assigned_count} 个账号占用</span><span>${pool.last_sync_at ? dateTime(pool.last_sync_at) : "未同步"}</span></div><div class="row-actions">${isAdmin() && pool.source_type === "api" ? `<button data-sync-pool="${pool.id}" title="同步 API">↻</button>` : ""}${isAdmin() ? `<button data-edit-pool="${pool.id}" title="编辑代理池">✎</button>` : ""}</div></article>`,
    )
    .join("");
  const selected = state.proxyPoolFilter
    ? state.proxies.filter(
        (item) => String(item.pool_id) === String(state.proxyPoolFilter),
      )
    : state.proxies;
  $("#proxies-empty").hidden = selected.length > 0;
  $("#proxies-body").innerHTML = paginatedItems("proxies", selected)
    .map(
      (item) =>
        `<tr><td><span class="row-title">${escapeHTML(item.name)}</span><span class="row-subtitle mono">${escapeHTML(item.host)}:${item.port}</span></td><td><span class="pill">${item.protocol.toUpperCase()}</span></td><td class="mono">${escapeHTML(item.exit_ip || "未检测")}</td><td class="num mono">${item.latency_ms == null ? "—" : `${item.latency_ms} ms`}</td><td>${escapeHTML(item.assigned_to || "未占用")}</td><td><span class="pill ${item.status === "active" ? "ok" : item.status === "error" ? "error" : "off"}">${item.status === "active" ? "正常" : item.status === "error" ? "异常" : "停用"}</span></td><td class="mono">${dateTime(item.last_test_at)}</td><td class="actions">${isAdmin() ? `<span class="row-actions"><button data-test-proxy="${item.id}" title="检测代理"><i data-lucide="activity"></i></button><button class="danger" data-delete-proxy="${item.id}" title="删除代理"><i data-lucide="trash-2"></i></button></span>` : '<span class="muted">只读</span>'}</td></tr>`,
    )
    .join("");
  const options = `<option value="">全部代理池</option>${state.proxyPools.map((pool) => `<option value="${pool.id}">${escapeHTML(pool.name)}</option>`).join("")}`;
  $("#proxy-pool-filter").innerHTML = options;
  $("#proxy-pool-filter").value = state.proxyPoolFilter;
  refreshIcons();
}

function renderAccess() {
  const admin = isAdmin();
  const canManageKeys = admin || state.me.role === "user";
  $("#user-toolbar").hidden = !admin;
  $("#users-panel").hidden = !admin;
  if (admin)
    $("#users-body").innerHTML = paginatedItems("users", state.users)
      .map(
        (item) =>
          `<tr><td><span class="row-title">${escapeHTML(item.name || item.username)}</span><span class="row-subtitle mono">${escapeHTML(item.username)}</span></td><td><span class="pill">${roleName(item.role)}</span></td><td><div class="group-pills">${item.allowed_group_ids.map((id) => groupMark(id, "pill")).join("")}</div></td><td class="num mono">${item.rpm_limit || "∞"}</td><td><span class="pill ${item.status === "active" ? "ok" : "off"}">${item.status === "active" ? "启用" : "停用"}</span></td><td class="mono">${dateTime(item.created_at)}</td><td class="actions"><span class="row-actions"><button data-edit-user="${item.id}" title="编辑用户"><i data-lucide="square-pen"></i></button>${item.id === state.me.id ? "" : `<button class="danger" data-delete-user="${item.id}" title="删除用户"><i data-lucide="trash-2"></i></button>`}</span></td></tr>`,
      )
      .join("");
  $("#keys-empty").hidden = state.keys.length > 0;
  $("#keys-body").innerHTML = paginatedItems("keys", state.keys)
    .map(
      (item) =>
        `<tr><td><span class="row-title">${escapeHTML(item.name)}</span><span class="row-subtitle mono">${escapeHTML(item.key_prefix)}••••••••</span></td><td>${escapeHTML(item.username)}</td><td>${groupMark(item.group_id, "pill")}</td><td class="num mono">${money(item.quota_used)} / ${item.quota > 0 ? money(item.quota) : "∞"}</td><td class="mono">${dateTime(item.expires_at)}</td><td class="mono">${dateTime(item.last_used_at)}</td><td><span class="pill ${item.status === "active" ? "ok" : "off"}">${item.status === "active" ? "启用" : "停用"}</span></td><td class="actions">${canManageKeys ? `<span class="row-actions"><button data-toggle-key="${item.id}" title="${item.status === "active" ? "禁用 API Key" : "启用 API Key"}"><i data-lucide="power"></i></button><button data-edit-key="${item.id}" title="编辑 API Key"><i data-lucide="square-pen"></i></button><button class="danger" data-delete-key="${item.id}" title="删除 API Key"><i data-lucide="trash-2"></i></button></span>` : '<span class="muted">只读</span>'}</td></tr>`,
    )
    .join("");
  $("#gateway-endpoint").textContent = `${location.origin}/v1/messages`;
  refreshIcons();
}

function renderPriceTable() {
  $("#price-count").textContent = `${state.prices.length} 个模型`;
  $("#prices-body").innerHTML = paginatedItems("prices", state.prices)
    .map(
      (item) =>
        `<tr><td><span class="row-title mono">${escapeHTML(item.model)}</span>${item.model === "*" ? '<span class="row-subtitle">默认回退价格</span>' : ""}</td><td><span class="pill ${item.source === "remote" ? "ok" : ""}">${item.source === "remote" ? "自动同步" : "手动覆盖"}</span></td><td class="num mono">${money(item.input_per_million)}</td><td class="num mono">${money(item.output_per_million)}</td><td class="num mono">${money(item.cache_creation_per_million)}</td><td class="num mono">${money(item.cache_read_per_million)}</td><td class="mono">${dateTime(item.updated_at)}</td><td class="actions">${isAdmin() ? `<span class="row-actions"><button data-edit-price="${item.id}" title="编辑价格"><i data-lucide="square-pen"></i></button>${item.model === "*" ? "" : `<button class="danger" data-delete-price="${item.id}" title="删除价格"><i data-lucide="trash-2"></i></button>`}</span>` : ""}</td></tr>`,
    )
    .join("");
  refreshIcons();
}
function renderPriceSync() {
  const item = state.pricingSync;
  if (!item) return;
  const labels = {
    current: "已同步",
    syncing: "同步中",
    error: "同步异常",
    idle: "等待同步",
  };
  $("#price-sync-status").textContent = labels[item.status] || item.status;
  $("#price-sync-status").className =
    `pill ${item.status === "current" ? "ok" : item.status === "error" ? "error" : "off"}`;
  $("#price-sync-detail").textContent =
    item.last_error ||
    `${item.model_count} 个模型 · 最近同步 ${dateTime(item.last_synced_at)} · 每 10 分钟自动检查`;
}
function renderAudit() {
  const total = state.serverPagination.audit?.total ?? state.audits.length;
  $("#audit-count").textContent = `${total} 条记录`;
  $("#audit-empty").hidden = state.audits.length > 0;
  const names = {
    "auth.login": "登录",
    "auth.logout": "退出",
    "account.create": "新增账号",
    "account.update": "编辑账号",
    "account.delete": "删除账号",
    "account.reauthorize": "更新授权",
    "account.quota_refresh": "刷新配额",
    "api_key.status": "API Key 状态",
  };
  $("#audit-body").innerHTML = paginatedItems("audit", state.audits)
    .map(
      (item) =>
        `<tr><td class="mono">${dateTime(item.created_at)}</td><td><span class="row-title">${escapeHTML(item.actor_username || "系统")}</span><span class="row-subtitle">${roleName(item.actor_role)}</span></td><td><span class="row-title">${escapeHTML(names[item.action] || item.action)}</span><span class="row-subtitle mono">${escapeHTML(item.method)} ${escapeHTML(item.path)}</span></td><td><span class="pill">${escapeHTML(item.target_type)}</span> <span class="mono">${escapeHTML(item.target_id || "—")}</span></td><td><span class="pill ${item.status_code < 400 ? "ok" : "error"}">HTTP ${item.status_code}</span></td><td class="mono">${escapeHTML(item.client_ip || "—")}</td><td class="num mono">${item.duration_ms} ms</td></tr>`,
    )
    .join("");
}
function renderBilling() {
  const data = state.billing;
  if (!data) return;
  $("#billing-metrics").innerHTML = [
    metric(
      "BILLED",
      money(data.totals.billed_cost),
      `${data.totals.requests} 次请求`,
      "a",
    ),
    metric("ACTUAL COST", money(data.totals.actual_cost), "账号成本", "b"),
    metric(
      "MARGIN",
      money(data.totals.margin),
      data.totals.billed_cost
        ? `${((data.totals.margin / data.totals.billed_cost) * 100).toFixed(1)}%`
        : "0%",
    ),
    metric(
      "TOKENS",
      compact(
        data.totals.input_tokens +
          data.totals.output_tokens +
          data.totals.cache_tokens,
      ),
      `输入 ${compact(data.totals.input_tokens)} / 输出 ${compact(data.totals.output_tokens)}`,
    ),
  ].join("");
  renderBreakdown();
  $("#usage-body").innerHTML = usageRows(
    paginatedItems("usage", state.usage),
    false,
  );
  $("#usage-empty").hidden = state.usage.length > 0;
}
function renderBreakdown() {
  if (!state.billing) return;
  const map = {
    group: state.billing.by_group,
    account: state.billing.by_account,
    purpose: state.billing.by_purpose,
  };
  const rows = map[state.breakdown] || [];
  const maxValue = Math.max(...rows.map((item) => item.billed_cost), 0);
  $("#breakdown-list").innerHTML = rows.length
    ? rows
        .map(
          (item) =>
            `<div class="breakdown-row"><div><strong>${escapeHTML(state.breakdown === "group" ? `${item.key.toUpperCase()} 分组` : item.name)}</strong><small>${item.requests} 次请求</small></div><div class="breakdown-bar"><span style="width:${maxValue ? Math.max((item.billed_cost / maxValue) * 100, 2) : 0}%"></span></div><div class="breakdown-values"><strong>${money(item.billed_cost)}</strong><small>成本 ${money(item.actual_cost)}</small></div></div>`,
        )
        .join("")
    : '<div class="empty-state"><strong>暂无拆分数据</strong></div>';
}
function usageRows(items, compactMode) {
  if (!items?.length)
    return compactMode
      ? '<tr><td colspan="7"><div class="empty-state"><strong>暂无计费流水</strong></div></td></tr>'
      : "";
  return items
    .map((item) => {
      const total =
        item.input_tokens +
        item.output_tokens +
        item.cache_creation_tokens +
        item.cache_read_tokens;
      if (compactMode)
        return `<tr><td class="mono">${dateTime(item.created_at)}</td><td><span class="row-title">${escapeHTML(item.purpose_name)}</span>${groupMark(item.group_id, "pill")}</td><td>${escapeHTML(item.account_name)}</td><td class="mono">${escapeHTML(item.model)}</td><td class="num mono">${compact(total)}</td><td class="num mono">${money(item.billed_cost)}</td><td class="num mono">${money(item.actual_cost)}</td></tr>`;
      return `<tr><td><span class="mono">${escapeHTML(item.request_id)}</span><span class="row-subtitle">${dateTime(item.created_at)}</span></td><td><span class="row-title">${escapeHTML(item.purpose_name)}</span>${groupMark(item.group_id, "pill")}</td><td>${escapeHTML(item.account_name)}</td><td class="mono">${escapeHTML(item.model)}</td><td class="num mono">${compact(item.input_tokens)}</td><td class="num mono">${compact(item.output_tokens)}</td><td class="num mono">${compact(item.cache_creation_tokens + item.cache_read_tokens)}</td><td class="num mono">${money(item.billed_cost)}</td><td class="num mono">${money(item.actual_cost)}</td><td class="num mono">${money(item.billed_cost - item.actual_cost)}</td></tr>`;
    })
    .join("");
}

function renderBatchResults() {
  $("#batch-result-body").innerHTML = paginatedItems(
    "batch",
    state.batchResults,
  )
    .map(
      (item) =>
        `<tr><td class="mono">${item.index}</td><td><span class="row-title">${escapeHTML(item.name || "未创建")}</span>${item.account_id ? `<span class="row-subtitle mono">#${item.account_id}</span>` : ""}</td><td>${escapeHTML(subscriptionName(item.subscription_type))}</td><td class="mono">${escapeHTML(item.proxy_ip || "—")}</td><td><span class="pill ${item.success ? "ok" : "error"}">${item.success ? "成功" : "失败"}</span></td><td class="${item.success ? "" : "error-copy"}">${escapeHTML(item.error || "授权完成")}</td></tr>`,
    )
    .join("");
  refreshIcons();
}

function populateSelects() {
  const purposes = state.dashboard?.purposes || state.purposes || [];
  const purposeOptions =
    purposes
      .map(
        (item) =>
          `<option value="${escapeHTML(item.key)}">${escapeHTML(item.name)}</option>`,
      )
      .join("") || "";
  $("#billing-purpose").innerHTML =
    `<option value="">全部用途</option>${purposeOptions}`;
  $("#usage-purpose").innerHTML = purposeOptions;
  $("#usage-account").innerHTML =
    `<option value="">按用途自动选择</option>${state.accounts.map((item) => `<option value="${item.id}">${escapeHTML(item.name)}</option>`).join("")}`;
  const poolOptions = state.proxyPools
    .map(
      (pool) => `<option value="${pool.id}">${escapeHTML(pool.name)}</option>`,
    )
    .join("");
  $("#account-proxy-pool").innerHTML =
    `<option value="">未绑定（不可授权/调度）</option>${poolOptions}`;
  $("#batch-proxy-pool").innerHTML =
    `<option value="">选择代理池</option>${poolOptions}`;
  $("#proxy-import-pool").innerHTML = poolOptions;
  $("#key-user").innerHTML = state.users
    .map(
      (user) =>
        `<option value="${user.id}">${escapeHTML(user.name || user.username)} · ${roleName(user.role)}</option>`,
    )
    .join("");
}

function setView(view) {
  state.view = view;
  $$(".view").forEach((node) =>
    node.classList.toggle("active", node.id === `view-${view}`),
  );
  $$(".nav-item").forEach((node) =>
    node.classList.toggle("active", node.dataset.view === view),
  );
  const [eyebrow, title, action] = viewMeta[view];
  $("#page-eyebrow").textContent = eyebrow;
  $("#page-title").textContent = title;
  $("#primary-action-label").textContent = action;
  const canAct =
    Boolean(action) &&
    (isAdmin() || (state.me?.role === "user" && view === "access"));
  $("#primary-action").hidden = !canAct;
  if (view === "billing") loadBilling();
  if (view === "audit") loadAudit();
  if (view === "accounts") loadAccountSummary();
  if (view === "daily") loadDaily();
  if (view === "authorization") loadAuthorization();
  refreshIcons();
  window.scrollTo({ top: 0, behavior: "smooth" });
}

function fillProxyOptions(poolID, selectedID = "") {
  const list = state.proxies.filter(
    (item) =>
      String(item.pool_id) === String(poolID) && item.status === "active",
  );
  $("#account-proxy").innerHTML =
    `<option value="">未指定</option>${list.map((item) => `<option value="${item.id}">${escapeHTML(item.name)} · ${escapeHTML(item.exit_ip || `${item.host}:${item.port}`)}${item.assigned_to ? ` · ${escapeHTML(item.assigned_to)}` : ""}</option>`).join("")}`;
  $("#account-proxy").value = selectedID || "";
}
function openAccount(account = null) {
  $("#account-form").reset();
  $("#account-id").value = account?.id || "";
  $("#account-dialog-title").textContent = account ? "编辑账号" : "新增账号";
  $("#account-name").value = account?.name || "";
  $("#account-auth-type").value = account?.auth_type || "oauth";
  $("#account-session-key").value = "";
  $("#account-status").value = account?.status || "active";
  $("#account-schedulable").checked = account?.schedulable ?? false;
  $("#account-concurrency").value = account?.concurrency ?? 3;
  $("#account-priority").value = account?.priority ?? 50;
  $("#account-rate").value = account?.rate_multiplier ?? 1;
  $("#account-price").value = account?.account_price ?? 0;
  $("#account-proxy-pool").value = account?.proxy_pool_id || "";
  $("#account-auto-proxy").checked = account?.auto_proxy || false;
  fillProxyOptions(account?.proxy_pool_id || "", account?.proxy_id || "");
  $("#account-base-rpm").value = account?.base_rpm || 15;
  $("#account-rpm-enabled").checked = (account?.base_rpm || 0) > 0;
  $("#account-rpm-strategy").value = account?.rpm_strategy || "tiered";
  $("#account-rpm-buffer").value = account?.rpm_sticky_buffer || 0;
  $("#account-queue-mode").value = account?.user_msg_queue_mode || "off";
  $("#account-request-passthrough").checked =
    account?.extra?.request_passthrough === true;
  $("#account-expires").value = dateTimeInput(account?.expires_at);
  $("#account-rate-reset").value = dateTimeInput(account?.rate_limit_reset_at);
  $("#account-credentials").value = "";
  $("#account-extra").value = account
    ? JSON.stringify(account.extra || {}, null, 2)
    : "{}";
  $("#account-error").value = account?.error_message || "";
  $("#account-notes").value = account?.notes || "";
  $$('input[name="account-group"]').forEach((node) => {
    node.checked = account
      ? account.group_ids.includes(node.value)
      : node.value === "a";
  });
  $("#credential-help").textContent = account?.has_credentials
    ? `当前凭证：${account.credential_hint}；留空保持不变。`
    : "保存后凭证不会在管理列表中返回。";
  syncAccountControls();
  showInitializedDialog("#account-dialog");
}
function syncAccountControls() {
  const hasPool = Boolean($("#account-proxy-pool").value);
  const schedulable = $("#account-schedulable");
  schedulable.disabled = !hasPool;
  if (!hasPool) schedulable.checked = false;
  $("#account-auto-proxy").disabled = !hasPool;
  $("#account-proxy").disabled = !hasPool || $("#account-auto-proxy").checked;
  $("#account-base-rpm").disabled = !$("#account-rpm-enabled").checked;
  $("#account-rpm-strategy").disabled = !$("#account-rpm-enabled").checked;
  $("#account-rpm-buffer").disabled = !$("#account-rpm-enabled").checked;
  syncAccountAuthFields();
  stabilizeAccountDialogViewport();
}
function stabilizeAccountDialogViewport() {
  const dialog = $("#account-dialog");
  if (!dialog.open) return;
  dialog.scrollTop = 0;
  requestAnimationFrame(() => {
    dialog.scrollTop = 0;
  });
}
function syncAccountAuthFields() {
  const creating = !$("#account-id").value;
  const usesSessionKey =
    creating && $("#account-auth-type").value !== "api_key";
  $("#account-session-key-field").hidden = !usesSessionKey;
  $("#account-session-key").required = false;
  $("#account-credentials-field").hidden = usesSessionKey;
  $("#account-credentials").required =
    creating && $("#account-auth-type").value === "api_key";
  const label = $("#account-submit span");
  label.textContent = creating
    ? usesSessionKey
      ? $("#account-session-key").value.trim()
        ? "验证 SK 并创建账号"
        : "创建待授权账号"
      : "创建账号"
    : "保存账号";
}
function accountPayload(account, overrides = {}) {
  return {
    name: account.name,
    platform: account.platform,
    auth_type: account.auth_type,
    extra: account.extra || {},
    status: account.status,
    schedulable: account.schedulable,
    concurrency: account.concurrency,
    priority: account.priority,
    rate_multiplier: account.rate_multiplier,
    account_price: account.account_price,
    notes: account.notes || "",
    error_message: account.error_message || "",
    expires_at: account.expires_at || "",
    rate_limit_reset_at: account.rate_limit_reset_at || "",
    group_ids: account.group_ids,
    proxy_pool_id: account.proxy_pool_id,
    proxy_id: account.proxy_id,
    auto_proxy: account.auto_proxy,
    base_rpm: account.base_rpm,
    rpm_strategy: account.rpm_strategy,
    rpm_sticky_buffer: account.rpm_sticky_buffer,
    user_msg_queue_mode: account.user_msg_queue_mode,
    ...overrides,
  };
}
function openPurpose(item = null) {
  $("#purpose-form").reset();
  $("#purpose-id").value = item?.id || "";
  $("#purpose-dialog-title").textContent = item ? "编辑用途" : "新增用途";
  $("#purpose-key").value = item?.key || "";
  $("#purpose-name").value = item?.name || "";
  $("#purpose-group").value = item?.active_group_id || "a";
  $("#purpose-description").value = item?.description || "";
  showInitializedDialog("#purpose-dialog");
}
function openGroup(item) {
  $("#group-form").reset();
  $("#group-id").value = item.id;
  $("#group-dialog-title").textContent = `编辑 ${item.id.toUpperCase()} 分组`;
  $("#group-name").value = item.name;
  $("#group-description").value = item.description || "";
  $("#group-rate").value = item.rate_multiplier;
  $("#group-status").value = item.status;
  $("#group-daily").value = item.daily_limit_usd ?? "";
  $("#group-monthly").value = item.monthly_limit_usd ?? "";
  showInitializedDialog("#group-dialog");
}
function openPool(item = null) {
  $("#proxy-pool-form").reset();
  $("#proxy-pool-id").value = item?.id || "";
  $("#proxy-pool-title").textContent = item ? "编辑代理池" : "新增代理池";
  $("#proxy-pool-name").value = item?.name || "";
  $("#proxy-pool-source").value = item?.source_type || "manual";
  $("#proxy-pool-protocol").value = item?.default_protocol || "socks5";
  $("#proxy-pool-status").value = item?.status || "active";
  $("#proxy-pool-api-url").value = item?.api_url || "";
  $("#proxy-pool-headers").value = item?.api_headers || "{}";
  toggleAPISource();
  showInitializedDialog("#proxy-pool-dialog");
}
function toggleAPISource() {
  $$(".api-source-field").forEach((node) => {
    node.hidden = $("#proxy-pool-source").value !== "api";
  });
}
function openUser(item = null) {
  $("#user-form").reset();
  $("#user-id").value = item?.id || "";
  $("#user-title").textContent = item ? "编辑用户" : "新增用户";
  $("#user-username").value = item?.username || "";
  $("#user-name").value = item?.name || "";
  $("#user-role").value = item?.role || "user";
  $("#user-status").value = item?.status || "active";
  $("#user-rpm").value = item?.rpm_limit || 0;
  $("#user-password").required = !item;
  $("#user-password-help").textContent = item
    ? "留空保持原密码；修改后旧会话失效"
    : "至少 8 位";
  $$('input[name="user-group"]').forEach((node) => {
    node.checked = item
      ? item.allowed_group_ids.includes(node.value)
      : node.value === "a";
  });
  const visiblePages =
    item?.visible_pages || rolePageDefaults[$("#user-role").value] || [];
  $$('input[name="user-page"]').forEach((node) => {
    node.checked = visiblePages.includes(node.value);
  });
  syncUserRole(false);
  showInitializedDialog("#user-dialog");
}
function syncUserRole(resetPages = false) {
  const manager = $("#user-role").value !== "user";
  $$('input[name="user-group"]').forEach((node) => {
    if (manager) node.checked = true;
    node.disabled = manager;
  });
  if (resetPages) {
    const defaults = rolePageDefaults[$("#user-role").value] || [];
    $$('input[name="user-page"]').forEach((node) => {
      node.checked = defaults.includes(node.value);
    });
  }
}
function syncKeyGroups(selected = "") {
  const owner =
    state.me.role === "user"
      ? state.me
      : state.users.find(
          (user) => String(user.id) === String($("#key-user").value),
        );
  const groups = owner?.role === "user" ? owner.allowed_group_ids : ["a", "b"];
  $("#key-group").innerHTML = groups
    .map((id) => `<option value="${id}">${id.toUpperCase()} 分组</option>`)
    .join("");
  $("#key-group").value = groups.includes(selected) ? selected : groups[0];
}
function openKey(item = null) {
  $("#key-form").reset();
  $("#key-id").value = item?.id || "";
  $("#key-name").value = item?.name || "";
  $("#key-quota").value = item?.quota || 0;
  $("#key-expires").value = dateTimeInput(item?.expires_at);
  $("#key-status").value = item?.status || "active";
  $("#key-user-field").hidden = state.me.role === "user";
  $("#key-status-field").hidden = !item;
  if (item) $("#key-user").value = item.user_id;
  else if (state.users.length)
    $("#key-user").value =
      state.users.find((user) => user.role === "user")?.id || state.users[0].id;
  syncKeyGroups(item?.group_id || state.me.allowed_group_ids?.[0] || "a");
  showInitializedDialog("#key-dialog");
}
function openPrice(item = null) {
  $("#price-form").reset();
  $("#price-model").value = item?.model || "";
  $("#price-model").readOnly = item?.model === "*";
  $("#price-input").value = item?.input_per_million ?? 0;
  $("#price-output").value = item?.output_per_million ?? 0;
  $("#price-cache-create").value = item?.cache_creation_per_million ?? 0;
  $("#price-cache-read").value = item?.cache_read_per_million ?? 0;
  showInitializedDialog("#price-dialog");
}
function openUsage() {
  $("#usage-form").reset();
  populateSelects();
  showInitializedDialog("#usage-dialog");
}
function openProxyImport() {
  $("#proxy-import-form").reset();
  showInitializedDialog("#proxy-import-dialog");
}
function openAccountAuth(account) {
  state.oauthSessionID = "";
  $("#auth-account-id").value = account.id;
  $("#account-auth-title").textContent = `更新授权 · ${account.name}`;
  $("#auth-session-key").value = "";
  $("#oauth-code").value = "";
  $("#oauth-exchange-fields").hidden = true;
  setChoiceValue(
    "#auth-mode",
    account.auth_type === "setup_token" ? "setup_token" : "oauth",
  );
  showInitializedDialog("#account-auth-dialog");
  refreshIcons();
}

document.addEventListener("click", async (event) => {
  const target = event.target.closest("button, [data-select-pool]");
  if (!target) return;
  if (target.dataset.paginationKey) {
    const key = target.dataset.paginationKey;
    state.paginationPages[key] =
      Number(state.paginationPages[key] || 1) + Number(target.dataset.pageStep);
    if (state.serverPagination[key]) await loadServerPage(key);
    else paginationTables[key].render();
    return;
  }
  if (target.hasAttribute("data-close-dialog")) {
    target.closest("dialog")?.close();
    return;
  }
  if (target.dataset.view) setView(target.dataset.view);
  if (target.dataset.viewJump) setView(target.dataset.viewJump);
  if (target.hasAttribute("data-open-purpose")) openPurpose();
  if (target.dataset.editAccount)
    openAccount(
      state.accounts.find(
        (item) => item.id === Number(target.dataset.editAccount),
      ),
    );
  if (target.dataset.authAccount)
    openAccountAuth(
      state.accounts.find(
        (item) => item.id === Number(target.dataset.authAccount),
      ),
    );
  if (target.dataset.editPurpose)
    openPurpose(
      state.dashboard.purposes.find(
        (item) => item.id === Number(target.dataset.editPurpose),
      ),
    );
  if (target.dataset.editGroup)
    openGroup(
      state.dashboard.groups.find(
        (item) => item.id === target.dataset.editGroup,
      ),
    );
  if (target.dataset.editPool)
    openPool(
      state.proxyPools.find(
        (item) => item.id === Number(target.dataset.editPool),
      ),
    );
  if (target.dataset.editUser)
    openUser(
      state.users.find((item) => item.id === Number(target.dataset.editUser)),
    );
  if (target.dataset.editKey)
    openKey(
      state.keys.find((item) => item.id === Number(target.dataset.editKey)),
    );
  if (target.dataset.editPrice)
    openPrice(
      state.prices.find((item) => item.id === Number(target.dataset.editPrice)),
    );
  if (target.dataset.selectPool) {
    state.proxyPoolFilter = target.dataset.selectPool;
    resetPagination("proxies");
    renderProxies();
  }
  if (target.dataset.breakdown) {
    state.breakdown = target.dataset.breakdown;
    $$("#breakdown-tabs button").forEach((node) =>
      node.classList.toggle(
        "active",
        node.dataset.breakdown === state.breakdown,
      ),
    );
    renderBreakdown();
  }
  if (
    target.dataset.group !== undefined &&
    target.closest("#account-group-filter")
  ) {
    state.accountGroup = target.dataset.group;
    state.selectedAccountIDs.clear();
    resetPagination("accounts");
    $$("#account-group-filter button").forEach((node) =>
      node.classList.toggle("active", node === target),
    );
    renderAccounts();
    loadAccountSummary();
  }
  try {
    if (target.dataset.purposeSwitch) {
      const item = state.dashboard.purposes.find(
        (value) => value.id === Number(target.dataset.purposeSwitch),
      );
      await api(`/api/purposes/${item.id}`, {
        method: "PUT",
        body: JSON.stringify({
          key: item.key,
          name: item.name,
          description: item.description,
          active_group_id: target.dataset.group,
        }),
      });
      toast(`${item.name} 已切换到 ${target.dataset.group.toUpperCase()} 分组`);
      await loadCore();
    }
    if (target.dataset.toggleAccount) {
      const item = state.accounts.find(
        (value) => value.id === Number(target.dataset.toggleAccount),
      );
      await api(`/api/accounts/${item.id}`, {
        method: "PUT",
        body: JSON.stringify(
          accountPayload(item, { schedulable: !item.schedulable }),
        ),
      });
      toast(item.schedulable ? "账号已暂停调度" : "账号已恢复调度");
      await loadCore();
    }
    if (target.dataset.refreshQuota) {
      await api(`/api/accounts/${target.dataset.refreshQuota}/quota/refresh`, {
        method: "POST",
        body: "{}",
      });
      toast("账号配额已刷新");
      await loadCore();
    }
    if (target.dataset.toggleKey) {
      const item = state.keys.find(
        (value) => value.id === Number(target.dataset.toggleKey),
      );
      const status = item.status === "active" ? "disabled" : "active";
      await api(`/api/api-keys/${item.id}/status`, {
        method: "PATCH",
        body: JSON.stringify({ status }),
      });
      toast(status === "active" ? "API Key 已启用" : "API Key 已禁用");
      await loadCore();
    }
    if (target.id === "sync-prices") {
      target.disabled = true;
      try {
        state.pricingSync = await api("/api/prices/sync", {
          method: "POST",
          body: "{}",
        });
        toast("模型价格同步完成");
        await loadCore();
      } finally {
        target.disabled = false;
      }
    }
    if (target.dataset.syncPool) {
      const result = await api(
        `/api/proxy-pools/${target.dataset.syncPool}/sync`,
        { method: "POST" },
      );
      toast(`同步完成：新增 ${result.created}，跳过 ${result.skipped}`);
      await loadCore();
    }
    if (target.dataset.testProxy) {
      const result = await api(
        `/api/proxies/${target.dataset.testProxy}/test`,
        { method: "POST" },
      );
      toast(`出口 ${result.ip}，延迟 ${result.latency_ms} ms`);
      await loadCore();
    }
    if (target.dataset.deleteProxy) {
      const item = state.proxies.find(
        (value) => value.id === Number(target.dataset.deleteProxy),
      );
      const impact = item.assigned_to
        ? `\n\n当前由 ${item.assigned_to} 使用。自动匹配账号会尝试切换到同池其他 IP；无法切换或手动绑定的账号将暂停调度。`
        : "";
      if (confirm(`确认删除代理“${item.name}”？${impact}`)) {
        const result = await api(`/api/proxies/${item.id}`, {
          method: "DELETE",
        });
        const details = [];
        if (result.reassigned_accounts)
          details.push(`${result.reassigned_accounts} 个账号已切换 IP`);
        if (result.paused_accounts)
          details.push(`${result.paused_accounts} 个账号已暂停`);
        toast(details.length ? `代理已删除，${details.join("，")}` : "代理已删除");
        await loadCore();
      }
      return;
    }
    for (const [dataset, path, collection, label] of [
      ["deleteAccount", "/api/accounts/", state.accounts, "账号"],
      ["deleteUser", "/api/users/", state.users, "用户"],
      ["deleteKey", "/api/api-keys/", state.keys, "SK"],
      ["deletePrice", "/api/prices/", state.prices, "价格"],
    ])
      if (target.dataset[dataset]) {
        const item = collection.find(
          (value) => value.id === Number(target.dataset[dataset]),
        );
        if (confirm(`确认删除${label}“${item.name || item.model}”？`)) {
          await api(path + item.id, { method: "DELETE" });
          toast(`${label}已删除`);
          await loadCore();
          if (state.view === "billing") await loadBilling();
        }
      }
  } catch (error) {
    toast(error.message, "error");
  }
});

document.addEventListener("change", async (event) => {
  const select = event.target.closest("select[data-pagination-size]");
  if (!select) return;
  const key = select.dataset.paginationSize;
  state.paginationSizes[key] = Number(select.value);
  resetPagination(key);
  if (state.serverPagination[key]) await loadServerPage(key);
  else paginationTables[key].render();
});

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#login-error").textContent = "";
  try {
    state.me = await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({
        username: $("#login-username").value,
        password: $("#login-password").value,
      }),
    });
    showApp();
    configureRole();
    initializeDates();
    await loadCore();
  } catch (error) {
    $("#login-error").textContent = error.message;
  }
});
$("#logout-button").addEventListener("click", async () => {
  try {
    await api("/api/auth/logout", { method: "POST" });
  } finally {
    state.me = null;
    state.selectedAccountIDs.clear();
    showLogin();
  }
});
$("#sidebar-toggle").addEventListener("click", () => {
  const collapsed = !$("#app-shell").classList.contains("sidebar-collapsed");
  setSidebarCollapsed(collapsed);
  try {
    localStorage.setItem(sidebarStorageKey, String(collapsed));
  } catch {
    // The drawer still works when persistence is unavailable.
  }
});
$("#primary-action").addEventListener("click", () => {
  if (state.view === "overview") openPurpose();
  if (state.view === "accounts") openAccount();
  if (state.view === "proxies") openProxyImport();
  if (state.view === "access") openKey();
  if (state.view === "billing") openUsage();
});
$("#refresh-button").addEventListener("click", async () => {
  await loadCore();
  if (state.view === "billing") await loadBilling();
  if (state.view === "audit") await loadAudit();
  if (state.view === "daily") await loadDaily();
  if (state.view === "authorization") await loadAuthorization();
  toast("数据已刷新");
});
$("#refresh-accounts").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  try {
    button.disabled = true;
    await loadAccounts();
    toast("账号列表已刷新");
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
  }
});
$("#delete-selected-accounts").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  const ids = selectedAccountIDs();
  if (!ids.length) return;
  const selectedNames = ids
    .map((id) => state.accounts.find((item) => item.id === id)?.name)
    .filter(Boolean);
  const preview = selectedNames.slice(0, 3).join("、");
  const remainder = selectedNames.length > 3 ? ` 等 ${ids.length} 个账号` : "";
  const confirmed = await confirmAction(
    `删除 ${ids.length} 个账号`,
    `将删除 ${preview}${remainder}，并立即停止这些账号的调度。历史计费和操作日志会继续保留。`,
    `确认删除 ${ids.length} 个`,
  );
  if (!confirmed) return;
  try {
    button.disabled = true;
    const result = await api("/api/accounts/batch-delete", {
      method: "POST",
      body: JSON.stringify({ ids }),
    });
    toast(`已删除 ${result.deleted} 个账号`);
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    syncAccountSelection();
  }
});
$("#add-proxy-pool").addEventListener("click", () => openPool());
$("#import-proxies").addEventListener("click", openProxyImport);
$("#add-user").addEventListener("click", () => openUser());
$("#add-api-key").addEventListener("click", () => openKey());
$("#add-price").addEventListener("click", () => openPrice());
$("#record-usage").addEventListener("click", openUsage);
$("#apply-billing-filters").addEventListener("click", () => {
  resetPagination("usage");
  loadBilling();
});
$("#apply-audit-filters").addEventListener("click", () => {
  resetPagination("audit");
  loadAudit();
});
$("#apply-authorization-filters").addEventListener(
  "click",
  () => {
    resetPagination("authorization");
    loadAuthorization();
  },
);
$("#daily-days").addEventListener("change", () => {
  resetPagination("daily");
  loadDaily();
});
let accountSearchTimer;
$("#account-search").addEventListener("input", (event) => {
  state.accountSearch = event.target.value;
  state.selectedAccountIDs.clear();
  resetPagination("accounts");
  renderAccounts();
  clearTimeout(accountSearchTimer);
  accountSearchTimer = setTimeout(loadAccountSummary, 250);
});
$("#account-status-tabs").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-account-status]");
  if (!button) return;
  state.accountStatus = button.dataset.accountStatus;
  state.selectedAccountIDs.clear();
  resetPagination("accounts");
  renderAccounts();
  loadAccountSummary();
});
$("#select-all-accounts").addEventListener("change", (event) => {
  const search = state.accountSearch.toLowerCase();
  state.accounts
    .filter(
      (item) =>
        (!state.accountGroup || item.group_ids.includes(state.accountGroup)) &&
        (!search ||
          `${item.name} ${item.notes} ${item.credential_hint}`
            .toLowerCase()
            .includes(search)) &&
        (!state.accountStatus || item.dispatch_status === state.accountStatus),
    )
    .forEach((item) =>
      event.target.checked
        ? state.selectedAccountIDs.add(item.id)
        : state.selectedAccountIDs.delete(item.id),
    );
  $$('#accounts-body input[data-account-select]').forEach(
    (node) => (node.checked = event.target.checked),
  );
  syncAccountSelection();
});
$("#accounts-body").addEventListener("change", (event) => {
  if (event.target.matches("input[data-account-select]")) {
    const id = Number(event.target.dataset.accountSelect);
    if (event.target.checked) state.selectedAccountIDs.add(id);
    else state.selectedAccountIDs.delete(id);
    syncAccountSelection();
  }
});
$("#account-from").addEventListener("change", loadAccountSummary);
$("#account-to").addEventListener("change", loadAccountSummary);
$("#proxy-pool-filter").addEventListener("change", (event) => {
  state.proxyPoolFilter = event.target.value;
  resetPagination("proxies");
  renderProxies();
});
$("#account-proxy-pool").addEventListener("change", (event) => {
  fillProxyOptions(event.target.value);
  if (!event.target.value) $("#account-auto-proxy").checked = false;
  syncAccountControls();
});
$("#account-auto-proxy").addEventListener("change", syncAccountControls);
$("#account-rpm-enabled").addEventListener("change", syncAccountControls);
$("#account-auth-type").addEventListener("change", syncAccountAuthFields);
$("#account-session-key").addEventListener("input", syncAccountAuthFields);
$("#proxy-pool-source").addEventListener("change", toggleAPISource);
$("#user-role").addEventListener("change", () => syncUserRole(true));
$("#key-user").addEventListener("change", () => syncKeyGroups());

$("#session-auth-submit").addEventListener("click", async () => {
  const button = $("#session-auth-submit");
  try {
    const accountID = $("#auth-account-id").value;
    const sessionKey = $("#auth-session-key").value.trim();
    if (!sessionKey) throw new Error("请输入 Claude Session Key");
    button.disabled = true;
    await api(`/api/accounts/${accountID}/session-auth`, {
      method: "POST",
      body: JSON.stringify({
        session_key: sessionKey,
        mode: $("#auth-mode").value,
      }),
    });
    $("#account-auth-dialog").close();
    toast("账号授权已更新");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
  }
});
$("#oauth-start").addEventListener("click", async () => {
  const button = $("#oauth-start");
  try {
    button.disabled = true;
    const result = await api(
      `/api/accounts/${$("#auth-account-id").value}/auth-url`,
      { method: "POST", body: JSON.stringify({ mode: $("#auth-mode").value }) },
    );
    state.oauthSessionID = result.session_id;
    $("#oauth-link").href = result.auth_url;
    $("#oauth-exchange-fields").hidden = false;
    window.open(result.auth_url, "_blank", "noopener");
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
  }
});
$("#oauth-exchange").addEventListener("click", async () => {
  const button = $("#oauth-exchange");
  try {
    if (!state.oauthSessionID || !$("#oauth-code").value.trim())
      throw new Error("请先完成 OAuth 并填写授权码");
    button.disabled = true;
    await api(`/api/accounts/${$("#auth-account-id").value}/oauth-exchange`, {
      method: "POST",
      body: JSON.stringify({
        session_id: state.oauthSessionID,
        code: $("#oauth-code").value.trim(),
      }),
    });
    $("#account-auth-dialog").close();
    toast("OAuth 授权已保存");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
  }
});

$("#batch-auth-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#batch-auth-submit");
  try {
    const sessionKeys = $("#batch-session-keys")
      .value.split(/[\n\r,]+/)
      .map((value) => value.trim())
      .filter(Boolean);
    const groupIDs = $$('input[name="batch-group"]:checked').map(
      (node) => node.value,
    );
    if (!sessionKeys.length) throw new Error("至少输入一个 Claude Session Key");
    if (!groupIDs.length) throw new Error("至少选择一个分组");
    button.disabled = true;
    button.querySelector("span").textContent = `正在授权 0 / ${sessionKeys.length}`;
    const result = await api("/api/accounts/batch-authorize", {
      method: "POST",
      body: JSON.stringify({
        session_keys: sessionKeys,
        proxy_pool_id: Number($("#batch-proxy-pool").value),
        group_ids: groupIDs,
        auth_type: $("#batch-auth-type").value,
        account_price: Number($("#batch-account-price").value || 0),
        concurrency: Number($("#batch-concurrency").value || 3),
        base_rpm: Number($("#batch-base-rpm").value || 0),
      }),
    });
    $("#batch-result-panel").hidden = false;
    $("#batch-result-summary").textContent =
      `${result.success} 成功 · ${result.failed} 失败 · 共 ${result.total}`;
    state.batchResults = result.items;
    resetPagination("batch");
    renderBatchResults();
    toast(`批量授权完成：成功 ${result.success}，失败 ${result.failed}`);
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
    button.querySelector("span").textContent = "开始批量授权";
  }
});

$("#account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#account-submit");
  try {
    button.disabled = true;
    const id = $("#account-id").value;
    const sessionKey =
      !id && $("#account-auth-type").value !== "api_key"
        ? $("#account-session-key").value.trim()
        : "";
    if (sessionKey) {
      button.querySelector("span").textContent = "正在验证 SK 并创建…";
    }
    const credentialsText = $("#account-credentials").value.trim();
    const groupIDs = $$('input[name="account-group"]:checked').map(
      (node) => node.value,
    );
    if (!groupIDs.length) throw new Error("至少选择一个分组");
    const poolID = Number($("#account-proxy-pool").value || 0);
    const proxyID = Number($("#account-proxy").value || 0);
    const extra = JSON.parse($("#account-extra").value.trim() || "{}");
    extra.request_passthrough = $("#account-request-passthrough").checked;
    const payload = {
      name: $("#account-name").value,
      platform: "anthropic",
      auth_type: $("#account-auth-type").value,
      extra,
      status: $("#account-status").value,
      schedulable: $("#account-schedulable").checked,
      concurrency: Number($("#account-concurrency").value),
      priority: Number($("#account-priority").value),
      rate_multiplier: Number($("#account-rate").value),
      account_price: Number($("#account-price").value),
      notes: $("#account-notes").value,
      error_message: $("#account-error").value,
      expires_at: isoFromInput($("#account-expires").value),
      rate_limit_reset_at: isoFromInput($("#account-rate-reset").value),
      group_ids: groupIDs,
      proxy_pool_id: poolID || null,
      proxy_id: proxyID || null,
      auto_proxy: poolID ? $("#account-auto-proxy").checked : false,
      base_rpm: $("#account-rpm-enabled").checked
        ? Number($("#account-base-rpm").value)
        : 0,
      rpm_strategy: $("#account-rpm-strategy").value,
      rpm_sticky_buffer: Number($("#account-rpm-buffer").value),
      user_msg_queue_mode: $("#account-queue-mode").value,
    };
    if (sessionKey) payload.session_key = sessionKey;
    if (credentialsText) payload.credentials = JSON.parse(credentialsText);
    await api(id ? `/api/accounts/${id}` : "/api/accounts", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    $("#account-dialog").close();
    toast(
      id
        ? "账号已更新"
        : sessionKey
          ? "SK 授权成功，账号已创建"
          : "账号已创建，请完成授权后再启用调度",
    );
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
    syncAccountAuthFields();
  }
});
$("#purpose-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const id = $("#purpose-id").value;
    const payload = {
      key: $("#purpose-key").value,
      name: $("#purpose-name").value,
      active_group_id: $("#purpose-group").value,
      description: $("#purpose-description").value,
    };
    await api(id ? `/api/purposes/${id}` : "/api/purposes", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    $("#purpose-dialog").close();
    toast(id ? "用途已更新" : "用途已创建");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#group-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const optional = (selector) =>
      $(selector).value === "" ? null : Number($(selector).value);
    const id = $("#group-id").value;
    await api(`/api/groups/${id}`, {
      method: "PUT",
      body: JSON.stringify({
        name: $("#group-name").value,
        description: $("#group-description").value,
        rate_multiplier: Number($("#group-rate").value),
        status: $("#group-status").value,
        daily_limit_usd: optional("#group-daily"),
        monthly_limit_usd: optional("#group-monthly"),
      }),
    });
    $("#group-dialog").close();
    toast("分组已更新");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#proxy-pool-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const id = $("#proxy-pool-id").value;
    const payload = {
      name: $("#proxy-pool-name").value,
      source_type: $("#proxy-pool-source").value,
      default_protocol: $("#proxy-pool-protocol").value,
      status: $("#proxy-pool-status").value,
      api_url: $("#proxy-pool-api-url").value,
      api_headers: $("#proxy-pool-headers").value || "{}",
    };
    await api(id ? `/api/proxy-pools/${id}` : "/api/proxy-pools", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    $("#proxy-pool-dialog").close();
    toast("代理池已保存");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#proxy-import-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const result = await api("/api/proxies/batch", {
      method: "POST",
      body: JSON.stringify({
        pool_id: Number($("#proxy-import-pool").value),
        text: $("#proxy-import-text").value,
      }),
    });
    $("#proxy-import-dialog").close();
    $("#proxy-import-text").value = "";
    toast(
      `导入完成：新增 ${result.created}，重复 ${result.skipped}，无效 ${result.invalid}`,
    );
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#user-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const id = $("#user-id").value;
    const payload = {
      username: $("#user-username").value,
      name: $("#user-name").value,
      password: $("#user-password").value,
      role: $("#user-role").value,
      status: $("#user-status").value,
      rpm_limit: Number($("#user-rpm").value),
      allowed_group_ids: $$('input[name="user-group"]:checked').map(
        (node) => node.value,
      ),
      visible_pages: $$('input[name="user-page"]:checked').map(
        (node) => node.value,
      ),
    };
    await api(id ? `/api/users/${id}` : "/api/users", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    $("#user-dialog").close();
    toast("用户已保存");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#key-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const id = $("#key-id").value;
    const payload = {
      user_id:
        state.me.role === "user" ? state.me.id : Number($("#key-user").value),
      name: $("#key-name").value,
      group_id: $("#key-group").value,
      quota: Number($("#key-quota").value),
      expires_at: isoFromInput($("#key-expires").value),
      status: id ? $("#key-status").value : "active",
    };
    const result = await api(id ? `/api/api-keys/${id}` : "/api/api-keys", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    $("#key-dialog").close();
    if (result.key) {
      $("#created-secret").textContent = result.key;
      showInitializedDialog("#secret-dialog");
    } else toast("SK 已更新");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#copy-secret").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("#created-secret").textContent);
  toast("SK 已复制");
});
$("#price-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await api("/api/prices", {
      method: "POST",
      body: JSON.stringify({
        model: $("#price-model").value,
        input_per_million: Number($("#price-input").value),
        output_per_million: Number($("#price-output").value),
        cache_creation_per_million: Number($("#price-cache-create").value),
        cache_read_per_million: Number($("#price-cache-read").value),
      }),
    });
    $("#price-dialog").close();
    toast("模型价格已保存");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#usage-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const payload = {
      purpose_key: $("#usage-purpose").value,
      account_id: Number($("#usage-account").value || 0),
      model: $("#usage-model").value,
      input_tokens: Number($("#usage-input").value),
      output_tokens: Number($("#usage-output").value),
      cache_creation_tokens: Number($("#usage-cache-create").value),
      cache_read_tokens: Number($("#usage-cache-read").value),
    };
    if ($("#usage-actual").value !== "")
      payload.actual_cost_override = Number($("#usage-actual").value);
    await api("/api/usage", { method: "POST", body: JSON.stringify(payload) });
    $("#usage-dialog").close();
    toast("用量已写入");
    await loadCore();
    await loadBilling();
  } catch (error) {
    toast(error.message, "error");
  }
});

function initializeDates() {
  const now = new Date();
  const first = new Date(now.getFullYear(), now.getMonth(), 1);
  const local = (date) =>
    new Date(date.getTime() - date.getTimezoneOffset() * 60000)
      .toISOString()
      .slice(0, 10);
  const from = local(first);
  const to = local(now);
  $("#billing-from").value = from;
  $("#billing-to").value = to;
  $("#account-from").value = from;
  $("#account-to").value = to;
  $("#audit-from").value = from;
  $("#audit-to").value = to;
  $("#authorization-from").value = from;
  $("#authorization-to").value = to;
}
window.setInterval(updateSurvivalClocks, 60000);
boot();
