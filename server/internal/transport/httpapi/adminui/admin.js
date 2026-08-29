(() => {
  "use strict";

  const pageMeta = {
    overview: ["SERVER", "服务总览"],
    pairing: ["IDENTITY", "客户端配对"],
    devices: ["IDENTITY", "设备管理"],
    memory: ["MEMORY", "记忆树"],
    knowledge: ["KNOWLEDGE", "知识库"],
    notesync: ["INTEGRATION", "NoteSync"]
  };
  const validPages = new Set(Object.keys(pageMeta));
  const state = {
    csrfToken: "",
    sessionTimer: null,
    activePage: "overview",
    overview: null,
    devices: [],
    memory: null,
    knowledge: null,
    notesync: null,
    selectedMemory: "",
    selectedKnowledgePath: "",
    selectedKnowledgeNode: ""
  };

  const byId = (id) => document.getElementById(id);
  const text = (tag, value, className) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    node.textContent = value ?? "";
    return node;
  };
  const button = (label, className, handler) => {
    const node = text("button", label, className);
    node.type = "button";
    node.addEventListener("click", handler);
    return node;
  };
  const shortID = (value) => value ? String(value).slice(0, 8) : "-";
  const formatDate = (value) => {
    if (!value) return "从未";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? String(value) : new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(date);
  };
  const statusMeta = Object.freeze({
    ready: ["就绪", "success"], healthy: ["正常", "success"], ok: ["正常", "success"], active: ["有效", "success"],
    applied: ["已应用", "success"], succeeded: ["成功", "success"], available: ["可读取", "success"], compatible: ["兼容", "success"],
    in_sync: ["已同步", "success"], remote_unchanged: ["远端未变化", "success"], resolved: ["已解决", "success"], closed: ["已关闭", "success"],
    absence_verified: ["已确认不存在", "success"], verified: ["已验证", "success"], confirmed: ["已确认", "success"],
    degraded: ["降级", "warning"], queued: ["队列中", "warning"], pending: ["待处理", "warning"], unknown: ["状态未知", "warning"],
    not_configured: ["未配置", "warning"], open: ["待审阅", "warning"], partial: ["部分完成", "warning"], not_applicable: ["不适用", "neutral"],
    unsupported: ["不支持", "warning"], local_changed: ["本地已变化", "warning"], remote_changed: ["远端已变化", "warning"], remote_missing: ["远端缺失", "warning"],
    unavailable: ["不可用", "danger"], failed: ["失败", "danger"], rejected: ["已拒绝", "danger"], permanently_rejected: ["永久拒绝", "danger"],
    redacted: ["已清除", "danger"], revoked: ["已撤销", "danger"], incompatible: ["不兼容", "danger"], not_ready: ["未就绪", "danger"],
    superseded: ["已被替代", "neutral"], delete_pending: ["等待删除", "warning"], deleted: ["已删除", "danger"], fenced: ["已隔离", "danger"],
    expiry_reconciling: ["正在核对过期状态", "warning"], expired: ["已过期", "danger"], both_changed: ["两端均已变化", "danger"],
    remote_moved: ["远端身份已移动", "danger"], unbased_remote: ["远端文档未纳管", "warning"], path_occupied: ["路径已占用", "danger"],
    invalid_remote_markdown: ["远端文档无效", "danger"], prepared: ["已准备", "warning"], sent: ["已发送", "warning"],
    reconciling: ["正在核对", "warning"], conflict: ["存在冲突", "danger"]
  });
  const statusClass = (value) => statusMeta[value]?.[1] || "neutral";
  const statusLabel = (value) => statusMeta[value]?.[0] || "未知状态";
  const reasonLabel = (value) => {
    if (!value) return "无附加原因";
    return ({
      local_revision_changed: "本地知识修订已变化", both_sides_changed: "本地与远端均已变化", remote_identity_moved: "远端文档身份已移动",
      unmanaged_remote_note: "远端文档尚未纳管", remote_markdown_invalid: "远端 Markdown 无效", remote_content_changed: "远端内容已变化",
      remote_note_missing: "远端文档缺失", remote_path_occupied: "远端路径已占用", publication_preflight_changed: "发布前状态已变化",
      publication_readback_changed: "发布回读结果已变化"
    }[value] || "原因未知");
  };

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");
    if (options.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    if (options.csrf) headers.set("X-Admin-CSRF", state.csrfToken);
    const response = await fetch(path, { ...options, headers, credentials: "same-origin" });
    if (response.status === 401) {
      showLogin();
      throw new Error("管理会话已失效，请重新登录");
    }
    let payload = null;
    if (response.status !== 204) {
      const type = response.headers.get("content-type") || "";
      payload = type.includes("application/json") ? await response.json() : await response.text();
    }
    if (!response.ok) {
      const body = payload;
      const code = body?.error?.code || body?.code || "";
      const localized = {
        notesync_not_configured: "NoteSync 尚未启用。保存启用配置并重启服务后再试。"
      }[code];
      const message = localized || (body && body.error?.message) || body?.message || `请求失败（${response.status}）`;
      throw new Error(message);
    }
    return payload;
  }

  function showNotice(message, kind = "warning") {
    const notice = byId("globalNotice");
    notice.textContent = message;
    notice.style.borderLeftColor = kind === "danger" ? "#b42318" : kind === "success" ? "#13795b" : "#9a6700";
    notice.hidden = false;
  }

  function clearNotice() {
    byId("globalNotice").hidden = true;
    byId("globalNotice").textContent = "";
  }

  function showLogin() {
    state.csrfToken = "";
    if (state.sessionTimer) clearTimeout(state.sessionTimer);
    state.sessionTimer = null;
    window.location.replace("/admin/login");
  }

  function scheduleSessionExpiry(value) {
    if (state.sessionTimer) clearTimeout(state.sessionTimer);
    const expiresAt = new Date(value).getTime();
    if (!Number.isFinite(expiresAt)) return;
    const delay = Math.max(0, Math.min(2_147_000_000, expiresAt - Date.now() + 250));
    state.sessionTimer = window.setTimeout(showLogin, delay);
  }

  function showConsole() {
    byId("consoleView").hidden = false;
  }

  async function bootstrapConsole() {
    try {
      const session = await api("/admin/api/session");
      state.csrfToken = session.csrf_token || "";
      scheduleSessionExpiry(session.expires_at);
      showConsole();
      await loadOverview(false);
      const page = validPages.has(location.hash.slice(1)) ? location.hash.slice(1) : "overview";
      await navigate(page, true);
    } catch (error) {
      if (!byId("consoleView").hidden) showNotice(error.message, "danger");
    }
  }
  window.bootstrapConsole = bootstrapConsole;

  async function navigate(page, replaceHash = false) {
    if (!validPages.has(page)) page = "overview";
    state.activePage = page;
    const [eyebrow, title] = pageMeta[page];
    byId("pageEyebrow").textContent = eyebrow;
    byId("pageTitle").textContent = title;
    document.querySelectorAll("[data-page-panel]").forEach((panel) => {
      const active = panel.dataset.pagePanel === page;
      panel.hidden = !active;
      panel.classList.toggle("active", active);
    });
    document.querySelectorAll("[data-page]").forEach((item) => {
      const active = item.dataset.page === page;
      item.classList.toggle("active", active);
      if (active) item.setAttribute("aria-current", "page"); else item.removeAttribute("aria-current");
    });
    if (window.matchMedia("(max-width: 780px)").matches) {
      document.querySelector(`[data-page="${page}"]`)?.scrollIntoView({ block: "nearest", inline: "center" });
    }
    const targetHash = `#${page}`;
    if (location.hash !== targetHash) {
      history[replaceHash ? "replaceState" : "pushState"](null, "", targetHash);
    }
    clearNotice();
    await loadPage(page, false);
  }

  async function loadPage(page, force) {
    byId("refreshPage").disabled = true;
    try {
      if (page === "overview") await loadOverview(force);
      if (page === "pairing" && !state.overview) await loadOverview(false);
      if (page === "devices") await loadDevices(force);
      if (page === "memory") await loadMemory(force);
      if (page === "knowledge") await loadKnowledge(force);
      if (page === "notesync") await loadNotesync(force);
    } catch (error) {
      showNotice(error.message, "danger");
    } finally {
      byId("refreshPage").disabled = false;
    }
  }

  async function loadOverview(force) {
    if (!force && state.overview) {
      renderOverview();
      return;
    }
    state.overview = await api("/admin/api/overview");
    state.csrfToken = state.overview.csrf_token || state.csrfToken;
    renderOverview();
  }

  function renderOverview() {
    const data = state.overview;
    const readiness = data.status || data.readiness || {};
    const ready = readiness.status !== "not_ready";
    const header = byId("headerStatus");
    header.className = `status-badge ${readiness.status === "healthy" ? "success" : readiness.status === "not_ready" ? "danger" : "warning"}`;
    header.textContent = readiness.status === "healthy" ? "服务就绪" : readiness.status === "not_ready" ? "服务未就绪" : "服务降级";
    byId("sidebarStatus").textContent = ready ? `服务器已连接 · ${statusLabel(readiness.status)}` : "服务器未就绪";

    const componentEntries = readiness.components && !Array.isArray(readiness.components) ? Object.entries(readiness.components) : [];
    const services = Array.isArray(readiness.components) ? readiness.components : componentEntries.map(([name, component]) => ({ name, ...component }));
    const memoryComponent = services.find((item) => item.name === "nocturne") || {};
    const notesyncComponent = services.find((item) => item.name === "notesync") || {};
    const stripValues = [
      ["服务状态", statusLabel(readiness.status)],
      ["已配对设备", String((data.devices || []).length)],
      ["Nocturne", statusLabel(memoryComponent.status || "unknown")],
      ["NoteSync", statusLabel(notesyncComponent.status || "unknown")]
    ];
    byId("statusStrip").replaceChildren(...stripValues.map(([label, value]) => {
      const cell = document.createElement("div");
      cell.className = "status-cell";
      cell.append(text("span", label), text("strong", value));
      return cell;
    }));

    const serviceList = byId("serviceList");
    serviceList.replaceChildren();
    if (services.length) {
      services.forEach((service) => {
        const row = document.createElement("div");
        row.className = "service-row";
        const name = document.createElement("div");
        name.className = "service-name";
        name.append(text("strong", componentLabel(service.name)), text("span", ["model", "notesync", "open_evaluation_worker"].includes(service.name) ? "可选集成" : "核心服务"));
        const badge = text("span", statusLabel(service.status), `status-badge ${statusClass(service.status)}`);
        row.append(name, badge, text("span", service.reason || "运行正常", "service-reason"));
        serviceList.append(row);
      });
    } else {
      serviceList.append(empty("暂无组件状态", "服务尚未返回组件明细。"));
    }

    const facts = [
      ["服务地址", data.server_url || data.public_base_url || "-"],
      ["配对接口", `${data.server_url || ""}/v1/pairings/exchange`],
      ["管理边界", "本机回环"],
      ["管理会话", "15 分钟"]
    ];
    const dl = byId("connectionFacts");
    dl.replaceChildren(...facts.map(([label, value]) => {
      const wrapper = document.createElement("div");
      wrapper.append(text("dt", label), text("dd", value));
      return wrapper;
    }));
    state.devices = data.devices || [];
    byId("deviceNavCount").textContent = String(state.devices.length);
  }

  async function loadDevices(force) {
    if (force || !state.overview) await loadOverview(true);
    renderDevices();
  }

  function renderDevices() {
    const query = byId("deviceSearch").value.trim().toLowerCase();
    const devices = state.devices.filter((device) => `${device.display_name || ""} ${device.id || ""}`.toLowerCase().includes(query));
    const rows = byId("deviceRows");
    const cards = byId("deviceCards");
    rows.replaceChildren();
    cards.replaceChildren();
    byId("deviceEmpty").hidden = devices.length !== 0;
    devices.forEach((device) => {
      rows.append(deviceTableRow(device));
      cards.append(deviceCard(device));
    });
  }

  function deviceTableRow(device) {
    const tr = document.createElement("tr");
    const nameCell = document.createElement("td");
    const name = document.createElement("div");
    name.className = "device-name";
    name.append(text("strong", device.display_name || "未命名设备"), text("span", device.id || "-"));
    nameCell.append(name);
    const scopes = document.createElement("td");
    scopes.append(scopeList(device.scopes || []));
    const status = document.createElement("td");
    status.append(text("span", device.revoked_at ? "已撤销" : "有效", `status-badge ${device.revoked_at ? "danger" : "success"}`));
    const used = text("td", formatDate(device.last_used_at || device.created_at));
    const actions = document.createElement("td");
    actions.className = "action-column";
    if (!device.revoked_at) actions.append(button("撤销", "secondary-button", () => revokeDevice(device)));
    tr.append(nameCell, scopes, status, used, actions);
    return tr;
  }

  function deviceCard(device) {
    const card = document.createElement("article");
    card.className = "record-card";
    const head = document.createElement("div");
    head.className = "record-card-head";
    const name = document.createElement("div");
    name.className = "device-name";
    name.append(text("strong", device.display_name || "未命名设备"), text("span", shortID(device.id)));
    head.append(name, text("span", device.revoked_at ? "已撤销" : "有效", `status-badge ${device.revoked_at ? "danger" : "success"}`));
    const dl = document.createElement("dl");
    [["权限", (device.scopes || []).join("、") || "-"], ["最近使用", formatDate(device.last_used_at || device.created_at)]].forEach(([k, v]) => dl.append(text("dt", k), text("dd", v)));
    card.append(head, dl);
    if (!device.revoked_at) card.append(button("撤销设备", "secondary-button", () => revokeDevice(device)));
    return card;
  }

  function scopeList(scopes) {
    const wrapper = document.createElement("div");
    wrapper.className = "scope-list";
    scopes.forEach((scope) => wrapper.append(text("span", scope, "scope-chip")));
    return wrapper;
  }

  async function revokeDevice(device) {
    const confirmed = await confirmAction("撤销设备", `撤销“${device.display_name || shortID(device.id)}”后，该设备的现有凭据将失效。`);
    if (!confirmed) return;
    try {
      await api(`/admin/api/devices/${encodeURIComponent(device.id)}/revoke`, { method: "POST", csrf: true });
      state.overview = null;
      await loadDevices(true);
      showNotice("设备已撤销。", "success");
    } catch (error) {
      showNotice(error.message, "danger");
    }
  }

  async function createPairing(event) {
    event.preventDefault();
    const submit = byId("pairingSubmit");
    submit.disabled = true;
    try {
      const profile = new FormData(event.currentTarget).get("pairingProfile");
      const payload = await api("/admin/api/pairing-codes", {
        method: "POST", csrf: true,
        body: JSON.stringify({ profile })
      });
      renderPairingResult(payload);
      state.overview = null;
    } catch (error) {
      showNotice(error.message, "danger");
    } finally {
      submit.disabled = false;
    }
  }

  function renderPairingResult(payload) {
    const wrapper = document.createElement("div");
    wrapper.className = "pairing-code";
    wrapper.append(text("span", "一次性配对码", "eyebrow"), text("div", payload.code || "-", "code-display"));
    const command = `edu-agent pair --server ${payload.server_url || ""} --code ${payload.code || ""}`;
    wrapper.append(text("pre", command, "command-display"));
    wrapper.append(button("复制配对命令", "secondary-button", async () => {
      await navigator.clipboard.writeText(command);
      showNotice("配对命令已复制。", "success");
    }));
    wrapper.append(text("small", `有效期至 ${formatDate(payload.expires_at)}`));
    byId("pairingResult").replaceChildren(wrapper);
  }

  async function loadMemory(force) {
    if (!force && state.memory) {
      renderMemory();
      return;
    }
    state.memory = await api("/admin/api/memory?limit=200");
    renderMemory();
  }

  async function loadMoreMemory() {
    const cursor = state.memory?.next_cursor;
    if (!cursor) return;
    const control = byId("loadMoreMemory");
    control.disabled = true;
    try {
      const page = await api(`/admin/api/memory?limit=200&cursor=${encodeURIComponent(cursor)}`);
      state.memory = { ...page, items: [...(state.memory.items || []), ...(page.items || [])] };
      renderMemory();
    } catch (error) {
      showNotice(error.message, "danger");
    } finally {
      control.disabled = false;
    }
  }

  function renderMemory() {
    const items = state.memory?.items || [];
    const active = items.filter((item) => item.record?.status === "applied").length;
    const queued = items.filter((item) => item.delivery_status === "queued").length;
    const unavailable = items.filter((item) => item.content_status !== "available").length;
    renderMetrics("memoryStats", [["已加载节点", items.length], ["当前生效", active], ["待同步", queued], ["内容受限", unavailable]]);
    byId("loadMoreMemory").hidden = !state.memory?.next_cursor;
    const query = byId("memorySearch").value.trim().toLowerCase();
    const filtered = items.filter((item) => {
      const record = item.record || {};
      return `${item.content || ""} ${record.logical_memory_id || ""} ${record.id || ""}`.toLowerCase().includes(query);
    });
    const tree = byId("memoryTree");
    tree.replaceChildren();
    if (!filtered.length) {
      tree.append(empty("没有记忆节点", query ? "没有匹配当前筛选的内容。" : "服务器尚未导出记忆内容。"));
      byId("memoryDetail").replaceChildren(empty("选择记忆节点", "查看内容、版本与 Nocturne 投递状态。"));
      return;
    }
    const groups = [
      ["当前记忆", filtered.filter((item) => item.record?.status === "applied")],
      ["历史与队列", filtered.filter((item) => item.record?.status !== "applied")]
    ];
    groups.forEach(([label, members]) => {
      if (!members.length) return;
      tree.append(text("div", label, "tree-group-label"));
      members.forEach((item) => {
        const record = item.record || {};
        const title = firstMeaningfulLine(item.content) || `记忆 ${shortID(record.logical_memory_id)}`;
        const node = document.createElement("button");
        node.type = "button";
        node.className = `tree-node${state.selectedMemory === record.id ? " active" : ""}`;
        node.setAttribute("aria-pressed", String(state.selectedMemory === record.id));
        const main = document.createElement("span");
        main.className = "tree-node-main";
        main.append(text("strong", title), text("span", `${shortID(record.logical_memory_id)} · 版本 ${record.revision || 0}`));
        node.append(main, text("span", statusLabel(item.delivery_status), `tree-node-meta status-badge ${statusClass(item.delivery_status)}`));
        node.addEventListener("click", () => {
          state.selectedMemory = record.id;
          renderMemory();
          renderMemoryDetail(item);
        });
        tree.append(node);
      });
    });
    const selected = filtered.find((item) => item.record?.id === state.selectedMemory) || filtered[0];
    state.selectedMemory = selected.record?.id || "";
    renderMemoryDetail(selected);
  }

  function renderMemoryDetail(item) {
    const host = byId("memoryDetail");
    const record = item.record || {};
    host.replaceChildren();
    const heading = document.createElement("div");
    heading.className = "section-heading";
    const titleWrap = document.createElement("div");
    titleWrap.append(text("p", "MEMORY NODE", "section-kicker"), text("h2", firstMeaningfulLine(item.content) || `记忆 ${shortID(record.logical_memory_id)}`));
    heading.append(titleWrap, text("span", statusLabel(record.status), `status-badge ${statusClass(record.status)}`));
    const facts = document.createElement("div");
    facts.className = "detail-grid";
    [
      ["逻辑 ID", record.logical_memory_id || "", ""],
      ["版本", String(record.revision || 0), ""],
      ["Nocturne", statusLabel(item.delivery_status), item.delivery_status],
      ["内容状态", statusLabel(item.content_status), item.content_status],
      ["创建时间", formatDate(record.created_at), ""],
      ["回执", item.receipt?.status ? statusLabel(item.receipt.status) : "-", item.receipt?.status || ""]
    ].forEach(([key, value, status]) => {
      const fact = document.createElement("div");
      fact.className = "detail-fact";
      const valueNode = status ? text("strong", value, `status-badge ${statusClass(status)}`) : text("strong", value || "-");
      fact.append(text("span", key), valueNode);
      facts.append(fact);
    });
    const content = document.createElement("div");
    content.className = "detail-content";
    content.append(text("h3", "记忆内容"), text("pre", item.content || "内容当前不可用。", "content-preview"));
    host.append(heading, facts, content);
  }

  async function loadKnowledge(force) {
    if (!force && state.knowledge) {
      renderKnowledge();
      return;
    }
    state.knowledge = await api("/admin/api/knowledge");
    renderKnowledge();
  }

  function renderKnowledge() {
    const data = state.knowledge || {};
    const documents = data.export?.documents || [];
    const nodes = data.tree?.revision?.documents?.flatMap((document) => document.document?.nodes || []) || [];
    renderMetrics("knowledgeStats", [["当前修订", data.head?.revision_no || 0], ["文档", documents.length], ["知识节点", nodes.length], ["更新时间", data.head ? formatDate(data.head.created_at) : "-"]]);
    const query = byId("knowledgeSearch").value.trim().toLowerCase();
    const filtered = documents.filter((document) => `${document.path} ${document.markdown}`.toLowerCase().includes(query));
    const tree = byId("knowledgeTree");
    tree.replaceChildren();
    if (!filtered.length) {
      tree.append(empty("知识库为空", query ? "没有匹配当前筛选的文档。" : "还没有可浏览的知识文档。"));
      byId("downloadKnowledge").disabled = true;
      byId("knowledgeDetailTitle").textContent = "没有匹配文档";
      byId("knowledgeContent").textContent = query ? "调整筛选条件后再选择文档。" : "知识库当前为空。";
      state.selectedKnowledgePath = "";
      state.selectedKnowledgeNode = "";
      return;
    }
    filtered.forEach((knowledgeDocument) => {
      const documentTree = data.tree?.revision?.documents?.find((entry) => entry.path === knowledgeDocument.path);
      const nodeCount = documentTree?.document?.nodes?.length || 0;
      const node = document.createElement("button");
      node.type = "button";
      const documentActive = state.selectedKnowledgePath === knowledgeDocument.path && !state.selectedKnowledgeNode;
      node.className = `tree-node${documentActive ? " active" : ""}`;
      node.setAttribute("aria-pressed", String(documentActive));
      const main = document.createElement("span");
      main.className = "tree-node-main";
      main.append(text("strong", knowledgeDocument.path), text("span", `${nodeCount} 个结构节点`));
      node.append(main, text("span", `${byteSize(knowledgeDocument.markdown)} KB`, "tree-node-meta"));
      node.addEventListener("click", () => {
        state.selectedKnowledgePath = knowledgeDocument.path;
        state.selectedKnowledgeNode = "";
        renderKnowledge();
      });
      tree.append(node);
      if (documentTree?.document?.nodes?.length) {
        documentTree.document.nodes.filter((entry) => entry.title).slice(0, 80).forEach((entry) => {
          const child = document.createElement("button");
          child.type = "button";
          const childActive = state.selectedKnowledgePath === knowledgeDocument.path && state.selectedKnowledgeNode === entry.node_revision_id;
          child.className = `tree-node${childActive ? " active" : ""}`;
          child.setAttribute("aria-pressed", String(childActive));
          child.style.paddingLeft = `${Math.min(48, 10 + (entry.heading_level || 1) * 8)}px`;
          const childMain = document.createElement("span");
          childMain.className = "tree-node-main";
          childMain.append(text("strong", entry.title), text("span", `第 ${entry.heading_range?.start_line || 0} 行`));
          child.append(childMain);
          child.addEventListener("click", () => {
            state.selectedKnowledgePath = knowledgeDocument.path;
            state.selectedKnowledgeNode = entry.node_revision_id;
            renderKnowledge();
          });
          tree.append(child);
        });
      }
    });
    const selected = filtered.find((document) => document.path === state.selectedKnowledgePath) || filtered[0];
    state.selectedKnowledgePath = selected.path;
    const selectedTree = data.tree?.revision?.documents?.find((entry) => entry.path === selected.path);
    const selectedNode = selectedTree?.document?.nodes?.find((entry) => entry.node_revision_id === state.selectedKnowledgeNode);
    if (!selectedNode) state.selectedKnowledgeNode = "";
    byId("knowledgeDetailTitle").textContent = selectedNode ? `${selected.path} · ${selectedNode.title}` : selected.path;
    byId("knowledgeContent").textContent = selectedNode ? markdownSection(selected.markdown, selectedNode.section_range) : (selected.markdown || "");
    byId("downloadKnowledge").disabled = false;
  }

  function downloadSelectedKnowledge() {
    const documentData = state.knowledge?.export?.documents?.find((entry) => entry.path === state.selectedKnowledgePath);
    if (!documentData) return;
    const blob = new Blob([documentData.markdown || ""], { type: "text/markdown;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = documentData.path.split("/").pop() || "knowledge.md";
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  async function loadNotesync(force) {
    if (!force && state.notesync) {
      renderNotesync();
      return;
    }
    const settings = await api("/admin/api/notesync");
    let reviews = { items: [], next_cursor: "" };
    let reviewsError = "";
    try {
      reviews = await api("/admin/api/notesync/reviews");
    } catch (error) {
      reviewsError = error.message;
    }
    state.notesync = {
      ...settings,
      reviews: reviews.items || [],
      reviewsNextCursor: reviews.next_cursor || "",
      reviewsError,
      preview: state.notesync?.preview || null,
      previewPath: state.notesync?.previewPath || ""
    };
    renderNotesync();
  }

  async function loadMoreNotesyncReviews() {
    const cursor = state.notesync?.reviewsNextCursor;
    if (!cursor) return;
    const control = byId("loadMoreNotesyncReviews");
    control.disabled = true;
    try {
      const page = await api(`/admin/api/notesync/reviews?cursor=${encodeURIComponent(cursor)}`);
      state.notesync.reviews = [...(state.notesync.reviews || []), ...(page.items || [])];
      state.notesync.reviewsNextCursor = page.next_cursor || "";
      state.notesync.reviewsError = "";
      renderNotesyncReviews(state.notesync.reviews, "");
    } catch (error) {
      showNotice(error.message, "danger");
    } finally {
      control.disabled = false;
    }
  }

  function renderNotesync() {
    const data = state.notesync || {};
    const active = data.active || {};
    const saved = data.saved || active;
    const runtime = data.runtime || {};
    const reviewCount = data.reviewsNextCursor ? `${(data.reviews || []).length}+` : (data.reviews || []).length;
    renderMetrics("notesyncStats", [["运行状态", runtime.configured ? (runtime.compatible ? "可用" : "不兼容") : "未启用"], ["版本", runtime.version || "-"], ["Vault", runtime.vault || saved.vault || "-"], ["待审阅", data.reviewsError ? "加载失败" : reviewCount]]);
    byId("notesyncEnabled").checked = Boolean(saved.enabled);
    byId("notesyncBaseURL").value = saved.base_url || "";
    byId("notesyncToken").value = "";
    byId("notesyncToken").placeholder = saved.api_key_configured ? "已保存，留空保持不变" : "输入 API 密钥";
    byId("notesyncVault").value = saved.vault || "";
    byId("notesyncPrefix").value = saved.path_prefix || "";
    ["notesyncEnabled", "notesyncBaseURL", "notesyncToken", "notesyncVault", "notesyncPrefix", "notesyncSave"].forEach((id) => byId(id).disabled = !data.settings_writable);
    const saveState = byId("notesyncSaveState");
    saveState.className = `status-badge ${data.restart_required ? "warning" : runtime.compatible ? "success" : "neutral"}`;
    saveState.textContent = data.restart_required ? "等待重启" : data.settings_writable ? "已生效" : "只读";
    if (data.restart_required) showNotice("NoteSync 配置已保存，重启服务器后生效。", "warning");
    renderNotesyncReviews(data.reviews || [], data.reviewsError);
    if (data.preview) renderNotesyncPreview(data.preview);
  }

  async function saveNotesync(event) {
    event.preventDefault();
    const save = byId("notesyncSave");
    save.disabled = true;
    try {
      const payload = await api("/admin/api/notesync/settings", {
        method: "POST", csrf: true,
        body: JSON.stringify({
          enabled: byId("notesyncEnabled").checked,
          base_url: byId("notesyncBaseURL").value.trim(),
          api_token: byId("notesyncToken").value,
          vault: byId("notesyncVault").value.trim(),
          path_prefix: byId("notesyncPrefix").value.trim()
        })
      });
      state.notesync = {
        ...payload,
        reviews: state.notesync?.reviews || [],
        reviewsNextCursor: state.notesync?.reviewsNextCursor || "",
        reviewsError: state.notesync?.reviewsError || "",
        preview: state.notesync?.preview || null,
        previewPath: state.notesync?.previewPath || ""
      };
      renderNotesync();
      showNotice(payload.restart_required ? "配置已保存，重启服务器后生效。" : "配置已保存。", "success");
    } catch (error) {
      showNotice(error.message, "danger");
    } finally {
      save.disabled = !state.notesync?.settings_writable;
    }
  }

  async function previewNotesync(event) {
    event.preventDefault();
    await loadNotesyncPreviewPage(1, false);
  }

  async function loadMoreNotesyncPreview() {
    const nextPage = state.notesync?.preview?.next_page;
    if (nextPage) await loadNotesyncPreviewPage(nextPage, true);
  }

  async function loadNotesyncPreviewPage(page, append) {
    const control = byId("loadMoreNotesyncPreview");
    control.disabled = true;
    const path = append ? (state.notesync?.previewPath || "") : byId("notesyncPreviewPath").value.trim();
    try {
      const preview = await api("/admin/api/notesync/preview", {
        method: "POST", csrf: true,
        body: JSON.stringify({ path, page, page_size: 25 })
      });
      if (!state.notesync) state.notesync = {};
      const priorItems = append ? (state.notesync.preview?.items || []) : [];
      state.notesync.preview = { ...preview, items: [...priorItems, ...(preview.items || [])] };
      state.notesync.previewPath = path;
      renderNotesyncPreview(state.notesync.preview);
    } catch (error) {
      showNotice(error.message, "danger");
    } finally {
      control.disabled = false;
    }
  }

  function renderNotesyncPreview(preview) {
    const host = byId("notesyncPreview");
    const loadMore = byId("loadMoreNotesyncPreview");
    loadMore.hidden = !preview?.next_page;
    host.replaceChildren();
    const items = preview?.items || [];
    if (!items.length) {
      host.append(empty("没有同步差异", "当前路径没有需要处理的 NoteSync 变化。"));
      return;
    }
    items.forEach((item) => host.append(compactItem(
      item.remote_path || item.local?.path || "未命名文档",
      `${statusLabel(item.category)} · ${reasonLabel(item.reason_code)}`,
      statusLabel(item.category),
      statusClass(item.category)
    )));
  }

  function renderNotesyncReviews(reviews, errorMessage) {
    const host = byId("notesyncReviews");
    byId("loadMoreNotesyncReviews").hidden = Boolean(errorMessage) || !state.notesync?.reviewsNextCursor;
    host.replaceChildren();
    if (errorMessage) {
      host.append(empty("审阅队列加载失败", errorMessage));
      return;
    }
    if (!reviews.length) {
      host.append(empty("审阅队列为空", "当前没有待处理的同步冲突。"));
      return;
    }
    reviews.forEach((review) => host.append(compactItem(
      review.canonical_path || review.remote_path || shortID(review.review_id),
      `${statusLabel(review.status)} · ${statusLabel(review.category)} · ${reasonLabel(review.reason_code)} · ${formatDate(review.updated_at)}`,
      statusLabel(review.status),
      statusClass(review.status),
      statusLabel(review.category),
      statusClass(review.category)
    )));
  }

  function compactItem(title, subtitle, label, kind, secondaryLabel = "", secondaryKind = "") {
    const item = document.createElement("div");
    item.className = "compact-item";
    const main = document.createElement("div");
    main.append(text("strong", title), text("span", subtitle));
    const badges = document.createElement("div");
    badges.className = "compact-badges";
    badges.append(text("span", label, `status-badge ${kind}`));
    if (secondaryLabel) badges.append(text("span", secondaryLabel, `status-badge ${secondaryKind}`));
    item.append(main, badges);
    return item;
  }

  function renderMetrics(id, entries) {
    byId(id).replaceChildren(...entries.map(([label, value]) => {
      const metric = document.createElement("div");
      metric.className = "metric";
      metric.append(text("span", label), text("strong", String(value)));
      return metric;
    }));
  }

  function empty(title, detail) {
    const node = document.createElement("div");
    node.className = "empty-state";
    node.append(text("strong", title), text("span", detail));
    return node;
  }

  function componentLabel(value) {
    return ({ postgresql: "PostgreSQL", model: "教学模型", open_evaluation_worker: "评估 Worker", offline_signer: "离线签名", offline_protocol: "离线协议", nocturne: "Nocturne Memory", notesync: "NoteSync" }[value] || value || "未知组件");
  }

  function markdownSection(markdown, range) {
    const lines = String(markdown || "").split(/\r?\n/);
    const start = Math.max(0, Number(range?.start_line || 1) - 1);
    const endLine = Number(range?.end_line || 0);
    const end = endLine > start ? Math.min(lines.length, endLine) : Math.min(lines.length, start + 80);
    return lines.slice(start, end).join("\n");
  }

  function firstMeaningfulLine(value) {
    return String(value || "").split(/\r?\n/).map((line) => line.replace(/^#+\s*/, "").trim()).find(Boolean)?.slice(0, 90) || "";
  }

  function byteSize(value) {
    return Math.max(0.1, new TextEncoder().encode(value || "").length / 1024).toFixed(1);
  }

  function confirmAction(title, message) {
    const dialog = byId("confirmDialog");
    byId("confirmTitle").textContent = title;
    byId("confirmMessage").textContent = message;
    dialog.showModal();
    return new Promise((resolve) => {
      dialog.addEventListener("close", () => resolve(dialog.returnValue === "confirm"), { once: true });
    });
  }

  async function logout() {
    try {
      await api("/admin/api/logout", { method: "POST", csrf: true });
      showLogin();
    } catch (error) {
      if (document.visibilityState !== "hidden") showNotice(error.message, "danger");
    }
  }

  document.addEventListener("DOMContentLoaded", () => {
    byId("primaryNav").addEventListener("click", (event) => {
      const item = event.target.closest("[data-page]");
      if (item) navigate(item.dataset.page);
    });
    byId("refreshPage").addEventListener("click", () => loadPage(state.activePage, true));
    byId("refreshDevices").addEventListener("click", () => loadDevices(true));
    byId("deviceSearch").addEventListener("input", renderDevices);
    byId("memorySearch").addEventListener("input", renderMemory);
    byId("loadMoreMemory").addEventListener("click", loadMoreMemory);
    byId("knowledgeSearch").addEventListener("input", renderKnowledge);
    byId("pairingForm").addEventListener("submit", createPairing);
    byId("downloadKnowledge").addEventListener("click", downloadSelectedKnowledge);
    byId("notesyncForm").addEventListener("submit", saveNotesync);
    byId("notesyncPreviewForm").addEventListener("submit", previewNotesync);
    byId("loadMoreNotesyncPreview").addEventListener("click", loadMoreNotesyncPreview);
    byId("loadMoreNotesyncReviews").addEventListener("click", loadMoreNotesyncReviews);
    byId("logoutButton").addEventListener("click", logout);
    byId("mobileLogoutButton").addEventListener("click", logout);
    window.addEventListener("hashchange", () => {
      const page = location.hash.slice(1);
      if (validPages.has(page) && page !== state.activePage) navigate(page, true);
    });
    window.addEventListener("admin-authenticated", bootstrapConsole);
    bootstrapConsole();
  });
})();
