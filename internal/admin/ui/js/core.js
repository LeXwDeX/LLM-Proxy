"use strict";
// Core owns shared state, API transport and modal primitives.
window.AdminApp = {
  modules: {},
  factories: [],
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
    return function () {
      var exports = app.modules[module];
      if (!exports || typeof exports[name] !== "function")
        throw new Error("模块依赖未就绪: " + module + "." + name);
      return exports[name].apply(exports, arguments);
    };
  },
  initializeModule: function (name) {
    if (this.modules[name]) return this.modules[name];
    var entry = this.factories.find(function (candidate) {
      return candidate.name === name;
    });
    if (!entry) throw new Error("模块未注册: " + name);
    var exports = entry.factory(this);
    this.modules[name] = exports;
    return exports;
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
  function namedError(name, message) {
    var error = new Error(message);
    error.name = name;
    return error;
  }
  // Timeouts and fetch network failures (TypeError) are worth a fresh attempt;
  // HTTP errors and validation failures are deterministic and are not.
  function isTransientError(error) {
    return (
      !!error && (error.name === "TimeoutError" || error.name === "TypeError")
    );
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
    )
      return;
    var here =
      (location.pathname || "") + (location.search || "") + (location.hash || "");
    location.assign("/login" + (here ? "?next=" + encodeURIComponent(here) : ""));
  }
  async function api(url, opts) {
    var init = Object.assign({}, opts || {});
    var timeoutMs =
      init.timeoutMs === undefined || init.timeoutMs === null
        ? API_TIMEOUT_MS
        : Number(init.timeoutMs) || 0;
    delete init.timeoutMs;
    var callerSignal = init.signal || null;
    init.headers = Object.assign({}, init.headers || {});
    init.headers["X-Requested-With"] = "XMLHttpRequest";
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
      var resp = await fetch(url, init);
      var text = await resp.text();
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
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (e) {
        data = { raw: text };
      }
    }
    if (resp.status === 401 && data && data.error === "unauthorized")
      redirectToLogin();
    if (!resp.ok)
      throw new Error(data.error || "请求失败 (HTTP " + resp.status + ")");
    return data;
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

  return {
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
    openModal: openModal,
    closeModal: closeModal,
    showConfirm: showConfirm,
  };
});
AdminApp.initializeModule("core");
