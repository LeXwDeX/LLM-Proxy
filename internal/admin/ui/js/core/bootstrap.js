"use strict";
// Bootstrap is the only startup entry. Schema must succeed before any feature binds events.
(function (app) {
  var inFlight = null;
  // Each startup request gets RETRY_DELAYS_MS.length automatic retries after a
  // timeout or network failure; every attempt is a new api() call (new fetch).
  // Deterministic failures (HTTP errors, schema validation) are not retried.
  var RETRY_DELAYS_MS = [500, 1500];
  function delay(ms) {
    return new Promise(function (resolve) {
      setTimeout(resolve, ms);
    });
  }
  async function withRetry(step, onRetry) {
    for (var attempt = 0; ; attempt++) {
      try {
        return await step();
      } catch (e) {
        if (
          attempt >= RETRY_DELAYS_MS.length ||
          !app.modules.core.isTransientError(e)
        )
          throw e;
        onRetry(e, attempt + 1, RETRY_DELAYS_MS.length);
        await delay(RETRY_DELAYS_MS[attempt]);
      }
    }
  }
  // Modules bootstrap itself calls; every module bound through app.bind is
  // checked as well (AdminApp.missingModules).
  var BOOT_MODULES = ["core", "schema", "navigation", "users", "overview", "audit_trace"];
  function assertModulesLoaded() {
    var missing = app.missingModules(BOOT_MODULES);
    if (missing.length)
      throw new Error(
        "以下模块未加载，对应脚本可能加载失败或存在语法错误: " + missing.join(", "),
      );
  }
  function boot() {
    if (inFlight) return inFlight;
    app.state.ready = false;
    document.body.classList.add("readonly-mode");
    var layout = document.querySelector(".layout");
    if (layout) layout.inert = true;
    ["nav-users", "nav-section-system", "readonlyBadge"].forEach(function (id) {
      var el = document.getElementById(id);
      if (el && el.style.display === "") el.style.display = "none";
    });
    inFlight = (async function () {
      var status = document.getElementById("adminBootStatus");
      var message = document.getElementById("adminBootMessage");
      var retry = document.getElementById("adminBootRetry");
      retry.hidden = true;
      message.textContent = "加载中…";
      function onRetry(error, attempt, max) {
        message.textContent =
          error.message + "，正在重试（" + attempt + "/" + max + "）…";
      }
      try {
        assertModulesLoaded();
        await withRetry(function () {
          return app.modules.schema.loadSchema();
        }, onRetry);
        app.initialize();
        assertModulesLoaded();
        var navigation = app.modules.navigation;
        // Read-only data loads start now; the layout stays inert and the body
        // readonly until /api/me resolves, so nothing editable is reachable.
        if (!navigation.resolveDeepLink())
          app.modules.overview.startOverviewAutoRefresh();
        // Trace is optional and must not delay readiness: an unreachable probe
        // leaves its page hidden. TODO: switch to the lightweight
        // /admin/data/trace/stats probe once that endpoint exists (the probe
        // URL lives in audit_trace.checkTraceEnabled).
        withRetry(
          function () {
            return app.modules.audit_trace.checkTraceEnabled();
          },
          function () {},
        ).then(
          function () {
            navigation.resolveDeepLink();
          },
          function () {},
        );
        var role;
        await withRetry(async function () {
          role = await app.modules.users.loadUserInfo();
        }, onRetry);
        if (role === "readonly") app.modules.users.applyReadonlyMode(document);
        app.state.ready = true;
        if (role === "admin") document.body.classList.remove("readonly-mode");
        if (layout) layout.inert = false;
        status.hidden = true;
        navigation.resolveDeepLink();
        app.modules.core.startVersionWatch();
        return true;
      } catch (e) {
        app.state.ready = false;
        try {
          if (app.modules.overview) app.modules.overview.stopOverviewAutoRefresh();
        } catch (stopError) {
          /* the overview module may be the one that failed */
        }
        app.modules.core.toast("管理初始化失败: " + e.message, "error");
        message.textContent = "加载失败: " + e.message + "。编辑功能尚未启用。";
        status.hidden = false;
        retry.hidden = false;
        return false;
      } finally {
        inFlight = null;
      }
    })();
    app.bootPromise = inFlight;
    return inFlight;
  }
  app.boot = boot;
  document.getElementById("adminBootRetry").addEventListener("click", boot);
  boot();
})(AdminApp);
