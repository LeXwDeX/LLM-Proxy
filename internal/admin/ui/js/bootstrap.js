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
  function boot() {
    if (inFlight) return inFlight;
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
        await withRetry(function () {
          return app.modules.schema.loadSchema();
        }, onRetry);
        app.initialize();
        // The session role (readonly mode) is applied before the page is ready.
        await withRetry(async function () {
          await app.modules.users.loadUserInfo();
        }, onRetry);
        try {
          await withRetry(function () {
            return app.modules.audit_trace.checkTraceEnabled();
          }, onRetry);
        } catch (e) {
          // Trace is optional: an unreachable probe leaves its page hidden.
        }
        app.state.ready = true;
        status.hidden = true;
        app.modules.navigation.loadPageData("overview");
        app.modules.overview.startOverviewAutoRefresh();
        return true;
      } catch (e) {
        app.state.ready = false;
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
