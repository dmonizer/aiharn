(() => {
  "use strict";

  const ENDPOINTS_KEY = "aiharn.remote.apis.v1";
  const ACTIVE_KEY = "aiharn.remote.active.v1";
  const TOKEN_PREFIX = "aiharn.remote.token.";
  const POLL_MS = 1000;

  const elements = {
    apiList: document.querySelector("#api-list"),
    agentList: document.querySelector("#agent-list"),
    transcript: document.querySelector("#transcript"),
    approvals: document.querySelector("#approvals"),
    connection: document.querySelector("#connection-state"),
    endpointLabel: document.querySelector("#endpoint-label"),
    agentTitle: document.querySelector("#agent-title"),
    sessionMeta: document.querySelector("#session-meta"),
    composer: document.querySelector("#composer"),
    message: document.querySelector("#message"),
    send: document.querySelector("#send"),
    queue: document.querySelector("#queue-state"),
    apiDialog: document.querySelector("#api-dialog"),
    apiForm: document.querySelector("#api-form"),
    apiName: document.querySelector("#api-name"),
    apiUrl: document.querySelector("#api-url"),
    apiToken: document.querySelector("#api-token"),
    tokenDialog: document.querySelector("#token-dialog"),
    tokenForm: document.querySelector("#token-form"),
    tokenValue: document.querySelector("#token-value"),
    tokenLabel: document.querySelector("#token-api-label")
  };

  const state = {
    endpoints: loadEndpoints(),
    activeId: localStorage.getItem(ACTIVE_KEY),
    snapshots: new Map(),
    selectedAgents: new Map(),
    drafts: new Map(),
    health: new Map(),
    request: null,
    timer: null,
    fingerprint: "",
    connected: false
  };

  if (!state.endpoints.some((item) => item.id === state.activeId)) {
    state.activeId = state.endpoints[0]?.id || "";
  }

  function normalizeURL(value) {
    const url = new URL(value || location.origin, location.href);
    if (url.protocol !== "http:" && url.protocol !== "https:") {
      throw new Error("API URL must use HTTP or HTTPS");
    }
    url.hash = "";
    url.search = "";
    url.pathname = url.pathname.replace(/\/api\/v1\/?$/, "").replace(/\/$/, "");
    return url.toString().replace(/\/$/, "");
  }

  function hash(value) {
    let result = 2166136261;
    for (let i = 0; i < value.length; i += 1) {
      result ^= value.charCodeAt(i);
      result = Math.imul(result, 16777619);
    }
    return (result >>> 0).toString(36);
  }

  function deployedEndpoints() {
    const configured = Array.isArray(window.AIHARN_CONFIG?.apis) ? window.AIHARN_CONFIG.apis : [];
    return configured.flatMap((item) => {
      try {
        const url = normalizeURL(item.url);
        return [{ id: "deployed-" + hash(url), name: item.name || new URL(url).host, url, deployed: true }];
      } catch {
        return [];
      }
    });
  }

  function savedEndpoints() {
    try {
      const parsed = JSON.parse(localStorage.getItem(ENDPOINTS_KEY) || "[]");
      if (!Array.isArray(parsed)) return [];
      return parsed.flatMap((item) => {
        try {
          const url = normalizeURL(item.url);
          const fallbackID = crypto.randomUUID?.() || Date.now().toString(36) +
            Math.random().toString(36).slice(2);
          return [{ id: String(item.id || fallbackID),
            name: String(item.name || new URL(url).host), url, deployed: false }];
        } catch {
          return [];
        }
      });
    } catch {
      return [];
    }
  }

  function loadEndpoints() {
    const merged = new Map();
    for (const endpoint of [...deployedEndpoints(), ...savedEndpoints()]) {
      if (![...merged.values()].some((item) => item.url === endpoint.url)) {
        merged.set(endpoint.id, endpoint);
      }
    }
    return [...merged.values()];
  }

  function persistEndpoints() {
    const local = state.endpoints
      .filter((endpoint) => !endpoint.deployed)
      .map(({ id, name, url }) => ({ id, name, url }));
    localStorage.setItem(ENDPOINTS_KEY, JSON.stringify(local));
  }

  function activeEndpoint() {
    return state.endpoints.find((endpoint) => endpoint.id === state.activeId);
  }

  function selectedAgentID() {
    return state.selectedAgents.get(state.activeId) || "";
  }

  function draftKey(endpointID = state.activeId, agentID = selectedAgentID()) {
    return endpointID + ":" + agentID;
  }

  function tokenFor(endpoint) {
    return endpoint ? sessionStorage.getItem(TOKEN_PREFIX + endpoint.id) || "" : "";
  }

  function setToken(endpoint, value) {
    if (!endpoint) return;
    if (value) sessionStorage.setItem(TOKEN_PREFIX + endpoint.id, value);
    else sessionStorage.removeItem(TOKEN_PREFIX + endpoint.id);
  }

  async function apiCall(endpoint, path, options = {}) {
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");
    const token = tokenFor(endpoint);
    if (token) headers.set("Authorization", "Bearer " + token);
    if (options.body) headers.set("Content-Type", "application/json");

    const response = await fetch(endpoint.url + "/api/v1" + path, {
      ...options,
      headers,
      cache: "no-store",
      signal: options.signal
    });
    let payload = {};
    try {
      payload = await response.json();
    } catch {
      payload = {};
    }
    if (!response.ok) {
      const error = new Error(payload.error || "API request failed (" + response.status + ")");
      error.status = response.status;
      throw error;
    }
    return payload;
  }

  function renderEndpointList() {
    elements.apiList.replaceChildren();
    for (const endpoint of state.endpoints) {
      const item = document.createElement("div");
      item.className = "api-item" + (endpoint.id === state.activeId ? " active" : "");
      item.tabIndex = 0;
      item.setAttribute("role", "button");
      item.addEventListener("click", () => selectEndpoint(endpoint.id));
      item.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") selectEndpoint(endpoint.id);
      });

      const dot = document.createElement("span");
      dot.className = "api-dot " + (state.health.get(endpoint.id) || "");
      const copy = document.createElement("span");
      copy.className = "api-copy";
      const name = document.createElement("strong");
      name.textContent = endpoint.name;
      const url = document.createElement("span");
      url.textContent = endpoint.url;
      copy.append(name, url);
      const remove = document.createElement("button");
      remove.className = "remove-api";
      remove.type = "button";
      remove.textContent = endpoint.deployed ? "⌁" : "×";
      remove.title = endpoint.deployed ? "Configured by deployment" : "Remove API";
      remove.disabled = endpoint.deployed;
      remove.addEventListener("click", (event) => {
        event.stopPropagation();
        removeEndpoint(endpoint.id);
      });
      item.append(dot, copy, remove);
      elements.apiList.append(item);
    }
  }

  function renderAgentList(agents, selectedID) {
    elements.agentList.replaceChildren();
    for (const agent of agents) {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "agent-item depth-" + Math.min(agent.depth || 0, 3) +
        (agent.id === selectedID ? " active" : "");
      item.setAttribute("aria-current", agent.id === selectedID ? "true" : "false");
      item.addEventListener("click", () => selectAgent(agent.id));
      const status = agent.paused ? "paused" : agent.state;
      const dot = document.createElement("span");
      dot.className = "agent-dot " + status;
      const copy = document.createElement("span");
      copy.className = "agent-copy";
      const name = document.createElement("strong");
      name.textContent = agent.depth ? agent.type : "Main · " + agent.type;
      const id = document.createElement("span");
      id.textContent = agent.id;
      copy.append(name, id);
      const state = document.createElement("span");
      state.className = "agent-state " + status;
      state.textContent = status;
      item.append(dot, copy, state);
      elements.agentList.append(item);
    }
  }

  function selectAgent(id) {
    const endpoint = activeEndpoint();
    if (!endpoint || selectedAgentID() === id) return;
    state.drafts.set(draftKey(), elements.message.value);
    state.selectedAgents.set(endpoint.id, id);
    elements.message.value = state.drafts.get(draftKey()) || "";
    state.fingerprint = "";
    state.request?.abort();
    renderAgentList(state.snapshots.get(endpoint.id)?.agents || [], id);
    elements.agentTitle.textContent = id;
    elements.message.disabled = true;
    elements.send.disabled = true;
    elements.transcript.replaceChildren(emptyState("Loading agent…", id));
    poll();
    document.body.classList.remove("menu-open");
  }

  function selectEndpoint(id) {
    if (state.activeId === id) {
      document.body.classList.remove("menu-open");
      return;
    }
    state.drafts.set(draftKey(), elements.message.value);
    state.activeId = id;
    elements.message.value = state.drafts.get(draftKey()) || "";
    localStorage.setItem(ACTIVE_KEY, id);
    state.fingerprint = "";
    state.connected = false;
    state.request?.abort();
    clearTimeout(state.timer);
    renderEndpointList();
    renderWaiting();
    poll();
    document.body.classList.remove("menu-open");
  }

  function removeEndpoint(id) {
    const endpoint = state.endpoints.find((item) => item.id === id);
    if (!endpoint || endpoint.deployed) return;
    setToken(endpoint, "");
    state.endpoints = state.endpoints.filter((item) => item.id !== id);
    persistEndpoints();
    if (state.activeId === id) {
      state.activeId = state.endpoints[0]?.id || "";
      localStorage.setItem(ACTIVE_KEY, state.activeId);
      state.fingerprint = "";
      poll();
    }
    renderEndpointList();
    if (!state.activeId) renderWaiting();
  }

  function showConnection(message) {
    elements.connection.textContent = message;
    elements.connection.classList.remove("hidden");
  }

  function hideConnection() {
    elements.connection.classList.add("hidden");
    elements.connection.textContent = "";
  }

  function renderWaiting() {
    const endpoint = activeEndpoint();
    elements.endpointLabel.textContent = endpoint?.name || "No API selected";
    elements.agentTitle.textContent = endpoint ? "Connecting…" : "Current session";
    elements.sessionMeta.replaceChildren();
    elements.agentList.replaceChildren();
    elements.approvals.replaceChildren();
    elements.message.disabled = true;
    elements.send.disabled = true;
    elements.queue.textContent = "";
    state.connected = false;
    elements.transcript.replaceChildren(emptyState(
      endpoint ? "Connecting to Aiharn…" : "Connect an Aiharn API",
      endpoint ? endpoint.url : "Add one or more active instances, then switch between them from the sidebar."
    ));
  }

  function emptyState(title, detail) {
    const box = document.createElement("div");
    box.className = "empty-state";
    const glyph = document.createElement("div");
    glyph.className = "empty-glyph";
    glyph.textContent = "⌁";
    const heading = document.createElement("h2");
    heading.textContent = title;
    const copy = document.createElement("p");
    copy.textContent = detail;
    box.append(glyph, heading, copy);
    if (!activeEndpoint()) {
      const button = document.createElement("button");
      button.className = "primary-button";
      button.textContent = "Add API";
      button.addEventListener("click", openAPIDialog);
      box.append(button);
    }
    return box;
  }

  function chip(text, className = "") {
    const item = document.createElement("span");
    item.className = "meta-chip " + className;
    item.textContent = text;
    return item;
  }

  function renderSnapshot(snapshot) {
    const endpoint = activeEndpoint();
    const session = snapshot.session || {};
    if (endpoint && !state.selectedAgents.has(endpoint.id)) {
      state.selectedAgents.set(endpoint.id, session.agent_id);
    }
    elements.endpointLabel.textContent = endpoint?.name || "Aiharn";
    elements.agentTitle.textContent = session.agent_id
      ? (session.agent_type && session.agent_type !== session.agent_id
        ? session.agent_type + " · " + session.agent_id : session.agent_id)
      : "Current session";
    elements.message.placeholder = "Message " + (session.agent_id || "Aiharn") + "…";
    renderAgentList(snapshot.agents?.length ? snapshot.agents : [{
      id: session.agent_id, type: session.agent_type,
      state: session.state, depth: 0, paused: false
    }], session.agent_id);
    elements.sessionMeta.replaceChildren(
      ...(session.model ? [chip(session.model)] : []),
      ...(session.channel ? [chip(session.channel)] : []),
      chip(session.approval_mode || "ask"),
      chip(session.state || "unknown", "state-" + (session.state || "unknown"))
    );
    elements.queue.textContent = snapshot.queued_messages
      ? snapshot.queued_messages + " queued"
      : "";
    const unavailable = session.state === "closed" || session.state === "errored";
    elements.message.disabled = unavailable;
    elements.send.disabled = unavailable || !elements.message.value.trim();
    renderApprovals(snapshot.pending_approvals || []);
    renderMessages(snapshot.messages || []);
    if (snapshot.last_error) showConnection(snapshot.last_error);
    else hideConnection();
    state.connected = true;
  }

  function renderMessages(messages) {
    const previousTop = elements.transcript.scrollTop;
    const nearBottom = elements.transcript.scrollHeight - previousTop -
      elements.transcript.clientHeight < 100;
    const expandedTools = new Set(Array.from(
      elements.transcript.querySelectorAll(".tool-block[open]"),
      (block) => block.dataset.toolKey
    ));
    elements.transcript.replaceChildren();
    if (!messages.length) {
      elements.transcript.append(emptyState("Session is ready", "Send a message to begin."));
      return;
    }

    for (const entry of groupMessages(messages)) {
      const message = entry.message;
      if (message.type === "message") {
        const row = document.createElement("div");
        row.className = "message-row " + (message.role || "");
        const body = document.createElement("div");
        body.className = "message";
        if (message.role !== "user") {
          const role = document.createElement("div");
          role.className = "message-role";
          role.textContent = message.role === "assistant" ? "Aiharn" : message.role;
          body.append(role);
        }
        body.append(document.createTextNode(message.content || ""));
        row.append(body);
        elements.transcript.append(row);
      } else {
        elements.transcript.append(renderToolBlock(entry, expandedTools));
      }
    }
    elements.transcript.scrollTop = nearBottom
      ? elements.transcript.scrollHeight
      : previousTop;
  }

  function groupMessages(messages) {
    const entries = [];
    const pending = new Map();
    const callCounts = new Map();
    for (const [index, message] of messages.entries()) {
      if (message.type === "tool_call") {
        const callID = message.call_id || "";
        const count = callCounts.get(callID) || 0;
        callCounts.set(callID, count + 1);
        const entry = { message, result: null, key: "call:" + callID + ":" + count };
        entries.push(entry);
        if (!pending.has(callID)) pending.set(callID, []);
        pending.get(callID).push(entry);
      } else if (message.type === "tool_result") {
        const callID = message.call_id || "";
        const waiting = pending.get(callID);
        if (waiting?.length) waiting.shift().result = message;
        else entries.push({ message, result: null, key: "result:" + callID + ":" + index });
      } else {
        entries.push({ message });
      }
    }
    return entries;
  }

  function renderToolBlock(entry, expandedTools) {
    const call = entry.message.type === "tool_call" ? entry.message : null;
    const result = entry.result || (call ? null : entry.message);
    const resultText = result?.content || "";
    const status = !result ? "pending" : resultText.trim().toLowerCase() === "denied by user"
      ? "denied" : resultText.startsWith("error:") ? "error" : "executed";
    const details = document.createElement("details");
    details.className = "tool-block " + status;
    details.dataset.toolKey = entry.key;
    details.open = expandedTools.has(entry.key);

    const summary = document.createElement("summary");
    summary.setAttribute("aria-label", status + " · " + (call?.name || "tool result"));
    const icon = document.createElement("span");
    icon.className = "tool-status-icon " + status;
    icon.textContent = { pending: "…", denied: "🛑", error: "!", executed: "✓" }[status];
    icon.title = status;
    icon.setAttribute("aria-hidden", "true");
    const title = document.createElement("span");
    title.textContent = call ? "tool · " + (call.name || "unknown") :
      "tool result · " + (result.call_id || "unknown");
    summary.append(icon, title);
    details.append(summary);

    if (call) appendToolSection(details, "Call", call.arguments);
    if (result) appendToolSection(details, "Result", result.content);
    return details;
  }

  function appendToolSection(details, label, value) {
    const section = document.createElement("div");
    section.className = "tool-section";
    const heading = document.createElement("div");
    heading.className = "tool-section-label";
    heading.textContent = label;
    const pre = document.createElement("pre");
    pre.textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2);
    section.append(heading, pre);
    details.append(section);
  }

  function renderApprovals(approvals) {
    elements.approvals.replaceChildren();
    for (const approval of approvals) {
      const card = document.createElement("article");
      card.className = "approval-card";
      const content = document.createElement("div");
      const heading = document.createElement("div");
      heading.className = "approval-heading";
      const label = document.createElement("div");
      label.className = "approval-label";
      label.textContent = "Permission required · " + approval.tool_name;
      const agent = document.createElement("div");
      agent.className = "approval-agent";
      const agentType = document.createElement("span");
      agentType.textContent = "Agent: " + (approval.agent_type || "unknown");
      const agentID = document.createElement("span");
      agentID.className = "approval-agent-id";
      agentID.textContent = "ID: " + (approval.agent_id || "unknown");
      agent.append(agentType, agentID);
      heading.append(label, agent);
      const command = document.createElement("pre");
      command.textContent = approval.command || JSON.stringify(approval.arguments, null, 2);
      content.append(heading, command);

      const actions = document.createElement("div");
      actions.className = "approval-actions";
      for (const [text, decision, className] of [
        ["Deny", "deny", "deny"],
        ["Allow once", "approve", "approve"],
        ["Always allow", "approve_all", "approve"]
      ]) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = className;
        button.textContent = text;
        button.addEventListener("click", () => resolveApproval(approval.id, decision, button));
        actions.append(button);
      }
      card.append(content, actions);
      elements.approvals.append(card);
    }
  }

  async function resolveApproval(id, decision, button) {
    const endpoint = activeEndpoint();
    if (!endpoint) return;
    for (const sibling of button.parentElement.children) sibling.disabled = true;
    try {
      await apiCall(endpoint, "/approvals/" + encodeURIComponent(id), {
        method: "POST", body: JSON.stringify({ decision })
      });
      await poll(true);
    } catch (error) {
      showConnection(error.message);
      for (const sibling of button.parentElement.children) sibling.disabled = false;
    }
  }

  async function poll(immediate = false) {
    clearTimeout(state.timer);
    const endpoint = activeEndpoint();
    if (!endpoint) {
      renderWaiting();
      return;
    }
    state.request?.abort();
    const request = new AbortController();
    state.request = request;
    try {
      const agentID = selectedAgentID();
      const path = "/session" + (agentID ? "?agent_id=" + encodeURIComponent(agentID) : "");
      const snapshot = await apiCall(endpoint, path, { signal: request.signal });
      if (endpoint.id !== state.activeId) return;
      if (agentID !== selectedAgentID()) return;
      state.health.set(endpoint.id, "online");
      const fingerprint = JSON.stringify(snapshot);
      if (fingerprint !== state.fingerprint) {
        state.fingerprint = fingerprint;
        state.snapshots.set(endpoint.id, snapshot);
        renderSnapshot(snapshot);
      }
      renderEndpointList();
    } catch (error) {
      if (error.name === "AbortError") return;
      if (error.status === 404 && selectedAgentID()) {
        state.selectedAgents.delete(endpoint.id);
        state.fingerprint = "";
        state.connected = false;
        elements.message.disabled = true;
        elements.send.disabled = true;
        showConnection("Selected agent is no longer available; returning to main.");
        return;
      }
      state.health.set(endpoint.id, "error");
      state.fingerprint = "";
      state.connected = false;
      elements.message.disabled = true;
      elements.send.disabled = true;
      showConnection(error.status === 401
        ? "Authentication required. Set the access token for this API."
        : error.message);
      renderEndpointList();
    } finally {
      if (endpoint.id === state.activeId) {
        state.timer = setTimeout(poll, immediate ? 50 : POLL_MS);
      }
    }
  }

  async function sendMessage(event) {
    event?.preventDefault();
    const endpoint = activeEndpoint();
    const agentID = selectedAgentID();
    const snapshot = endpoint && state.snapshots.get(endpoint.id);
    const rootID = snapshot?.agents?.[0]?.id || snapshot?.session?.agent_id || "";
    const content = elements.message.value;
    if (!endpoint || !state.connected || elements.message.disabled || !content.trim()) return;
    elements.send.disabled = true;
    try {
      await apiCall(endpoint, "/messages", {
        method: "POST", body: JSON.stringify(messagePayload(content, agentID, rootID))
      });
      state.drafts.delete(draftKey(endpoint.id, agentID));
      if (endpoint.id === state.activeId && agentID === selectedAgentID() && elements.message.value === content) {
        elements.message.value = "";
        updateComposer();
      }
      await poll(true);
    } catch (error) {
      showConnection(error.message);
    } finally {
      elements.send.disabled = !state.connected || elements.message.disabled || !elements.message.value.trim();
    }
  }

  function messagePayload(content, agentID, rootID) {
    return agentID && agentID !== rootID ? { content, agent_id: agentID } : { content };
  }

  function openAPIDialog() {
    elements.apiForm.reset();
    elements.apiDialog.showModal();
    setTimeout(() => elements.apiName.focus(), 0);
  }

  function openTokenDialog() {
    const endpoint = activeEndpoint();
    if (!endpoint) return;
    elements.tokenLabel.textContent = endpoint.name + " · " + endpoint.url;
    elements.tokenValue.value = tokenFor(endpoint);
    elements.tokenDialog.showModal();
    setTimeout(() => elements.tokenValue.focus(), 0);
  }

  function updateComposer() {
    elements.send.disabled = !state.connected || elements.message.disabled || !elements.message.value.trim();
  }

  document.querySelectorAll("#add-api, #manage-api, .add-api-cta").forEach((button) => {
    button.addEventListener("click", openAPIDialog);
  });
  document.querySelectorAll(".close-dialog").forEach((button) => {
    button.addEventListener("click", () => elements.apiDialog.close());
  });
  document.querySelectorAll(".close-token-dialog").forEach((button) => {
    button.addEventListener("click", () => elements.tokenDialog.close());
  });
  document.querySelector("#set-token").addEventListener("click", openTokenDialog);
  document.querySelector("#mobile-menu").addEventListener("click", () => {
    document.body.classList.toggle("menu-open");
  });

  elements.apiForm.addEventListener("submit", (event) => {
    event.preventDefault();
    try {
      const url = normalizeURL(elements.apiUrl.value);
      let endpoint = state.endpoints.find((item) => item.url === url);
      if (!endpoint) {
        endpoint = {
          id: "user-" + (crypto.randomUUID?.() || Date.now().toString(36)),
          name: elements.apiName.value.trim() || new URL(url).host,
          url,
          deployed: false
        };
        state.endpoints.push(endpoint);
      } else if (!endpoint.deployed) {
        endpoint.name = elements.apiName.value.trim() || endpoint.name;
      }
      setToken(endpoint, elements.apiToken.value);
      persistEndpoints();
      elements.apiDialog.close();
      selectEndpoint(endpoint.id);
      renderEndpointList();
    } catch {
      elements.apiUrl.setCustomValidity("Enter a valid HTTP(S) API URL.");
      elements.apiUrl.reportValidity();
    }
  });
  elements.apiUrl.addEventListener("input", () => elements.apiUrl.setCustomValidity(""));

  elements.tokenForm.addEventListener("submit", (event) => {
    event.preventDefault();
    setToken(activeEndpoint(), elements.tokenValue.value);
    elements.tokenDialog.close();
    state.fingerprint = "";
    poll(true);
  });
  document.querySelector("#forget-token").addEventListener("click", () => {
    setToken(activeEndpoint(), "");
    elements.tokenValue.value = "";
    elements.tokenDialog.close();
    state.fingerprint = "";
    poll(true);
  });

  elements.composer.addEventListener("submit", sendMessage);
  elements.message.addEventListener("input", updateComposer);
  elements.message.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      sendMessage();
    }
  });

  const composerResize = document.querySelector(".composer-resize");
  let resizeStart = null;
  composerResize.addEventListener("pointerdown", (event) => {
    resizeStart = { y: event.clientY, h: elements.message.offsetHeight };
    composerResize.setPointerCapture(event.pointerId);
    document.body.classList.add("resizing");
  });
  composerResize.addEventListener("pointermove", (event) => {
    if (!resizeStart) return;
    const nearBottom = elements.transcript.scrollHeight - elements.transcript.scrollTop -
      elements.transcript.clientHeight < 100;
    const max = Math.round(window.innerHeight * 0.4);
    const height = Math.min(Math.max(resizeStart.h + (resizeStart.y - event.clientY), 40), max);
    elements.message.style.height = height + "px";
    if (nearBottom) elements.transcript.scrollTop = elements.transcript.scrollHeight;
  });
  const endResize = () => {
    resizeStart = null;
    document.body.classList.remove("resizing");
  };
  composerResize.addEventListener("pointerup", endResize);
  composerResize.addEventListener("pointercancel", endResize);

  renderEndpointList();
  renderWaiting();
  if (state.activeId) poll();
})();
