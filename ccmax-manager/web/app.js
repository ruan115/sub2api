const state = {
  me: null,
  view: "overview",
  dashboard: null,
  groups: [],
  strategies: [],
  strategiesLoaded: false,
  strategiesLoading: null,
  accounts: [],
  archivedAccounts: [],
  accountSummary: null,
  realtime: null,
  realtimeLoading: null,
  prices: [],
  pricingSync: null,
  billing: null,
  usage: [],
  audits: [],
  errors: null,
  daily: [],
  authorization: null,
  deauth: null,
  deauthWindow: 60,
  purposes: [],
  proxyPools: [],
  proxies: [],
  users: [],
  keys: [],
  accountGroup: "",
  accountSearch: "",
  accountStatus: "",
  deadStatus: "pending",
  deadSearch: "",
  accountsLoadedAt: 0,
  accountsLoading: null,
  accountAutoRefresh: true,
  breakdown: "group",
  proxyPoolFilter: "",
  proxySearch: "",
  oauthSessionID: "",
  batchResults: [],
  paginationPages: {},
  paginationSizes: {},
  serverPagination: {},
  selectedAccountIDs: new Set(),
  selectedDeadAccountIDs: new Set(),
  selectedProxyIDs: new Set(),
  selectedStrategyAccountIDs: new Set(),
  strategyAccountMode: "bind",
  strategyAccountID: "",
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
  errors: ["OPERATIONS / ERROR EVENTS", "报错信息", ""],
  strategies: ["DISPATCH / STRATEGIES", "策略观测", "新增策略"],
};
const rolePageDefaults = {
  admin: Object.keys(viewMeta),
  readonly_admin: [
    "overview",
    "accounts",
    "dead",
    "daily",
    "authorization",
    "errors",
    "proxies",
    "pricing",
    "billing",
    "audit",
  ],
  user: ["accounts", "access"],
};
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

function beginButtonRequest(button, busyText = "") {
  if (!button || button.dataset.requestPending === "true") return null;
  const label = button.querySelector("span");
  const previousLabel = label?.textContent || "";
  button.dataset.requestPending = "true";
  button.disabled = true;
  button.setAttribute("aria-busy", "true");
  if (label && busyText) label.textContent = busyText;
  return () => {
    delete button.dataset.requestPending;
    button.disabled = false;
    button.removeAttribute("aria-busy");
    if (label && busyText) label.textContent = previousLabel;
  };
}

const isAdmin = () => state.me?.role === "admin";
const isManager = () =>
  state.me?.role === "admin" || state.me?.role === "readonly_admin";
const canView = (page) =>
  state.me?.role === "admin" || state.me?.visible_pages?.includes(page);
const uiChoices = new Map();
const sidebarStorageKey = "ccmax.sidebar.collapsed";
const accountAutoRefreshStorageKey = "ccmax.accounts.auto-refresh";
const accountColumnWidthsStorageKey = "ccmax.accounts.column-widths";
const accountAutoRefreshInterval = 30000;
const realtimeRefreshInterval = 5000;
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
  errors: { body: "error-body", size: 20, render: renderErrors },
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
      <label class="pagination-jump"><span class="sr-only">跳转页码</span><input type="number" min="1" max="${totalPages}" value="${page}" data-pagination-jump="${key}" aria-label="跳转页码" /><span>/ ${totalPages}</span></label>
      <button type="button" data-pagination-go="${key}" title="跳转" aria-label="跳转到指定页"><i data-lucide="arrow-right-to-line"></i></button>
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
  if (key === "errors") return loadErrors();
}

async function goToPaginationPage(key, value) {
  const server = state.serverPagination[key];
  const control = document.querySelector(`input[data-pagination-jump="${key}"]`);
  const totalPages = Math.max(
    1,
    Number(server?.totalPages || control?.max || 1),
  );
  const page = Math.min(totalPages, Math.max(1, Number(value) || 1));
  state.paginationPages[key] = page;
  if (server) await loadServerPage(key);
  else paginationTables[key].render();
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
  initializeResizableTable();
}

function initializeResizableTable() {
  const table = $(".accounts-table");
  if (!table || table.dataset.resizableReady === "true") return;
  table.dataset.resizableReady = "true";
  const minimumWidths = {
    select: 36,
    account: 140,
    status: 88,
    subscription: 64,
    price: 57,
    billing: 57,
    quota: 186,
    requests: 48,
    onboarded: 72,
    survival: 70,
    "last-used": 72,
    actions: 84,
  };
  let saved = {};
  let savedChanged = false;
  try {
    saved = JSON.parse(localStorage.getItem(accountColumnWidthsStorageKey) || "{}");
  } catch {
    saved = {};
  }
  $$("th[data-column]", table).forEach((header) => {
    const key = header.dataset.column;
    const minimum = minimumWidths[key] || 64;
    if (Number(saved[key]) > 0) {
      const width = Math.max(minimum, Number(saved[key]));
      header.style.width = `${width}px`;
      if (width !== Number(saved[key])) {
        saved[key] = width;
        savedChanged = true;
      }
    }
    if (key === "select") return;
    const handle = document.createElement("span");
    handle.className = "column-resizer";
    handle.setAttribute("aria-hidden", "true");
    header.append(handle);
    handle.addEventListener("pointerdown", (event) => {
      event.preventDefault();
      const startX = event.clientX;
      const startWidth = header.getBoundingClientRect().width;
      const startTableWidth = table.getBoundingClientRect().width;
      handle.setPointerCapture(event.pointerId);
      table.classList.add("is-resizing");
      const move = (moveEvent) => {
        const delta = moveEvent.clientX - startX;
        header.style.width = `${Math.max(minimum, startWidth + delta)}px`;
        table.style.width = `${Math.max(table.parentElement.clientWidth, startTableWidth + delta)}px`;
      };
      const finish = () => {
        handle.removeEventListener("pointermove", move);
        handle.removeEventListener("pointerup", finish);
        handle.removeEventListener("pointercancel", finish);
        table.classList.remove("is-resizing");
        saved[key] = Math.round(header.getBoundingClientRect().width);
        localStorage.setItem(accountColumnWidthsStorageKey, JSON.stringify(saved));
      };
      handle.addEventListener("pointermove", move);
      handle.addEventListener("pointerup", finish);
      handle.addEventListener("pointercancel", finish);
    });
  });
  if (savedChanged)
    localStorage.setItem(accountColumnWidthsStorageKey, JSON.stringify(saved));
}
function resetDialogViewport(dialog) {
  dialog.scrollTop = 0;
  $$("form, .form-grid, .form-stack, .secret-body, .strategy-account-list", dialog).forEach((node) => {
    node.scrollTop = 0;
  });
}
function showInitializedDialog(selector) {
  const dialog = $(selector);
  resetDialogViewport(dialog);
  dialog.showModal();
  const reset = () => resetDialogViewport(dialog);
  requestAnimationFrame(() => {
    reset();
    requestAnimationFrame(reset);
  });
  // The modal entrance animation can restore the previous scroll offset after
  // the first layout frame in WebKit/Chromium.
  setTimeout(reset, 250);
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
  const groupID = String(id || "").toLowerCase();
  const item = state.groups.find((group) => group.id === groupID);
  const title = item?.name || `${groupID.toUpperCase()} 分组`;
  const label = groupID === "a" || groupID === "b" ? groupID.toUpperCase() : title;
  const tone = groupID === "a" || groupID === "b" ? groupID : "dynamic";
  return `<span class="group-mark ${tone} ${modifier}" title="${escapeHTML(title)}"><svg aria-hidden="true"><use href="#claude-group-icon"></use></svg><span>${escapeHTML(label)}</span></span>`;
}
function availableGroups(activeOnly = false) {
  return state.groups.filter(
    (group) => !activeOnly || group.status === "active",
  );
}
function groupOption(group, selected = "") {
  const suffix = group.reserve_pool_enabled
    ? "（储备池）"
    : group.status === "active"
      ? ""
      : "（已停用）";
  return `<option value="${escapeHTML(group.id)}" ${group.id === selected ? "selected" : ""}>${escapeHTML(group.name + suffix)}</option>`;
}
function fillGroupSelect(
  selector,
  includeAll = false,
  activeOnly = false,
  dispatchOnly = false,
) {
  const select = $(selector);
  if (!select) return;
  const selected = select.value;
  const groups = availableGroups(activeOnly).filter(
    (group) => !dispatchOnly || !group.reserve_pool_enabled,
  );
  select.innerHTML = `${includeAll ? '<option value="">全部分组</option>' : ""}${groups
    .map((group) => groupOption(group, selected))
    .join("")}`;
  const values = [...select.options].map((option) => option.value);
  if (values.includes(selected)) select.value = selected;
}
function fillGroupPicker(selector, inputName, activeOnly = false) {
  const fieldset = $(selector);
  if (!fieldset) return;
  const selected = new Set(
    $$(`input[name="${inputName}"]:checked`, fieldset).map((node) => node.value),
  );
  fieldset.querySelectorAll(".group-choice").forEach((node) => node.remove());
  const groups = availableGroups(activeOnly);
  if (!selected.size && groups.length) selected.add(groups[0].id);
  const html = groups
    .map(
      (group) =>
        `<label class="group-choice group-${group.id === "a" || group.id === "b" ? group.id : "dynamic"}" title="${escapeHTML(group.name)}"><input class="${inputName === "batch-edit-group" ? "batch-edit-value" : ""}" type="checkbox" name="${inputName}" value="${escapeHTML(group.id)}" ${selected.has(group.id) ? "checked" : ""} /><span class="group-choice-icon"><svg aria-hidden="true"><use href="#claude-group-icon"></use></svg></span><b>${escapeHTML(group.name)}${group.reserve_pool_enabled ? " · 储备" : ""}</b></label>`,
    )
    .join("");
  const hint = fieldset.querySelector("small");
  if (hint) hint.insertAdjacentHTML("beforebegin", html);
  else fieldset.insertAdjacentHTML("beforeend", html);
}
function hydrateGroupControls() {
  const buttons = [
    '<button class="active" data-group="">全部</button>',
    ...availableGroups().map(
      (group) =>
        `<button data-group="${escapeHTML(group.id)}">${groupMark(group.id, "button-mark")}</button>`,
    ),
  ];
  $("#account-group-filter").innerHTML = buttons.join("");
  const activeButton = $(`#account-group-filter button[data-group="${state.accountGroup}"]`);
  if (activeButton) {
    $$("#account-group-filter button").forEach((button) =>
      button.classList.toggle("active", button === activeButton),
    );
  } else {
    state.accountGroup = "";
  }
  fillGroupPicker("#batch-group-picker", "batch-group", true);
  fillGroupPicker("#account-group-picker", "account-group");
  fillGroupPicker("#batch-edit-group-picker", "batch-edit-group");
  fillGroupPicker("#user-group-picker", "user-group");
  fillGroupSelect("#purpose-group", false, true, true);
  fillGroupSelect("#billing-group", true);
  fillGroupSelect("#error-group", true);
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
async function copyToClipboard(value) {
  const text = String(value || "");
  if (!text) throw new Error("没有可复制的内容");
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Continue with the selection-based fallback for restricted browsers.
    }
  }
  const fallback = document.createElement("textarea");
  fallback.value = text;
  fallback.setAttribute("readonly", "");
  fallback.style.position = "fixed";
  fallback.style.opacity = "0";
  fallback.style.pointerEvents = "none";
  document.body.append(fallback);
  fallback.select();
  const copied = document.execCommand("copy");
  fallback.remove();
  if (!copied) throw new Error("浏览器拒绝访问剪切板");
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
  document.body.classList.toggle("ordinary-user", state.me.role === "user");
  $("#identity-name").textContent = state.me.name || state.me.username;
  $("#identity-role").textContent = roleName(state.me.role);
  $("#readonly-banner").hidden = state.me.role !== "readonly_admin";
  $$(".nav-item[data-view]").forEach((node) => {
    node.hidden = !canView(node.dataset.view);
  });
  $$(".write-action").forEach((node) => {
    node.hidden = !isAdmin();
  });
  const accountRefreshLabel = isAdmin()
    ? "检测当前页账号存活状态并刷新列表"
    : "刷新账号列表";
  $("#refresh-accounts").title = accountRefreshLabel;
  $("#refresh-accounts").setAttribute("aria-label", accountRefreshLabel);
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
      state.groups = state.dashboard.groups;
      renderDashboard();
    } else if (
      ["accounts", "dead", "onboarding", "billing", "access", "errors"].some(
        (page) => canView(page),
      )
    ) {
      state.groups = await api("/api/groups");
    }
    if (state.groups.length) hydrateGroupControls();
    if (canView("billing") && !canView("overview"))
      state.purposes = await api("/api/purposes");
    const inventoryLoads = [];
    if (canView("accounts") || canView("dead") || canView("onboarding"))
      inventoryLoads.push(loadAccounts());
    if (canView("proxies") || canView("onboarding"))
      inventoryLoads.push(loadProxyInventory());
    await Promise.all(inventoryLoads);
    if (canView("pricing")) {
      [state.prices, state.pricingSync] = await Promise.all([
        api("/api/prices"),
        api("/api/prices/sync-status"),
      ]);
      renderPriceTable();
      renderPriceSync();
    }
    if (canView("access")) {
      if (state.me.role === "user") {
        state.me = await api("/api/me");
        $("#identity-name").textContent = state.me.name || state.me.username;
      }
      state.keys = await api("/api/api-keys");
      if (isAdmin()) state.users = await api("/api/users");
      renderAccess();
    } else if (canView("billing")) {
      // The ledger needs the key list for its SK filter even when the caller
      // cannot open the access page.
      state.keys = await api("/api/api-keys");
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
  if (state.accountsLoading) return state.accountsLoading;
  state.accountsLoading = (async () => {
    const [accounts, archivedAccounts] = await Promise.all([
      api("/api/accounts"),
      canView("dead") ? api("/api/accounts?archived=only") : Promise.resolve([]),
    ]);
    state.accounts = accounts;
    state.archivedAccounts = archivedAccounts;
    const accountIDs = new Set(state.accounts.map((item) => item.id));
    state.selectedAccountIDs = new Set(
      [...state.selectedAccountIDs].filter((id) => accountIDs.has(id)),
    );
    const deadIDs = new Set(
      state.accounts
        .filter((item) => item.dispatch_status === "error")
        .map((item) => item.id),
    );
    state.selectedDeadAccountIDs = new Set(
      [...state.selectedDeadAccountIDs].filter((id) => deadIDs.has(id)),
    );
    state.accountsLoadedAt = performance.now();
    renderAccounts();
    renderDeadAccounts();
    if (canView("accounts"))
      await Promise.all([loadAccountSummary(), loadRealtime()]);
  })();
  try {
    return await state.accountsLoading;
  } finally {
    state.accountsLoading = null;
  }
}

async function loadProxyInventory() {
  [state.proxyPools, state.proxies] = await Promise.all([
    api("/api/proxy-pools"),
    api("/api/proxies"),
  ]);
  if (canView("proxies")) renderProxies();
}

async function loadRealtime() {
  if (!canView("accounts")) return;
  if (state.realtimeLoading) return state.realtimeLoading;
  const requestedGroup = state.accountGroup;
  state.realtimeLoading = (async () => {
    const params = new URLSearchParams();
    if (requestedGroup) params.set("group_id", requestedGroup);
    const realtime = await api(`/api/stats/realtime?${params}`);
    if (requestedGroup !== state.accountGroup) return;
    state.realtime = realtime;
    renderRealtime();
  })();
  try {
    return await state.realtimeLoading;
  } finally {
    state.realtimeLoading = null;
    if (requestedGroup !== state.accountGroup && state.view === "accounts") {
      loadRealtime().catch(() => {});
    }
  }
}

