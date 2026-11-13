"use strict";
// Usage tables: summaries, cost shares, aggregate tables and the model-group tree.
AdminApp.register("usageTables", function (app) {
  var state = app.state;
  var modelGroupLabel = app.bind("quota", "modelGroupLabel");
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var createTableSort = app.bind("core", "createTableSort");
  var S = app.bind("usageState", "store")();
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
  var usageTokenFields = [
    "input_tokens",
    "output_tokens",
    "cached_tokens",
    "cache_creation_tokens",
    "tool_use_prompt_tokens",
  ];
  // Column sorting (shared createTableSort). Each table keeps its default order (client: cost, then total
  // tokens, both descending; model: the visible model-group label, then total tokens descending; model-group
  // tree: total tokens descending, then name) and a header click cycles ascending, descending, default.
  // Cells that render as "不可用" have no value and sort last.
  var usageNumericKeys = [
    "requests",
    "input_tokens",
    "output_tokens",
    "cached_tokens",
    "cache_creation_tokens",
    "total",
    "cost",
  ];
  function usageAggregateColumns(textKeys) {
    var columns = {};
    textKeys.concat(usageNumericKeys).forEach(function (key) {
      columns[key] = function (row) {
        return usageSortValue(row, key);
      };
    });
    return columns;
  }
  function usageClientDefault(a, b) {
    return (
      Number(b.estimated_cost || 0) - Number(a.estimated_cost || 0) ||
      totalTok(b) - totalTok(a)
    );
  }
  function usageModelDefault(a, b) {
    return (
      String(modelGroupLabel(a.model_group || "-")).localeCompare(
        String(modelGroupLabel(b.model_group || "-")),
      ) || totalTok(b) - totalTok(a)
    );
  }
  var usageTables = [
    {
      table: "usageByClientTable",
      head: "usageByClientTableHead",
      tbody: "usageByClientBody",
      rows: [],
      columnCount: 8,
      columns: usageAggregateColumns(["key"]),
      defaultCompare: usageClientDefault,
    },
    {
      table: "usageByModelTable",
      head: "usageByModelTableHead",
      tbody: "usageByModelBody",
      rows: [],
      columnCount: 9,
      columns: usageAggregateColumns(["model_group", "model"]),
      defaultCompare: usageModelDefault,
    },
  ];
  var usageTableMap = {};
  usageTables.forEach(function (t) {
    usageTableMap[t.tbody] = t;
    t.sort = createTableSort({
      columns: t.columns,
      onChange: function () {
        if (S.usageSnapshot) renderAggregateTable(t);
      },
    });
    t.sort.bind(document.getElementById(t.head));
  });

  function setUsageTableRows(tbody, rows) {
    usageTableMap[tbody].rows = rows;
  }
  function renderUsageTables() {
    usageTables.forEach(renderAggregateTable);
  }
  function usageSortValue(r, key) {
    if (key === "key")
      return String(
        r.key || (r.model_group ? r.model_group + " / " + (r.model || "") : ""),
      );
    if (key === "model_group")
      return String(modelGroupLabel(r.model_group || "-"));
    if (key === "model") return String(r.model || r.key || "");
    if (key === "total") return usageTokensAvailable(r) ? totalTok(r) : null;
    if (key === "cost") return usageNumber(r.estimated_cost);
    return usageNumber(r[key]);
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
    // Callers may pass an ad-hoc descriptor ({tbody, rows}); ordering always comes from the registered table.
    var registered = usageTableMap[t.tbody] || t;
    tb.innerHTML = registered.sort
      .apply(t.rows, registered.defaultCompare)
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
          usageTotalCells(r, S.usageDenominator) +
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
  // The model-group tree sorts each level (groups, the targets inside a group, the models inside a target)
  // by the same column. Items are group {name, label, totals, targets}, target {name, totals, models} or a raw
  // model row; the name column sorts a group by its visible label.
  function usageTreeTotals(item) {
    return item.totals || item;
  }
  function usageTreeName(item) {
    return String(item.name != null ? item.name : item.model || "");
  }
  function usageTreeLabel(item) {
    if (item.targets) return String(item.label || item.name || "");
    return usageTreeName(item);
  }
  function usageTreeValue(item, key) {
    var totals = usageTreeTotals(item);
    if (key === "total") return usageTokensAvailable(totals) ? totalTok(totals) : null;
    if (key === "cost") return usageNumber(totals.estimated_cost);
    return usageNumber(totals[key]);
  }
  function usageTreeDefault(a, b) {
    return (
      totalTok(usageTreeTotals(b)) - totalTok(usageTreeTotals(a)) ||
      usageTreeName(a).localeCompare(usageTreeName(b))
    );
  }
  var usageTreeColumns = { model_group: usageTreeLabel };
  usageNumericKeys.forEach(function (key) {
    usageTreeColumns[key] = function (item) {
      return usageTreeValue(item, key);
    };
  });
  var usageModelGroupSort = createTableSort({
    columns: usageTreeColumns,
    onChange: function () {
      if (S.usageSnapshot) renderUsageModelGroups(state.usageModelGroupRows || []);
    },
  });
  usageModelGroupSort.bind(document.getElementById("usageByModelGroupTableHead"));

  function renderUsageModelGroups(rows, denominator) {
    var tb = document.getElementById("usageByModelGroupBody");
    rows = Array.isArray(rows) ? rows : [];
    denominator = denominator || S.usageDenominator;
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
    usageModelGroupSort
      .apply(
        Object.keys(groups).map(function (groupName) {
          var group = groups[groupName];
          return {
            name: groupName,
            label: modelGroupLabel(groupName),
            totals: group.totals,
            targets: group.targets,
          };
        }),
        usageTreeDefault,
      )
      .forEach(function (groupItem) {
        var groupName = groupItem.name;
        var group = groups[groupName],
          groupExpanded = state.usageModelGroupExpanded[groupName] !== false;
        var targetNames = usageModelGroupSort.apply(
          Object.keys(group.targets).map(function (targetName) {
            return group.targets[targetName];
          }),
          usageTreeDefault,
        );
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
          usageModelGroupSort
            .apply(target.models, usageTreeDefault)
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
      renderUsageModelGroups(state.usageModelGroupRows || [], S.usageDenominator);
    });

  return {addUsageTotals: addUsageTotals, renderAggregateTable: renderAggregateTable, renderUsageModelGroups: renderUsageModelGroups, renderUsageSummary: renderUsageSummary, setUsageModelRequestHeader: setUsageModelRequestHeader, usageCell: usageCell, usageCostDenominator: usageCostDenominator, usageCostHtml: usageCostHtml, usageCostShare: usageCostShare, usageExpandButton: usageExpandButton, usageModelRequestHeader: usageModelRequestHeader, usageNumber: usageNumber, usageSortValue: usageSortValue, usageSubsetHtml: usageSubsetHtml, usageTokensAvailable: usageTokensAvailable, usageTotalCells: usageTotalCells, usageTotals: usageTotals, setUsageTableRows: setUsageTableRows, renderUsageTables: renderUsageTables};
});
