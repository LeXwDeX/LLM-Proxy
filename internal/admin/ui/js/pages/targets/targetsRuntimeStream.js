"use strict";
// Issue 225: one live runtime stream per page. The targets and pools sub-tabs
// share the /admin/events snapshot feed; while it is unavailable the same
// applyRuntimeSnapshot path consumes /admin/metrics polling instead. The stream
// is a foreground observer only: it closes when the tab is hidden, when another
// page is selected and on pagehide, then reconnects from a full snapshot.
// Part of the targets page; the main module re-exports subscribe/status.
AdminApp.register("targetsRuntimeStream", function (app) {
  var state = app.state;
  var api = app.bind("core", "api");
  var applyRuntimeSnapshot = app.bind("targets", "applyRuntimeSnapshot");
  app.requires(["targets"]);

  var EVENTS_URL = "/admin/events";
  var METRICS_URL = "/admin/metrics";
  var SESSION_URL = "/admin/api/me";
  // The hub emits a frame every 5s and the fallback polls every 30s; three
  // missed intervals mean the view no longer reflects the server.
  var EXPECTED_FRAME_MS = 5000;
  var POLL_MS = 30000;
  var RETRY_BASE_MS = 5000;
  var RETRY_MAX_MS = 60000;
  // EventSource.readyState (constants are not always exposed on the constructor).
  var CONNECTING = 0;
  var CLOSED = 2;
  var STATUS_ID = "targetsRuntimeStatus";

  var source = null;
  var pollTimer = null;
  var retryTimer = null;
  var watchdogTimer = null;
  var retryDelay = RETRY_BASE_MS;
  var polling = false;
  var sessionStopped = false;
  var lastFrameAt = 0;
  var mode = "idle";

  // window.EventSource keeps the UI lint (ui_syntax_test) aware that this is a
  // host global, not a locally declared helper.
  function streamSupported() {
    return typeof window !== "undefined" && typeof window.EventSource === "function";
  }
  function pageActive() {
    return state.currentPage === "targets" && !document.hidden;
  }
  function sessionExpired(error) {
    return !!error && (error.name === "SessionExpiredError" || error.status === 401);
  }

  /* ===== Read-only status badge (targets panel header) ===== */
  function statusText() {
    if (mode === "live") return "实时更新";
    if (mode === "connecting") return "实时更新连接中";
    if (mode === "polling") return polling ? "轮询中" : "实时更新连接中";
    if (mode === "stale") return polling ? "轮询中（更新停滞）" : "实时更新已断开";
    if (mode === "stopped") return "实时更新已断开（会话需重新登录）";
    if (mode === "disconnected")
      return polling ? "实时更新已断开，轮询中" : "实时更新已断开，重连中";
    return "";
  }
  function renderStatus() {
    var el = document.getElementById(STATUS_ID);
    if (!el) return;
    var text = statusText();
    el.hidden = !text;
    if (el.textContent !== text) el.textContent = text;
    ["tone-ok", "tone-warn", "tone-bad"].forEach(function (cls) {
      el.classList.remove(cls);
    });
    if (mode === "live") el.classList.add("tone-ok");
    else if (mode === "connecting" || mode === "polling" || mode === "disconnected")
      el.classList.add("tone-warn");
    else if (text) el.classList.add("tone-bad");
    el.title = text ? "目标运行态实时通道：" + text : "";
  }
  function setMode(next) {
    mode = next;
    renderStatus();
  }

  /* ===== Timers ===== */
  function clearPollTimer() {
    if (pollTimer !== null) clearTimeout(pollTimer);
    pollTimer = null;
  }
  function stopPolling() {
    polling = false;
    clearPollTimer();
  }
  function clearRetryTimer() {
    if (retryTimer !== null) clearTimeout(retryTimer);
    retryTimer = null;
  }
  function clearWatchdog() {
    if (watchdogTimer !== null) clearTimeout(watchdogTimer);
    watchdogTimer = null;
  }
  function armWatchdog() {
    clearWatchdog();
    watchdogTimer = setTimeout(function () {
      watchdogTimer = null;
      if (!pageActive()) return;
      setMode("stale");
    }, (polling ? POLL_MS : EXPECTED_FRAME_MS) * 3);
  }
  function closeSource() {
    if (!source) return;
    var es = source;
    source = null;
    try {
      es.close();
    } catch (e) {
      /* an already closed EventSource throws in some browsers */
    }
  }
  // Leaving the page, hiding the tab or losing the session tears everything
  // down; a later entry reconnects from a full snapshot.
  function shutdown() {
    stopPolling();
    clearRetryTimer();
    clearWatchdog();
    closeSource();
    retryDelay = RETRY_BASE_MS;
    setMode("idle");
  }
  function stopForSession() {
    sessionStopped = true;
    stopPolling();
    clearRetryTimer();
    clearWatchdog();
    closeSource();
    setMode("stopped");
  }

  /* ===== Fallback polling ===== */
  function schedulePoll(delay) {
    clearPollTimer();
    pollTimer = setTimeout(pollOnce, delay);
  }
  async function pollOnce() {
    pollTimer = null;
    if (!pageActive() || !polling || sessionStopped) return;
    try {
      var data = await api(METRICS_URL, { background: true });
      if (!pageActive() || !polling || sessionStopped) return;
      if (data && typeof data === "object" &&
          (data.targets_runtime === undefined || data.targets_runtime === null ||
            Array.isArray(data.targets_runtime))) {
        lastFrameAt = Date.now();
        setMode("polling");
        armWatchdog();
        applyRuntimeSnapshot(data);
      }
    } catch (e) {
      if (sessionExpired(e)) {
        stopForSession();
        return;
      }
      setMode("disconnected");
    }
    if (pageActive() && polling && !sessionStopped) schedulePoll(POLL_MS);
  }
  function startPolling() {
    if (!pageActive() || sessionStopped) return;
    polling = true;
    if (pollTimer === null) schedulePoll(0);
    setMode("polling");
  }

  /* ===== Session probe and reconnect backoff ===== */
  async function probeSession() {
    try {
      await api(SESSION_URL, { background: true });
    } catch (e) {
      if (sessionExpired(e)) stopForSession();
    }
  }
  function scheduleRetry() {
    clearRetryTimer();
    if (!pageActive() || sessionStopped) return;
    var delay = retryDelay;
    retryDelay = Math.min(retryDelay * 2, RETRY_MAX_MS);
    retryTimer = setTimeout(function () {
      retryTimer = null;
      if (!pageActive() || sessionStopped || source) return;
      if (!connect()) scheduleRetry();
    }, delay);
  }

  /* ===== EventSource ===== */
  function onSnapshot(event) {
    var data = null;
    try {
      data = JSON.parse(String((event && event.data) || ""));
    } catch (e) {
      return;
    }
    if (!data || typeof data !== "object") return;
    if (data.targets_runtime !== undefined && data.targets_runtime !== null &&
        !Array.isArray(data.targets_runtime)) return;
    lastFrameAt = Date.now();
    retryDelay = RETRY_BASE_MS;
    stopPolling();
    setMode("live");
    armWatchdog();
    applyRuntimeSnapshot(data);
  }
  function onError(es) {
    var readyState = Number(es && es.readyState);
    if (readyState === CONNECTING) {
      // The EventSource reconnects on its own (server retry hint); rebuilding it
      // here would add a second connection.
      setMode("disconnected");
      return;
    }
    if (readyState !== CLOSED) {
      setMode("disconnected");
      return;
    }
    closeSource();
    // Fatal stream failure (e.g. 404, 204, revoked session): keep the view live
    // through polling while a probe tells a dead session from a dead endpoint.
    startPolling();
    scheduleRetry();
    probeSession();
  }
  // Returns false only when the constructor failed and a retry is worthwhile;
  // an unsupported browser is already served by polling.
  function connect() {
    if (!pageActive() || sessionStopped || source) return true;
    if (!streamSupported()) {
      startPolling();
      return true;
    }
    clearRetryTimer();
    var es;
    try {
      es = new window.EventSource(EVENTS_URL);
    } catch (e) {
      startPolling();
      return false;
    }
    source = es;
    setMode("connecting");
    armWatchdog();
    es.addEventListener("open", function () {
      if (source !== es) return;
      armWatchdog();
    });
    es.addEventListener("snapshot", function (event) {
      if (source !== es) return;
      onSnapshot(event);
    });
    es.addEventListener("error", function () {
      if (source !== es) return;
      onError(es);
    });
    return true;
  }

  function subscribe(force) {
    // A page entry (not a mere visibility change) is the only thing that clears
    // an expired-session stop: the user may have logged in again.
    if (force === true) {
      sessionStopped = false;
      retryDelay = RETRY_BASE_MS;
    }
    if (!pageActive()) {
      shutdown();
      return;
    }
    if (sessionStopped || source) return;
    if (!connect()) scheduleRetry();
  }
  function status() {
    return {
      mode: mode,
      supported: streamSupported(),
      connected: !!(source && Number(source.readyState) === 1),
      polling: polling,
      lastFrameAt: lastFrameAt,
      retryDelay: retryDelay,
    };
  }

  /* ===== Foreground-only lifecycle ===== */
  function onVisibilityChange() {
    if (document.hidden) {
      shutdown();
      return;
    }
    subscribe(false);
  }
  function onHashChange() {
    var page = typeof location !== "undefined" && location
      ? String(location.hash || "").replace(/^#\/?/, "")
      : "";
    if (page === "targets") subscribe(false);
    else shutdown();
  }
  function onDocumentClick(event) {
    var target = event && event.target;
    var item = target && typeof target.closest === "function"
      ? target.closest(".sidebar-nav-item")
      : null;
    if (!item) return;
    if (item.dataset && item.dataset.page === "targets") {
      if (pageActive()) subscribe(false);
      return;
    }
    shutdown();
  }
  document.addEventListener("click", onDocumentClick, true);
  document.addEventListener("visibilitychange", onVisibilityChange);
  function onPageShow() {
    // A page restored from the back/forward cache was hidden via pagehide.
    subscribe(false);
  }
  if (typeof window !== "undefined" && typeof window.addEventListener === "function") {
    window.addEventListener("pagehide", shutdown);
    window.addEventListener("pageshow", onPageShow);
    window.addEventListener("hashchange", onHashChange);
  }

  return {
    subscribe: subscribe,
    status: status,
  };
});