function initializeRealtimeRefresh() {
  window.setInterval(async () => {
    if (
      !state.me ||
      state.view !== "accounts" ||
      document.hidden ||
      $("dialog[open]") ||
      state.realtimeLoading ||
      !canView("accounts")
    )
      return;
    try {
      await loadRealtime();
    } catch {
      // Keep the last rolling snapshot; the next interval retries.
    }
  }, realtimeRefreshInterval);
}

function initializeAccountAutoRefresh() {
  try {
    state.accountAutoRefresh =
      localStorage.getItem(accountAutoRefreshStorageKey) !== "false";
  } catch {
    state.accountAutoRefresh = true;
  }
  const toggle = $("#account-auto-refresh");
  toggle.checked = state.accountAutoRefresh;
  toggle.addEventListener("change", () => {
    state.accountAutoRefresh = toggle.checked;
    try {
      localStorage.setItem(
        accountAutoRefreshStorageKey,
        String(state.accountAutoRefresh),
      );
    } catch {
      // Automatic refresh still works when persistence is unavailable.
    }
  });
  window.setInterval(async () => {
    if (
      !state.accountAutoRefresh ||
      !state.me ||
      document.hidden ||
      $("dialog[open]") ||
      state.accountsLoading ||
      !["accounts", "dead", "onboarding"].includes(state.view) ||
      (!canView("accounts") && !canView("dead") && !canView("onboarding"))
    )
      return;
    try {
      await loadAccounts();
    } catch {
      // Keep the last good account snapshot; the next interval retries.
    }
  }, accountAutoRefreshInterval);
}

async function loadBilling() {
  if (!canView("billing")) return;
  const params = new URLSearchParams(paginationParams("usage"));
  for (const [key, selector] of [
    ["from", "#billing-from"],
    ["to", "#billing-to"],
    ["group_id", "#billing-group"],
    ["purpose_key", "#billing-purpose"],
    ["api_key_id", "#billing-api-key"],
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
  const from = $("#daily-from").value;
  const to = $("#daily-to").value;
  if (!from || !to) {
    toast("请选择完整的开始和结束日期", "error");
    return;
  }
  if (from > to) {
    toast("开始日期不能晚于结束日期", "error");
    return;
  }
  try {
    const params = new URLSearchParams({ from, to });
    state.daily = await api(`/api/stats/daily?${params}`);
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

async function loadErrors() {
  if (!canView("errors")) return;
  const params = new URLSearchParams(paginationParams("errors"));
  if ($("#error-search").value)
    params.set("search", $("#error-search").value.trim());
  if ($("#error-source").value)
    params.set("source", $("#error-source").value);
  if ($("#error-group").value)
    params.set("group_id", $("#error-group").value);
  if ($("#error-from").value) params.set("from", $("#error-from").value);
  if ($("#error-to").value) params.set("to", $("#error-to").value);
  try {
    state.errors = await api(`/api/error-logs?${params}`);
    setServerPagination("errors", state.errors);
    renderErrors();
    loadErrorInsights();
  } catch (error) {
    toast(error.message, "error");
  }
}

let insightChart = null;
let insightTimelineChart = null;
async function loadErrorInsights() {
  const source = $("#error-source").value;
  $("#error-insight-panel").hidden = Boolean(source && source !== "gateway");
  if (source && source !== "gateway") return;
  const params = new URLSearchParams();
  params.set("status", $("#insight-status").value || "401");
  if ($("#error-search").value)
    params.set("search", $("#error-search").value.trim());
  if ($("#error-group").value)
    params.set("group_id", $("#error-group").value);
  if ($("#error-from").value) params.set("from", $("#error-from").value);
  if ($("#error-to").value) params.set("to", $("#error-to").value);
  try {
    const data = await api(`/api/error-insights?${params}`);
    renderErrorInsights(data);
  } catch (error) {
    toast(error.message, "error");
  }
}

function renderErrorInsights(data) {
  const accounts = data.accounts || [];
  const events = data.events || [];
  const timeline = data.timeline || [];
  $("#insight-empty").hidden =
    accounts.length > 0 || events.length > 0 || timeline.length > 0;
  $("#insight-body").innerHTML = events
    .map(
      (item) => `<tr>
        <td class="mono time-cell">${dateTime(item.created_at)}</td>
        <td>${escapeHTML(item.account_name || `#${item.account_id}`)}</td>
        <td class="num mono">${item.rpm >= 0 ? item.rpm : "—"}</td>
        <td class="num mono">${item.tpm >= 0 ? Number(item.tpm).toLocaleString("zh-CN") : "—"}</td>
        <td class="num mono">${item.total_requests >= 0 ? Number(item.total_requests).toLocaleString("zh-CN") : "—"}</td>
        <td class="error-message-cell">${escapeHTML(item.message)}</td>
      </tr>`,
    )
    .join("");
  const canvas = $("#insight-chart");
  if (insightChart) {
    insightChart.destroy();
    insightChart = null;
  }
  if (insightTimelineChart) {
    insightTimelineChart.destroy();
    insightTimelineChart = null;
  }
  const granularity = data.timeline_granularity === "hour" ? "hour" : "day";
  $("#insight-timeline-granularity").textContent =
    granularity === "hour" ? "按小时" : "按天";
  if (typeof Chart === "undefined") return;
  if (timeline.length) {
    insightTimelineChart = new Chart($("#insight-timeline-chart"), {
      type: "bar",
      data: {
        labels: timeline.map((item) => dateTime(item.bucket)),
        datasets: [
          {
            label: `${data.status} 报错`,
            data: timeline.map((item) => item.count),
            backgroundColor: "rgba(196, 65, 59, 0.72)",
            borderColor: "rgba(153, 45, 41, 0.95)",
            borderWidth: 1,
            borderRadius: 4,
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: "index", intersect: false },
        scales: {
          x: { grid: { display: false }, ticks: { maxRotation: 0 } },
          y: { beginAtZero: true, ticks: { precision: 0 } },
        },
        plugins: { legend: { display: false } },
      },
    });
  }
  if (!accounts.length) return;
  insightChart = new Chart(canvas, {
    type: "bar",
    data: {
      labels: accounts.map((item) => item.account_name || `#${item.account_id}`),
      datasets: [
        {
          label: `${data.status} 次数`,
          data: accounts.map((item) => item.count),
          backgroundColor: "rgba(239, 68, 68, 0.65)",
          yAxisID: "y",
        },
        {
          label: "报错时平均瞬时 RPM",
          data: accounts.map((item) => Math.round(item.avg_rpm * 10) / 10),
          backgroundColor: "rgba(59, 130, 246, 0.65)",
          yAxisID: "y1",
        },
        {
          label: "基础 RPM 限制",
          data: accounts.map((item) => item.base_rpm),
          type: "line",
          borderColor: "rgba(245, 158, 11, 0.9)",
          backgroundColor: "rgba(245, 158, 11, 0.9)",
          pointRadius: 3,
          fill: false,
          yAxisID: "y1",
        },
      ],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: "index", intersect: false },
      scales: {
        y: {
          position: "left",
          beginAtZero: true,
          title: { display: true, text: "报错次数" },
          ticks: { precision: 0 },
        },
        y1: {
          position: "right",
          beginAtZero: true,
          grid: { drawOnChartArea: false },
          title: { display: true, text: "RPM" },
        },
      },
    },
  });
}

const strategyModeLabels = {
  "": "跟随分组",
  serial: "串行填满",
  balance: "负载均衡",
  round_robin: "均匀 RPM",
  concentrated: "集中调度",
};
const strategyRPMModeLabels = {
  fixed: "固定硬限",
  tiered: "三区模型",
  sticky_exempt: "粘性豁免",
};
let expandedStrategyID = "";
let strategyAccordionInitialized = false;

async function loadStrategies() {
  if (!canView("strategies")) return;
  if (state.strategiesLoading) return state.strategiesLoading;
  state.strategiesLoading = (async () => {
    state.strategies = await api("/api/strategies/observe");
    state.strategiesLoaded = true;
    renderStrategies();
  })();
  try {
    return await state.strategiesLoading;
  } catch (error) {
    toast(error.message, "error");
  } finally {
    state.strategiesLoading = null;
  }
}

function initializeStrategyRefresh() {
  window.setInterval(() => {
    if (
      !state.me ||
      state.view !== "strategies" ||
      document.hidden ||
      $("dialog[open]") ||
      state.strategiesLoading ||
      !canView("strategies")
    )
      return;
    loadStrategies();
  }, realtimeRefreshInterval);
}

function strategyLimitText(value, unit = "") {
  return value > 0 ? `${Number(value).toLocaleString("zh-CN")}${unit}` : "不限";
}

function renderStrategies() {
  const strategies = state.strategies || [];
  $("#strategy-empty").hidden = strategies.length > 0;
  $("#strategy-count").textContent = `${strategies.length} 个策略`;
  $("#strategy-cards").innerHTML = strategies
    .map((item) => {
      const actions = isAdmin()
        ? `<span class="row-actions strategy-actions">
            <button data-bind-strategy="${item.id}" title="导入账号到策略池"><i data-lucide="user-plus"></i></button>
            <button data-unbind-strategy="${item.id}" title="从策略池移出账号"><i data-lucide="user-minus"></i></button>
            <button data-edit-strategy="${item.id}" title="编辑策略"><i data-lucide="pencil"></i></button>
            <button class="danger" data-delete-strategy="${item.id}" title="删除策略"><i data-lucide="trash-2"></i></button>
          </span>`
        : "";
      const boundLabel = item.bound_accounts
        ? `${item.bound_accounts} 个账号`
        : "未绑定账号";
      return `<article class="strategy-card">
        <div class="strategy-card-head">
          <strong title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</strong>
          ${actions}
        </div>
        <p class="strategy-card-desc">${escapeHTML(item.description || strategyModeLabels[item.dispatch_mode] || "")}</p>
        <div class="strategy-card-grid">
          <div><span>RPM</span><b>${item.current_rpm} / ${strategyLimitText(item.rpm_limit)}</b></div>
          <div><span>TPM</span><b>${Number(item.current_tpm).toLocaleString("zh-CN")} / ${strategyLimitText(item.tpm_limit)}</b></div>
          <div><span>并发</span><b>${item.current_inflight} / ${strategyLimitText(item.concurrency_limit)}</b></div>
          <div><span>存活账号</span><b class="${item.accounts_alive < item.bound_accounts ? "warn" : ""}">${item.accounts_alive} / ${item.bound_accounts}${item.accounts_pending ? `<small> · 待调度 ${item.accounts_pending}</small>` : ""}</b></div>
        </div>
        <footer>
          <span class="pill">${escapeHTML(strategyRPMModeLabels[item.rpm_strategy] || item.rpm_strategy)}</span>
          <span class="pill">${escapeHTML(strategyModeLabels[item.dispatch_mode] ?? item.dispatch_mode)}</span>
          <span class="pill ${item.bound_groups ? "ok" : "off"}">${item.bound_groups} 个分组绑定</span>
          <span class="pill ${item.bound_accounts ? "ok" : "off"}">${escapeHTML(boundLabel)}</span>
        </footer>
      </article>`;
    })
    .join("");
  renderStrategyAccordion(strategies);
  refreshIcons();
}

function strategyAccountCandidates(strategyID, mode) {
  const currentID = Number(strategyID);
  const search = $("#strategy-account-search")?.value.trim().toLowerCase() || "";
  return (state.accounts || [])
    .filter((account) =>
      mode === "unbind"
        ? Number(account.strategy_id || 0) === currentID
        : Number(account.strategy_id || 0) !== currentID,
    )
    .filter((account) => {
      if (!search) return true;
      return [
        account.id,
        account.name,
        account.credential_hint,
        account.source_sk_hint,
        account.proxy_name,
        account.proxy_hint,
        account.proxy_ip,
      ]
        .filter((value) => value !== null && value !== undefined)
        .join(" ")
        .toLowerCase()
        .includes(search);
    });
}

function renderStrategyAccountList() {
  const strategyID = state.strategyAccountID;
  const mode = state.strategyAccountMode;
  const candidates = strategyAccountCandidates(strategyID, mode);
  const selected = candidates.filter((item) =>
    state.selectedStrategyAccountIDs.has(item.id),
  );
  $("#strategy-account-count").textContent = `${selected.length} / ${candidates.length} 个账号`;
  $("#strategy-account-list").innerHTML = candidates.length
    ? candidates
        .map((item) => {
          const groups = item.group_ids?.length
            ? item.group_ids.map((groupID) => groupMark(groupID, "mini")).join("")
            : '<span class="muted">无分组</span>';
          const proxy = item.proxy_ip || item.proxy_hint || "未绑定 IP";
          const checked = state.selectedStrategyAccountIDs.has(item.id)
            ? "checked"
            : "";
          return `<label class="strategy-account-row">
            <input type="checkbox" data-strategy-account-select="${item.id}" ${checked} />
            <span>
              <strong title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</strong>
              <small title="${escapeHTML(proxy)}">${groups}<em>${escapeHTML(proxy)}</em></small>
            </span>
            <b class="pill ${item.dispatch_status === "normal" ? "ok" : item.dispatch_status === "error" ? "error" : "off"}">${item.dispatch_status === "normal" ? "正常" : item.dispatch_status === "error" ? "错误" : "暂不可调度"}</b>
          </label>`;
        })
        .join("")
    : '<div class="empty-state compact"><strong>没有可选择账号</strong></div>';
  refreshIcons();
}

async function openStrategyAccountDialog(strategyID, mode) {
  if (!state.accounts.length && canView("accounts")) await loadAccounts();
  const item = state.strategies.find(
    (entry) => String(entry.id) === String(strategyID),
  );
  if (!item) return;
  state.strategyAccountID = String(strategyID);
  state.strategyAccountMode = mode;
  state.selectedStrategyAccountIDs.clear();
  $("#strategy-account-title").textContent =
    mode === "unbind"
      ? `从“${item.name}”移出账号`
      : `导入账号到“${item.name}”`;
  $("#strategy-account-id").value = item.id;
  $("#strategy-account-mode").value = mode;
  $("#strategy-account-search").value = "";
  renderStrategyAccountList();
  showInitializedDialog("#strategy-account-dialog");
}

function strategySummaryStatus(item) {
  if (!item.bound_accounts) {
    return { label: "未绑定", className: "off" };
  }
  if (!item.accounts_alive) {
    return { label: "无可用账号", className: "error" };
  }
  if (item.accounts_pending) {
    return { label: `${item.accounts_pending} 个待调度`, className: "warn" };
  }
  return { label: "运行正常", className: "ok" };
}

function strategyAccountRows(item, open) {
  const accounts = item.accounts || [];
  if (!accounts.length) {
    return `<tr class="strategy-account-detail-row strategy-account-empty" ${open ? "" : "hidden"}>
      <td colspan="5"><i data-lucide="inbox"></i><span>该策略暂未绑定账号</span></td>
    </tr>`;
  }
  return accounts
    .map((account) => {
      const dispatchLabel =
        account.dispatch === "pending"
          ? "待调度"
          : account.alive
            ? "存活"
            : "不可用";
      const dispatchClass =
        account.dispatch === "pending"
          ? "warn"
          : account.alive
            ? "ok"
            : "error";
      const groups = account.group_ids?.length
        ? account.group_ids.join("、")
        : "未分组";
      return `<tr class="strategy-account-detail-row" ${open ? "" : "hidden"}>
        <td><span class="strategy-account-indent" aria-hidden="true"></span><strong class="strategy-account-name" title="${escapeHTML(account.name)}">${escapeHTML(account.name)}</strong></td>
        <td class="num" title="当前 RPM ${Number(account.rpm || 0).toLocaleString("zh-CN")}">${Number(account.rpm || 0).toLocaleString("zh-CN")}</td>
        <td class="num" title="当前 TPM ${Number(account.tpm || 0).toLocaleString("zh-CN")}">${Number(account.tpm || 0).toLocaleString("zh-CN")}</td>
        <td><span class="pill ${dispatchClass}" title="${escapeHTML(account.status || dispatchLabel)}">${dispatchLabel}</span></td>
        <td><span class="strategy-account-groups" title="${escapeHTML(groups)}">${escapeHTML(groups)}</span></td>
      </tr>`;
    })
    .join("");
}

function renderStrategyAccordion(strategies) {
  const root = $("#strategy-accordion");
  if (!root) return;
  if (!strategies.length) {
    expandedStrategyID = "";
    strategyAccordionInitialized = false;
    root.innerHTML = `<table class="strategy-accordion-table"><tbody><tr class="strategy-accordion-placeholder"><td colspan="5"><i data-lucide="list-tree"></i><span>暂无策略数据</span></td></tr></tbody></table>`;
    return;
  }

  if (!strategyAccordionInitialized) {
    expandedStrategyID = "";
    strategyAccordionInitialized = true;
  } else if (
    expandedStrategyID &&
    !strategies.some((item) => String(item.id) === expandedStrategyID)
  ) {
    expandedStrategyID = "";
  }

  root.innerHTML = `<table class="strategy-accordion-table">
      <colgroup>
        <col class="strategy-col-name" />
        <col />
        <col />
        <col class="strategy-col-status" />
        <col class="strategy-col-binding" />
      </colgroup>
      <thead>
        <tr>
          <th>策略 / 账号</th>
          <th class="num">当前 RPM</th>
          <th class="num">当前 TPM</th>
          <th>存活状态</th>
          <th>分组</th>
        </tr>
      </thead>
      ${strategies
        .map((item) => {
          const strategyID = String(item.id);
          const open = expandedStrategyID === strategyID;
          const status = strategySummaryStatus(item);
          const dispatchMode =
            strategyModeLabels[item.dispatch_mode] ?? item.dispatch_mode;
          const description = item.description || dispatchMode || "未填写说明";
          return `<tbody class="strategy-accordion-group ${open ? "is-open" : ""}">
            <tr class="strategy-summary-row">
              <td>
                <button class="strategy-accordion-trigger" type="button" data-strategy-toggle="${item.id}" aria-expanded="${open}" title="${open ? "收起账号详情" : "展开账号详情"}">
                  <span class="strategy-trigger-icon"><i data-lucide="chevron-right"></i></span>
                  <span class="strategy-trigger-copy">
                    <strong title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</strong>
                    <small title="${escapeHTML(description)}">${escapeHTML(description)}</small>
                  </span>
                </button>
              </td>
              <td class="num" title="当前 ${item.current_rpm}，限制 ${strategyLimitText(item.rpm_limit)}">${Number(item.current_rpm || 0).toLocaleString("zh-CN")} <small>/ ${strategyLimitText(item.rpm_limit)}</small></td>
              <td class="num" title="当前 ${Number(item.current_tpm || 0).toLocaleString("zh-CN")}，限制 ${strategyLimitText(item.tpm_limit)}">${Number(item.current_tpm || 0).toLocaleString("zh-CN")} <small>/ ${strategyLimitText(item.tpm_limit)}</small></td>
              <td><span class="pill ${status.className}" title="${escapeHTML(status.label)}">${escapeHTML(status.label)}</span><small class="strategy-survival" title="${item.accounts_alive} 个存活，${item.bound_accounts} 个已绑定">${item.accounts_alive} / ${item.bound_accounts}</small></td>
              <td><span class="strategy-account-groups" title="${escapeHTML(`${item.bound_groups} 个分组 · ${dispatchMode}`)}">${item.bound_groups} 个分组 · ${escapeHTML(dispatchMode)}</span></td>
            </tr>
            ${strategyAccountRows(item, open)}
          </tbody>`;
        })
        .join("")}
    </table>`;
}

function openStrategy(item = null) {
  $("#strategy-dialog-title").textContent = item ? "编辑策略" : "新增策略";
  $("#strategy-id").value = item?.id || "";
  $("#strategy-name").value = item?.name || "";
  $("#strategy-description").value = item?.description || "";
  $("#strategy-rpm").value = item?.rpm_limit ?? 0;
  $("#strategy-tpm").value = item?.tpm_limit ?? 0;
  $("#strategy-concurrency").value = item?.concurrency_limit ?? 0;
  $("#strategy-rpm-mode").value = item?.rpm_strategy || "fixed";
  $("#strategy-buffer").value = item?.rpm_sticky_buffer ?? 0;
  $("#strategy-dispatch-mode").value = item?.dispatch_mode || "";
  showInitializedDialog("#strategy-dialog");
}

function fillStrategySelect(select, selectedID) {
  const current = selectedID ? String(selectedID) : "";
  // Each select declares its own "no strategy" wording in the markup.
  const placeholder = select.dataset.emptyLabel || select.options[0]?.text || "";
  select.dataset.emptyLabel = placeholder;
  select.innerHTML =
    `<option value="">${escapeHTML(placeholder)}</option>` +
    (state.strategies || [])
      .map(
        (item) =>
          `<option value="${item.id}" ${String(item.id) === current ? "selected" : ""}>${escapeHTML(item.name)}</option>`,
      )
      .join("");
}

async function ensureStrategiesLoaded() {
  if (state.strategiesLoaded || !canView("strategies")) return;
  try {
    state.strategies = await api("/api/strategies/observe");
    state.strategiesLoaded = true;
  } catch {
    // Selects keep only the placeholder; strategySelectPayload then omits
    // strategy_id so an unreadable list can never silently unbind anything.
  }
}

// Returns undefined while the strategy list is unknown, so the field is dropped
// from the payload and the backend keeps the current binding.
function strategySelectPayload(select) {
  return state.strategiesLoaded ? Number(select.value || 0) : undefined;
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
        `<div class="purpose-row"><div class="purpose-name"><strong>${escapeHTML(item.name)}</strong><small>${escapeHTML(item.key)}</small></div><span class="purpose-description">${escapeHTML(item.description || "—")}</span><div class="purpose-actions"><select class="purpose-group-select" data-purpose-switch="${item.id}" ${isAdmin() ? "" : "disabled"} aria-label="${escapeHTML(item.name)} 调度分组">${state.groups.filter((group) => !group.reserve_pool_enabled && (group.status === "active" || group.id === item.active_group_id)).map((group) => groupOption(group, item.active_group_id)).join("")}</select>${isAdmin() ? `<button class="icon-button compact" data-edit-purpose="${item.id}" title="编辑用途">···</button>` : ""}</div></div>`,
    )
    .join("");
  $("#group-cards").innerHTML = data.groups
    .map((item) => {
      const ratio = item.total_accounts
        ? Math.round((item.active_accounts / item.total_accounts) * 100)
        : 0;
      const streamDispatch = item.adaptive_hedge_enabled
        ? "大 RPM 自适应"
        : item.stream_hedge_enabled
          ? "极速竞速"
          : "串行";
      const passthroughCount = [
        item.service_tier_passthrough_enabled,
        item.inference_geo_passthrough_enabled,
        item.speed_passthrough_enabled,
        item.anthropic_beta_passthrough_enabled,
      ].filter(Boolean).length;
      return `<article class="group-card ${item.id === "a" || item.id === "b" ? item.id : "dynamic"}"><div class="group-card-head">${groupMark(item.id, "large")}${isAdmin() ? `<button class="icon-button group-settings" data-edit-group="${item.id}">···</button>` : ""}</div><h3 title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</h3><p title="${escapeHTML(item.description || "—")}">${escapeHTML(item.description || "—")}</p><div class="group-stat-line"><span>${item.reserve_pool_enabled ? "储备账号" : "可用账号"}</span><strong>${item.active_accounts} / ${item.total_accounts}</strong></div><div class="capacity-bar"><span style="width:${ratio}%"></span></div><div class="group-stat-line"><span>分组角色</span><strong>${item.reserve_pool_enabled ? "按需储备" : "请求调度"}</strong></div><div class="group-stat-line"><span>本月计费</span><strong>${money(item.month_billed_cost)}</strong></div><div class="group-stat-line"><span>计费倍率</span><strong>× ${Number(item.rate_multiplier).toFixed(2)}</strong></div><div class="group-stat-line"><span>请求模式</span><strong>${item.reserve_pool_enabled ? "不接收请求" : item.normal_request_mode ? "蒸馏兼容" : "Sub2 原版"}</strong></div><div class="group-stat-line"><span>身份句</span><strong>${item.claude_code_identity_enabled ? "开启" : "关闭"}</strong></div><div class="group-stat-line"><span>静默降级</span><strong>${item.reject_anthropic_downgrade_enabled ? "拒绝" : "允许"}</strong></div><div class="group-stat-line"><span>用户蒸馏</span><strong>${item.reject_distillation_enabled ? "拒绝" : "允许"}</strong></div><div class="group-stat-line"><span>字段透传</span><strong>${passthroughCount ? `${passthroughCount} 项` : "关闭"}</strong></div><div class="group-stat-line"><span>工具名</span><strong>${item.mcp_tool_names_enabled ? "MCP 化" : "默认"}</strong></div><div class="group-stat-line"><span>账号调度</span><strong>${item.reserve_pool_enabled ? "缺口单向补号" : item.rpm_dispatch_enabled ? "RPM 集中" : "兼容轮询"}</strong></div><div class="group-stat-line"><span>429 重试</span><strong>${item.rate_limit_wait_enabled ? `${Number(item.rate_limit_wait_seconds || 5)}s` : "立即切换"}</strong></div><div class="group-stat-line"><span>529 熔断</span><strong>${Number(item.overload_cooldown_seconds || 10)}s</strong></div><div class="group-stat-line"><span>流式调度</span><strong>${streamDispatch}</strong></div></article>`;
    })
    .join("");
  $("#recent-usage-body").innerHTML = usageRows(data.recent_usage, true);
}

