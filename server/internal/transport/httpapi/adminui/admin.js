const state = {
  serverURL: "",
  devices: [],
  pairingTimer: null,
  csrfToken: "",
  sessionTimer: null,
  deviceFilter: "all",
  deviceSearch: "",
};

const componentNames = {
  model: "服务端模型集成",
  nocturne: "Nocturne 投递服务",
  notesync: "NoteSync 笔记同步",
  offline_protocol: "离线学习协议",
  offline_signer: "离线签名服务",
  open_evaluation_worker: "开放评估任务",
  postgresql: "PostgreSQL 数据库",
};

const reasonNames = {
  healthy: "运行正常",
  degraded: "部分功能未就绪",
  not_ready: "尚未就绪",
  not_configured: "未配置，不影响基础功能",
  model_unavailable: "模型不可用",
  unknown: "状态未知",
};

const statusNames = {
  healthy: "运行正常",
  degraded: "部分功能未就绪",
  not_ready: "服务未就绪",
  unknown: "状态未知",
};

const scopeNames = {
  "devices:manage": "管理设备",
  "devices:read": "查看设备",
  "knowledge:approve": "审批知识",
  "knowledge:read": "读取知识",
  "knowledge:write": "维护知识",
  "learning:approve": "审批学习",
  "learning:read": "读取进度",
  "learning:write": "进行学习",
  "memory:read": "读取偏好",
  "memory:write": "保存偏好",
  "model:probe": "探测模型",
  "privacy:device": "设备隐私",
  "privacy:read": "读取隐私状态",
};

const byID = (id) => document.getElementById(id);

function showToast(message, error = false) {
  const toast = byID("toast");
  toast.textContent = message;
  toast.classList.toggle("error", error);
  toast.hidden = false;
  window.clearTimeout(showToast.timer);
  showToast.timer = window.setTimeout(() => {
    toast.hidden = true;
  }, 3200);
}

async function api(path, options = {}) {
  const method = options.method || "GET";
  const headers = { ...(options.headers || {}) };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && method !== "HEAD" && state.csrfToken) {
    headers["X-Admin-CSRF"] = state.csrfToken;
  }
  const response = await fetch(path, {
    credentials: "same-origin",
    ...options,
    headers,
  });
  if (response.status === 401) {
    window.location.assign("/admin/login");
    throw new Error("登录会话已过期");
  }
  if (response.status === 204) {
    await response.text();
    return null;
  }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(
      body.error?.message || body.detail || body.title || `请求失败（${response.status}）`,
    );
  }
  return body;
}

function labelReason(value) {
  if (!value) return "状态未知";
  return reasonNames[value] || value.replaceAll("_", " ");
}

function renderStatus(report) {
  const reportStatus = report.status || "unknown";
  const overall = byID("overall-status");
  overall.textContent = statusNames[reportStatus] || labelReason(reportStatus);
  overall.className = `overall-status ${reportStatus}`;

  const entries = Object.entries(report.components || {}).sort(([left], [right]) =>
    left.localeCompare(right),
  );
  const healthyCount = entries.filter(([, component]) => component.status === "healthy").length;
  byID("healthy-count").textContent = String(healthyCount);
  byID("attention-count").textContent = String(entries.length - healthyCount);

  const grid = byID("component-grid");
  grid.replaceChildren();
  for (const [name, component] of entries) {
    const item = document.createElement("div");
    item.className = "component";

    const title = document.createElement("div");
    title.className = "component-name";
    const dot = document.createElement("span");
    dot.className = `status-dot ${component.status}`;
    dot.setAttribute("aria-hidden", "true");
    const text = document.createElement("span");
    text.textContent = componentNames[name] || name;
    title.append(dot, text);
    item.append(title);

    const reason = document.createElement("p");
    reason.className = "component-reason";
    reason.textContent = labelReason(component.reason || component.status);
    item.append(reason);
    grid.append(item);
  }
}

