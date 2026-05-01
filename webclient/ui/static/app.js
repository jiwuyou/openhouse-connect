(() => {
  "use strict";

  const $ = (id) => /** @type {HTMLElement} */ (document.getElementById(id));

  const app = $("app");
  const drawerOverlay = $("drawerOverlay");
  const menuBtn = /** @type {HTMLButtonElement} */ ($("menuBtn"));
  const drawerCloseBtn = /** @type {HTMLButtonElement} */ ($("drawerCloseBtn"));

  const statusPill = $("statusPill");
  const statusText = $("statusText");

  const projectInput = /** @type {HTMLInputElement} */ ($("projectInput"));
  const sessionInput = /** @type {HTMLInputElement} */ ($("sessionInput"));
  const applyBtn = /** @type {HTMLButtonElement} */ ($("applyBtn"));
  const sessionSelect = /** @type {HTMLSelectElement} */ ($("sessionSelect"));
  const sessionFilter = /** @type {HTMLInputElement} */ ($("sessionFilter"));
  const sessionList = $("sessionList");
  const refreshSessionsBtn = /** @type {HTMLButtonElement} */ ($("refreshSessionsBtn"));
  const reloadBtn = /** @type {HTMLButtonElement} */ ($("reloadBtn"));
  const projectBadge = $("projectBadge");
  const sessionBadge = $("sessionBadge");
  const tokenBtn = /** @type {HTMLButtonElement} */ ($("tokenBtn"));
  const authDialog = /** @type {HTMLDialogElement} */ ($("authDialog"));
  const authForm = /** @type {HTMLFormElement} */ ($("authForm"));
  const tokenInput = /** @type {HTMLInputElement} */ ($("tokenInput"));
  const clearTokenBtn = /** @type {HTMLButtonElement} */ ($("clearTokenBtn"));
  const cancelTokenBtn = /** @type {HTMLButtonElement} */ ($("cancelTokenBtn"));

  const messageList = $("messageList");
  const newMsgBtn = /** @type {HTMLButtonElement} */ ($("newMsgBtn"));

  const composerForm = /** @type {HTMLFormElement} */ ($("composerForm"));
  const composerInput = /** @type {HTMLTextAreaElement} */ ($("composerInput"));
  const sendBtn = /** @type {HTMLButtonElement} */ ($("sendBtn"));

  /** @type {EventSource | null} */
  let es = null;
  /** @type {string} */
  let currentProject = "default";
  /** @type {string} */
  let currentSession = "default";
  /** @type {string} */
  let token = "";

  /** @type {Map<string, any>} */
  const seen = new Map();

  /** @type {number | null} */
  let reconnectTimer = null;
  /** @type {number} */
  let reconnectAttempt = 0;
  /** @type {boolean} */
  let everConnectedForThisTarget = false;
  /** @type {Array<{id:string,label:string}>} */
  let sessionItems = [];
  /** @type {boolean} */
  let authPromptShown = false;

  const tokenStorageKey = "openhouse_webclient_token";

  const setStatus = (mode, text) => {
    statusPill.classList.remove("pill--neutral", "pill--ok", "pill--warn", "pill--bad");
    const cls =
      mode === "ok" ? "pill--ok" : mode === "warn" ? "pill--warn" : mode === "bad" ? "pill--bad" : "pill--neutral";
    statusPill.classList.add(cls);
    statusText.textContent = text;
  };

  const setDrawer = (open) => {
    app.dataset.drawer = open ? "open" : "closed";
    drawerOverlay.hidden = !open;
  };

  const updateTargetChrome = () => {
    projectBadge.textContent = currentProject;
    projectBadge.title = currentProject;
    sessionBadge.textContent = currentSession;
    sessionBadge.title = currentSession;
    projectInput.value = currentProject;
    sessionInput.value = currentSession;
  };

  const readParams = () => {
    const u = new URL(window.location.href);
    const project = (u.searchParams.get("project") || "").trim();
    const session = (u.searchParams.get("session") || "").trim();
    const t = (u.searchParams.get("token") || "").trim();
    if (project) currentProject = project;
    if (session) currentSession = session;
    token = (window.localStorage.getItem(tokenStorageKey) || "").trim();
    if (t) {
      token = t;
      window.localStorage.setItem(tokenStorageKey, token);
      u.searchParams.delete("token");
      window.history.replaceState({}, "", u.toString());
    }
  };

  const writeParams = () => {
    const u = new URL(window.location.href);
    u.searchParams.set("project", currentProject);
    u.searchParams.set("session", currentSession);
    u.searchParams.delete("token");
    window.history.replaceState({}, "", u.toString());
  };

  const saveToken = (value) => {
    token = value.trim();
    if (token) window.localStorage.setItem(tokenStorageKey, token);
    else window.localStorage.removeItem(tokenStorageKey);
    authPromptShown = false;
    writeParams();
  };

  const openAuthDialog = () => {
    tokenInput.value = token;
    if (typeof authDialog.showModal === "function") {
      authDialog.showModal();
    } else {
      authDialog.setAttribute("open", "");
    }
    window.setTimeout(() => tokenInput.focus(), 0);
  };

  const closeAuthDialog = () => {
    if (typeof authDialog.close === "function") {
      authDialog.close();
    } else {
      authDialog.removeAttribute("open");
    }
  };

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  const apiUrl = (path) => {
    if (!token) return path;
    // Keep token for EventSource (no headers) and for query-based auth backends.
    try {
      const u = new URL(path, window.location.origin);
      if (!u.searchParams.get("token")) u.searchParams.set("token", token);
      return u.pathname + (u.search ? u.search : "");
    } catch {
      const sep = path.includes("?") ? "&" : "?";
      if (path.includes("token=")) return path;
      return `${path}${sep}token=${encodeURIComponent(token)}`;
    }
  };

  const assetUrl = (raw) => {
    const value = String(raw || "");
    if (!token || !value) return value;
    try {
      const u = new URL(value, window.location.origin);
      if (u.origin !== window.location.origin) return value;
      if (!u.searchParams.get("token")) u.searchParams.set("token", token);
      return u.pathname + (u.search ? u.search : "");
    } catch {
      return value;
    }
  };

  const withAuth = (init = {}) => {
    if (!token) return init;
    const headers = new Headers(init.headers || {});
    if (!headers.get("Authorization")) headers.set("Authorization", `Bearer ${token}`);
    return { ...init, headers };
  };

  const safeJson = async (res) => {
    const text = await res.text();
    if (!text) return null;
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  };

  const fetchJson = async (path, init) => {
    const res = await fetch(apiUrl(path), withAuth(init));
    if (!res.ok) {
      const body = await safeJson(res);
      const msg = typeof body === "string" ? body : JSON.stringify(body);
      throw new Error(`${res.status} ${res.statusText}${msg ? `: ${msg}` : ""}`);
    }
    return await safeJson(res);
  };

  const basename = (p) => {
    try {
      const u = new URL(p, window.location.origin);
      const parts = u.pathname.split("/").filter(Boolean);
      return parts.length ? parts[parts.length - 1] : p;
    } catch {
      const parts = String(p).split("/").filter(Boolean);
      return parts.length ? parts[parts.length - 1] : String(p);
    }
  };

  const isImageAttachment = (att) => {
    const url = (att && att.url) || "";
    const kind = (att && att.kind) || "";
    if (String(kind).toLowerCase() === "image") return true;
    const type = (att && (att.content_type || att.mime_type || att.mime || att.type)) || "";
    if (typeof type === "string" && type.toLowerCase().startsWith("image/")) return true;
    const lower = String(url).toLowerCase();
    return (
      lower.endsWith(".png") ||
      lower.endsWith(".jpg") ||
      lower.endsWith(".jpeg") ||
      lower.endsWith(".gif") ||
      lower.endsWith(".webp") ||
      lower.endsWith(".bmp") ||
      lower.endsWith(".svg")
    );
  };

  const msgKey = (m) => {
    if (!m || typeof m !== "object") return `raw:${String(m)}`;
    const id = m.id ?? m.message_id ?? m.mid ?? null;
    if (id !== null && id !== undefined && String(id)) return `id:${String(id)}`;
    const t = m.created_at ?? m.createdAt ?? m.ts ?? m.time ?? "";
    const role = m.role ?? m.from ?? m.sender ?? "";
    const content = m.content ?? m.text ?? "";
    return `hash:${String(t)}|${String(role)}|${String(content).slice(0, 80)}|${String(content).length}`;
  };

  const nearBottom = () => {
    const el = messageList;
    const threshold = 240; // px
    return el.scrollHeight - el.scrollTop - el.clientHeight < threshold;
  };

  const scrollToBottom = () => {
    messageList.scrollTop = messageList.scrollHeight;
  };

  const showNewMsgIndicator = () => {
    newMsgBtn.hidden = false;
  };

  const hideNewMsgIndicator = () => {
    newMsgBtn.hidden = true;
  };

  const formatTime = (m) => {
    const raw = m.timestamp ?? m.created_at ?? m.createdAt ?? m.ts ?? m.time ?? null;
    if (!raw) return "";
    const d = typeof raw === "number" ? new Date(raw) : new Date(String(raw));
    if (Number.isNaN(d.getTime())) return String(raw);
    return d.toLocaleString();
  };

  const normalizeRole = (m) => {
    const r = (m && (m.role ?? m.from ?? m.sender ?? m.author)) || "";
    const s = String(r).toLowerCase();
    if (s.includes("user")) return "user";
    if (s.includes("assistant") || s.includes("agent") || s.includes("bot")) return "assistant";
    if (s.includes("system")) return "system";
    return s || "assistant";
  };

  const renderMessage = (m) => {
    const role = normalizeRole(m);
    const wrap = document.createElement("div");
    wrap.className = `msg ${role === "user" ? "msg--user" : ""}`;

    const meta = document.createElement("div");
    meta.className = "msg__meta";
    const time = formatTime(m);
    meta.textContent = time ? `${role} - ${time}` : role;

    const bubble = document.createElement("div");
    bubble.className = "bubble";

    const text = document.createElement("div");
    text.className = "bubble__text";
    const content = m && (m.content ?? m.text ?? m.message ?? "");
    text.textContent = content === null || content === undefined ? "" : String(content);

    bubble.appendChild(text);

    const atts = m && (m.attachments ?? m.files ?? m.artifacts ?? null);
    if (Array.isArray(atts) && atts.length) {
      const attWrap = document.createElement("div");
      attWrap.className = "attachments";

      for (const a of atts) {
        const url = a && (a.url ?? a.href ?? a.path ?? "");
        if (!url) continue;
        const name = (a && (a.name ?? a.filename ?? a.title)) || basename(url);
        const tag = (a && (a.content_type || a.mime_type || a.mime || a.type || a.kind)) || "";

        if (isImageAttachment({ url, ...a })) {
          const imgWrap = document.createElement("div");
          imgWrap.className = "attimgWrap";

          const img = document.createElement("img");
          img.className = "attimg";
          img.loading = "lazy";
          img.alt = name;
          img.src = assetUrl(url);

          const row = document.createElement("div");
          row.className = "att";

          const badge = document.createElement("span");
          badge.className = "att__tag";
          badge.textContent = tag ? String(tag) : "image";

          const link = document.createElement("a");
          link.href = assetUrl(url);
          link.target = "_blank";
          link.rel = "noreferrer";
          link.textContent = name;

          row.appendChild(badge);
          row.appendChild(link);

          imgWrap.appendChild(img);
          imgWrap.appendChild(row);
          attWrap.appendChild(imgWrap);
        } else {
          const row = document.createElement("div");
          row.className = "att";

          const badge = document.createElement("span");
          badge.className = "att__tag";
          badge.textContent = tag ? String(tag) : "file";

          const link = document.createElement("a");
          link.href = assetUrl(url);
          link.target = "_blank";
          link.rel = "noreferrer";
          link.textContent = name;

          row.appendChild(badge);
          row.appendChild(link);
          attWrap.appendChild(row);
        }
      }
      if (attWrap.childNodes.length) bubble.appendChild(attWrap);
    }

    wrap.appendChild(meta);
    wrap.appendChild(bubble);
    return wrap;
  };

  const clearMessages = () => {
    seen.clear();
    messageList.textContent = "";
    hideNewMsgIndicator();
  };

  const appendMessage = (m, { source } = { source: "unknown" }) => {
    const key = msgKey(m);
    if (seen.has(key)) return false;
    seen.set(key, m);

    const shouldStick = nearBottom();
    messageList.appendChild(renderMessage(m));
    if (shouldStick) {
      scrollToBottom();
      hideNewMsgIndicator();
    } else if (source === "sse") {
      showNewMsgIndicator();
    }
    return true;
  };

  const setLoadingState = (loading) => {
    sendBtn.disabled = loading;
  };

  const checkHealth = async () => {
    try {
      const res = await fetch(apiUrl("/healthz"), withAuth());
      if (res.status === 401) {
        setStatus("bad", "auth required");
        if (!authPromptShown) {
          authPromptShown = true;
          openAuthDialog();
        }
        return false;
      }
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      setStatus("ok", "connected");
      return true;
    } catch {
      setStatus("bad", "offline");
      return false;
    }
  };

  const loadSessions = async () => {
    const path = `/api/projects/${encodeURIComponent(currentProject)}/sessions`;
    const data = await fetchJson(path);

    const items = [];
    if (Array.isArray(data)) {
      for (const x of data) {
        if (x && typeof x === "object") {
          const id = String(x.id ?? x.session ?? x.name ?? "");
          if (!id) continue;
          const label = String(x.label ?? x.title ?? id);
          items.push({ id, label });
        } else if (x !== null && x !== undefined) {
          const id = String(x);
          if (!id) continue;
          items.push({ id, label: id });
        }
      }
    }

    items.sort((a, b) => a.id.localeCompare(b.id));
    sessionItems = items;

    sessionSelect.textContent = "";
    const opt0 = document.createElement("option");
    opt0.value = "";
    opt0.textContent = items.length ? "Select..." : "No sessions";
    sessionSelect.appendChild(opt0);

    for (const it of items) {
      const opt = document.createElement("option");
      opt.value = it.id;
      opt.textContent = it.label;
      if (it.id === currentSession) opt.selected = true;
      sessionSelect.appendChild(opt);
    }
    renderSessionList();
  };

  const renderSessionList = () => {
    const filter = sessionFilter.value.trim().toLowerCase();
    const visible = filter
      ? sessionItems.filter((it) => `${it.id} ${it.label}`.toLowerCase().includes(filter))
      : sessionItems;

    sessionList.textContent = "";
    if (!visible.length) {
      const empty = document.createElement("div");
      empty.className = "sessionEmpty";
      empty.textContent = sessionItems.length ? "No matches" : "No sessions";
      sessionList.appendChild(empty);
      return;
    }

    for (const it of visible) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "sessionItem";
      btn.setAttribute("role", "option");
      btn.setAttribute("aria-selected", it.id === currentSession ? "true" : "false");
      btn.title = it.label;

      const id = document.createElement("span");
      id.className = "sessionItem__id";
      id.textContent = it.label;

      const tag = document.createElement("span");
      tag.className = "sessionItem__tag";
      tag.textContent = it.id === currentSession ? "active" : "open";

      btn.appendChild(id);
      btn.appendChild(tag);
      btn.addEventListener("click", async () => {
        sessionInput.value = it.id;
        setDrawer(false);
        await applyProjectSession();
      });
      sessionList.appendChild(btn);
    }
  };

  const loadHistory = async () => {
    const path = `/api/projects/${encodeURIComponent(currentProject)}/sessions/${encodeURIComponent(
      currentSession
    )}/messages`;
    const data = await fetchJson(path);

    clearMessages();
    if (Array.isArray(data)) {
      for (const m of data) appendMessage(m, { source: "history" });
      scrollToBottom();
      return;
    }
    if (data && typeof data === "object" && Array.isArray(data.messages)) {
      for (const m of data.messages) appendMessage(m, { source: "history" });
      scrollToBottom();
    }
  };

  const stopEvents = () => {
    if (reconnectTimer) {
      window.clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    if (es) {
      try {
        es.close();
      } catch {}
      es = null;
    }
  };

  const scheduleReconnect = () => {
    if (reconnectTimer) return;
    reconnectAttempt = Math.min(reconnectAttempt + 1, 7);
    const backoff = Math.min(1000 * 2 ** reconnectAttempt, 15000);
    reconnectTimer = window.setTimeout(async () => {
      reconnectTimer = null;
      await startEvents();
    }, backoff);
  };

  const startEvents = async () => {
    stopEvents();
    const ok = await checkHealth();
    if (!ok) {
      scheduleReconnect();
      return;
    }

    const path = `/api/projects/${encodeURIComponent(currentProject)}/sessions/${encodeURIComponent(
      currentSession
    )}/events`;
    setStatus("warn", "connecting");
    everConnectedForThisTarget = false;

    try {
      es = new EventSource(apiUrl(path));
    } catch {
      setStatus("bad", "offline");
      scheduleReconnect();
      return;
    }

    es.onopen = async () => {
      const isReconnect = reconnectAttempt > 0 || everConnectedForThisTarget;
      everConnectedForThisTarget = true;
      reconnectAttempt = 0;
      setStatus("ok", "connected");
      if (isReconnect) {
        try {
          await loadHistory();
        } catch {}
      }
    };

    es.onmessage = (ev) => {
      try {
        const m = JSON.parse(ev.data);
        appendMessage(m, { source: "sse" });
      } catch {
        // Ignore non-JSON payloads.
      }
    };

    es.onerror = () => {
      setStatus("warn", "reconnecting");
      scheduleReconnect();
    };
  };

  const applyProjectSession = async () => {
    const p = projectInput.value.trim() || "default";
    const s = sessionInput.value.trim() || "default";
    const changed = p !== currentProject || s !== currentSession;
    currentProject = p;
    currentSession = s;
    writeParams();
    updateTargetChrome();
    renderSessionList();
    if (!changed) return;
    reconnectAttempt = 0;

    try {
      await loadSessions();
    } catch {}
    try {
      await loadHistory();
    } catch {}
    await startEvents();
  };

  const sendMessage = async (content) => {
    const path = `/api/projects/${encodeURIComponent(currentProject)}/sessions/${encodeURIComponent(
      currentSession
    )}/messages`;

    await fetchJson(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content }),
    });
  };

  const autosizeTextarea = () => {
    const el = composerInput;
    el.style.height = "auto";
    const h = Math.min(el.scrollHeight, 140);
    el.style.height = `${h}px`;
  };

  const init = async () => {
    readParams();
    updateTargetChrome();

    setStatus("neutral", "disconnected");

    tokenBtn.addEventListener("click", () => openAuthDialog());
    authForm.addEventListener("submit", async (e) => {
      e.preventDefault();
      saveToken(tokenInput.value);
      closeAuthDialog();
      try {
        await loadSessions();
      } catch {}
      try {
        await loadHistory();
      } catch {}
      await startEvents();
    });
    clearTokenBtn.addEventListener("click", async () => {
      saveToken("");
      closeAuthDialog();
      await startEvents();
    });
    cancelTokenBtn.addEventListener("click", () => closeAuthDialog());

    menuBtn.addEventListener("click", () => setDrawer(true));
    drawerCloseBtn.addEventListener("click", () => setDrawer(false));
    drawerOverlay.addEventListener("click", () => setDrawer(false));
    window.addEventListener("keydown", (e) => {
      if (e.key === "Escape") setDrawer(false);
    });

    newMsgBtn.addEventListener("click", () => {
      scrollToBottom();
      hideNewMsgIndicator();
    });

    messageList.addEventListener("scroll", () => {
      if (nearBottom()) hideNewMsgIndicator();
    });

    applyBtn.addEventListener("click", async () => {
      await applyProjectSession();
    });

    refreshSessionsBtn.addEventListener("click", async () => {
      try {
        await loadSessions();
      } catch {}
    });

    reloadBtn.addEventListener("click", async () => {
      try {
        await loadSessions();
      } catch {}
      try {
        await loadHistory();
      } catch {}
      await startEvents();
    });

    sessionFilter.addEventListener("input", () => renderSessionList());

    sessionSelect.addEventListener("change", async () => {
      const v = sessionSelect.value.trim();
      if (!v) return;
      sessionInput.value = v;
      await applyProjectSession();
    });

    composerInput.addEventListener("input", () => autosizeTextarea());
    autosizeTextarea();

    composerForm.addEventListener("submit", async (e) => {
      e.preventDefault();
      const content = composerInput.value.trim();
      if (!content) return;

      setLoadingState(true);
      try {
        await sendMessage(content);
        composerInput.value = "";
        autosizeTextarea();
        composerInput.focus();
        // Expect SSE to deliver; resync defensively.
        await sleep(120);
        await loadHistory();
      } catch {
        setStatus("bad", "send failed");
      } finally {
        setLoadingState(false);
      }
    });

    // Initial boot
    try {
      await loadSessions();
    } catch {}
    try {
      await loadHistory();
    } catch {}
    await startEvents();
  };

  window.addEventListener("beforeunload", () => stopEvents());
  init();
})();