const limitWindowLabels = { "5h": "5h 限制", "7d": "7d 限制" };

// The 5h / 7d buckets are subsets of "暂不可调度", so an empty filter or the
// parent filter still matches a window-limited account.
function accountMatchesStatus(account, status) {
  if (!status) return true;
  if (status === "limited_5h" || status === "limited_7d") {
    return (
      account.dispatch_status === "unavailable" &&
      account.limit_window === status.slice("limited_".length)
    );
  }
  return account.dispatch_status === status;
}

function accountStatus(account) {
  if (account.dispatch_status === "error") return ["错误", "error"];
  if (account.dispatch_status === "unavailable") {
    const label = limitWindowLabels[account.limit_window];
    return label ? [label, "warn"] : ["暂不可调度", "off"];
  }
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
function accountSubscriptionName(item) {
  const subscription = subscriptionName(item?.subscription_type);
  const tier = String(item?.rate_limit_tier || "").toLowerCase();
  const multiplier = tier.match(/(?:^|_)(\d+x)(?:_|$)/)?.[1];
  return subscription === "Max" && multiplier
    ? `${subscription} ${multiplier}`
    : subscription;
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
  const duration = durationClockText(minutes, minutes !== null);
  return `<time class="live-duration mono" data-account-survival="${item.id}" title="${duration} · ${status}">${duration}</time>`;
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
    const resetText = resetAt ? dateTime(resetAt) : "等待刷新";
    return `<div class="usage-window-row"><b>${label}</b><span><i style="width:${used}%"></i></span><strong>${used.toFixed(0)}%</strong><small title="${escapeHTML(resetText)}">${resetText}</small></div>`;
  };
  const load = state.realtime?.accounts?.find(
    (entry) => entry.account_id === item.id,
  );
  return `<div class="usage-window">${row("5h", item.quota_5h_utilization, item.quota_5h_reset_at)}${row("7d", item.quota_7d_utilization, item.quota_7d_reset_at)}<div class="usage-window-row live-row" data-account-realtime="${item.id}">${accountLiveLoadInner(load, item.base_rpm)}</div></div>`;
}
function accountLiveLoadInner(load, fallbackBaseRPM = 0) {
  const rpm = Number(load?.rpm || 0);
  const tpm = Number(load?.tpm || 0);
  const inflight = Number(load?.inflight || 0);
  const baseRPM = Number(
    load?.effective_rpm ?? load?.base_rpm ?? fallbackBaseRPM ?? 0,
  );
  const temporaryRPM = Number(load?.temporary_rpm || 0);
  let tone = "idle";
  if (baseRPM > 0 && rpm >= baseRPM) tone = "error";
  else if (baseRPM > 0 && rpm >= baseRPM * 0.8) tone = "warn";
  else if (rpm > 0 || inflight > 0) tone = "ok";
  const ratio =
    baseRPM > 0 ? Math.min(100, Math.round((rpm / baseRPM) * 100)) : 0;
  const capacity = baseRPM > 0 ? `${rpm}/${baseRPM}` : `${rpm}/∞`;
  const detail = `TPM ${compact(tpm)} · 在途 ${inflight}`;
  const thresholdHint = temporaryRPM > 0 ? ` · 临时阈值 ${temporaryRPM}` : "";
  const hint = `最近 60 秒：RPM ${capacity}${baseRPM > 0 ? "" : "（未设置 RPM 上限）"}${thresholdHint} · TPM ${Number(tpm).toLocaleString("zh-CN")} · 在途 ${inflight}`;
  return `<b>RPM</b><span class="${tone}"><i style="width:${ratio}%"></i></span><strong class="${tone}" title="${escapeHTML(hint)}">${capacity}</strong><small title="${escapeHTML(hint)}">${detail}</small>`;
}
function renderRealtime() {
  const data = state.realtime;
  if (!data) return;
  $("#realtime-rpm").textContent = Number(data.rpm || 0).toLocaleString("zh-CN");
  $("#realtime-tpm").textContent = compact(data.tpm);
  $("#realtime-inflight").textContent = Number(
    data.inflight || 0,
  ).toLocaleString("zh-CN");
  $("#realtime-active-accounts").textContent = `${data.active_accounts} / ${data.eligible_accounts}`;
  const capacity = data.unlimited_capacity
    ? "含不限速账号"
    : `容量 ${Number(data.rpm_capacity || 0).toLocaleString("zh-CN")} RPM`;
  $("#realtime-rpm-capacity").textContent = capacity;
  $("#realtime-rpm-capacity").title = capacity;
  const activeNames = (data.accounts || [])
    .filter((item) => item.active)
    .map((item) => item.name)
    .join("、");
  const activeText = activeNames || "暂无负载";
  $("#realtime-active-names").textContent = activeText;
  $("#realtime-active-names").title = activeText;
  $("#realtime-active-item").title = activeNames
    ? `当前激活：${activeNames}`
    : "最近 60 秒没有账号承担请求";
  const updated = new Date(data.updated_at);
  const updatedText = Number.isNaN(updated.getTime())
    ? "刚刚更新"
    : `${updated.toLocaleTimeString("zh-CN", { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" })} 更新`;
  $("#realtime-updated-at").textContent = updatedText;
  $("#realtime-updated-at").title = `${updatedText} · 每 5 秒局部刷新`;
  const loads = new Map(
    (data.accounts || []).map((item) => [item.account_id, item]),
  );
  $$('[data-account-realtime]').forEach((node) => {
    const id = Number(node.dataset.accountRealtime);
    const account = state.accounts.find((item) => item.id === id);
    node.innerHTML = accountLiveLoadInner(loads.get(id), account?.base_rpm);
  });
}
function closeAccountActionMenu() {
  const menu = $("#account-action-menu");
  if (!menu || menu.hidden) return;
  menu.hidden = true;
  menu.innerHTML = "";
  const trigger = $('[data-account-menu][aria-expanded="true"]');
  if (trigger) trigger.setAttribute("aria-expanded", "false");
}
function accountNeedsSchedulingResume(item) {
  return !item.schedulable || item.dispatch_status === "unavailable";
}
function openAccountActionMenu(trigger, item) {
  const menu = $("#account-action-menu");
  const alreadyOpen =
    !menu.hidden && menu.dataset.accountId === String(item.id);
  closeAccountActionMenu();
  if (alreadyOpen) return;
  const shouldResume = accountNeedsSchedulingResume(item);
  menu.dataset.accountId = String(item.id);
  menu.innerHTML = `
    <button type="button" role="menuitem" data-toggle-account="${item.id}" data-resume-account="${shouldResume}"><i data-lucide="${shouldResume ? "play" : "pause"}"></i><span>${shouldResume ? "恢复调度" : "暂停调度"}</span></button>
    <button type="button" role="menuitem" data-edit-account="${item.id}"><i data-lucide="square-pen"></i><span>编辑账号</span></button>
    <button type="button" role="menuitem" class="danger" data-delete-account="${item.id}"><i data-lucide="trash-2"></i><span>删除账号</span></button>`;
  menu.hidden = false;
  trigger.setAttribute("aria-expanded", "true");
  refreshIcons();
  const rect = trigger.getBoundingClientRect();
  const margin = 8;
  const menuRect = menu.getBoundingClientRect();
  const left = Math.min(
    Math.max(margin, rect.right - menuRect.width),
    window.innerWidth - menuRect.width - margin,
  );
  const top =
    rect.bottom + margin + menuRect.height <= window.innerHeight
      ? rect.bottom + 5
      : Math.max(margin, rect.top - menuRect.height - 5);
  menu.style.left = `${left}px`;
  menu.style.top = `${top}px`;
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
  closeAccountActionMenu();
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
    limited_5h: scoped.filter((item) => accountMatchesStatus(item, "limited_5h"))
      .length,
    limited_7d: scoped.filter((item) => accountMatchesStatus(item, "limited_7d"))
      .length,
    error: scoped.filter((item) => item.dispatch_status === "error").length,
  };
  $("#status-all-count").textContent = scoped.length;
  $("#status-normal-count").textContent = counts.normal;
  $("#status-unavailable-count").textContent = counts.unavailable;
  $("#status-limited-5h-count").textContent = counts.limited_5h;
  $("#status-limited-7d-count").textContent = counts.limited_7d;
  $("#status-error-count").textContent = counts.error;
  $$("#account-status-tabs button").forEach((node) => {
    node.classList.toggle(
      "active",
      node.dataset.accountStatus === state.accountStatus,
    );
  });
  const filtered = scoped.filter((item) =>
    accountMatchesStatus(item, state.accountStatus),
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
        ? `<span class="row-actions account-primary-actions"><button data-auth-account="${item.id}" class="${item.auth_status === "reauth_required" ? "attention" : ""}" title="更新授权" aria-label="更新授权"><i data-lucide="key-round"></i></button><button data-refresh-quota="${item.id}" title="测试并刷新账号配额" aria-label="测试并刷新账号配额"><i data-lucide="activity"></i></button><button data-account-menu="${item.id}" title="更多操作" aria-label="更多操作" aria-haspopup="menu" aria-expanded="false"><i data-lucide="ellipsis"></i></button></span>`
        : '<span class="muted">只读</span>';
      const authDetail = item.auth_error || authText;
      const checked = item.auth_checked_at
        ? ` · 检测 ${dateTime(item.auth_checked_at)}`
        : " · 尚未检测";
      const proxyHint = item.proxy_hint || "未绑定代理";
      const statusDetail = `${authDetail}${checked}`;
      const onboardedAt = dateTime(item.onboarded_at);
      const lastUsedAt = dateTime(item.last_used_at);
      const subscription = accountSubscriptionName(item);
      const subscriptionTitle = item.rate_limit_tier
        ? `${subscription} · ${item.rate_limit_tier}`
        : subscription;
      return `<tr><td class="select-column admin-only-column"><input type="checkbox" data-account-select="${item.id}" aria-label="选择 ${escapeHTML(item.name)}" ${state.selectedAccountIDs.has(item.id) ? "checked" : ""} ${isAdmin() ? "" : "disabled"} /></td><td><span class="row-title" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</span><span class="row-subtitle account-meta">${groups}<span class="mono" title="${escapeHTML(proxyHint)}">${escapeHTML(proxyHint)}</span></span></td><td><span class="pill ${statusClass}">${statusText}</span><span class="row-subtitle" title="${escapeHTML(statusDetail)}">${escapeHTML(statusDetail)}</span></td><td><span class="subscription-badge" title="${escapeHTML(subscriptionTitle)}">${escapeHTML(subscription)}</span></td><td class="num money-cell">${money(item.account_price)}</td><td class="num money-cell emphasis">${money(item.total_billed_cost)}</td><td>${accountUsageCell(item)}</td><td class="num mono request-count">${Number(item.request_count).toLocaleString("zh-CN")}</td><td class="mono time-cell" title="${escapeHTML(onboardedAt)}">${onboardedAt}</td><td>${survivalCell(item)}</td><td class="mono time-cell" title="${escapeHTML(lastUsedAt)}">${lastUsedAt}</td><td class="actions admin-only-column">${actions}</td></tr>`;
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
      accountMatchesStatus(item, state.accountStatus),
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
  $("#edit-selected-accounts").disabled =
    !isAdmin() || state.selectedAccountIDs.size === 0;
  $("#schedule-selected-accounts").disabled =
    !isAdmin() || state.selectedAccountIDs.size === 0;
  $("#pause-selected-accounts").disabled =
    !isAdmin() || state.selectedAccountIDs.size === 0;
}

