"use strict";
// Overview owns the selected usage range, live activity window, client
// selection and page polling.
AdminApp.register("overview", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var fmtPct = app.bind("core", "fmtPct");
  var totalTok = app.bind("core", "totalTok");
  var api = app.bind("core", "api");
  var schemaLabel = null;
  try { schemaLabel = app.bind("schema", "schemaLabel"); } catch (error) { schemaLabel = null; }
  var snapshot = null;
  var selectedClient = null;
  var focusFrom = null;
  var sortKey = "active_requests";
  var sortDirection = -1;
  var rows = new Map();
  var enabled = false;
  var generation = 0;
  var aggregateGeneration = 0;
  var timer = null;
  var activityFlight = null;
  var historyFlight = null;
  var metricsFlight = null;
  var aggregateFlight = null;
  var activityController = null;
  var historyController = null;
  var metricsController = null;
  var aggregateController = null;
  var historyFetchedAt = 0;
  var metricsFetchedAt = 0;
  var aggregateFetchedAt = 0;
  var historySnapshot = null;
  var metricsSnapshot = null;
  var aggregateSnapshot = null;
  var historyError = "";
  var aggregateError = "";
  var aggregateLoading = false;
  // The window that produced aggregateSnapshot; the note, trend axis and slot
  // rollup always describe this window until a newer request succeeds.
  var aggregateSnapshotWindow = null;
  // The window of the latest aggregate request (may differ after local midnight).
  var aggregateRequestWindow = null;
  var rangeButtons = [];
  var barButtons = [];
  var barMetric = "requests";
  var trendSlots = null;
  var trendFocus = null;
  var requestTimeoutMs = 12000;
  var tooltipTrigger = null;
  var metricIDs = ["activityActiveRequests", "activityActiveClients", "activityWebsocketSessions", "activityTotalTokens", "activityInputTokens", "activityOutputTokens"];
  var chartIDs = ["activityConcurrencyChart", "activityTokenChart"];
  var trendIDs = ["ovTokenTrend", "ovBarTrend"];
  // Overview-only presentation keys; they are not Schema business enums.
  var rangeOptions = [
    {key: "today", label: "今日"},
    {key: "yesterday", label: "昨日"},
    {key: "7d", label: "近 7 天"},
    {key: "30d", label: "近 30 天"},
    {key: "last_month", label: "上个自然月"},
  ];
  var rangeStorageKey = "llmproxy.overview.range";
  var selectedRange = readStoredRange();
  var emptyRow = document.createElement("tr");
  var emptyCell = document.createElement("td");
  emptyCell.colSpan = 6;
  emptyRow.appendChild(emptyCell);
  emptyCell.textContent = "正在加载客户端活动…";
  document.getElementById("activityClientsBody").innerHTML = "";
  document.getElementById("activityClientsBody").appendChild(emptyRow);
  function el(id) { return document.getElementById(id); }
  function text(id, value) { el(id).textContent = value; }
  function visible() { return state.currentPage === "overview" && !document.hidden; }
  function clock(value) {
    var date = new Date(value);
    return isNaN(date.getTime()) ? String(value || "—") : date.toLocaleTimeString("zh-CN", {hour12: false});
  }
  function number(value) { return fmtNum(value); }
  function isObject(value) { return !!value && typeof value === "object" && !Array.isArray(value); }
  function hasFields(value) { return isObject(value) && Object.keys(value).length > 0; }
  function counter(value) { return typeof value === "number" && isFinite(value) && value >= 0; }
  function localDate(value) {
    var date = new Date(value);
    if (isNaN(date.getTime()) || date.getUTCFullYear() < 2000) return "—";
    function pad(value) { return String(value).padStart(2, "0"); }
    return date.getFullYear() + "-" + pad(date.getMonth() + 1) + "-" + pad(date.getDate()) + " " + pad(date.getHours()) + ":" + pad(date.getMinutes()) + ":" + pad(date.getSeconds());
  }
  function duration(seconds) {
    if (!counter(seconds)) return "不可用";
    var total = Math.floor(seconds);
    var hours = Math.floor(total / 3600);
    var minutes = Math.floor((total % 3600) / 60);
    if (hours > 0) return hours + " 小时 " + minutes + " 分钟";
    if (minutes > 0) return minutes + " 分钟";
    return total + " 秒";
  }
  function targetPhase(target) {
    if (target && target.Paused === true) return "offline";
    var phase = String(target && target.Phase || "").toLowerCase();
    if (phase === "ready" || phase === "muted" || phase === "half-open" || phase === "offline") return phase;
    if (target && target.Muted === true) return "muted";
    return "unknown";
  }
  function phaseLabel(target) {
    var phase = targetPhase(target);
    if (phase === "unknown") return "未知";
    if (schemaLabel) {
      try {
        var label = schemaLabel("target_phases", phase);
        if (label) return label;
      } catch (error) { /* Schema may be unavailable; a raw phase is still honest. */ }
    }
    return phase;
  }
  function keyIssue(target) {
    if (!counter(target && target.TotalKeys)) return "unknown";
    if (target.TotalKeys <= 1) return "false";
    if (counter(target.ActiveKeys) && target.ActiveKeys < target.TotalKeys) return "true";
    var failures = ["RateLimitedKeys", "BlockedKeys", "ExhaustedSubscriptionKeys", "ExhaustedAPIKeys", "OtherExhaustedKeys"];
    if (failures.some(function (key) { return counter(target[key]) && target[key] > 0; })) return "true";
    var complete = ["ActiveKeys"].concat(failures).every(function (key) { return counter(target[key]); });
    return complete ? "false" : "unknown";
  }
  function keySummary(target) {
    if (!counter(target.TotalKeys) || keyIssue(target) === "unknown") return "密钥状态不可用";
    if (target.TotalKeys === 0) return "未配置独立密钥，可能使用 Bearer/透传";
    if (target.TotalKeys === 1) return "目标级状态；无独立密钥池明细";
    var parts = ["可用 " + (counter(target.ActiveKeys) ? fmtNum(target.ActiveKeys) : "不可用") + " / " + fmtNum(target.TotalKeys)];
    if (counter(target.RateLimitedKeys) && target.RateLimitedKeys > 0) parts.push("限流 " + fmtNum(target.RateLimitedKeys));
    if (counter(target.BlockedKeys) && target.BlockedKeys > 0) parts.push("屏蔽 " + fmtNum(target.BlockedKeys));
    if (counter(target.ExhaustedSubscriptionKeys) && target.ExhaustedSubscriptionKeys > 0) parts.push("订阅额度耗尽 " + fmtNum(target.ExhaustedSubscriptionKeys));
    if (counter(target.ExhaustedAPIKeys) && target.ExhaustedAPIKeys > 0) parts.push("API 额度耗尽 " + fmtNum(target.ExhaustedAPIKeys));
    if (counter(target.OtherExhaustedKeys) && target.OtherExhaustedKeys > 0) parts.push("其他失效 " + fmtNum(target.OtherExhaustedKeys));
    return parts.join(" · ");
  }
  function targetRank(target) {
    var phase = targetPhase(target);
    if (phase === "muted") return 0;
    if (phase === "half-open") return 1;
    if (phase === "offline") return 2;
    if (phase === "ready") {
      var issue = keyIssue(target);
      return issue === "true" ? 3 : issue === "unknown" ? 4 : 5;
    }
    return 6;
  }
  function rangeOption(key) {
    return rangeOptions.find(function (option) { return option.key === key; }) || null;
  }
  // localStorage may be missing or throw (private mode, blocked site data).
  function rangeStorage() {
    try { return typeof localStorage !== "undefined" && localStorage ? localStorage : null; } catch (error) { return null; }
  }
  function readStoredRange() {
    try {
      var storage = rangeStorage();
      var saved = storage ? storage.getItem(rangeStorageKey) : null;
      return rangeOption(saved) ? saved : "today";
    } catch (error) { return "today"; }
  }
  function storeRange(key) {
    try {
      var storage = rangeStorage();
      if (storage) storage.setItem(rangeStorageKey, key);
    } catch (error) { /* Remembering the range is a convenience only. */ }
  }
  // rangeWindow returns browser-local boundaries. The aggregate API truncates
  // from/to to whole UTC hours and includes the end hour, so closed ranges send
  // `to = end - 1ms` and never pick up the next period's first hour.
  function rangeWindow(key, now) {
    var option = rangeOption(key);
    if (!option) return null;
    var current = new Date(now instanceof Date ? now.getTime() : Number(now));
    var year = current.getFullYear(), month = current.getMonth(), day = current.getDate();
    var from, end = null, granularity = "hour";
    if (key === "today") from = new Date(year, month, day);
    else if (key === "yesterday") { from = new Date(year, month, day - 1); end = new Date(year, month, day); }
    else if (key === "7d") { from = new Date(year, month, day - 6); granularity = "day"; }
    else if (key === "30d") { from = new Date(year, month, day - 29); granularity = "day"; }
    else { from = new Date(year, month - 1, 1); end = new Date(year, month, 1); granularity = "day"; }
    return {key: key, label: option.label, from: from, to: end ? new Date(end.getTime() - 1) : current, end: end || current, closed: !!end, granularity: granularity};
  }
  function aggregatePath(window) {
    return "/admin/data/usage/aggregate?group_by=hour&from=" + encodeURIComponent(window.from.toISOString()) + "&to=" + encodeURIComponent(window.to.toISOString());
  }
  function amount(value) { return counter(value) ? value : 0; }
  // Input shown on the overview includes cache read/write and tool input, so
  // input + output equals the total-token convention used by totalTok.
  function inputTotal(data) {
    return amount(data.input_tokens) + amount(data.cached_tokens) + amount(data.cache_creation_tokens) + amount(data.tool_use_prompt_tokens);
  }
  function emptySlot(start, end) {
    return {start: start, end: end, requests: 0, input: 0, output: 0, cost: 0, incompleteUsage: 0, incompleteCost: 0, buckets: 0, empty: true, missing: false};
  }
  // rollupBuckets turns hourly aggregate buckets into display slots: whole UTC
  // hours for single-day ranges, browser-local days for multi-day ranges.
  // Empty slots are created only inside [window.from, window.to], so future
  // hours of today are never drawn as zero. A bucket that starts before the
  // first slot (UTC truncation in half-hour zones) is kept in the first slot.
  function rollupBuckets(buckets, window) {
    var slots = [];
    var fromMs = window.from.getTime(), toMs = window.to.getTime();
    if (window.granularity === "day") {
      for (var i = 0; i < 400; i++) {
        var start = new Date(window.from.getFullYear(), window.from.getMonth(), window.from.getDate() + i);
        if (start.getTime() > toMs) break;
        slots.push(emptySlot(start, new Date(start.getFullYear(), start.getMonth(), start.getDate() + 1)));
      }
    } else {
      var hour = 3600000;
      for (var at = Math.floor(fromMs / hour) * hour; at <= toMs; at += hour) slots.push(emptySlot(new Date(at), new Date(at + hour)));
    }
    if (!slots.length) return slots;
    (Array.isArray(buckets) ? buckets : []).forEach(function (bucket) {
      var started = Date.parse(bucket && bucket.bucket_start);
      var index = 0;
      if (isFinite(started)) {
        var moment = Math.max(started, slots[0].start.getTime());
        for (var j = slots.length - 1; j >= 0; j--) {
          if (slots[j].start.getTime() <= moment) { index = j; break; }
        }
      }
      var slot = slots[index];
      bucket = bucket || {};
      slot.empty = false;
      slot.buckets++;
      if (!isFinite(started) || !["requests", "input_tokens", "output_tokens", "estimated_cost"].every(function (key) { return counter(bucket[key]); })) slot.missing = true;
      slot.requests += amount(bucket.requests);
      slot.input += inputTotal(bucket);
      slot.output += amount(bucket.output_tokens);
      slot.cost += amount(bucket.estimated_cost);
      slot.incompleteUsage += amount(bucket.incomplete_usage_count);
      slot.incompleteCost += amount(bucket.incomplete_cost_count);
    });
    return slots;
  }
  function rangeTotals() {
    return aggregateSnapshot && isObject(aggregateSnapshot.totals) ? aggregateSnapshot.totals : null;
  }
  // True when the shown snapshot covers a different period than the one just
  // requested, e.g. yesterday's "today" after the page stayed open past midnight.
  // Within the same period only `to` advances and `from` stays equal.
  function snapshotOutdated() {
    return !!(aggregateSnapshot && aggregateSnapshotWindow && aggregateRequestWindow &&
      aggregateSnapshotWindow.from.getTime() !== aggregateRequestWindow.from.getTime());
  }
  function aggregateStaleText() {
    var verb = aggregateError === "加载超时" ? "超时" : "失败";
    if (snapshotOutdated()) return "更新" + verb + "，显示 " + windowSpan(aggregateSnapshotWindow) + " 的上次数据";
    return "更新" + verb + "，显示上次数据";
  }
  function rangeComplete(data) {
    return isObject(data) && ["requests", "input_tokens", "output_tokens", "estimated_cost"].every(function (key) { return counter(data[key]); });
  }
  function statsComplete(stats) {
    return isObject(stats) && stats.available === true && ["total", "success", "failure", "canceled", "record_failures"].every(function (key) { return counter(stats[key]); });
  }
  function persistenceComplete(value) {
    return isObject(value) && ["usage_async_pending", "usage_async_deferred", "request_stats_dropped"].every(function (key) { return counter(value[key]); });
  }
  function metricsComplete() {
    if (!isObject(metricsSnapshot) || !counter(metricsSnapshot.uptime_seconds)) return false;
    if (!isObject(metricsSnapshot.requests) || !counter(metricsSnapshot.requests.total)) return false;
    if (!persistenceComplete(metricsSnapshot.persistence)) return false;
    var budget = metricsSnapshot.observation_budget;
    if (!isObject(budget) || !isObject(budget.observation_budget_exhausted)) return false;
    return counter(budget.observation_budget_exhausted.br) && counter(budget.observation_budget_exhausted.zstd);
  }
  function historyStaleText() {
    return "更新" + (historyError === "加载超时" ? "超时" : "失败") + "，显示上次数据";
  }
  function schedule() {
    if (timer !== null) clearTimeout(timer);
    timer = null;
    if (enabled && visible()) timer = setTimeout(function () {
      timer = null;
      loadOverview();
    }, 5000);
  }
  // Each page-owned request settles even if the transport ignores cancellation.
  // Abort also releases the real fetch before another poll can start.
  function request(path, controller) {
    var timeout, onAbort;
    var expired = false;
    return new Promise(function (resolve, reject) {
      onAbort = function () {
        var error = new Error(expired ? "请求超时" : "请求已取消");
        error.name = expired ? "TimeoutError" : "AbortError";
        reject(error);
      };
      controller.signal.addEventListener("abort", onAbort, {once: true});
      timeout = setTimeout(function () { expired = true; controller.abort(); }, requestTimeoutMs);
      // timeoutMs: 0 turns off the core api default so requestTimeoutMs is the only deadline.
      Promise.resolve(api(path, {signal: controller.signal, timeoutMs: 0})).then(resolve, reject);
    }).finally(function () {
      clearTimeout(timeout);
      controller.signal.removeEventListener("abort", onAbort);
    });
  }
  function compareTime(from, to) {
    var milliseconds = Date.parse(from) - Date.parse(to);
    if (milliseconds !== 0) return milliseconds;
    // Go emits RFC3339Nano; Date.parse truncates the remaining six digits.
    function submillisecond(value) {
      var fraction = /\.(\d+)(?:Z|[+-]\d{2}:\d{2})$/.exec(value);
      return fraction ? fraction[1].padEnd(9, "0").slice(3, 9) : "000000";
    }
    var first = submillisecond(from), second = submillisecond(to);
    return first < second ? -1 : first > second ? 1 : 0;
  }
  function cancelRequests() {
    generation++;
    aggregateGeneration++;
    if (timer !== null) clearTimeout(timer);
    timer = null;
    if (activityController) activityController.abort();
    if (historyController) { historyController.abort(); historyFetchedAt = 0; }
    if (metricsController) metricsController.abort();
    if (aggregateController) aggregateController.abort();
    activityController = historyController = metricsController = aggregateController = null;
    activityFlight = historyFlight = metricsFlight = aggregateFlight = null;
    // Like activity and history, keep the last metrics and range snapshots on
    // screen; resetting only the cache age forces a refetch on return without
    // flashing "loading" or "unavailable" first.
    metricsFetchedAt = 0;
    aggregateFetchedAt = 0;
    aggregateLoading = false;
    el("refreshOverviewBtn").disabled = false;
    el("page-overview").setAttribute("aria-busy", "false");
    hideTooltip();
  }
  function reportActivityStatus(error) {
    var status = el("activityStatus");
    if (error) {
      status.textContent = error.name === "TimeoutError"
        ? (snapshot ? "更新超时，显示上次数据；可点击刷新重试。" : "实时活动加载超时，请点击刷新重试。")
        : (snapshot ? "更新失败，显示上次数据；可点击刷新重试。" : "实时活动暂不可用，请点击刷新重试。");
      status.dataset.state = "error";
      return;
    }
    var warnings = [];
    if (Date.now() - Date.parse(snapshot.generated_at) > 15000) warnings.push("数据更新延迟");
    if (!snapshot.runtime.complete) warnings.push("采样不足");
    if (snapshot.series.some(function (bucket) { return bucket.partial; })) warnings.push("部分时间桶不足10秒");
    if (snapshot.totals.incomplete_usage_count > 0) warnings.push(number(snapshot.totals.incomplete_usage_count) + " 条用量不完整");
    status.textContent = warnings.length ? warnings.join(" · ") : (snapshot.runtime.active_requests === 0 && snapshot.totals.total_tokens === 0 ? "当前无在途请求，近5分钟暂无已记录用量。" : "");
    status.dataset.state = warnings.length ? "partial" : "ready";
  }
  function syncRangeButtons() {
    rangeButtons.forEach(function (button) {
      button.setAttribute("aria-pressed", button.dataset.overviewRange === selectedRange ? "true" : "false");
    });
  }
  function pad(value) { return String(value).padStart(2, "0"); }
  function shortDate(date) { return pad(date.getMonth() + 1) + "-" + pad(date.getDate()); }
  function shortTime(date) { return pad(date.getHours()) + ":" + pad(date.getMinutes()); }
  function offsetMinutes(date) { return -date.getTimezoneOffset(); }
  function offsetLabel(date) {
    var minutes = offsetMinutes(date);
    var sign = minutes < 0 ? "-" : "+";
    minutes = Math.abs(minutes);
    return "UTC" + sign + pad(Math.floor(minutes / 60)) + ":" + pad(minutes % 60);
  }
  function windowSpan(window) {
    var year = new Date(Date.now()).getFullYear();
    var withYear = window.from.getFullYear() !== year || window.end.getFullYear() !== year;
    function stamp(date) { return (withYear ? date.getFullYear() + "-" : "") + shortDate(date) + " " + shortTime(date); }
    return stamp(window.from) + " – " + stamp(window.end);
  }
  // frozen: the window describes a retained snapshot after a failed refresh, so an
  // open range ends at the snapshot's end instead of "now".
  function renderRangeWindow(window, staleText, frozen) {
    var note = el("ovRangeWindow");
    if (!window) { note.textContent = "统计区间不可用"; return; }
    var offsets = offsetLabel(window.from);
    if (offsetLabel(window.end) !== offsets) offsets += " / " + offsetLabel(window.end);
    var parts = [windowSpan(window) + (window.closed ? "" : frozen ? "（截至 " + shortTime(window.end) + "）" : "（截至当前）"), "本地时区 " + offsets, window.granularity === "hour" ? "按小时显示" : "小时用量按本地自然日汇总"];
    if (staleText) parts.push(staleText);
    var uneven = offsetMinutes(window.from) % 60 !== 0 || offsetMinutes(window.end) % 60 !== 0;
    note.textContent = parts.join(" · ") + (uneven ? "。当前时区不是整点偏移，后端按 UTC 整点截取起止，边界可能多含不足一小时的相邻用量。" : "");
    note.dataset.state = uneven || staleText ? "partial" : "ready";
  }
  function renderRange() {
    var option = rangeOption(selectedRange);
    // Describe the data on screen: the snapshot's own window until a newer request succeeds.
    var shownWindow = aggregateSnapshotWindow || aggregateRequestWindow || rangeWindow(selectedRange, Date.now());
    var frozen = !!aggregateError && !!aggregateSnapshot && shownWindow === aggregateSnapshotWindow;
    renderRangeWindow(shownWindow, aggregateError && snapshotOutdated() ? aggregateStaleText() : "", frozen);
    text("ovRangeLabel", option.label);
    text("topClientsRange", option.label);
    text("topModelsRange", option.label);
    var data = rangeTotals();
    var complete = rangeComplete(data);
    var requests = data && counter(data.requests) ? data.requests : null;
    var hasTokens = data && counter(data.input_tokens) && counter(data.output_tokens);
    var cost = data && counter(data.estimated_cost) ? data.estimated_cost : null;
    var clients = aggregateSnapshot && Array.isArray(aggregateSnapshot.by_client) ? aggregateSnapshot.by_client.length : null;
    var models = aggregateSnapshot && Array.isArray(aggregateSnapshot.by_model) ? aggregateSnapshot.by_model.length : null;
    text("ovRangeRequests", requests === null ? "不可用" : fmtNum(requests));
    text("ovRangeTotalTokens", hasTokens ? fmtNum(totalTok(data)) : "不可用");
    text("ovRangeInputTokens", hasTokens ? fmtNum(inputTotal(data)) : "不可用");
    text("ovRangeOutputTokens", hasTokens ? fmtNum(data.output_tokens) : "不可用");
    text("ovRangeCost", cost === null ? "不可用" : fmtMoney(cost));
    text("ovRangeClients", clients === null ? "不可用" : fmtNum(clients));
    text("ovRangeModels", models === null ? "不可用" : fmtNum(models));
    var notes = ["费用为估算金额，已含模型组系数（模型组扣费系数），不是厂商官方账单；$0.00 只表示已记录费用为零，不推断为未定价或免费。"];
    var incomplete = false;
    if (data) {
      if (counter(data.incomplete_usage_count) && data.incomplete_usage_count > 0) { incomplete = true; notes.push(fmtNum(data.incomplete_usage_count) + " 条用量不完整。"); }
      if (counter(data.incomplete_cost_count) && data.incomplete_cost_count > 0) { incomplete = true; notes.push(fmtNum(data.incomplete_cost_count) + " 条成本覆盖不完整。"); }
    }
    var status = el("ovRangeStatus");
    if (!aggregateSnapshot && aggregateError) {
      status.textContent = option.label + "用量不可用：" + aggregateError + "，请点击刷新重试。";
      status.dataset.state = "error";
    } else if (!aggregateSnapshot && aggregateLoading) {
      status.textContent = "正在加载" + option.label + "用量…";
      status.dataset.state = "";
    } else if (!aggregateSnapshot) {
      status.textContent = option.label + "用量不可用。";
      status.dataset.state = "error";
    } else {
      status.textContent = (complete ? "" : "部分字段不可用。") + notes.join(" ");
      status.dataset.state = complete && !incomplete && !aggregateError ? "ready" : "partial";
      if (aggregateError) status.textContent = "区间用量" + aggregateStaleText() + "。" + status.textContent;
    }
  }
  function slotLabel(slot) {
    return trendGranularity() === "hour" ? shortTime(slot.start) : shortDate(slot.start);
  }
  function trendGranularity() {
    return aggregateSnapshotWindow ? aggregateSnapshotWindow.granularity : (rangeWindow(selectedRange, Date.now()) || {}).granularity;
  }
  function slotReadout(slot) {
    var span = trendGranularity() === "hour" ? shortDate(slot.start) + " " + shortTime(slot.start) + "–" + shortTime(slot.end) : shortDate(slot.start);
    var parts = [span, "请求 " + fmtNum(slot.requests), "输入 " + fmtNum(slot.input), "输出 " + fmtNum(slot.output), "估算费用 " + fmtMoney(slot.cost)];
    if (slot.incompleteUsage > 0) parts.push("用量不完整");
    if (slot.incompleteCost > 0) parts.push("成本覆盖不完整");
    if (slot.missing) parts.push("部分字段不可用");
    return parts.join(" · ");
  }
  function moneyAxis(value) {
    if (!(value > 0)) return {maximum: 1, step: 0.25, ticks: 4};
    var desired = value * 1.1 / 4;
    var scale = Math.pow(10, Math.floor(Math.log10(desired)));
    var step = [1, 2, 2.5, 5, 10].find(function (factor) { return factor * scale >= desired; }) * scale;
    var ticks = Math.ceil(value * 1.1 / step);
    return {maximum: ticks * step, step: step, ticks: ticks};
  }
  function moneyLabel(value) {
    if (value >= 1000) return "$" + compactNumber(value);
    return "$" + Number(value.toPrecision(3));
  }
  function trendHTML(chart, slots, token) {
    var geometry = chartGeometry(chart);
    chart.setAttribute("viewBox", "0 0 " + geometry.width + " " + geometry.height);
    var left = geometry.left, right = geometry.right, top = geometry.top, bottom = geometry.bottom;
    var band = (right - left) / slots.length;
    function x(index) { return left + band * (index + 0.5); }
    var cost = !token && barMetric === "cost";
    var keys = token ? ["input", "output"] : [cost ? "cost" : "requests"];
    var peak = Math.max.apply(null, [0].concat(slots.flatMap(function (slot) { return keys.map(function (key) { return slot[key]; }); })));
    var axis = cost ? moneyAxis(peak) : chartAxis(peak);
    function y(value) { return bottom - value / axis.maximum * (bottom - top); }
    var focus = slots.findIndex(function (slot) { return slot.start.getTime() === trendFocus; });
    var svg = "";
    if (!token && focus >= 0) svg += '<rect class="overview-trend-focus" x="' + (x(focus) - band / 2) + '" y="' + top + '" width="' + band + '" height="' + (bottom - top) + '"/>';
    for (var i = 0; i <= axis.ticks; i++) {
      var value = axis.step * i;
      svg += '<line class="activity-gridline" x1="' + left + '" x2="' + right + '" y1="' + y(value) + '" y2="' + y(value) + '"/><text class="activity-axis" x="34" y="' + (y(value) + 4) + '" text-anchor="end">' + esc(cost ? moneyLabel(value) : compactNumber(value)) + '</text>';
    }
    var labels = [0, Math.floor((slots.length - 1) / 2), slots.length - 1].filter(function (value, index, list) { return list.indexOf(value) === index; });
    labels.forEach(function (index) {
      var anchor = slots.length === 1 ? "middle" : index === 0 ? "start" : index === slots.length - 1 ? "end" : "middle";
      var position = anchor === "start" ? left : anchor === "end" ? right : x(index);
      svg += '<text class="activity-axis" x="' + position + '" y="' + (geometry.height - 12) + '" text-anchor="' + anchor + '">' + esc(slotLabel(slots[index])) + '</text>';
    });
    if (token) {
      keys.forEach(function (key, keyIndex) {
        var lineClass = keyIndex ? "activity-output" : "activity-input";
        if (slots.length === 1) svg += '<circle class="' + lineClass + '" cx="' + x(0) + '" cy="' + y(slots[0][key]) + '" r="2.5"/>';
        else svg += '<polyline class="activity-line ' + lineClass + '" points="' + slots.map(function (slot, index) { return x(index) + "," + y(slot[key]); }).join(" ") + '"/>';
      });
      if (focus >= 0) {
        svg += '<line class="activity-cursor" x1="' + x(focus) + '" x2="' + x(focus) + '" y1="' + top + '" y2="' + bottom + '"/>';
        keys.forEach(function (key, keyIndex) {
          svg += '<circle class="activity-point ' + (keyIndex ? "activity-output" : "activity-input") + '" cx="' + x(focus) + '" cy="' + y(slots[focus][key]) + '" r="4"/>';
        });
      }
    } else {
      var width = Math.max(1.5, Math.min(28, band * 0.62));
      slots.forEach(function (slot, index) {
        var value = slot[keys[0]];
        if (!(value > 0)) return;
        svg += '<rect class="overview-bar overview-bar-' + (cost ? "cost" : "requests") + '" x="' + (x(index) - width / 2) + '" y="' + y(value) + '" width="' + width + '" height="' + (bottom - y(value)) + '" rx="2"/>';
      });
    }
    return svg;
  }
  function syncBarButtons() {
    barButtons.forEach(function (button) {
      button.setAttribute("aria-pressed", button.dataset.overviewBar === barMetric ? "true" : "false");
    });
    text("ovBarLegend", barMetric === "cost" ? "估算费用（含模型组系数）" : "请求数");
    text("ovBarUnit", barMetric === "cost" ? "美元" : "个请求");
    el("ovBarLegendSwatch").setAttribute("data-metric", barMetric);
  }
  function trendLabel(token) {
    var option = rangeOption(selectedRange);
    var name = token ? "Token 趋势（输入与输出）" : (barMetric === "cost" ? "估算费用趋势" : "请求数趋势");
    return option.label + "，" + name;
  }
  // drawTrends redraws from cached slots; hover and keyboard moves never refetch.
  function drawTrends() {
    syncBarButtons();
    var slots = trendSlots;
    var readout = "";
    if (!slots) {
      readout = aggregateLoading && !aggregateSnapshot ? "正在加载趋势…" : "区间趋势不可用";
      trendIDs.forEach(function (id, index) {
        el(id).innerHTML = "";
        el(id).setAttribute("aria-label", trendLabel(index === 0) + "，" + (aggregateLoading && !aggregateSnapshot ? "正在加载" : "不可用"));
      });
      text("ovTokenTrendReadout", readout);
      text("ovBarTrendReadout", readout);
      return;
    }
    if (trendFocus !== null && !slots.some(function (slot) { return slot.start.getTime() === trendFocus; })) trendFocus = null;
    var unit = trendGranularity() === "hour" ? "按小时" : "按本地自然日";
    trendIDs.forEach(function (id, index) {
      el(id).innerHTML = slots.length ? trendHTML(el(id), slots, index === 0) : "";
      el(id).setAttribute("aria-label", trendLabel(index === 0) + "，" + unit + "，共 " + slots.length + " 个时间桶；方向键查看读数");
    });
    var focused = slots.find(function (slot) { return slot.start.getTime() === trendFocus; });
    var empty = slots.every(function (slot) { return slot.empty; });
    readout = focused ? slotReadout(focused) : (empty ? "该区间暂无已记录用量" : "移入图表或聚焦后使用方向键查看读数");
    text("ovTokenTrendReadout", readout);
    text("ovBarTrendReadout", readout);
  }
  function renderTrends() {
    var buckets = aggregateSnapshot ? aggregateSnapshot.buckets : undefined;
    // Go encodes an empty bucket slice as null; a missing key is unavailable.
    if (buckets === null && Object.prototype.hasOwnProperty.call(aggregateSnapshot, "buckets")) buckets = [];
    trendSlots = Array.isArray(buckets) && aggregateSnapshotWindow ? rollupBuckets(buckets, aggregateSnapshotWindow) : null;
    text("ovTrendUnit", trendGranularity() === "hour" ? "按小时" : "按天（本地自然日）");
    var status = el("ovTrendStatus");
    if (!trendSlots) {
      if (aggregateLoading && !aggregateSnapshot && !aggregateError) {
        status.textContent = "正在加载区间趋势…";
        status.dataset.state = "";
      } else {
        status.textContent = "区间趋势不可用：" + (aggregateError ? aggregateError : aggregateSnapshot ? "聚合响应缺少时间桶" : "尚未加载聚合") + "。";
        status.dataset.state = "error";
      }
    } else {
      var empty = trendSlots.every(function (slot) { return slot.empty; });
      var notes = [];
      var usageGaps = trendSlots.filter(function (slot) { return slot.incompleteUsage > 0; }).length;
      var missing = trendSlots.filter(function (slot) { return slot.missing; }).length;
      if (usageGaps) notes.push(fmtNum(usageGaps) + " 个时间桶含不完整用量");
      if (missing) notes.push(fmtNum(missing) + " 个时间桶部分字段缺失");
      status.textContent = (empty ? "该区间暂无已记录用量，时间桶按 0 显示。" : "共 " + fmtNum(trendSlots.length) + " 个时间桶；没有用量的时间桶按 0 显示。") + (notes.length ? notes.join("，") + "。" : "");
      status.dataset.state = notes.length || aggregateError ? "partial" : "ready";
      if (aggregateError) status.textContent = "区间趋势" + aggregateStaleText() + "。" + status.textContent;
    }
    drawTrends();
  }
  function renderRangePanels() {
    renderRange();
    renderTrends();
    renderTop();
  }
  function targetRow(target) {
    var phase = targetPhase(target);
    var issue = keyIssue(target);
    var status = '<span class="overview-phase phase-' + esc(phase) + '">' + esc(phaseLabel(target)) + '</span>';
    if (phase === "muted" && localDate(target.MutedUntil) !== "—") status += ' <span class="overview-status">冷却至 ' + esc(localDate(target.MutedUntil)) + '</span>';
    if (phase === "offline" && target.Paused === true) status += ' <span class="overview-status">已暂停</span>';
    var capacity = counter(target.Capacity) ? (target.Capacity === 0 ? "不限" : fmtNum(target.Capacity)) : "不可用";
    var inflight = counter(target.InFlight) ? fmtNum(target.InFlight) : "不可用";
    var window = isObject(target.Window15m) ? "峰值 " + (counter(target.Window15m.in_flight_peak) ? fmtNum(target.Window15m.in_flight_peak) : "不可用") + " · 熔断 " + (counter(target.Window15m.breaker_trips) ? fmtNum(target.Window15m.breaker_trips) : "不可用") : "不可用";
    return '<tr data-target-name="' + esc(String(target.Name || "")) + '" data-target-phase="' + esc(phase) + '" data-target-key-issue="' + issue + '"><td>' + esc(String(target.Name || "未知目标")) + '</td><td>' + status + '</td><td>' + esc(keySummary(target)) + '</td><td>' + esc(inflight + " / " + capacity) + '</td><td>' + esc(window) + '</td></tr>';
  }
  function renderTargetHealth() {
    var body = el("targetHealthBody");
    var count = el("targetHealthCount");
    var status = el("targetHealthStatus");
    var targets = historySnapshot && Array.isArray(historySnapshot.targets) ? historySnapshot.targets.slice() : null;
    if (!targets) {
      body.innerHTML = '<tr><td colspan="5">目标数据不可用</td></tr>';
      count.textContent = "不可用";
      status.textContent = historyError ? "目标数据" + historyStaleText() + "，目标数据不可用" : "目标数据不可用";
      status.dataset.state = "error";
      return;
    }
    targets = targets.map(function (target, index) { return {target: target || {}, index: index}; }).sort(function (a, b) {
      var difference = targetRank(a.target) - targetRank(b.target);
      return difference || a.index - b.index;
    });
    body.innerHTML = targets.length ? targets.map(function (entry) { return targetRow(entry.target); }).join("") : '<tr><td colspan="5">暂无已配置目标</td></tr>';
    count.textContent = fmtNum(historySnapshot.targets.length) + " 个目标";
    var attention = targets.filter(function (entry) { return targetRank(entry.target) <= 4; }).length;
    var unknown = targets.filter(function (entry) { return targetPhase(entry.target) === "unknown"; }).length;
    if (!targets.length) status.textContent = "暂无目标";
    else if (attention) status.textContent = fmtNum(attention) + " 个目标需要关注" + (unknown ? "，另有 " + fmtNum(unknown) + " 个目标状态未知" : "");
    else if (unknown) status.textContent = fmtNum(unknown) + " 个目标状态未知";
    else status.textContent = "目标均在正常状态";
    if (historyError) {
      status.textContent += "；目标数据" + historyStaleText();
      status.dataset.state = "partial";
    } else {
      status.dataset.state = attention || unknown ? "partial" : "ready";
    }
  }
  function topRows(list) {
    return list.slice().sort(function (a, b) {
      var ac = counter(a && a.estimated_cost) ? a.estimated_cost : -Infinity;
      var bc = counter(b && b.estimated_cost) ? b.estimated_cost : -Infinity;
      if (ac !== bc) return bc - ac;
      var ar = counter(a && a.requests) ? a.requests : -Infinity;
      var br = counter(b && b.requests) ? b.requests : -Infinity;
      if (ar !== br) return br - ar;
      return String(a && a.key || "").localeCompare(String(b && b.key || ""), "zh-CN");
    }).slice(0, 10);
  }
  function costShare(entry, total) {
    if (!counter(entry && entry.estimated_cost) || !counter(total)) return "不可用";
    if (total <= 0) return "—";
    return fmtPct(Math.round(entry.estimated_cost / total * 1000) / 10);
  }
  function topRow(entry, attribute, total) {
    var name = entry && entry.key ? String(entry.key) : "未知";
    var requests = entry && counter(entry.requests) ? fmtNum(entry.requests) : "不可用";
    var tokens = entry && counter(entry.input_tokens) && counter(entry.output_tokens) ? fmtNum(totalTok(entry)) : "不可用";
    var cost = entry && counter(entry.estimated_cost) ? fmtMoney(entry.estimated_cost) : "不可用";
    return '<tr data-' + attribute + '="' + esc(name) + '"><td>' + esc(name) + '</td><td>' + requests + '</td><td>' + tokens + '</td><td>' + cost + '</td><td>' + esc(costShare(entry, total)) + '</td></tr>';
  }
  var topCaveat = "按估算费用、再按请求数排序；费用为估算金额，已含模型组系数（模型组扣费系数），占比按区间总估算费用计算；$0.00 不推断为未定价或免费。";
  function renderTopPanel(bodyID, statusID, list, kind, attribute) {
    var body = el(bodyID);
    var status = el(statusID);
    if (!aggregateSnapshot) {
      var loading = aggregateLoading && !aggregateError;
      body.innerHTML = '<tr><td colspan="5">' + (loading ? "正在加载费用排行…" : "费用排行不可用") + '</td></tr>';
      status.textContent = loading ? "正在加载费用排行…" : "费用排行不可用：" + (aggregateError || "尚未加载聚合") + "。";
      status.dataset.state = loading ? "" : "error";
      return;
    }
    var stale = aggregateError ? "费用排行" + aggregateStaleText() + "；" : "";
    if (!Array.isArray(list)) {
      body.innerHTML = '<tr><td colspan="5">费用排行不可用</td></tr>';
      status.textContent = stale + "费用排行不可用：聚合响应缺少" + kind + "维度。";
      status.dataset.state = "error";
      return;
    }
    if (!list.length) {
      body.innerHTML = '<tr><td colspan="5">该区间暂无可聚合的' + esc(kind) + '用量</td></tr>';
      status.textContent = stale + "该区间暂无可聚合的" + kind + "用量。";
      status.dataset.state = stale ? "partial" : "ready";
      return;
    }
    var totals = rangeTotals();
    body.innerHTML = topRows(list).map(function (entry) { return topRow(entry, attribute, totals ? totals.estimated_cost : undefined); }).join("");
    status.textContent = stale + "共 " + fmtNum(list.length) + " 个" + kind + (list.length > 10 ? "，显示前 10" : "") + "。" + topCaveat;
    status.dataset.state = stale ? "partial" : "ready";
  }
  function renderTop() {
    var data = aggregateSnapshot || {};
    renderTopPanel("topClientsBody", "topClientsStatus", data.by_client, "客户端", "top-client");
    renderTopPanel("topModelsBody", "topModelsStatus", data.by_model, "模型", "top-model");
  }
  function metricsUsable() {
    return isObject(metricsSnapshot);
  }
  function renderChain() {
    var ov = historySnapshot;
    var targets = ov && Array.isArray(ov.targets) ? ov.targets : null;
    var clientCount = ov && counter(ov.client_count) ? ov.client_count : null;
    var clientText = clientCount === null ? "配置客户端不可用" : fmtNum(clientCount) + " 个配置客户端";
    if (snapshot && counter(snapshot.runtime.active_clients)) clientText += " · 活跃 " + fmtNum(snapshot.runtime.active_clients);
    text("chainClients", clientText);
    var proxyParts = [];
    var metrics = metricsUsable() ? metricsSnapshot : null;
    if (metrics) {
      proxyParts.push(isObject(metrics.requests) && counter(metrics.requests.total) ? "累计 " + fmtNum(metrics.requests.total) : "累计请求不可用");
      proxyParts.push(counter(metrics.uptime_seconds) ? "运行 " + duration(metrics.uptime_seconds) : "运行时长不可用");
    } else {
      proxyParts.push("运行指标不可用");
    }
    text("chainProxy", proxyParts.join(" · "));
    if (targets) {
      if (!targets.length) {
        text("chainTargets", "暂无目标");
      } else {
        var healthy = targets.filter(function (target) { return targetPhase(target) === "ready" && keyIssue(target) === "false"; }).length;
        text("chainTargets", fmtNum(healthy) + " / " + fmtNum(targets.length));
      }
    } else {
      text("chainTargets", "不可用");
    }
    var keys = el("chainKeys");
    var keysAvailable = !!targets;
    var keysDetail = el("chainKeysDetail");
    keysDetail.textContent = "可用 / 总数";
    if (targets && !targets.length) {
      keys.textContent = "暂无目标";
      keys.setAttribute("data-active", "0");
      keys.setAttribute("data-total", "0");
      keys.setAttribute("data-rate-limited", "0");
    } else if (targets) {
      var total = 0, active = 0, rate = 0, complete = true;
      targets.forEach(function (target) {
        target = target || {};
        if (!["TotalKeys", "ActiveKeys", "RateLimitedKeys"].every(function (key) { return counter(target[key]); })) { complete = false; return; }
        total += target.TotalKeys;
        active += target.ActiveKeys;
        rate += target.RateLimitedKeys;
      });
      if (complete) {
        keys.textContent = total === 0 ? "未配置独立密钥" : fmtNum(active) + " / " + fmtNum(total);
        keysDetail.textContent = total === 0 ? "可能使用 Bearer/透传" : "可用 / 总数" + (rate > 0 ? " · 限流 " + fmtNum(rate) : "");
        keys.setAttribute("data-active", String(active));
        keys.setAttribute("data-total", String(total));
        keys.setAttribute("data-rate-limited", String(rate));
      } else {
        keysAvailable = false;
        keys.textContent = "不可用";
        keysDetail.textContent = "部分目标缺少密钥计数";
        keys.setAttribute("data-active", "unavailable");
        keys.setAttribute("data-total", "unavailable");
        keys.setAttribute("data-rate-limited", "unavailable");
      }
    } else {
      keys.textContent = "不可用";
      keysDetail.textContent = "目标数据不可用";
      keys.setAttribute("data-active", "unavailable");
      keys.setAttribute("data-total", "unavailable");
      keys.setAttribute("data-rate-limited", "unavailable");
    }
    var stats = ov && isObject(ov.request_stats_72h) ? ov.request_stats_72h : null;
    if (stats && stats.available === true) {
      text("ovLiveSuccessRate", stats.success_rate == null ? "--" : fmtPct(stats.success_rate));
      text("ovLiveSuccessDetail", counter(stats.total) ? (stats.total === 0 ? "暂无完成样本" : fmtNum(stats.total) + " 个完成样本") : "样本总数不可用");
    } else {
      text("ovLiveSuccessRate", "不可用");
      text("ovLiveSuccessDetail", "请求完成统计不可用");
    }
    var available = [!!targets, !!snapshot, metricsComplete(), clientCount !== null, keysAvailable].filter(Boolean).length;
    var chain = el("chainSummary");
    var status = el("chainStatus");
    if (historyError) {
      chain.dataset.state = historySnapshot ? "partial" : "error";
      status.textContent = historySnapshot ? "实时状态" + historyStaleText() : "实时状态" + historyStaleText() + "，目标与客户端数据不可用";
    } else if (available === 5) {
      chain.dataset.state = "ready";
      status.textContent = "实时状态已加载";
    } else if (available === 0) {
      chain.dataset.state = "error";
      status.textContent = "实时状态暂不可用";
    } else {
      chain.dataset.state = "partial";
      status.textContent = "部分实时状态不可用";
    }
  }
  function attentionItem(kind, text) {
    return '<li data-attention-kind="' + kind + '">' + esc(text) + '</li>';
  }
  function renderAttention() {
    var items = [];
    var ov = historySnapshot;
    var targets = ov && Array.isArray(ov.targets) ? ov.targets : null;
    var persistence = isObject(metricsSnapshot && metricsSnapshot.persistence) ? metricsSnapshot.persistence : null;
    if (targets) {
      targets.forEach(function (target) {
        target = target || {};
        var name = String(target.Name || "未知目标");
        var phase = targetPhase(target);
        if (phase === "muted") items.push(["target", "目标 " + name + " 冷却至 " + localDate(target.MutedUntil)]);
        else if (phase === "half-open") items.push(["target", "目标 " + name + " 正在恢复探测"]);
        else if (phase === "offline") items.push(["target", "目标 " + name + " 已暂停，不接收流量"]);
        else if (phase === "unknown") items.push(["target", "目标 " + name + " 状态未知，无法确认可用性"]);
        else if (keyIssue(target) === "true") {
          var total = counter(target.TotalKeys) ? target.TotalKeys : 0;
          var active = counter(target.ActiveKeys) ? target.ActiveKeys : 0;
          var details = [];
          if (counter(target.RateLimitedKeys) && target.RateLimitedKeys > 0) details.push("限流 " + fmtNum(target.RateLimitedKeys));
          if (counter(target.BlockedKeys) && target.BlockedKeys > 0) details.push("屏蔽 " + fmtNum(target.BlockedKeys));
          if (counter(target.ExhaustedSubscriptionKeys) && target.ExhaustedSubscriptionKeys > 0) details.push("订阅额度耗尽 " + fmtNum(target.ExhaustedSubscriptionKeys));
          if (counter(target.ExhaustedAPIKeys) && target.ExhaustedAPIKeys > 0) details.push("API 额度耗尽 " + fmtNum(target.ExhaustedAPIKeys));
          if (counter(target.OtherExhaustedKeys) && target.OtherExhaustedKeys > 0) details.push("其他失效 " + fmtNum(target.OtherExhaustedKeys));
          var unavailable = Math.max(0, total - active);
          items.push(["keys", "目标 " + name + (unavailable > 0 ? " 有 " + fmtNum(unavailable) + " 个密钥不可用" : " 的密钥状态异常") + (details.length ? "（" + details.join("，") + "）" : "")]);
        } else if (keyIssue(target) === "unknown" && counter(target.TotalKeys) && target.TotalKeys > 1) {
          items.push(["keys", "目标 " + name + " 的密钥状态未知，无法确认可用性"]);
        }
      });
    }
    if (persistence) {
      var pending = counter(persistence.usage_async_pending) ? persistence.usage_async_pending : 0;
      var deferred = counter(persistence.usage_async_deferred) ? persistence.usage_async_deferred : 0;
      var dropped = counter(persistence.request_stats_dropped) ? persistence.request_stats_dropped : 0;
      if (pending > 0) items.push(["persistence", "用量写入积压 " + fmtNum(pending) + " 条"]);
      if (deferred > 0) items.push(["persistence", "用量等待后台重试 " + fmtNum(deferred) + " 条"]);
      if (dropped > 0) items.push(["persistence", "请求完成统计队列丢弃 " + fmtNum(dropped) + " 条"]);
    }
    var budget = metricsUsable() && isObject(metricsSnapshot.observation_budget) ? metricsSnapshot.observation_budget : null;
    if (budget && isObject(budget.observation_budget_exhausted)) {
      var br = counter(budget.observation_budget_exhausted.br) ? budget.observation_budget_exhausted.br : 0;
      var zstd = counter(budget.observation_budget_exhausted.zstd) ? budget.observation_budget_exhausted.zstd : 0;
      if (br + zstd > 0) items.push(["persistence", "压缩响应观察预算拒绝 " + fmtNum(br + zstd) + " 次（br " + fmtNum(br) + " / zstd " + fmtNum(zstd) + "）"]);
    }
    // Process-lifetime request persistence counters: never present them as a
    // success rate or as a selected-range figure.
    var requests = ov && isObject(ov.requests) ? ov.requests : null;
    if (requests) {
      [
        ["usage_record_failures", "用量写入失败"],
        ["usage_client_gone_no_usage", "客户端断开且未取得用量"],
        ["usage_upstream_interrupted_no_usage", "上游中断且未取得用量"],
        ["request_stats_record_failures", "请求完成统计写入失败"],
      ].forEach(function (entry) {
        var count = requests[entry[0]];
        if (counter(count) && count > 0) items.push(["persistence", "自启动累计 " + fmtNum(count) + " 次" + entry[1]]);
      });
    }
    var stats = ov && isObject(ov.request_stats_72h) ? ov.request_stats_72h : null;
    if (stats && stats.available === true) {
      if (counter(stats.failure) && stats.failure > 0) items.push(["request-stats", "72 小时完成样本中 " + fmtNum(stats.failure) + " 个失败"]);
      if (counter(stats.canceled) && stats.canceled > 0) items.push(["request-stats", "72 小时完成样本中 " + fmtNum(stats.canceled) + " 个取消"]);
    }
    var usage = rangeTotals();
    if (usage) {
      // A retained snapshot may cover another period (e.g. yesterday after midnight); name it.
      var usageStale = aggregateError && aggregateSnapshotWindow ? "（更新" + (aggregateError === "加载超时" ? "超时" : "失败") + "，基于 " + windowSpan(aggregateSnapshotWindow) + " 的上次数据）" : "";
      if (counter(usage.incomplete_usage_count) && usage.incomplete_usage_count > 0) items.push(["usage", "当前区间有 " + fmtNum(usage.incomplete_usage_count) + " 条用量记录不完整" + usageStale]);
      if (counter(usage.incomplete_cost_count) && usage.incomplete_cost_count > 0) items.push(["usage", "当前区间有 " + fmtNum(usage.incomplete_cost_count) + " 条成本覆盖不完整" + usageStale]);
    }
    var list = el("overviewAttentionList");
    var status = el("overviewAttentionStatus");
    var known = !!(targets || persistence || usage || requests || stats);
    var keysKnown = !!targets && targets.every(function (target) { return keyIssue(target) !== "unknown"; });
    var phasesKnown = !!targets && targets.every(function (target) { return targetPhase(target) !== "unknown"; });
    var ready = !historyError && !aggregateError && !!targets && keysKnown && phasesKnown && metricsComplete() && statsComplete(stats) && rangeComplete(usage);
    list.innerHTML = items.length ? items.map(function (item) { return attentionItem(item[0], item[1]); }).join("") : '<li class="overview-empty">' + (known ? "暂无需要关注的问题" : "关注数据不可用") + '</li>';
    text("overviewAttentionCount", fmtNum(items.length) + " 项");
    if (!known) {
      status.textContent = "关注数据不可用";
      status.dataset.state = "error";
    } else if (!ready && historyError && historySnapshot) {
      status.textContent = historyStaleText() + (items.length ? "；部分数据不可用，以下为已确认问题" : "；部分数据不可用");
      status.dataset.state = "partial";
    } else if (items.length) {
      status.textContent = ready ? "已检查请求统计、当前区间用量、运行指标与密钥计数" : "部分数据不可用，以下为已确认问题";
      status.dataset.state = ready ? "ready" : "partial";
    } else {
      status.textContent = ready ? "关注数据已加载" : "部分数据不可用";
      status.dataset.state = ready ? "ready" : "partial";
    }
  }
  function renderRecentChanges() {
    var body = el("recentChangesBody");
    var status = el("recentChangesStatus");
    var events = historySnapshot && Array.isArray(historySnapshot.recent_audit) ? historySnapshot.recent_audit.slice() : null;
    if (!events) {
      body.innerHTML = '<tr><td colspan="5">最近变更不可用</td></tr>';
      status.textContent = historyError ? "最近变更" + historyStaleText() + "，最近变更不可用" : "最近变更不可用";
      status.dataset.state = "error";
      return;
    }
    if (!events.length) {
      body.innerHTML = '<tr><td colspan="5">暂无可显示的最近变更</td></tr>';
      status.textContent = "暂无变更" + (historyError ? "；最近变更" + historyStaleText() : "");
      status.dataset.state = historyError ? "partial" : "ready";
      return;
    }
    events.sort(function (a, b) {
      var at = Date.parse(a && a.timestamp), bt = Date.parse(b && b.timestamp);
      at = isFinite(at) ? at : -Infinity;
      bt = isFinite(bt) ? bt : -Infinity;
      return bt - at;
    });
    events = events.slice(0, 5);
    body.innerHTML = events.map(function (event, index) {
      var id = event && event.id ? String(event.id) : String(event && event.timestamp || index);
      return '<tr data-audit-id="' + esc(id) + '"><td><time datetime="' + esc(event && event.timestamp || "") + '">' + esc(localDate(event && event.timestamp)) + '</time></td><td>' + esc(event && event.actor || "—") + '</td><td>' + esc(event && event.action || "—") + '</td><td>' + esc(event && event.object || "—") + '</td><td>' + esc(event && event.result || "—") + '</td></tr>';
    }).join("");
    status.textContent = "最近 " + fmtNum(events.length) + " 条变更" + (historyError ? "；最近变更" + historyStaleText() : "");
    status.dataset.state = historyError ? "partial" : "ready";
  }
  function renderSnapshot(data) {
    snapshot = data;
    text("activityActiveRequests", number(data.runtime.active_requests));
    text("activityActiveClients", number(data.runtime.active_clients));
    text("activityWebsocketSessions", number(data.runtime.websocket_sessions));
    text("activityTotalTokens", number(data.totals.total_tokens));
    text("activityInputTokens", number(data.totals.input_tokens));
    text("activityOutputTokens", number(data.totals.output_tokens));
    text("activityUpdatedAt", "更新于 " + clock(data.generated_at));
    el("activityUpdatedAt").setAttribute("datetime", data.generated_at);
    reportActivityStatus();
    renderClients();
    renderCharts();
    renderChain();
    renderAttention();
  }
  function validSnapshot(data) {
    function timestamp(value) { return typeof value === "string" && Number.isFinite(Date.parse(value)); }
    function counter(value) { return Number.isSafeInteger(value) && value >= 0; }
    function totals(value) { return value && ["input_tokens", "output_tokens", "total_tokens", "incomplete_usage_count"].every(function (key) { return counter(value[key]); }); }
    function series(value) {
      return Array.isArray(value) && value.every(function (bucket) {
        return bucket && timestamp(bucket.from) && timestamp(bucket.to) && compareTime(bucket.from, bucket.to) < 0 && typeof bucket.partial === "boolean" && (bucket.active_requests_peak === null || counter(bucket.active_requests_peak)) && totals(bucket);
      });
    }
    if (!data || !timestamp(data.generated_at) || !data.window || !data.runtime) return false;
    var window = data.window;
    var runtime = data.runtime;
    if (!timestamp(window.from) || !timestamp(window.to) || compareTime(window.from, window.to) >= 0 || window.step_seconds !== 10) return false;
    if (!timestamp(runtime.since) || typeof runtime.complete !== "boolean") return false;
    if (!["active_requests", "active_clients", "websocket_sessions"].every(function (key) { return counter(runtime[key]); })) return false;
    if (!totals(data.totals) || !series(data.series) || !Array.isArray(data.clients)) return false;
    var names = new Set();
    return data.clients.every(function (client) {
      if (!client || typeof client.name !== "string" || names.has(client.name)) return false;
      names.add(client.name);
      if (typeof client.configured !== "boolean" || !counter(client.active_requests) || !counter(client.websocket_sessions)) return false;
      if (!totals(client.totals) || !series(client.series) || client.series.length !== data.series.length) return false;
      return client.series.every(function (bucket, index) {
        return bucket.from === data.series[index].from && bucket.to === data.series[index].to;
      });
    });
  }
  function loadActivity() {
    if (!visible()) return Promise.resolve();
    if (activityFlight) return activityFlight;
    var epoch = generation;
    var controller = new AbortController();
    activityController = controller;
    el("refreshOverviewBtn").disabled = true;
    el("page-overview").setAttribute("aria-busy", "true");
    if (!snapshot) text("activityStatus", "正在加载实时活动…");
    var flight = (async function () {
      try {
        var data = await request("/admin/api/activity", controller);
        if (epoch !== generation || controller.signal.aborted || !visible()) return;
        if (!validSnapshot(data)) throw new Error("实时活动数据格式无效");
        // A response from an older sample must never rewind the displayed window.
        if (snapshot && data.runtime.since === snapshot.runtime.since && compareTime(data.generated_at, snapshot.generated_at) < 0) {
          text("activityStatus", "采样时间未更新，显示上次数据。请稍后刷新。");
          el("activityStatus").dataset.state = "partial";
          return;
        }
        renderSnapshot(data);
      } catch (error) {
        if (epoch !== generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        if (!snapshot) {
          metricIDs.forEach(function (id) { text(id, "不可用"); });
          text("activityUpdatedAt", "尚无成功采样");
          chartIDs.forEach(function (id) { el(id).innerHTML = ""; });
          text("activityConcurrencyReadout", "曲线暂不可用");
          text("activityTokenReadout", "曲线暂不可用");
          emptyCell.textContent = "客户端活动暂不可用";
          el("activityClientsBody").appendChild(emptyRow);
        }
        reportActivityStatus(error);
        renderChain();
        renderAttention();
      } finally {
        if (epoch === generation) {
          activityFlight = null;
          activityController = null;
          el("refreshOverviewBtn").disabled = false;
          el("page-overview").setAttribute("aria-busy", "false");
          schedule();
        }
      }
    })();
    activityFlight = flight;
    return flight;
  }
  function clientValue(client, key) {
    return key === "name" ? client.name : (key === "active_requests" || key === "websocket_sessions" ? client[key] : client.totals[key]);
  }
  function selectClient(name) {
    if (selectedClient === name) return;
    selectedClient = name;
    renderClients();
    renderCharts();
    var view = document.defaultView;
    if (view && !view.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      chartIDs.forEach(function (id) {
        var chart = el(id);
        if (typeof chart.animate === "function") chart.animate([{opacity: 0.75}, {opacity: 1}], {duration: 120, easing: "ease-out"});
      });
    }
  }
  function makeRow(name) {
    var row = document.createElement("tr");
    var cells = [];
    for (var i = 0; i < 6; i++) {
      cells.push(document.createElement("td"));
      row.appendChild(cells[i]);
    }
    var button = document.createElement("button");
    button.type = "button";
    button.className = "activity-client-button";
    button.textContent = name;
    button.addEventListener("click", function () { selectClient(selectedClient === name ? null : name); });
    cells[0].appendChild(button);
    var label = document.createElement("span");
    label.className = "activity-unconfigured";
    label.textContent = "未配置";
    cells[0].appendChild(label);
    row.addEventListener("click", function (event) {
      if (event.target !== button) selectClient(selectedClient === name ? null : name);
    });
    return {node: row, cells: cells, button: button, label: label};
  }
  function renderClients() {
    if (!snapshot) return;
    var filter = el("activityClientSearch").value.trim().toLocaleLowerCase();
    var list = snapshot.clients.filter(function (client) { return client.name.toLocaleLowerCase().includes(filter); });
    list.sort(function (a, b) {
      var result = sortKey === "name" ? a.name.localeCompare(b.name, "zh-CN") : Number(clientValue(a, sortKey)) - Number(clientValue(b, sortKey));
      return result * sortDirection || a.name.localeCompare(b.name, "zh-CN");
    });
    var body = el("activityClientsBody");
    var names = new Set(list.map(function (client) { return client.name; }));
    var configuredNames = new Set(snapshot.clients.map(function (client) { return client.name; }));
    var focused = document.activeElement;
    rows.forEach(function (row, name) {
      if (!names.has(name) && row.node.parentNode === body) body.removeChild(row.node);
      if (!configuredNames.has(name)) rows.delete(name);
    });
    if (emptyRow.parentNode === body) body.removeChild(emptyRow);
    list.forEach(function (client, index) {
      var row = rows.get(client.name);
      if (!row) { row = makeRow(client.name); rows.set(client.name, row); }
      row.node.dataset.selected = String(client.name === selectedClient);
      row.button.setAttribute("aria-pressed", String(client.name === selectedClient));
      row.button.setAttribute("aria-label", "查看 " + client.name + " 的曲线");
      row.label.hidden = client.configured !== false;
      ["active_requests", "websocket_sessions", "input_tokens", "output_tokens", "total_tokens"].forEach(function (key, i) { row.cells[i + 1].textContent = number(clientValue(client, key)); });
      // Reuse the row and button so five-second refreshes do not replace focus.
      if (body.children[index] !== row.node) body.insertBefore(row.node, body.children[index] || null);
    });
    if (!list.length) {
      emptyCell.textContent = filter ? "没有匹配的客户端" : "近5分钟暂无客户端活动";
      body.appendChild(emptyRow);
    }
    if (focused && focused !== document.activeElement && typeof focused.focus === "function" && focused.isConnected) focused.focus({preventScroll: true});
    text("activityClientCount", list.length + " / " + snapshot.clients.length + " 个客户端");
    document.querySelectorAll("[data-activity-sort]").forEach(function (button) {
      var selected = button.dataset.activitySort === sortKey;
      button.setAttribute("aria-label", button.dataset.label + (selected ? (sortDirection === 1 ? "，当前升序，点击降序" : "，当前降序，点击升序") : "，点击排序"));
      var header = button.closest("th");
      if (header) header.setAttribute("aria-sort", selected ? (sortDirection === 1 ? "ascending" : "descending") : "none");
      button.dataset.direction = selected ? (sortDirection === 1 ? "asc" : "desc") : "none";
      var icon = button.querySelector("path");
      if (icon) icon.setAttribute("d", selected ? (sortDirection === 1 ? "m5 10 3-3 3 3" : "m5 6 3 3 3-3") : "m5 6 3-3 3 3M5 10l3 3 3-3");
    });
  }
  function currentSeries() {
    if (!snapshot) return [];
    if (selectedClient === null) return snapshot.series;
    var client = snapshot.clients.find(function (candidate) { return candidate.name === selectedClient; });
    return client ? client.series : [];
  }
  function chartAxis(value) {
    if (value <= 0) return {maximum: 4, step: 1, ticks: 4};
    var desiredStep = Math.max(1, value * 1.1 / 4);
    var scale = Math.pow(10, Math.floor(Math.log10(desiredStep)));
    var step = Math.ceil([1, 2, 2.5, 3, 5, 10].find(function (factor) { return factor * scale >= desiredStep; }) * scale);
    var ticks = Math.ceil(value * 1.1 / step);
    return {maximum: ticks * step, step: step, ticks: ticks};
  }
  function chartGeometry(chart) {
    var bounds = chart.getBoundingClientRect();
    var width = Math.max(160, bounds.width || 560);
    var height = Math.max(150, bounds.height || 230);
    return {width: width, height: height, left: 44, right: width - 12, top: 16, bottom: height - 42};
  }
  function compactNumber(value) {
    if (value >= 1000000000) return Number((value / 1000000000).toPrecision(3)) + "G";
    if (value >= 1000000) return Number((value / 1000000).toPrecision(3)) + "M";
    if (value >= 1000) return Number((value / 1000).toPrecision(3)) + "k";
    return number(value);
  }
  function chartHTML(series, tokenChart, chart) {
    var geometry = chartGeometry(chart);
    chart.setAttribute("viewBox", "0 0 " + geometry.width + " " + geometry.height);
    var left = geometry.left, right = geometry.right, top = geometry.top, bottom = geometry.bottom;
    var windowFrom = Date.parse(snapshot.window.from), windowTo = Date.parse(snapshot.window.to);
    function timeX(time) { return left + (time - windowFrom) / (windowTo - windowFrom) * (right - left); }
    var keys = tokenChart ? ["input_tokens", "output_tokens"] : ["active_requests_peak"];
    var axis = chartAxis(Math.max.apply(null, [0].concat(series.flatMap(function (bucket) { return keys.map(function (key) { return bucket[key] === null ? 0 : Number(bucket[key]); }); }))));
    var maximum = axis.maximum;
    var x = function (index) { return timeX((Math.max(windowFrom, Date.parse(series[index].from)) + Math.min(windowTo, Date.parse(series[index].to))) / 2); };
    var y = function (value) { return bottom - value / maximum * (bottom - top); };
    var svg = "";
    for (var i = 0; i <= axis.ticks; i++) {
      var value = axis.step * i;
      svg += '<line class="activity-gridline" x1="' + left + '" x2="' + right + '" y1="' + y(value) + '" y2="' + y(value) + '"/><text class="activity-axis" x="34" y="' + (y(value) + 4) + '" text-anchor="end">' + esc(compactNumber(value)) + '</text>';
    }
    [0, 0.5, 1].forEach(function (ratio) {
      var time = Date.parse(snapshot.window.from) + ratio * (Date.parse(snapshot.window.to) - Date.parse(snapshot.window.from));
      svg += '<text class="activity-axis" x="' + (left + ratio * (right - left)) + '" y="' + (geometry.height - 12) + '" text-anchor="' + (ratio === 0 ? "start" : ratio === 1 ? "end" : "middle") + '">' + esc(clock(time)) + '</text>';
    });
    if (tokenChart) {
      keys.forEach(function (key, keyIndex) {
        var segments = [], points = [];
        function flush() { if (points.length) { segments.push(points); points = []; } }
        series.forEach(function (bucket, index) {
          if (bucket[key] === null || bucket[key] === undefined) { flush(); return; }
          points.push({x: x(index), y: y(Number(bucket[key]))});
        });
        flush();
        var lineClass = keyIndex ? "activity-output" : "activity-input";
        segments.forEach(function (segment) {
          if (segment.length === 1) svg += '<circle class="' + lineClass + '" cx="' + segment[0].x + '" cy="' + segment[0].y + '" r="2.5"/>';
          else svg += '<polyline class="activity-line ' + lineClass + '" points="' + segment.map(function (point) { return point.x + "," + point.y; }).join(" ") + '"/>';
        });
      });
    } else {
      var barWidth = Math.max(2, Math.min(24, (right - left) / Math.max(1, series.length) * 0.62));
      series.forEach(function (bucket, index) {
        if (bucket.active_requests_peak === null || bucket.active_requests_peak === undefined) return;
        var peak = Number(bucket.active_requests_peak);
        if (!isFinite(peak) || peak <= 0) return;
        svg += '<rect class="activity-concurrency-bar" x="' + (x(index) - barWidth / 2) + '" y="' + y(peak) + '" width="' + barWidth + '" height="' + (bottom - y(peak)) + '" rx="2"/>';
      });
    }
    var index = series.findIndex(function (bucket) { return bucket.from === focusFrom; });
    if (index >= 0) {
      svg += '<line class="activity-cursor" x1="' + x(index) + '" x2="' + x(index) + '" y1="' + top + '" y2="' + bottom + '"/>';
      keys.forEach(function (key, keyIndex) {
        if (series[index][key] == null) return;
        svg += '<circle class="activity-point ' + (tokenChart ? (keyIndex ? "activity-output" : "activity-input") : "activity-concurrency") + '" cx="' + x(index) + '" cy="' + y(Number(series[index][key])) + '" r="4"/>';
      });
    }
    return svg;
  }
  function renderCharts() {
    if (!snapshot) return;
    text("activityChartScope", selectedClient === null ? "全站" : selectedClient);
    el("activityResetScope").hidden = selectedClient === null;
    var series = currentSeries();
    var missing = selectedClient !== null && !snapshot.clients.some(function (client) { return client.name === selectedClient; });
    text("activityScopeStatus", missing ? "该客户端已离开近5分钟窗口，可返回全站。" : "");
    if (focusFrom && !series.some(function (bucket) { return bucket.from === focusFrom; })) focusFrom = series.length ? series[0].from : null;
    chartIDs.forEach(function (id, index) {
      el(id).innerHTML = series.length ? chartHTML(series, index === 1, el(id)) : "";
      el(id).setAttribute("aria-label", (selectedClient === null ? "全站" : selectedClient) + (index ? "输入与输出 Token，近5分钟，每10秒；方向键查看读数" : "并发峰值，近5分钟，每10秒；方向键查看读数"));
    });
    var bucket = series.find(function (candidate) { return candidate.from === focusFrom; });
    var prefix = bucket ? clock(bucket.from) + "–" + clock(bucket.to) + (bucket.partial ? " · 部分桶" : "") + " · " : "";
    text("activityConcurrencyReadout", bucket ? prefix + (bucket.active_requests_peak === null ? "峰值未采样" : "峰值 " + number(bucket.active_requests_peak) + " 个请求") : (series.length ? "移入曲线或聚焦后使用方向键查看读数" : "暂无曲线数据"));
    text("activityTokenReadout", bucket ? prefix + "输入 " + number(bucket.input_tokens) + " · 输出 " + number(bucket.output_tokens) + (bucket.incomplete_usage_count > 0 ? " · 用量不完整" : "") : (series.length ? "移入曲线或聚焦后使用方向键查看读数" : "暂无曲线数据"));
  }
  chartIDs.forEach(function (id) {
    var chart = el(id);
    chart.addEventListener("pointermove", function (event) {
      var series = currentSeries();
      if (!series.length) return;
      var bounds = chart.getBoundingClientRect();
      var geometry = chartGeometry(chart);
      var relative = (event.clientX - bounds.left - geometry.left) / (geometry.right - geometry.left);
      var time = Date.parse(snapshot.window.from) + Math.max(0, Math.min(1, relative)) * (Date.parse(snapshot.window.to) - Date.parse(snapshot.window.from));
      var index = series.findIndex(function (bucket) { return time >= Date.parse(bucket.from) && time < Date.parse(bucket.to); });
      if (index < 0) index = time < Date.parse(series[0].from) ? 0 : series.length - 1;
      if (focusFrom !== series[index].from) { focusFrom = series[index].from; renderCharts(); }
    });
    chart.addEventListener("focus", function () {
      var series = currentSeries();
      if (!focusFrom && series.length) { focusFrom = series[series.length - 1].from; renderCharts(); }
    });
    chart.addEventListener("keydown", function (event) {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      var series = currentSeries();
      if (!series.length) return;
      event.preventDefault();
      var index = series.findIndex(function (bucket) { return bucket.from === focusFrom; });
      if (index < 0) index = series.length - 1;
      index = event.key === "Home" ? 0 : event.key === "End" ? series.length - 1 : Math.max(0, Math.min(series.length - 1, index + (event.key === "ArrowRight" ? 1 : -1)));
      focusFrom = series[index].from;
      renderCharts();
    });
  });
  if (typeof ResizeObserver === "function") {
    var chartObserver = new ResizeObserver(function () { renderCharts(); });
    chartIDs.forEach(function (id) { chartObserver.observe(el(id)); });
  } else if (document.defaultView) {
    document.defaultView.addEventListener("resize", renderCharts);
  }
  el("activityClientSearch").addEventListener("input", renderClients);
  el("activityResetScope").addEventListener("click", function () { selectClient(null); });
  document.querySelectorAll("[data-activity-sort]").forEach(function (button) {
    button.addEventListener("click", function () {
      sortDirection = sortKey === button.dataset.activitySort ? -sortDirection : (button.dataset.activitySort === "name" ? 1 : -1);
      sortKey = button.dataset.activitySort;
      renderClients();
    });
  });

  // Historical completion samples and cost keep their existing API and semantics.
  function renderHistory(ov) {
    var stats = isObject(ov.request_stats_72h) ? ov.request_stats_72h : null;
    var available = !!stats && stats.available === true;
    var totalAvailable = available && counter(stats.total);
    var failuresAvailable = available && counter(stats.record_failures);
    var statsStatus;
    if (!available) statsStatus = "请求完成统计暂不可用，请稍后刷新。";
    else {
      statsStatus = "";
      if (!totalAvailable) statsStatus += "完成样本总数不可用。";
      else if (stats.total === 0) statsStatus += "暂无完成样本。";
      if (!failuresAvailable) statsStatus += "统计写入失败计数不可用，无法判断样本完整性。";
      else if (stats.record_failures > 0) statsStatus += "警告：本进程有 " + number(stats.record_failures) + " 次统计写入失败，样本可能不完整。";
    }
    text("activityHistoryState", available ? (!failuresAvailable ? "样本完整性不可用" : stats.record_failures > 0 ? "样本可能不完整" : "") : "统计暂不可用");
    text("ovTotalReqs", totalAvailable ? fmtNum(stats.total) : "不可用");
    text("ovSuccessRate", available ? (stats.success_rate == null ? "--" : fmtPct(stats.success_rate)) : "不可用");
    text("ovRequestStatsStatus", statsStatus);
    text("activityHistoryTooltip", "按代理请求完成及有效 WebSocket create 轮次计数；成功率 = 成功 / 总量（含失败、取消），不是服务可用率。不含代理前鉴权/IP 拒绝、健康/管理/ping、会话控制及无效或鉴权复核拒绝消息。" + (available ? "采集开始：" + (stats.since || "不可用") + "；统计区间：[" + (stats.from || "不可用") + "，" + (stats.to || "不可用") + ")。当前未结束分钟不计入。" : "请求完成统计暂不可用。"));
    text("ovTargets", counter(ov.active_targets) ? fmtNum(ov.active_targets) : "不可用");
    text("ovClients", counter(ov.client_count) ? fmtNum(ov.client_count) : "不可用");
    text("ovModelCosts", counter(ov.model_cost_count) ? fmtNum(ov.model_cost_count) : "不可用");
    var summary = isObject(ov.usage_summary) ? ov.usage_summary : null;
    var periods = [{k: "last_hour", l: "近1小时"}, {k: "today", l: "今日"}, {k: "yesterday", l: "昨日"}, {k: "last_7_days", l: "近7天"}, {k: "last_30_days", l: "近30天"}];
    el("overviewUsageSummary").innerHTML = '<table class="activity-history-table"><thead><tr><th>区间</th><th>用量事件数</th><th>总 Token</th><th>历史费用</th></tr></thead><tbody>' + periods.map(function (period) {
      var data = summary && isObject(summary[period.k]) ? summary[period.k] : null;
      var requests = data && counter(data.requests) ? fmtNum(data.requests) : "不可用";
      var total = data && counter(data.input_tokens) && counter(data.output_tokens) ? fmtNum(totalTok(data)) : "不可用";
      var cost = data && counter(data.estimated_cost) ? fmtMoney(data.estimated_cost) : "不可用";
      return '<tr><th scope="row">' + esc(period.l) + '</th><td>' + esc(requests) + '</td><td>' + esc(total) + '</td><td>' + esc(cost) + '</td></tr>';
    }).join("") + '</tbody></table>';
    renderTargetHealth();
    renderRecentChanges();
    renderAttention();
    renderChain();
    if (tooltipTrigger && tooltipTrigger.dataset.activityInfoSource === "activityHistoryTooltip") showTooltip(tooltipTrigger);
  }
  function loadMetrics(force) {
    if (!visible()) return Promise.resolve();
    if (metricsFlight) return metricsFlight;
    if (!force && metricsFetchedAt && Date.now() - metricsFetchedAt < 60000) return Promise.resolve();
    var epoch = generation;
    var controller = new AbortController();
    metricsController = controller;
    var flight = (async function () {
      try {
        var data = await request("/admin/metrics", controller);
        if (epoch !== generation || controller.signal.aborted || !visible()) return;
        if (!isObject(data)) throw new Error("指标数据格式无效");
        metricsSnapshot = data;
        metricsFetchedAt = Date.now();
        renderChain();
        renderAttention();
      } catch (error) {
        if (epoch !== generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        metricsSnapshot = null;
        metricsFetchedAt = 0;
        renderChain();
        renderAttention();
      } finally {
        if (epoch === generation) { metricsFlight = null; metricsController = null; }
      }
    })();
    metricsFlight = flight;
    return flight;
  }
  function loadAggregate(force) {
    if (!visible()) return Promise.resolve();
    if (aggregateFlight) return aggregateFlight;
    if (!force && aggregateFetchedAt && Date.now() - aggregateFetchedAt < 60000) return Promise.resolve();
    var epoch = aggregateGeneration;
    var controller = new AbortController();
    var window = rangeWindow(selectedRange, Date.now());
    aggregateController = controller;
    aggregateLoading = true;
    aggregateRequestWindow = window;
    if (!aggregateSnapshot) {
      aggregateError = "";
      renderRangePanels();
    }
    var flight = (async function () {
      try {
        var data = await request(aggregatePath(window), controller);
        if (epoch !== aggregateGeneration || controller.signal.aborted || !visible()) return;
        if (!isObject(data) || !isObject(data.totals)) throw new Error("聚合数据格式无效");
        aggregateSnapshot = data;
        aggregateSnapshotWindow = window;
        aggregateError = "";
        aggregateFetchedAt = Date.now();
      } catch (error) {
        if (epoch !== aggregateGeneration || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        // Keep the last snapshot (and its own window) of this range and mark it stale.
        aggregateError = error.name === "TimeoutError" ? "加载超时" : "加载失败";
        aggregateFetchedAt = 0;
      } finally {
        if (epoch === aggregateGeneration) {
          aggregateLoading = false;
          aggregateFlight = null;
          aggregateController = null;
          renderRangePanels();
          renderAttention();
        }
      }
    })();
    aggregateFlight = flight;
    return flight;
  }
  function selectRange(key) {
    if (!rangeOption(key) || key === selectedRange) {
      syncRangeButtons();
      return;
    }
    selectedRange = key;
    storeRange(key);
    // A new generation makes any in-flight response for the old range inert.
    aggregateGeneration++;
    if (aggregateController) aggregateController.abort();
    aggregateFlight = null;
    aggregateController = null;
    aggregateFetchedAt = 0;
    aggregateSnapshot = null;
    aggregateSnapshotWindow = null;
    aggregateRequestWindow = rangeWindow(key, Date.now());
    aggregateError = "";
    aggregateLoading = true;
    trendFocus = null;
    syncRangeButtons();
    renderRangePanels();
    renderAttention();
    if (visible()) loadAggregate(true);
  }
  function loadHistory(force) {
    if (!visible()) return Promise.resolve();
    if (historyFlight) return historyFlight;
    if (!force && historyFetchedAt && Date.now() - historyFetchedAt < 60000) return Promise.resolve();
    var epoch = generation;
    var controller = new AbortController();
    historyController = controller;
    historyFetchedAt = Date.now();
    var flight = (async function () {
      try {
        var data = await request("/admin/api/overview", controller);
        if (epoch === generation && !controller.signal.aborted && visible()) {
          historySnapshot = isObject(data) ? data : {};
          historyError = "";
          renderHistory(historySnapshot);
        }
      } catch (error) {
        if (epoch !== generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        historyFetchedAt = 0;
        var reason = error.name === "TimeoutError" ? "加载超时" : "加载失败";
        historyError = reason;
        text("activityHistoryState", reason);
        text("ovRequestStatsStatus", "历史统计" + reason + (historySnapshot ? "，显示上次数据；" : "，") + "请点击刷新重试。");
        if (!historySnapshot) {
          ["ovTotalReqs", "ovSuccessRate", "ovTargets", "ovClients", "ovModelCosts"].forEach(function (id) { text(id, "不可用"); });
          el("overviewUsageSummary").innerHTML = '<p class="activity-history-error">历史费用' + reason + '，请点击刷新重试。</p>';
          historySnapshot = null;
        }
        renderTargetHealth();
        renderRecentChanges();
        renderAttention();
        renderChain();
      } finally {
        if (epoch === generation) { historyFlight = null; historyController = null; }
      }
    })();
    historyFlight = flight;
    return flight;
  }
  function loadOverview(forceHistory) {
    return Promise.all([loadActivity(), loadHistory(forceHistory === true), loadMetrics(forceHistory === true), loadAggregate(forceHistory === true)]);
  }
  function startOverviewAutoRefresh() {
    enabled = true;
    loadOverview();
  }
  function stopOverviewAutoRefresh() {
    enabled = false;
    cancelRequests();
  }
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) cancelRequests();
    else if (enabled && visible()) loadOverview();
  });
  el("refreshOverviewBtn").addEventListener("click", function () { loadOverview(true); });
  rangeButtons = Array.prototype.slice.call(document.querySelectorAll("[data-overview-range]"));
  rangeButtons.forEach(function (button) {
    button.addEventListener("click", function () { selectRange(button.dataset.overviewRange); });
  });
  syncRangeButtons();
  renderRangeWindow(rangeWindow(selectedRange, Date.now()));
  barButtons = Array.prototype.slice.call(document.querySelectorAll("[data-overview-bar]"));
  barButtons.forEach(function (button) {
    button.addEventListener("click", function () {
      var metric = button.dataset.overviewBar === "cost" ? "cost" : "requests";
      if (metric === barMetric) return;
      barMetric = metric;
      drawTrends();
    });
  });
  function focusTrend(index) {
    if (!trendSlots || !trendSlots.length) return;
    index = Math.max(0, Math.min(trendSlots.length - 1, index));
    var start = trendSlots[index].start.getTime();
    if (start !== trendFocus) { trendFocus = start; drawTrends(); }
  }
  trendIDs.forEach(function (id) {
    var chart = el(id);
    chart.addEventListener("pointermove", function (event) {
      if (!trendSlots || !trendSlots.length) return;
      var bounds = chart.getBoundingClientRect();
      var geometry = chartGeometry(chart);
      var relative = (event.clientX - bounds.left - geometry.left) / (geometry.right - geometry.left);
      focusTrend(Math.floor(Math.max(0, Math.min(0.9999, relative)) * trendSlots.length));
    });
    chart.addEventListener("focus", function () {
      if (trendFocus === null && trendSlots && trendSlots.length) focusTrend(trendSlots.length - 1);
    });
    chart.addEventListener("keydown", function (event) {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      if (!trendSlots || !trendSlots.length) return;
      event.preventDefault();
      var index = trendSlots.findIndex(function (slot) { return slot.start.getTime() === trendFocus; });
      if (index < 0) index = trendSlots.length - 1;
      index = event.key === "Home" ? 0 : event.key === "End" ? trendSlots.length - 1 : index + (event.key === "ArrowRight" ? 1 : -1);
      focusTrend(index);
    });
  });
  if (typeof ResizeObserver === "function") {
    var trendObserver = new ResizeObserver(function () { drawTrends(); });
    trendIDs.forEach(function (id) { trendObserver.observe(el(id)); });
  } else if (document.defaultView) {
    document.defaultView.addEventListener("resize", drawTrends);
  }

  function hideTooltip() {
    el("activityTooltip").hidden = true;
    tooltipTrigger = null;
  }
  function showTooltip(trigger) {
    tooltipTrigger = trigger;
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
    trigger.addEventListener("pointerdown", function (event) { touchPointer = event.pointerType === "touch"; if (touchPointer) touchWasOpen = tooltipTrigger === trigger; });
    trigger.addEventListener("click", function (event) {
      if (touchPointer ? touchWasOpen : tooltipTrigger === trigger) hideTooltip();
      else showTooltip(trigger);
      touchPointer = false;
    });
  });
  document.addEventListener("keydown", function (event) { if (event.key === "Escape") hideTooltip(); });
  document.addEventListener("pointerdown", function (event) { if (tooltipTrigger && event.target !== tooltipTrigger && !tooltipTrigger.contains(event.target)) hideTooltip(); });
  document.addEventListener("scroll", hideTooltip, true);
  return {loadOverview: loadOverview, startOverviewAutoRefresh: startOverviewAutoRefresh, stopOverviewAutoRefresh: stopOverviewAutoRefresh, selectRange: selectRange, rangeWindow: rangeWindow, rollupBuckets: rollupBuckets, aggregatePath: aggregatePath};
});
