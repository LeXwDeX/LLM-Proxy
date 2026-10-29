"use strict";
// Targets owns target CRUD, health summaries, the read-only detail drawer,
// key cards, model mappings and provider capability controls.
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
  var loadPools = app.bind("pools", "loadPools");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var loadCatalog = app.bind("catalog", "loadCatalog");
  var catalogFilterModels = app.bind("catalog", "catalogFilterModels");
  var targetsLoadSequence = 0;
  var targetsLoaded = false;
  var targetsLoadFailed = false;
  var targetPoolsSnapshot = null;
  var targetPoolOptionsSnapshot = null;
  function targetPoolsAvailable() { return targetPoolsSnapshot !== null; }
  var schedulingRefresh = null;
  function beginSchedulingRefresh() {
    if (schedulingRefresh) schedulingRefresh.finish({ superseded: true });
    var refresh = {};
    refresh.promise = new Promise(function (resolve) { refresh.finish = resolve; });
    schedulingRefresh = refresh;
    return ++targetsLoadSequence;
  }
  function schedulingRefreshCurrent(sequence) { return sequence === targetsLoadSequence; }
  async function awaitSchedulingRefresh() {
    while (true) {
      var refresh = schedulingRefresh;
      var result = await refresh.promise;
      if (refresh !== schedulingRefresh || result.superseded) continue;
      if (result.error) throw result.error;
      if (result.runtimeOnly) return loadTargets(true);
      return state.targets;
    }
  }
  function validTargets(result) {
    return result && Array.isArray(result.targets) && result.targets.every(function (t) {
      return t && typeof t === "object" && typeof t.name === "string" && !!t.name &&
        (t.model_mappings == null || (Array.isArray(t.model_mappings) &&
          t.model_mappings.every(function (m) { return m && typeof m === "object"; })));
    });
  }
  function validPoolRoster(result) {
    return !!result && Array.isArray(result.pools) && result.pools.every(function (pool) {
      return pool && typeof pool === "object" && !Array.isArray(pool) &&
        typeof pool.name === "string" && !!pool.name.trim() &&
        (pool.paused === undefined || typeof pool.paused === "boolean") &&
        (pool.display_name === undefined || typeof pool.display_name === "string") &&
        (pool.description === undefined || typeof pool.description === "string");
    });
  }
  function publishSchedulingRefresh(sequence, targets, runtime, pools) {
    if (!schedulingRefreshCurrent(sequence)) return null;
    if (!validTargets(targets)) throw new Error("目标列表格式无效");
    state.targets = targets.targets;
    state.runtime = runtime;
    targetPoolsSnapshot = validPoolRoster(pools) ? pools.pools : null;
    if (targetPoolsSnapshot) state.pools = targetPoolsSnapshot;
    updateTargetPoolAvailability();
    if (document.getElementById("targetModal").classList.contains("open")) renderKeyList();
    targetsLoaded = true;
    targetsLoadFailed = false;
    if (state.editingTarget && !editingKeyBusy) {
      var editing = state.targets.find(function (t) { return t.name === state.editingTarget.name; });
      if (!editing || editing.key_snapshot !== editingKeySnapshot || !sameKeyIndices(editing)) {
        editingKeyNeedsReview = true;
        renderKeyList();
      } else if (!editingKeyNeedsReview) {
        updateEditingKeyStatuses(editing);
        renderKeyList();
      }
    }
    renderTargets();
    if (app.modules.clients) app.modules.clients.onSchedulingRefresh();
    if (app.modules.pools) app.modules.pools.onSchedulingRefresh();
    schedulingRefresh.finish({});
    return state.targets;
  }
  function failSchedulingRefresh(sequence, error) {
    if (!schedulingRefreshCurrent(sequence)) return false;
    targetsLoadFailed = true;
    targetPoolsSnapshot = null;
    updateTargetPoolAvailability();
    if (document.getElementById("targetModal").classList.contains("open")) renderKeyList();
    renderTargets();
    if (app.modules.clients) app.modules.clients.onSchedulingRefresh();
    if (app.modules.pools) app.modules.pools.onSchedulingRefresh();
    schedulingRefresh.finish({ error: error || new Error("目标列表加载失败") });
    return true;
  }
  async function loadTargets(propagateError) {
    var sequence = beginSchedulingRefresh();
    try {
      // Pool pause lives on the roster, and another admin session may resume it
      // between refreshes. Reload it every time; a failed fetch keeps the last
      // known roster instead of turning cached pause into an authoritative fact.
      var fetches = [
        api("/admin/data/targets"),
        fetchSchedulingRuntime(true),
        api("/admin/data/pools").catch(function () {
          return null;
        }),
      ];
      var results = await Promise.all(fetches);
      var targets = publishSchedulingRefresh(sequence, results[0], results[1], results[2]);
      if (targets === null && propagateError === true) return awaitSchedulingRefresh();
      return targets;
    } catch (e) {
      if (!failSchedulingRefresh(sequence, e)) {
        if (propagateError === true) return awaitSchedulingRefresh();
        return null;
      }
      if (propagateError === true) throw e;
      toast("加载目标失败: " + e.message, "error");
      return null;
    }
  }
  // fetchSchedulingRuntime：拉取 /admin/metrics 的 targets_runtime 调度观测快照
  // （S5）。失败时清空快照，避免把过期运行态当成当前健康状态。
  async function fetchSchedulingRuntime(captureOnly) {
    var sequence = captureOnly ? null : beginSchedulingRefresh();
    var map = null;
    try {
      var m = await api("/admin/metrics");
      map = {};
      if (Array.isArray(m.targets_runtime)) {
        m.targets_runtime.forEach(function (rt) {
          map[String(rt.name || "").toLowerCase()] = rt;
        });
      }
    } catch (e) {
      // Stale snapshots would misreport health; metrics stay best-effort.
      map = null;
    }
    if (!captureOnly && schedulingRefreshCurrent(sequence)) {
      state.runtime = map;
      schedulingRefresh.finish({ runtimeOnly: true });
    }
    return map;
  }
  /* ===== Target health summaries and detail drawer ===== */
  var targetFilter = "all";
  var detailTargetName = "";
  var detailTab = "overview";
  var targetFilterSpecs = [
    { key: "all", label: "全部" },
    { key: "attention", label: "异常" },
    { key: "healthy", label: "正常" },
    { key: "paused", label: "已暂停" },
  ];
  var targetHealthRanks = { attention: 0, healthy: 1, paused: 2 };
  // keypool.status keeps exhausted=true after an expired cooldown; only the
  // 可用(待激活) display label means the key can be retried without an issue.
  // 失效(待请求验证), blocked keys and active cooldowns still need attention.
  function keyStatusNeedsAttention(status) {
    return (
      !!status &&
      (!!status.blocked ||
        (!!status.exhausted && status.status !== "可用(待激活)"))
    );
  }
  function keyStatusRecoverablePending(status) {
    return (
      !!status &&
      !!status.exhausted &&
      !status.blocked &&
      status.status === "可用(待激活)"
    );
  }
  function targetKeyCounts(t) {
    var statuses = t && Array.isArray(t.key_statuses)
      ? t.key_statuses.filter(function (s) { return !!s && typeof s === "object"; })
      : [];
    var extraKeys = t && Array.isArray(t.api_keys) ? t.api_keys.length : 0;
    return {
      total: (t && t.has_api_key ? 1 : 0) + extraKeys,
      blocked: statuses.filter(function (s) {
        return !!s.blocked;
      }).length,
      exhausted: statuses.filter(function (s) {
        return keyStatusNeedsAttention(s) && !s.blocked;
      }).length,
      recoverable: statuses.filter(keyStatusRecoverablePending).length,
    };
  }
  function targetKeyStatusComplete(t) {
    var counts = targetKeyCounts(t);
    if (counts.total <= 1) return true;
    if (usableKeySnapshot(t) && t.key_runtime_indices.every(function (i) { return i === null; })) return true;
    if (hasUnmappedNonemptyKey(t)) return false;
    var statuses = Array.isArray(t.key_statuses) ? t.key_statuses : [];
    var indices = usableKeySnapshot(t) ? t.key_runtime_indices : null;
    var expected = indices
      ? indices.filter(function (i) { return Number.isInteger(i) && i >= 0; })
      : [];
    expected = expected.filter(function (i, n) { return expected.indexOf(i) === n; });
    if (!expected.length) {
      expected = Array.from({ length: counts.total }, function (_, i) { return i; });
    }
    return expected.every(function (index) {
      return statuses.some(function (s) {
        return s && s.index === index && typeof s.blocked === "boolean" &&
          typeof s.exhausted === "boolean" && typeof s.status === "string" && !!s.status;
      });
    });
  }
  function targetRuntimeFor(t) {
    return state.runtime
      ? state.runtime[String((t && t.name) || "").toLowerCase()] || null
      : null;
  }
  // Known phases come from the schema; a missing or malformed snapshot stays
  // attention instead of being treated as healthy.
  function targetPhaseKnown(phase) {
    if (typeof phase !== "string" || !phase) return false;
    var phases = (state.schema && state.schema.target_phases) || [];
    return phases.some(function (option) {
      return option.value === phase;
    });
  }
  // Pool pause is applied to runtime targets (proxy.applyPoolPause); the saved
  // target keeps its own paused flag. Read the roster only, never write it.
  // This roster can lag behind /admin/metrics, so health uses it only when the
  // runtime snapshot is missing or also reports a pause.
  function targetPoolPaused(t) {
    var poolName = String((t && t.pool) || "").toLowerCase();
    if (!poolName || !targetPoolsSnapshot) return false;
    return targetPoolsSnapshot.some(function (p) {
      return (
        String((p && p.name) || "").toLowerCase() === poolName && !!p.paused
      );
    });
  }
  function targetHealth(t) {
    var keys = targetKeyCounts(t);
    var rt = targetRuntimeFor(t);
    var phase = rt && rt.phase ? String(rt.phase) : "";
    if (targetsLoadFailed) {
      return {
        key: "attention", label: "需要关注", tone: "warn",
        reason: "目标或部署池数据更新失败；显示上次数据",
      };
    }
    if (t && t.paused) {
      return {
        key: "paused",
        label: "已暂停",
        tone: "muted",
        reason: "目标已暂停，不参与转发",
      };
    }
    // A fresh runtime snapshot is authoritative. Only fall back to the roster
    // when runtime is absent, or use its reason when runtime also shows pause.
    if (!rt) {
      if (targetPoolPaused(t)) {
        return {
          key: "paused",
          label: "已暂停",
          tone: "muted",
          reason:
            "所属部署池已暂停，目标作为池成员停止转发；目标自身暂停设置未修改",
        };
      }
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "运行态不可用：/admin/metrics 未返回该目标",
      };
    }
    if (rt.paused || phase === "offline") {
      if (targetPoolPaused(t)) {
        return {
          key: "paused",
          label: "已暂停",
          tone: "muted",
          reason:
            "所属部署池已暂停，目标作为池成员停止转发；目标自身暂停设置未修改",
        };
      }
      return {
        key: "paused",
        label: "已暂停",
        tone: "muted",
        reason: rt.paused
          ? "运行态显示目标已暂停，不参与转发；目标自身暂停设置未修改"
          : "运行态相位为暂停（offline），不参与转发；目标自身暂停设置未修改",
      };
    }
    if (!targetPhaseKnown(phase)) {
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "运行态不可用：调度相位缺失或无法识别",
      };
    }
    if (keys.blocked || keys.exhausted) {
      var issues = [];
      if (keys.blocked) issues.push(keys.blocked + " 个 Key 已屏蔽");
      if (keys.exhausted) issues.push(keys.exhausted + " 个 Key 额度或限流不可用");
      return {
        key: "attention",
        label: "需要关注",
        tone: "bad",
        reason: issues.join("，"),
      };
    }
    if (!targetKeyStatusComplete(t)) {
      return {
        key: "attention", label: "需要关注", tone: "warn",
        reason: "多密钥运行状态明细不完整",
      };
    }
    if (phase && phase !== "ready") {
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "运行状态：" + (schemaLabel("target_phases", phase) || phase),
      };
    }
    var models = t && Array.isArray(t.model_mappings) ? t.model_mappings : [];
    if (models.length && models.every(function (m) { return !!m.paused; })) {
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "全部模型已暂停",
      };
    }
    return {
      key: "healthy",
      label: "正常",
      tone: "ok",
      reason: "目标已启用，调度运行正常",
    };
  }
  function targetHealthCounts() {
    var targets = Array.isArray(state.targets) ? state.targets : [];
    var counts = { all: targets.length, attention: 0, healthy: 0, paused: 0 };
    targets.forEach(function (t) {
      var key = targetHealth(t).key;
      if (counts[key] !== undefined) counts[key]++;
    });
    return counts;
  }
  function targetSort(a, b) {
    var ar = targetHealthRanks[targetHealth(a).key];
    var br = targetHealthRanks[targetHealth(b).key];
    if (ar === undefined) ar = 3;
    if (br === undefined) br = 3;
    return ar - br || String(a.name || "").localeCompare(String(b.name || ""));
  }
  function filteredTargets() {
    var targets = Array.isArray(state.targets) ? state.targets : [];
    return targets
      .slice()
      .sort(targetSort)
      .filter(function (t) {
        return targetFilter === "all" || targetHealth(t).key === targetFilter;
      });
  }
  function renderTargetStatusFilters() {
    if (targetsLoadFailed && !targetsLoaded) {
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
          (targetFilter === spec.key) +
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
    targetFilter =
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
  function detailRow(label, value) {
    return (
      "<div class='target-detail-row'><dt>" +
      esc(label) +
      "</dt><dd>" +
      value +
      "</dd></div>"
    );
  }
  function detailGrid(rows) {
    return "<dl class='target-detail-grid'>" + rows.join("") + "</dl>";
  }
  function targetKeyDetailSummary(t) {
    var counts = targetKeyCounts(t);
    if (!counts.total) {
      return t.allow_bearer_passthrough
        ? "未配置独立密钥；已允许 Bearer 透传，客户端凭据可能直接发往上游"
        : "未配置独立密钥";
    }
    if (!Array.isArray(t.key_statuses) || !t.key_statuses.length) {
      return counts.total + " 个密钥；运行态明细尚未上报";
    }
    var parts = [counts.total + " 个密钥"];
    if (counts.blocked) parts.push("屏蔽 " + counts.blocked);
    if (counts.exhausted) parts.push("不可用 " + counts.exhausted);
    if (counts.recoverable) parts.push("可用(待激活) " + counts.recoverable);
    if (!targetKeyStatusComplete(t)) {
      parts.push("运行态明细不完整");
    } else if (!counts.blocked && !counts.exhausted) {
      parts.push(counts.recoverable ? "无阻塞异常" : "未报告异常");
    }
    return parts.join("；");
  }
  function renderTargetOverview(t) {
    var health = targetHealth(t);
    var rt = targetRuntimeFor(t);
    var models = Array.isArray(t.model_mappings) ? t.model_mappings : [];
    var pausedModels = models.filter(function (m) {
      return !!m.paused;
    }).length;
    var runtimeText;
    if (!rt) {
      runtimeText = "运行态不可用（/admin/metrics 未返回该目标）";
    } else if (!targetPhaseKnown(rt.phase)) {
      runtimeText = "运行态不可用（调度相位缺失或无法识别）";
    } else {
      runtimeText =
        schemaLabel("target_phases", rt.phase) +
        " · 在途 " +
        Number(rt.in_flight || 0) +
        (rt.max_concurrency > 0 ? "/" + rt.max_concurrency : "") +
        " · 亲和 " +
        Number(rt.affinity_hits || 0) +
        " · 熔断 " +
        Number(rt.breaker_trips || 0);
      if (rt.window_15m) {
        runtimeText += " · 15 分钟峰值 " + Number(rt.window_15m.in_flight_peak || 0);
      }
    }
    return (
      detailGrid([
        detailRow(
          "健康状态",
          "<span class='target-health-badge tone-" +
            esc(health.tone) +
            "'>" +
            esc(health.label) +
            "</span> " +
            esc(health.reason),
        ),
        detailRow("类型", esc(epDisplayName(t.endpoint_type) || t.endpoint_type || "—")),
        detailRow(
          "Endpoint",
          t.endpoint
            ? "<span class='target-detail-mono'>" + esc(t.endpoint) + "</span>" +
              (isReadonlyMode() ? "（仅显示协议和主机）" : "")
            : isReadonlyMode() ? "不可查看" : "未配置",
        ),
        detailRow(
          "部署池",
          t.pool ? esc(poolDisplayName(t.pool)) : "未加入部署池",
        ),
        detailRow("调度观测", esc(runtimeText)),
        detailRow("密钥", esc(targetKeyDetailSummary(t))),
        detailRow(
          "模型",
          models.length
            ? esc(
                models.length +
                  " 个映射" +
                  (pausedModels ? "，" + pausedModels + " 个已暂停" : ""),
              )
            : "未限定模型",
        ),
      ]) +
      "<p class='target-detail-note'>详情只读取 /admin/data/targets 与 /admin/metrics 的现有字段，不展示明文密钥或逐 Key 请求数。</p>"
    );
  }
  // Go serializes an unset time.Time as 0001-01-01T00:00:00Z. A cooldown that
  // never existed must not read as a real deadline.
  function keyCooldownEnd(status) {
    var value = status && status.cooldown_end;
    if (!value) return "";
    var date = new Date(value);
    if (isNaN(date.getTime()) || date.getUTCFullYear() <= 1) return "";
    return String(value);
  }
  function renderTargetKeys(t) {
    var statuses = Array.isArray(t.key_statuses)
      ? t.key_statuses.filter(function (s) { return !!s && typeof s === "object"; })
      : [];
    var counts = targetKeyCounts(t);
    if (!statuses.length) {
      return (
        "<div class='target-detail-empty'>" +
        (counts.total
          ? esc(counts.total + " 个密钥已保存，运行状态明细尚未上报。")
          : "未配置独立密钥。") +
        "</div><p class='target-detail-note'>明文密钥不会回读。密钥状态由运行中的 Key 池上报；未上报时不推断每个 Key 的请求数。</p>" +
        (t.allow_bearer_passthrough
          ? "<p class='target-detail-note'>已允许 Bearer 透传：未配置独立密钥时，客户端凭据可能直接发往上游。</p>"
          : "")
      );
    }
    return (
      "<div class='target-detail-keys'>" +
      statuses
        .map(function (status) {
          var meta = [];
          if (status.blocked) meta.push("已屏蔽");
          if (keyStatusNeedsAttention(status) && !status.blocked)
            meta.push("额度或限流不可用");
          var cooldownEnd = keyCooldownEnd(status);
          if (cooldownEnd) meta.push("冷却结束 " + cooldownEnd);
          return (
            "<div class='target-detail-key'><span class='target-detail-key-mask'>" +
            esc(status.key_mask || "掩码不可用") +
            "</span>" +
            (status.status
              ? "<span style='" +
                keyStatusColor(status.status) +
                ";padding:2px 8px;border-radius:10px;font-size:11px;font-weight:600;white-space:nowrap;'>" +
                esc(status.status) +
                "</span>"
              : "") +
            (meta.length
              ? "<span class='target-detail-key-meta'>" + esc(meta.join(" · ")) + "</span>"
              : "") +
            "</div>"
          );
        })
        .join("") +
      "</div>" +
      (!targetKeyStatusComplete(t) ? "<p class='target-detail-note'>密钥运行态明细不完整。</p>" : "") +
      "<p class='target-detail-note'>仅显示密钥掩码和运行状态；不显示明文密钥，也不推导每个 Key 的请求数。</p>"
    );
  }
  function renderTargetModels(t) {
    var models = Array.isArray(t.model_mappings) ? t.model_mappings : [];
    if (!models.length) {
      return (
        "<div class='target-detail-empty'>未限定模型；上游接受配置中的任意模型。</div>" +
        "<p class='target-detail-note'>模型行只反映已保存的上游映射，不提供默认模型或测试模型选择。</p>"
      );
    }
    return (
      "<div class='target-detail-models'><div class='target-model-list'>" +
      models
        .map(function (m) {
          var label = m.fallback
            ? m.upstream + " (" + m.fallback + ")"
            : m.upstream;
          if (m.paused) label = "已暂停 · " + label;
          return (
            "<span class='target-model-badge' title='" +
            esc(label) +
            "'>" +
            esc(label) +
            "</span>"
          );
        })
        .join("") +
      "</div></div><p class='target-detail-note'>括号内为下游兜底别名；已暂停的模型不接收请求。</p>"
    );
  }
  function renderTargetAdvanced(t) {
    var headerNames =
      t.custom_headers && typeof t.custom_headers === "object"
        ? Object.keys(t.custom_headers)
        : [];
    var bodyNames =
      t.custom_body && typeof t.custom_body === "object"
        ? Object.keys(t.custom_body)
        : [];
    function onOff(value) {
      return value ? "开启" : "关闭";
    }
    function raw(value) {
      return value ? "<span class='target-detail-mono'>" + esc(value) + "</span>" : "未配置";
    }
    return (
      detailGrid([
        detailRow(
          "用量适配",
          t.usage_adapter
            ? esc(schemaLabel("usage_adapters", t.usage_adapter))
            : "未配置",
        ),
        detailRow(
          "认证方式",
          t.auth_mode ? esc(schemaLabel("auth_modes", t.auth_mode)) : "默认",
        ),
        detailRow("允许 Bearer 透传", onOff(t.allow_bearer_passthrough)),
        detailRow("Key 额度重置时间", raw(t.key_reset_time)),
        detailRow("模型前缀", raw(t.model_prefix)),
        detailRow(
          "图片操作",
          t.image_operation
            ? esc(schemaLabel("image_operations", t.image_operation) || t.image_operation)
            : "未配置",
        ),
        detailRow("请求流式用量", onOff(t.request_stream_usage_if_missing)),
        detailRow("Responses 优先 WebSocket", onOff(t.prefer_websocket)),
        detailRow("上游支持 Responses", onOff(t.supports_responses)),
        detailRow("OpenAI 路径前缀", raw(t.openai_prefix)),
        detailRow("Anthropic 路径前缀", raw(t.anthropic_prefix)),
        detailRow(
          "自定义请求头",
          isReadonlyMode() ? "不可查看" : headerNames.length
            ? esc(headerNames.join("、")) + "<span class='target-detail-key-meta'>（仅字段名）</span>"
            : "未配置",
        ),
        detailRow(
          "自定义请求体",
          isReadonlyMode() ? "不可查看" : bodyNames.length
            ? esc(bodyNames.join("、")) + "<span class='target-detail-key-meta'>（仅字段名）</span>"
            : "未配置",
        ),
      ]) +
      (isReadonlyMode()
        ? "<p class='target-detail-note'>只读用户不可查看自定义请求头与请求体。</p>"
        : "<p class='target-detail-note'>高级设置是当前保存值的只读视图；修改请使用「编辑目标」。自定义请求头与请求体只显示字段名，不显示值。</p>")
    );
  }
  function setTargetDetailTab(tab) {
    detailTab = tab;
    document
      .querySelectorAll("[data-target-detail-tab]")
      .forEach(function (button) {
        var active = button.dataset.targetDetailTab === tab;
        button.classList.toggle("active", active);
        button.setAttribute("aria-selected", active ? "true" : "false");
        button.tabIndex = active ? 0 : -1;
      });
    [
      ["targetDetailOverview", "overview"],
      ["targetDetailKeys", "keys"],
      ["targetDetailModels", "models"],
      ["targetDetailAdvanced", "advanced"],
    ].forEach(function (entry) {
      var panel = document.getElementById(entry[0]);
      panel.hidden = entry[1] !== tab;
    });
  }
  function switchTargetDetailTab(tab) {
    var valid = ["overview", "keys", "models", "advanced"];
    if (valid.indexOf(tab) < 0) return;
    setTargetDetailTab(tab);
  }
  function renderTargetDetail(t) {
    var health = targetHealth(t);
    document.getElementById("targetDetailTitle").textContent = t.name;
    document.getElementById("targetDetailSubtitle").textContent =
      health.label +
      " · " +
      (epDisplayName(t.endpoint_type) || t.endpoint_type || "类型未知");
    document.getElementById("targetDetailOverview").innerHTML = renderTargetOverview(t);
    document.getElementById("targetDetailKeys").innerHTML = renderTargetKeys(t);
    document.getElementById("targetDetailModels").innerHTML = renderTargetModels(t);
    document.getElementById("targetDetailAdvanced").innerHTML = renderTargetAdvanced(t);
    setTargetDetailTab(detailTab);
  }
  function openTargetDetail(name) {
    var t = (state.targets || []).find(function (target) {
      return target.name === name;
    });
    if (!t) return false;
    detailTargetName = name;
    detailTab = "overview";
    renderTargetDetail(t);
    openModal("targetDetailDrawer");
    return true;
  }
  function closeTargetDetail() {
    closeModal("targetDetailDrawer");
  }
  function refreshOpenTargetDetail() {
    if (!detailTargetName) return;
    var drawer = document.getElementById("targetDetailDrawer");
    if (!drawer || !drawer.classList.contains("open")) return;
    var t = (state.targets || []).find(function (target) {
      return target.name === detailTargetName;
    });
    if (!t) {
      detailTargetName = "";
      closeTargetDetail();
      return;
    }
    renderTargetDetail(t);
  }
  /* ===== Key Card List (target modal) ===== */
  var tgtKeyList = [];
  var editingKeySnapshot = null;
  var editingKeyIndices = null;
  var editingKeyNeedsReview = false;
  var editingKeyStatusStale = false;
  var editingKeyBusy = false;
  var editingKeyReloading = false;
  var editingKeySaving = false;
  var editingKeySession = 0;
  var editingDraftRevision = 0;
  var createdDraftNeedsReopen = false;
  ["input", "change"].forEach(function (eventName) {
    document.getElementById("targetModal").addEventListener(eventName, function () { editingDraftRevision++; });
  });
  document.getElementById("targetModal").addEventListener("click", function (event) {
    var button = event.target.closest("button");
    if (editingKeySaving && button && button.id !== "saveTargetBtn") editingDraftRevision++;
  });

  function hasUnmappedNonemptyKey(t) {
    if (!t || !Array.isArray(t.api_keys) || !Array.isArray(t.key_runtime_indices)) return false;
    return t.key_runtime_indices.some(function (index, slot) {
      var mask = slot === 0 ? t.api_key : t.api_keys[slot - 1];
      // Empty slots and short keys both mask as ****; whitespace-only keys are absent at runtime.
      return index === null && typeof mask === "string" && mask !== "****" && !!mask.replace("...", "").trim();
    });
  }
  function usableKeySnapshot(t) {
    if (
      !t || typeof t.key_snapshot !== "string" || !t.key_snapshot ||
      typeof t.has_api_key !== "boolean" || typeof t.api_key !== "string" ||
      t.has_api_key !== !!t.api_key || !Array.isArray(t.api_keys) ||
      !t.api_keys.every(function (k) { return typeof k === "string"; }) ||
      !Array.isArray(t.key_runtime_indices) ||
      t.key_runtime_indices.length !== t.api_keys.length + 1 ||
      (!t.has_api_key && t.key_runtime_indices[0] !== null) ||
      (hasUnmappedNonemptyKey(t) &&
        !(t.key_runtime_indices.every(function (i) { return i === null; }) &&
          (t.key_statuses == null || (Array.isArray(t.key_statuses) && !t.key_statuses.length))))
    ) return false;
    var statuses = Array.isArray(t.key_statuses) ? t.key_statuses : [];
    return t.key_runtime_indices.every(function (i) {
      return i === null || (Number.isInteger(i) && i >= 0 && statuses.some(function (s) {
        return s && s.index === i && typeof s.blocked === "boolean" &&
          typeof s.exhausted === "boolean" && typeof s.status === "string" && !!s.status;
      }));
    });
  }
  function keyListFromSnapshot(t) {
    var keys = Array.isArray(t.api_keys) ? t.api_keys : [];
    var statuses = Array.isArray(t.key_statuses) ? t.key_statuses : [];
    var indices = Array.isArray(t.key_runtime_indices) ? t.key_runtime_indices : [];
    var list = [];
    function add(value, slot) {
      var runtimeIndex = indices[slot];
      var status = Number.isInteger(runtimeIndex)
        ? statuses.find(function (s) { return s && s.index === runtimeIndex; }) || null
        : null;
      list.push({
        value: value, ref: "__existing_key_index__:" + slot,
        runtimeIndex: runtimeIndex, status: status,
      });
    }
    if (t.has_api_key || keys.length) add(t.api_key || "", 0);
    keys.forEach(function (key, i) { add(key, i + 1); });
    return list;
  }
  function sameKeyIndices(t) {
    return usableKeySnapshot(t) && editingKeyIndices &&
      t.key_runtime_indices.length === editingKeyIndices.length &&
      t.key_runtime_indices.every(function (index, i) { return index === editingKeyIndices[i]; });
  }
  function updateEditingKeyStatuses(t) {
    var statuses = Array.isArray(t.key_statuses) ? t.key_statuses : [];
    tgtKeyList.forEach(function (item) {
      if (item.ref) item.status = Number.isInteger(item.runtimeIndex)
        ? statuses.find(function (s) { return s && s.index === item.runtimeIndex; }) || null : null;
    });
    editingKeyStatusStale = false;
  }
  async function refreshEditingTarget() {
    var session = editingKeySession;
    var name = state.editingTarget && state.editingTarget.name;
    var targets = await loadTargets();
    if (session !== editingKeySession || !state.editingTarget || state.editingTarget.name !== name) return;
    if (!targets) {
      editingKeyStatusStale = true;
    } else {
      var fresh = targets.find(function (t) { return t.name === name; });
      if (!fresh || fresh.key_snapshot !== editingKeySnapshot || !sameKeyIndices(fresh)) {
        editingKeyNeedsReview = true;
      } else if (!editingKeyNeedsReview) {
        updateEditingKeyStatuses(fresh);
      }
    }
    renderKeyList();
  }

  async function reloadEditingKeys(session, name) {
    var targets = await loadTargets();
    if (session !== editingKeySession || !state.editingTarget || state.editingTarget.name !== name) return;
    var fresh = targets && targets.find(function (t) { return t.name === name; });
    if (!usableKeySnapshot(fresh)) {
      editingKeyNeedsReview = true;
      renderKeyList();
      toast("密钥快照不可用，请刷新后重新确认", "error");
      return;
    }
    var additions = tgtKeyList.filter(function (item) { return !item.ref; });
    tgtKeyList = keyListFromSnapshot(fresh).concat(additions);
    state.editingTarget = fresh;
    editingKeySnapshot = fresh.key_snapshot;
    editingKeyIndices = fresh.key_runtime_indices.slice();
    editingKeyNeedsReview = false;
    editingKeyStatusStale = false;
    renderKeyList();
  }

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
    var notice = "";
    if (editingKeySaving && !state.editingTarget) {
      notice = "<p role='status'>目标创建中，暂不能修改密钥。</p>";
    }
    if (createdDraftNeedsReopen) {
      notice = "<p role='alert'>目标已创建，后续修改未保存。请复制草稿并重新打开目标编辑。</p>";
    }
    if (state.editingTarget && editingKeyNeedsReview) {
      notice = "<p role='alert'>密钥快照已变化或不可用，草稿仍保留。请重新载入并确认；已移除的已保存密钥需要重新选择，新增密钥及其他表单草稿会保留。保存前请核对其他草稿，它们可能覆盖其他人更新的配置。 " +
        "<button type='button' class='btn btn-sm readonly-hide' data-reload-key-draft='1'" + (editingKeyBusy ? " disabled" : "") + ">" +
        (editingKeyReloading ? "正在载入…" : "重新载入密钥并确认") + "</button></p>";
    } else if (state.editingTarget && editingKeyStatusStale) {
      notice = "<p role='status'>密钥状态刷新失败，卡片显示上次状态；请刷新后查看。</p>";
    }
    if (!targetPoolsSnapshot || targetPoolOptionsSnapshot !== targetPoolsSnapshot) {
      notice += "<p role='status'>部署池数据不可用或选项可能过期，当前选择已保留；请重新打开编辑查看最新选项。</p>";
    }
    if (!tgtKeyList.length && !notice) {
      container.style.display = "none";
      container.innerHTML = "";
      return;
    }
    container.style.display = "block";
    var isEditing = !!state.editingTarget;
    container.innerHTML = notice + tgtKeyList
      .map(function (item, i) {
        var mask = item.status && item.status.key_mask ? item.status.key_mask : item.ref
          ? (item.value || "空主密钥槽位") : item.value.length <= 8 ? "****" : maskKey(item.value);
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
        if (isEditing && item.status && typeof item.status.blocked === "boolean" &&
            typeof item.status.exhausted === "boolean" && !editingKeyNeedsReview && !editingKeyStatusStale && !editingKeyBusy) {
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
          "'" + ((editingKeyReloading && item.ref) || (editingKeySaving && !isEditing) || createdDraftNeedsReopen ? " disabled" : "") +
          " title='移除' aria-label='移除' style='padding:2px 8px;font-size:11px;color:var(--danger);'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><path d='M6 6l12 12M18 6L6 18'></path></svg></button>";
        var cooldownInfo = "";
        var itemCooldownEnd = keyCooldownEnd(item.status);
        if (item.status && item.status.exhausted && itemCooldownEnd) {
          var cd = new Date(itemCooldownEnd);
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
          "<span style='font-family:var(--font-mono);font-size:12px;color:var(--text-secondary);flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;' title='" +
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
    if ((editingKeySaving && !state.editingTarget) || createdDraftNeedsReopen) {
      toast(createdDraftNeedsReopen ? "目标已创建，请重新打开目标后再添加密钥。" : "创建请求进行中，请等待后再添加密钥。", "error");
      return;
    }
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
    editingDraftRevision++;
    input.value = "";
    renderKeyList();
  }

  function removeKeyFromList(idx) {
    if ((editingKeySaving && !state.editingTarget) || createdDraftNeedsReopen) return;
    if (!Number.isInteger(idx) || idx < 0 || idx >= tgtKeyList.length ||
        (editingKeyReloading && tgtKeyList[idx].ref)) return;
    tgtKeyList.splice(idx, 1);
    editingDraftRevision++;
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
      var t = e.target.closest ? e.target.closest("button") || e.target : e.target;
      if (t.dataset.removeKeyIdx !== undefined) {
        removeKeyFromList(parseInt(t.dataset.removeKeyIdx, 10));
      }
      if (t.dataset.reloadKeyDraft !== undefined && state.editingTarget && !editingKeyBusy) {
        var reloadSession = editingKeySession;
        var reloadName = state.editingTarget.name;
        showConfirm(
          "重新载入已保存的密钥会重置本次密钥移除选择，请重新选择需要移除的密钥。新增密钥和其他表单草稿会保留，保存前请核对，以免覆盖其他人更新的配置。继续吗？",
          async function () {
            if (reloadSession === editingKeySession && !editingKeyBusy) {
              editingKeyBusy = true;
              editingKeyReloading = true;
              renderKeyList();
              try {
                await reloadEditingKeys(reloadSession, reloadName);
              } finally {
                if (reloadSession === editingKeySession) {
                  editingKeyBusy = false;
                  editingKeyReloading = false;
                  renderKeyList();
                }
              }
            }
          },
        );
      }
      var block = t.dataset.blockKeyIdx !== undefined;
      if ((block || t.dataset.unblockKeyIdx !== undefined) && state.editingTarget &&
          !editingKeyNeedsReview && !editingKeyStatusStale && !editingKeyBusy && editingKeySnapshot) {
        var idx = parseInt(block ? t.dataset.blockKeyIdx : t.dataset.unblockKeyIdx, 10);
        var name = state.editingTarget.name;
        var session = editingKeySession;
        var snapshot = editingKeySnapshot;
        showConfirm(block ? "确认屏蔽此 Key？屏蔽后该 Key 将不参与转发，直到手动解除。" : "确认解除屏蔽此 Key？解除后该 Key 将重新参与转发。", async function () {
          if (session !== editingKeySession || editingKeyBusy || editingKeyNeedsReview || editingKeyStatusStale ||
              snapshot !== editingKeySnapshot || !tgtKeyList.some(function (item) { return item.status && item.runtimeIndex === idx; })) return;
          var targetsBeforeOperation = state.targets;
          editingKeyBusy = true;
          renderKeyList();
          try {
            var result = await api("/admin/data/targets/" + encodeURIComponent(name) + "/keys/" + idx + (block ? "/block" : "/unblock"),
              { method: "PUT", headers: { "X-Target-Key-Snapshot": snapshot } });
            if (session !== editingKeySession) return;
            if (result && typeof result.key_snapshot === "string" && result.key_snapshot && result.key_snapshot !== snapshot) {
              editingKeySnapshot = result.key_snapshot;
            } else {
              editingKeyNeedsReview = true;
            }
            editingKeyStatusStale = true;
            toast(block ? "已屏蔽 Key" : "已解除屏蔽", "success");
            await refreshEditingTarget();
          } catch (err) {
            if (session !== editingKeySession) return;
            editingKeyNeedsReview = true;
            toast("操作失败，密钥草稿已保留，请重新确认", "error");
          } finally {
            if (session === editingKeySession) {
              if (state.targets !== targetsBeforeOperation) {
                var latest = state.targets.find(function (item) { return item.name === name; });
                if (!latest || latest.key_snapshot !== editingKeySnapshot || !sameKeyIndices(latest)) {
                  editingKeyNeedsReview = true;
                }
              }
              editingKeyBusy = false;
              renderKeyList();
            }
          }
        });
      }
    });

  function renderTargets() {
    if (modelHint) hideModelHint();
    renderTargetStatusFilters();
    var tb = document.getElementById("targetsTableBody");
    var targets = filteredTargets();
    if (targetsLoadFailed && !targetsLoaded) {
      tb.innerHTML = "<tr class='empty-row'><td colspan='8' role='alert'>加载目标失败，请点击「刷新」重试。</td></tr>";
      return;
    }
    var staleNotice = targetsLoadFailed
      ? "<tr><td colspan='8' role='status'>更新失败，显示上次加载的数据；请点击「刷新」重试。</td></tr>" : "";
    if (!targetPoolsSnapshot) {
      staleNotice += "<tr><td colspan='8' role='status'>部署池数据不可用，池名称及表单选项可能是上次数据；请刷新重试。</td></tr>";
    }
    if (!state.targets.length) {
      tb.innerHTML =
        staleNotice + "<tr class='empty-row'><td colspan='8'>暂无目标配置</td></tr>";
      refreshOpenTargetDetail();
      return;
    }
    if (!targets.length) {
      tb.innerHTML =
        staleNotice + "<tr class='empty-row'><td colspan='8'>当前筛选无目标</td></tr>";
      refreshOpenTargetDetail();
      return;
    }
    tb.innerHTML = staleNotice + targets
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
      .join("");
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
                // Server probes keys sequentially, 5s each; no client deadline.
                { method: "POST", timeoutMs: 0 },
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
        editingDraftRevision++;
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
        // Server bounds each upstream attempt (60s) and may rotate keys/paths.
        timeoutMs: 0,
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
      '<input class="form-control" placeholder="Value (JSON)" style="flex:2;font-size:13px;font-family:var(--font-mono);" value="' +
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
    var session = ++editingKeySession;
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
    if (session !== editingKeySession) return;
    createdDraftNeedsReopen = false;
    document.getElementById("tgtNewKeyInput").disabled = false;
    document.getElementById("addKeyBtn").disabled = false;
    state.editingTarget = t || null;
    editingDraftRevision = 0;
    editingKeyBusy = false;
    editingKeyReloading = false;
    editingKeySaving = false;
    editingKeySnapshot = usableKeySnapshot(t) ? t.key_snapshot : null;
    editingKeyIndices = usableKeySnapshot(t) ? t.key_runtime_indices.slice() : null;
    editingKeyNeedsReview = !!t && (!editingKeySnapshot || targetsLoadFailed);
    editingKeyStatusStale = false;
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
    // Config slot references must stay paired with the snapshot that issued them.
    tgtKeyList = t ? keyListFromSnapshot(t) : [];
    renderKeyList();
    document.getElementById("tgtNewKeyInput").value = "";
    document.getElementById("tgtKeyResetTime").value =
      t && t.key_reset_time ? t.key_reset_time : "";
    renderTargetPoolOptions(t && t.pool ? t.pool : "");
    renderKeyList();
    updateTargetFormVisibility();
    openModal("targetModal");
  }
  // renderTargetPoolOptions 重建目标表单的池下拉（数据来自 /admin/data/pools）。
  function updateTargetPoolAvailability() {
    var sel = document.getElementById("tgtPool");
    var label = targetPoolsSnapshot && targetPoolOptionsSnapshot === targetPoolsSnapshot
      ? "部署池" : "部署池数据不可用或选项可能过期，请重新打开编辑查看";
    sel.setAttribute("aria-label", label);
    sel.setAttribute("title", label);
  }
  function renderTargetPoolOptions(selected) {
    var sel = document.getElementById("tgtPool");
    targetPoolOptionsSnapshot = targetPoolsSnapshot;
    updateTargetPoolAvailability();
    var val = selected || "";
    sel.innerHTML = "<option value=''>不属于任何池</option>";
    (targetPoolsSnapshot || state.pools || [])
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
    var known = (targetPoolsSnapshot || state.pools || []).some(function (p) {
      return String(p.name || "") === val;
    });
    if (val && !known) {
      // Roster absence is not proof of deletion: the roster can be stale after
      // a failed refresh or a concurrent change. Keep the saved membership
      // selectable; choosing "不属于任何池" still unbinds it explicitly.
      var current = document.createElement("option");
      current.value = val;
      current.textContent = poolDisplayName(val);
      sel.appendChild(current);
      sel.value = val;
      return;
    }
    sel.value = val;
    if (sel.value !== val) sel.value = "";
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
  document
    .getElementById("targetDetailTabs")
    .addEventListener("click", function (event) {
      var tab = event.target.closest("[data-target-detail-tab]");
      if (tab) switchTargetDetailTab(tab.dataset.targetDetailTab);
    });
  document
    .getElementById("targetDetailTabs")
    .addEventListener("keydown", function (event) {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      var tabs = Array.prototype.slice.call(
        document.querySelectorAll("[data-target-detail-tab]"),
      );
      var current = tabs.indexOf(document.activeElement);
      if (current < 0) return;
      event.preventDefault();
      var next =
        (current + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) %
        tabs.length;
      tabs[next].focus();
      switchTargetDetailTab(tabs[next].dataset.targetDetailTab);
    });
  document
    .getElementById("targetDetailEditBtn")
    .addEventListener("click", function () {
      var target = (state.targets || []).find(function (candidate) {
        return candidate.name === detailTargetName;
      });
      if (!target) return;
      closeTargetDetail();
      openTargetModal(target);
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
      if (!state.editingTarget && document.getElementById("tgtNewKeyInput").value.trim()) {
        toast("请先添加输入的密钥，再保存目标。", "error");
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

      if (createdDraftNeedsReopen) {
        toast("目标已创建。请先复制未保存的修改，再重新打开目标编辑。", "error");
        return;
      }
      if (editingKeyBusy || (state.editingTarget && (editingKeyNeedsReview || !editingKeySnapshot))) {
        toast("密钥快照不可用或操作正在进行，请重新确认后保存", "error");
        return;
      }

      var saveSession = editingKeySession;
      var saveRevision = editingDraftRevision;
      var wasEditing = !!state.editingTarget;
      editingKeyBusy = true;
      editingKeySaving = true;
      if (!wasEditing) {
        document.getElementById("tgtNewKeyInput").disabled = true;
        document.getElementById("addKeyBtn").disabled = true;
        renderKeyList();
      }
      try {
        if (state.editingTarget) {
          renderKeyList();
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
            headers: { "X-Target-Key-Snapshot": editingKeySnapshot },
            body: updateBody,
          });
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
        }
        if (saveSession !== editingKeySession) {
          await loadTargets();
          return;
        }
        if (!wasEditing) {
          document.getElementById("tgtNewKeyInput").disabled = false;
          document.getElementById("addKeyBtn").disabled = false;
        }
        if (saveRevision !== editingDraftRevision) {
          editingKeyBusy = false;
          editingKeySaving = false;
          if (wasEditing) editingKeyNeedsReview = true;
          else {
            createdDraftNeedsReopen = true;
            document.getElementById("tgtNewKeyInput").disabled = true;
            document.getElementById("addKeyBtn").disabled = true;
          }
          renderKeyList();
          toast(wasEditing ? "请求时的内容已保存，后续修改未保存；请重新确认密钥" :
            "目标已创建，后续修改未保存。请复制草稿并重新打开目标编辑。", "error");
          await loadTargets();
          return;
        }
        toast(wasEditing ? "目标已更新" : "目标已创建", "success");
        state.editingTarget = null;
        editingKeySession++;
        editingKeyBusy = false;
        editingKeySaving = false;
        closeModal("targetModal");
        await loadTargets();
      } catch (e) {
        if (saveSession !== editingKeySession) return;
        if (!wasEditing) {
          document.getElementById("tgtNewKeyInput").disabled = false;
          document.getElementById("addKeyBtn").disabled = false;
        }
        editingKeyBusy = false;
        editingKeySaving = false;
        if (state.editingTarget) {
          editingKeyNeedsReview = true;
        }
        renderKeyList();
        toast("保存失败: " + e.message, "error");
      }
    });

  return {
    loadTargets: loadTargets,
    beginSchedulingRefresh: beginSchedulingRefresh,
    schedulingRefreshCurrent: schedulingRefreshCurrent,
    awaitSchedulingRefresh: awaitSchedulingRefresh,
    targetPoolsAvailable: targetPoolsAvailable,
    validPoolRoster: validPoolRoster,
    publishSchedulingRefresh: publishSchedulingRefresh,
    failSchedulingRefresh: failSchedulingRefresh,
    fetchSchedulingRuntime: fetchSchedulingRuntime,
    targetHealth: targetHealth,
    targetHealthCounts: targetHealthCounts,
    filteredTargets: filteredTargets,
    renderTargetStatusFilters: renderTargetStatusFilters,
    setTargetFilter: setTargetFilter,
    switchTargetPageTab: switchTargetPageTab,
    openTargetDetail: openTargetDetail,
    closeTargetDetail: closeTargetDetail,
    switchTargetDetailTab: switchTargetDetailTab,
    refreshOpenTargetDetail: refreshOpenTargetDetail,
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
