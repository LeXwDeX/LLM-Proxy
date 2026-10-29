"use strict";
// People summary table: status, usage columns, filters and detail rows.
AdminApp.register("quotaSummary", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var showConfirm = app.bind("core", "showConfirm");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var modelGroupLabel = app.bind("quotaModel", "modelGroupLabel");
  var jobLevelEntryName = app.bind("quotaModel", "jobLevelEntryName");
  var jobLevelByName = app.bind("quotaModel", "jobLevelByName");
  var jobLevelCoefText = app.bind("quotaModel", "jobLevelCoefText");
  var quotaNumber = app.bind("quotaModel", "quotaNumber");
  var quotaResetText = app.bind("quotaModel", "quotaResetText");
  var loadQuotaManagement = app.bind("quotaData", "loadQuotaManagement");
  var openQuotaEditDrawer = app.bind("quotaDrawer", "openQuotaEditDrawer");
  var quotaDimensions = app.modules.quotaModel.quotaDimensions;
  function quotaConvertedDetail(status) {
    var groups = status.groups || {};
    var names = Object.keys(groups).sort();
    var html =
      "<div class='quota-detail-note'>人员行的各周期进度是各独立限额组的合计，不是共享额度池；每个模型组的上限分别生效，任一组不限时合计显示不限。基数是配置值，已绑定职级时基数为 0 表示该周期上限不限；未绑定职级的请求被拒绝，可用上限以额度状态为准。扣费系数只用于消耗折算，不乘入可用上限；但扣费系数为 0 时该组不限。下次重置时间按模型组和周期分别列出。</div>";
    if (status.calibration_error) {
      html += "<div class='quota-detail-group'><h4>计费校准不可用</h4><div>" +
        esc(status.calibration_error) +
        "</div><div>用量暂未完成校准，请求暂时拒绝。修复计费数据后重新校准恢复。</div></div>";
    }
    if (status.job_level_missing) {
      html +=
        "<div class='quota-detail-group'><h4>职级</h4><div style='padding:6px 0;color:var(--bad);font-size:12px;'>未绑定职级：未配置额度，请求将被 429 拒绝。请在「编辑额度」中绑定职级。</div></div>";
    } else if (status.job_level) {
      var level = jobLevelByName(status.job_level);
      html +=
        "<div class='quota-detail-group'><h4>职级可用额度倍率 · " +
        esc(status.job_level) +
        "</h4><div style='font-size:12px;color:var(--text-muted);padding:2px 0;'>" +
        quotaDimensions
          .map(function (dim) {
            return (
              esc(dim.label) +
              " × " +
              esc(jobLevelCoefText(level && level[dim.job_coefficient_field]))
            );
          })
          .join(" · ") +
        "</div></div>";
    }
    if (names.length) {
      var groupRows = names
        .map(function (name) {
          var g = groups[name] || {},
            quotas = g.quotas || {};
          var gc = Number(g.cost_coefficient == null ? 1 : g.cost_coefficient);
          var pc = Number(g.plan_coefficient == null ? 1 : g.plan_coefficient);
          var rank = g.rank_coefficients || {};
          var base = g.base_limits || {};
          return (
            "<tr><th>" +
            esc(modelGroupLabel(name)) +
            (g.in_plan ? "" : " <span class='quota-badge none'>兜底</span>") +
            "</th>" +
            quotaDimensions
              .map(function (dim) {
                var q = quotas[dim.key] || {};
                var formula =
                  "可用上限：基数 " +
                  quotaNumber(base[dim.key] || 0) +
                  " × 职级倍率 " +
                  quotaNumber(rank[dim.key] == null ? 1 : rank[dim.key]) +
                  " × 个人倍率 " +
                  quotaNumber(pc) +
                  "；基数、职级倍率、个人倍率及扣费系数均 >0 时生成正上限，扣费系数为 0 时该组不限；折算消耗：原价 × 扣费系数 " +
                  quotaNumber(gc);
                var reset = quotaResetText(q.resets_at);
                return (
                  "<td>" +
                  quotaNumber(q.used || 0) +
                  " / " +
                  (q.limit > 0 ? quotaNumber(q.limit) : "不限") +
                  "<small style='display:block;color:var(--text-muted);font-weight:400;'>" +
                  formula +
                  "</small>" +
                  (reset
                    ? "<small class='quota-reset'>下次重置：" +
                      esc(reset) +
                      "</small>"
                    : "") +
                  "</td>"
                );
              })
              .join("") +
            "</tr>"
          );
        })
        .join("");
      html +=
        "<div class='quota-detail-group'><h4>模型组折算消耗与可用额度上限</h4><div class='quota-detail-scroll'><table class='quota-detail-table quota-group-table'><thead><tr><th>模型组</th>" +
        quotaDimensions
          .map(function (dim) {
            return "<th>" + esc(dim.label) + "（已折算消耗 / 可用上限）</th>";
          })
          .join("") +
        "</tr></thead><tbody>" +
        groupRows +
        "</tbody></table></div></div>";
    } else if (!status.calibration_error && !status.job_level_missing) {
      html +=
        "<span style='color:var(--text-muted)'>" +
        (status.job_level ? "未加入模型组" : "暂无用量") +
        "</span>";
    }
    return "<div class='quota-detail'>" + html + "</div>";
  }
  function quotaRowStatus(item) {
    var s = item.status || {},
      groups = s.groups || {};
    var c = item.client || {};
    if (s.calibration_error || (s.exceeded && s.exceeded.scope === "accounting"))
      return { key: "accounting", label: "计费校准不可用", badge: "warn" };
    if (
      !c.deleted &&
      (s.job_level_missing || (s.exceeded && s.exceeded.scope === "job_level"))
    )
      return { key: "joblevel", label: "职级未配置", badge: "over" };
    if (
      Object.keys(groups).some(function (name) {
        return groups[name].exceeded;
      })
    )
      return { key: "group", label: "模型组超限", badge: "over" };
    var configured = false,
      near = false;
    function scan(q) {
      if (q.limit > 0) {
        configured = true;
        if ((q.used || 0) / q.limit >= 0.9) near = true;
      }
    }
    Object.keys(groups).forEach(function (name) {
      quotaDimensions.forEach(function (dim) {
        scan((groups[name].quotas || {})[dim.key] || {});
      });
    });
    if (near) return { key: "warn", label: "接近限额", badge: "warn" };
    if (configured) return { key: "ok", label: "正常", badge: "ok" };
    if (
      Object.keys(groups).some(function (name) {
        return groups[name].in_plan;
      })
    )
      return { key: "unlimited", label: "不限", badge: "none" };
    return { key: "none", label: "未配置", badge: "none" };
  }
  function quotaBarCell(q, unavailable, dimLabel) {
    var prefix = dimLabel ? dimLabel + " " : "";
    if (unavailable) {
      var unavailableText = prefix + unavailable;
      if (unavailable === "-")
        return (
          "<div class='quota-cell'><span class='quota-pct'>-</span></div>"
        );
      return (
        "<div class='quota-cell' role='group' aria-label='" +
        esc(unavailableText) +
        "'><div class='quota-bar-wrap'><div class='quota-bar' aria-hidden='true'><div class='quota-bar-inner q-none' style='width:0%'></div></div></div><div class='quota-pct'>" +
        esc(unavailable) +
        "</div></div>"
      );
    }
    q = q || {};
    var limit = Number(q.limit || 0),
      used = Number(q.used || 0);
    if (!isFinite(limit) || limit <= 0) {
      return (
        "<div class='quota-cell' role='group' aria-label='" +
        esc(prefix + "不限（无正数上限）") +
        "'><div class='quota-bar-wrap'><div class='quota-bar' aria-hidden='true'><div class='quota-bar-inner q-none' style='width:100%'></div></div></div><div class='quota-pct'>不限</div></div>"
      );
    }
    if (!isFinite(used) || used < 0) used = 0;
    var pct = Math.min(100, Math.round((used / limit) * 100));
    var cls = pct >= 90 ? "q-over" : pct >= 70 ? "q-warn" : "q-ok";
    var stateText =
      prefix +
      "已用 " +
      fmtMoney(used) +
      "，上限 " +
      fmtMoney(limit) +
      "，" +
      pct +
      "%" +
      (used > limit ? "，超出上限" : "");
    return (
      "<div class='quota-cell'><div class='quota-bar-wrap'><div class='quota-bar' role='progressbar' aria-valuemin='0' aria-valuemax='100' aria-valuenow='" +
      pct +
      "' aria-label='" +
      esc(stateText) +
      "'><div class='quota-bar-inner " +
      cls +
      "' style='width:" +
      pct +
      "%'></div></div></div><div class='quota-pct'>" +
      fmtMoney(used) +
      " / " +
      fmtMoney(limit) +
      "</div></div>"
    );
  }
  function quotaGroupSummary(groups) {
    var names = Object.keys(groups || {});
    if (!names.length)
      return "<span style='color:var(--text-muted);'>未配置</span>";
    var exceeded = names.filter(function (name) {
      return groups[name].exceeded;
    }).length;
    return (
      "<span>" +
      names.length +
      " 个模型组" +
      (exceeded
        ? " · <span class='quota-badge over'>" + exceeded + " 超限</span>"
        : "") +
      "</span>"
    );
  }
  function renderQuotaSummaryFilters() {
    [
      [
        "quotaSummaryProjectFilter",
        "project_group",
        "projectGroups",
        "全部项目组",
      ],
      ["quotaSummaryJobFilter", "job_level", "jobLevels", "全部职级"],
    ].forEach(function (spec) {
      var select = document.getElementById(spec[0]),
        current = select.value,
        values = {};
      (state[spec[2]] || []).forEach(function (v) {
        var name = jobLevelEntryName(v);
        if (name) values[name] = true;
      });
      (state.quotaSummary || []).forEach(function (item) {
        var v = (item.client || {})[spec[1]];
        if (v) values[v] = true;
      });
      select.innerHTML =
        "<option value=''>" +
        spec[3] +
        "</option>" +
        Object.keys(values)
          .sort()
          .map(function (v) {
            return "<option value='" + esc(v) + "'>" + esc(v) + "</option>";
          })
          .join("");
      if (values[current]) select.value = current;
    });
  }
  function quotaTotalTok(r) {
    return totalTok(r);
  }
  var quotaTokenFields = [
    "input_tokens",
    "output_tokens",
    "cached_tokens",
    "cache_creation_tokens",
    "tool_use_prompt_tokens",
  ];
  function quotaFiniteNumber(value) {
    return typeof value === "number" && isFinite(value) ? value : null;
  }
  function quotaUsageStats(name) {
    if (state.clientUsage30dPending)
      return {
        tokens: null,
        cost: null,
        unavailable: false,
        pending: true,
        usageIncomplete: 0,
        costIncomplete: 0,
      };
    if (state.clientUsage30dUnavailable)
      return {
        tokens: null,
        cost: null,
        unavailable: true,
        usageIncomplete: 0,
        costIncomplete: 0,
      };
    var r = (state.clientUsage30d || {})[name];
    if (!r)
      return {
        tokens: 0,
        cost: 0,
        unavailable: false,
        usageIncomplete: 0,
        costIncomplete: 0,
      };
    var hasTokens = quotaTokenFields.some(function (field) {
      return quotaFiniteNumber(r[field]) !== null;
    });
    return {
      tokens: hasTokens ? quotaTotalTok(r) : null,
      cost: quotaFiniteNumber(r.estimated_cost),
      unavailable: false,
      usageIncomplete: Number(r.incomplete_usage_count || 0),
      costIncomplete: Number(r.incomplete_cost_count || 0),
    };
  }
  function quotaTokenCell(stats) {
    if (stats.pending) return "<span class='quota-usage-unavailable'>加载中…</span>";
    if (stats.tokens === null)
      return (
        "<span class='quota-usage-unavailable'>不可用</span>" +
        (stats.usageIncomplete
          ? "<small class='quota-usage-note'>用量不完整 " +
            stats.usageIncomplete +
            " 条记录</small>"
          : "")
      );
    var html = fmtNum(stats.tokens);
    if (stats.usageIncomplete)
      html +=
        "<small class='quota-usage-note'>用量不完整 " +
        stats.usageIncomplete +
        " 条记录</small>";
    return html;
  }
  function quotaCostCell(stats) {
    if (stats.pending) return "<span class='quota-usage-unavailable'>加载中…</span>";
    if (stats.cost === null) {
      return (
        "<span class='quota-usage-unavailable'>不可用</span><small class='quota-cost-note'>" +
        (stats.unavailable
          ? "30 天用量加载失败"
          : "聚合接口未返回 estimated_cost") +
        "</small>" +
        (stats.costIncomplete
          ? "<small class='quota-cost-note'>费用不完整 " +
            stats.costIncomplete +
            " 条记录</small>"
          : "")
      );
    }
    var html = fmtMoney(stats.cost);
    if (stats.costIncomplete)
      html +=
        "<small class='quota-cost-note'>费用不完整 " +
        stats.costIncomplete +
        " 条记录</small>";
    return html;
  }
  function quotaSummaryScopedRows() {
    var search = (document.getElementById("quotaSearch").value || "")
      .trim()
      .toLowerCase();
    var project = document.getElementById("quotaSummaryProjectFilter").value;
    var job = document.getElementById("quotaSummaryJobFilter").value;
    return (state.quotaSummary || [])
      .map(function (item) {
        return { item: item, status: quotaRowStatus(item) };
      })
      .filter(function (entry) {
        var c = entry.item.client || {};
        if (
          search &&
          !(
            (c.name || "").toLowerCase().indexOf(search) >= 0 ||
            (c.ip_address || "").toLowerCase().indexOf(search) >= 0
          )
        )
          return false;
        if (project && (c.project_group || "未分组") !== project) return false;
        if (job && String(c.job_level || "").toLowerCase() !== job.toLowerCase()) return false;
        return true;
      });
  }
  function renderQuotaStatusCounts(rows) {
    var select = document.getElementById("quotaSummaryStatusFilter");
    var options = Array.prototype.slice.call(select.options || []);
    if (!options.length) return;
    var counts = {};
    rows.forEach(function (entry) {
      counts[entry.status.key] = (counts[entry.status.key] || 0) + 1;
    });
    options.forEach(function (option) {
      var base = String(option.textContent || "").replace(/\s*\(\d+\)$/, "");
      var count = option.value ? counts[option.value] || 0 : rows.length;
      option.textContent = base + " (" + count + ")";
    });
  }
  function renderQuotaSummary() {
    var statusFilter = document.getElementById(
      "quotaSummaryStatusFilter",
    ).value;
    var scoped = quotaSummaryScopedRows();
    renderQuotaStatusCounts(scoped);
    var rows = scoped
      .filter(function (entry) {
        return !statusFilter || entry.status.key === statusFilter;
      })
      .sort(function (a, b) {
        return String((a.item.client || {}).name || "").localeCompare(
          String((b.item.client || {}).name || ""),
        );
      });
    document.getElementById("quotaSummaryBody").innerHTML = rows.length
      ? rows
          .map(function (entry) {
            var item = entry.item,
              c = item.client || {},
              s = item.status || {},
              groups = s.groups || {},
              st = entry.status;
            var expanded = !!state.quotaExpanded[c.name];
            var detailId = "quota-detail-" + encodeURIComponent(c.name);
            var usage30 = quotaUsageStats(c.name);
            var deletedBadge = c.deleted
              ? " <span class='quota-badge del'>已删除</span>"
              : "";
            var jobLabel = c.job_level
              ? esc(c.job_level)
              : c.deleted
                ? "-"
                : "<span class='quota-badge warn'>未绑职级</span>";
            var personCell =
              "<strong>" +
              esc(c.name) +
              "</strong>" +
              deletedBadge +
              "<br><span style='font-size:12px;color:var(--text-muted);'>" +
              esc(c.ip_address || "-") +
              " · " +
              jobLabel +
              " · " +
              esc(c.project_group || "未分组") +
              "</span>";
            var quotaUnavailable = c.deleted
              ? "-"
              : st.key === "accounting"
                ? "校准不可用"
                : st.key === "joblevel"
                ? "职级未配置"
                : st.key === "none"
                  ? "未配置"
                  : "";
            var actions = c.deleted
              ? "<div class='btn-group'><button class='btn btn-sm btn-danger readonly-hide' data-purge-quota='" +
                esc(c.name) +
                "'>清除记录</button></div>"
              : "<div class='btn-group'><button class='btn btn-sm readonly-hide' data-edit-quota='" +
                esc(c.name) +
                "'>编辑额度</button></div>";
            var row =
              "<tr data-quota-person='" +
              esc(c.name) +
              "'>" +
              "<td><button class='quota-expand-btn" +
              (expanded ? " open" : "") +
              "' data-expand-quota='" +
              esc(c.name) +
              "' aria-label='展开/收起明细' aria-expanded='" +
              (expanded ? "true" : "false") +
              "' aria-controls='" +
              esc(detailId) +
              "' title='展开/收起明细'>" +
              (expanded ? "▼" : "▶") +
              "</button></td>" +
              "<td class='quota-person-cell'>" +
              personCell +
              "</td>" +
              quotaDimensions
                .map(function (dim) {
                  return (
                    "<td>" +
                    quotaBarCell(
                      (s.quotas || {})[dim.key],
                      quotaUnavailable,
                      dim.label,
                    ) +
                    "</td>"
                  );
                })
                .join("") +
              "<td class='quota-number quota-token-cell'>" +
              quotaTokenCell(usage30) +
              "</td>" +
              "<td class='quota-number quota-cost-cell'>" +
              quotaCostCell(usage30) +
              "</td>" +
              "<td>" +
              quotaGroupSummary(groups) +
              "</td>" +
              "<td><span class='quota-badge " +
              st.badge +
              "'>" +
              st.label +
              "</span></td>" +
              "<td>" +
              actions +
              "</td></tr>";
            if (expanded)
              row +=
                "<tr class='quota-detail-row' id='" +
                esc(detailId) +
                "' data-quota-detail='" +
                esc(c.name) +
                "'><td colspan='" +
                (quotaDimensions.length + 7) +
                "'>" +
                quotaConvertedDetail(s) +
                "</td></tr>";
            return row;
          })
          .join("")
      : "<tr class='empty-row'><td colspan='" +
        (quotaDimensions.length + 7) +
        "'>暂无客户端</td></tr>";
  }
  // Typing re-renders the whole table, so wait for a short pause instead of every keystroke.
  var searchTimer = null;
  document.getElementById("quotaSearch").addEventListener("input", function () {
    if (searchTimer !== null) clearTimeout(searchTimer);
    searchTimer = setTimeout(function () {
      searchTimer = null;
      renderQuotaSummary();
    }, 200);
  });
  document
    .getElementById("quotaSummaryProjectFilter")
    .addEventListener("change", renderQuotaSummary);
  document
    .getElementById("quotaSummaryJobFilter")
    .addEventListener("change", renderQuotaSummary);
  document
    .getElementById("quotaSummaryStatusFilter")
    .addEventListener("change", renderQuotaSummary);
  document
    .getElementById("quotaSummaryBody")
    .addEventListener("click", function (e) {
      var expandBtn = e.target.closest("[data-expand-quota]");
      if (expandBtn) {
        var name = expandBtn.dataset.expandQuota;
        state.quotaExpanded = state.quotaExpanded || {};
        state.quotaExpanded[name] = !state.quotaExpanded[name];
        renderQuotaSummary();
        return;
      }
      var editBtn = e.target.closest("[data-edit-quota]");
      if (editBtn) {
        var item = (state.quotaSummary || []).find(function (x) {
          return (x.client || {}).name === editBtn.dataset.editQuota;
        });
        if (item) openQuotaEditDrawer(item);
      }
      var purgeBtn = e.target.closest("[data-purge-quota]");
      if (purgeBtn) {
        if (isReadonlyMode()) return;
        var name = purgeBtn.dataset.purgeQuota;
        showConfirm(
          "确认清除已删除客户端「" +
            name +
            "」的额度记录？清除后月度统计中不再显示该行。",
          async function () {
            try {
              await api("/admin/data/quota/" + encodeURIComponent(name), {
                method: "DELETE",
              });
              toast("记录已清除", "success");
              await loadQuotaManagement();
            } catch (err) {
              toast("清除失败: " + err.message, "error");
            }
          },
        );
      }
    });
  return {
    quotaConvertedDetail: quotaConvertedDetail,
    quotaRowStatus: quotaRowStatus,
    quotaBarCell: quotaBarCell,
    quotaGroupSummary: quotaGroupSummary,
    renderQuotaSummaryFilters: renderQuotaSummaryFilters,
    quotaTotalTok: quotaTotalTok,
    quotaFiniteNumber: quotaFiniteNumber,
    quotaUsageStats: quotaUsageStats,
    quotaTokenCell: quotaTokenCell,
    quotaCostCell: quotaCostCell,
    quotaSummaryScopedRows: quotaSummaryScopedRows,
    renderQuotaStatusCounts: renderQuotaStatusCounts,
    renderQuotaSummary: renderQuotaSummary,
  };
});
