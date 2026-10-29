"use strict";
// Overview owns request plumbing, polling and the page entry points; the sibling overview_* modules own state, helpers and rendering.
AdminApp.register("overview", function (app) {
  var api = app.bind("core", "api");
  var S = app.bind("overviewState", "store")();
  var loadActivity = app.bind("overviewActivity", "loadActivity");
  var renderChain = app.bind("overviewHealth", "renderChain");
  var renderHistory = app.bind("overviewHealth", "renderHistory");
  var renderRecentChanges = app.bind("overviewHealth", "renderRecentChanges");
  var renderTargetHealth = app.bind("overviewHealth", "renderTargetHealth");
  var aggregatePath = app.bind("overviewRange", "aggregatePath");
  var rangeOption = app.bind("overviewRange", "rangeOption");
  var rangeWindow = app.bind("overviewRange", "rangeWindow");
  var storeRange = app.bind("overviewRange", "storeRange");
  var renderRangePanels = app.bind("overviewTrends", "renderRangePanels");
  var renderRangeWindow = app.bind("overviewTrends", "renderRangeWindow");
  var el = app.bind("overviewUtil", "el");
  var isObject = app.bind("overviewUtil", "isObject");
  var text = app.bind("overviewUtil", "text");
  var visible = app.bind("overviewUtil", "visible");
  var rollupBuckets = app.bind("overviewRange", "rollupBuckets");
  var requestTimeoutMs = 12000;
  // Multi-day aggregates scan many hourly cells and get a longer deadline.
  var multiDayTimeoutMs = 45000;
  var baseDelayMs = 5000;
  var maxBackoffMs = 60000;
  // Exponential backoff: 5s, 10s, 20s, 40s, then 60s while a resource keeps failing.
  function backoffDelay(failures) {
    return Math.min(baseDelayMs * Math.pow(2, Math.max(0, failures)), maxBackoffMs);
  }
  function schedule() {
    if (S.timer !== null) clearTimeout(S.timer);
    S.timer = null;
    if (S.enabled && visible()) S.timer = setTimeout(function () {
      S.timer = null;
      loadOverview();
    }, backoffDelay(S.pollFailures));
  }
  // Each page-owned request settles even if the transport ignores cancellation.
  // Abort also releases the real fetch before another poll can start.
  function request(path, controller, timeoutMs) {
    var timeout, onAbort;
    var expired = false;
    return new Promise(function (resolve, reject) {
      onAbort = function () {
        var error = new Error(expired ? "请求超时" : "请求已取消");
        error.name = expired ? "TimeoutError" : "AbortError";
        reject(error);
      };
      controller.signal.addEventListener("abort", onAbort, {once: true});
      timeout = setTimeout(function () { expired = true; controller.abort(); }, timeoutMs || requestTimeoutMs);
      // timeoutMs: 0 turns off the core api default so requestTimeoutMs is the only deadline.
      Promise.resolve(api(path, {signal: controller.signal, timeoutMs: 0})).then(resolve, reject);
    }).finally(function () {
      clearTimeout(timeout);
      controller.signal.removeEventListener("abort", onAbort);
    });
  }
  function cancelRequests() {
    S.generation++;
    S.aggregateGeneration++;
    if (S.timer !== null) clearTimeout(S.timer);
    S.timer = null;
    if (S.activityFreshnessTimer !== null) clearTimeout(S.activityFreshnessTimer);
    S.activityFreshnessTimer = null;
    if (S.activityController) S.activityController.abort();
    if (S.historyController) { S.historyController.abort(); S.historyFetchedAt = 0; }
    if (S.metricsController) S.metricsController.abort();
    if (S.aggregateController) S.aggregateController.abort();
    S.activityController = S.historyController = S.metricsController = S.aggregateController = null;
    S.activityFlight = S.historyFlight = S.metricsFlight = S.aggregateFlight = null;
    // Like activity and history, keep the last metrics and range snapshots on
    // screen; resetting only the cache age forces a refetch on return without
    // flashing "loading" or "unavailable" first.
    S.metricsFetchedAt = 0;
    S.aggregateFetchedAt = 0;
    S.aggregateLoading = false;
    el("refreshOverviewBtn").disabled = false;
    el("page-overview").setAttribute("aria-busy", "false");
    hideTooltip();
  }
  function syncRangeButtons() {
    S.rangeButtons.forEach(function (button) {
      button.setAttribute("aria-pressed", button.dataset.overviewRange === S.selectedRange ? "true" : "false");
    });
  }
  function loadMetrics(force) {
    if (!visible()) return Promise.resolve();
    if (S.metricsFlight) return S.metricsFlight;
    if (!force && S.metricsFetchedAt && Date.now() - S.metricsFetchedAt < 60000) return Promise.resolve();
    if (!force && Date.now() < S.metricsRetryAt) return Promise.resolve();
    var epoch = S.generation;
    var controller = new AbortController();
    S.metricsController = controller;
    var flight = (async function () {
      try {
        var data = await request("/admin/metrics", controller);
        if (epoch !== S.generation || controller.signal.aborted || !visible()) return;
        if (!isObject(data)) throw new Error("指标数据格式无效");
        S.metricsSnapshot = data;
        S.metricsFetchedAt = Date.now();
        S.metricsFailures = 0;
        S.metricsRetryAt = 0;
        renderChain();
      } catch (error) {
        if (epoch !== S.generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        S.metricsSnapshot = null;
        S.metricsFetchedAt = 0;
        S.metricsRetryAt = Date.now() + backoffDelay(S.metricsFailures++);
        renderChain();
      } finally {
        if (epoch === S.generation) { S.metricsFlight = null; S.metricsController = null; }
      }
    })();
    S.metricsFlight = flight;
    return flight;
  }
  function loadAggregate(force) {
    if (!visible()) return Promise.resolve();
    if (S.aggregateFlight) return S.aggregateFlight;
    if (!force && S.aggregateFetchedAt && Date.now() - S.aggregateFetchedAt < 60000) return Promise.resolve();
    if (!force && Date.now() < S.aggregateRetryAt) return Promise.resolve();
    var epoch = S.aggregateGeneration;
    var controller = new AbortController();
    var window = rangeWindow(S.selectedRange, Date.now());
    S.aggregateController = controller;
    S.aggregateLoading = true;
    S.aggregateRequestWindow = window;
    if (!S.aggregateSnapshot) {
      S.aggregateError = "";
      renderRangePanels();
    }
    var flight = (async function () {
      try {
        var data = await request(aggregatePath(window), controller, window.granularity === "day" ? multiDayTimeoutMs : requestTimeoutMs);
        if (epoch !== S.aggregateGeneration || controller.signal.aborted || !visible()) return;
        if (!isObject(data) || !isObject(data.totals)) throw new Error("聚合数据格式无效");
        S.aggregateSnapshot = data;
        S.aggregateSnapshotWindow = window;
        S.aggregateError = "";
        S.aggregateFetchedAt = Date.now();
        S.aggregateFailures = 0;
        S.aggregateRetryAt = 0;
      } catch (error) {
        if (epoch !== S.aggregateGeneration || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        // Keep the last snapshot (and its own window) of this range and mark it stale.
        S.aggregateError = error.name === "TimeoutError" ? "加载超时" : "加载失败";
        S.aggregateFetchedAt = 0;
        S.aggregateRetryAt = Date.now() + backoffDelay(S.aggregateFailures++);
      } finally {
        if (epoch === S.aggregateGeneration) {
          S.aggregateLoading = false;
          S.aggregateFlight = null;
          S.aggregateController = null;
          renderRangePanels();
        }
      }
    })();
    S.aggregateFlight = flight;
    return flight;
  }
  function selectRange(key) {
    if (!rangeOption(key) || key === S.selectedRange) {
      syncRangeButtons();
      return;
    }
    S.selectedRange = key;
    storeRange(key);
    // A new generation makes any in-flight response for the old range inert.
    S.aggregateGeneration++;
    if (S.aggregateController) S.aggregateController.abort();
    S.aggregateFlight = null;
    S.aggregateController = null;
    S.aggregateFetchedAt = 0;
    S.aggregateFailures = 0;
    S.aggregateRetryAt = 0;
    S.aggregateSnapshot = null;
    S.aggregateSnapshotWindow = null;
    S.aggregateRequestWindow = rangeWindow(key, Date.now());
    S.aggregateError = "";
    S.aggregateLoading = true;
    S.trendFocus = null;
    syncRangeButtons();
    renderRangePanels();
    if (visible()) loadAggregate(true);
  }
  function loadHistory(force) {
    if (!visible()) return Promise.resolve();
    if (S.historyFlight) return S.historyFlight;
    if (!force && S.historyFetchedAt && Date.now() - S.historyFetchedAt < 60000) return Promise.resolve();
    if (!force && Date.now() < S.historyRetryAt) return Promise.resolve();
    var epoch = S.generation;
    var controller = new AbortController();
    S.historyController = controller;
    S.historyFetchedAt = Date.now();
    var flight = (async function () {
      try {
        var data = await request("/admin/api/overview", controller, multiDayTimeoutMs);
        if (epoch === S.generation && !controller.signal.aborted && visible()) {
          S.historySnapshot = isObject(data) ? data : {};
          S.historyError = "";
          S.historyFailures = 0;
          S.historyRetryAt = 0;
          renderHistory(S.historySnapshot);
        }
      } catch (error) {
        if (epoch !== S.generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        S.historyFetchedAt = 0;
        S.historyRetryAt = Date.now() + backoffDelay(S.historyFailures++);
        var reason = error.name === "TimeoutError" ? "加载超时" : "加载失败";
        S.historyError = reason;
        text("activityHistoryState", reason);
        text("ovRequestStatsStatus", "历史统计" + reason + (S.historySnapshot ? "，显示上次数据；" : "，") + "请点击刷新重试。");
        if (!S.historySnapshot) {
          ["ovTotalReqs", "ovSuccessRate", "ovTargets", "ovClients", "ovModelCosts"].forEach(function (id) { text(id, "不可用"); });
          el("overviewUsageSummary").innerHTML = '<p class="activity-history-error">历史费用' + reason + '，请点击刷新重试。</p>';
          S.historySnapshot = null;
        }
        renderTargetHealth();
        renderRecentChanges();
        renderChain();
      } finally {
        if (epoch === S.generation) { S.historyFlight = null; S.historyController = null; }
      }
    })();
    S.historyFlight = flight;
    return flight;
  }
  function loadOverview(forceHistory) {
    if (forceHistory === true) S.pollFailures = 0;
    return Promise.all([loadActivity(), loadHistory(forceHistory === true), loadMetrics(forceHistory === true), loadAggregate(forceHistory === true)]);
  }
  function startOverviewAutoRefresh() {
    S.enabled = true;
    S.pollFailures = 0;
    loadOverview();
  }
  function stopOverviewAutoRefresh() {
    S.enabled = false;
    cancelRequests();
  }
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) cancelRequests();
    else if (S.enabled && visible()) loadOverview();
  });
  el("refreshOverviewBtn").addEventListener("click", function () { loadOverview(true); });
  S.rangeButtons = Array.prototype.slice.call(document.querySelectorAll("[data-overview-range]"));
  S.rangeButtons.forEach(function (button) {
    button.addEventListener("click", function () { selectRange(button.dataset.overviewRange); });
  });
  syncRangeButtons();
  renderRangeWindow(rangeWindow(S.selectedRange, Date.now()));
  function hideTooltip() {
    el("activityTooltip").hidden = true;
    S.tooltipTrigger = null;
  }
  function showTooltip(trigger) {
    S.tooltipTrigger = trigger;
    var tooltip = el("activityTooltip");
    tooltip.textContent = trigger.dataset.activityInfoSource ? el(trigger.dataset.activityInfoSource).textContent : trigger.dataset.activityInfo;
    tooltip.hidden = false;
    var bounds = trigger.getBoundingClientRect();
    var viewport = document.documentElement;
    var width = viewport.clientWidth;
    var height = viewport.clientHeight;
    var tip = tooltip.getBoundingClientRect();
    tooltip.style.left = Math.max(12, Math.min(bounds.left, width - tip.width - 12)) + "px";
    tooltip.style.top = (bounds.bottom + tip.height + 12 < height ? bounds.bottom + 8 : Math.max(12, bounds.top - tip.height - 8)) + "px";
  }
  document.querySelectorAll("[data-activity-info]").forEach(function (trigger) {
    trigger.addEventListener("pointerenter", function (event) { if (event.pointerType !== "touch") showTooltip(trigger); });
    trigger.addEventListener("pointerleave", function () { if (document.activeElement !== trigger) hideTooltip(); });
    trigger.addEventListener("focus", function () { showTooltip(trigger); });
    trigger.addEventListener("blur", hideTooltip);
    // Touch focus may fire before click; use pointerdown to remember the toggle.
    var touchWasOpen = false;
    var touchPointer = false;
    trigger.addEventListener("pointerdown", function (event) { touchPointer = event.pointerType === "touch"; if (touchPointer) touchWasOpen = S.tooltipTrigger === trigger; });
    trigger.addEventListener("click", function (event) {
      if (touchPointer ? touchWasOpen : S.tooltipTrigger === trigger) hideTooltip();
      else showTooltip(trigger);
      touchPointer = false;
    });
  });
  document.addEventListener("keydown", function (event) { if (event.key === "Escape") hideTooltip(); });
  document.addEventListener("pointerdown", function (event) { if (S.tooltipTrigger && event.target !== S.tooltipTrigger && !S.tooltipTrigger.contains(event.target)) hideTooltip(); });
  document.addEventListener("scroll", hideTooltip, true);
  return {loadOverview: loadOverview, startOverviewAutoRefresh: startOverviewAutoRefresh, stopOverviewAutoRefresh: stopOverviewAutoRefresh, selectRange: selectRange, rangeWindow: rangeWindow, rollupBuckets: rollupBuckets, aggregatePath: aggregatePath, request: request, schedule: schedule, showTooltip: showTooltip};
});
