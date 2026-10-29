"use strict";
// Usage tables: summaries, cost shares, aggregate tables and the model-group tree.
AdminApp.register("usage_tables", function (app) {
  var state = app.state;
  var modelGroupLabel = app.bind("quota", "modelGroupLabel");
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var S = app.bind("usage_state", "store")();
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
          if (S.usageSnapshot) renderAggregateTable(t);
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
        if (S.usageSnapshot) renderUsageModelGroups(state.usageModelGroupRows || []);
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
  return {addUsageTotals: addUsageTotals, compareUsageModelGroupItems: compareUsageModelGroupItems, renderAggregateTable: renderAggregateTable, renderUsageModelGroups: renderUsageModelGroups, renderUsageSummary: renderUsageSummary, setUsageModelRequestHeader: setUsageModelRequestHeader, updateUsageModelGroupSortArrows: updateUsageModelGroupSortArrows, updateUsageSortArrows: updateUsageSortArrows, usageCell: usageCell, usageCostDenominator: usageCostDenominator, usageCostHtml: usageCostHtml, usageCostShare: usageCostShare, usageExpandButton: usageExpandButton, usageModelGroupSortValue: usageModelGroupSortValue, usageModelRequestHeader: usageModelRequestHeader, usageNumber: usageNumber, usageSortValue: usageSortValue, usageSubsetHtml: usageSubsetHtml, usageTokensAvailable: usageTokensAvailable, usageTotalCells: usageTotalCells, usageTotals: usageTotals, setUsageTableRows: setUsageTableRows, renderUsageTables: renderUsageTables};
});