function renderDeadAccounts() {
  const pending = state.accounts.filter(
    (item) => item.dispatch_status === "error",
  );
  const archived = state.archivedAccounts;
  const source = state.deadStatus === "archived" ? archived : pending;
  const dead = source.filter(deadAccountMatchesSearch);
  $("#nav-dead-count").textContent = pending.length;
  $("#dead-pending-count").textContent = pending.length;
  $("#dead-archived-count").textContent = archived.length;
  $("#dead-list-count").textContent = state.deadSearch.trim()
    ? `${dead.length} / ${source.length} 个账号`
    : `${dead.length} 个账号`;
  $("#dead-table-title").textContent = state.deadStatus === "archived" ? "归档账户" : "死亡账户";
  $("#dead-empty").hidden = dead.length > 0;
  $("#dead-empty strong").textContent = state.deadSearch.trim()
    ? "没有匹配账户"
    : state.deadStatus === "archived"
      ? "暂无归档账户"
      : "暂无死亡账户";
  $("#dead-empty span").textContent = state.deadSearch.trim()
    ? "请调整账号、邮箱或代理 IP 搜索条件。"
    : state.deadStatus === "archived"
      ? "归档后的死亡账户会保留在这里。"
      : "授权失效或状态错误的账号会显示在这里。";
  const average =
    dead.length > 0
      ? dead.reduce((sum, item) => sum + item.survival_seconds, 0) / dead.length
      : 0;
  $("#dead-metrics").innerHTML = [
    metric(state.deadStatus === "archived" ? "ARCHIVED ACCOUNTS" : "DEAD ACCOUNTS", dead.length, state.deadStatus === "archived" ? "已释放代理 IP" : "授权失效或账号错误", "b"),
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
        ? state.deadStatus === "archived"
          ? `<span class="row-actions"><button data-restore-account="${item.id}" title="移出归档"><i data-lucide="archive-restore"></i></button></span>`
          : `<span class="row-actions"><button data-auth-account="${item.id}" class="attention" title="重新授权"><i data-lucide="key-round"></i></button><button data-edit-account="${item.id}" title="编辑账号"><i data-lucide="square-pen"></i></button><button data-archive-account="${item.id}" title="归档并释放 IP"><i data-lucide="archive"></i></button></span>`
        : '<span class="muted">只读</span>';
      const selectable = isAdmin() && state.deadStatus === "pending";
      const proxyHint = item.proxy_hint || "—";
      const proxyDetail =
        item.proxy_ip && !proxyHint.includes(item.proxy_ip)
          ? `${proxyHint} · ${item.proxy_ip}`
          : proxyHint;
      return `<tr><td class="select-column admin-only-column">${selectable ? `<input type="checkbox" data-dead-select="${item.id}" aria-label="选择 ${escapeHTML(item.name)}" ${state.selectedDeadAccountIDs.has(item.id) ? "checked" : ""} />` : "—"}</td><td><span class="row-title" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</span><span class="row-subtitle">${escapeHTML(item.credential_hint)}</span></td><td>${escapeHTML(accountSubscriptionName(item))}</td><td><span class="row-title" title="${escapeHTML(item.proxy_name || "未绑定")}">${escapeHTML(item.proxy_name || "未绑定")}</span><span class="row-subtitle mono" title="${escapeHTML(proxyDetail)}">${escapeHTML(proxyDetail)}</span></td><td class="mono time-cell">${dateTime(item.onboarded_at)}</td><td class="mono time-cell">${dateTime(item.invalidated_at)}</td><td class="mono time-cell">${dateTime(item.archived_at)}</td><td>${survivalCell(item)}</td><td class="num mono">${Number(item.request_count).toLocaleString("zh-CN")}</td><td class="num money-cell">${money(item.total_billed_cost)}</td><td class="error-copy" title="${escapeHTML(item.auth_error || item.error_message || "账号状态错误")}">${escapeHTML(item.auth_error || item.error_message || "账号状态错误")}</td><td class="actions">${actions}</td></tr>`;
    })
    .join("");
  syncDeadSelection();
  refreshIcons();
}

function deadAccountMatchesSearch(item) {
  const search = state.deadSearch.trim().toLowerCase();
  if (!search) return true;
  return [
    item.id,
    item.name,
    item.credential_hint,
    item.source_sk_hint,
    item.proxy_name,
    item.proxy_hint,
    item.proxy_ip,
  ]
    .filter((value) => value !== null && value !== undefined)
    .join(" ")
    .toLowerCase()
    .includes(search);
}

