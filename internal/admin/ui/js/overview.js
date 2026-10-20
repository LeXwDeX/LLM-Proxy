"use strict";
// Overview owns the live activity window, client selection and page polling.
AdminApp.register("overview", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var fmtPct = app.bind("core", "fmtPct");
  var totalTok = app.bind("core", "totalTok");
  var api = app.bind("core", "api");
  var snapshot = null;
  var selectedClient = null;
  var focusFrom = null;
  var sortKey = "active_requests";
  var sortDirection = -1;
  var rows = new Map();
  var enabled = false;
  var generation = 0;
  var timer = null;
  var activityFlight = null;
  var historyFlight = null;
  var activityController = null;
  var historyController = null;
  var historyFetchedAt = 0;
  var historySnapshot = null;
  var requestTimeoutMs = 12000;
  var tooltipTrigger = null;
  var metricIDs = ["activityActiveRequests", "activityActiveClients", "activityWebsocketSessions", "activityTotalTokens", "activityInputTokens", "activityOutputTokens"];
  var chartIDs = ["activityConcurrencyChart", "activityTokenChart"];
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
      Promise.resolve(api(path, {signal: controller.signal})).then(resolve, reject);
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
    if (timer !== null) clearTimeout(timer);
    timer = null;
    if (activityController) activityController.abort();
    if (historyController) { historyController.abort(); historyFetchedAt = 0; }
    activityController = historyController = null;
    activityFlight = historyFlight = null;
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
    keys.forEach(function (key, keyIndex) {
      var segments = [], points = [];
      function flush() { if (points.length) { segments.push(points); points = []; } }
      series.forEach(function (bucket, index) {
        if (bucket[key] === null || bucket[key] === undefined) { flush(); return; }
        points.push({x: x(index), y: y(Number(bucket[key]))});
      });
      flush();
      var lineClass = tokenChart ? (keyIndex ? "activity-output" : "activity-input") : "activity-concurrency";
      segments.forEach(function (segment) {
        if (segment.length === 1) svg += '<circle class="' + lineClass + '" cx="' + segment[0].x + '" cy="' + segment[0].y + '" r="2.5"/>';
        else svg += '<polyline class="activity-line ' + lineClass + '" points="' + segment.map(function (point) { return point.x + "," + point.y; }).join(" ") + '"/>';
      });
    });
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
    var stats = ov.request_stats_72h;
    var available = stats && stats.available === true;
    text("activityHistoryState", available ? (stats.record_failures > 0 ? "样本可能不完整" : "") : "统计暂不可用");
    text("ovTotalReqs", available ? number(stats.total) : "不可用");
    text("ovSuccessRate", available ? (stats.success_rate == null ? "--" : fmtPct(stats.success_rate)) : "不可用");
    text("ovRequestStatsStatus", available ? (stats.total === 0 ? "暂无完成样本。" : "") + (stats.record_failures > 0 ? "警告：本进程有 " + number(stats.record_failures) + " 次统计写入失败，样本可能不完整。" : "") : "请求完成统计暂不可用，请稍后刷新。");
    text("activityHistoryTooltip", "按代理请求完成及有效 WebSocket create 轮次计数；成功率 = 成功 / 总量（含失败、取消），不是服务可用率。不含代理前鉴权/IP 拒绝、健康/管理/ping、会话控制及无效或鉴权复核拒绝消息。" + (available ? "采集开始：" + stats.since + "；统计区间：[" + stats.from + "，" + stats.to + ")。当前未结束分钟不计入。" : "请求完成统计暂不可用。"));
    text("ovTargets", number(ov.active_targets));
    text("ovClients", number(ov.client_count));
    text("ovModelCosts", number(ov.model_cost_count));
    var summary = ov.usage_summary || {};
    var periods = [{k: "last_hour", l: "近1小时"}, {k: "today", l: "今日"}, {k: "yesterday", l: "昨日"}, {k: "last_7_days", l: "近7天"}, {k: "last_30_days", l: "近30天"}];
    el("overviewUsageSummary").innerHTML = '<table class="activity-history-table"><thead><tr><th>区间</th><th>用量事件数</th><th>总 Token</th><th>历史费用</th></tr></thead><tbody>' + periods.map(function (period) {
      var data = summary[period.k] || {};
      return '<tr><th scope="row">' + esc(period.l) + '</th><td>' + esc(number(data.requests)) + '</td><td>' + esc(number(totalTok(data))) + '</td><td>' + esc(fmtMoney(data.estimated_cost)) + '</td></tr>';
    }).join("") + '</tbody></table>';
    if (tooltipTrigger && tooltipTrigger.dataset.activityInfoSource === "activityHistoryTooltip") showTooltip(tooltipTrigger);
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
          historySnapshot = data || {};
          renderHistory(historySnapshot);
        }
      } catch (error) {
        if (epoch !== generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        historyFetchedAt = 0;
        var reason = error.name === "TimeoutError" ? "加载超时" : "加载失败";
        text("activityHistoryState", reason);
        text("ovRequestStatsStatus", "历史统计" + reason + (historySnapshot ? "，显示上次数据；" : "，") + "请点击刷新重试。");
        if (!historySnapshot) {
          ["ovTotalReqs", "ovSuccessRate", "ovTargets", "ovClients", "ovModelCosts"].forEach(function (id) { text(id, "不可用"); });
          el("overviewUsageSummary").innerHTML = '<p class="activity-history-error">历史费用' + reason + '，请点击刷新重试。</p>';
        }
      } finally {
        if (epoch === generation) { historyFlight = null; historyController = null; }
      }
    })();
    historyFlight = flight;
    return flight;
  }
  function loadOverview(forceHistory) {
    return Promise.all([loadActivity(), loadHistory(forceHistory === true)]);
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
  return {loadOverview: loadOverview, startOverviewAutoRefresh: startOverviewAutoRefresh, stopOverviewAutoRefresh: stopOverviewAutoRefresh};
});
