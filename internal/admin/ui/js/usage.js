"use strict";
// Usage owns time/filter selection, aggregate tables and daily charts.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("usage", function (app) {
  var state = app.state;
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

  var activeRange = "today";
  document.querySelectorAll(".time-range-btn").forEach(function (btn) {
    btn.addEventListener("click", function () {
      document.querySelectorAll(".time-range-btn").forEach(function (b) {
        b.classList.remove("active");
      });
      btn.classList.add("active");
      activeRange = btn.dataset.range;
      if (activeRange === "custom") {
        document.getElementById("customRangeInputs").style.display = "flex";
        var r = getTimeRange("7d");
        document.getElementById("usageFrom").value = toLocalInput(
          new Date(r.from),
        );
        document.getElementById("usageTo").value = toLocalInput(new Date(r.to));
      } else {
        document.getElementById("customRangeInputs").style.display = "none";
        /* Reset client filter so dropdown refreshes for the new time range */
        document.getElementById("usageClientFilter").value = "";
        loadUsage();
      }
    });
  });
  document
    .getElementById("applyCustomRange")
    .addEventListener("click", function () {
      document.getElementById("usageClientFilter").value = "";
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
        state.modelGroups = Array.isArray(data.model_groups)
          ? data.model_groups
          : [];
        state.modelGroupsLoaded = true;
        refreshUsageModelGroupFilter();
        return state.modelGroups;
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
    var range;
    if (activeRange === "custom") {
      try {
        range = { from: inputToISO("usageFrom"), to: inputToISO("usageTo") };
      } catch (e) {
        toast(e.message, "error");
        return;
      }
    } else {
      range = getTimeRange(activeRange);
    }
    state.usageRange = range;
    try {
      await ensureModelGroupsLoaded();
      var qs = new URLSearchParams({ from: range.from, to: range.to });
      var clientFilter = document.getElementById("usageClientFilter").value;
      if (clientFilter) qs.set("client_name", clientFilter);
      var modelGroupFilter = usageModelGroupValues();
      appendUsageModelGroups(qs, modelGroupFilter);
      var chartQs = new URLSearchParams({
        from: new Date(Date.now() - 30 * 24 * 3600 * 1000).toISOString(),
        to: new Date().toISOString(),
      });
      if (clientFilter) chartQs.set("client_name", clientFilter);
      appendUsageModelGroups(chartQs, modelGroupFilter);
      var results = await Promise.all([
        api("/admin/data/usage/aggregate?group_by=day&" + qs.toString()),
        api("/admin/data/usage/aggregate?group_by=day&" + chartQs.toString()),
      ]);
      var mainAgg = results[0];
      var byDay = results[1];
      refreshUsageModelGroupFilter();

      /* Update client filter dropdown with clients that have data in this range */
      if (!clientFilter) {
        var activeNames = (mainAgg.by_client || []).map(function (c) {
          return c.key;
        });
        populateUsageClientFilter(activeNames);
      }

      /* Summary cards from aggregate totals (respects client filter) */
      var sp = mainAgg.totals || {};
      document.getElementById("usReqs").textContent = fmtNum(sp.requests);
      document.getElementById("usInput").textContent = fmtNum(sp.input_tokens);
      document.getElementById("usOutput").textContent = fmtNum(
        sp.output_tokens,
      );
      document.getElementById("usCached").textContent = fmtNum(
        sp.cached_tokens,
      );
      document.getElementById("usCacheWrite").textContent = fmtNum(
        sp.cache_creation_tokens,
      );
      document.getElementById("usCost").innerHTML = usageCostHtml(sp);

      usageTableMap.usageByClientBody.rows = mainAgg.by_client || [];
      var modelRows = mainAgg.by_model_group_model;
      if (!modelRows || !modelRows.length) modelRows = mainAgg.by_model || [];
      usageTableMap.usageByModelBody.rows = modelRows;
      usageTables.forEach(renderAggregateTable);
      state.usageModelGroupRows = mainAgg.by_target_model || [];
      renderUsageModelGroups(state.usageModelGroupRows);
      renderDailyChart(byDay.buckets || []);
    } catch (e) {
      toast("加载统计失败: " + e.message, "error");
    }
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
          "<td>" +
          fmtNum(r.requests) +
          "</td>" +
          "<td>" +
          fmtNum(r.input_tokens) +
          "</td>" +
          "<td>" +
          usageSubsetHtml(r.output_tokens, r.reasoning_tokens, "推理") +
          "</td>" +
          "<td>" +
          usageSubsetHtml(
            r.cached_tokens,
            r.explicit_cache_read_tokens,
            "显式",
          ) +
          "</td>" +
          "<td>" +
          fmtNum(r.cache_creation_tokens) +
          "</td>" +
          "<td>" +
          fmtNum(totalTok(r)) +
          "</td>" +
          "<td>" +
          usageCostHtml(r) +
          "</td></tr>"
        );
      })
      .join("");
  }

  function usageSubsetHtml(total, subset, label) {
    // Keep a consistent token slot; CSS hides absent details and centers the main value.
    return (
      "<span class='usage-token-value'><span>" +
      fmtNum(total) +
      "</span><small class='usage-token-detail'>" +
      (Number(subset || 0) ? "其中" + esc(label) + " " + fmtNum(subset) : "") +
      "</small></span>"
    );
  }
  function usageCostHtml(r) {
    var label = fmtMoney(r.estimated_cost);
    var incomplete = Number(r.incomplete_usage_count || 0);
    var partial = Number(r.incomplete_cost_count || 0);
    if (incomplete || partial)
      label +=
        " <span title='用量不完整：" +
        incomplete +
        "；成本覆盖不完整：" +
        partial +
        "'>（部分）</span>";
    return label;
  }
  function usageTotals() {
    return {
      requests: 0,
      input_tokens: 0,
      output_tokens: 0,
      cached_tokens: 0,
      cache_creation_tokens: 0,
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
      "estimated_cost",
      "incomplete_usage_count",
      "incomplete_cost_count",
      "reasoning_tokens",
      "explicit_cache_read_tokens",
    ].forEach(function (key) {
      target[key] += Number(source[key] || 0);
    });
  }
  function usageTotalCells(totals) {
    return (
      "<td>" +
      fmtNum(totals.requests) +
      "</td>" +
      "<td>" +
      fmtNum(totals.input_tokens) +
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
      fmtNum(totals.cache_creation_tokens) +
      "</td>" +
      "<td>" +
      fmtNum(totalTok(totals)) +
      "</td>" +
      "<td>" +
      usageCostHtml(totals) +
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

  function renderUsageModelGroups(rows) {
    var tb = document.getElementById("usageByModelGroupBody");
    if (!rows.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='8'>当前时间段无数据</td></tr>";
      return;
    }
    var groups = {};
    rows.forEach(function (row) {
      var groupName = String(row.model_group || "company_general");
      var targetName = String(row.target || "unknown_target");
      if (!groups[groupName])
        groups[groupName] = { totals: usageTotals(), targets: {} };
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
            " Targets</span></td>" +
            usageTotalCells(group.totals) +
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
              " 模型</span></td>" +
              usageTotalCells(target.totals) +
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
                  usageTotalCells(model) +
                  "</tr>",
              );
            });
        });
      });
    tb.innerHTML = html.join("");
  }

  function renderDailyChart(buckets) {
    var chart = document.getElementById("dailyChart");
    if (!buckets.length) {
      chart.innerHTML =
        "<div style='color:var(--text-muted);padding:32px;text-align:center;width:100%;'>当前时间段内暂无统计数据</div>";
      return;
    }
    var rows = buckets
      .slice()
      .sort(function (a, b) {
        return String(a.bucket_start).localeCompare(String(b.bucket_start));
      })
      .map(function (b) {
        var reqs = Number(b.requests || 0);
        var total = totalTok(b);
        return {
          date: String(b.bucket_start || "").slice(5, 10),
          reqs: reqs,
          total: total,
          cost: Number(b.estimated_cost || 0),
        };
      });
    var maxTotal =
      rows.reduce(function (m, r) {
        return Math.max(m, r.total);
      }, 0) || 1;
    chart.innerHTML = rows
      .map(function (r) {
        var h = Math.max(4, Math.round((r.total / maxTotal) * 180));
        var title =
          "总 Token: " +
          fmtNum(r.total) +
          "\n请求数: " +
          fmtNum(r.reqs) +
          "\n费用: " +
          fmtMoney(r.cost);
        return (
          "<div class='chart-bar-group' title='" +
          esc(title) +
          "'>" +
          "<div class='chart-value'>" +
          fmtNum(r.total) +
          "</div>" +
          "<div class='chart-bar' style='height:" +
          h +
          "px'></div>" +
          "<div class='chart-label'>" +
          esc(r.date) +
          "</div>" +
          "</div>"
        );
      })
      .join("");
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
      renderUsageModelGroups(state.usageModelGroupRows || []);
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
          renderAggregateTable(t);
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
        renderUsageModelGroups(state.usageModelGroupRows || []);
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