function syncDeadSelection() {
  const pending = state.accounts.filter(
    (item) => item.dispatch_status === "error" && deadAccountMatchesSearch(item),
  );
  const selected = pending.filter((item) => state.selectedDeadAccountIDs.has(item.id));
  $("#selected-dead-count").textContent = selected.length;
  const archiveButton = $("#archive-selected-accounts");
  archiveButton.hidden = state.deadStatus !== "pending";
  archiveButton.disabled = !isAdmin() || state.deadStatus !== "pending" || selected.length === 0;
  const selectAll = $("#select-all-dead");
  selectAll.hidden = state.deadStatus !== "pending";
  selectAll.disabled = !isAdmin() || state.deadStatus !== "pending" || pending.length === 0;
  selectAll.checked = pending.length > 0 && selected.length === pending.length;
  selectAll.indeterminate = selected.length > 0 && selected.length < pending.length;
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

const deauthWindowLabels = {
  15: "15 分钟",
  60: "1 小时",
  360: "6 小时",
  1440: "24 小时",
  10080: "7 天",
};

async function loadDeauthMonitor() {
  if (!canView("authorization")) return;
  try {
    state.deauth = await api(
      `/api/authorization-deauth?window=${state.deauthWindow}`,
    );
    renderDeauthMonitor();
  } catch (error) {
    toast(error.message, "error");
  }
}

function renderDeauthMonitor() {
  const data = state.deauth;
  if (!data) return;
  const label = deauthWindowLabels[data.window_minutes] || `${data.window_minutes} 分钟`;
  const still401 = data.accounts_401 - data.recovered_401;
  $("#deauth-metrics").innerHTML = [
    metric(
      "401 DE-AUTH",
      data.accounts_401,
      `近 ${label} 因 401 掉授权的账号数`,
      data.accounts_401 > 0 ? "b" : "",
    ),
    metric("STILL DOWN", still401, `其中仍未恢复 · 已恢复 ${data.recovered_401}`),
    metric("ALL CAUSES", data.total, `近 ${label} 全部掉授权次数`),
    metric(
      "PENDING REAUTH",
      data.pending_reauth,
      "当前等待重新授权的账号总数",
      "a",
    ),
  ].join("");
  $("#deauth-causes").innerHTML = data.causes
    .map(
      (item) =>
        `<span class="deauth-cause ${item.count > 0 ? "active" : ""}"><b>${item.count}</b>${escapeHTML(item.label)}</span>`,
    )
    .join("");
  $("#deauth-body").innerHTML = data.events.length
    ? data.events
        .map(
          (item) =>
            `<tr><td class="mono">${dateTime(item.at)}</td><td><span class="row-title">${escapeHTML(item.name)}</span><span class="row-subtitle">#${item.account_id}</span></td><td>${(item.group_ids ? item.group_ids.split(",") : []).map((id) => groupMark(id, "pill")).join("") || "—"}</td><td><span class="pill ${item.cause === "oauth_401" ? "error" : ""}">${escapeHTML(item.label)}</span></td><td><span class="pill ${item.recovered ? "ok" : "error"}">${item.recovered ? "已恢复" : "待授权"}</span></td><td class="error-copy deauth-reason-cell">${escapeHTML(item.reason || "—")}</td></tr>`,
        )
        .join("")
    : `<tr><td colspan="6" class="muted">近 ${escapeHTML(label)} 没有账号掉授权</td></tr>`;
}

function renderErrors() {
  const data = state.errors;
  if (!data) return;
  const summary = data.summary;
  $("#error-count").textContent = `${summary.total} 条记录`;
  $("#error-empty").hidden = data.items.length > 0;
  $("#error-metrics").innerHTML = [
    metric(
      "TOTAL",
      summary.total,
      `当前筛选 · 默认保留 ${data.retention_days || 7} 天`,
    ),
    metric("GATEWAY", summary.gateway, "API 请求失败", "b"),
    metric("ACCOUNTS", summary.accounts, "账号状态异常"),
    metric("AUTHORIZATION", summary.authorization, "授权失败"),
    metric(
      "OTHER",
      summary.proxies + summary.system + summary.audit,
      "代理、系统与后台操作",
    ),
  ].join("");
  const sourceNames = {
    account: "账号",
    authorization: "授权",
    gateway: "API",
    proxy: "代理",
    audit: "操作",
    system: "系统",
  };
  const categoryNames = {
    account_auth: "授权 / Token",
    account_state: "账号状态",
    authorization: "授权流程",
    gateway_request: "API 请求",
    gateway_capacity: "调度容量不足",
    upstream_bad_request: "上游参数错误",
    upstream_authentication_rejected: "上游认证拒绝",
    upstream_authentication_revoked: "上游 Token 撤销",
    upstream_forbidden: "上游禁止访问",
    upstream_forbidden_proxy_challenge: "代理出口验证",
    upstream_forbidden_identity_verification: "身份验证要求",
    upstream_forbidden_oauth_policy: "组织 OAuth 策略",
    upstream_rate_limited: "上游限流 / 配额",
    upstream_overloaded: "上游过载",
    upstream_service_error: "上游服务错误",
    upstream_error: "上游错误",
    client_canceled: "调用方取消",
    timeout: "请求超时",
    proxy_test: "代理检测",
    proxy_pool_sync: "代理池同步",
    admin_request: "后台请求",
    pricing_sync: "价格同步",
  };
  $("#error-body").innerHTML = paginatedItems("errors", data.items)
    .map((item) => {
      const target =
        item.account_name ||
        (item.source === "audit" ? item.actor : "") ||
        "系统";
      const targetNote = item.account_id
        ? `#${item.account_id}`
        : item.source === "audit" && item.actor
          ? "操作用户"
          : "—";
      const context = [item.method, item.path].filter(Boolean).join(" ");
      const category = categoryNames[item.category] || item.category;
      const contextTitle = context || category;
      const elapsed = item.duration_ms
        ? item.duration_ms < 1000
          ? `${item.duration_ms} ms`
          : `${(item.duration_ms / 1000).toFixed(2)} s`
        : "";
      const requestTrace = [
        item.client_request_id
          ? `NewAPI ${item.client_request_id}`
          : item.request_id
            ? `请求 ${item.request_id}`
            : "",
        item.trace_id ? `CCMAX ${item.trace_id}` : "",
      ]
        .filter(Boolean)
        .join(" · ");
      const dispatchNote = dispatchDiagnosticsText(item.dispatch_diagnostics);
      const contextNote = [
        context ? category : "",
        elapsed,
        dispatchNote,
        requestTrace,
      ]
        .filter(Boolean)
        .join(" · ") || "—";
      const contextTooltip = [
        contextTitle,
        contextNote,
        item.upstream_request_id
          ? `上游 ${item.upstream_request_id}`
          : "",
      ]
        .filter(Boolean)
        .join("\n");
      const groups = item.group_ids?.length
        ? item.group_ids.map((groupID) => groupMark(groupID, "pill")).join(" ")
        : '<span class="muted">—</span>';
      const status = item.status_code
        ? `<span class="pill error">HTTP ${item.status_code}</span>`
        : '<span class="pill error">错误</span>';
      return `<tr><td class="mono">${dateTime(item.created_at)}</td><td><span class="pill error-source ${escapeHTML(item.source)}">${escapeHTML(sourceNames[item.source] || item.source)}</span></td><td>${status}</td><td><span class="row-title truncate-cell" title="${escapeHTML(target)}">${escapeHTML(target)}</span><span class="row-subtitle">${escapeHTML(targetNote)}</span></td><td>${groups}</td><td class="mono truncate-cell" title="${escapeHTML(item.proxy_ip || "")}">${escapeHTML(item.proxy_ip || "—")}</td><td title="${escapeHTML(contextTooltip)}"><span class="row-title truncate-cell">${escapeHTML(contextTitle)}</span><span class="row-subtitle">${escapeHTML(contextNote)}</span></td><td class="error-message-cell" title="${escapeHTML(item.message)}">${escapeHTML(item.message)}</td></tr>`;
    })
    .join("");
}

function dispatchDiagnosticsText(value) {
  if (!value) return "";
  try {
    const item = typeof value === "string" ? JSON.parse(value) : value;
    return [
      `候选 ${Number(item.candidates || 0)}`,
      item.excluded ? `重试排除 ${item.excluded}` : "",
      item.strategy_missing ? `策略未绑定 ${item.strategy_missing}` : "",
      item.model_cooldown ? `模型冷却 ${item.model_cooldown}` : "",
      item.model_unsupported ? `模型不支持 ${item.model_unsupported}` : "",
      item.concurrency_blocked ? `并发阻塞 ${item.concurrency_blocked}` : "",
      item.temporary_rpm_blocked
        ? `临时 RPM ${item.temporary_rpm_blocked}`
        : "",
      item.rpm_blocked ? `RPM 阻塞 ${item.rpm_blocked}` : "",
      item.tpm_blocked ? `TPM 阻塞 ${item.tpm_blocked}` : "",
    ]
      .filter(Boolean)
      .join(" / ");
  } catch {
    return "";
  }
}

function renderProxies() {
  $("#nav-proxy-count").textContent = state.proxies.length;
  $("#proxy-pool-list").innerHTML = state.proxyPools
    .map(
      (pool) =>
        `<article class="pool-row ${String(pool.id) === String(state.proxyPoolFilter) ? "selected" : ""}" data-select-pool="${pool.id}"><div><strong>${escapeHTML(pool.name)}</strong><small>${pool.source_type === "api" ? "API 自动同步" : "手动维护"} · ${pool.available_count}/${pool.proxy_count} 可用</small></div><div class="pool-meter"><span style="width:${pool.proxy_count ? (pool.available_count / pool.proxy_count) * 100 : 0}%"></span></div><div class="pool-meta"><span>${pool.assigned_count} 个账号占用</span><span>${pool.last_sync_at ? dateTime(pool.last_sync_at) : "未同步"}</span></div><div class="row-actions">${isAdmin() && pool.source_type === "api" ? `<button data-sync-pool="${pool.id}" title="同步 API">↻</button>` : ""}${isAdmin() ? `<button data-edit-pool="${pool.id}" title="编辑代理池">✎</button><button class="danger" data-delete-pool="${pool.id}" title="删除代理池">✕</button>` : ""}</div></article>`,
    )
    .join("");
  const selected = filteredProxies();
  const visibleIDs = new Set(selected.map((item) => item.id));
  state.selectedProxyIDs = new Set(
    [...state.selectedProxyIDs].filter((id) => visibleIDs.has(id)),
  );
  $("#proxies-empty").hidden = selected.length > 0;
  $("#proxies-body").innerHTML = paginatedItems("proxies", selected)
    .map(
      (item) =>
        `<tr>
          <td class="select-column admin-only-column"><input type="checkbox" data-proxy-select="${item.id}" aria-label="选择 ${escapeHTML(item.name)}" ${state.selectedProxyIDs.has(item.id) ? "checked" : ""} ${isAdmin() ? "" : "disabled"} /></td>
          <td><span class="row-title" title="${escapeHTML(item.name)}">${escapeHTML(item.name)}</span><span class="row-subtitle mono" title="${escapeHTML(item.host)}:${item.port}">${escapeHTML(item.host)}:${item.port}</span></td>
          <td><span class="pill">${item.protocol.toUpperCase()}</span></td>
          <td class="mono" title="${escapeHTML(item.exit_ip || "未检测")}">${escapeHTML(item.exit_ip || "未检测")}</td>
          <td class="num mono">${item.latency_ms == null ? "—" : `${item.latency_ms} ms`}</td>
          <td title="${escapeHTML(item.assigned_to || "未占用")}">${escapeHTML(item.assigned_to || "未占用")}</td>
          <td class="num mono" title="历史绑定过该 IP 的不同账号数">${Number(item.used_account_count || 0).toLocaleString("zh-CN")}</td>
          <td><span class="pill ${item.status === "active" ? "ok" : item.status === "error" ? "error" : "off"}">${item.status === "active" ? "正常" : item.status === "error" ? "异常" : "停用"}</span></td>
          <td class="mono" title="${escapeHTML(dateTime(item.last_test_at))}">${dateTime(item.last_test_at)}</td>
          <td class="actions">${isAdmin() ? `<span class="row-actions"><button data-test-proxy="${item.id}" title="检测代理"><i data-lucide="activity"></i></button><button class="danger" data-delete-proxy="${item.id}" title="删除代理"><i data-lucide="trash-2"></i></button></span>` : '<span class="muted">只读</span>'}</td>
        </tr>`,
    )
    .join("");
  const options = `<option value="">全部代理池</option>${state.proxyPools.map((pool) => `<option value="${pool.id}">${escapeHTML(pool.name)}</option>`).join("")}`;
  $("#proxy-pool-filter").innerHTML = options;
  $("#proxy-pool-filter").value = state.proxyPoolFilter;
  syncProxySelection(selected);
  refreshIcons();
}

function filteredProxies() {
  const search = state.proxySearch.trim().toLowerCase();
  return state.proxies.filter((item) => {
    if (
      state.proxyPoolFilter &&
      String(item.pool_id) !== String(state.proxyPoolFilter)
    )
      return false;
    if (!search) return true;
    return [
      item.id,
      item.name,
      item.pool_name,
      item.protocol,
      item.host,
      item.port,
      item.exit_ip,
      item.assigned_to,
      item.status,
    ]
      .filter((value) => value !== null && value !== undefined)
      .join(" ")
      .toLowerCase()
      .includes(search);
  });
}

function syncProxySelection(scope = filteredProxies()) {
  const selectable = isAdmin() ? scope : [];
  const selected = selectable.filter((item) => state.selectedProxyIDs.has(item.id));
  const selectAll = $("#select-all-proxies");
  selectAll.disabled = !isAdmin() || selectable.length === 0;
  selectAll.checked = selectable.length > 0 && selected.length === selectable.length;
  selectAll.indeterminate = selected.length > 0 && selected.length < selectable.length;
  $("#selected-proxy-count").textContent = selected.length;
  $("#delete-proxies-batch").disabled = !isAdmin() || selected.length === 0;
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
          `<tr><td><span class="row-title">${escapeHTML(item.name || item.username)}</span><span class="row-subtitle mono">${escapeHTML(item.username)}</span></td><td><span class="pill">${roleName(item.role)}</span></td><td><div class="group-pills">${item.allowed_group_ids.map((id) => groupMark(id, "pill")).join("")}</div></td><td class="num mono">${item.balance == null ? "不限" : money(item.balance)}</td><td class="num mono">${item.rpm_limit || "∞"}</td><td><span class="pill ${item.status === "active" ? "ok" : "off"}">${item.status === "active" ? "启用" : "停用"}</span></td><td class="mono">${dateTime(item.created_at)}</td><td class="actions"><span class="row-actions"><button data-edit-user="${item.id}" title="编辑用户"><i data-lucide="square-pen"></i></button>${item.id === state.me.id ? "" : `<button class="danger" data-delete-user="${item.id}" title="删除用户"><i data-lucide="trash-2"></i></button>`}</span></td></tr>`,
      )
      .join("");
  $("#keys-empty").hidden = state.keys.length > 0;
  $("#keys-body").innerHTML = paginatedItems("keys", state.keys)
    .map(
      (item) =>
        `<tr><td><span class="row-title">${escapeHTML(item.name)}</span><span class="row-subtitle mono">${escapeHTML(item.key_prefix)}••••••••</span></td><td>${escapeHTML(item.username)}</td><td>${groupMark(item.group_id, "pill")}</td><td class="num mono">${money(item.quota_used)} / ${item.quota > 0 ? money(item.quota) : "∞"}</td><td class="mono">${dateTime(item.expires_at)}</td><td class="mono">${dateTime(item.last_used_at)}</td><td><span class="pill ${item.status === "active" ? "ok" : "off"}">${item.status === "active" ? "启用" : "停用"}</span></td><td class="actions">${canManageKeys ? `<span class="row-actions">${item.key ? `<button data-copy-key="${item.id}" title="复制 SK"><i data-lucide="copy"></i></button>` : ""}<button data-toggle-key="${item.id}" title="${item.status === "active" ? "禁用 API Key" : "启用 API Key"}"><i data-lucide="power"></i></button><button data-edit-key="${item.id}" title="编辑 API Key"><i data-lucide="square-pen"></i></button><button class="danger" data-delete-key="${item.id}" title="删除 API Key"><i data-lucide="trash-2"></i></button></span>` : '<span class="muted">只读</span>'}</td></tr>`,
    )
    .join("");
  $("#gateway-endpoint").textContent = `${location.origin}/v1/messages`;
  const balanceSummary = $("#user-balance-summary");
  balanceSummary.hidden = state.me.role !== "user";
  if (!balanceSummary.hidden)
    balanceSummary.textContent = `可支配余额 ${state.me.balance == null ? "不限" : money(state.me.balance)}`;
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
  const userMetrics = [
    metric(
      "BILLED",
      money(data.totals.billed_cost),
      `${data.totals.requests} 次请求`,
      "a",
    ),
    metric(
      "AVAILABLE",
      data.available_balance == null ? "不限" : money(data.available_balance),
      "可支配余额",
      "b",
    ),
    metric("REQUESTS", compact(data.totals.requests), "当前筛选区间"),
    metric(
      "TOKENS",
      compact(
        data.totals.input_tokens +
          data.totals.output_tokens +
          data.totals.cache_tokens,
      ),
      `输入 ${compact(data.totals.input_tokens)} / 输出 ${compact(data.totals.output_tokens)}`,
    ),
  ];
  const managerMetrics = [
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
  ];
  $("#billing-metrics").innerHTML = (
    state.me.role === "user" ? userMetrics : managerMetrics
  ).join("");
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
    key: state.billing.by_api_key,
    purpose: state.billing.by_purpose,
  };
  const rows = map[state.breakdown] || [];
  const maxValue = Math.max(...rows.map((item) => item.billed_cost), 0);
  $("#breakdown-list").innerHTML = rows.length
    ? rows
        .map(
          (item) =>
            `<div class="breakdown-row"><div><strong>${escapeHTML(state.breakdown === "group" ? `${item.key.toUpperCase()} 分组` : item.name)}</strong><small>${item.requests} 次请求</small></div><div class="breakdown-bar"><span style="width:${maxValue ? Math.max((item.billed_cost / maxValue) * 100, 2) : 0}%"></span></div><div class="breakdown-values"><strong>${money(item.billed_cost)}</strong>${state.me.role === "user" ? "" : `<small>成本 ${money(item.actual_cost)}</small>`}</div></div>`,
        )
        .join("")
    : '<div class="empty-state"><strong>暂无拆分数据</strong></div>';
}
function usageAccountSKCell(item) {
  if (!item.account_sk_hint)
    return '<span class="muted" title="OAuth 授权或该流水产生时尚未保存账号授权 SK 的脱敏标识">未记录</span>';
  return `<span class="row-title mono" title="${escapeHTML(item.account_sk_hint)}">${escapeHTML(item.account_sk_hint)}</span>`;
}
function usageRows(items, compactMode) {
  if (!items?.length)
    return compactMode
      ? '<tr><td colspan="8"><div class="empty-state"><strong>暂无计费流水</strong></div></td></tr>'
      : "";
  return items
    .map((item) => {
      const total =
        item.input_tokens +
        item.output_tokens +
        item.cache_creation_tokens +
        item.cache_read_tokens;
      if (compactMode)
        return `<tr><td class="mono">${dateTime(item.created_at)}</td><td><span class="row-title" title="${escapeHTML(item.purpose_name)}">${escapeHTML(item.purpose_name)}</span>${groupMark(item.group_id, "pill")}</td><td>${usageAccountSKCell(item)}</td><td title="${escapeHTML(item.account_name)}">${escapeHTML(item.account_name)}</td><td class="mono" title="${escapeHTML(item.model)}">${escapeHTML(item.model)}</td><td class="num mono">${compact(total)}</td><td class="num mono">${money(item.billed_cost)}</td><td class="num mono internal-cost-column">${money(item.actual_cost)}</td></tr>`;
      const requestTooltip = [
        item.client_request_id
          ? `NewAPI: ${item.client_request_id}`
          : `请求: ${item.request_id}`,
        item.trace_id ? `CCMAX: ${item.trace_id}` : "",
        item.upstream_request_id ? `上游: ${item.upstream_request_id}` : "",
      ]
        .filter(Boolean)
        .join("\n");
      const requestNote = [
        dateTime(item.created_at),
        item.trace_id ? `CCMAX ${item.trace_id}` : "",
      ]
        .filter(Boolean)
        .join(" · ");
      const displayedRequestID = item.client_request_id || item.request_id;
      return `<tr><td title="${escapeHTML(requestTooltip)}"><span class="mono row-title">${escapeHTML(displayedRequestID)}</span><span class="row-subtitle mono">${escapeHTML(requestNote)}</span></td><td><span class="row-title" title="${escapeHTML(item.purpose_name)}">${escapeHTML(item.purpose_name)}</span>${groupMark(item.group_id, "pill")}</td><td>${usageAccountSKCell(item)}</td><td title="${escapeHTML(item.account_name)}">${escapeHTML(item.account_name)}</td><td class="mono" title="${escapeHTML(item.model)}">${escapeHTML(item.model)}</td><td class="num mono">${compact(item.input_tokens)}</td><td class="num mono">${compact(item.output_tokens)}</td><td class="num mono">${compact(item.cache_creation_tokens + item.cache_read_tokens)}</td><td class="num mono">${money(item.billed_cost)}</td><td class="num mono internal-cost-column">${money(item.actual_cost)}</td><td class="num mono internal-cost-column">${money(item.billed_cost - item.actual_cost)}</td></tr>`;
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
        `<tr><td class="mono">${item.index}</td><td><span class="row-title">${escapeHTML(item.name || "未创建")}</span>${item.account_id ? `<span class="row-subtitle mono">#${item.account_id}</span>` : ""}</td><td>${escapeHTML(subscriptionName(item.subscription_type))}</td><td class="mono">${escapeHTML(item.proxy_ip || "—")}</td><td><span class="pill ${item.skipped ? "off" : item.success ? "ok" : "error"}">${item.updated ? "已更新" : item.skipped ? "已跳过" : item.success ? "成功" : "失败"}</span></td><td class="${item.success || item.skipped ? "" : "error-copy"}">${escapeHTML(item.error || "授权完成")}</td></tr>`,
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
  const keyFilter = $("#billing-api-key");
  const selectedKey = keyFilter.value;
  keyFilter.innerHTML = `<option value="">全部调用 Key</option>${state.keys
    .map(
      (item) =>
        `<option value="${item.id}">${escapeHTML(item.name)}（${escapeHTML(item.username)}）</option>`,
    )
    .join("")}`;
  keyFilter.value = state.keys.some(
    (item) => String(item.id) === String(selectedKey),
  )
    ? selectedKey
    : "";
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
  $("#proxy-import-pool").innerHTML =
    `<option value="">选择目标代理池</option>${poolOptions}`;
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
  if (view === "accounts") {
    loadAccountSummary();
    loadRealtime();
  }
  if (view === "daily") loadDaily();
  if (view === "authorization") {
    loadAuthorization();
    loadDeauthMonitor();
  }
  if (view === "errors") loadErrors();
  if (view === "strategies") loadStrategies();
  if (view === "onboarding")
    ensureStrategiesLoaded().then(() =>
      fillStrategySelect($("#batch-strategy"), $("#batch-strategy").value),
    );
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
  $("#account-concurrency").value = account?.concurrency ?? 10;
  $("#account-priority").value = account?.priority ?? 50;
  $("#account-rate").value = account?.rate_multiplier ?? 1;
  $("#account-price").value = account?.account_price ?? 0;
  $("#account-proxy-pool").value = account?.proxy_pool_id || "";
  $("#account-auto-proxy").checked = account?.auto_proxy || false;
  $("#account-proxy-text").value = "";
  fillProxyOptions(account?.proxy_pool_id || "", account?.proxy_id || "");
  $("#account-base-rpm").value = account?.base_rpm || 15;
  $("#account-rpm-enabled").checked = (account?.base_rpm || 0) > 0;
  $("#account-rpm-strategy").value = account?.rpm_strategy || "tiered";
  $("#account-rpm-buffer").value = account?.rpm_sticky_buffer || 0;
  fillStrategySelect($("#account-strategy"), account?.strategy_id);
  ensureStrategiesLoaded().then(() =>
    fillStrategySelect($("#account-strategy"), account?.strategy_id),
  );
  $("#account-queue-mode").value = account?.user_msg_queue_mode || "off";
  $("#account-request-passthrough").checked =
    account?.extra?.request_passthrough === true;
  $("#account-mcp-tool-names").value =
    typeof account?.extra?.mcp_tool_names === "boolean"
      ? account.extra.mcp_tool_names
        ? "on"
        : "off"
      : "";
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
      : node.value === availableGroups(true)[0]?.id;
  });
  $("#credential-help").textContent = account?.has_credentials
    ? `当前凭证：${account.credential_hint}；留空保持不变。`
    : "保存后凭证不会在管理列表中返回。";
  syncAccountControls();
  showInitializedDialog("#account-dialog");
}
function strategyName(id) {
  if (!id) return "跟随分组";
  const item = (state.strategies || []).find((entry) => entry.id === id);
  return item ? item.name : `#${id}`;
}
function batchEditCommonValue(accounts, key, normalize = (value) => value) {
  const values = accounts.map((account) => normalize(account[key]));
  const first = values[0];
  return {
    value: first,
    mixed: values.some((value) => JSON.stringify(value) !== JSON.stringify(first)),
  };
}
function openBatchAccountEdit() {
  const accounts = selectedAccountIDs()
    .map((id) => state.accounts.find((item) => item.id === id))
    .filter(Boolean);
  if (!accounts.length) return;
  $("#batch-account-form").reset();
  $("#batch-edit-count").textContent = `已选择 ${accounts.length} 个账号`;
  const fields = [
    ["concurrency", "#batch-edit-concurrency"],
    ["base_rpm", "#batch-edit-base-rpm"],
    ["rpm_strategy", "#batch-edit-rpm-strategy"],
    ["rpm_sticky_buffer", "#batch-edit-rpm-buffer"],
    ["user_msg_queue_mode", "#batch-edit-queue-mode"],
    ["priority", "#batch-edit-priority"],
    ["rate_multiplier", "#batch-edit-rate"],
    ["account_price", "#batch-edit-price"],
  ];
  fields.forEach(([key, selector]) => {
    const current = batchEditCommonValue(accounts, key);
    $(selector).value = current.value ?? "";
    $(`[data-batch-edit-hint="${key}"]`).textContent = current.mixed
      ? "已选账号当前值不一致"
      : `当前值：${current.value ?? "—"}`;
  });
  const strategy = batchEditCommonValue(
    accounts,
    "strategy_id",
    (value) => value || 0,
  );
  const showStrategy = () => {
    fillStrategySelect(
      $("#batch-edit-strategy"),
      strategy.mixed ? 0 : strategy.value,
    );
    $('[data-batch-edit-hint="strategy_id"]').textContent = strategy.mixed
      ? "已选账号当前策略不一致"
      : `当前策略：${strategyName(strategy.value)}`;
  };
  showStrategy();
  ensureStrategiesLoaded().then(showStrategy);
  const groups = batchEditCommonValue(accounts, "group_ids", (value) =>
    [...(value || [])].sort(),
  );
  $$('.batch-edit-value[name="batch-edit-group"]').forEach((node) => {
    node.checked = groups.value.includes(node.value);
  });
  $('[data-batch-edit-hint="group_ids"]').textContent = groups.mixed
    ? "已选账号当前分组不一致"
    : `当前分组：${groups.value.map((value) => value.toUpperCase()).join("、")}`;
  syncBatchEditControls();
  showInitializedDialog("#batch-account-dialog");
}
function syncBatchEditControls() {
  $$('[data-batch-edit-apply]').forEach((apply) => {
    const container =
      apply.dataset.batchEditApply === "group_ids"
        ? apply.closest("fieldset")
        : apply.closest(".batch-edit-field");
    $$(".batch-edit-value", container).forEach((node) => {
      node.disabled = !apply.checked;
    });
  });
}
function syncAccountControls() {
  const hasPool = Boolean($("#account-proxy-pool").value);
  const hasManualProxy = Boolean($("#account-proxy-text").value.trim());
  const schedulable = $("#account-schedulable");
  schedulable.disabled = !hasPool;
  if (!hasPool) schedulable.checked = false;
  $("#account-auto-proxy").disabled = !hasPool || hasManualProxy;
  $("#account-proxy").disabled =
    !hasPool || hasManualProxy || $("#account-auto-proxy").checked;
  $("#account-base-rpm").disabled = !$("#account-rpm-enabled").checked;
  $("#account-rpm-strategy").disabled = !$("#account-rpm-enabled").checked;
  $("#account-rpm-buffer").disabled = !$("#account-rpm-enabled").checked;
  $("#account-rpm-buffer-hint").textContent =
    $("#account-rpm-strategy").value === "fixed"
      ? "固定硬限：n = 粘性会话豁免额度，0 表示完全硬限不豁免"
      : "0 使用并发数与基础 RPM 自动计算";
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
  const fallbackGroup = availableGroups(true).find(
    (group) => !group.reserve_pool_enabled,
  )?.id || "";
  const selectedGroup = item?.active_group_id || fallbackGroup;
  $("#purpose-group").innerHTML = state.groups
    .filter(
      (group) =>
        !group.reserve_pool_enabled &&
        (group.status === "active" || group.id === selectedGroup),
    )
    .map((group) => groupOption(group, selectedGroup))
    .join("");
  $("#purpose-group").value = selectedGroup;
  $("#purpose-description").value = item?.description || "";
  showInitializedDialog("#purpose-dialog");
}
async function switchPurposeGroup(purposeID, groupID) {
  const item = state.purposes.find((value) => value.id === Number(purposeID));
  if (!item || item.active_group_id === groupID) return;
  await api(`/api/purposes/${item.id}`, {
    method: "PUT",
    body: JSON.stringify({
      key: item.key,
      name: item.name,
      description: item.description,
      active_group_id: groupID,
    }),
  });
  const group = state.groups.find((value) => value.id === groupID);
  toast(`${item.name} 已切换到 ${group?.name || groupID}`);
  await loadCore();
}
function syncGroupRateLimitWaitFields() {
  $("#group-rate-limit-wait-seconds").disabled = !$(
    "#group-rate-limit-wait-enabled",
  ).checked;
}
function openGroup(item = null) {
  $("#group-form").reset();
  $("#group-id").value = item?.id || "";
  $("#group-dialog-title").textContent = item ? "编辑分组" : "新增分组";
  $("#group-name").value = item?.name || "";
  $("#group-description").value = item?.description || "";
  $("#group-rate").value = item?.rate_multiplier ?? 1;
  $("#group-status").value = item?.status || "active";
  $("#group-daily").value = item?.daily_limit_usd ?? "";
  $("#group-monthly").value = item?.monthly_limit_usd ?? "";
  $("#group-reserve-pool").checked = Boolean(item?.reserve_pool_enabled);
  fillStrategySelect($("#group-strategy"), item?.strategy_id);
  ensureStrategiesLoaded().then(() =>
    fillStrategySelect($("#group-strategy"), item?.strategy_id),
  );
  $("#group-normal-request-mode").checked = Boolean(item?.normal_request_mode);
  $("#group-claude-code-identity").checked = Boolean(
    item?.claude_code_identity_enabled,
  );
  $("#group-reject-anthropic-downgrade").checked = Boolean(
    item?.reject_anthropic_downgrade_enabled,
  );
  $("#group-reject-distillation").checked = Boolean(
    item?.reject_distillation_enabled,
  );
  $("#group-mcp-tool-names").checked = Boolean(item?.mcp_tool_names_enabled);
  $("#group-passthrough-service-tier").checked = Boolean(
    item?.service_tier_passthrough_enabled,
  );
  $("#group-passthrough-inference-geo").checked = Boolean(
    item?.inference_geo_passthrough_enabled,
  );
  $("#group-passthrough-speed").checked = Boolean(
    item?.speed_passthrough_enabled,
  );
  $("#group-passthrough-anthropic-beta").checked = Boolean(
    item?.anthropic_beta_passthrough_enabled,
  );
  $("#group-rpm-dispatch-enabled").checked = Boolean(
    item ? item.rpm_dispatch_enabled : true,
  );
  $("#group-overload-cooldown").value = Number(
    item?.overload_cooldown_seconds || 10,
  );
  $("#group-rate-limit-wait-enabled").checked = Boolean(
    item?.rate_limit_wait_enabled,
  );
  $("#group-rate-limit-wait-seconds").value = Number(
    item?.rate_limit_wait_seconds || 5,
  );
  syncGroupRateLimitWaitFields();
  $("#group-strategy-required").checked = Boolean(item?.strategy_required_enabled);
  $("#group-capacity-queue-enabled").checked = Boolean(
    item?.capacity_queue_enabled,
  );
  $("#group-capacity-queue-timeout").value = Number(
    item?.capacity_queue_timeout_seconds || 30,
  );
  $("#group-stream-hedge-enabled").checked = Boolean(item?.stream_hedge_enabled);
  $("#group-adaptive-hedge-enabled").checked = Boolean(
    item?.adaptive_hedge_enabled,
  );
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
  $("#user-balance").value = item?.balance ?? "";
  $("#user-rpm").value = item?.rpm_limit || 0;
  $("#user-password").required = !item;
  $("#user-password-help").textContent = item
    ? "留空保持原密码；修改后旧会话失效"
    : "至少 8 位";
  $$('input[name="user-group"]').forEach((node) => {
    node.checked = item
      ? item.allowed_group_ids.includes(node.value)
      : node.value === availableGroups(true)[0]?.id;
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
  const allowed = new Set(owner?.allowed_group_ids || []);
  const groups = availableGroups().filter(
    (group) =>
      !group.reserve_pool_enabled &&
      (owner?.role !== "user" || allowed.has(group.id)),
  );
  $("#key-group").innerHTML = groups
    .map((group) => groupOption(group, selected))
    .join("");
  const ids = groups.map((group) => group.id);
  $("#key-group").value = ids.includes(selected) ? selected : ids[0] || "";
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
  syncKeyGroups(
    item?.group_id || state.me.allowed_group_ids?.[0] || state.groups[0]?.id || "",
  );
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
  const selectedPool = state.proxyPools.find(
    (pool) => String(pool.id) === String(state.proxyPoolFilter),
  );
  $("#proxy-import-pool").value = selectedPool ? String(selectedPool.id) : "";
  showInitializedDialog("#proxy-import-dialog");
}
function openAccountAuth(account) {
  state.oauthSessionID = "";
  $("#auth-account-id").value = account.id;
  $("#account-auth-title").textContent = `更新授权 · ${account.name}`;
  $("#auth-session-key").value = "";
  $("#oauth-code").value = "";
  $("#oauth-link").value = "";
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
  if (!target) {
    if (!event.target.closest("#account-action-menu")) closeAccountActionMenu();
    return;
  }
  if (target.dataset.strategyToggle) {
    const strategyID = String(target.dataset.strategyToggle);
    expandedStrategyID =
      expandedStrategyID === strategyID ? "" : strategyID;
    renderStrategyAccordion(state.strategies || []);
    refreshIcons();
    return;
  }
  if (target.dataset.accountMenu) {
    const item = state.accounts.find(
      (account) => account.id === Number(target.dataset.accountMenu),
    );
    if (item) openAccountActionMenu(target, item);
    return;
  }
  closeAccountActionMenu();
  if (target.dataset.paginationKey) {
    const key = target.dataset.paginationKey;
    state.paginationPages[key] =
      Number(state.paginationPages[key] || 1) + Number(target.dataset.pageStep);
    if (state.serverPagination[key]) await loadServerPage(key);
    else paginationTables[key].render();
    return;
  }
  if (target.dataset.paginationGo) {
    const key = target.dataset.paginationGo;
    await goToPaginationPage(
      key,
      document.querySelector(`input[data-pagination-jump="${key}"]`)?.value,
    );
    return;
  }
  if (target.hasAttribute("data-close-dialog")) {
    target.closest("dialog")?.close();
    return;
  }
  if (target.dataset.view) setView(target.dataset.view);
  if (target.dataset.viewJump) setView(target.dataset.viewJump);
  if (target.hasAttribute("data-open-purpose")) openPurpose();
  if (target.hasAttribute("data-open-group")) openGroup();
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
      state.groups.find(
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
    loadRealtime();
  }
  try {
    if (target.dataset.purposeSwitch) {
      await switchPurposeGroup(
        target.dataset.purposeSwitch,
        target.dataset.group,
      );
    }
    if (target.dataset.toggleAccount) {
      const item = state.accounts.find(
        (value) => value.id === Number(target.dataset.toggleAccount),
      );
      const schedulable = target.dataset.resumeAccount === "true";
      const result = await api("/api/accounts/batch-schedule", {
        method: "POST",
        body: JSON.stringify({ ids: [item.id], schedulable }),
      });
      if (schedulable && Number(result.updated || 0) === 0) {
        throw new Error("账号授权或代理状态无效，无法恢复调度");
      }
      toast(schedulable ? "账号已恢复调度" : "账号已暂停调度");
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
    if (target.dataset.archiveAccount) {
      const item = state.accounts.find(
        (value) => value.id === Number(target.dataset.archiveAccount),
      );
      if (!item) return;
      const confirmed = await confirmAction(
        `归档“${item.name}”`,
        `账号会退出当前账号池并释放 ${item.proxy_hint || "已绑定的代理 IP"}。历史计费、请求和授权记录会继续保留。`,
        "确认归档",
      );
      if (!confirmed) return;
      await api(`/api/accounts/${item.id}/archive`, {
        method: "POST",
        body: "{}",
      });
      state.selectedDeadAccountIDs.delete(item.id);
      toast("账号已归档，代理 IP 已释放");
      await loadCore();
      return;
    }
    if (target.dataset.restoreAccount) {
      const item = state.archivedAccounts.find(
        (value) => value.id === Number(target.dataset.restoreAccount),
      );
      if (!item) return;
      const confirmed = await confirmAction(
        `移出归档“${item.name}”`,
        "账号会回到账号池并保持暂停调度，旧代理 IP 不会自动重新绑定。",
        "确认移出",
      );
      if (!confirmed) return;
      await api(`/api/accounts/${item.id}/restore`, {
        method: "POST",
        body: "{}",
      });
      toast("账号已移出归档，请重新分配代理或授权后再打开调度");
      await loadCore();
      return;
    }
    if (target.dataset.copyKey) {
      const item = state.keys.find(
        (value) => value.id === Number(target.dataset.copyKey),
      );
      try {
        await copyToClipboard(item?.key);
        toast("SK 已复制");
      } catch (error) {
        toast(error.message, "error");
      }
      return;
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
    if (target.dataset.deletePool) {
      const item = state.proxyPools.find(
        (value) => value.id === Number(target.dataset.deletePool),
      );
      if (!item) return;
      if (item.assigned_count) {
        toast(
          `代理池“${item.name}”仍被 ${item.assigned_count} 个账号占用，请先解绑账号`,
          "error",
        );
        return;
      }
      const confirmed = await confirmAction(
        `删除代理池“${item.name}”`,
        item.proxy_count
          ? `池内 ${item.proxy_count} 个代理会一并删除，删除后无法恢复。`
          : "该代理池当前没有代理，删除后无法恢复。",
        "确认删除",
      );
      if (!confirmed) return;
      const result = await api(`/api/proxy-pools/${item.id}`, {
        method: "DELETE",
      });
      if (String(state.proxyPoolFilter) === String(item.id))
        state.proxyPoolFilter = "";
      resetPagination("proxies");
      toast(
        result?.deleted_proxies
          ? `代理池已删除，同时清理 ${result.deleted_proxies} 个代理`
          : "代理池已删除",
      );
      await loadCore();
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
  const accountGroupChoice = event.target.closest(
    'input[name="account-group"], input[name="batch-group"], input[name="batch-edit-group"]',
  );
  if (accountGroupChoice?.checked) {
    const selectedGroup = state.groups.find(
      (group) => group.id === accountGroupChoice.value,
    );
    const picker = accountGroupChoice.closest("fieldset");
    $$(`input[name="${accountGroupChoice.name}"]`, picker).forEach((node) => {
      const group = state.groups.find((item) => item.id === node.value);
      if (
        node !== accountGroupChoice &&
        (selectedGroup?.reserve_pool_enabled || group?.reserve_pool_enabled)
      ) {
        node.checked = false;
      }
    });
  }
  const purpose = event.target.closest("select[data-purpose-switch]");
  if (purpose) {
    try {
      await switchPurposeGroup(purpose.dataset.purposeSwitch, purpose.value);
    } catch (error) {
      toast(error.message, "error");
      await loadCore();
    }
    return;
  }
  const select = event.target.closest("select[data-pagination-size]");
  if (!select) return;
  const key = select.dataset.paginationSize;
  state.paginationSizes[key] = Number(select.value);
  resetPagination(key);
  if (state.serverPagination[key]) await loadServerPage(key);
  else paginationTables[key].render();
});

document.addEventListener("keydown", async (event) => {
  const input = event.target.closest("input[data-pagination-jump]");
  if (!input || event.key !== "Enter") return;
  event.preventDefault();
  await goToPaginationPage(input.dataset.paginationJump, input.value);
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
    state.selectedDeadAccountIDs.clear();
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
  if (state.view === "strategies") openStrategy();
});
$("#refresh-button").addEventListener("click", async () => {
  await loadCore();
  if (state.view === "billing") await loadBilling();
  if (state.view === "audit") await loadAudit();
  if (state.view === "daily") await loadDaily();
  if (state.view === "authorization") {
    await loadAuthorization();
    await loadDeauthMonitor();
  }
  if (state.view === "errors") await loadErrors();
  if (state.view === "strategies") await loadStrategies();
  toast("数据已刷新");
});
$("#refresh-accounts").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  try {
    button.disabled = true;
    let health = null;
    const visibleAccountIDs = $$('[data-account-select]').map((node) =>
      Number(node.dataset.accountSelect),
    );
    if (isAdmin() && visibleAccountIDs.length)
      health = await api("/api/accounts/health/refresh", {
        method: "POST",
        body: JSON.stringify({ ids: visibleAccountIDs }),
      });
    await loadAccounts();
    if (health)
      toast(
        `检测 ${health.checked} 个账号：${health.healthy} 个正常，${health.failed} 个失败`,
        health.failed ? "error" : "success",
      );
    else toast("账号列表已刷新");
  } catch (error) {
    if (error.message.includes("存活检测正在运行")) {
      await loadAccounts();
      toast("后台存活检测正在运行，已刷新当前状态");
    } else toast(error.message, "error");
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

$("#archive-selected-accounts").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  const ids = [...state.selectedDeadAccountIDs];
  if (!ids.length) return;
  const selectedNames = ids
    .map((id) => state.accounts.find((item) => item.id === id)?.name)
    .filter(Boolean);
  const preview = selectedNames.slice(0, 3).join("、");
  const remainder = selectedNames.length > 3 ? ` 等 ${ids.length} 个账号` : "";
  const confirmed = await confirmAction(
    `归档 ${ids.length} 个死亡账号`,
    `将归档 ${preview}${remainder}，立即释放各账号当前绑定的代理 IP，并保留历史计费、请求和授权记录。`,
    `确认归档 ${ids.length} 个`,
  );
  if (!confirmed) return;
  try {
    button.disabled = true;
    const result = await api("/api/accounts/batch-archive", {
      method: "POST",
      body: JSON.stringify({ ids }),
    });
    state.selectedDeadAccountIDs.clear();
    toast(
      `已归档 ${result.archived} 个账号并释放 IP${result.skipped ? `，跳过 ${result.skipped} 个非死亡账号` : ""}`,
      result.skipped ? "error" : "success",
    );
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    syncDeadSelection();
  }
});

async function updateSelectedAccountSchedule(schedulable) {
  const ids = selectedAccountIDs();
  if (!ids.length) return;
  if (!schedulable) {
    const confirmed = await confirmAction(
      `暂停 ${ids.length} 个账号`,
      "已选账号将立即停止接收新请求，账号数据和授权信息会继续保留。",
      `确认暂停 ${ids.length} 个`,
    );
    if (!confirmed) return;
  }
  const buttons = [
    $("#schedule-selected-accounts"),
    $("#pause-selected-accounts"),
  ];
  try {
    buttons.forEach((button) => (button.disabled = true));
    const result = await api("/api/accounts/batch-schedule", {
      method: "POST",
      body: JSON.stringify({ ids, schedulable }),
    });
    state.selectedAccountIDs.clear();
    await loadCore();
    const skipped = Number(result.skipped || 0);
    toast(
      `${schedulable ? "已开启" : "已暂停"} ${result.updated} 个账号${skipped ? `，${skipped} 个因授权或代理状态未处理` : ""}`,
      skipped && schedulable ? "error" : "success",
    );
  } catch (error) {
    toast(error.message, "error");
  } finally {
    syncAccountSelection();
  }
}

$("#schedule-selected-accounts").addEventListener("click", () =>
  updateSelectedAccountSchedule(true),
);
$("#pause-selected-accounts").addEventListener("click", () =>
  updateSelectedAccountSchedule(false),
);
$("#edit-selected-accounts").addEventListener("click", openBatchAccountEdit);
$$('[data-batch-edit-apply]').forEach((node) =>
  node.addEventListener("change", syncBatchEditControls),
);
$("#add-proxy-pool").addEventListener("click", () => openPool());
$("#import-proxies").addEventListener("click", openProxyImport);
$("#test-proxies-batch").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  const poolID = Number(state.proxyPoolFilter || 0);
  const candidates = state.proxies.filter(
    (item) =>
      item.status !== "disabled" &&
      (!poolID || Number(item.pool_id) === poolID),
  );
  if (!candidates.length) {
    toast("当前范围没有可检测代理", "error");
    return;
  }
  const label = button.querySelector("span");
  try {
    button.disabled = true;
    label.textContent = "检测中…";
    const result = await api("/api/proxies/batch-test", {
      method: "POST",
      body: JSON.stringify({ pool_id: poolID, concurrency: 8 }),
    });
    await loadCore();
    toast(
      `代理检测完成：正常 ${result.success}，异常 ${result.failed}，共 ${result.total}`,
      result.failed ? "error" : "success",
    );
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
    label.textContent = "批量检测";
  }
});
$("#delete-proxies-batch").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  const ids = [...state.selectedProxyIDs];
  if (!ids.length) return;
  const names = ids
    .map((id) => state.proxies.find((item) => item.id === id)?.name)
    .filter(Boolean);
  const preview = names.slice(0, 3).join("、");
  const suffix = names.length > 3 ? ` 等 ${ids.length} 个代理` : "";
  const confirmed = await confirmAction(
    `删除 ${ids.length} 个代理`,
    `将删除 ${preview}${suffix}。已占用代理会触发自动账号换 IP，无法换 IP 的账号会暂停调度。`,
    `确认删除 ${ids.length} 个`,
  );
  if (!confirmed) return;
  try {
    button.disabled = true;
    const result = await api("/api/proxies/batch-delete", {
      method: "POST",
      body: JSON.stringify({ ids }),
    });
    state.selectedProxyIDs.clear();
    resetPagination("proxies");
    toast(
      `已删除 ${result.deleted} 个代理，换 IP ${result.reassigned_accounts} 个，暂停 ${result.paused_accounts} 个账号`,
      result.paused_accounts ? "error" : "success",
    );
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    syncProxySelection();
  }
});
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
$("#deauth-window").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-window]");
  if (!button) return;
  state.deauthWindow = Number(button.dataset.window);
  $$("#deauth-window button").forEach((node) =>
    node.classList.toggle("active", node === button),
  );
  loadDeauthMonitor();
});
$("#refresh-deauth").addEventListener("click", async () => {
  await loadDeauthMonitor();
  toast("掉授权监控已刷新");
});
$("#apply-error-filters").addEventListener("click", () => {
  resetPagination("errors");
  loadErrors();
});
let errorSearchTimer;
$("#error-search").addEventListener("input", () => {
  clearTimeout(errorSearchTimer);
  errorSearchTimer = setTimeout(() => {
    resetPagination("errors");
    loadErrors();
  }, 300);
});
$("#error-search").addEventListener("keydown", (event) => {
  if (event.key !== "Enter") return;
  event.preventDefault();
  clearTimeout(errorSearchTimer);
  resetPagination("errors");
  loadErrors();
});
$("#insight-status").addEventListener("change", loadErrorInsights);
$("#apply-daily-range").addEventListener("click", () => {
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
$("#dead-status-tabs").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-dead-status]");
  if (!button) return;
  state.deadStatus = button.dataset.deadStatus;
  state.selectedDeadAccountIDs.clear();
  resetPagination("dead");
  $$("#dead-status-tabs button").forEach((node) =>
    node.classList.toggle("active", node === button),
  );
  renderDeadAccounts();
});
$("#dead-search").addEventListener("input", (event) => {
  state.deadSearch = event.target.value;
  state.selectedDeadAccountIDs.clear();
  resetPagination("dead");
  renderDeadAccounts();
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
        accountMatchesStatus(item, state.accountStatus),
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
$("#select-all-dead").addEventListener("change", (event) => {
  const pending = state.accounts.filter(
    (item) =>
      item.dispatch_status === "error" && deadAccountMatchesSearch(item),
  );
  pending.forEach((item) =>
    event.target.checked
      ? state.selectedDeadAccountIDs.add(item.id)
      : state.selectedDeadAccountIDs.delete(item.id),
  );
  renderDeadAccounts();
});
$("#dead-accounts-body").addEventListener("change", (event) => {
  if (!event.target.matches("input[data-dead-select]")) return;
  const id = Number(event.target.dataset.deadSelect);
  if (event.target.checked) state.selectedDeadAccountIDs.add(id);
  else state.selectedDeadAccountIDs.delete(id);
  syncDeadSelection();
});
$("#account-from").addEventListener("change", loadAccountSummary);
$("#account-to").addEventListener("change", loadAccountSummary);
$("#proxy-pool-filter").addEventListener("change", (event) => {
  state.proxyPoolFilter = event.target.value;
  state.selectedProxyIDs.clear();
  resetPagination("proxies");
  renderProxies();
});
$("#proxy-search").addEventListener("input", (event) => {
  state.proxySearch = event.target.value;
  state.selectedProxyIDs.clear();
  resetPagination("proxies");
  renderProxies();
});
$("#select-all-proxies").addEventListener("change", (event) => {
  filteredProxies().forEach((item) =>
    event.target.checked
      ? state.selectedProxyIDs.add(item.id)
      : state.selectedProxyIDs.delete(item.id),
  );
  renderProxies();
});
$("#proxies-body").addEventListener("change", (event) => {
  if (!event.target.matches("input[data-proxy-select]")) return;
  const id = Number(event.target.dataset.proxySelect);
  if (event.target.checked) state.selectedProxyIDs.add(id);
  else state.selectedProxyIDs.delete(id);
  syncProxySelection();
});
$("#account-proxy-pool").addEventListener("change", (event) => {
  fillProxyOptions(event.target.value);
  if (!event.target.value) $("#account-auto-proxy").checked = false;
  syncAccountControls();
});
$("#account-auto-proxy").addEventListener("change", syncAccountControls);
$("#account-proxy-text").addEventListener("input", () => {
  const hasPool = Boolean($("#account-proxy-pool").value);
  const hasManualProxy = Boolean($("#account-proxy-text").value.trim());
  if (hasManualProxy) $("#account-auto-proxy").checked = false;
  $("#account-auto-proxy").disabled = !hasPool || hasManualProxy;
  $("#account-proxy").disabled =
    !hasPool || hasManualProxy || $("#account-auto-proxy").checked;
});
$("#account-rpm-enabled").addEventListener("change", syncAccountControls);
$("#account-rpm-strategy").addEventListener("change", syncAccountControls);
$("#account-auth-type").addEventListener("change", syncAccountAuthFields);
$("#account-session-key").addEventListener("input", syncAccountAuthFields);
$("#proxy-pool-source").addEventListener("change", toggleAPISource);
$("#user-role").addEventListener("change", () => syncUserRole(true));
$("#key-user").addEventListener("change", () => syncKeyGroups());

$("#session-auth-submit").addEventListener("click", async () => {
  const button = $("#session-auth-submit");
  const finishRequest = beginButtonRequest(button, "正在更新授权…");
  if (!finishRequest) return;
  try {
    const accountID = $("#auth-account-id").value;
    const sessionKey = $("#auth-session-key").value.trim();
    if (!sessionKey) throw new Error("请输入 Claude Session Key");
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
    finishRequest();
  }
});
$("#oauth-start").addEventListener("click", async () => {
  const button = $("#oauth-start");
  const finishRequest = beginButtonRequest(button, "正在生成…");
  if (!finishRequest) return;
  try {
    const result = await api(
      `/api/accounts/${$("#auth-account-id").value}/auth-url`,
      { method: "POST", body: JSON.stringify({ mode: $("#auth-mode").value }) },
    );
    state.oauthSessionID = result.session_id;
    $("#oauth-link").value = result.auth_url;
    $("#oauth-exchange-fields").hidden = false;
    try {
      await copyToClipboard(result.auth_url);
      toast("OAuth 链接已复制");
    } catch {
      toast("OAuth 链接已生成，请点击复制");
    }
  } catch (error) {
    toast(error.message, "error");
  } finally {
    finishRequest();
  }
});
$("#copy-oauth-link").addEventListener("click", async () => {
  try {
    await copyToClipboard($("#oauth-link").value);
    toast("OAuth 链接已复制");
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#oauth-exchange").addEventListener("click", async () => {
  const button = $("#oauth-exchange");
  const finishRequest = beginButtonRequest(button, "正在保存授权…");
  if (!finishRequest) return;
  try {
    if (!state.oauthSessionID || !$("#oauth-code").value.trim())
      throw new Error("请先完成 OAuth 并填写授权码");
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
    finishRequest();
  }
});

$("#batch-auth-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#batch-auth-submit");
  const finishRequest = beginButtonRequest(button, "正在准备授权…");
  if (!finishRequest) return;
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
    button.querySelector("span").textContent = `正在授权 0 / ${sessionKeys.length}`;
    const result = await api("/api/accounts/batch-authorize", {
      method: "POST",
      body: JSON.stringify({
        session_keys: sessionKeys,
        proxy_pool_id: Number($("#batch-proxy-pool").value),
        group_ids: groupIDs,
        auth_type: $("#batch-auth-type").value,
        account_price: Number($("#batch-account-price").value || 0),
        concurrency: Number($("#batch-concurrency").value || 10),
        base_rpm: Number($("#batch-base-rpm").value || 0),
        rpm_strategy: $("#batch-rpm-strategy").value,
        rpm_sticky_buffer: Number($("#batch-rpm-buffer").value || 0),
        strategy_id: strategySelectPayload($("#batch-strategy")),
      }),
    });
    $("#batch-result-panel").hidden = false;
    $("#batch-result-summary").textContent =
      `${result.success} 成功 · ${result.updated || 0} 更新 · ${result.skipped || 0} 跳过 · ${result.failed} 失败 · 共 ${result.total}`;
    state.batchResults = result.items;
    resetPagination("batch");
    renderBatchResults();
    toast(
      `批量授权完成：成功 ${result.success}，更新 ${result.updated || 0}，失败 ${result.failed}`,
    );
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    finishRequest();
  }
});

