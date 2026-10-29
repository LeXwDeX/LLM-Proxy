"use strict";
// Overview range panels: summary cards, trend charts and top lists for the selected range.
AdminApp.register("overview_trends", function (app) {
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var fmtPct = app.bind("core", "fmtPct");
  var totalTok = app.bind("core", "totalTok");
  var S = app.bind("overview_state", "store")();
  var request = app.bind("overview", "request");
  var rangeOption = app.bind("overview_range", "rangeOption");
  var rangeWindow = app.bind("overview_range", "rangeWindow");
  var rollupBuckets = app.bind("overview_range", "rollupBuckets");
  var windowSpan = app.bind("overview_range", "windowSpan");
  var barRect = app.bind("overview_util", "barRect");
  var barWidth = app.bind("overview_util", "barWidth");
  var chartAxis = app.bind("overview_util", "chartAxis");
  var chartGeometry = app.bind("overview_util", "chartGeometry");
  var compactNumber = app.bind("overview_util", "compactNumber");
  var counter = app.bind("overview_util", "counter");
  var el = app.bind("overview_util", "el");
  var inputTotal = app.bind("overview_util", "inputTotal");
  var isObject = app.bind("overview_util", "isObject");
  var offsetLabel = app.bind("overview_util", "offsetLabel");
  var offsetMinutes = app.bind("overview_util", "offsetMinutes");
  var shortDate = app.bind("overview_util", "shortDate");
  var shortTime = app.bind("overview_util", "shortTime");
  var stackedBars = app.bind("overview_util", "stackedBars");
  var text = app.bind("overview_util", "text");
  var trendIDs = ["ovTokenTrend", "ovBarTrend"];
  function rangeTotals() {
    return S.aggregateSnapshot && isObject(S.aggregateSnapshot.totals) ? S.aggregateSnapshot.totals : null;
  }
  // True when the shown snapshot covers a different period than the one just
  // requested, e.g. yesterday's "today" after the page stayed open past midnight.
  // Within the same period only `to` advances and `from` stays equal.
  function snapshotOutdated() {
    return !!(S.aggregateSnapshot && S.aggregateSnapshotWindow && S.aggregateRequestWindow &&
      S.aggregateSnapshotWindow.from.getTime() !== S.aggregateRequestWindow.from.getTime());
  }
  function aggregateStaleText() {
    var verb = S.aggregateError === "加载超时" ? "超时" : "失败";
    if (snapshotOutdated()) return "更新" + verb + "，显示 " + windowSpan(S.aggregateSnapshotWindow) + " 的上次数据";
    return "更新" + verb + "，显示上次数据";
  }
  function rangeComplete(data) {
    return isObject(data) && ["requests", "input_tokens", "output_tokens", "estimated_cost"].every(function (key) { return counter(data[key]); });
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
    var option = rangeOption(S.selectedRange);
    // Describe the data on screen: the snapshot's own window until a newer request succeeds.
    var shownWindow = S.aggregateSnapshotWindow || S.aggregateRequestWindow || rangeWindow(S.selectedRange, Date.now());
    var frozen = !!S.aggregateError && !!S.aggregateSnapshot && shownWindow === S.aggregateSnapshotWindow;
    renderRangeWindow(shownWindow, S.aggregateError && snapshotOutdated() ? aggregateStaleText() : "", frozen);
    text("ovRangeLabel", option.label);
    text("topClientsRange", option.label);
    text("topModelsRange", option.label);
    var data = rangeTotals();
    var complete = rangeComplete(data);
    var requests = data && counter(data.requests) ? data.requests : null;
    var hasTokens = data && counter(data.input_tokens) && counter(data.output_tokens);
    var cost = data && counter(data.estimated_cost) ? data.estimated_cost : null;
    var clients = S.aggregateSnapshot && Array.isArray(S.aggregateSnapshot.by_client) ? S.aggregateSnapshot.by_client.length : null;
    var models = S.aggregateSnapshot && Array.isArray(S.aggregateSnapshot.by_model) ? S.aggregateSnapshot.by_model.length : null;
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
    if (!S.aggregateSnapshot && S.aggregateError) {
      status.textContent = option.label + "用量不可用：" + S.aggregateError + "，请点击刷新重试。";
      status.dataset.state = "error";
    } else if (!S.aggregateSnapshot && S.aggregateLoading) {
      status.textContent = "正在加载" + option.label + "用量…";
      status.dataset.state = "";
    } else if (!S.aggregateSnapshot) {
      status.textContent = option.label + "用量不可用。";
      status.dataset.state = "error";
    } else {
      status.textContent = (complete ? "" : "部分字段不可用。") + notes.join(" ");
      status.dataset.state = complete && !incomplete && !S.aggregateError ? "ready" : "partial";
      if (S.aggregateError) status.textContent = "区间用量" + aggregateStaleText() + "。" + status.textContent;
    }
  }
  function slotLabel(slot) {
    return trendGranularity() === "hour" ? shortTime(slot.start) : shortDate(slot.start);
  }
  function trendGranularity() {
    return S.aggregateSnapshotWindow ? S.aggregateSnapshotWindow.granularity : (rangeWindow(S.selectedRange, Date.now()) || {}).granularity;
  }
  function slotReadout(slot) {
    var span = trendGranularity() === "hour" ? shortDate(slot.start) + " " + shortTime(slot.start) + "–" + shortTime(slot.end) : shortDate(slot.start);
    var parts = [span, "请求权重 " + fmtNum(slot.requests), "输入 " + fmtNum(slot.input), "输出 " + fmtNum(slot.output), "估算费用 " + fmtMoney(slot.cost)];
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
    var cost = !token && S.barMetric === "cost";
    var keys = token ? ["input", "output"] : [cost ? "cost" : "requests"];
    var peak = Math.max.apply(null, [0].concat(slots.map(function (slot) { return keys.reduce(function (sum, key) { return sum + slot[key]; }, 0); })));
    var axis = cost ? moneyAxis(peak) : chartAxis(peak);
    function y(value) { return bottom - value / axis.maximum * (bottom - top); }
    var focus = slots.findIndex(function (slot) { return slot.start.getTime() === S.trendFocus; });
    var svg = "";
    if (focus >= 0) svg += '<rect class="overview-trend-focus" x="' + (x(focus) - band / 2) + '" y="' + top + '" width="' + band + '" height="' + (bottom - top) + '"/>';
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
    var width = barWidth(band);
    slots.forEach(function (slot, index) {
      if (token) svg += stackedBars(x(index) - width / 2, width, [slot.input, slot.output], y);
      else svg += barRect("overview-bar overview-bar-" + (cost ? "cost" : "requests"), x(index) - width / 2, width, 0, slot[keys[0]], y);
    });
    return svg;
  }
  function syncBarButtons() {
    S.barButtons.forEach(function (button) {
      button.setAttribute("aria-pressed", button.dataset.overviewBar === S.barMetric ? "true" : "false");
    });
    text("ovBarLegend", S.barMetric === "cost" ? "估算费用（含模型组系数）" : "请求权重");
    text("ovBarUnit", S.barMetric === "cost" ? "美元" : "请求权重");
    el("ovBarLegendSwatch").setAttribute("data-metric", S.barMetric);
  }
  function trendLabel(token) {
    var option = rangeOption(S.selectedRange);
    var name = token ? "Token 趋势（输入与输出）" : (S.barMetric === "cost" ? "估算费用趋势" : "请求权重趋势");
    return option.label + "，" + name;
  }
  // drawTrends redraws from cached slots; hover and keyboard moves never refetch.
  function drawTrends() {
    syncBarButtons();
    var slots = S.trendSlots;
    var readout = "";
    if (!slots) {
      readout = S.aggregateLoading && !S.aggregateSnapshot ? "正在加载趋势…" : "区间趋势不可用";
      trendIDs.forEach(function (id, index) {
        el(id).innerHTML = "";
        el(id).setAttribute("aria-label", trendLabel(index === 0) + "，" + (S.aggregateLoading && !S.aggregateSnapshot ? "正在加载" : "不可用"));
      });
      text("ovTokenTrendReadout", readout);
      text("ovBarTrendReadout", readout);
      return;
    }
    if (S.trendFocus !== null && !slots.some(function (slot) { return slot.start.getTime() === S.trendFocus; })) S.trendFocus = null;
    var unit = trendGranularity() === "hour" ? "按小时" : "按本地自然日";
    trendIDs.forEach(function (id, index) {
      el(id).innerHTML = slots.length ? trendHTML(el(id), slots, index === 0) : "";
      el(id).setAttribute("aria-label", trendLabel(index === 0) + "，" + unit + "，共 " + slots.length + " 个时间桶；方向键查看读数");
    });
    var focused = slots.find(function (slot) { return slot.start.getTime() === S.trendFocus; });
    var empty = slots.every(function (slot) { return slot.empty; });
    readout = focused ? slotReadout(focused) : (empty ? "该区间暂无已记录用量" : "移入图表或聚焦后使用方向键查看读数");
    text("ovTokenTrendReadout", readout);
    text("ovBarTrendReadout", readout);
  }
  function renderTrends() {
    var buckets = S.aggregateSnapshot ? S.aggregateSnapshot.buckets : undefined;
    // Go encodes an empty bucket slice as null; a missing key is unavailable.
    if (buckets === null && Object.prototype.hasOwnProperty.call(S.aggregateSnapshot, "buckets")) buckets = [];
    S.trendSlots = Array.isArray(buckets) && S.aggregateSnapshotWindow ? rollupBuckets(buckets, S.aggregateSnapshotWindow) : null;
    text("ovTrendUnit", trendGranularity() === "hour" ? "按小时" : "按天（本地自然日）");
    var status = el("ovTrendStatus");
    if (!S.trendSlots) {
      if (S.aggregateLoading && !S.aggregateSnapshot && !S.aggregateError) {
        status.textContent = "正在加载区间趋势…";
        status.dataset.state = "";
      } else {
        status.textContent = "区间趋势不可用：" + (S.aggregateError ? S.aggregateError : S.aggregateSnapshot ? "聚合响应缺少时间桶" : "尚未加载聚合") + "。";
        status.dataset.state = "error";
      }
    } else {
      var empty = S.trendSlots.every(function (slot) { return slot.empty; });
      var notes = [];
      var usageGaps = S.trendSlots.filter(function (slot) { return slot.incompleteUsage > 0; }).length;
      var missing = S.trendSlots.filter(function (slot) { return slot.missing; }).length;
      if (usageGaps) notes.push(fmtNum(usageGaps) + " 个时间桶含不完整用量");
      if (missing) notes.push(fmtNum(missing) + " 个时间桶部分字段缺失");
      status.textContent = (empty ? "该区间暂无已记录用量，时间桶按 0 显示。" : "共 " + fmtNum(S.trendSlots.length) + " 个时间桶；没有用量的时间桶按 0 显示。") + (notes.length ? notes.join("，") + "。" : "");
      status.dataset.state = notes.length || S.aggregateError ? "partial" : "ready";
      if (S.aggregateError) status.textContent = "区间趋势" + aggregateStaleText() + "。" + status.textContent;
    }
    drawTrends();
  }
  function renderRangePanels() {
    renderRange();
    renderTrends();
    renderTop();
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
  var topCaveat = "按估算费用、再按请求权重排序；费用为估算金额，已含模型组系数（模型组扣费系数），占比按区间总估算费用计算；$0.00 不推断为未定价或免费。";
  function renderTopPanel(bodyID, statusID, list, kind, attribute) {
    var body = el(bodyID);
    var status = el(statusID);
    if (!S.aggregateSnapshot) {
      var loading = S.aggregateLoading && !S.aggregateError;
      body.innerHTML = '<tr><td colspan="5">' + (loading ? "正在加载费用排行…" : "费用排行不可用") + '</td></tr>';
      status.textContent = loading ? "正在加载费用排行…" : "费用排行不可用：" + (S.aggregateError || "尚未加载聚合") + "。";
      status.dataset.state = loading ? "" : "error";
      return;
    }
    var stale = S.aggregateError ? "费用排行" + aggregateStaleText() + "；" : "";
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
    var data = S.aggregateSnapshot || {};
    renderTopPanel("topClientsBody", "topClientsStatus", data.by_client, "客户端", "top-client");
    renderTopPanel("topModelsBody", "topModelsStatus", data.by_model, "模型", "top-model");
  }
  S.barButtons = Array.prototype.slice.call(document.querySelectorAll("[data-overview-bar]"));
  S.barButtons.forEach(function (button) {
    button.addEventListener("click", function () {
      var metric = button.dataset.overviewBar === "cost" ? "cost" : "requests";
      if (metric === S.barMetric) return;
      S.barMetric = metric;
      drawTrends();
    });
  });
  function focusTrend(index) {
    if (!S.trendSlots || !S.trendSlots.length) return;
    index = Math.max(0, Math.min(S.trendSlots.length - 1, index));
    var start = S.trendSlots[index].start.getTime();
    if (start !== S.trendFocus) { S.trendFocus = start; drawTrends(); }
  }
  trendIDs.forEach(function (id) {
    var chart = el(id);
    chart.addEventListener("pointermove", function (event) {
      if (!S.trendSlots || !S.trendSlots.length) return;
      var bounds = chart.getBoundingClientRect();
      var geometry = chartGeometry(chart);
      var relative = (event.clientX - bounds.left - geometry.left) / (geometry.right - geometry.left);
      focusTrend(Math.floor(Math.max(0, Math.min(0.9999, relative)) * S.trendSlots.length));
    });
    chart.addEventListener("focus", function () {
      if (S.trendFocus === null && S.trendSlots && S.trendSlots.length) focusTrend(S.trendSlots.length - 1);
    });
    chart.addEventListener("keydown", function (event) {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      if (!S.trendSlots || !S.trendSlots.length) return;
      event.preventDefault();
      var index = S.trendSlots.findIndex(function (slot) { return slot.start.getTime() === S.trendFocus; });
      if (index < 0) index = S.trendSlots.length - 1;
      index = event.key === "Home" ? 0 : event.key === "End" ? S.trendSlots.length - 1 : index + (event.key === "ArrowRight" ? 1 : -1);
      focusTrend(index);
    });
  });
  if (typeof ResizeObserver === "function") {
    var trendObserver = new ResizeObserver(function () { drawTrends(); });
    trendIDs.forEach(function (id) { trendObserver.observe(el(id)); });
  } else if (document.defaultView) {
    document.defaultView.addEventListener("resize", drawTrends);
  }
  return {renderRangePanels: renderRangePanels, renderRangeWindow: renderRangeWindow};
});
