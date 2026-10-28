"use strict";
// Usage owns time/filter selection, aggregate tables and daily charts.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("usage", function (app) {
  var state = app.state;
  state.usageModelGroupExpanded = usageSafeMap(state.usageModelGroupExpanded);
  state.usageModelTargetExpanded = usageSafeMap(state.usageModelTargetExpanded);
  function usageSafeMap(source) {
    var safe = Object.create(null);
    if (source && typeof source === "object")
      Object.keys(source).forEach(function (key) {
        safe[key] = source[key];
      });
    return safe;
  }
  var modelGroupLabel = app.bind("quota", "modelGroupLabel");
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var toLocalInput = app.bind("core", "toLocalInput");
  var inputToISO = app.bind("core", "inputToISO");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  /* ===== Usage / Stats ===== */
  var usageChartMetric = "tokens";
  var usageChartBuckets = [];
  var usageChartGroupBy = "day";
  var usageLoadVersion = 0;
  var usageSnapshot = null;
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
  var usageDenominator = { total: null, usageIncomplete: false, costIncomplete: false };
  var usageTokenFields = [
    "input_tokens",
    "output_tokens",
    "cached_tokens",
    "cache_creation_tokens",
    "tool_use_prompt_tokens",
  ];
  function getTimeRange(range) {
    var now = new Date();
    var to = now;
    var from;
    switch (range) {
      case "1h":
        from = new Date(now.getTime() - 3600 * 1000);
        break;
      case "today":
        from = new Date(now.getFullYear(), now.getMonth(), now.getDate());
        break;
      case "yesterday":
        from = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
        to = new Date(now.getFullYear(), now.getMonth(), now.getDate());
        break;
      case "7d":
        from = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
        from.setHours(0, 0, 0, 0);
        break;
      case "30d":
        from = new Date(now.getTime() - 30 * 24 * 3600 * 1000);
        from.setHours(0, 0, 0, 0);
        break;
      default:
        from = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
    }
    return { from: from.toISOString(), to: to.toISOString() };
  }

  function usageGroupBy(rangeName, range) {
    if (rangeName === "7d" || rangeName === "30d") return "day";
    if (rangeName === "custom") {
      var from = Date.parse(range.from);
      var to = Date.parse(range.to);
      return to - from <= 48 * 3600 * 1000 ? "hour" : "day";
    }
    return "hour";
  }

  var activeRange = "today";
  document.querySelectorAll(".time-range-btn").forEach(function (btn) {
    btn.addEventListener("click", function () {
      document.querySelectorAll(".time-range-btn").forEach(function (b) {
        b.classList.remove("active");
      });
      btn.classList.add("active");
      activeRange = btn.dataset.range;
      if (activeRange === "custom") {
        ++usageLoadVersion;
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

  function populateUsageClientFilter(activeClients) {
    var sel = document.getElementById("usageClientFilter");
    var current = sel.value;
    sel.innerHTML = '<option value="">全部客户端</option>';
    var list = activeClients || [];
    list
      .slice()
      .sort(function (a, b) {
        return String(a).localeCompare(String(b));
      })
      .forEach(function (name) {
        var opt = document.createElement("option");
        opt.value = name;
        opt.textContent = name;
        sel.appendChild(opt);
      });
    if (current && list.indexOf(current) >= 0) {
      sel.value = current;
    } else {
      sel.value = "";
    }
  }
  function populateUsageModelGroupFilter(groups) {
    var menu = document.getElementById("usageModelGroupFilterMenu");
    var selected = usageModelGroupValues();
    var names = (groups || [])
      .map(function (g) {
        return typeof g === "string" ? g : g.name;
      })
      .filter(Boolean);
    names = names
      .filter(function (name, i, all) {
        return all.indexOf(name) === i;
      })
      .sort(function (a, b) {
        return String(a).localeCompare(String(b));
      });
    selected = selected.filter(function (name) {
      return names.indexOf(name) >= 0;
    });
    menu.innerHTML = "";
    appendUsageModelGroupOption(menu, "", "全部模型组", selected.length === 0);
    names.forEach(function (name) {
      appendUsageModelGroupOption(
        menu,
        name,
        modelGroupLabel(name),
        selected.indexOf(name) >= 0,
      );
    });
    updateUsageModelGroupFilterButton();
  }
  function appendUsageModelGroupOption(menu, value, label, checked) {
    var wrapper = document.createElement("label");
    wrapper.className = "usage-model-group-option";
    var input = document.createElement("input");
    input.type = "checkbox";
    input.value = value;
    input.checked = checked;
    input.setAttribute("data-model-group-value", value);
    wrapper.appendChild(input);
    var text = document.createElement("span");
    text.textContent = label;
    wrapper.appendChild(text);
    menu.appendChild(wrapper);
  }
  function updateUsageModelGroupFilterButton() {
    var button = document.getElementById("usageModelGroupFilterButton");
    var groups = usageModelGroupValues();
    button.textContent =
      groups.length === 0
        ? "全部模型组"
        : groups.length === 1
          ? modelGroupLabel(groups[0])
          : "已选 " + groups.length + " 个模型组";
  }
  function refreshUsageModelGroupFilter() {
    populateUsageModelGroupFilter(state.modelGroups || []);
  }
  function ensureModelGroupsLoaded() {
    if (state.modelGroupsLoaded) return Promise.resolve(state.modelGroups);
    if (state.modelGroupsPromise) return state.modelGroupsPromise;
    state.modelGroupsPromise = api("/admin/data/model-groups")
      .then(function (data) {
        if (state.modelGroupsLoaded) return state.modelGroups;
        state.modelGroups = Array.isArray(data.model_groups)
          ? data.model_groups
          : [];
        state.modelGroupsLoaded = true;
        refreshUsageModelGroupFilter();
        return state.modelGroups;
      })
      .catch(function (error) {
        if (state.modelGroupsLoaded) return state.modelGroups;
        throw error;
      })
      .finally(function () {
        state.modelGroupsPromise = null;
      });
    return state.modelGroupsPromise;
  }
  function usageModelGroupValues() {
    return Array.prototype.slice
      .call(
        document.querySelectorAll(
          "#usageModelGroupFilterMenu input[type=checkbox][data-model-group-value]",
        ),
      )
      .filter(function (input) {
        return input.checked && input.value;
      })
      .map(function (input) {
        return input.value;
      });
  }
  function appendUsageModelGroups(qs, groups) {
    groups.forEach(function (name) {
      qs.append("model_group", name);
    });
  }
  document
    .getElementById("usageClientFilter")
    .addEventListener("change", function () {
      loadUsage();
    });
  document
    .getElementById("usageModelGroupFilterButton")
    .addEventListener("click", function () {
      var menu = document.getElementById("usageModelGroupFilterMenu");
      var open = menu.hidden;
      menu.hidden = !open;
      this.setAttribute("aria-expanded", String(open));
      if (open) {
        var first = menu.querySelector("input");
        if (first) first.focus();
      }
    });
  document
    .getElementById("usageModelGroupFilterMenu")
    .addEventListener("change", function (event) {
      var input = event.target;
      if (!input || input.type !== "checkbox") return;
      var inputs = Array.prototype.slice.call(
        this.querySelectorAll("input[type=checkbox]"),
      );
      if (!input.value && input.checked) {
        inputs.forEach(function (option) {
          option.checked = option === input;
        });
      } else if (input.value && input.checked) {
        inputs[0].checked = false;
      }
      if (!usageModelGroupValues().length) {
        inputs[0].checked = true;
      }
      updateUsageModelGroupFilterButton();
      loadUsage();
    });
  document.addEventListener("click", function (event) {
    var filter = document.getElementById("usageModelGroupFilter");
    if (filter && !filter.contains(event.target)) {
      var menu = document.getElementById("usageModelGroupFilterMenu");
      var button = document.getElementById("usageModelGroupFilterButton");
      menu.hidden = true;
      button.setAttribute("aria-expanded", "false");
    }
  });
  document
    .getElementById("usageModelGroupFilter")
    .addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        var menu = document.getElementById("usageModelGroupFilterMenu");
        var button = document.getElementById("usageModelGroupFilterButton");
        menu.hidden = true;
        button.setAttribute("aria-expanded", "false");
        button.focus();
      }
    });

  async function loadUsage() {
    var version = ++usageLoadVersion;
    var range;
    if (activeRange === "custom") {
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
      range = getTimeRange(activeRange);
    }
    state.usageRange = range;
    var clientFilter = document.getElementById("usageClientFilter").value;
    var modelGroupFilter = usageModelGroupValues();
    var rangeName = activeRange;
    var rendering = false;
    showUsageStatus("正在加载统计", false);
    try {
      await ensureModelGroupsLoaded();
      if (version !== usageLoadVersion) return;
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
      if (version !== usageLoadVersion) return;
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
      usageSnapshot = snapshot;
      usageStatus.hidden = true;
      document
        .getElementById("usageSummaryCards")
        .setAttribute("aria-label", "当前筛选汇总");
    } catch (e) {
      if (version !== usageLoadVersion) return;
      if (rendering && usageSnapshot) {
        try {
          renderUsageAggregate(usageSnapshot, false);
        } catch (restoreError) {
          usageSnapshot = null;
        }
      }
      showUsageStatus("统计加载失败", true);
      toast("加载统计失败: " + e.message, "error");
    }
  }

  function showUsageStatus(message, canRetry) {
    var summary = document.getElementById("usageSummaryCards");
    if (usageSnapshot) {
      var snapshot = usageSnapshot;
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

    usageDenominator = usageCostDenominator(totals);
    renderUsageSummary(totals);

    usageTableMap.usageByClientBody.rows = mainAgg.by_client || [];
    var modelRows = Array.isArray(mainAgg.by_model_group_model)
      ? mainAgg.by_model_group_model
      : [];
    var modelRequestSource = "by_model_group_model";
    if (!modelRows.length) {
      modelRows = Array.isArray(mainAgg.by_model) ? mainAgg.by_model : [];
      modelRequestSource = "by_model";
    }
    if (!modelRows.length) modelRequestSource = "none";
    usageTableMap.usageByModelBody.rows = modelRows;
    setUsageModelRequestHeader(
      modelRequestSource,
      modelGroupFilter.length > 0,
    );
    usageTables.forEach(renderAggregateTable);
    state.usageModelGroupRows = mainAgg.by_target_model || [];
    renderUsageModelGroups(state.usageModelGroupRows, usageDenominator);
    usageChartBuckets = mainAgg.buckets || [];
    usageChartGroupBy = responseGroupBy;
    setUsageChartNote(responseGroupBy);
    renderDailyChart(usageChartBuckets, responseGroupBy);
  }

  var usageTables = [
    {
      table: "usageByClientTable",
      tbody: "usageByClientBody",
      rows: [],
      columnCount: 8,
      sort: { key: "cost", dir: -1 },
    },
    {
      table: "usageByModelTable",
      tbody: "usageByModelBody",
      rows: [],
      columnCount: 9,
      sort: { key: "model_group", dir: 1 },
    },
  ];
  var usageModelGroupSort = { key: "total", dir: -1 };
  var usageTableMap = {};
  usageTables.forEach(function (t) {
    usageTableMap[t.tbody] = t;
  });

  function usageSortValue(r, key) {
    if (key === "key")
      return String(
        r.key || (r.model_group ? r.model_group + " / " + (r.model || "") : ""),
      );
    if (key === "model_group")
      return String(modelGroupLabel(r.model_group || "-"));
    if (key === "model") return String(r.model || r.key || "");
    if (key === "total") return totalTok(r);
    if (key === "cost") return Number(r.estimated_cost || 0);
    return Number(r[key] || 0);
  }

  function renderAggregateTable(t) {
    var tb = document.getElementById(t.tbody);
    if (!t.rows.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='" +
        (t.columnCount || 7) +
        "'>当前时间段无数据</td></tr>";
      return;
    }
    tb.innerHTML = t.rows
      .slice()
      .sort(function (a, b) {
        var va = usageSortValue(a, t.sort.key),
          vb = usageSortValue(b, t.sort.key);
        var cmp =
          ["key", "model_group", "model"].indexOf(t.sort.key) >= 0
            ? va.localeCompare(vb)
            : va - vb;
        if (cmp !== 0) return cmp * t.sort.dir;
        return totalTok(b) - totalTok(a);
      })
      .map(function (r) {
        var group = r.model_group || "-";
        var model = r.model || r.key || r.bucket_start || "unknown";
        var identity =
          t.tbody === "usageByModelBody"
            ? "<td>" +
              esc(modelGroupLabel(group)) +
              "</td><td>" +
              esc(model) +
              "</td>"
            : "<td>" + esc(r.key || r.bucket_start || "unknown") + "</td>";
        return (
          "<tr>" +
          identity +
          usageTotalCells(r, usageDenominator) +
          "</tr>"
        );
      })
      .join("");
  }

  function usageNumber(value) {
    return typeof value === "number" && isFinite(value) ? value : null;
  }
  function usageCell(value) {
    var numeric = usageNumber(value);
    return numeric === null ? "不可用" : fmtNum(numeric);
  }
  function usageTokensAvailable(totals) {
    return usageTokenFields.some(function (key) {
      return usageNumber(totals[key]) !== null;
    });
  }
  function usageTotalHtml(totals) {
    return usageTokensAvailable(totals) ? fmtNum(totalTok(totals)) : "不可用";
  }
  function usageCostDenominator(totals) {
    var usageIncomplete =
      (usageNumber(totals && totals.incomplete_usage_count) || 0) > 0;
    var costIncomplete =
      (usageNumber(totals && totals.incomplete_cost_count) || 0) > 0;
    return {
      total: usageNumber(totals && totals.estimated_cost),
      usageIncomplete: usageIncomplete,
      costIncomplete: costIncomplete,
      incomplete: usageIncomplete || costIncomplete,
    };
  }
  function usageCostShare(cost, denominator) {
    if (cost === null) return "—";
    if (!denominator || denominator.total === null) return "占比不可用";
    if (denominator.usageIncomplete && denominator.costIncomplete)
      return "占比不可用（用量和费用不完整）";
    if (denominator.usageIncomplete) return "占比不可用（用量不完整）";
    if (denominator.costIncomplete) return "占比不可用（费用不完整）";
    if (denominator.total <= 0) return "占比不可用（总费用为 0）";
    return "占比 " + ((cost / denominator.total) * 100).toFixed(1) + "%";
  }
  function renderUsageSummary(totals) {
    totals = totals || {};
    document.getElementById("usReqs").textContent = usageCell(totals.requests);
    document.getElementById("usInput").textContent = usageCell(totals.input_tokens);
    document.getElementById("usOutput").textContent = usageCell(totals.output_tokens);
    document.getElementById("usCached").textContent = usageCell(totals.cached_tokens);
    document.getElementById("usCacheWrite").textContent = usageCell(
      totals.cache_creation_tokens,
    );
    document.getElementById("usTotal").textContent = usageTotalHtml(totals);
    document.getElementById("usCost").innerHTML = usageCostHtml(totals);
  }
  function usageSubsetHtml(total, subset, label) {
    // Keep a consistent token slot; CSS hides absent details and centers the main value.
    var totalValue = usageNumber(total);
    var subsetValue = usageNumber(subset);
    return (
      "<span class='usage-token-value'><span>" +
      (totalValue === null ? "不可用" : fmtNum(totalValue)) +
      "</span><small class='usage-token-detail'>" +
      (subsetValue ? "其中" + esc(label) + " " + fmtNum(subsetValue) : "") +
      "</small></span>"
    );
  }
  function usageModelRequestHeader(source, filtered) {
    if (source === "by_model_group_model" || (source === "by_model" && filtered))
      return {
        label: "请求（成功）",
        title: "成功请求权重（状态码 1–499），不是记录数",
      };
    if (source === "by_model")
      return {
        label: "请求权重",
        title:
          "请求权重；模型组明细为空时回退到按模型汇总，未选模型组，含全部请求",
      };
    return {
      label: "请求权重",
      title: "请求权重；当前无模型明细数据",
    };
  }
  function setUsageModelRequestHeader(source, filtered) {
    var th = document.getElementById("usageModelRequestHeader");
    if (!th) return;
    var header = usageModelRequestHeader(source, filtered);
    th.textContent = header.label;
    th.title = header.title;
  }
  function usageCostHtml(r, denominator) {
    var cost = usageNumber(r.estimated_cost);
    var label = cost === null ? "不可用" : fmtMoney(cost);
    var incompleteUsage = Number(r.incomplete_usage_count || 0);
    var incompleteCost = Number(r.incomplete_cost_count || 0);
    var details = [];
    if (incompleteUsage)
      details.push("用量不完整 " + incompleteUsage + " 条记录");
    if (incompleteCost)
      details.push("费用不完整 " + incompleteCost + " 条记录");
    if (details.length)
      label +=
        " <span title='" +
        esc(details.join("；") + "（按记录计数，一个请求可对应多条记录）") +
        "'>（部分）</span>";
    if (denominator)
      label +=
        "<small class='usage-cost-share'>" +
        esc(
          usageCostShare(cost, {
            total: denominator.total,
            usageIncomplete:
              !!denominator.usageIncomplete || incompleteUsage > 0,
            costIncomplete: !!denominator.costIncomplete || incompleteCost > 0,
          }),
        ) +
        "</small>";
    return label;
  }
  function usageTotals() {
    return {
      requests: 0,
      input_tokens: 0,
      output_tokens: 0,
      cached_tokens: 0,
      cache_creation_tokens: 0,
      tool_use_prompt_tokens: 0,
      estimated_cost: 0,
      incomplete_usage_count: 0,
      incomplete_cost_count: 0,
      reasoning_tokens: 0,
      explicit_cache_read_tokens: 0,
    };
  }
  function addUsageTotals(target, source) {
    [
      "requests",
      "input_tokens",
      "output_tokens",
      "cached_tokens",
      "cache_creation_tokens",
      "tool_use_prompt_tokens",
      "incomplete_usage_count",
      "incomplete_cost_count",
      "reasoning_tokens",
      "explicit_cache_read_tokens",
    ].forEach(function (key) {
      target[key] += Number(source[key] || 0);
    });
    var cost = usageNumber(source.estimated_cost);
    var previousCost = usageNumber(target.estimated_cost);
    target.estimated_cost =
      previousCost === null || cost === null ? null : previousCost + cost;
  }
  function usageTotalCells(totals, denominator) {
    return (
      "<td>" +
      usageCell(totals.requests) +
      "</td>" +
      "<td>" +
      usageCell(totals.input_tokens) +
      "</td>" +
      "<td>" +
      usageSubsetHtml(totals.output_tokens, totals.reasoning_tokens, "推理") +
      "</td>" +
      "<td>" +
      usageSubsetHtml(
        totals.cached_tokens,
        totals.explicit_cache_read_tokens,
        "显式",
      ) +
      "</td>" +
      "<td>" +
      usageCell(totals.cache_creation_tokens) +
      "</td>" +
      "<td>" +
      usageTotalHtml(totals) +
      "</td>" +
      "<td>" +
      usageCostHtml(totals, denominator) +
      "</td>"
    );
  }
  function usageExpandButton(kind, key, expanded) {
    return (
      "<button class='quota-expand-btn" +
      (expanded ? " open" : "") +
      "' data-usage-expand-kind='" +
      esc(kind) +
      "' data-usage-expand-key='" +
      esc(encodeURIComponent(key)) +
      "' aria-label='展开/收起明细' aria-expanded='" +
      (expanded ? "true" : "false") +
      "' title='展开/收起明细'>" +
      (expanded ? "▼" : "▶") +
      "</button>"
    );
  }
  function usageModelGroupSortValue(item, key) {
    if (key === "model_group")
      return String(item.name || item.target || item.model || "");
    if (key === "total") return totalTok(item.totals || item);
    if (key === "cost")
      return Number((item.totals || item).estimated_cost || 0);
    return Number((item.totals || item)[key] || 0);
  }

  function compareUsageModelGroupItems(a, b, key, level) {
    var va = usageModelGroupSortValue(a, key),
      vb = usageModelGroupSortValue(b, key);
    var cmp;
    if (key === "model_group") {
      var aName =
        level === "group" ? a.name : level === "target" ? a.name : a.model;
      var bName =
        level === "group" ? b.name : level === "target" ? b.name : b.model;
      cmp = String(aName || "").localeCompare(String(bName || ""));
    } else {
      cmp = va - vb;
    }
    if (cmp !== 0) return cmp * usageModelGroupSort.dir;
    var aFallback =
      level === "group" ? a.name : level === "target" ? a.name : a.model;
    var bFallback =
      level === "group" ? b.name : level === "target" ? b.name : b.model;
    return String(aFallback || "").localeCompare(String(bFallback || ""));
  }

  function updateUsageModelGroupSortArrows() {
    document
      .getElementById("usageByModelGroupTable")
      .querySelectorAll("thead th[data-sort]")
      .forEach(function (th) {
        th.classList.remove("sorted-asc", "sorted-desc");
        if (th.getAttribute("data-sort") === usageModelGroupSort.key) {
          th.classList.add(
            usageModelGroupSort.dir > 0 ? "sorted-asc" : "sorted-desc",
          );
        }
      });
  }

  function renderUsageModelGroups(rows, denominator) {
    var tb = document.getElementById("usageByModelGroupBody");
    rows = Array.isArray(rows) ? rows : [];
    denominator = denominator || usageDenominator;
    if (!rows.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='8'>当前时间段无数据</td></tr>";
      return;
    }
    var groups = Object.create(null);
    rows.forEach(function (row) {
      var groupName = String(row.model_group || "company_general");
      var targetName = String(row.target || "unknown_target");
      if (!groups[groupName])
        groups[groupName] = {
          totals: usageTotals(),
          targets: Object.create(null),
        };
      var group = groups[groupName];
      addUsageTotals(group.totals, row);
      if (!group.targets[targetName])
        group.targets[targetName] = {
          name: targetName,
          deleted: false,
          totals: usageTotals(),
          models: [],
        };
      var target = group.targets[targetName];
      target.deleted = target.deleted || !!row.target_deleted;
      addUsageTotals(target.totals, row);
      target.models.push(row);
    });
    var html = [];
    Object.keys(groups)
      .map(function (groupName) {
        var group = groups[groupName];
        return {
          name: groupName,
          totals: group.totals,
          targets: group.targets,
        };
      })
      .sort(function (a, b) {
        return compareUsageModelGroupItems(
          a,
          b,
          usageModelGroupSort.key,
          "group",
        );
      })
      .forEach(function (groupItem) {
        var groupName = groupItem.name;
        var group = groups[groupName],
          groupExpanded = state.usageModelGroupExpanded[groupName] !== false;
        var targetNames = Object.keys(group.targets)
          .map(function (targetName) {
            return group.targets[targetName];
          })
          .sort(function (a, b) {
            return compareUsageModelGroupItems(
              a,
              b,
              usageModelGroupSort.key,
              "target",
            );
          });
        html.push(
          "<tr class='usage-tree-row usage-group-row'><td>" +
            usageExpandButton("group", groupName, groupExpanded) +
            " <strong>" +
            esc(modelGroupLabel(groupName)) +
            "</strong> <span class='usage-tree-muted'>· " +
            targetNames.length +
            " 个目标</span></td>" +
            usageTotalCells(group.totals, denominator) +
            "</tr>",
        );
        if (!groupExpanded) return;
        targetNames.forEach(function (target) {
          var targetKey = groupName + "\x00" + target.name;
          var targetExpanded = !!state.usageModelTargetExpanded[targetKey];
          var targetLabel = target.name;
          if (target.deleted) targetLabel += " ";
          var deletedBadge = target.deleted
            ? " <span class='quota-badge del'>已删除</span>"
            : "";
          html.push(
            "<tr class='usage-tree-row usage-target-row'><td class='usage-tree-indent-target'>" +
              usageExpandButton("target", targetKey, targetExpanded) +
              " <strong>" +
              esc(targetLabel) +
              "</strong>" +
              deletedBadge +
              " <span class='usage-tree-muted'>· " +
              target.models.length +
              " 个模型</span></td>" +
              usageTotalCells(target.totals, denominator) +
              "</tr>",
          );
          if (!targetExpanded) return;
          target.models
            .slice()
            .sort(function (a, b) {
              return compareUsageModelGroupItems(
                a,
                b,
                usageModelGroupSort.key,
                "model",
              );
            })
            .forEach(function (model) {
              html.push(
                "<tr class='usage-tree-row usage-model-row'><td class='usage-tree-indent-model'>↳ " +
                  esc(model.model || "unknown") +
                  "</td>" +
                  usageTotalCells(model, denominator) +
                  "</tr>",
              );
            });
        });
      });
    tb.innerHTML = html.join("");
  }

  function setUsageChartNote(groupBy) {
    var note = document.getElementById("usageChartNote");
    if (!note) return;
    note.textContent =
      groupBy === "hour"
        ? "按查询时的时间范围和筛选 · 按 UTC 小时聚合；开始与结束时间所在整点小时均整桶计入（含边界小时），因此边界桶可能包含范围外记录。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。"
        : "按查询时的时间范围和筛选 · 所选范围内的小时用量按 UTC 自然日分组；开始与结束边界所在整点小时整桶计入，不扩展到完整起止 UTC 日。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。";
  }
  function usageChartLabel(start, groupBy) {
    var text = String(start || "");
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2})/.exec(text);
    if (!match) return text || "—";
    return groupBy === "hour"
      ? match[2] + "-" + match[3] + " " + match[4] + ":00"
      : match[2] + "-" + match[3];
  }
  function usageChartValue(bucket, metric) {
    if (metric === "cost") return usageNumber(bucket.estimated_cost);
    return usageTokensAvailable(bucket) ? totalTok(bucket) : null;
  }
  function setUsageChartMetric(metric) {
    usageChartMetric = metric === "cost" ? "cost" : "tokens";
    [
      ["usageChartMetricTokens", "tokens"],
      ["usageChartMetricCost", "cost"],
    ].forEach(function (item) {
      var button = document.getElementById(item[0]);
      if (!button) return;
      var active = item[1] === usageChartMetric;
      button.setAttribute("aria-pressed", String(active));
      button.classList.toggle("active", active);
    });
    if (usageSnapshot) renderDailyChart(usageChartBuckets, usageChartGroupBy);
  }
  function renderDailyChart(buckets, groupBy) {
    var chart = document.getElementById("dailyChart");
    buckets = Array.isArray(buckets) ? buckets : [];
    groupBy = groupBy || usageChartGroupBy || "day";
    usageChartBuckets = buckets;
    usageChartGroupBy = groupBy;
    if (!buckets.length) {
      chart.innerHTML =
        "<div style='color:var(--text-muted);padding:32px;text-align:center;width:100%;'>当前时间段内暂无统计数据</div>";
      return;
    }
    var isCost = usageChartMetric === "cost";
    var rows = buckets
      .slice()
      .sort(function (a, b) {
        return String(a.bucket_start).localeCompare(String(b.bucket_start));
      })
      .map(function (b) {
        return {
          label: usageChartLabel(b.bucket_start, groupBy),
          requests: b.requests,
          value: usageChartValue(b, usageChartMetric),
        };
      });
    var maxValue =
      rows.reduce(function (m, r) {
        return r.value === null ? m : Math.max(m, r.value);
      }, 0) || 1;
    chart.innerHTML = rows
      .map(function (r) {
        var missing = r.value === null;
        var h = missing
          ? 0
          : Math.max(4, Math.round((r.value / maxValue) * 180));
        var valueText = missing
          ? "不可用"
          : isCost
            ? fmtMoney(r.value)
            : fmtNum(r.value);
        var metricTitle = isCost
          ? "费用: " + valueText
          : "总 Token: " + valueText;
        var title =
          "UTC " +
          r.label +
          "\n" +
          metricTitle +
          "\n请求权重: " +
          usageCell(r.requests);
        var aria =
          "UTC " +
          r.label +
          "，" +
          (isCost ? "费用 " : "总 Token ") +
          valueText +
          "，请求权重 " +
          usageCell(r.requests);
        return (
          "<div class='chart-bar-group' tabindex='0' role='img' title='" +
          esc(title) +
          "' aria-label='" +
          esc(aria) +
          "'>" +
          "<div class='chart-value'>" +
          esc(valueText) +
          "</div>" +
          "<div class='chart-bar" +
          (missing ? " usage-chart-missing" : "") +
          "' style='height:" +
          h +
          "px'></div>" +
          "<div class='chart-label'>" +
          esc(r.label) +
          "</div>" +
          "</div>"
        );
      })
      .join("");
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
  var activeUsageTab = "group";
  function selectUsageTab(name, focus) {
    var selected = usageTabItems.find(function (item) {
      return item.name === name;
    });
    if (!selected) return;
    activeUsageTab = name;
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
        return item.name === activeUsageTab;
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

  document
    .getElementById("usageChartMetricTokens")
    .addEventListener("click", function () {
      setUsageChartMetric("tokens");
    });
  document
    .getElementById("usageChartMetricCost")
    .addEventListener("click", function () {
      setUsageChartMetric("cost");
    });

  document
    .getElementById("usageByModelGroupBody")
    .addEventListener("click", function (event) {
      var button = event.target.closest("[data-usage-expand-kind]");
      if (!button) return;
      var kind = button.dataset.usageExpandKind;
      var key = decodeURIComponent(button.dataset.usageExpandKey || "");
      if (kind === "group") {
        state.usageModelGroupExpanded[key] =
          state.usageModelGroupExpanded[key] === false;
      } else if (kind === "target") {
        state.usageModelTargetExpanded[key] =
          !state.usageModelTargetExpanded[key];
      }
      renderUsageModelGroups(state.usageModelGroupRows || [], usageDenominator);
    });

  usageTables.forEach(function (t) {
    document
      .getElementById(t.table)
      .querySelectorAll("thead th[data-sort]")
      .forEach(function (th) {
        th.addEventListener("click", function () {
          var key = th.getAttribute("data-sort");
          if (t.sort.key === key) {
            t.sort.dir = -t.sort.dir;
          } else {
            t.sort.key = key;
            t.sort.dir = ["key", "model_group", "model"].indexOf(key) >= 0 ? 1 : -1;
          }
          if (usageSnapshot) renderAggregateTable(t);
          updateUsageSortArrows();
        });
      });
  });
  document
    .getElementById("usageByModelGroupTable")
    .querySelectorAll("thead th[data-sort]")
    .forEach(function (th) {
      th.addEventListener("click", function () {
        var key = th.getAttribute("data-sort");
        if (usageModelGroupSort.key === key) {
          usageModelGroupSort.dir = -usageModelGroupSort.dir;
        } else {
          usageModelGroupSort.key = key;
          usageModelGroupSort.dir = key === "model_group" ? 1 : -1;
        }
        if (usageSnapshot) renderUsageModelGroups(state.usageModelGroupRows || []);
        updateUsageModelGroupSortArrows();
      });
    });
  function updateUsageSortArrows() {
    usageTables.forEach(function (t) {
      document
        .getElementById(t.table)
        .querySelectorAll("thead th[data-sort]")
        .forEach(function (th) {
          th.classList.remove("sorted-asc", "sorted-desc");
          if (th.getAttribute("data-sort") === t.sort.key) {
            th.classList.add(t.sort.dir > 0 ? "sorted-asc" : "sorted-desc");
          }
        });
    });
  }
  updateUsageSortArrows();
  updateUsageModelGroupSortArrows();

  window.refreshUsageModelGroupFilter = refreshUsageModelGroupFilter;
  return {
    getTimeRange: getTimeRange,
    usageGroupBy: usageGroupBy,
    usageNumber: usageNumber,
    usageCostDenominator: usageCostDenominator,
    usageCostShare: usageCostShare,
    usageModelRequestHeader: usageModelRequestHeader,
    setUsageModelRequestHeader: setUsageModelRequestHeader,
    renderUsageSummary: renderUsageSummary,
    selectUsageTab: selectUsageTab,
    setUsageChartMetric: setUsageChartMetric,
    populateUsageClientFilter: populateUsageClientFilter,
    populateUsageModelGroupFilter: populateUsageModelGroupFilter,
    appendUsageModelGroupOption: appendUsageModelGroupOption,
    updateUsageModelGroupFilterButton: updateUsageModelGroupFilterButton,
    refreshUsageModelGroupFilter: refreshUsageModelGroupFilter,
    ensureModelGroupsLoaded: ensureModelGroupsLoaded,
    usageModelGroupValues: usageModelGroupValues,
    appendUsageModelGroups: appendUsageModelGroups,
    loadUsage: loadUsage,
    usageSortValue: usageSortValue,
    renderAggregateTable: renderAggregateTable,
    usageSubsetHtml: usageSubsetHtml,
    usageCostHtml: usageCostHtml,
    usageTotals: usageTotals,
    addUsageTotals: addUsageTotals,
    usageTotalCells: usageTotalCells,
    usageExpandButton: usageExpandButton,
    usageModelGroupSortValue: usageModelGroupSortValue,
    compareUsageModelGroupItems: compareUsageModelGroupItems,
    updateUsageModelGroupSortArrows: updateUsageModelGroupSortArrows,
    renderUsageModelGroups: renderUsageModelGroups,
    renderDailyChart: renderDailyChart,
    updateUsageSortArrows: updateUsageSortArrows,
  };
});