$("#batch-account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#batch-account-submit");
  const ids = selectedAccountIDs();
  if (!ids.length) {
    $("#batch-account-dialog").close();
    return;
  }
  const applies = (key) =>
    $(`[data-batch-edit-apply="${key}"]`).checked;
  const payload = { ids };
  const numberFields = [
    ["concurrency", "#batch-edit-concurrency"],
    ["base_rpm", "#batch-edit-base-rpm"],
    ["rpm_sticky_buffer", "#batch-edit-rpm-buffer"],
    ["priority", "#batch-edit-priority"],
    ["rate_multiplier", "#batch-edit-rate"],
    ["account_price", "#batch-edit-price"],
  ];
  numberFields.forEach(([key, selector]) => {
    if (applies(key)) payload[key] = Number($(selector).value);
  });
  if (applies("rpm_strategy"))
    payload.rpm_strategy = $("#batch-edit-rpm-strategy").value;
  if (applies("user_msg_queue_mode"))
    payload.user_msg_queue_mode = $("#batch-edit-queue-mode").value;
  if (applies("strategy_id")) {
    if (!state.strategiesLoaded) {
      toast("策略列表不可用，无法批量修改调度策略", "error");
      return;
    }
    payload.strategy_id = Number($("#batch-edit-strategy").value || 0);
  }
  if (applies("group_ids"))
    payload.group_ids = $$('input[name="batch-edit-group"]:checked').map(
      (node) => node.value,
    );
  if (Object.keys(payload).length === 1) {
    toast("至少勾选一个需要应用的字段", "error");
    return;
  }
  if (applies("group_ids") && !payload.group_ids.length) {
    toast("批量修改分组时至少选择一个分组", "error");
    return;
  }
  try {
    button.disabled = true;
    const result = await api("/api/accounts/batch-update", {
      method: "POST",
      body: JSON.stringify(payload),
    });
    $("#batch-account-dialog").close();
    state.selectedAccountIDs.clear();
    await loadCore();
    toast(`已更新 ${result.updated} 个账号的共有配置`);
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
    syncAccountSelection();
  }
});

