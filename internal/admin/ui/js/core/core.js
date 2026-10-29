"use strict";
// Core owns shared state, API transport and modal primitives.
window.AdminApp = {
  modules: {},
  factories: [],
  referenced: {}, // module names other modules bound to (see missingModules)
  register: function (name, factory) {
    if (
      this.factories.some(function (entry) {
        return entry.name === name;
      })
    )
      throw new Error("重复模块: " + name);
    this.factories.push({ name: name, factory: factory });
  },
  bind: function (module, name) {
    var app = this;
    app.referenced[module] = true;
    return function () {
      var exports = app.modules[module];
      if (!exports || typeof exports[name] !== "function")
        throw new Error("模块依赖未就绪: " + module + "." + name);
      return exports[name].apply(exports, arguments);
    };
  },
  // Declares modules this one needs without binding an export (e.g. a part
  // that only wires event handlers), so missingModules still reports them.
  requires: function (names) {
    var app = this;
    names.forEach(function (name) {
      app.referenced[name] = true;
    });
  },
  initializeModule: function (name) {
    if (this.modules[name]) return this.modules[name];
    var entry = this.factories.find(function (candidate) {
      return candidate.name === name;
    });
    if (!entry) throw new Error("模块未注册: " + name);
    var exports;
    try {
      exports = entry.factory(this);
    } catch (e) {
      var failure = new Error("模块 " + name + " 初始化失败: " + (e && e.message));
      failure.cause = e;
      throw failure;
    }
    this.modules[name] = exports;
    return exports;
  },
  // Names (from the given list plus every module bound so far) whose factory
  // never registered, i.e. whose script failed to load or parse.
  missingModules: function (names) {
    var app = this;
    var wanted = (names || []).concat(Object.keys(app.referenced));
    return wanted.filter(function (name, i) {
      return (
        wanted.indexOf(name) === i &&
        !app.factories.some(function (entry) {
          return entry.name === name;
        })
      );
    });
  },
  initialize: function () {
    var app = this;
    app.factories.forEach(function (entry) {
      app.initializeModule(entry.name);
    });
  },
};
AdminApp.register("core", function (app) {
  /* ===== Utility ===== */
  function esc(v) {
    return String(v)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }
  function fmtNum(v) {
    return Number(v || 0).toLocaleString("zh-CN");
  }
  function fmtMoney(v) {
    return "$" + Number(v || 0).toFixed(2);
  }
  function fmtPct(v) {
    return Number(v || 0).toFixed(1) + "%";
  }
  function totalTok(r) {
    return (
      Number(r.input_tokens || 0) +
      Number(r.output_tokens || 0) +
      Number(r.cached_tokens || 0) +
      Number(r.cache_creation_tokens || 0) +
      Number(r.tool_use_prompt_tokens || 0)
    );
  }
  function maskKey(k) {
    if (!k) return "-";
    if (k.length <= 8)
      return k.substring(0, 2) + "****" + k.substring(k.length - 2);
    return k.substring(0, 4) + "****" + k.substring(k.length - 4);
  }
  function fmtTime(ts) {
    if (!ts) return "-";
    var d = new Date(ts);
    if (isNaN(d.getTime())) return String(ts);
    var pad = function (n) {
      return String(n).padStart(2, "0");
    };
    return (
      d.getFullYear() +
      "-" +
      pad(d.getMonth() + 1) +
      "-" +
      pad(d.getDate()) +
      " " +
      pad(d.getHours()) +
      ":" +
      pad(d.getMinutes()) +
      ":" +
      pad(d.getSeconds())
    );
  }
  function toLocalInput(d) {
    var local = new Date(d.getTime() - d.getTimezoneOffset() * 60000);
    return local.toISOString().slice(0, 16);
  }
  function inputToISO(id) {
    var v = document.getElementById(id).value;
    var d = new Date(v);
    if (isNaN(d.getTime())) throw new Error("请填写有效时间");
    return d.toISOString();
  }

  /* ===== Theme ===== */
  var THEME_STORAGE_KEY = "llms-proxy-admin-theme";
  function savedTheme() {
    try {
      return localStorage.getItem(THEME_STORAGE_KEY) === "dark"
        ? "dark"
        : "light";
    } catch (e) {
      return "light";
    }
  }
  function applyTheme(theme) {
    var dark = theme === "dark";
    var root = document.documentElement;
    if (root && root.classList) {
      root.classList.toggle("dark", dark);
      root.setAttribute("data-theme", dark ? "dark" : "light");
    }
    var toggle = document.getElementById("themeToggle");
    if (toggle) {
      var label = dark ? "切换到亮色模式" : "切换到暗色模式";
      toggle.setAttribute("aria-pressed", dark ? "true" : "false");
      toggle.setAttribute("aria-label", label);
      toggle.setAttribute("title", label);
    }
  }
  applyTheme(savedTheme());
  var themeToggle = document.getElementById("themeToggle");
  if (themeToggle) {
    themeToggle.addEventListener("click", function () {
      var root = document.documentElement;
      if (!root || !root.classList) return;
      var next = root.classList.contains("dark") ? "light" : "dark";
      applyTheme(next);
      try {
        localStorage.setItem(THEME_STORAGE_KEY, next);
      } catch (e) {
        /* storage can be unavailable in private or embedded contexts */
      }
    });
  }

  /* ===== Toast ===== */
  var toastContainer = document.getElementById("toastContainer");
  function toast(msg, type) {
    var el = document.createElement("div");
    el.className = "toast " + (type || "info");
    el.textContent = msg;
    toastContainer.appendChild(el);
    setTimeout(function () {
      el.remove();
    }, 3000);
  }

  /* ===== API ===== */
  // Every request has a deadline: a keep-alive socket silently dropped by a
  // middlebox would otherwise leave fetch pending forever (Issue #54).
  // opts.timeoutMs overrides it; 0 disables it for intentionally long calls.
  var API_TIMEOUT_MS = 15000;
  var loginRedirecting = false;
  var LOGIN_REDIRECT_RESET_MS = 5000;
  function namedError(name, message) {
    var error = new Error(message);
    error.name = name;
    return error;
  }
  // Timeouts and fetch network failures (TypeError) are worth a fresh attempt;
  // HTTP errors and validation failures are deterministic and are not.
  // Only failures marked by api() itself count: a TypeError thrown by page code
  // is a bug, not a flaky network, and must not be retried.
  function isTransientError(error) {
    return (
      !!error && (error.name === "TimeoutError" || error.isNetworkError === true)
    );
  }
  // A write whose outcome the client cannot know: the request may or may not have
  // been applied. Timeouts, network failures, any HTTP 5xx (a gateway 502/503/504
  // can sit in front of a server that did commit) and error bodies that are not
  // JSON (a proxy's HTML page, not our server's answer). Callers must look at the
  // server's state before reporting success or failure. Deterministic 4xx answers
  // with a JSON body are definite failures and are not unconfirmed.
  function isUnconfirmedOutcome(error) {
    if (!error) return false;
    if (error.name === "TimeoutError" || error.isNetworkError === true) return true;
    if (typeof error.status === "number") {
      if (error.status >= 500) return true;
      if (error.invalidBody === true) return true;
    }
    return false;
  }
  // fetch() rejects with TypeError on network failure; aborts keep their name.
  function asNetworkError(e) {
    if (!e || e.name !== "TypeError") return e;
    var error = new Error(e.message);
    error.name = "NetworkError";
    error.isNetworkError = true;
    error.cause = e;
    return error;
  }
  // Matches SessionManager.unauthorized (session.go): 401 {"error":"unauthorized"}.
  // Other 401 bodies (e.g. "not authenticated") and other statuses never navigate.
  // next returns the user to this page; portal.sanitizeNext accepts /admin paths
  // and falls back to /admin for anything else.
  function redirectToLogin() {
    if (loginRedirecting) return;
    loginRedirecting = true;
    if (
      typeof location === "undefined" ||
      !location ||
      typeof location.assign !== "function"
    ) {
      loginRedirecting = false;
      return;
    }
    var here =
      (location.pathname || "") + (location.search || "") + (location.hash || "");
    location.assign("/login" + (here ? "?next=" + encodeURIComponent(here) : ""));
    // A page that is really leaving never runs this timer; if navigation was
    // blocked (beforeunload, offline), later 401s must be able to retry.
    setTimeout(function () {
      loginRedirecting = false;
    }, LOGIN_REDIRECT_RESET_MS);
  }

  /* ===== Banners (session expiry, stale deploy) ===== */
  var BANNER_STYLE =
    "position:fixed;left:0;right:0;z-index:3000;padding:10px 16px;display:flex;gap:12px;align-items:center;justify-content:center;font-size:14px;";
  function showBanner(id, position, message, action) {
    if (document.getElementById(id)) return;
    var el = document.createElement("div");
    el.id = id;
    el.setAttribute("role", "alert");
    el.style.cssText =
      BANNER_STYLE +
      position +
      ":0;background:var(--warn-soft,#fff3cd);color:var(--warn,#7a4b00);box-shadow:0 1px 6px rgba(0,0,0,.2);";
    var text = document.createElement("span");
    text.textContent = message;
    el.appendChild(text);
    if (action) el.appendChild(action);
    document.body.appendChild(el);
  }
  function removeBanner(id) {
    var el = document.getElementById(id);
    if (el) el.remove();
  }
  // True when navigating away would discard user input: an open modal/drawer
  // or a form that registered itself through setFormDirty.
  var dirtyForms = {};
  function setFormDirty(key, dirty) {
    if (dirty) dirtyForms[key] = true;
    else delete dirtyForms[key];
  }
  function hasUnsavedUi() {
    if (Object.keys(dirtyForms).length > 0) return true;
    try {
      return (
        document.querySelectorAll(
          ".modal-overlay.open, .quota-drawer-overlay.open",
        ).length > 0 ||
        document.querySelectorAll('[data-dirty="true"]').length > 0
      );
    } catch (e) {
      return false;
    }
  }
  var SESSION_BANNER_ID = "sessionExpiredBanner";
  var SESSION_EXPIRED_MESSAGE = "会话已过期，请在新标签页登录后重试";
  function showSessionExpiredBanner() {
    var link = document.createElement("a");
    link.href = "/login?next=/admin";
    link.target = "_blank";
    link.rel = "noopener";
    link.textContent = "打开登录页";
    link.style.cssText = "font-weight:600;text-decoration:underline;color:inherit;";
    showBanner(SESSION_BANNER_ID, "top", SESSION_EXPIRED_MESSAGE, link);
  }
  var knownVersion = null;
  var STALE_BANNER_ID = "adminStaleBanner";
  function noteServerVersion(version) {
    if (typeof version !== "string" || !version) return;
    if (knownVersion === null) {
      knownVersion = version;
      return;
    }
    if (version === knownVersion) return;
    var reload = document.createElement("button");
    reload.type = "button";
    reload.className = "btn btn-sm";
    reload.textContent = "刷新页面";
    reload.addEventListener("click", function () {
      if (typeof location !== "undefined" && location && location.reload)
        location.reload();
    });
    showBanner(STALE_BANNER_ID, "bottom", "后台已更新，请刷新页面", reload);
  }
  // A cheap background /api/me check (does not slide the session) catches a
  // deploy that happened while this tab stayed open.
  var VERSION_CHECK_MS = 5 * 60 * 1000;
  var versionWatching = false;
  function checkVersion() {
    if (document.getElementById(STALE_BANNER_ID)) return Promise.resolve();
    return api("/admin/api/me", { background: true }).catch(function () {});
  }
  function startVersionWatch() {
    if (versionWatching) return;
    versionWatching = true;
    if (typeof setInterval === "function") setInterval(checkVersion, VERSION_CHECK_MS);
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) checkVersion();
    });
  }
  async function api(url, opts) {
    var init = Object.assign({}, opts || {});
    var timeoutMs =
      init.timeoutMs === undefined || init.timeoutMs === null
        ? API_TIMEOUT_MS
        : Number(init.timeoutMs) || 0;
    delete init.timeoutMs;
    var background = init.background === true;
    delete init.background;
    var callerSignal = init.signal || null;
    init.headers = Object.assign({}, init.headers || {});
    init.headers["X-Requested-With"] = "XMLHttpRequest";
    // Polls must not keep the session alive (SessionManager.Middleware).
    if (background) init.headers["X-Admin-Background"] = "1";
    if (init.body && typeof init.body !== "string") {
      init.body = JSON.stringify(init.body);
      init.headers["Content-Type"] = "application/json";
    }
    // Either the caller's signal or the deadline aborts the real fetch. The
    // guard also settles the request when a transport ignores abort, and it
    // spans reading the body, not only the response headers.
    var controller =
      typeof AbortController === "function" ? new AbortController() : null;
    if (controller) init.signal = controller.signal;
    var timer = null;
    var onCallerAbort = null;
    var failure = null;
    var guard = new Promise(function (resolve, reject) {
      function fail(error) {
        if (failure) return;
        failure = error;
        reject(error);
        if (controller) controller.abort();
      }
      if (callerSignal) {
        onCallerAbort = function () {
          fail(namedError("AbortError", "请求已取消"));
        };
        if (callerSignal.aborted) onCallerAbort();
        else
          callerSignal.addEventListener("abort", onCallerAbort, { once: true });
      }
      if (timeoutMs > 0)
        timer = setTimeout(function () {
          fail(
            namedError(
              "TimeoutError",
              "请求超时（" + Math.round(timeoutMs / 1000) + " 秒无响应）",
            ),
          );
        }, timeoutMs);
    });
    guard.catch(function () {});
    var exchange = (async function () {
      var resp, text;
      try {
        resp = await fetch(url, init);
        text = await resp.text();
      } catch (e) {
        throw asNetworkError(e);
      }
      return { resp: resp, text: text };
    })();
    exchange.catch(function () {});
    var result;
    try {
      result = await Promise.race([exchange, guard]);
    } catch (e) {
      throw failure || e;
    } finally {
      if (timer !== null) clearTimeout(timer);
      if (callerSignal && onCallerAbort)
        callerSignal.removeEventListener("abort", onCallerAbort);
    }
    var resp = result.resp;
    var text = result.text;
    var data = {};
    var invalidBody = !text;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (e) {
        data = { raw: text };
        invalidBody = true;
      }
    }
    if (resp.status === 401 && data && data.error === "unauthorized") {
      // Never throw away unsaved input: keep the page and ask for a re-login
      // in another tab; the caller keeps its form and may retry afterwards.
      // Background polls never navigate: they may fire while the user edits an
      // inline form that is invisible to hasUnsavedUi().
      if (background || hasUnsavedUi()) {
        showSessionExpiredBanner();
        throw namedError("SessionExpiredError", SESSION_EXPIRED_MESSAGE);
      }
      redirectToLogin();
    }
    if (resp.ok) {
      removeBanner(SESSION_BANNER_ID);
      if (/^\/admin\/api\/me(\?|$)/.test(url)) noteServerVersion(data && data.version);
    }
    if (!resp.ok) {
      // status and code let callers tell a revision conflict from a generic failure.
      var httpError = new Error(data.error || "请求失败 (HTTP " + resp.status + ")");
      httpError.status = resp.status;
      if (invalidBody) httpError.invalidBody = true;
      if (data && typeof data.code === "string") httpError.code = data.code;
      throw httpError;
    }
    return data;
  }

  /* ===== Global error handlers ===== */
  // Last line of defence: log everything, tell the user at most once per window.
  var errorToastCooldown = false;
  function reportUnhandled(kind, reason) {
    console.error("[admin] " + kind, reason);
    if (reason && (reason.name === "SessionExpiredError" || reason.name === "AbortError"))
      return;
    if (errorToastCooldown) return;
    errorToastCooldown = true;
    setTimeout(function () {
      errorToastCooldown = false;
    }, 5000);
    var detail = reason && reason.message ? reason.message : String(reason || "");
    toast("页面出现未处理的错误" + (detail ? ": " + detail : ""), "error");
  }
  if (typeof window.addEventListener === "function") {
    window.addEventListener("error", function (event) {
      var message = (event && event.message) || "";
      // Benign browser notification, not a failure.
      if (event && !event.error && /^ResizeObserver loop/.test(message)) return;
      // Script load/parse failures of our own modules belong to the boot watchdog.
      if (
        event &&
        String(event.filename || "").indexOf("/admin/ui/js/") >= 0 &&
        (!event.error || event.error.name === "SyntaxError")
      )
        return;
      reportUnhandled("error", (event && (event.error || event.message)) || event);
    });
    window.addEventListener("unhandledrejection", function (event) {
      reportUnhandled("unhandledrejection", event && event.reason);
    });
  }

  /* ===== Modal ===== */
  function openModal(id) {
    if (!state.ready) {
      toast("管理元数据尚未就绪，请先重试加载", "error");
      return;
    }
    var el = document.getElementById(id);
    if (!el) {
      console.warn("openModal: 目标弹层不存在: #" + id);
      return;
    }
    el.classList.add("open");
  }
  function closeModal(id) {
    var el = document.getElementById(id);
    if (!el) {
      console.warn("closeModal: 目标弹层不存在: #" + id);
      return;
    }
    el.classList.remove("open");
  }
  document.querySelectorAll("[data-close]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      closeModal(btn.dataset.close);
    });
  });
  document.querySelectorAll(".modal-overlay").forEach(function (overlay) {
    // Only close on overlay click for confirm dialogs, not for form modals
  });

  var confirmCallback = null;
  function showConfirm(msg, cb) {
    document.getElementById("confirmMessage").textContent = msg;
    confirmCallback = cb;
    openModal("confirmModal");
  }
  document
    .getElementById("confirmOkBtn")
    .addEventListener("click", function () {
      closeModal("confirmModal");
      if (confirmCallback) {
        confirmCallback();
        confirmCallback = null;
      }
    });

  /* ===== State ===== */
  var state = (app.state = {
    ready: false,
    schema: null,
    targets: [],
    editingTarget: null,
    pools: [],
    runtime: {}, // S5 调度可观测：{name_lower: {phase,in_flight,max_concurrency,utilization,affinity_hits,breaker_trips}}
    timeSegments: [],
    editingPool: null,
    clients: [],
    editingClient: null,
    usageRange: { from: null, to: null },
    catalogAllCache: null,
    catalogCache: {},
    endpointTypeMetas: [],
    modelGroups: [],
    modelGroupsLoaded: false,
    modelGroupsPromise: null,
    editingModelGroup: null,
    jobLevels: [],
    projectGroups: [],
    dictsLoaded: false,
    editingDict: null,
    quotaExpanded: {},
    pricingExpanded: {},
    usageModelGroupExpanded: {},
    usageModelTargetExpanded: {},
    currentUser: null,
  });

  window.state = state;
  window.esc = esc;
  window.toast = toast;
  window.api = api;
  window.openModal = openModal;
  window.closeModal = closeModal;
  window.showConfirm = showConfirm;
  window.fmtNum = fmtNum;
  window.fmtMoney = fmtMoney;
  window.fmtTime = fmtTime;

  // Runs fn once at a time per key; the button stays disabled while it is in flight and is
  // re-enabled afterwards (never restored from a value captured at start).
  var inflight = Object.create(null);
  async function runGuarded(key, button, fn) {
    if (inflight[key]) return;
    inflight[key] = true;
    if (button) button.disabled = true;
    try {
      return await fn();
    } finally {
      inflight[key] = false;
      if (button) button.disabled = false;
    }
  }
  return {
    runGuarded: runGuarded,
    esc: esc,
    fmtNum: fmtNum,
    fmtMoney: fmtMoney,
    fmtPct: fmtPct,
    totalTok: totalTok,
    maskKey: maskKey,
    fmtTime: fmtTime,
    toLocalInput: toLocalInput,
    inputToISO: inputToISO,
    toast: toast,
    api: api,
    isTransientError: isTransientError,
    isUnconfirmedOutcome: isUnconfirmedOutcome,
    setFormDirty: setFormDirty,
    startVersionWatch: startVersionWatch,
    openModal: openModal,
    closeModal: closeModal,
    showConfirm: showConfirm,
  };
});
AdminApp.initializeModule("core");
