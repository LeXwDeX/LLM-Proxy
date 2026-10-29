"use strict";
// Overview live activity: five-minute activity snapshot, client table and activity charts.
AdminApp.register("overviewActivity", function (app) {
  var esc = app.bind("core", "esc");
  var S = app.bind("overviewState", "store")();
  var renderChain = app.bind("overviewHealth", "renderChain");
  var request = app.bind("overview", "request");
  var schedule = app.bind("overview", "schedule");
  var barRect = app.bind("overviewUtil", "barRect");
  var barWidth = app.bind("overviewUtil", "barWidth");
  var chartAxis = app.bind("overviewUtil", "chartAxis");
  var chartGeometry = app.bind("overviewUtil", "chartGeometry");
  var clock = app.bind("overviewUtil", "clock");
  var compactNumber = app.bind("overviewUtil", "compactNumber");
  var compareTime = app.bind("overviewUtil", "compareTime");
  var counter = app.bind("overviewUtil", "counter");
  var duration = app.bind("overviewUtil", "duration");
  var el = app.bind("overviewUtil", "el");
  var number = app.bind("overviewUtil", "number");
  var stackedBars = app.bind("overviewUtil", "stackedBars");
  var text = app.bind("overviewUtil", "text");
  var visible = app.bind("overviewUtil", "visible");
  var rows = new Map();
  var metricIDs = ["activityActiveRequests", "activityActiveClients", "activityWebsocketSessions", "activityTotalTokens", "activityInputTokens", "activityOutputTokens"];
  var chartIDs = ["activityConcurrencyChart", "activityTokenChart"];
  var emptyRow = document.createElement("tr");
  var emptyCell = document.createElement("td");
  emptyCell.colSpan = 6;
  emptyRow.appendChild(emptyCell);
  emptyCell.textContent = "正在加载客户端活动…";
  document.getElementById("activityClientsBody").innerHTML = "";
  document.getElementById("activityClientsBody").appendChild(emptyRow);
  function reportActivityStatus(error) {
    var status = el("activityStatus");
    if (error) {
      var errorMessage = error.name === "TimeoutError"
        ? (S.snapshot ? "更新超时，显示上次数据；可点击刷新重试。" : "实时活动加载超时，请点击刷新重试。")
        : (S.snapshot ? "更新失败，显示上次数据；可点击刷新重试。" : "实时活动暂不可用，请点击刷新重试。");
      if (status.textContent !== errorMessage) status.textContent = errorMessage;
      status.dataset.state = "error";
      return;
    }
    var warnings = [];
    if (Date.now() - Date.parse(S.snapshot.generated_at) > 15000) warnings.push("数据更新延迟");
    if (!S.snapshot.runtime.complete) warnings.push("采样不足");
    if (S.snapshot.series.some(function (bucket) { return bucket.partial; })) warnings.push("部分时间桶不足10秒");
    if (S.snapshot.totals.incomplete_usage_count > 0) warnings.push(number(S.snapshot.totals.incomplete_usage_count) + " 条用量不完整");
    var message = warnings.length ? warnings.join(" · ") : (S.snapshot.runtime.active_requests === 0 && S.snapshot.totals.total_tokens === 0 ? "当前无在途请求，近5分钟暂无已记录用量。" : "");
    if (status.textContent !== message) status.textContent = message;
    status.dataset.state = warnings.length ? "partial" : "ready";
  }
  function scheduleActivityFreshness() {
    if (S.activityFreshnessTimer !== null) clearTimeout(S.activityFreshnessTimer);
    S.activityFreshnessTimer = null;
    if (!S.snapshot || !visible()) return;
    var delay = Date.parse(S.snapshot.generated_at) + 15001 - Date.now();
    if (delay <= 0) return;
    S.activityFreshnessTimer = setTimeout(function () {
      S.activityFreshnessTimer = null;
      if (!S.snapshot || !visible()) return;
      if (!S.activityError) reportActivityStatus();
      renderChain();
      scheduleActivityFreshness();
    }, Math.min(delay, 2147483647));
  }
  function renderSnapshot(data) {
    S.snapshot = data;
    S.activityError = "";
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
    scheduleActivityFreshness();
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
  function loadActivity(background) {
    if (!visible()) return Promise.resolve();
    if (S.activityFlight) return S.activityFlight;
    if (S.snapshot) {
      if (!S.activityError) reportActivityStatus();
      renderChain();
      scheduleActivityFreshness();
    }
    var epoch = S.generation;
    var controller = new AbortController();
    S.activityController = controller;
    el("refreshOverviewBtn").disabled = true;
    el("page-overview").setAttribute("aria-busy", "true");
    if (!S.snapshot) text("activityStatus", "正在加载实时活动…");
    var flight = (async function () {
      try {
        var data = await request("/admin/api/activity", controller, undefined, background);
        if (epoch !== S.generation || controller.signal.aborted || !visible()) return;
        if (!validSnapshot(data)) throw new Error("实时活动数据格式无效");
        // A response from an older sample must never rewind the displayed window.
        var sampleOrder = S.snapshot && data.runtime.since === S.snapshot.runtime.since ? compareTime(data.generated_at, S.snapshot.generated_at) : 1;
        if (sampleOrder < 0 || (sampleOrder === 0 && S.activityError)) {
          var status = el("activityStatus");
          var message = "采样时间未更新，显示上次数据。请稍后刷新。";
          if (status.textContent !== message) status.textContent = message;
          status.dataset.state = "partial";
          S.activityError = "实时活动" + message;
          renderChain();
          return;
        }
        S.pollFailures = 0;
        renderSnapshot(data);
      } catch (error) {
        if (epoch !== S.generation || !visible() || (controller.signal.aborted && error.name !== "TimeoutError")) return;
        if (!S.snapshot) {
          metricIDs.forEach(function (id) { text(id, "不可用"); });
          text("activityUpdatedAt", "尚无成功采样");
          chartIDs.forEach(function (id) { el(id).innerHTML = ""; });
          text("activityConcurrencyReadout", "图表暂不可用");
          text("activityTokenReadout", "图表暂不可用");
          emptyCell.textContent = "客户端活动暂不可用";
          el("activityClientsBody").appendChild(emptyRow);
        }
        S.pollFailures++;
        reportActivityStatus(error);
        S.activityError = (S.snapshot ? "实时活动" : "") + el("activityStatus").textContent;
        renderChain();
      } finally {
        if (epoch === S.generation) {
          S.activityFlight = null;
          S.activityController = null;
          el("refreshOverviewBtn").disabled = false;
          el("page-overview").setAttribute("aria-busy", "false");
          schedule();
        }
      }
    })();
    S.activityFlight = flight;
    return flight;
  }
  function clientValue(client, key) {
    return key === "name" ? client.name : (key === "active_requests" || key === "websocket_sessions" ? client[key] : client.totals[key]);
  }
  function selectClient(name) {
    if (S.selectedClient === name) return;
    S.selectedClient = name;
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
    button.addEventListener("click", function () { selectClient(S.selectedClient === name ? null : name); });
    cells[0].appendChild(button);
    var label = document.createElement("span");
    label.className = "activity-unconfigured";
    label.textContent = "未配置";
    cells[0].appendChild(label);
    row.addEventListener("click", function (event) {
      if (event.target !== button) selectClient(S.selectedClient === name ? null : name);
    });
    return {node: row, cells: cells, button: button, label: label};
  }
  function renderClients() {
    if (!S.snapshot) return;
    var filter = el("activityClientSearch").value.trim().toLocaleLowerCase();
    var list = S.snapshot.clients.filter(function (client) { return client.name.toLocaleLowerCase().includes(filter); });
    list.sort(function (a, b) {
      var result = S.sortKey === "name" ? a.name.localeCompare(b.name, "zh-CN") : Number(clientValue(a, S.sortKey)) - Number(clientValue(b, S.sortKey));
      return result * S.sortDirection || a.name.localeCompare(b.name, "zh-CN");
    });
    var body = el("activityClientsBody");
    var names = new Set(list.map(function (client) { return client.name; }));
    var configuredNames = new Set(S.snapshot.clients.map(function (client) { return client.name; }));
    var focused = document.activeElement;
    rows.forEach(function (row, name) {
      if (!names.has(name) && row.node.parentNode === body) body.removeChild(row.node);
      if (!configuredNames.has(name)) rows.delete(name);
    });
    if (emptyRow.parentNode === body) body.removeChild(emptyRow);
    list.forEach(function (client, index) {
      var row = rows.get(client.name);
      if (!row) { row = makeRow(client.name); rows.set(client.name, row); }
      row.node.dataset.selected = String(client.name === S.selectedClient);
      row.button.setAttribute("aria-pressed", String(client.name === S.selectedClient));
      row.button.setAttribute("aria-label", "查看 " + client.name + " 的近5分钟图表");
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
    text("activityClientCount", list.length + " / " + S.snapshot.clients.length + " 个客户端");
    document.querySelectorAll("[data-activity-sort]").forEach(function (button) {
      var selected = button.dataset.activitySort === S.sortKey;
      button.setAttribute("aria-label", button.dataset.label + (selected ? (S.sortDirection === 1 ? "，当前升序，点击降序" : "，当前降序，点击升序") : "，点击排序"));
      var header = button.closest("th");
      if (header) header.setAttribute("aria-sort", selected ? (S.sortDirection === 1 ? "ascending" : "descending") : "none");
      button.dataset.direction = selected ? (S.sortDirection === 1 ? "asc" : "desc") : "none";
      var icon = button.querySelector("path");
      if (icon) icon.setAttribute("d", selected ? (S.sortDirection === 1 ? "m5 10 3-3 3 3" : "m5 6 3 3 3-3") : "m5 6 3-3 3 3M5 10l3 3 3-3");
    });
  }
  function currentSeries() {
    if (!S.snapshot) return [];
    if (S.selectedClient === null) return S.snapshot.series;
    var client = S.snapshot.clients.find(function (candidate) { return candidate.name === S.selectedClient; });
    return client ? client.series : [];
  }
  function chartHTML(series, tokenChart, chart) {
    var geometry = chartGeometry(chart);
    chart.setAttribute("viewBox", "0 0 " + geometry.width + " " + geometry.height);
    var left = geometry.left, right = geometry.right, top = geometry.top, bottom = geometry.bottom;
    var windowFrom = Date.parse(S.snapshot.window.from), windowTo = Date.parse(S.snapshot.window.to);
    function timeX(time) { return left + (time - windowFrom) / (windowTo - windowFrom) * (right - left); }
    var keys = tokenChart ? ["input_tokens", "output_tokens"] : ["active_requests_peak"];
    var axis = chartAxis(Math.max.apply(null, [0].concat(series.map(function (bucket) { return keys.reduce(function (sum, key) { return sum + (bucket[key] == null ? 0 : Number(bucket[key])); }, 0); }))));
    var maximum = axis.maximum;
    var slotLeft = function (index) { return timeX(Math.max(windowFrom, Date.parse(series[index].from))); };
    var slotRight = function (index) { return timeX(Math.min(windowTo, Date.parse(series[index].to))); };
    var y = function (value) { return bottom - value / maximum * (bottom - top); };
    var svg = "";
    for (var i = 0; i <= axis.ticks; i++) {
      var value = axis.step * i;
      svg += '<line class="activity-gridline" x1="' + left + '" x2="' + right + '" y1="' + y(value) + '" y2="' + y(value) + '"/><text class="activity-axis" x="34" y="' + (y(value) + 4) + '" text-anchor="end">' + esc(compactNumber(value)) + '</text>';
    }
    [0, 0.5, 1].forEach(function (ratio) {
      var time = Date.parse(S.snapshot.window.from) + ratio * (Date.parse(S.snapshot.window.to) - Date.parse(S.snapshot.window.from));
      svg += '<text class="activity-axis" x="' + (left + ratio * (right - left)) + '" y="' + (geometry.height - 12) + '" text-anchor="' + (ratio === 0 ? "start" : ratio === 1 ? "end" : "middle") + '">' + esc(clock(time)) + '</text>';
    });
    var index = series.findIndex(function (bucket) { return bucket.from === S.focusFrom; });
    if (index >= 0) {
      svg = '<rect class="overview-trend-focus" x="' + slotLeft(index) + '" y="' + top + '" width="' + Math.max(0, slotRight(index) - slotLeft(index)) + '" height="' + (bottom - top) + '"/>' + svg;
    }
    // The window starts and ends mid-bucket, so edge buckets are clipped; each bar
    // is sized from its own visible span to stay inside it instead of sharing one
    // width that would run into the neighbouring bar.
    series.forEach(function (bucket, index) {
      var span = Math.max(0, slotRight(index) - slotLeft(index)), width = Math.min(barWidth(span), span), barLeft = slotLeft(index) + (span - width) / 2;
      if (tokenChart) svg += stackedBars(barLeft, width, keys.map(function (key) { return bucket[key] == null ? 0 : Number(bucket[key]); }), y);
      else if (bucket.active_requests_peak != null) svg += barRect("activity-concurrency-bar", barLeft, width, 0, Number(bucket.active_requests_peak), y);
    });
    return svg;
  }
  function renderCharts() {
    if (!S.snapshot) return;
    text("activityChartScope", S.selectedClient === null ? "全站" : S.selectedClient);
    el("activityResetScope").hidden = S.selectedClient === null;
    var series = currentSeries();
    var missing = S.selectedClient !== null && !S.snapshot.clients.some(function (client) { return client.name === S.selectedClient; });
    text("activityScopeStatus", missing ? "该客户端已离开近5分钟窗口，可返回全站。" : "");
    if (S.focusFrom && !series.some(function (bucket) { return bucket.from === S.focusFrom; })) S.focusFrom = series.length ? series[0].from : null;
    chartIDs.forEach(function (id, index) {
      el(id).innerHTML = series.length ? chartHTML(series, index === 1, el(id)) : "";
      el(id).setAttribute("aria-label", (S.selectedClient === null ? "全站" : S.selectedClient) + (index ? "输入与输出 Token，近5分钟，每10秒；方向键查看读数" : "并发峰值，近5分钟，每10秒；方向键查看读数"));
    });
    var bucket = series.find(function (candidate) { return candidate.from === S.focusFrom; });
    var prefix = bucket ? clock(bucket.from) + "–" + clock(bucket.to) + (bucket.partial ? " · 部分桶" : "") + " · " : "";
    text("activityConcurrencyReadout", bucket ? prefix + (bucket.active_requests_peak === null ? "峰值未采样" : "峰值 " + number(bucket.active_requests_peak) + " 个请求") : (series.length ? "移入图表或聚焦后使用方向键查看读数" : "暂无图表数据"));
    text("activityTokenReadout", bucket ? prefix + "输入 " + number(bucket.input_tokens) + " · 输出 " + number(bucket.output_tokens) + (bucket.incomplete_usage_count > 0 ? " · 用量不完整" : "") : (series.length ? "移入图表或聚焦后使用方向键查看读数" : "暂无图表数据"));
  }
  chartIDs.forEach(function (id) {
    var chart = el(id);
    chart.addEventListener("pointermove", function (event) {
      var series = currentSeries();
      if (!series.length) return;
      var bounds = chart.getBoundingClientRect();
      var geometry = chartGeometry(chart);
      var relative = (event.clientX - bounds.left - geometry.left) / (geometry.right - geometry.left);
      var time = Date.parse(S.snapshot.window.from) + Math.max(0, Math.min(1, relative)) * (Date.parse(S.snapshot.window.to) - Date.parse(S.snapshot.window.from));
      var index = series.findIndex(function (bucket) { return time >= Date.parse(bucket.from) && time < Date.parse(bucket.to); });
      if (index < 0) index = time < Date.parse(series[0].from) ? 0 : series.length - 1;
      if (S.focusFrom !== series[index].from) { S.focusFrom = series[index].from; renderCharts(); }
    });
    chart.addEventListener("focus", function () {
      var series = currentSeries();
      if (!S.focusFrom && series.length) { S.focusFrom = series[series.length - 1].from; renderCharts(); }
    });
    chart.addEventListener("keydown", function (event) {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      var series = currentSeries();
      if (!series.length) return;
      event.preventDefault();
      var index = series.findIndex(function (bucket) { return bucket.from === S.focusFrom; });
      if (index < 0) index = series.length - 1;
      index = event.key === "Home" ? 0 : event.key === "End" ? series.length - 1 : Math.max(0, Math.min(series.length - 1, index + (event.key === "ArrowRight" ? 1 : -1)));
      S.focusFrom = series[index].from;
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
      S.sortDirection = S.sortKey === button.dataset.activitySort ? -S.sortDirection : (button.dataset.activitySort === "name" ? 1 : -1);
      S.sortKey = button.dataset.activitySort;
      renderClients();
    });
  });
  return {loadActivity: loadActivity};
});