function formatTime(value) {
  if (!value) return "从未使用";
  return new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

function filteredDevices() {
  const query = state.deviceSearch.trim().toLocaleLowerCase("zh-CN");
  return state.devices.filter((device) => {
    const revoked = Boolean(device.revoked_at);
    if (state.deviceFilter === "active" && revoked) return false;
    if (state.deviceFilter === "revoked" && !revoked) return false;
    if (!query) return true;
    return `${device.display_name || ""} ${device.id || ""}`.toLocaleLowerCase("zh-CN").includes(query);
  });
}

function renderDevices() {
  const devices = filteredDevices();
  const activeCount = state.devices.filter((device) => !device.revoked_at).length;
  byID("device-count").textContent = `${activeCount} 台活跃设备`;
  byID("active-device-count").textContent = String(activeCount);
  byID("empty-devices").hidden = devices.length !== 0;

  const rows = byID("device-rows");
  rows.replaceChildren();
  const ordered = [...devices].sort((left, right) => {
    if (Boolean(left.revoked_at) !== Boolean(right.revoked_at)) return left.revoked_at ? 1 : -1;
    return new Date(right.created_at) - new Date(left.created_at);
  });

  for (const device of ordered) {
    const row = document.createElement("tr");

    const identityCell = document.createElement("td");
    const name = document.createElement("span");
    name.className = "device-name";
    name.textContent = device.display_name || "未命名设备";
    const id = document.createElement("span");
    id.className = "device-id";
    id.textContent = device.id;
    id.title = device.id;
    identityCell.append(name, id);

    const scopeCell = document.createElement("td");
    const scopes = document.createElement("div");
    scopes.className = "scope-list";
    for (const scopeName of device.scopes || []) {
      const scope = document.createElement("span");
      scope.className = "scope";
      scope.textContent = scopeNames[scopeName] || scopeName;
      scope.title = scopeName;
      scopes.append(scope);
    }
    scopeCell.append(scopes);

    const usedCell = document.createElement("td");
    usedCell.textContent = formatTime(device.last_used_at);

    const statusCell = document.createElement("td");
    const status = document.createElement("span");
    status.className = `state ${device.revoked_at ? "revoked" : "active"}`;
    status.textContent = device.revoked_at ? "已撤销" : "活跃";
    statusCell.append(status);

    const actionCell = document.createElement("td");
    if (!device.revoked_at) {
      const revoke = document.createElement("button");
      revoke.type = "button";
      revoke.className = "danger-button";
      revoke.textContent = "撤销授权";
      revoke.addEventListener("click", () => revokeDevice(device, revoke));
      actionCell.append(revoke);
    }

    row.append(identityCell, scopeCell, usedCell, statusCell, actionCell);
    rows.append(row);
  }
}

async function loadSession() {
  const session = await api("/admin/api/session");
  state.csrfToken = session.csrf_token;
  window.clearTimeout(state.sessionTimer);
  const remaining = Math.max(0, new Date(session.expires_at).getTime() - Date.now());
  state.sessionTimer = window.setTimeout(() => {
    window.location.assign("/admin/login");
  }, remaining);
}

async function loadOverview() {
  const refresh = byID("refresh");
  refresh.disabled = true;
  try {
    const overview = await api("/admin/api/overview");
    state.serverURL = overview.server_url;
    state.devices = overview.devices || [];
    byID("server-address").textContent = overview.server_url;
    renderStatus(overview.status);
    renderDevices();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    refresh.disabled = false;
  }
}

async function createPairingCode(event) {
  event.preventDefault();
  const button = event.submitter;
  button.disabled = true;
  try {
    const result = await api("/admin/api/pairing-codes", {
      method: "POST",
      body: JSON.stringify({ profile: byID("profile").value }),
    });
    byID("pairing-code").textContent = result.code;
    byID("pairing-expiry").textContent = `有效至 ${formatTime(result.expires_at)}`;
    byID("server-url").textContent = result.server_url;
    byID("pairing-result").hidden = false;
    window.clearTimeout(state.pairingTimer);
    const expiresIn = Math.max(0, new Date(result.expires_at).getTime() - Date.now());
    state.pairingTimer = window.setTimeout(() => {
      byID("pairing-code").textContent = "";
      byID("pairing-result").hidden = true;
    }, expiresIn);
    showToast("配对码已生成");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function revokeDevice(device, button) {
  if (!window.confirm(`确定撤销“${device.display_name || "未命名设备"}”的访问权限吗？`)) return;
  button.disabled = true;
  try {
    await api(`/admin/api/devices/${encodeURIComponent(device.id)}/revoke`, {
      method: "POST",
      body: "{}",
    });
    showToast("设备授权已撤销");
    await loadOverview();
  } catch (error) {
    button.disabled = false;
    showToast(error.message, true);
  }
}

async function copyCode() {
  const code = byID("pairing-code").textContent;
  try {
    await navigator.clipboard.writeText(code);
    showToast("配对码已复制");
  } catch {
    showToast("无法访问剪贴板，请手动复制", true);
  }
}

async function logout() {
  const button = byID("logout");
  button.disabled = true;
  try {
    await api("/admin/api/logout", { method: "POST", body: "{}" });
    await new Promise((resolve) => window.setTimeout(resolve, 50));
    window.location.assign("/admin/login");
  } catch (error) {
    button.disabled = false;
    showToast(error.message, true);
  }
}

function selectDeviceFilter(event) {
  const button = event.target.closest("button[data-filter]");
  if (!button) return;
  state.deviceFilter = button.dataset.filter;
  for (const item of byID("device-filter").querySelectorAll("button")) {
    item.classList.toggle("active", item === button);
  }
  renderDevices();
}

async function initialize() {
  try {
    await loadSession();
    await loadOverview();
  } catch (error) {
    showToast(error.message, true);
  }
}

byID("refresh").addEventListener("click", loadOverview);
byID("logout").addEventListener("click", logout);
byID("pairing-form").addEventListener("submit", createPairingCode);
byID("copy-code").addEventListener("click", copyCode);
byID("device-filter").addEventListener("click", selectDeviceFilter);
byID("device-search").addEventListener("input", (event) => {
  state.deviceSearch = event.target.value;
  renderDevices();
});
initialize();
