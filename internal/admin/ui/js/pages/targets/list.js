"use strict";
// Targets list: table rendering, status filters, model hints and row actions.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsList", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var showConfirm = app.bind("core", "showConfirm");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var poolDisplayName = app.bind("pools", "poolDisplayName");
  var loadPools = app.bind("pools", "loadPools");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var filteredTargets = app.bind("targetsHealth", "filteredTargets");
  var withHealthMemo = app.bind("targetsHealth", "withHealthMemo");
  var keyStatusColor = app.bind("targetsHealth", "keyStatusColor");
  var keyStatusNeedsAttention = app.bind("targetsHealth", "keyStatusNeedsAttention");
  var loadTargets = app.bind("targets", "loadTargets");
  var openTargetDetail = app.bind("targetsDetail", "openTargetDetail");
  var openTargetModal = app.bind("targetsModal", "openTargetModal");
  var refreshOpenTargetDetail = app.bind("targetsDetail", "refreshOpenTargetDetail");
  var targetHealth = app.bind("targetsHealth", "targetHealth");
  var targetHealthCounts = app.bind("targetsHealth", "targetHealthCounts");
  var targetPhaseKnown = app.bind("targetsHealth", "targetPhaseKnown");
  var targetFilterSpecs = [
    { key: "all", label: "全部" },
    { key: "attention", label: "异常" },
    { key: "healthy", label: "正常" },
    { key: "paused", label: "已暂停" },
  ];
  function renderTargetStatusFilters() {
    if (ts.targetsLoadFailed && !ts.targetsLoaded) {
      document.getElementById("targetStatusFilters").innerHTML = "<span role='status'>目标状态不可用</span>";
      return;
    }
    var counts = targetHealthCounts();
    document.getElementById("targetStatusFilters").innerHTML = targetFilterSpecs
      .map(function (spec) {
        return (
          '<button type="button" class="target-status-chip" data-target-filter="' +
          spec.key +
          '" aria-pressed="' +
          (ts.targetFilter === spec.key) +
          '">' +
          esc(spec.label) +
          " <span class='target-status-count'>" +
          counts[spec.key] +
          "</span></button>"
        );
      })
      .join("");
  }
  function setTargetFilter(filter) {
    ts.targetFilter =
      targetFilterSpecs.some(function (spec) {
        return spec.key === filter;
      })
        ? filter
        : "all";
    renderTargets();
  }
  function switchTargetPageTab(name) {
    var pools = name === "pools";
    document.getElementById("targetsMainPanel").hidden = pools;
    document.getElementById("targetsPoolsPanel").hidden = !pools;
    [
      ["targetsMainTab", !pools],
      ["targetsPoolsTab", pools],
    ].forEach(function (entry) {
      var tab = document.getElementById(entry[0]);
      tab.classList.toggle("active", entry[1]);
      tab.setAttribute("aria-selected", entry[1] ? "true" : "false");
      tab.tabIndex = entry[1] ? 0 : -1;
    });
    return pools ? loadPools() : loadTargets();
  }
  function renderTargets() {
    return withHealthMemo(renderTargetsBody);
  }
  // Rows are replaced only when their markup changed. A refresh that returns the
  // same data (tab switch, manual refresh) would otherwise swap the DOM under
  // the pointer and keyboard focus, dropping hovers and clicks in flight.
  var renderedTableHtml = null;
  function writeTargetsTable(tb, html) {
    if (renderedTableHtml === html) return;
    if (modelHint) hideModelHint();
    tb.innerHTML = html;
    renderedTableHtml = html;
  }
  function renderTargetsBody() {
    renderTargetStatusFilters();
    var tb = document.getElementById("targetsTableBody");
    var targets = filteredTargets();
    if (ts.targetsLoadFailed && !ts.targetsLoaded) {
      writeTargetsTable(tb, "<tr class='empty-row'><td colspan='8' role='alert'>加载目标失败，请点击「刷新」重试。</td></tr>");
      return;
    }
    var staleNotice = ts.targetsLoadFailed
      ? "<tr><td colspan='8' role='status'>更新失败，显示上次加载的数据；请点击「刷新」重试。</td></tr>" : "";
    if (!ts.targetPoolsSnapshot) {
      staleNotice += "<tr><td colspan='8' role='status'>部署池数据不可用，池名称及表单选项可能是上次数据；请刷新重试。</td></tr>";
    }
    if (!state.targets.length) {
      writeTargetsTable(tb,
        staleNotice + "<tr class='empty-row'><td colspan='8'>暂无目标配置</td></tr>");
      refreshOpenTargetDetail();
      return;
    }
    if (!targets.length) {
      writeTargetsTable(tb,
        staleNotice + "<tr class='empty-row'><td colspan='8'>当前筛选无目标</td></tr>");
      refreshOpenTargetDetail();
      return;
    }
    writeTargetsTable(tb, staleNotice + targets
      .map(function (t) {
        var health = targetHealth(t);
        var poolCell = t.pool
          ? "<span class='client-target-pool-tag'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='1.8' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='3' y='4' width='18' height='7' rx='2'></rect><rect x='3' y='13' width='18' height='7' rx='2'></rect><path d='M7 7.5h.01M7 16.5h.01'></path></svg> " +
            esc(poolDisplayName(t.pool)) +
            "</span>"
          : "<span style='color:var(--text-muted)'>—</span>";
        var modelNames = (
          Array.isArray(t.model_mappings) ? t.model_mappings : []
        ).map(function (m) {
          var label = m.fallback
            ? m.upstream + " (" + m.fallback + ")"
            : m.upstream;
          return m.paused ? "已暂停 · " + label : label;
        });
        var models = modelNames.length
          ? "<div class='target-model-list'>" +
            modelNames
              .map(function (name) {
                return (
                  "<button type='button' class='target-model-badge'>" +
                  esc(name) +
                  "</button>"
                );
              })
              .join("") +
            "</div>"
          : "<span style='color:var(--text-muted)'>不限</span>";
        var extraKeyCount = Array.isArray(t.api_keys) ? t.api_keys.length : 0;
        var totalKeyCount = (t.has_api_key ? 1 : 0) + extraKeyCount;
        var keyHint = t.allow_bearer_passthrough
          ? "支持 Bearer 透传"
          : t.auth_mode === "bearer"
            ? "Bearer 认证"
            : "API Key 认证";
        var keyStatus =
          "<span title='" +
          esc(keyHint) +
          "' style='background:var(--accent-soft);color:var(--accent);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
          totalKeyCount +
          " keys</span>";
        var keyPoolInfo = "";
        if (Array.isArray(t.key_statuses) && t.key_statuses.length > 0) {
          var keyStatuses = t.key_statuses.filter(function (s) { return !!s && typeof s === "object"; });
          var blockedCount = keyStatuses.filter(function (s) {
            return s.blocked;
          }).length;
          var quotaSubCount = keyStatuses.filter(function (s) {
            return (
              keyStatusNeedsAttention(s) &&
              s.exhaust_reason === "quota_exceeded_subscription"
            );
          }).length;
          var quotaAPICount = keyStatuses.filter(function (s) {
            return (
              keyStatusNeedsAttention(s) &&
              s.exhaust_reason === "quota_exceeded_api"
            );
          }).length;
          var quotaCount = keyStatuses.filter(function (s) {
            return (
              keyStatusNeedsAttention(s) &&
              s.exhaust_reason === "quota_exceeded"
            );
          }).length;
          var rateCount = keyStatuses.filter(function (s) {
            return (
              keyStatusNeedsAttention(s) && s.exhaust_reason === "rate_limited"
            );
          }).length;
          // GLM 限流细分：额度类（5h/周月/未知）与速率类（QPS），与前端 keyStatusColor 文案一一对应
          var glmQuotaCount = keyStatuses.filter(function (s) {
            return (
              keyStatusNeedsAttention(s) &&
              !s.blocked &&
              (s.exhaust_reason === "glm_rate_limit_5h" ||
                s.exhaust_reason === "glm_rate_limit_quota" ||
                s.exhaust_reason === "glm_rate_limit_unknown")
            );
          }).length;
          var glmRateCount = keyStatuses.filter(function (s) {
            return (
              keyStatusNeedsAttention(s) &&
              s.exhaust_reason === "glm_rate_limit_qps"
            );
          }).length;
          // 其他失效原因：invalid_token / billing_error / account_disabled 等（5 分钟冷却自动恢复）
          // 排除所有已命名类（含 glm_ 前缀），避免 GLM reason 被误算进"失效"
          var otherExhaustedCount = keyStatuses.filter(function (s) {
            if (!keyStatusNeedsAttention(s) || s.blocked) return false;
            var r = s.exhaust_reason;
            if (
              r === "quota_exceeded_subscription" ||
              r === "quota_exceeded_api" ||
              r === "quota_exceeded" ||
              r === "rate_limited"
            )
              return false;
            // 排除所有 glm_ 前缀 reason（已由 glmQuotaCount/glmRateCount 覆盖）
            if (r && r.indexOf("glm_") === 0) return false;
            return true;
          }).length;
          if (blockedCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--bad-soft);color:var(--bad);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              blockedCount +
              " 屏蔽</span>";
          if (quotaSubCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--warn-soft);color:var(--warn);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              quotaSubCount +
              " 超限(自动)</span>";
          if (quotaAPICount > 0)
            keyPoolInfo +=
              " <span style='background:var(--warn-soft);color:var(--warn);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              quotaAPICount +
              " 超限(人工)</span>";
          if (quotaCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--warn-soft);color:var(--warn);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              quotaCount +
              " 超限</span>";
          if (glmQuotaCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--warn-soft);color:var(--warn);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              glmQuotaCount +
              " GLM额度限流</span>";
          if (glmRateCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--out-soft);color:var(--out);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              glmRateCount +
              " GLM速率限流</span>";
          if (rateCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--out-soft);color:var(--out);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              rateCount +
              " 限流</span>";
          if (otherExhaustedCount > 0)
            keyPoolInfo +=
              " <span style='background:var(--accent-soft);color:var(--accent);padding:1px 6px;border-radius:4px;font-size:11px;'>" +
              otherExhaustedCount +
              " 失效</span>";
          // 唤醒按钮：当存在任何被屏蔽/超额/失效的 key 时显示（探测成功才会恢复）
          var exhaustedCount =
            blockedCount +
            quotaSubCount +
            quotaAPICount +
            quotaCount +
            rateCount +
            glmQuotaCount +
            glmRateCount +
            otherExhaustedCount;
          if (exhaustedCount > 0) {
            keyPoolInfo +=
              " <button class='btn btn-sm readonly-hide' data-wakeup-target='" +
              esc(t.name) +
              "' title='唤醒探测被屏蔽/失效的Key' style='padding:1px 6px;font-size:11px;background:var(--ok);color:var(--accent-ink);border:none;border-radius:4px;cursor:pointer;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><path d='M20 7v5h-5M4 17v-5h5'></path><path d='M6.1 7a7 7 0 0 1 11.5-1L20 9M4 15l2.4 3a7 7 0 0 0 11.5-1'></path></svg> 唤醒</button>";
          }
        }
        keyStatus += keyPoolInfo;
        var phaseCell = "";
        var schedCell = "<span style='color:var(--text-muted)'>—</span>";
        var rt = state.runtime
          ? state.runtime[String(t.name || "").toLowerCase()]
          : null;
        if (!rt || !targetPhaseKnown(rt.phase)) {
          phaseCell =
            "<span style='background:var(--muted-soft);color:var(--ink-3);padding:2px 8px;border-radius:4px;font-size:12px;'>运行态不可用</span> ";
        } else {
          var phaseStyles = {
            ready: { bg: "var(--ok-soft)", fg: "var(--ok)" },
            muted: { bg: "var(--warn-soft)", fg: "var(--warn)" },
            "half-open": { bg: "var(--out-soft)", fg: "var(--out)" },
            offline: { bg: "var(--muted-soft)", fg: "var(--ink-3)" },
          };
          var ph = phaseStyles[rt.phase] || {
            bg: "var(--muted-soft)",
            fg: "var(--ink-3)",
          };
          ph.label = schemaLabel("target_phases", rt.phase);
          phaseCell =
            "<span style='background:" +
            ph.bg +
            ";color:" +
            ph.fg +
            ";padding:2px 8px;border-radius:4px;font-size:12px;'>" +
            esc(ph.label) +
            "</span> ";
          var recent = rt.window_15m;
          if (recent) {
            schedCell =
              "<span style='font-size:12px;' title='过去15分钟在途峰值'>在途 " +
              recent.in_flight_peak +
              "</span>" +
              " <span style='font-size:12px;color:var(--accent);' title='过去15分钟粘连回粘命中次数'>亲和 " +
              recent.affinity_hits +
              "</span>" +
              " <span style='font-size:12px;color:" +
              (recent.breaker_trips > 0 ? "var(--bad)" : "var(--ink-3)") +
              ";' title='过去15分钟进入或延长冷却的次数'>熔断 " +
              recent.breaker_trips +
              "</span>";
          }
        }
        // The switch edits only the saved target pause flag. Effective pause can
        // also come from the pool or runtime; showing that as this switch would
        // let a resume click clear the pool-pause symptom while changing nothing.
        var ownPaused = !!t.paused;
        var pauseSwitch =
          "<button type='button' role='switch' class='target-toggle readonly-disable' aria-checked='" +
          !ownPaused +
          "' aria-label='目标「" +
          esc(t.name) +
          "」自身暂停状态，当前" +
          (ownPaused ? "已暂停" : "未暂停") +
          "' title='只控制目标自身暂停设置；部署池暂停只影响运行态和健康状态' " +
          (ownPaused ? "data-resume-tgt='" : "data-pause-tgt='") +
          esc(t.name) +
          "'" +
          (isReadonlyMode() ? " disabled" : "") +
          ">" +
          "<span class='target-toggle-track' aria-hidden='true'></span><span>" +
          (ownPaused ? "目标已暂停" : "目标启用") +
          "</span></button>";
        return (
          "<tr data-target-health='" +
          esc(health.key) +
          "'><td><div class='target-status-cell'>" +
          pauseSwitch +
          "<span class='target-health-badge tone-" +
          esc(health.tone) +
          "' title='" +
          esc(health.reason) +
          "'>" +
          esc(health.label) +
          "</span></div></td>" +
          "<td class='target-text' title='" +
          esc(t.name) +
          "'>" +
          "<button type='button' class='target-detail-link' data-detail-tgt='" +
          esc(t.name) +
          "'>" +
          esc(t.name) +
          "</button>" +
          "<span class='target-endpoint-text' title='" +
          esc(t.endpoint || "") +
          "'>" +
          esc(t.endpoint ? t.endpoint + (isReadonlyMode() ? "（仅显示协议和主机）" : "") :
            isReadonlyMode() ? "Endpoint 不可查看" : "未配置 Endpoint") +
          "</span></td>" +
          "<td><div class='target-cell-content'>" +
          keyStatus +
          "</div></td>" +
          "<td><span class='badge target-type-badge' style='background:var(--primary-light);color:var(--primary);padding:2px 8px;border-radius:4px;font-size:12px;'>" +
          esc(epDisplayName(t.endpoint_type)) +
          "</span></td>" +
          "<td class='target-text' title='" +
          esc(poolDisplayName(t.pool || "")) +
          "'>" +
          poolCell +
          "</td>" +
          "<td><div class='target-cell-content'>" +
          phaseCell +
          schedCell +
          "</div></td>" +
          "<td><div class='target-cell-content'>" +
          models +
          "</div></td>" +
          "<td><div class='btn-group'>" +
          "<button class='btn btn-sm readonly-hide' data-edit-tgt='" +
          esc(t.name) +
          "'>编辑</button>" +
          "<button class='btn btn-danger btn-sm readonly-hide' data-del-tgt='" +
          esc(t.name) +
          "'>删除</button>" +
          "</div></td></tr>"
        );
      })
      .join(""));
    refreshOpenTargetDetail();
  }
  // Mount outside the scrolling cell so the full name is never clipped by the table.
  var modelHint = document.createElement("div");
  modelHint.id = "targetModelHint";
  modelHint.className = "target-model-hint";
  modelHint.setAttribute("role", "tooltip");
  modelHint.hidden = true;
  document.body.appendChild(modelHint);
  var modelHintAnchor = null;
  function hideModelHint() {
    if (modelHintAnchor) modelHintAnchor.removeAttribute("aria-describedby");
    modelHintAnchor = null;
    modelHint.hidden = true;
  }
  function showModelHint(badge) {
    hideModelHint();
    modelHintAnchor = badge;
    modelHint.textContent = badge.textContent;
    badge.setAttribute("aria-describedby", modelHint.id);
    modelHint.hidden = false;
    var rect = badge.getBoundingClientRect();
    var box = modelHint.getBoundingClientRect();
    modelHint.style.left =
      Math.max(8, Math.min(rect.left, innerWidth - box.width - 8)) + "px";
    modelHint.style.top =
      Math.max(
        8,
        rect.bottom + box.height + 8 <= innerHeight
          ? rect.bottom + 4
          : rect.top - box.height - 4,
      ) + "px";
  }
  var modelHintTable = document.getElementById("targetsTableBody");
  ["mouseover", "focusin", "click"].forEach(function (eventName) {
    modelHintTable.addEventListener(eventName, function (event) {
      var badge = event.target.closest(".target-model-badge");
      if (badge) showModelHint(badge);
    });
  });
  modelHintTable.addEventListener("mouseout", function (event) {
    if (
      !modelHintAnchor ||
      modelHintAnchor.contains(event.relatedTarget) ||
      document.activeElement === modelHintAnchor
    )
      return;
    hideModelHint();
  });
  modelHintTable.addEventListener("focusout", hideModelHint);
  document.addEventListener("click", function (event) {
    if (!event.target.closest(".target-model-badge")) hideModelHint();
  });
  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") hideModelHint();
  });
  window.addEventListener("resize", hideModelHint);
  document.addEventListener("scroll", hideModelHint, true);

  // Row actions that must not run twice at once (double click, repeated confirm).
  var WAKEUP_TIMEOUT_MS = 10 * 60 * 1000;
  var rowActionsInFlight = Object.create(null);
  function beginRowAction(key) {
    if (rowActionsInFlight[key]) return false;
    rowActionsInFlight[key] = true;
    return true;
  }
  function endRowAction(key) {
    delete rowActionsInFlight[key];
  }
  // ===== 供应商 → 模型 → 费用/计费 层级入口（二级弹窗） =====
  document
    .getElementById("targetsTableBody")
    .addEventListener("click", function (e) {
      var t = e.target.closest("button") || e.target;
      if (
        t.disabled ||
        (isReadonlyMode() && (t.dataset.pauseTgt || t.dataset.resumeTgt))
      )
        return;
      if (t.dataset.mcTgt) {
      }
      if (t.dataset.detailTgt) {
        openTargetDetail(t.dataset.detailTgt);
      }
      if (t.dataset.editTgt) {
        var tgt = state.targets.find(function (x) {
          return x.name === t.dataset.editTgt;
        });
        if (tgt) openTargetModal(tgt);
      }
      if (t.dataset.delTgt) {
        var name = t.dataset.delTgt;
        showConfirm(
          "确认删除目标「" + name + "」？此操作不可恢复。",
          async function () {
            try {
              await api("/admin/data/targets/" + encodeURIComponent(name), {
                method: "DELETE",
              });
              toast("已删除目标: " + name, "success");
              await loadTargets();
            } catch (e) {
              toast("删除失败: " + e.message, "error");
            }
          },
        );
      }
      if (t.dataset.pauseTgt) {
        var pauseName = t.dataset.pauseTgt;
        showConfirm(
          "确认暂停目标「" + pauseName + "」？暂停后不会转发到该上游。",
          async function () {
            try {
              await api(
                "/admin/data/targets/" +
                  encodeURIComponent(pauseName) +
                  "/pause",
                { method: "PUT" },
              );
              toast("已暂停目标: " + pauseName, "success");
              await loadTargets();
            } catch (e) {
              toast("暂停失败: " + e.message, "error");
            }
          },
        );
      }
      if (t.dataset.resumeTgt) {
        var resumeName = t.dataset.resumeTgt;
        if (!beginRowAction("resume:" + resumeName)) return;
        (async function () {
          try {
            await api(
              "/admin/data/targets/" +
                encodeURIComponent(resumeName) +
                "/resume",
              { method: "PUT" },
            );
            toast("已恢复目标: " + resumeName, "success");
            await loadTargets();
          } catch (e) {
            toast("恢复失败: " + e.message, "error");
          } finally {
            endRowAction("resume:" + resumeName);
          }
        })();
      }
      // 唤醒按钮处理
      if (t.dataset.wakeupTarget) {
        var name = t.dataset.wakeupTarget;
        showConfirm(
          "确认对目标「" +
            name +
            "」执行唤醒探测？\n将尝试恢复所有被屏蔽/超额的 Key。",
          async function () {
            if (!beginRowAction("wakeup:" + name)) return;
            try {
              var result = await api(
                "/admin/data/targets/" +
                  encodeURIComponent(name) +
                  "/keys/wakeup",
                // The server probes keys one by one (5s each). The deadline is
                // generous but finite so a dead connection cannot hang the UI.
                { method: "POST", timeoutMs: WAKEUP_TIMEOUT_MS },
              );
              var recovered = result.recovered || 0;
              toast("唤醒完成: 恢复了 " + recovered + " 个 Key", "success");
              await loadTargets();
            } catch (e) {
              toast("唤醒失败: " + e.message, "error");
            } finally {
              endRowAction("wakeup:" + name);
            }
          },
        );
      }
    });
  document
    .getElementById("newTargetBtn")
    .addEventListener("click", function () {
      openTargetModal(null);
    });
  document
    .getElementById("refreshTargetsBtn")
    .addEventListener("click", function () {
      loadTargets();
    });
  document
    .getElementById("targetsMainTab")
    .addEventListener("click", function () {
      switchTargetPageTab("targets");
    });
  document
    .getElementById("targetsPoolsTab")
    .addEventListener("click", function () {
      switchTargetPageTab("pools");
    });
  ["targetsMainTab", "targetsPoolsTab"].forEach(function (id) {
    document.getElementById(id).addEventListener("keydown", function (event) {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      event.preventDefault();
      switchTargetPageTab(id === "targetsMainTab" ? "pools" : "targets");
      document
        .getElementById(id === "targetsMainTab" ? "targetsPoolsTab" : "targetsMainTab")
        .focus();
    });
  });
  document
    .getElementById("targetStatusFilters")
    .addEventListener("click", function (event) {
      var chip = event.target.closest("[data-target-filter]");
      if (chip) setTargetFilter(chip.dataset.targetFilter);
    });
  return {
    renderTargetStatusFilters: renderTargetStatusFilters,
    setTargetFilter: setTargetFilter,
    switchTargetPageTab: switchTargetPageTab,
    renderTargets: renderTargets,
    hideModelHint: hideModelHint,
    showModelHint: showModelHint,
  };
});