$("#account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#account-submit");
  const finishRequest = beginButtonRequest(button);
  if (!finishRequest) return;
  try {
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
    const proxyText = $("#account-proxy-text").value.trim();
    const extra = JSON.parse($("#account-extra").value.trim() || "{}");
    extra.request_passthrough = $("#account-request-passthrough").checked;
    const mcpToolNames = $("#account-mcp-tool-names").value;
    if (mcpToolNames === "") delete extra.mcp_tool_names;
    else extra.mcp_tool_names = mcpToolNames === "on";
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
      proxy_id: proxyText ? null : proxyID || null,
      proxy_text: proxyText,
      auto_proxy:
        poolID && !proxyText ? $("#account-auto-proxy").checked : false,
      base_rpm: $("#account-rpm-enabled").checked
        ? Number($("#account-base-rpm").value)
        : 0,
      rpm_strategy: $("#account-rpm-strategy").value,
      rpm_sticky_buffer: Number($("#account-rpm-buffer").value),
      strategy_id: strategySelectPayload($("#account-strategy")),
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
    finishRequest();
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
$("#group-stream-hedge-enabled").addEventListener("change", (event) => {
  if (event.target.checked) {
    $("#group-adaptive-hedge-enabled").checked = false;
    $("#group-rpm-dispatch-enabled").checked = false;
  }
});
$("#group-adaptive-hedge-enabled").addEventListener("change", (event) => {
  if (event.target.checked) {
    $("#group-stream-hedge-enabled").checked = false;
    $("#group-rpm-dispatch-enabled").checked = false;
  }
});
$("#group-rpm-dispatch-enabled").addEventListener("change", (event) => {
  if (event.target.checked) {
    $("#group-stream-hedge-enabled").checked = false;
    $("#group-adaptive-hedge-enabled").checked = false;
  }
});
$("#group-rate-limit-wait-enabled").addEventListener(
  "change",
  syncGroupRateLimitWaitFields,
);
$("#group-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const optional = (selector) =>
      $(selector).value === "" ? null : Number($(selector).value);
    const id = $("#group-id").value;
    await api(id ? `/api/groups/${id}` : "/api/groups", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify({
        name: $("#group-name").value,
        description: $("#group-description").value,
        rate_multiplier: Number($("#group-rate").value),
        status: $("#group-status").value,
        daily_limit_usd: optional("#group-daily"),
        monthly_limit_usd: optional("#group-monthly"),
        reserve_pool_enabled: $("#group-reserve-pool").checked,
        normal_request_mode: $("#group-normal-request-mode").checked,
        claude_code_identity_enabled: $("#group-claude-code-identity").checked,
        reject_anthropic_downgrade_enabled: $(
          "#group-reject-anthropic-downgrade",
        ).checked,
        reject_distillation_enabled: $("#group-reject-distillation").checked,
        stream_hedge_enabled: $("#group-stream-hedge-enabled").checked,
        adaptive_hedge_enabled: $("#group-adaptive-hedge-enabled").checked,
        rpm_dispatch_enabled: $("#group-rpm-dispatch-enabled").checked,
        mcp_tool_names_enabled: $("#group-mcp-tool-names").checked,
        service_tier_passthrough_enabled: $(
          "#group-passthrough-service-tier",
        ).checked,
        inference_geo_passthrough_enabled: $(
          "#group-passthrough-inference-geo",
        ).checked,
        speed_passthrough_enabled: $("#group-passthrough-speed").checked,
        anthropic_beta_passthrough_enabled: $(
          "#group-passthrough-anthropic-beta",
        ).checked,
        overload_cooldown_seconds: Number($("#group-overload-cooldown").value),
        rate_limit_wait_enabled: $("#group-rate-limit-wait-enabled").checked,
        rate_limit_wait_seconds: Number(
          $("#group-rate-limit-wait-seconds").value,
        ),
        strategy_required_enabled: $("#group-strategy-required").checked,
        capacity_queue_enabled: $("#group-capacity-queue-enabled").checked,
        capacity_queue_timeout_seconds: Number(
          $("#group-capacity-queue-timeout").value,
        ),
        strategy_id: strategySelectPayload($("#group-strategy")),
      }),
    });
    $("#group-dialog").close();
    toast(id ? "分组已更新" : "分组已创建");
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#add-strategy").addEventListener("click", () => openStrategy());
$("#refresh-strategies").addEventListener("click", async () => {
  await loadStrategies();
  toast("策略数据已刷新");
});
$("#strategy-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const id = $("#strategy-id").value;
    await api(id ? `/api/strategies/${id}` : "/api/strategies", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify({
        name: $("#strategy-name").value,
        description: $("#strategy-description").value,
        rpm_limit: Number($("#strategy-rpm").value),
        tpm_limit: Number($("#strategy-tpm").value),
        concurrency_limit: Number($("#strategy-concurrency").value),
        rpm_strategy: $("#strategy-rpm-mode").value,
        rpm_sticky_buffer: Number($("#strategy-buffer").value),
        dispatch_mode: $("#strategy-dispatch-mode").value,
      }),
    });
    $("#strategy-dialog").close();
    toast(id ? "策略已更新" : "策略已创建");
    await loadStrategies();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#strategy-cards").addEventListener("click", async (event) => {
  const bindButton = event.target.closest("[data-bind-strategy]");
  if (bindButton) {
    await openStrategyAccountDialog(bindButton.dataset.bindStrategy, "bind");
    return;
  }
  const unbindButton = event.target.closest("[data-unbind-strategy]");
  if (unbindButton) {
    await openStrategyAccountDialog(unbindButton.dataset.unbindStrategy, "unbind");
    return;
  }
  const editButton = event.target.closest("[data-edit-strategy]");
  if (editButton) {
    const item = state.strategies.find(
      (entry) => String(entry.id) === editButton.dataset.editStrategy,
    );
    if (item) openStrategy(item);
    return;
  }
  const deleteButton = event.target.closest("[data-delete-strategy]");
  if (!deleteButton) return;
  const item = state.strategies.find(
    (entry) => String(entry.id) === deleteButton.dataset.deleteStrategy,
  );
  if (!item) return;
  const confirmed = await confirmAction(
    `删除策略“${item.name}”`,
    "仍被分组或账号绑定的策略无法删除，请先在分组/账号编辑中解绑。",
    "确认删除",
  );
  if (!confirmed) return;
  try {
    await api(`/api/strategies/${item.id}`, { method: "DELETE" });
    toast("策略已删除");
    await loadStrategies();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#strategy-account-search").addEventListener("input", () => {
  state.selectedStrategyAccountIDs.clear();
  renderStrategyAccountList();
});
$("#strategy-account-list").addEventListener("change", (event) => {
  if (!event.target.matches("input[data-strategy-account-select]")) return;
  const id = Number(event.target.dataset.strategyAccountSelect);
  if (event.target.checked) state.selectedStrategyAccountIDs.add(id);
  else state.selectedStrategyAccountIDs.delete(id);
  renderStrategyAccountList();
});
$("#strategy-account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const ids = [...state.selectedStrategyAccountIDs];
  if (!ids.length) {
    toast("请选择账号", "error");
    return;
  }
  const strategyID = Number($("#strategy-account-id").value);
  const mode = $("#strategy-account-mode").value;
  const button = $("#strategy-account-submit");
  try {
    button.disabled = true;
    await api("/api/accounts/batch-update", {
      method: "POST",
      body: JSON.stringify({
        ids,
        strategy_id: mode === "unbind" ? 0 : strategyID,
      }),
    });
    $("#strategy-account-dialog").close();
    state.selectedStrategyAccountIDs.clear();
    toast(mode === "unbind" ? "账号已移出策略池" : "账号已导入策略池");
    await Promise.all([loadAccounts(), loadStrategies()]);
  } catch (error) {
    toast(error.message, "error");
  } finally {
    button.disabled = false;
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
    const saved = await api(id ? `/api/proxy-pools/${id}` : "/api/proxy-pools", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    $("#proxy-pool-dialog").close();
    toast(
      saved.protocol_synced
        ? `代理池已保存，已同步 ${saved.protocol_synced} 个代理`
        : "代理池已保存",
    );
    await loadCore();
  } catch (error) {
    toast(error.message, "error");
  }
});
$("#proxy-import-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const poolID = Number($("#proxy-import-pool").value);
    const pool = state.proxyPools.find((item) => item.id === poolID);
    const result = await api("/api/proxies/batch", {
      method: "POST",
      body: JSON.stringify({
        pool_id: poolID,
        text: $("#proxy-import-text").value,
      }),
    });
    $("#proxy-import-dialog").close();
    $("#proxy-import-text").value = "";
    state.proxyPoolFilter = String(poolID);
    resetPagination("proxies");
    await loadCore();
    toast(
      `导入完成：新增 ${result.created}，已存在/重复 ${result.skipped}，无效 ${result.invalid}；已显示 ${pool?.name || "目标代理池"}`,
    );
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
      balance:
        $("#user-balance").value === ""
          ? null
          : Number($("#user-balance").value),
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
  try {
    await copyToClipboard($("#created-secret").textContent);
    toast("SK 已复制");
  } catch (error) {
    toast(error.message, "error");
  }
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
  const dailyStart = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 29);
  const errorStart = new Date(now.getTime() - 7 * 24 * 60 * 60 * 1000);
  const local = (date) =>
    new Date(date.getTime() - date.getTimezoneOffset() * 60000)
      .toISOString()
      .slice(0, 16);
  const localDate = (date) => local(date).slice(0, 10);
  const from = local(first);
  const to = local(now);
  $("#daily-from").value = localDate(dailyStart);
  $("#daily-to").value = localDate(now);
  $("#daily-to").max = localDate(now);
  $("#billing-from").value = from;
  $("#billing-to").value = to;
  $("#account-from").value = from;
  $("#account-to").value = to;
  $("#audit-from").value = from;
  $("#audit-to").value = to;
  $("#authorization-from").value = from;
  $("#authorization-to").value = to;
  $("#error-from").value = local(errorStart);
  $("#error-to").value = to;
}
initializeAccountAutoRefresh();
initializeRealtimeRefresh();
initializeStrategyRefresh();
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") closeAccountActionMenu();
});
window.addEventListener("resize", closeAccountActionMenu);
window.addEventListener("scroll", closeAccountActionMenu, true);
window.setInterval(updateSurvivalClocks, 60000);
boot();
