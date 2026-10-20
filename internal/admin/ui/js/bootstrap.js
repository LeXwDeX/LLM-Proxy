"use strict";
// Bootstrap is the only startup entry. Schema must succeed before any feature binds events.
(function (app) {
  var inFlight = null;
  function boot() {
    if (inFlight) return inFlight;
    inFlight = (async function () {
      var status = document.getElementById("adminBootStatus");
      var message = document.getElementById("adminBootMessage");
      var retry = document.getElementById("adminBootRetry");
      retry.hidden = true;
      message.textContent = "加载中…";
      try {
        await app.modules.schema.loadSchema();
        app.initialize();
        await app.modules.users.loadUserInfo();
        await app.modules.audit_trace.checkTraceEnabled();
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
