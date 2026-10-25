"use strict";
// Targets owns target CRUD, key cards, model mappings and provider capability controls.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("targets", function (app) {
  var state = app.state;
  var maskKey = app.bind("core", "maskKey");
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var epMeta = app.bind("schema", "epMeta");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var setSchemaSelectValue = app.bind("schema", "setSchemaSelectValue");
  var poolMemberCount = app.bind("pools", "poolMemberCount");
  var poolDisplayName = app.bind("pools", "poolDisplayName");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var loadCatalog = app.bind("catalog", "loadCatalog");
  var catalogFilterModels = app.bind("catalog", "catalogFilterModels");
  async function loadTargets() {
    try {
      var fetches = [api("/admin/data/targets"), fetchSchedulingRuntime()];
      // 池徽章与池下拉依赖池数据，缺失时一并拉取。
      if (!state.pools || !state.pools.length) {
        fetches.push(
          api("/admin/data/pools").catch(function () {
            return { pools: [] };
          }),
        );
      }
      var results = await Promise.all(fetches);
      state.targets = Array.isArray(results[0].targets)
        ? results[0].targets
        : [];
      if (results[2] && Array.isArray(results[2].pools)) {
        state.pools = results[2].pools;
      }
      renderTargets();
    } catch (e) {
      toast("加载目标失败: " + e.message, "error");
    }
  }
  // fetchSchedulingRuntime：拉取 /admin/metrics 的 targets_runtime 调度观测快照
  // （S5），失败时保留旧数据（观测是尽力而为，不阻塞管理台主流程）。
  async function fetchSchedulingRuntime() {
    try {
      var m = await api("/admin/metrics");
      var map = {};
      if (Array.isArray(m.targets_runtime)) {
        m.targets_runtime.forEach(function (rt) {
          map[String(rt.name || "").toLowerCase()] = rt;
        });
      }
      state.runtime = map;
    } catch (e) {
      /* 指标缺失不影响列表功能 */
    }
    return null;
  }
  // refreshEditingTarget：block/unblock 等改变了 key 运行时状态后，
  // 从后端重新拉取最新数据，同步更新列表 + 编辑模态框内的 key 卡片。
  // 不手动改本地状态（之前手动改 key_statuses 的方式会在 key_statuses
  // 缺失/字段不一致时导致"刷新才生效"和"要点两次"的问题）。
  async function refreshEditingTarget() {
    // 先保存用户在本次编辑中新增但尚未保存的 key（无 ref 字段），
    // 避免从后端重建 tgtKeyList 时丢失。
    var newKeys = tgtKeyList
      .filter(function (item) {
        return !item.ref;
      })
      .map(function (item) {
        return { value: item.value, status: null };
      });
    await loadTargets();
    if (state.editingTarget) {
      var fresh = state.targets.find(function (x) {
        return x.name === state.editingTarget.name;
      });
      if (fresh) {
        state.editingTarget = fresh;
        // 重建 tgtKeyList（与 openTargetModal 逻辑一致）
        var keyStatuses = Array.isArray(fresh.key_statuses)
          ? fresh.key_statuses
          : [];
        tgtKeyList = [];
        if (fresh.api_key) {
          var ks0 =
            keyStatuses.find(function (s) {
              return s.index === 0;
            }) || null;
          tgtKeyList.push({
            value: fresh.api_key,
            ref: "__existing_key_index__:0",
            status: ks0,
          });
        }
        if (Array.isArray(fresh.api_keys)) {
          fresh.api_keys.forEach(function (k, i) {
            if (!k) return;
            if (fresh.api_key && k === fresh.api_key) return;
            var ksIdx =
              keyStatuses.find(function (s) {
                return s.index === i + 1;
              }) || null;
            tgtKeyList.push({
              value: k,
              ref: "__existing_key_index__:" + (i + 1),
              status: ksIdx,
            });
          });
        }
        // 追加用户新增但尚未保存的 key
        newKeys.forEach(function (nk) {
          var exists = tgtKeyList.some(function (item) {
            return item.value === nk.value;
          });
          if (!exists) tgtKeyList.push(nk);
        });
        renderKeyList();
      }
    }
  }
  /* ===== Key Card List (target modal) ===== */
  var tgtKeyList = []; // [{value: "sk-xxx", ref: "__existing_key_index__:0", status: {index, key_mask, status, blocked, exhausted, exhaust_reason, cooldown_end}}]

  function keyStatusColor(s) {
    switch (s) {
      case "已屏蔽":
        return "background:var(--bad-soft);color:var(--bad)";
      case "额度超限":
        return "background:var(--warn-soft);color:var(--warn)";
      case "额度超限(自动恢复)":
        return "background:var(--warn-soft);color:var(--warn)";
      case "额度超限(人工恢复)":
        return "background:var(--warn-soft);color:var(--warn)";
      case "限流中":
        return "background:var(--out-soft);color:var(--out)";
      case "使用中":
        return "background:var(--ok-soft);color:var(--ok)";
      case "等待中":
        return "background:var(--muted-soft);color:var(--ink-3)";
      case "使用过":
        return "background:var(--accent-soft);color:var(--accent)";
      // 新增文案（status() GLM 分类 / 冷却过期区分）
      case "可用(待激活)":
        return "background:var(--accent-soft);color:var(--accent)"; // 浅蓝（sky-50/sky-700），区别于"使用中"绿色但仍属"接近可用"的冷色调
      case "失效(待请求验证)":
        return "background:var(--bad-soft);color:var(--bad)"; // 红色系警示
      case "失效(冷却中)":
        return "background:var(--out-soft);color:var(--out)"; // 橙色系
      case "GLM额度限流":
        return "background:var(--warn-soft);color:var(--warn)"; // 黄色系额度超限色
      case "GLM速率限流":
        return "background:var(--out-soft);color:var(--out)"; // 橙色系限流色
      case "失效":
        return "background:var(--bad-soft);color:var(--bad)"; // 兜底失效
      default:
        return "background:var(--muted-soft);color:var(--ink-3)";
    }
  }

  function renderKeyList() {
    var container = document.getElementById("keyCardList");
    if (!tgtKeyList.length) {
      container.style.display = "none";
      container.innerHTML = "";
      return;
    }
    container.style.display = "block";
    var isEditing = !!state.editingTarget;
    container.innerHTML = tgtKeyList
      .map(function (item, i) {
        var mask = item.status ? item.status.key_mask : maskKey(item.value);
        var statusBadge = "";
        if (item.status) {
          statusBadge =
            "<span style='" +
            keyStatusColor(item.status.status) +
            ";padding:2px 8px;border-radius:10px;font-size:11px;font-weight:600;white-space:nowrap;'>" +
            esc(item.status.status) +
            "</span>";
        }
        var actions = "";
        if (isEditing && item.status) {
          var idx = item.status.index;
          if (item.status.blocked || item.status.exhausted) {
            actions =
              "<button class='btn btn-sm readonly-hide' data-unblock-key-idx='" +
              idx +
              "' title='解除屏蔽' aria-label='解除屏蔽' style='padding:2px 8px;font-size:11px;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='4' y='11' width='16' height='10' rx='2'></rect><path d='M8 11V7a4 4 0 0 1 7.5-2'></path></svg></button>";
          } else {
            actions =
              "<button class='btn btn-sm readonly-hide' data-block-key-idx='" +
              idx +
              "' title='屏蔽' aria-label='屏蔽' style='padding:2px 8px;font-size:11px;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='4' y='11' width='16' height='10' rx='2'></rect><path d='M8 11V7a4 4 0 0 1 8 0v4'></path></svg></button>";
          }
        }
        var removeBtn =
          "<button class='btn btn-sm readonly-hide' data-remove-key-idx='" +
          i +
          "' title='移除' aria-label='移除' style='padding:2px 8px;font-size:11px;color:var(--danger);'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><path d='M6 6l12 12M18 6L6 18'></path></svg></button>";
        var cooldownInfo = "";
        if (item.status && item.status.exhausted && item.status.cooldown_end) {
          var cd = new Date(item.status.cooldown_end);
          var now = new Date();
          if (cd > now) {
            var mins = Math.ceil((cd - now) / 60000);
            cooldownInfo =
              "<span style='font-size:11px;color:var(--text-muted);'>" +
              mins +
              "min</span>";
          }
        }
        return (
          "<div style='display:flex;align-items:center;gap:8px;padding:6px 10px;border:1px solid var(--line);border-radius:6px;margin-bottom:4px;background:var(--surface-2);'>" +
          "<span style='font-family:ui-monospace, SF Mono, Menlo, Consolas, monospace;font-size:12px;color:var(--text-secondary);flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;' title='" +
          esc(mask) +
          "'>" +
          esc(mask) +
          "</span>" +
          statusBadge +
          cooldownInfo +
          actions +
          removeBtn +
          "</div>"
        );
      })
      .join("");
  }

  function addKeyToList() {
    var input = document.getElementById("tgtNewKeyInput");
    var val = input.value.trim();
    if (!val) return;
    // dedup
    for (var i = 0; i < tgtKeyList.length; i++) {
      if (tgtKeyList[i].value === val) {
        toast("Key 已存在", "error");
        return;
      }
    }
    tgtKeyList.push({ value: val, status: null });
    input.value = "";
    renderKeyList();
  }

  function removeKeyFromList(idx) {
    tgtKeyList.splice(idx, 1);
    renderKeyList();
  }

  document.getElementById("addKeyBtn").addEventListener("click", addKeyToList);
  document
    .getElementById("tgtNewKeyInput")
    .addEventListener("keydown", function (e) {
      if (e.key === "Enter") {
        e.preventDefault();
        addKeyToList();
      }
    });

  // Event delegation for key card actions (block/unblock/remove)
  document
    .getElementById("keyCardList")
    .addEventListener("click", function (e) {
      var t = e.target;
      if (t.dataset.removeKeyIdx !== undefined) {
        removeKeyFromList(parseInt(t.dataset.removeKeyIdx, 10));
      }
      if (t.dataset.blockKeyIdx !== undefined && state.editingTarget) {
        var idx = parseInt(t.dataset.blockKeyIdx, 10);
        var name = state.editingTarget.name;
        showConfirm(
          "确认屏蔽此 Key？屏蔽后该 Key 将不参与转发，直到手动解除。",
          async function () {
            try {
              await api(
                "/admin/data/targets/" +
                  encodeURIComponent(name) +
                  "/keys/" +
                  idx +
                  "/block",
                { method: "PUT" },
              );
              toast("已屏蔽 Key", "success");
              await refreshEditingTarget();
            } catch (err) {
              toast("操作失败: " + err.message, "error");
            }
          },
        );
      }
      if (t.dataset.unblockKeyIdx !== undefined && state.editingTarget) {
        var idx2 = parseInt(t.dataset.unblockKeyIdx, 10);
        var name2 = state.editingTarget.name;
        showConfirm(
          "确认解除屏蔽此 Key？解除后该 Key 将重新参与转发。",
          async function () {
            try {
              await api(
                "/admin/data/targets/" +
                  encodeURIComponent(name2) +
                  "/keys/" +
                  idx2 +
                  "/unblock",
                { method: "PUT" },
              );
              toast("已解除屏蔽", "success");
              await refreshEditingTarget();
            } catch (err) {
              toast("操作失败: " + err.message, "error");
            }
          },
        );
      }
    });

  function renderTargets() {
    if (modelHint) hideModelHint();
    var tb = document.getElementById("targetsTableBody");
    if (!state.targets.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='9'>暂无目标配置</td></tr>";
      return;
    }
    tb.innerHTML = state.targets
      .slice()
      .sort(function (a, b) {
        return (
          Number(!!a.paused) - Number(!!b.paused) ||
          String(a.name || "").localeCompare(String(b.name || ""))
        );
      })
      .map(function (t) {
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
          var blockedCount = t.key_statuses.filter(function (s) {
            return s.blocked;
          }).length;
          var quotaSubCount = t.key_statuses.filter(function (s) {
            return (
              s.exhausted && s.exhaust_reason === "quota_exceeded_subscription"
            );
          }).length;
          var quotaAPICount = t.key_statuses.filter(function (s) {
            return s.exhausted && s.exhaust_reason === "quota_exceeded_api";
          }).length;
          var quotaCount = t.key_statuses.filter(function (s) {
            return s.exhausted && s.exhaust_reason === "quota_exceeded";
          }).length;
          var rateCount = t.key_statuses.filter(function (s) {
            return s.exhausted && s.exhaust_reason === "rate_limited";
          }).length;
          // GLM 限流细分：额度类（5h/周月/未知）与速率类（QPS），与前端 keyStatusColor 文案一一对应
          var glmQuotaCount = t.key_statuses.filter(function (s) {
            return (
              s.exhausted &&
              !s.blocked &&
              (s.exhaust_reason === "glm_rate_limit_5h" ||
                s.exhaust_reason === "glm_rate_limit_quota" ||
                s.exhaust_reason === "glm_rate_limit_unknown")
            );
          }).length;
          var glmRateCount = t.key_statuses.filter(function (s) {
            return (
              s.exhausted &&
              !s.blocked &&
              s.exhaust_reason === "glm_rate_limit_qps"
            );
          }).length;
          // 其他失效原因：invalid_token / billing_error / account_disabled 等（5 分钟冷却自动恢复）
          // 排除所有已命名类（含 glm_ 前缀），避免 GLM reason 被误算进"失效"
          var otherExhaustedCount = t.key_statuses.filter(function (s) {
            if (!s.exhausted || s.blocked) return false;
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
        if (rt) {
          var phaseStyles = {
            ready: { bg: "var(--ok-soft)", fg: "var(--ok)" },
            muted: { bg: "var(--warn-soft)", fg: "var(--warn)" },
            "half-open": { bg: "var(--out-soft)", fg: "var(--out)" },
            offline: { bg: "var(--muted-soft)", fg: "var(--ink-3)" },
          };
          var ph = phaseStyles[rt.phase] || { bg: "var(--muted-soft)", fg: "var(--ink-3)" };
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
        var pauseSwitch =
          "<button type='button' role='switch' class='target-toggle readonly-disable' aria-checked='" +
          !t.paused +
          "' aria-label='" +
          esc(t.name) +
          " 使用状态' " +
          (t.paused ? "data-resume-tgt='" : "data-pause-tgt='") +
          esc(t.name) +
          "'" +
          (isReadonlyMode() ? " disabled" : "") +
          ">" +
          "<span class='target-toggle-track' aria-hidden='true'></span><span>" +
          (t.paused ? "暂停" : "使用") +
          "</span></button>";
        return (
          "<tr><td>" +
          pauseSwitch +
          "</td><td class='target-text' title='" +
          esc(t.name) +
          "'>" +
          esc(t.name) +
          "</td>" +
          "<td><div class='target-cell-content'>" +
          phaseCell +
          keyStatus +
          "</div></td>" +
          "<td><span class='badge target-type-badge' style='background:var(--primary-light);color:var(--primary);padding:2px 8px;border-radius:4px;font-size:12px;'>" +
          esc(epDisplayName(t.endpoint_type)) +
          "</span></td>" +
          "<td style='max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;' title='" +
          esc(t.endpoint) +
          "'>" +
          esc(t.endpoint) +
          "</td>" +
          "<td class='target-text' title='" +
          esc(poolDisplayName(t.pool || "")) +
          "'>" +
          poolCell +
          "</td>" +
          "<td><div class='target-cell-content'>" +
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
      .join("");
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
            try {
              var result = await api(
                "/admin/data/targets/" +
                  encodeURIComponent(name) +
                  "/keys/wakeup",
                { method: "POST" },
              );
              var recovered = result.recovered || 0;
              toast("唤醒完成: 恢复了 " + recovered + " 个 Key", "success");
              await loadTargets();
            } catch (e) {
              toast("唤醒失败: " + e.message, "error");
            }
          },
        );
      }
    });
  function updateTargetFormVisibility() {
    var epType = document.getElementById("tgtType").value;
    var meta = epMeta(epType) || {};
    document.getElementById("tgtCapabilities").textContent =
      "协议：" +
      (meta.protocols || [])
        .map(function (value) {
          return schemaLabel("protocols", value);
        })
        .join(" / ") +
      "；请求格式：" +
      (meta.request_schemas || [])
        .map(function (value) {
          return schemaLabel("request_schemas", value);
        })
        .join(" / ");
    var supportsBearer = !!meta.supports_bearer_passthrough;
    var supportsAuthMode = !!meta.supports_auth_mode;
    var supportsImageOperation = !!meta.supports_image_operation;
    document.getElementById("tgtBearerGroup").style.display = supportsBearer
      ? ""
      : "none";
    document.getElementById("tgtAuthModeGroup").style.display = supportsAuthMode
      ? ""
      : "none";
    document.getElementById("tgtImageOperationGroup").style.display =
      supportsImageOperation ? "" : "none";
    document.getElementById("tgtPreferWSGroup").style.display =
      meta.supports_prefer_websocket ? "" : "none";
    // Visibility is capability-driven; switching type must not clear persisted values.
    document.getElementById("tgtDualProtocolGroup").style.display =
      meta.supports_dual_protocol ? "" : "none";
    document.getElementById("tgtStreamUsageGroup").style.display =
      (meta.request_schemas || []).includes(state.schema.stream_usage_request_schema) ? "" : "none";
    // API Keys 和冷却期对所有 provider 类型都显示（统一 keypool 亲和机制）
    applyEndpointPlaceholder(epType);
    renderTgtModelSuggestions(epType);
  }
  // applyEndpointPlaceholder 按 endpoint_type 给 Endpoint URL 输入框替换 placeholder 与提示文案。
  // 通用模板：每个 provider 给出官方文档推荐的 base_url 示例和必要的接入提示。
  // 不再硬编码 Azure 描述。
  function applyEndpointPlaceholder(epType) {
    var ep = document.getElementById("tgtEndpoint");
    var hint = document.getElementById("tgtEndpointHint");
    var meta = epMeta(epType) || {};
    if (ep) ep.placeholder = meta.endpoint_placeholder || "";
    if (hint) hint.textContent = meta.endpoint_hint || "";
  }
  async function renderTgtModelSuggestions(epType, filterText) {
    var container = document.getElementById("tgtModelSuggestions");
    container.innerHTML = "";
    var models = await loadCatalog(epType);
    var filtered = catalogFilterModels(models, filterText);
    filtered.slice(0, 50).forEach(function (m) {
      var tag = document.createElement("span");
      tag.style.cssText =
        "display:inline-block;padding:2px 8px;border-radius:4px;background:var(--primary-light);color:var(--primary);font-size:12px;cursor:pointer;";
      tag.textContent = m.model || m.display_name || "";
      tag.title = m.display_name || m.model;
      tag.addEventListener("click", function () {
        var modelName = m.model;
        // Skip if this model already exists in some row
        var existingInputs = document.querySelectorAll(
          "#model-mappings-container .upstream-input",
        );
        for (var i = 0; i < existingInputs.length; i++) {
          if (existingInputs[i].value.trim() === modelName) return;
        }
        addModelMappingRow(modelName, "", !!state.editingTarget);
      });
      container.appendChild(tag);
    });
    if (filterText && filterText.trim() && filtered.length === 0) {
      container.innerHTML =
        "<span style='color:var(--text-muted);font-size:12px;'>无匹配模型</span>";
    }
  }
  // addModelMappingRow creates a new row in the model-mappings editor.
  // Uses DOM API (createElement + .value) to prevent XSS from special characters.
  // showTest: 仅在编辑已保存的 target 时显示测试/暂停按钮（新建时无 target 名无法测试）。
  // paused: 该模型已暂停（不可被请求），行内显示删除线并可一键恢复。
  function addModelMappingRow(upstream, fallback, showTest, paused) {
    var container = document.getElementById("model-mappings-container");
    var row = document.createElement("div");
    row.className = "model-mapping-row";

    var upstreamInput = document.createElement("input");
    upstreamInput.type = "text";
    upstreamInput.className = "upstream-input";
    upstreamInput.placeholder = "上游名称（必填）";
    upstreamInput.value = upstream || "";
    upstreamInput.setAttribute("list", "catalogModelDatalist"); // catalog 软建议，保留自由输入（上游私有模型名不在目录内）
    upstreamInput.autocomplete = "off";

    var fallbackInput = document.createElement("input");
    fallbackInput.type = "text";
    fallbackInput.className = "fallback-input";
    fallbackInput.placeholder = "下游兜底（可选）";
    fallbackInput.value = fallback || "";

    var removeBtn = document.createElement("button");
    removeBtn.type = "button";
    removeBtn.className = "btn-remove-row";
    removeBtn.textContent = "\u00d7";
    removeBtn.addEventListener("click", function () {
      row.remove();
    });

    function applyPausedStyle() {
      var isPaused = row.dataset.paused === "1";
      upstreamInput.style.textDecoration = isPaused ? "line-through" : "";
      upstreamInput.style.opacity = isPaused ? "0.55" : "";
      fallbackInput.style.opacity = isPaused ? "0.55" : "";
    }
    row.dataset.paused = paused ? "1" : "";
    applyPausedStyle();

    row.appendChild(upstreamInput);
    row.appendChild(fallbackInput);
    if (showTest) {
      var pauseBtn = document.createElement("button");
      pauseBtn.type = "button";
      pauseBtn.className = "btn btn-sm pause-model-btn";
      pauseBtn.title =
        "暂停后该模型（含别名）不可被请求；与负载均衡无关，仅控制可用性";
      function renderPauseBtn() {
        var isPaused = row.dataset.paused === "1";
        pauseBtn.textContent = isPaused ? "恢复" : "暂停";
        pauseBtn.classList.toggle("btn-danger", !isPaused);
      }
      renderPauseBtn();
      pauseBtn.addEventListener("click", function () {
        row.dataset.paused = row.dataset.paused === "1" ? "" : "1";
        renderPauseBtn();
        applyPausedStyle();
      });
      row.appendChild(pauseBtn);
      var testBtn = document.createElement("button");
      testBtn.type = "button";
      testBtn.className = "btn btn-sm test-model-btn";
      testBtn.textContent = "测试";
      testBtn.title = "此检测发送最小 HTTP 请求检查上游响应；不验证 WebSocket 通道";
      testBtn.addEventListener("click", function () {
        testModelForRow(testBtn, upstreamInput.value.trim());
      });
      row.appendChild(testBtn);
    }
    row.appendChild(removeBtn);
    container.appendChild(row);
  }

  // testModelForRow 调用后端 test-model API 并弹 toast 反馈结果。
  function testModelForRow(btn, model) {
    if (!model) {
      toast("请先填写上游模型名", "error");
      return;
    }
    var targetName = state.editingTarget ? state.editingTarget.name : "";
    if (!targetName) {
      toast("请先保存目标后再测试模型", "error");
      return;
    }
    var originalText = btn.textContent;
    btn.disabled = true;
    btn.textContent = "测试中...";
    api(
      "/admin/data/targets/" + encodeURIComponent(targetName) + "/test-model",
      {
        method: "POST",
        body: { model: model },
      },
    )
      .then(function (res) {
        if (res && res.success) {
          toast(
            "HTTP 检测通过（HTTP " +
              res.status_code +
              ", " +
              res.latency_ms +
              "ms）",
            "success",
          );
        } else {
          var errMsg = res && res.error ? res.error : "未知错误";
          var status =
            res && res.statusCode ? " (HTTP " + res.statusCode + ")" : "";
          toast("测试失败: " + errMsg + status, "error");
        }
      })
      .catch(function (e) {
        toast("测试请求失败: " + e.message, "error");
      })
      .finally(function () {
        btn.disabled = false;
        btn.textContent = originalText;
      });
  }

  // collectModelMappings reads all non-empty rows from the editor.
  // Returns array suitable for the model_mappings API field.
  function collectModelMappings() {
    var mappings = [];
    var rows = document.querySelectorAll(
      "#model-mappings-container .model-mapping-row",
    );
    rows.forEach(function (row) {
      var upstream = row.querySelector(".upstream-input").value.trim();
      var fallback = row.querySelector(".fallback-input").value.trim();
      if (upstream) {
        var mapping = { upstream: upstream };
        if (fallback) {
          mapping.fallback = fallback;
        }
        if (row.dataset.paused === "1") {
          mapping.paused = true;
        }
        mappings.push(mapping);
      }
    });
    return mappings;
  }

  function updateModelPrefixPreview() {
    var prefix = document.getElementById("tgtModelPrefix").value;
    var first = document.getElementById("model-mappings-container").querySelector(".upstream-input");
    var model = first ? first.value.trim() : "";
    document.getElementById("tgtModelPrefixPreview").textContent = model
      ? "上游模型名预览：" + prefix + model
      : "填写一行模型后，可预览发给上游的完整模型名。";
  }
  document.getElementById("tgtModelPrefix").addEventListener("input", updateModelPrefixPreview);
  document.getElementById("model-mappings-container").addEventListener("input", updateModelPrefixPreview);

  // ===== Custom Headers KV editor =====
  function addCustomHeaderRow(name, value) {
    var container = document.getElementById("custom-headers-container");
    var row = document.createElement("div");
    row.style.cssText =
      "display:flex;gap:8px;margin-bottom:4px;align-items:center;";
    row.innerHTML =
      '<input class="form-control" placeholder="Header Name" style="flex:1;font-size:13px;" value="' +
      (name || "").replace(/"/g, "&quot;") +
      '">' +
      '<input class="form-control" placeholder="Value" style="flex:2;font-size:13px;" value="' +
      (value || "").replace(/"/g, "&quot;") +
      '">' +
      '<button type="button" class="btn btn-sm btn-danger" style="padding:2px 8px;">&times;</button>';
    row.querySelector("button").addEventListener("click", function () {
      row.remove();
    });
    container.appendChild(row);
  }
  document
    .getElementById("add-custom-header")
    .addEventListener("click", function () {
      addCustomHeaderRow("", "");
    });
  function collectCustomHeaders() {
    var headers = {};
    document
      .querySelectorAll("#custom-headers-container > div")
      .forEach(function (row) {
        var inputs = row.querySelectorAll("input");
        var name = inputs[0].value.trim();
        var value = inputs[1].value;
        if (name) headers[name] = value;
      });
    // 始终返回对象（含空 {}）：update 时让 handler 真正清空字段，create 时空 map 由 omitempty 省略
    return headers;
  }

  // ===== Custom Body KV editor =====
  function addCustomBodyRow(name, value) {
    var container = document.getElementById("custom-body-container");
    var row = document.createElement("div");
    row.style.cssText =
      "display:flex;gap:8px;margin-bottom:4px;align-items:center;";
    row.innerHTML =
      '<input class="form-control" placeholder="Field Name" style="flex:1;font-size:13px;" value="' +
      (name || "").replace(/"/g, "&quot;") +
      '">' +
      '<input class="form-control" placeholder="Value (JSON)" style="flex:2;font-size:13px;font-family:ui-monospace, SF Mono, Menlo, Consolas, monospace;" value="' +
      (value || "").replace(/"/g, "&quot;") +
      '">' +
      '<button type="button" class="btn btn-sm btn-danger" style="padding:2px 8px;">&times;</button>';
    row.querySelector("button").addEventListener("click", function () {
      row.remove();
    });
    container.appendChild(row);
  }
  document
    .getElementById("add-custom-body")
    .addEventListener("click", function () {
      addCustomBodyRow("", "");
    });
  function collectCustomBody() {
    var body = {};
    document
      .querySelectorAll("#custom-body-container > div")
      .forEach(function (row) {
        var inputs = row.querySelectorAll("input");
        var name = inputs[0].value.trim();
        var rawValue = inputs[1].value;
        if (!name) return;
        // Try to parse as JSON; if fails, use as string
        try {
          body[name] = JSON.parse(rawValue);
        } catch (e) {
          body[name] = rawValue;
        }
      });
    // 始终返回对象（含空 {}）：update 时让 handler 真正清空字段，create 时空 map 由 omitempty 省略
    return body;
  }

  async function openTargetModal(t) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    // 保证模型目录缓存可用，供模型行的 datalist 软建议；失败不阻塞弹窗
    if (!state.catalogAllCache) {
      try {
        var catData = await api("/admin/data/catalog");
        state.catalogAllCache = Array.isArray(catData.models)
          ? catData.models
          : [];
      } catch (e) {
        /* 软建议数据缺失可容忍 */
      }
    }
    state.editingTarget = t || null;
    document.getElementById("targetModalTitle").textContent = t
      ? "编辑目标"
      : "新增目标";
    document.getElementById("tgtName").value = t ? t.name : "";
    document.getElementById("tgtName").readOnly = !!t;
    setSchemaSelectValue(
      "tgtType",
      t
        ? t.endpoint_type || state.schema.defaults.endpoint_type
        : state.schema.defaults.endpoint_type,
    );
    document.getElementById("tgtEndpoint").value = t ? t.endpoint : "";
    var epType = t
      ? t.endpoint_type || state.schema.defaults.endpoint_type
      : state.schema.defaults.endpoint_type;
    document.getElementById("tgtAllowBearer").checked = t
      ? !!t.allow_bearer_passthrough
      : false;
    document.getElementById("tgtPaused").checked = t ? !!t.paused : false;
    document.getElementById("tgtPreferWS").checked = t ? !!t.prefer_websocket : false;
    document.getElementById("tgtOpenAIPrefix").value =
      t && t.openai_prefix != null ? t.openai_prefix : state.schema.defaults.openai_prefix;
    document.getElementById("tgtAnthropicPrefix").value =
      t && t.anthropic_prefix != null ? t.anthropic_prefix : state.schema.defaults.anthropic_prefix;
    document.getElementById("tgtSupportsResponses").checked =
      t && t.supports_responses != null ? t.supports_responses : state.schema.defaults.supports_responses;
    document.getElementById("tgtRequestStreamUsage").checked =
      t && t.request_stream_usage_if_missing != null ? t.request_stream_usage_if_missing : state.schema.defaults.request_stream_usage_if_missing;
    setSchemaSelectValue(
      "tgtAuthMode",
      t
        ? t.auth_mode || state.schema.defaults.auth_mode
        : state.schema.defaults.auth_mode,
    );
    setSchemaSelectValue(
      "tgtUsageAdapter",
      t ? t.usage_adapter : state.schema.defaults.usage_adapter,
    );
    setUsageAdapterError("");
    setSchemaSelectValue(
      "tgtImageOperation",
      t
        ? t.image_operation || state.schema.defaults.image_operation
        : state.schema.defaults.image_operation,
    );
    document.getElementById("tgtModelPrefix").value = t ? t.model_prefix || "" : "";
    // 模型映射：清空容器后按 model_mappings 填充行，无数据时给一行空行
    var mappingsContainer = document.getElementById("model-mappings-container");
    mappingsContainer.innerHTML = "";
    if (t && Array.isArray(t.model_mappings) && t.model_mappings.length > 0) {
      t.model_mappings.forEach(function (m) {
        addModelMappingRow(m.upstream || "", m.fallback || "", !!t, !!m.paused);
      });
    } else {
      addModelMappingRow("", "", !!t);
    }
    updateModelPrefixPreview();
    // Custom Headers：填充 KV 行
    var headersContainer = document.getElementById("custom-headers-container");
    headersContainer.innerHTML = "";
    if (t && t.custom_headers) {
      Object.keys(t.custom_headers).forEach(function (k) {
        addCustomHeaderRow(k, t.custom_headers[k]);
      });
    }
    if (headersContainer.children.length === 0) addCustomHeaderRow("", "");

    // Custom Body：填充 KV 行
    var bodyContainer = document.getElementById("custom-body-container");
    bodyContainer.innerHTML = "";
    if (t && t.custom_body) {
      Object.keys(t.custom_body).forEach(function (k) {
        addCustomBodyRow(k, JSON.stringify(t.custom_body[k]));
      });
    }
    if (bodyContainer.children.length === 0) addCustomBodyRow("", "");
    // API Keys：构建卡片列表（合并 api_key + api_keys + key_statuses）
    tgtKeyList = [];
    var keyStatuses = t && Array.isArray(t.key_statuses) ? t.key_statuses : [];
    if (t && t.api_key) {
      var ks0 =
        keyStatuses.find(function (s) {
          return s.index === 0;
        }) || null;
      tgtKeyList.push({
        value: t.api_key,
        ref: "__existing_key_index__:0",
        status: ks0,
      });
    }
    if (t && Array.isArray(t.api_keys)) {
      t.api_keys.forEach(function (k, i) {
        if (!k) return;
        // dedup against api_key
        if (t.api_key && k === t.api_key) return;
        var ksIdx =
          keyStatuses.find(function (s) {
            return s.index === i + 1;
          }) || null;
        tgtKeyList.push({
          value: k,
          ref: "__existing_key_index__:" + (i + 1),
          status: ksIdx,
        });
      });
    }
    renderKeyList();
    document.getElementById("tgtNewKeyInput").value = "";
    document.getElementById("tgtKeyResetTime").value =
      t && t.key_reset_time ? t.key_reset_time : "";
    renderTargetPoolOptions(t && t.pool ? t.pool : "");
    updateTargetFormVisibility();
    openModal("targetModal");
  }
  // renderTargetPoolOptions 重建目标表单的池下拉（数据来自 /admin/data/pools）。
  function renderTargetPoolOptions(selected) {
    var sel = document.getElementById("tgtPool");
    var val = selected || "";
    sel.innerHTML = "<option value=''>不属于任何池</option>";
    (state.pools || [])
      .slice()
      .sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      })
      .forEach(function (p) {
        var opt = document.createElement("option");
        opt.value = p.name;
        opt.textContent =
          poolDisplayName(p) +
          (poolMemberCount(p.name) > 0
            ? "（" + poolMemberCount(p.name) + " 成员）"
            : "");
        sel.appendChild(opt);
      });
    sel.value = val;
    if (sel.value !== val) sel.value = ""; // 选中池不存在（已删除）时回退为未分组
  }
  document
    .getElementById("tgtType")
    .addEventListener("change", updateTargetFormVisibility);
  function setUsageAdapterError(message) {
    var error = document.getElementById("tgtUsageAdapterError");
    error.textContent = message;
    error.hidden = !message;
    document.getElementById("tgtUsageAdapter").setAttribute(
      "aria-invalid", message ? "true" : "false",
    );
  }
  document.getElementById("tgtUsageAdapter").addEventListener("change", function () {
    setUsageAdapterError("");
  });
  var tgtModelDebounce = null;
  // Delegated input listener: filter suggestions based on any upstream-input being typed
  document
    .getElementById("model-mappings-container")
    .addEventListener("input", function (e) {
      if (
        e.target &&
        e.target.classList &&
        e.target.classList.contains("upstream-input")
      ) {
        clearTimeout(tgtModelDebounce);
        var val = e.target.value;
        tgtModelDebounce = setTimeout(function () {
          var epType = document.getElementById("tgtType").value;
          renderTgtModelSuggestions(epType, val);
        }, 250);
      }
    });
  document
    .getElementById("add-model-mapping")
    .addEventListener("click", function () {
      addModelMappingRow("", "", !!state.editingTarget);
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
    .getElementById("saveTargetBtn")
    .addEventListener("click", async function () {
      var name = document.getElementById("tgtName").value.trim();
      var epType = document.getElementById("tgtType").value;
      var endpoint = document.getElementById("tgtEndpoint").value.trim();
      var usageAdapter = document.getElementById("tgtUsageAdapter").value;
      var allowBearer = document.getElementById("tgtAllowBearer").checked;
      var paused = document.getElementById("tgtPaused").checked;
      var authMode = document.getElementById("tgtAuthMode").value;
      var imageOperation = document.getElementById("tgtImageOperation").value;
      // Only types that support it submit the preference; the server also
      // clears it for any other type.
      var preferWS =
        !!(epMeta(epType) || {}).supports_prefer_websocket &&
        document.getElementById("tgtPreferWS").checked;
      var modelMappings = collectModelMappings();
      var customHeaders = collectCustomHeaders();
      var customBody = collectCustomBody();
      // 统一 keypool：从卡片列表读取，第一个为 api_key，其余为 api_keys
      var allKeys = tgtKeyList
        .map(function (item) {
          return item.ref || item.value;
        })
        .filter(Boolean);
      var apiKey = allKeys.length > 0 ? allKeys[0] : "";
      var apiKeys = allKeys.length > 1 ? allKeys.slice(1) : [];
      var keyResetTime = document
        .getElementById("tgtKeyResetTime")
        .value.trim();
      var pool = document.getElementById("tgtPool").value;

      if (!name) {
        toast("请填写名称", "error");
        return;
      }
      if (!endpoint) {
        toast("请填写 Endpoint URL", "error");
        return;
      }
      if (
        !usageAdapter ||
        !state.schema.usage_adapters.some(function (option) {
          return option.value === usageAdapter;
        })
      ) {
        setUsageAdapterError("请选择计费类型后再保存。");
        document.getElementById("tgtUsageAdapter").focus();
        return;
      }
      setUsageAdapterError("");

      try {
        if (state.editingTarget) {
          var updateBody = {
            endpoint_type: epType,
            endpoint: endpoint,
            paused: paused,
            allow_bearer_passthrough: allowBearer,
            auth_mode: authMode,
            usage_adapter: usageAdapter,
            image_operation: imageOperation,
            prefer_websocket: preferWS,
            openai_prefix: document.getElementById("tgtOpenAIPrefix").value,
            anthropic_prefix: document.getElementById("tgtAnthropicPrefix").value,
            supports_responses: document.getElementById("tgtSupportsResponses").checked,
            request_stream_usage_if_missing: document.getElementById("tgtRequestStreamUsage").checked,
            model_mappings: modelMappings,
            model_prefix: document.getElementById("tgtModelPrefix").value,
            custom_headers: customHeaders,
            custom_body: customBody,
            api_key: apiKey,
            api_keys: apiKeys,
            key_reset_time: keyResetTime,
            pool: pool,
          };
          await api("/admin/data/targets/" + encodeURIComponent(name), {
            method: "PUT",
            body: updateBody,
          });
          toast("目标已更新", "success");
        } else {
          await api("/admin/data/targets", {
            method: "POST",
            body: {
              name: name,
              endpoint_type: epType,
              endpoint: endpoint,
              api_key: apiKey,
              paused: paused,
              allow_bearer_passthrough: allowBearer,
              auth_mode: authMode,
              usage_adapter: usageAdapter,
              image_operation: imageOperation,
              prefer_websocket: preferWS,
              openai_prefix: document.getElementById("tgtOpenAIPrefix").value,
              anthropic_prefix: document.getElementById("tgtAnthropicPrefix").value,
              supports_responses: document.getElementById("tgtSupportsResponses").checked,
              request_stream_usage_if_missing: document.getElementById("tgtRequestStreamUsage").checked,
              model_mappings: modelMappings,
            model_prefix: document.getElementById("tgtModelPrefix").value,
              custom_headers: customHeaders,
              custom_body: customBody,
              api_keys: apiKeys,
              key_reset_time: keyResetTime,
              pool: pool,
            },
          });
          toast("目标已创建", "success");
        }
        closeModal("targetModal");
        await loadTargets();
      } catch (e) {
        toast("保存失败: " + e.message, "error");
      }
    });

  return {
    loadTargets: loadTargets,
    fetchSchedulingRuntime: fetchSchedulingRuntime,
    refreshEditingTarget: refreshEditingTarget,
    keyStatusColor: keyStatusColor,
    renderKeyList: renderKeyList,
    addKeyToList: addKeyToList,
    removeKeyFromList: removeKeyFromList,
    renderTargets: renderTargets,
    hideModelHint: hideModelHint,
    showModelHint: showModelHint,
    updateTargetFormVisibility: updateTargetFormVisibility,
    applyEndpointPlaceholder: applyEndpointPlaceholder,
    renderTgtModelSuggestions: renderTgtModelSuggestions,
    addModelMappingRow: addModelMappingRow,
    testModelForRow: testModelForRow,
    collectModelMappings: collectModelMappings,
    addCustomHeaderRow: addCustomHeaderRow,
    collectCustomHeaders: collectCustomHeaders,
    addCustomBodyRow: addCustomBodyRow,
    collectCustomBody: collectCustomBody,
    openTargetModal: openTargetModal,
    renderTargetPoolOptions: renderTargetPoolOptions,
  };
});
