"use strict";
// Usage owns time/filter selection, loading, status banner and tabs; sibling usage_* modules own state, filters, tables and the chart.
AdminApp.register("usage", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toLocalInput = app.bind("core", "toLocalInput");
  var inputToISO = app.bind("core", "inputToISO");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var S = app.bind("usage_state", "store")();
  var renderDailyChart = app.bind("usage_chart", "renderDailyChart");
  var setUsageChartMetric = app.bind("usage_chart", "setUsageChartMetric");
  var setUsageChartNote = app.bind("usage_chart", "setUsageChartNote");
  var appendUsageModelGroupOption = app.bind("usage_filters", "appendUsageModelGroupOption");
  var appendUsageModelGroups = app.bind("usage_filters", "appendUsageModelGroups");
  var ensureModelGroupsLoaded = app.bind("usage_filters", "ensureModelGroupsLoaded");
  var populateUsageClientFilter = app.bind("usage_filters", "populateUsageClientFilter");
  var populateUsageModelGroupFilter = app.bind("usage_filters", "populateUsageModelGroupFilter");
  var refreshUsageModelGroupFilter = app.bind("usage_filters", "refreshUsageModelGroupFilter");
  var updateUsageModelGroupFilterButton = app.bind("usage_filters", "updateUsageModelGroupFilterButton");
  var usageModelGroupValues = app.bind("usage_filters", "usageModelGroupValues");
  var getTimeRange = app.bind("usage_range", "getTimeRange");
  var usageGroupBy = app.bind("usage_range", "usageGroupBy");
  var addUsageTotals = app.bind("usage_tables", "addUsageTotals");
  var compareUsageModelGroupItems = app.bind("usage_tables", "compareUsageModelGroupItems");
  var renderAggregateTable = app.bind("usage_tables", "renderAggregateTable");
  var renderUsageTables = app.bind("usage_tables", "renderUsageTables");
  var setUsageTableRows = app.bind("usage_tables", "setUsageTableRows");
  var renderUsageModelGroups = app.bind("usage_tables", "renderUsageModelGroups");
  var renderUsageSummary = app.bind("usage_tables", "renderUsageSummary");
  var setUsageModelRequestHeader = app.bind("usage_tables", "setUsageModelRequestHeader");
  var updateUsageModelGroupSortArrows = app.bind("usage_tables", "updateUsageModelGroupSortArrows");
  var updateUsageSortArrows = app.bind("usage_tables", "updateUsageSortArrows");
  var usageCostDenominator = app.bind("usage_tables", "usageCostDenominator");
  var usageCostHtml = app.bind("usage_tables", "usageCostHtml");
  var usageCostShare = app.bind("usage_tables", "usageCostShare");
  var usageExpandButton = app.bind("usage_tables", "usageExpandButton");
  var usageModelGroupSortValue = app.bind("usage_tables", "usageModelGroupSortValue");
  var usageModelRequestHeader = app.bind("usage_tables", "usageModelRequestHeader");
  var usageNumber = app.bind("usage_tables", "usageNumber");
  var usageSortValue = app.bind("usage_tables", "usageSortValue");
  var usageSubsetHtml = app.bind("usage_tables", "usageSubsetHtml");
  var usageTotalCells = app.bind("usage_tables", "usageTotalCells");
  var usageTotals = app.bind("usage_tables", "usageTotals");
  var usageStatus = document.createElement("div");
  var usageStatusText = document.createElement("span");
  var usageRetry = document.createElement("button");
  usageStatus.setAttribute("data-usage-status", "true");
  usageStatus.style.flexBasis = "100%";
  usageStatus.style.minWidth = "0";
  usageStatus.style.order = "-1";
  usageStatus.style.padding = "8px 14px";
  usageStatus.style.borderRadius = "6px";
  usageStatus.style.background = "var(--warn-soft)";
  usageStatus.style.color = "var(--warn)";
  usageStatus.style.overflowWrap = "anywhere";
  usageStatus.hidden = true;
  usageStatusText.setAttribute("role", "status");
  usageStatusText.setAttribute("aria-live", "polite");
  usageRetry.type = "button";
  usageRetry.className = "btn btn-sm";
  usageRetry.textContent = "重试";
  usageRetry.style.marginLeft = "8px";
  usageRetry.addEventListener("click", function () {
    loadUsage();
  });
  usageStatus.appendChild(usageStatusText);
  usageStatus.appendChild(usageRetry);
  document.querySelector(".time-range-bar").appendChild(usageStatus);
  document.querySelectorAll(".time-range-btn").forEach(function (btn) {
    btn.addEventListener("click", function () {
      document.querySelectorAll(".time-range-btn").forEach(function (b) {
        b.classList.remove("active");
      });
      btn.classList.add("active");
      S.activeRange = btn.dataset.range;
      if (S.activeRange === "custom") {
        ++S.usageLoadVersion;
        showUsageStatus("请设置并应用自定义时间", false);
        document.getElementById("customRangeInputs").style.display = "flex";
        var fromInput = document.getElementById("usageFrom");
        var toInput = document.getElementById("usageTo");
        if (!fromInput.value || !toInput.value) {
          var r = getTimeRange("7d");
          fromInput.value = toLocalInput(new Date(r.from));
          toInput.value = toLocalInput(new Date(r.to));
        }
      } else {
        document.getElementById("customRangeInputs").style.display = "none";
        loadUsage();
      }
    });
  });
  document
    .getElementById("applyCustomRange")
    .addEventListener("click", function () {
      loadUsage();
    });

  async function loadUsage() {
    var version = ++S.usageLoadVersion;
    var range;
    if (S.activeRange === "custom") {
      try {
        range = { from: inputToISO("usageFrom"), to: inputToISO("usageTo") };
      } catch (e) {
        showUsageStatus("自定义时间无效", false);
        toast(e.message, "error");
        return;
      }
      if (Date.parse(range.from) > Date.parse(range.to)) {
        showUsageStatus("自定义时间无效", false);
        toast("开始时间不能晚于结束时间", "error");
        return;
      }
    } else {
      range = getTimeRange(S.activeRange);
    }
    state.usageRange = range;
    var clientFilter = document.getElementById("usageClientFilter").value;
    var modelGroupFilter = usageModelGroupValues();
    var rangeName = S.activeRange;
    var rendering = false;
    showUsageStatus("正在加载统计", false);
    try {
      await ensureModelGroupsLoaded();
      if (version !== S.usageLoadVersion) return;
      var qs = new URLSearchParams({ from: range.from, to: range.to });
      if (clientFilter) qs.set("client_name", clientFilter);
      appendUsageModelGroups(qs, modelGroupFilter);
      var groupBy = usageGroupBy(rangeName, range);
      var mainAgg = await api(
        "/admin/data/usage/aggregate?group_by=" +
          groupBy +
          "&" +
          qs.toString(),
        { timeoutMs: 60000 }, // wide ranges scan many usage events
      );
      if (version !== S.usageLoadVersion) return;
      if (!validUsageAggregate(mainAgg)) throw new Error("统计响应格式无效");
      var snapshot = {
        range: range,
        client: clientFilter,
        groups: modelGroupFilter,
        groupBy: groupBy,
        data: mainAgg,
      };
      rendering = true;
      renderUsageAggregate(snapshot, true);
      S.usageSnapshot = snapshot;
      usageStatus.hidden = true;
      document
        .getElementById("usageSummaryCards")
        .setAttribute("aria-label", "当前筛选汇总");
    } catch (e) {
      if (version !== S.usageLoadVersion) return;
      if (rendering && S.usageSnapshot) {
        try {
          renderUsageAggregate(S.usageSnapshot, false);
        } catch (restoreError) {
          S.usageSnapshot = null;
        }
      }
      showUsageStatus("统计加载失败", true);
      toast("加载统计失败: " + e.message, "error");
    }
  }

  function showUsageStatus(message, canRetry) {
    var summary = document.getElementById("usageSummaryCards");
    if (S.usageSnapshot) {
      var snapshot = S.usageSnapshot;
      message +=
        "；当前显示上次过期数据（UTC " + snapshot.range.from + " 至 " +
        snapshot.range.to + "；客户端：" + (snapshot.client || "全部") +
        "；模型组：" + (snapshot.groups.join("、") || "全部") + "）。";
      summary.setAttribute("aria-label", "上次查询汇总（过期）");
    } else {
      message += "；暂无可显示数据。";
      summary.setAttribute("aria-label", "统计不可用");
      renderUsageSummary({});
      [["usageByModelGroupBody", 8], ["usageByClientBody", 8], ["usageByModelBody", 9]].forEach(function (item) {
        document.getElementById(item[0]).innerHTML =
          "<tr class='empty-row'><td colspan='" + item[1] + "'>" + esc(message) + "</td></tr>";
      });
      document.getElementById("dailyChart").innerHTML =
        "<div style='color:var(--text-muted);padding:32px;text-align:center;width:100%;'>" +
        esc(message) + "</div>";
    }
    usageStatusText.textContent = message;
    usageRetry.hidden = !canRetry;
    usageRetry.style.display = canRetry ? "" : "none";
    usageStatus.hidden = false;
  }

  function validUsageAggregate(data) {
    if (!data || typeof data !== "object" || Array.isArray(data)) return false;
    var arrays = ["buckets", "by_client", "by_model", "by_model_group_model", "by_target_model"];
    if (
      !["totals", "buckets", "by_client", "by_model"].every(function (key) {
        return Object.prototype.hasOwnProperty.call(data, key);
      })
    ) return false;
    if (
      data.totals == null ||
      typeof data.totals !== "object" || Array.isArray(data.totals)
    ) return false;
    return arrays.every(function (key) {
      return data[key] == null ||
        (Array.isArray(data[key]) && data[key].every(function (row) {
          return row && typeof row === "object" && !Array.isArray(row);
        }));
    });
  }

  function renderUsageAggregate(snapshot, updateFilters) {
    var mainAgg = snapshot.data;
    var clientFilter = snapshot.client;
    var modelGroupFilter = snapshot.groups;
    var responseGroupBy =
      mainAgg.group_by === "hour" || mainAgg.group_by === "day"
        ? mainAgg.group_by
        : snapshot.groupBy;
    var totals = mainAgg.totals && typeof mainAgg.totals === "object"
      ? mainAgg.totals
      : {};
    if (updateFilters) {
      refreshUsageModelGroupFilter();

      /* Update client filter dropdown with clients that have data in this range */
      if (!clientFilter) {
        var activeNames = (mainAgg.by_client || []).map(function (c) {
          return c.key;
        });
        populateUsageClientFilter(activeNames);
      }
    }

    S.usageDenominator = usageCostDenominator(totals);
    renderUsageSummary(totals);

    setUsageTableRows("usageByClientBody", mainAgg.by_client || []);
    var modelRows = Array.isArray(mainAgg.by_model_group_model)
      ? mainAgg.by_model_group_model
      : [];
    var modelRequestSource = "by_model_group_model";
    if (!modelRows.length) {
      modelRows = Array.isArray(mainAgg.by_model) ? mainAgg.by_model : [];
      modelRequestSource = "by_model";
    }
    if (!modelRows.length) modelRequestSource = "none";
    setUsageTableRows("usageByModelBody", modelRows);
    setUsageModelRequestHeader(
      modelRequestSource,
      modelGroupFilter.length > 0,
    );
    renderUsageTables();
    state.usageModelGroupRows = mainAgg.by_target_model || [];
    renderUsageModelGroups(state.usageModelGroupRows, S.usageDenominator);
    S.usageChartBuckets = mainAgg.buckets || [];
    S.usageChartGroupBy = responseGroupBy;
    setUsageChartNote(responseGroupBy);
    renderDailyChart(S.usageChartBuckets, responseGroupBy, snapshot.range);
  }

  var usageTabItems = [
    {
      name: "group",
      button: document.getElementById("usageTabGroup"),
      panel: document.getElementById("usagePanelGroup"),
    },
    {
      name: "client",
      button: document.getElementById("usageTabClient"),
      panel: document.getElementById("usagePanelClient"),
    },
    {
      name: "model",
      button: document.getElementById("usageTabModel"),
      panel: document.getElementById("usagePanelModel"),
    },
  ];
  function selectUsageTab(name, focus) {
    var selected = usageTabItems.find(function (item) {
      return item.name === name;
    });
    if (!selected) return;
    S.activeUsageTab = name;
    usageTabItems.forEach(function (item) {
      var active = item === selected;
      item.button.setAttribute("aria-selected", String(active));
      item.button.tabIndex = active ? 0 : -1;
      item.button.classList.toggle("active", active);
      item.panel.hidden = !active;
    });
    if (focus) selected.button.focus();
  }
  usageTabItems.forEach(function (item) {
    item.button.addEventListener("click", function () {
      selectUsageTab(item.name, false);
    });
  });
  document
    .getElementById("usageTabList")
    .addEventListener("keydown", function (event) {
      var horizontal = ["ArrowLeft", "ArrowRight", "Home", "End"];
      if (horizontal.indexOf(event.key) < 0) return;
      event.preventDefault();
      var index = usageTabItems.findIndex(function (item) {
        return item.name === S.activeUsageTab;
      });
      if (event.key === "Home") index = 0;
      else if (event.key === "End") index = usageTabItems.length - 1;
      else
        index =
          (index + (event.key === "ArrowRight" ? 1 : -1) +
            usageTabItems.length) %
          usageTabItems.length;
      selectUsageTab(usageTabItems[index].name, true);
    });
  selectUsageTab("group", false);
  return {getTimeRange: getTimeRange, usageGroupBy: usageGroupBy, usageNumber: usageNumber, usageCostDenominator: usageCostDenominator, usageCostShare: usageCostShare, usageModelRequestHeader: usageModelRequestHeader, setUsageModelRequestHeader: setUsageModelRequestHeader, renderUsageSummary: renderUsageSummary, selectUsageTab: selectUsageTab, setUsageChartMetric: setUsageChartMetric, populateUsageClientFilter: populateUsageClientFilter, populateUsageModelGroupFilter: populateUsageModelGroupFilter, appendUsageModelGroupOption: appendUsageModelGroupOption, updateUsageModelGroupFilterButton: updateUsageModelGroupFilterButton, refreshUsageModelGroupFilter: refreshUsageModelGroupFilter, ensureModelGroupsLoaded: ensureModelGroupsLoaded, usageModelGroupValues: usageModelGroupValues, appendUsageModelGroups: appendUsageModelGroups, loadUsage: loadUsage, usageSortValue: usageSortValue, renderAggregateTable: renderAggregateTable, usageSubsetHtml: usageSubsetHtml, usageCostHtml: usageCostHtml, usageTotals: usageTotals, addUsageTotals: addUsageTotals, usageTotalCells: usageTotalCells, usageExpandButton: usageExpandButton, usageModelGroupSortValue: usageModelGroupSortValue, compareUsageModelGroupItems: compareUsageModelGroupItems, updateUsageModelGroupSortArrows: updateUsageModelGroupSortArrows, renderUsageModelGroups: renderUsageModelGroups, renderDailyChart: renderDailyChart, updateUsageSortArrows: updateUsageSortArrows};
});
