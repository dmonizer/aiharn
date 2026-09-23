(() => {
  "use strict";

  const ENDPOINT_KEY = "aiharn.remote.endpoint.v2";
  const CREDENTIALS_PREFIX = "aiharn.remote.credentials.v1:";
  const SESSION_KEY = "aiharn.remote.session.v2";
  const LEGACY_ENDPOINTS_KEY = "aiharn.remote.apis.v1";
  const LEGACY_ACTIVE_KEY = "aiharn.remote.active.v1";
  sessionStorage.removeItem("aiharn.remote.token.v2");
  for (const key of Object.keys(sessionStorage)) {
    if (key.startsWith("aiharn.remote.token.")) sessionStorage.removeItem(key);
  }
  const POLL_MS = 1000;
  const SESSIONS_POLL_MS = 5000;
  // A server built before the API reported its agent roster returns no "agents"
  // field at all; the console used to invent a one-entry list in that case,
  // which made a stale binary look like a healthy session with no subagents.
  const STALE_SERVER_NOTICE = "This API server is older than this console and does not " +
    "report agents. Rebuild and restart aiharn to see subagents.";
  // A server built before session management existed has no /sessions route and
  // rejects unknown JSON fields, so it would answer 400 to a payload carrying
  // session_id. The console detects that and omits the field, which keeps it
  // usable against an older backend.
  const LEGACY_SERVER_NOTICE = "This API server is older than this console: it has no session " +
    "support and does not report agents. Rebuild and restart aiharn to list, create, rename " +
    "and close sessions, and to see subagents.";

  const elements = {
    sessionList: document.querySelector("#session-list"),
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
    settingsDialog: document.querySelector("#settings-dialog"),
    settingsForm: document.querySelector("#settings-form"),
    settingsName: document.querySelector("#settings-name"),
    settingsUrl: document.querySelector("#settings-url"),
    settingsUsername: document.querySelector("#settings-username"),
    settingsPassword: document.querySelector("#settings-password"),
    settingsUrlHelp: document.querySelector("#settings-url-help"),
    settingsPasswordHelp: document.querySelector("#settings-password-help"),
    settingsError: document.querySelector("#settings-error"),
    sessionDialog: document.querySelector("#session-dialog"),
    sessionForm: document.querySelector("#session-form"),
    sessionTitle: document.querySelector("#session-dialog-title"),
    sessionName: document.querySelector("#session-name"),
    sessionSubmit: document.querySelector("#session-submit"),
    sessionError: document.querySelector("#session-error")
  };

  const state = {
    endpoint: loadEndpoint(),
    sessionId: localStorage.getItem(SESSION_KEY) || "",
    sessions: [],
    defaultSessionId: "",
    snapshots: new Map(),
    selectedAgents: new Map(),
    drafts: new Map(),
    request: null,
    sessionsRequest: null,
    timer: null,
    sessionsTimer: null,
    // sessionSupport is null until /sessions has been probed: true when the
    // server understands session_id, false when it is an older build.
    sessionSupport: null,
    fingerprint: "",
    connected: false,
    sessionMode: "create",
    renameTarget: ""
  };

  let openChannelMenu = null;

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

  // deploymentEndpoint returns the endpoint baked into config.js, accepting
  // either {endpoint: {...}} or the legacy {apis: [...]} shape. A deployment
  // endpoint wins over the one stored in this browser.
  function deploymentEndpoint() {
    const config = window.AIHARN_CONFIG || {};
    const candidate = config.endpoint ||
      (Array.isArray(config.apis) ? config.apis[0] : null);
    if (!candidate || !candidate.url) return null;
    try {
      const url = normalizeURL(candidate.url);
      return { name: candidate.name || new URL(url).host, url, deployed: true };
    } catch {
      return null;
    }
  }

  // loadEndpoint returns the single configured endpoint, migrating the legacy
  // multi-API storage on first use. The legacy keys are left untouched so that
  // reverting this console still works.
  function loadEndpoint() {
    let stored = null;
    try {
      const parsed = JSON.parse(localStorage.getItem(ENDPOINT_KEY) || "null");
      if (parsed && parsed.url) {
        const url = normalizeURL(parsed.url);
        stored = { name: parsed.name || new URL(url).host, url };
      }
    } catch {
      stored = null;
    }
    if (!stored) stored = migrateEndpoint();
    return deploymentEndpoint() || stored;
  }

  function migrateEndpoint() {
    try {
      const list = JSON.parse(localStorage.getItem(LEGACY_ENDPOINTS_KEY) || "[]");
      if (!Array.isArray(list) || !list.length) return null;
      const activeId = localStorage.getItem(LEGACY_ACTIVE_KEY);
      const chosen = list.find((item) => item && item.id === activeId) || list[0];
      if (!chosen || !chosen.url) return null;
      const url = normalizeURL(chosen.url);
      const migrated = { name: chosen.name || new URL(url).host, url };
      localStorage.setItem(ENDPOINT_KEY, JSON.stringify(migrated));
      return migrated;
    } catch {
      return null;
    }
  }

  function saveEndpoint(endpoint) {
    state.endpoint = endpoint;
    if (!endpoint.deployed) {
      localStorage.setItem(ENDPOINT_KEY, JSON.stringify({ name: endpoint.name, url: endpoint.url }));
    }
  }

  function credentialsKey(endpoint = state.endpoint) {
    return CREDENTIALS_PREFIX + (endpoint?.url || "");
  }

  function credentials() {
    try {
      return JSON.parse(sessionStorage.getItem(credentialsKey()) || "null") || {};
    } catch {
      return {};
    }
  }

  function username() {
    return credentials().username || "";
  }

  function password() {
    return credentials().password || "";
  }

  function forgetCredentials(endpoint = state.endpoint) {
    sessionStorage.removeItem(credentialsKey(endpoint));
  }

  function basicAuthorization(user, pass) {
    const bytes = new TextEncoder().encode(user + ":" + pass);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return "Basic " + btoa(binary);
  }

  function selectedAgentID() {
    return state.selectedAgents.get(state.sessionId) || "";
  }

  function setMenuOpen(open) {
    document.body.classList.toggle("menu-open", open);
    const button = document.querySelector("#mobile-menu");
    button.setAttribute("aria-expanded", String(open));
    button.setAttribute("aria-label", open ? "Close sessions" : "Open sessions");
  }

  function draftKey(sessionID = state.sessionId, agentID = selectedAgentID()) {
    return sessionID + ":" + agentID;
  }

  // The wire protocol keys everything by agent id; a spawned agent may also
  // carry a human-readable name. Resolve display labels from the current
  // session's cached roster so the list, header, approvals and agent-message
  // headers all agree. A missing name (an older server reports none) or a name
  // that merely repeats the id is not a custom name, so the id stands in.
  function agentLabel(id) {
    if (!id) return "?";
    const roster = state.snapshots.get(state.sessionId)?.agents;
    if (Array.isArray(roster)) {
      const match = roster.find((agent) => agent && agent.id === id);
      if (match && typeof match.name === "string" && match.name && match.name !== id) {
        return match.name;
      }
    }
    return id;
  }

  function clearSessionState() {
    state.sessions = [];
    state.defaultSessionId = "";
    state.snapshots = new Map();
    state.selectedAgents = new Map();
    state.sessionId = "";
    localStorage.removeItem(SESSION_KEY);
  }

  async function apiCall(path, options = {}) {
    const endpoint = state.endpoint;
    if (!endpoint) throw new Error("No API is configured");
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");
    if (username() && password()) {
      headers.set("Authorization", basicAuthorization(username(), password()));
    }
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

  function activeSession() {
    return state.sessions.find((item) => item.id === state.sessionId);
  }

  function renderSessionList() {
    elements.sessionList.replaceChildren();
    if (!state.sessions.length) {
      const empty = document.createElement("div");
      empty.className = "session-empty";
      empty.textContent = !state.endpoint
        ? "No API configured."
        : state.sessionSupport === false
          ? "This server has no session support."
          : "No sessions reported.";
      elements.sessionList.append(empty);
      return;
    }
    for (const session of state.sessions) {
      const item = document.createElement("div");
      item.className = "session-item" + (session.id === state.sessionId ? " active" : "");
      item.tabIndex = 0;
      item.setAttribute("role", "button");
      item.setAttribute("aria-current", session.id === state.sessionId ? "true" : "false");
      const choose = () => selectSession(session.id);
      item.addEventListener("click", choose);
      item.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          choose();
        }
      });

      const dot = document.createElement("span");
      dot.className = "session-dot " + (session.state || "");
      const copy = document.createElement("span");
      copy.className = "session-copy";
      const name = document.createElement("strong");
      name.textContent = session.name || session.id;
      if (session.default) {
        const marker = document.createElement("em");
        marker.textContent = "default";
        name.append(marker);
      }
      const detail = document.createElement("span");
      const count = Array.isArray(session.agents) ? session.agents.length - 1 : 0;
      detail.textContent = count > 0
        ? count + (count === 1 ? " subagent" : " subagents")
        : (session.model || session.id);
      copy.append(name, detail);

      const actions = document.createElement("span");
      actions.className = "session-actions";
      const rename = document.createElement("button");
      rename.type = "button";
      rename.className = "session-action";
      rename.textContent = "✎";
      rename.title = "Rename session";
      rename.setAttribute("aria-label", "Rename session " + (session.name || session.id));
      rename.addEventListener("click", (event) => {
        event.stopPropagation();
        openSessionDialog("rename", session);
      });
      actions.append(rename);
      if (!session.default) {
        const close = document.createElement("button");
        close.type = "button";
        close.className = "session-action close";
        close.textContent = "×";
        close.title = "Close session";
        close.setAttribute("aria-label", "Close session " + (session.name || session.id));
        close.addEventListener("click", (event) => {
          event.stopPropagation();
          closeSession(session.id);
        });
        actions.append(close);
      }
      item.append(dot, copy, actions);
      elements.sessionList.append(item);
    }
  }

  function renderAgentList(agents, selectedID) {
    elements.agentList.replaceChildren();
    if (agents.length <= 1) {
      const hint = document.createElement("div");
      hint.className = "agent-empty";
      hint.textContent = "No subagents yet.";
      elements.agentList.append(hint);
    }
    for (const agent of agents) {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "agent-item depth-" + Math.min(agent.depth || 0, 3) +
        (agent.id === selectedID ? " active" : "");
      item.setAttribute("aria-current", agent.id === selectedID ? "true" : "false");
      item.addEventListener("click", () => selectAgent(agent.id));
      const status = agent.paused ? "paused" : (agent.state || "unknown");
      const dot = document.createElement("span");
      dot.className = "agent-dot " + status;
      const copy = document.createElement("span");
      copy.className = "agent-copy";
      const name = document.createElement("strong");
      const named = typeof agent.name === "string" && agent.name && agent.name !== agent.id;
      name.textContent = named ? agent.name : (agent.depth ? agent.type : "Main · " + agent.type);
      const id = document.createElement("span");
      id.textContent = named ? agent.type + " · " + agent.id : agent.id;
      copy.append(name, id);
      const state = document.createElement("span");
      state.className = "agent-state " + status;
      state.textContent = status;
      item.append(dot, copy, state);
      elements.agentList.append(item);
    }
  }

  function selectAgent(id) {
    if (!state.endpoint || selectedAgentID() === id) {
      setMenuOpen(false);
      return;
    }
    state.drafts.set(draftKey(), elements.message.value);
    state.selectedAgents.set(state.sessionId, id);
    elements.message.value = state.drafts.get(draftKey()) || "";
    state.fingerprint = "";
    state.request?.abort();
    const snapshot = state.snapshots.get(state.sessionId);
    renderAgentList(snapshot?.agents || [], id);
    // Mirror renderSnapshot's header using the cached roster: a custom name
    // replaces the bare id until the next poll confirms the selection.
    const label = agentLabel(id);
    const entry = (snapshot?.agents || []).find((agent) => agent && agent.id === id);
    elements.agentTitle.textContent = label === id ? id : label + (entry?.type ? " · " + entry.type : "");
    elements.message.disabled = true;
    elements.send.disabled = true;
    elements.transcript.replaceChildren(emptyState("Loading agent…", id));
    renderPendingAgentMessages();
    poll();
    setMenuOpen(false);
  }

  function selectSession(id) {
    if (!state.endpoint || state.sessionId === id) {
      setMenuOpen(false);
      return;
    }
    state.drafts.set(draftKey(), elements.message.value);
    state.sessionId = id;
    localStorage.setItem(SESSION_KEY, id);
    elements.message.value = state.drafts.get(draftKey()) || "";
    state.fingerprint = "";
    state.connected = false;
    state.request?.abort();
    clearTimeout(state.timer);
    renderSessionList();
    renderWaiting();
    poll();
    setMenuOpen(false);
  }

  async function closeSession(id) {
    const session = state.sessions.find((item) => item.id === id);
    if (!session || session.default || state.sessionSupport === false) return;
    if (!window.confirm("Close session \"" + (session.name || id) + "\"?")) return;
    try {
      await apiCall("/sessions/" + encodeURIComponent(id), { method: "DELETE" });
    } catch (error) {
      showConnection(error.message);
      return;
    }
    if (state.sessionId === id) {
      state.sessionId = "";
      localStorage.removeItem(SESSION_KEY);
    }
    state.fingerprint = "";
    await pollSessions();
    poll(true);
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
    const endpoint = state.endpoint;
    const session = activeSession();
    elements.endpointLabel.textContent = endpoint
      ? endpoint.name + (session?.name ? " · " + session.name : "")
      : "Not configured";
    elements.agentTitle.textContent = endpoint ? "Connecting…" : "Current session";
    elements.sessionMeta.replaceChildren();
    elements.agentList.replaceChildren();
    elements.approvals.replaceChildren();
    renderPendingAgentMessages();
    elements.message.disabled = true;
    elements.send.disabled = true;
    elements.queue.textContent = "";
    state.connected = false;
    elements.transcript.replaceChildren(emptyState(
      endpoint ? "Connecting to Aiharn…" : "Configure an Aiharn API",
      endpoint ? endpoint.url : "Open settings to point this console at an Aiharn instance."
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
    if (!state.endpoint) {
      const button = document.createElement("button");
      button.className = "primary-button";
      button.textContent = "Open settings";
      button.addEventListener("click", openSettingsDialog);
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

  // renderSessionMeta paints the session meta row (model, channel, approval
  // mode, state). The approval mode is interactive only when the snapshot
  // advertises the "approval_mode" capability: a server that predates the
  // /session/approval route would otherwise receive a doomed POST and surface
  // an error, so an old server keeps the plain, non-clickable chip. The
  // execution channel is interactive only when the server advertises the
  // "channel_switch" capability and the session is live.
  function renderSessionMeta(session, capabilities, channels) {
    const items = [];
    if (session.model) items.push(chip(session.model));
    const channel = channelChip(session, capabilities, channels);
    if (channel) items.push(channel);
    items.push(approvalChip(session, capabilities));
    items.push(chip(session.state || "unknown", "state-" + (session.state || "unknown")));
    elements.sessionMeta.replaceChildren(...items);
  }

  function approvalChip(session, capabilities) {
    const mode = session.approval_mode || "ask";
    const unavailable = session.state === "closed" || session.state === "errored";
    const capable = Array.isArray(capabilities) &&
      capabilities.indexOf("approval_mode") !== -1;
    if (!capable || unavailable) return chip(mode);
    const next = mode === "ask" ? "allow-all" : "ask";
    const button = document.createElement("button");
    button.type = "button";
    button.className = "meta-chip approval-toggle";
    button.textContent = mode;
    button.title = "Switch approval mode to " + next;
    button.setAttribute("aria-label",
      "Approval mode: " + mode + ". Switch to " + next + ".");
    button.addEventListener("click", () => toggleApprovalMode(button, capabilities));
    return button;
  }

  async function toggleApprovalMode(button, capabilities) {
    if (!state.endpoint || button.disabled) return;
    const snapshot = state.snapshots.get(state.sessionId);
    const session = snapshot?.session || {};
    const mode = session.approval_mode || "ask";
    const next = mode === "ask" ? "allow-all" : "ask";
    const payload = { mode: next };
    // Only a server that has answered /sessions understands session_id; an
    // older one would reject the field, so it is omitted unless supported.
    if (state.sessionSupport === true && state.sessionId) {
      payload.session_id = state.sessionId;
    }
    button.disabled = true;
    try {
      const result = await apiCall("/session/approval", {
        method: "POST", body: JSON.stringify(payload)
      });
      const applied = result && typeof result.mode === "string" ? result.mode : next;
      setApprovalMode(applied);
      // Re-render immediately so the chip shows the new mode; the once-per-
      // second poll will confirm it against the server.
      renderSessionMeta(session, capabilities, snapshot?.channels);
    } catch (error) {
      // Surface through the existing notice and restore the chip so the poll
      // loop keeps running instead of leaving a stuck, disabled button.
      showConnection(error.message || "Could not change the approval mode.");
      button.disabled = false;
    }
  }

  // setApprovalMode records the server-confirmed mode on the active session and
  // its cached snapshot so the next render reflects it without a fresh poll.
  function setApprovalMode(mode) {
    const entry = activeSession();
    if (entry) entry.approval_mode = mode;
    const snapshot = state.snapshots.get(state.sessionId);
    if (snapshot && snapshot.session) snapshot.session.approval_mode = mode;
  }

  // channelChip renders the execution-channel chip. When the server advertises
  // channel switching and the session is live, the chip is a button that opens a
  // dropdown; otherwise it is the existing plain chip.
  function channelChip(session, capabilities, channels) {
    if (!session.channel) return null;
    const capable = Array.isArray(capabilities) &&
      capabilities.indexOf("channel_switch") !== -1;
    const unavailable = session.state === "closed" || session.state === "errored";
    if (!capable || unavailable) return chip(session.channel);

    const wrapper = document.createElement("span");
    wrapper.className = "channel-menu-anchor";
    const button = document.createElement("button");
    button.type = "button";
    button.className = "meta-chip channel-toggle";
    button.textContent = session.channel;
    button.title = "Switch execution channel";
    button.setAttribute("aria-haspopup", "listbox");
    button.setAttribute("aria-expanded", "false");
    button.addEventListener("click", () => toggleChannelMenu(button, channels));
    wrapper.append(button);
    return wrapper;
  }

  function closeChannelMenu() {
    if (openChannelMenu) {
      openChannelMenu.remove();
      openChannelMenu = null;
    }
    const open = document.querySelector(".channel-toggle[aria-expanded='true']");
    if (open) open.setAttribute("aria-expanded", "false");
  }

  function toggleChannelMenu(button, channels) {
    if (openChannelMenu && button.getAttribute("aria-expanded") === "true") {
      closeChannelMenu();
      return;
    }
    closeChannelMenu();
    button.setAttribute("aria-expanded", "true");

    const wrapper = document.createElement("span");
    wrapper.className = "channel-menu-anchor channel-menu-open";
    const menu = document.createElement("div");
    menu.className = "channel-menu";
    menu.setAttribute("role", "listbox");
    menu.setAttribute("aria-label", "Execution channel");

    const current = button.textContent;
    for (const channel of channels || []) {
      const option = document.createElement("button");
      option.type = "button";
      option.className = "channel-option" + (channel.name === current ? " active" : "");
      option.setAttribute("role", "option");
      option.setAttribute("aria-selected", channel.name === current ? "true" : "false");

      const label = document.createElement("span");
      label.textContent = channel.name;
      const type = document.createElement("span");
      type.className = "channel-option-type";
      type.textContent = channel.type;
      option.append(label, type);

      option.addEventListener("click", () => {
        closeChannelMenu();
        selectChannel(channel.name);
      });
      menu.append(option);
    }

    wrapper.append(menu);
    button.parentElement.appendChild(wrapper);
    openChannelMenu = wrapper;
  }

  async function selectChannel(name) {
    if (!state.endpoint || !name) return;
    const payload = { channel: name };
    if (state.sessionSupport === true && state.sessionId) {
      payload.session_id = state.sessionId;
    }
    const snapshot = state.snapshots.get(state.sessionId);
    const session = snapshot?.session || {};
    try {
      const result = await apiCall("/session/channel", {
        method: "POST", body: JSON.stringify(payload)
      });
      const applied = result && typeof result.channel === "string" ? result.channel : name;
      setChannel(applied);
      renderSessionMeta(session, snapshot?.capabilities, snapshot?.channels);
    } catch (error) {
      showConnection(error.message || "Could not change the execution channel.");
    }
    await poll(true);
  }

  // setChannel records the server-confirmed channel on the active session and its
  // cached snapshot so the next render reflects it without a fresh poll.
  function setChannel(name) {
    const entry = activeSession();
    if (entry) entry.channel = name;
    const snapshot = state.snapshots.get(state.sessionId);
    if (snapshot && snapshot.session) snapshot.session.channel = name;
  }

  function renderSnapshot(snapshot) {
    const endpoint = state.endpoint;
    const session = snapshot.session || {};
    if (!state.selectedAgents.has(state.sessionId)) {
      state.selectedAgents.set(state.sessionId, session.agent_id);
    }
    const current = activeSession();
    elements.endpointLabel.textContent = endpoint
      ? endpoint.name + (current?.name ? " · " + current.name : "")
      : "Aiharn";
    // A spawned agent may carry a human-readable name; the header and the
    // composer placeholder prefer it, matching the agent list.
    const selectedLabel = session.agent_id ? agentLabel(session.agent_id) : "";
    const selectedName = selectedLabel && selectedLabel !== session.agent_id ? selectedLabel : "";
    elements.agentTitle.textContent = selectedName
      ? selectedName + (session.agent_type ? " · " + session.agent_type : "")
      : session.agent_id
        ? (session.agent_type && session.agent_type !== session.agent_id
          ? session.agent_type + " · " + session.agent_id : session.agent_id)
        : "Current session";
    elements.message.placeholder = "Message " +
      (selectedName || session.agent_id || "Aiharn") + "…";

    const agents = Array.isArray(snapshot.agents) ? snapshot.agents : [];
    const stale = !Array.isArray(snapshot.agents);
    renderAgentList(stale ? [{
      id: session.agent_id, type: session.agent_type,
      state: session.state, depth: 0, paused: false
    }] : agents, session.agent_id);

    renderSessionMeta(session, snapshot.capabilities, snapshot.channels);
    elements.queue.textContent = snapshot.queued_messages
      ? snapshot.queued_messages + " queued"
      : "";
    const unavailable = session.state === "closed" || session.state === "errored";
    elements.message.disabled = unavailable;
    elements.send.disabled = unavailable || !elements.message.value.trim();
    renderApprovals(snapshot.pending_approvals || [], snapshot.pending_tool_limits || []);
    renderMessages(snapshot.messages || [], snapshot.live_reasoning);
    renderPendingAgentMessages(snapshot.pending_agent_messages);
    if (state.sessionSupport === false) showConnection(LEGACY_SERVER_NOTICE);
    else if (stale) showConnection(STALE_SERVER_NOTICE);
    else if (snapshot.agents_error) showConnection(snapshot.agents_error);
    else if (snapshot.last_error) showConnection(snapshot.last_error);
    else hideConnection();
    state.connected = true;
  }

  // Agent messages still queued in an inbox are not part of the transcript
  // yet, so the poll (which is the only thing that refreshes the transcript)
  // cannot show them there. Render them in a bounded strip directly above the
  // composer, visually distinct from the transcript, so a queued agent message
  // is visible before the next turn. The strip is hidden entirely when the
  // server is older (no pending_agent_messages field) or the array is empty.
  function renderPendingAgentMessages(pending) {
    let strip = elements.pendingAgentMessages;
    if (!strip) {
      strip = document.createElement("section");
      strip.id = "pending-agent-messages";
      strip.className = "pending-agent-messages hidden";
      strip.setAttribute("aria-label", "Agent messages");
      if (elements.composer && elements.composer.parentNode) {
        elements.composer.parentNode.insertBefore(strip, elements.composer);
      }
      elements.pendingAgentMessages = strip;
    }
    strip.replaceChildren();
    const list = Array.isArray(pending) ? pending : [];
    if (!list.length) {
      strip.classList.add("hidden");
      return;
    }
    strip.classList.remove("hidden");
    const head = document.createElement("div");
    head.className = "pending-head";
    const title = document.createElement("span");
    title.className = "pending-title";
    title.textContent = "Agent messages";
    const hint = document.createElement("span");
    hint.className = "pending-hint";
    hint.textContent = "Queued between agents. They appear in the transcript after your next message.";
    head.append(title, hint);
    const items = document.createElement("ul");
    items.className = "pending-list";
    for (const entry of list) {
      const item = document.createElement("li");
      item.className = "pending-item";
      const header = document.createElement("div");
      header.className = "pending-item-header";
      const arrow = entry && entry.direction === "down" ? "\u2193" : "\u2191";
      header.textContent = arrow + " " + agentLabel(entry && entry.from) + " \u2192 " +
        agentLabel(entry && entry.to) + " \u00b7 " + ((entry && entry.kind) || "message");
      const text = document.createElement("div");
      text.className = "pending-item-body";
      text.textContent = (entry && entry.content) || "";
      item.append(header, text);
      items.append(item);
    }
    strip.append(head, items);
  }

  const markdownTags = new Set([
    "A", "BLOCKQUOTE", "BR", "CODE", "DEL", "EM", "H1", "H2", "H3",
    "H4", "H5", "H6", "HR", "IMG", "INPUT", "LI", "OL", "P", "PRE",
    "STRONG", "TABLE", "TBODY", "TD", "TH", "THEAD", "TR", "UL"
  ]);

  function safeMarkdownURL(raw, image) {
    if (!raw) return null;
    if (image && /^data:image\/(?:gif|jpe?g|png|webp);base64,/i.test(raw)) return raw;
    let parsed;
    try {
      parsed = new URL(raw, window.location.href);
    } catch (_) {
      return null;
    }
    if (image) return parsed.origin === window.location.origin ? raw : null;
    return ["http:", "https:", "mailto:"].includes(parsed.protocol) ? raw : null;
  }

  // API endpoints are user-configurable, so even server-rendered Markdown is
  // treated as untrusted. markdown-go already escapes raw HTML; this second
  // pass limits the DOM shape and URL schemes before insertion.
  function markdownFragment(html) {
    const template = document.createElement("template");
    template.innerHTML = html;
    for (const node of Array.from(template.content.querySelectorAll("*")).reverse()) {
      if (!markdownTags.has(node.tagName)) {
        node.replaceWith(document.createTextNode(node.textContent || ""));
        continue;
      }
      const attributes = Array.from(node.attributes);
      for (const attribute of attributes) node.removeAttribute(attribute.name);
      if (node.tagName === "A") {
        const href = attributes.find((attribute) => attribute.name.toLowerCase() === "href")?.value;
        const title = attributes.find((attribute) => attribute.name.toLowerCase() === "title")?.value;
        const safe = safeMarkdownURL(href, false);
        if (safe) {
          node.setAttribute("href", safe);
          node.setAttribute("target", "_blank");
          node.setAttribute("rel", "noopener noreferrer");
        }
        if (title) node.setAttribute("title", title);
      } else if (node.tagName === "IMG") {
        const src = attributes.find((attribute) => attribute.name.toLowerCase() === "src")?.value;
        const alt = attributes.find((attribute) => attribute.name.toLowerCase() === "alt")?.value;
        const title = attributes.find((attribute) => attribute.name.toLowerCase() === "title")?.value;
        const safe = safeMarkdownURL(src, true);
        if (safe) node.setAttribute("src", safe);
        if (alt) node.setAttribute("alt", alt);
        if (title) node.setAttribute("title", title);
      } else if (node.tagName === "CODE") {
        const name = attributes.find((attribute) => attribute.name.toLowerCase() === "class")?.value;
        if (/^language-[a-z0-9_+.-]+$/i.test(name || "")) node.className = name;
      } else if (node.tagName === "OL") {
        const start = attributes.find((attribute) => attribute.name.toLowerCase() === "start")?.value;
        if (/^-?\d+$/.test(start || "")) node.setAttribute("start", start);
      } else if (node.tagName === "TD" || node.tagName === "TH") {
        const align = attributes.find((attribute) => attribute.name.toLowerCase() === "align")?.value;
        if (["left", "center", "right"].includes(align)) node.setAttribute("align", align);
      } else if (node.tagName === "INPUT") {
        node.setAttribute("type", "checkbox");
        node.setAttribute("disabled", "");
        if (attributes.some((attribute) => attribute.name.toLowerCase() === "checked")) {
          node.setAttribute("checked", "");
        }
      }
    }
    return template.content;
  }

  function appendMessageContent(body, message) {
    if (typeof message.html === "string" && message.html) {
      body.classList.add("markdown");
      body.append(markdownFragment(message.html));
      return;
    }
    body.append(document.createTextNode(message.content || ""));
  }

  function renderMessages(messages, liveReasoning) {
    const previousTop = elements.transcript.scrollTop;
    const nearBottom = elements.transcript.scrollHeight - previousTop -
      elements.transcript.clientHeight < 100;
    const expandedTools = new Set(Array.from(
      elements.transcript.querySelectorAll(".tool-block[open]"),
      (block) => block.dataset.toolKey
    ));
    const existingThinking = Array.from(elements.transcript.querySelectorAll(".thinking-block"));
    const expandedThinking = new Set(existingThinking.filter((block) => block.open).map((block) => block.dataset.thinkingKey));
    const hadLiveThinking = existingThinking.some((block) => block.dataset.thinkingKey === "live");
    elements.transcript.replaceChildren();
    if (!messages.length && !liveReasoning) {
      elements.transcript.append(emptyState("Session is ready", "Send a message to begin."));
      return;
    }

    let thinkingIndex = 0;
    for (const entry of groupSequentialToolCalls(groupMessages(messages))) {
      if (entry.group) {
        elements.transcript.append(renderToolGroup(entry, expandedTools));
        continue;
      }
      const message = entry.message;
      if (message.type === "reasoning") {
        const key = "thinking-" + thinkingIndex++;
        elements.transcript.append(renderThinking(message.content || "", false, key, expandedThinking.has(key)));
      } else if (message.type === "message") {
        // A completed subagent report arrives in the top-level history as a
        // role:"user" item injected by the agent system. Without an origin it is
        // indistinguishable from something the human typed, so it used to wear
        // the user's own green bubble. Render those as a distinct delivery row;
        // an older server that omits origin keeps the original behaviour.
        // A newer server also stamps agent-to-agent traffic with a `delivery`
        // object naming the direction, endpoints and kind; that gets its own
        // header. A human message keeps origin:"human" and never carries one.
        const delivery = message.role === "user" &&
          message.delivery && typeof message.delivery === "object"
          ? message.delivery : null;
        const agentAuthored = message.role === "user" &&
          (message.origin === "agent" || Boolean(delivery));
        const row = document.createElement("div");
        const body = document.createElement("div");
        if (agentAuthored) {
          row.className = "message-row delivery";
          body.className = "message message-delivery";
          if (delivery) {
            const head = document.createElement("div");
            head.className = "delivery-header";
            const arrow = delivery.direction === "down" ? "\u2193" : "\u2191";
            head.textContent = arrow + " " + agentLabel(delivery.from) + " \u2192 " +
              agentLabel(delivery.to) + " \u00b7 " + (delivery.kind || "message");
            body.append(head);
          } else {
            const label = document.createElement("div");
            label.className = "message-role";
            label.textContent = "Agent message";
            body.append(label);
          }
        } else {
          row.className = "message-row " + (message.role || "");
          body.className = "message";
          if (message.role !== "user") {
            const role = document.createElement("div");
            role.className = "message-role";
            role.textContent = message.role === "assistant" ? "Aiharn" : message.role;
            body.append(role);
          }
        }
        appendMessageContent(body, message);
        row.append(body);
        elements.transcript.append(row);
      } else {
        elements.transcript.append(renderToolBlock(entry, expandedTools));
      }
    }
    if (liveReasoning) {
      elements.transcript.append(renderThinking(liveReasoning.text || "", Boolean(liveReasoning.active),
        "live", expandedThinking.has("live") || !hadLiveThinking));
    }
    elements.transcript.scrollTop = nearBottom
      ? elements.transcript.scrollHeight
      : previousTop;
  }

  function renderThinking(content, active, key, expanded) {
    const details = document.createElement("details");
    details.className = "thinking-block" + (active ? " active" : "");
    details.dataset.thinkingKey = key;
    details.open = expanded;
    const summary = document.createElement("summary");
    const icon = document.createElement("span");
    icon.className = "thinking-icon";
    icon.textContent = active ? "◌" : "✓";
    summary.append(icon, document.createTextNode(active ? "Thinking…" : "Thinking"));
    const body = document.createElement("pre");
    body.textContent = content || (active ? "Waiting for the model…" : "");
    details.append(summary, body);
    return details;
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

  function toolStatusFor(call, result) {
    const resultText = result?.content || "";
    if (!result) return "pending";
    if (resultText.trim().toLowerCase() === "denied by user") return "denied";
    if (resultText.startsWith("error:")) return "error";
    return "executed";
  }
  function toolStatusIcon(status) {
    return { pending: "…", denied: "🛑", error: "!", executed: "✓" }[status];
  }
  function toolStatusRank(status) {
    return { executed: 0, pending: 1, denied: 2, error: 3 }[status] ?? 0;
  }
  function aggregateToolStatus(calls) {
    let worst = "executed";
    for (const call of calls) {
      const status = toolStatusFor(call.message, call.result);
      if (toolStatusRank(status) > toolStatusRank(worst)) worst = status;
    }
    return worst;
  }

  function groupSequentialToolCalls(entries) {
    const grouped = [];
    let run = [];
    const flush = () => {
      if (!run.length) return;
      if (run.length === 1) {
        grouped.push(run[0]);
      } else {
        grouped.push({
          group: true,
          calls: run,
          key: "group:" + run.map((c) => c.key).join(","),
        });
      }
      run = [];
    };
    for (const entry of entries) {
      if (entry.message?.type === "tool_call") {
        run.push(entry);
      } else {
        flush();
        grouped.push(entry);
      }
    }
    flush();
    return grouped;
  }

  function renderToolGroup(group, expandedTools) {
    const calls = group.calls;
    const status = aggregateToolStatus(calls);
    const details = document.createElement("details");
    details.className = "tool-block tool-group " + status;
    details.dataset.toolKey = group.key;
    details.open = expandedTools.has(group.key);

    const summary = document.createElement("summary");
    summary.setAttribute("aria-label", status + " · " + calls.length + " tool calls");
    const icon = document.createElement("span");
    icon.className = "tool-status-icon " + status;
    icon.textContent = toolStatusIcon(status);
    icon.title = status;
    icon.setAttribute("aria-hidden", "true");
    const title = document.createElement("span");
    title.textContent = "tool calls (" + calls.length + ")";
    summary.append(icon, title);
    details.append(summary);

    for (const call of calls) {
      const callStatus = toolStatusFor(call.message, call.result);
      const item = document.createElement("div");
      item.className = "tool-group-call";
      const head = document.createElement("div");
      head.className = "tool-group-call-head";
      const callIcon = document.createElement("span");
      callIcon.className = "tool-status-icon " + callStatus;
      callIcon.textContent = toolStatusIcon(callStatus);
      callIcon.title = callStatus;
      callIcon.setAttribute("aria-hidden", "true");
      head.append(callIcon, document.createTextNode("tool · " + (call.message.name || "unknown")));
      item.append(head);
      appendToolSection(item, "Call", call.message.arguments);
      if (call.result) appendToolSection(item, "Result", call.result.content);
      details.append(item);
    }
    return details;
  }

  function renderToolBlock(entry, expandedTools) {
    const call = entry.message.type === "tool_call" ? entry.message : null;
    const result = entry.result || (call ? null : entry.message);
    const status = toolStatusFor(call, result);
    const details = document.createElement("details");
    details.className = "tool-block " + status;
    details.dataset.toolKey = entry.key;
    details.open = expandedTools.has(entry.key);

    const summary = document.createElement("summary");
    summary.setAttribute("aria-label", status + " · " + (call?.name || "tool result"));
    const icon = document.createElement("span");
    icon.className = "tool-status-icon " + status;
    icon.textContent = toolStatusIcon(status);
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

  function renderApprovals(approvals, toolLimits = []) {
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
      const agentName = approval.agent_id && agentLabel(approval.agent_id) !== approval.agent_id
        ? agentLabel(approval.agent_id) : "";
      agentType.textContent = "Agent: " + (approval.agent_type || "unknown") +
        (agentName ? " · " + agentName : "");
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
    for (const limit of toolLimits) {
      const card = document.createElement("article");
      card.className = "approval-card";
      const content = document.createElement("div");
      const heading = document.createElement("div");
      heading.className = "approval-heading";
      heading.textContent = agentLabel(limit.agent_id) + " used " + limit.count +
        " tool calls this turn. Stop, continue without a cap, or double to " + (limit.limit * 2) + "?";
      content.append(heading);
      const actions = document.createElement("div");
      actions.className = "approval-actions";
      for (const [label, decision] of [["Stop", "stop"], ["Continue", "continue"], ["Double", "double"]]) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = decision === "stop" ? "deny" : "approve";
        button.textContent = label;
        button.addEventListener("click", async () => {
          for (const sibling of actions.children) sibling.disabled = true;
          try {
            const payload = { decision, agent_id: limit.agent_id };
            if (state.sessionSupport === true && state.sessionId) payload.session_id = state.sessionId;
            await apiCall("/tool-limits/" + encodeURIComponent(limit.id), {
              method: "POST", body: JSON.stringify(payload)
            });
            poll(true);
          } catch (error) {
            showConnection(error.message || "Could not resolve tool-call limit.");
            for (const sibling of actions.children) sibling.disabled = false;
          }
        });
        actions.append(button);
      }
      card.append(content, actions);
      elements.approvals.append(card);
    }
  }

  async function resolveApproval(id, decision, button) {
    if (!state.endpoint) return;
    for (const sibling of button.parentElement.children) sibling.disabled = true;
    try {
      const payload = { decision };
      if (state.sessionSupport === true && state.sessionId) payload.session_id = state.sessionId;
      await apiCall("/approvals/" + encodeURIComponent(id), {
        method: "POST", body: JSON.stringify(payload)
      });
      await poll(true);
    } catch (error) {
      showConnection(error.message);
      for (const sibling of button.parentElement.children) sibling.disabled = false;
    }
  }

  async function pollSessions() {
    if (!state.endpoint) {
      renderSessionList();
      return;
    }
    state.sessionsRequest?.abort();
    const request = new AbortController();
    state.sessionsRequest = request;
    try {
      const payload = await apiCall("/sessions", { signal: request.signal });
      state.sessionSupport = true;
      const sessions = Array.isArray(payload.sessions) ? payload.sessions : [];
      state.sessions = sessions;
      state.defaultSessionId = payload.default_session_id || "";
      const known = sessions.some((item) => item.id === state.sessionId);
      const previous = state.sessionId;
      if (!state.sessionId || !known) {
        const fallback = state.defaultSessionId || sessions[0]?.id || "";
        state.sessionId = fallback;
        if (fallback) localStorage.setItem(SESSION_KEY, fallback);
      }
      renderSessionList();
      if (state.sessionId !== previous) {
        // A different session is now active: drop the cached render and fetch
        // it without waiting for the next tick.
        state.fingerprint = "";
        state.selectedAgents.delete(state.sessionId);
        renderWaiting();
        poll(true);
      }
    } catch (error) {
      if (error.name === "AbortError") return;
      if (error.status === 404 || error.status === 405) {
        // An older server: it has no session routes and would reject the
        // session_id field on messages. Degrade to a single implicit session
        // instead of failing. The poll keeps running, so restarting the server
        // is picked up without a reload.
        state.sessionSupport = false;
        state.sessionId = "";
        state.sessions = [];
        renderSessionList();
        showConnection(LEGACY_SERVER_NOTICE);
        return;
      }
      state.sessions = [];
      renderSessionList();
      showConnection(error.status === 401
        ? "Invalid username or password. Open settings to log in."
        : error.message);
    } finally {
      clearTimeout(state.sessionsTimer);
      state.sessionsTimer = setTimeout(pollSessions, SESSIONS_POLL_MS);
    }
  }

  async function poll(immediate = false) {
    clearTimeout(state.timer);
    if (!state.endpoint) {
      renderWaiting();
      return;
    }
    state.request?.abort();
    const request = new AbortController();
    state.request = request;
    const sessionID = state.sessionId;
    try {
      const agentID = selectedAgentID();
      const query = [];
      if (sessionID) query.push("session_id=" + encodeURIComponent(sessionID));
      if (agentID) query.push("agent_id=" + encodeURIComponent(agentID));
      const path = "/session" + (query.length ? "?" + query.join("&") : "");
      const snapshot = await apiCall(path, { signal: request.signal });
      if (sessionID !== state.sessionId) return;
      if (agentID !== selectedAgentID()) return;
      const fingerprint = JSON.stringify(snapshot);
      if (fingerprint !== state.fingerprint) {
        state.fingerprint = fingerprint;
        state.snapshots.set(state.sessionId, snapshot);
        renderSnapshot(snapshot);
      }
    } catch (error) {
      if (error.name === "AbortError") return;
      if (sessionID !== state.sessionId) return;
      if (error.status === 404) {
        if (selectedAgentID()) {
          state.selectedAgents.delete(state.sessionId);
          state.fingerprint = "";
          state.connected = false;
          elements.message.disabled = true;
          elements.send.disabled = true;
          showConnection("Selected agent is no longer available; returning to main.");
        } else {
          state.sessionId = state.defaultSessionId || "";
          if (state.sessionId) localStorage.setItem(SESSION_KEY, state.sessionId);
          state.fingerprint = "";
          state.connected = false;
          renderSessionList();
          renderWaiting();
          showConnection("That session is gone; switched to the default session.");
        }
        return;
      }
      state.fingerprint = "";
      state.connected = false;
      elements.message.disabled = true;
      elements.send.disabled = true;
      showConnection(error.status === 401
        ? "Invalid username or password. Open settings to log in."
        : error.message);
    } finally {
      // Reschedule whenever this is still the newest poll. Comparing on the
      // session id instead would stop the loop for good whenever the active
      // session changed while the request was in flight (which is exactly what
      // happens on startup, when the session list resolves after the first
      // poll), leaving the console stuck on "Connecting".
      if (state.request === request) {
        state.timer = setTimeout(poll, immediate ? 50 : POLL_MS);
      }
    }
  }

  async function sendMessage(event) {
    event?.preventDefault();
    const agentID = selectedAgentID();
    const snapshot = state.snapshots.get(state.sessionId);
    const rootID = snapshot?.agents?.[0]?.id || snapshot?.session?.agent_id || "";
    const content = elements.message.value;
    if (!state.endpoint || !state.connected || elements.message.disabled || !content.trim()) return;
    elements.send.disabled = true;
    try {
      await apiCall("/messages", {
        method: "POST",
        body: JSON.stringify(messagePayload(content, agentID, rootID, state.sessionId))
      });
      state.drafts.delete(draftKey());
      if (agentID === selectedAgentID() && elements.message.value === content) {
        elements.message.value = "";
        updateComposer();
      }
      await poll(true);
    } catch (error) {
      showConnection(error.message);
    } finally {
      elements.send.disabled = !state.connected || elements.message.disabled ||
        !elements.message.value.trim();
    }
  }

  function messagePayload(content, agentID, rootID, sessionID) {
    const payload = { content };
    if (agentID && agentID !== rootID) payload.agent_id = agentID;
    // Only a server that has answered /sessions understands this field; an
    // older one rejects the whole body as invalid JSON.
    if (sessionID && state.sessionSupport === true) payload.session_id = sessionID;
    return payload;
  }

  function showDialogError(element, message) {
    element.textContent = message;
    element.classList.toggle("hidden", !message);
  }

  function openSettingsDialog() {
    const endpoint = state.endpoint;
    elements.settingsName.value = endpoint?.name || "";
    elements.settingsUrl.value = endpoint?.url || "";
    elements.settingsUsername.value = username();
    elements.settingsPassword.value = "";
    elements.settingsUrl.readOnly = Boolean(endpoint?.deployed);
    elements.settingsUrlHelp.textContent = endpoint?.deployed
      ? "Configured by deployment; login below is still editable."
      : "";
    if (!endpoint?.deployed) {
      elements.settingsUrlHelp.textContent = "Use the server origin, without /api/v1.";
    }
    elements.settingsPasswordHelp.textContent = password()
      ? "Leave blank to keep the password for this browser tab."
      : "Enter your password to log in. Credentials stay in this browser tab.";
    showDialogError(elements.settingsError, "");
    elements.settingsDialog.showModal();
    setTimeout(() => elements.settingsName.focus(), 0);
  }

  function openSessionDialog(mode, session) {
    if (state.sessionSupport === false) {
      state.feedback = LEGACY_SERVER_NOTICE;
      showConnection(LEGACY_SERVER_NOTICE);
      return;
    }
    state.sessionMode = mode;
    state.renameTarget = session?.id || "";
    elements.sessionTitle.textContent = mode === "rename" ? "Rename session" : "New session";
    elements.sessionSubmit.textContent = mode === "rename" ? "Rename" : "Create";
    elements.sessionName.value = mode === "rename"
      ? (session?.name || "")
      : "Session " + (state.sessions.length + 1);
    showDialogError(elements.sessionError, "");
    elements.sessionDialog.showModal();
    setTimeout(() => {
      elements.sessionName.focus();
      elements.sessionName.select();
    }, 0);
  }

  function updateComposer() {
    elements.send.disabled = !state.connected || elements.message.disabled ||
      !elements.message.value.trim();
  }

  function refreshAfterEndpointChange() {
    state.fingerprint = "";
    state.connected = false;
    state.sessionsRequest?.abort();
    state.request?.abort();
  }

  document.querySelectorAll("#open-settings, #topbar-settings").forEach((button) => {
    button.addEventListener("click", openSettingsDialog);
  });
  document.querySelectorAll(".close-settings").forEach((button) => {
    button.addEventListener("click", () => elements.settingsDialog.close());
  });
  document.querySelectorAll(".close-session").forEach((button) => {
    button.addEventListener("click", () => elements.sessionDialog.close());
  });
  document.querySelector("#new-session").addEventListener("click", () => openSessionDialog("create"));
  document.querySelector("#mobile-menu").addEventListener("click", () => {
    setMenuOpen(!document.body.classList.contains("menu-open"));
  });
  document.querySelector("#sidebar-scrim").addEventListener("click", () => setMenuOpen(false));

  elements.settingsForm.addEventListener("submit", (event) => {
    event.preventDefault();
    let url;
    try {
      url = normalizeURL(elements.settingsUrl.value);
    } catch {
      showDialogError(elements.settingsError, "Enter a valid HTTP(S) API URL.");
      return;
    }
    const previous = state.endpoint;
    let name = elements.settingsName.value.trim();
    if (!name) {
      try {
        name = new URL(url).host;
      } catch {
        name = url;
      }
    }
    const changed = !previous || previous.url !== url;
    const enteredUsername = elements.settingsUsername.value.trim();
    const enteredPassword = elements.settingsPassword.value;
    if (!enteredUsername || (!enteredPassword && (changed || enteredUsername !== username() || !password()))) {
      showDialogError(elements.settingsError, "Enter a username and password.");
      return;
    }
    if (changed) forgetCredentials(previous);
    saveEndpoint({
      name: previous?.deployed ? previous.name : name,
      url,
      deployed: Boolean(previous?.deployed)
    });
    sessionStorage.setItem(credentialsKey(), JSON.stringify({
      username: enteredUsername,
      password: enteredPassword || password()
    }));
    elements.settingsPassword.value = "";
    elements.settingsDialog.close();
    if (changed) {
      // Session ids belong to one server, so a new URL starts a new selection.
      clearSessionState();
      refreshAfterEndpointChange();
      renderSessionList();
      renderWaiting();
      pollSessions();
    } else {
      refreshAfterEndpointChange();
      poll(true);
    }
  });
  elements.settingsUrl.addEventListener("input", () => showDialogError(elements.settingsError, ""));
  document.querySelector("#forget-credentials").addEventListener("click", () => {
    forgetCredentials();
    elements.settingsUsername.value = "";
    elements.settingsPassword.value = "";
    elements.settingsPasswordHelp.textContent =
      "Enter your password to log in. Credentials stay in this browser tab.";
    showDialogError(elements.settingsError, "");
    refreshAfterEndpointChange();
    poll(true);
  });

  elements.sessionForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const name = elements.sessionName.value.trim();
    if (!name) {
      showDialogError(elements.sessionError, "Enter a session name.");
      return;
    }
    const renaming = state.sessionMode === "rename";
    elements.sessionSubmit.disabled = true;
    try {
      if (renaming) {
        await apiCall("/sessions/" + encodeURIComponent(state.renameTarget), {
          method: "PATCH", body: JSON.stringify({ name })
        });
      } else {
        const created = await apiCall("/sessions", {
          method: "POST", body: JSON.stringify({ name })
        });
        elements.sessionDialog.close();
        await pollSessions();
        if (created?.id) {
          selectSession(created.id);
          return;
        }
      }
      elements.sessionDialog.close();
      await pollSessions();
    } catch (error) {
      showDialogError(elements.sessionError, error.message);
    } finally {
      elements.sessionSubmit.disabled = false;
    }
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

  document.addEventListener("click", (event) => {
    // A click on the toggle button is handled by its own listener; treating it
    // as an outside click here would immediately close the menu it just opened.
    const onToggle = event.target && event.target.closest && event.target.closest(".channel-toggle");
    if (openChannelMenu && !openChannelMenu.contains(event.target) && !onToggle) {
      closeChannelMenu();
    }
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeChannelMenu();
  });

  renderSessionList();
  renderWaiting();
  if (state.endpoint) {
    pollSessions();
    poll();
  }
})();
