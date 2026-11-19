"use strict";
// Targets owns the shared page state, the scheduling refresh and the public exports.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targets", function (app) {
  app.requires(["targetsSave"]);
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtTime = app.bind("core", "fmtTime");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var renderKeyList = app.bind("targetsKeyList", "renderKeyList");
  var renderTargets = app.bind("targetsList", "renderTargets");
  var syncEditingWithRefresh = app.bind("targetsKeyList", "syncEditingWithRefresh");
  var updateTargetPoolAvailability = app.bind("targetsModal", "updateTargetPoolAvailability");
  // Shared mutable page state. Only this object crosses file boundaries; sibling
  // modules reach it through `shared` and never keep their own copies.
  var ts = {
    targetsLoadSequence: 0,
    targetsLoaded: false,
    targetsLoadFailed: false,
    targetPoolsSnapshot: null,
    targetPoolOptionsSnapshot: null,
    schedulingRefresh: null,
    targetFilter: "all",
    detailTargetName: "",
    detailTab: "overview",
    tgtKeyList: [],
    // Editor state. editingETag is the server ETag the draft is based on; the server
    // arbitrates conflicts through If-Match. editingKeyIdsOk false means the saved
    // keys cannot be referenced safely: the key list is display-only and saves omit it.
    editingETag: "",
    editingKeyIdsOk: true,
    editingConflict: false,
    editingRemote: "",
    editingKeyStatusStale: false,
    editingKeyBusy: false,
    editingKeyReloading: false,
    editingKeySaving: false,
    editingKeySession: 0,
    healthMemo: null,
    runtimeEpoch: 0,
  };
  function targetPoolsAvailable() { return ts.targetPoolsSnapshot !== null; }
  function beginSchedulingRefresh() {
    if (ts.schedulingRefresh) ts.schedulingRefresh.finish({ superseded: true });
    var refresh = {};
    refresh.promise = new Promise(function (resolve) { refresh.finish = resolve; });
    ts.schedulingRefresh = refresh;
    return ++ts.targetsLoadSequence;
  }
  function schedulingRefreshCurrent(sequence) { return sequence === ts.targetsLoadSequence; }
  async function awaitSchedulingRefresh() {
    while (true) {
      var refresh = ts.schedulingRefresh;
      var result = await refresh.promise;
      if (refresh !== ts.schedulingRefresh || result.superseded) continue;
      if (result.error) throw result.error;
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
  // ---- Issue 212: upstream generation performance (read-only /admin/metrics) ----
  // model_performance is absent on older servers and for targets without samples;
  // every helper below renders nothing (or 暂无性能样本 in the detail view) then.
  function perfNumber(value) {
    var n = Number(value);
    return isFinite(n) ? n : 0;
  }
  // TTFT and TPS percentiles are histogram bucket representatives (ms / tokens per second).
  function formatPerfTTFT(ms) {
    var n = perfNumber(ms);
    return n >= 1000 ? (n / 1000).toFixed(1) + "s" : Math.round(n) + "ms";
  }
  function formatPerfTPS(tps) {
    return Math.round(perfNumber(tps)) + " tok/s";
  }
  function perfModels(rt) {
    return rt && Array.isArray(rt.model_performance)
      ? rt.model_performance.filter(function (m) {
          return m && typeof m === "object" && typeof m.upstream_model === "string" && !!m.upstream_model;
        })
      : [];
  }
  function perfModelsByName(models) {
    return models.slice().sort(function (a, b) {
      return String(a.upstream_model).localeCompare(String(b.upstream_model));
    });
  }
  // A model's effective sample count: TTFT and TPS each count, the larger wins.
  function perfSampleScore(m) {
    return Math.max(perfNumber(m.ttft && m.ttft.samples), perfNumber(m.tps && m.tps.samples));
  }
  // The representative model has the most samples; ties go to the smaller upstream name.
  function representativePerfModel(models) {
    var best = null;
    models.forEach(function (m) {
      if (!best) {
        best = m;
        return;
      }
      var diff = perfSampleScore(m) - perfSampleScore(best);
      if (diff > 0 || (diff === 0 && String(m.upstream_model).localeCompare(String(best.upstream_model)) < 0)) best = m;
    });
    return best;
  }
  // One line per model for the list/summary tooltip: percentiles, samples, penalty,
  // non-stream attempts and the snapshot time.
  function perfModelTitle(m) {
    var ttft = m.ttft || {};
    var tps = m.tps || {};
    var penalty = m.penalty_ttft || {};
    var nonStream = m.non_stream || {};
    var parts = [
      String(m.upstream_model),
      "TTFT p50 " + formatPerfTTFT(ttft.p50) + " / p90 " + formatPerfTTFT(ttft.p90),
      "TPS p50 " + formatPerfTPS(tps.p50) + " / p90 " + formatPerfTPS(tps.p90),
      "样本 TTFT " + perfNumber(ttft.samples) + " / TPS " + perfNumber(tps.samples),
      "惩罚样本 " + perfNumber(m.penalty_samples),
    ];
    if (perfNumber(penalty.samples) > 0) {
      parts.push("惩罚 TTFT p50 " + formatPerfTTFT(penalty.p50) + " / p90 " + formatPerfTTFT(penalty.p90));
    }
    if (perfNumber(nonStream.samples) > 0) {
      parts.push(
        "非流式 " + perfNumber(nonStream.samples) + " 次，均值 " +
          formatPerfTTFT(nonStream.avg_ms) + " / " + Math.round(perfNumber(nonStream.avg_output_tokens)) + " tokens",
      );
    } else {
      parts.push("非流式 无样本");
    }
    if (m.updated_at) parts.push("更新 " + fmtTime(m.updated_at));
    if (m.insufficient) parts.push("样本不足");
    return parts.join(" · ");
  }
  // Issue 224: performance metrics live only in the status badge tooltip. The
  // summary exposes the full per-model title plus whether the representative
  // model has any samples, so callers can omit the performance segment entirely.
  function performanceSummary(rt) {
    var models = perfModels(rt);
    if (!models.length) return null;
    var best = representativePerfModel(models);
    return {
      title: perfModelsByName(models).map(perfModelTitle).join("\n"),
      hasSamples: perfSampleScore(best) > 0,
    };
  }
  // Per-model table for the target detail overview. All columns are informational
  // (no sorting); the table still declares data-sort-none for the header gate.
  function performanceDetailHtml(rt) {
    var models = perfModelsByName(perfModels(rt));
    if (!models.length) return "<p class='target-perf-empty'>暂无性能样本</p>";
    var rows = models
      .map(function (m) {
        var ttft = m.ttft || {};
        var tps = m.tps || {};
        var nonStream = m.non_stream || {};
        var ttftText = perfNumber(ttft.samples) > 0
          ? formatPerfTTFT(ttft.p50) + " / " + formatPerfTTFT(ttft.p90)
          : "—";
        var tpsText = perfNumber(tps.samples) > 0
          ? formatPerfTPS(tps.p50) + " / " + formatPerfTPS(tps.p90)
          : "—";
        var samplesText = perfNumber(ttft.samples) + " / " + perfNumber(tps.samples);
        var streamText = perfNumber(nonStream.samples) > 0
          ? perfNumber(nonStream.samples) + " 次 · " + formatPerfTTFT(nonStream.avg_ms) +
            " · " + Math.round(perfNumber(nonStream.avg_output_tokens)) + " tokens"
          : "—";
        var updated = m.updated_at ? " title='更新 " + esc(fmtTime(m.updated_at)) + "'" : "";
        return (
          "<tr><td class='mono'" + updated + ">" + esc(m.upstream_model) + "</td>" +
          "<td>" + ttftText + "</td>" +
          "<td>" + tpsText + "</td>" +
          "<td>" + samplesText + "</td>" +
          "<td>" + perfNumber(m.penalty_samples) + "</td>" +
          "<td>" + streamText + "</td>" +
          "<td>" + (m.insufficient ? "<span class='quota-badge warn'>样本不足</span>" : "—") + "</td></tr>"
        );
      })
      .join("");
    return (
      "<div class='target-perf-wrap'><table class='target-perf-table'><thead><tr>" +
      "<th data-sort-none>上游模型</th><th data-sort-none>TTFT p50 / p90</th><th data-sort-none>TPS p50 / p90</th>" +
      "<th data-sort-none>样本 (TTFT/TPS)</th><th data-sort-none>惩罚样本</th><th data-sort-none>非流式</th>" +
      "<th data-sort-none>状态</th>" +
      "</tr></thead><tbody>" + rows + "</tbody></table></div>"
    );
  }
  function publishSchedulingRefresh(sequence, targets, runtime, pools) {
    if (!schedulingRefreshCurrent(sequence)) return null;
    // The refresh settles with success only when every step below ran. A throw
    // propagates to failSchedulingRefresh, which settles it with the error.
    var refresh = ts.schedulingRefresh;
    if (!validTargets(targets)) throw new Error("目标列表格式无效");
    state.targets = targets.targets;
    // fetchSchedulingRuntime hands back both the per-target runtime and the
    // optional shadow_routing array; accept a bare map for compatibility.
    var runtimeMap = runtime && typeof runtime === "object" && runtime.runtime !== undefined ? runtime.runtime : runtime;
    state.runtime = runtimeMap || null;
    state.shadowRouting = runtime && Array.isArray(runtime.shadow) ? runtime.shadow : null;
    ts.runtimeEpoch++;
    ts.targetPoolsSnapshot = validPoolRoster(pools) ? pools.pools : null;
    if (ts.targetPoolsSnapshot) state.pools = ts.targetPoolsSnapshot;
    updateTargetPoolAvailability();
    ts.targetsLoaded = true;
    ts.targetsLoadFailed = false;
    // Only the edited target matters here: another target changing never touches the editor.
    if (state.editingTarget) syncEditingWithRefresh();
    if (document.getElementById("targetModal").classList.contains("open")) renderKeyList();
    renderTargets();
    if (app.modules.clients) app.modules.clients.onSchedulingRefresh();
    if (app.modules.pools) app.modules.pools.onSchedulingRefresh();
    refresh.finish({});
    return state.targets;
  }
  function failSchedulingRefresh(sequence, error) {
    if (!schedulingRefreshCurrent(sequence)) return false;
    var refresh = ts.schedulingRefresh;
    try {
      ts.targetsLoadFailed = true;
      ts.targetPoolsSnapshot = null;
      updateTargetPoolAvailability();
      if (document.getElementById("targetModal").classList.contains("open")) renderKeyList();
      renderTargets();
      if (app.modules.clients) app.modules.clients.onSchedulingRefresh();
      if (app.modules.pools) app.modules.pools.onSchedulingRefresh();
    } finally {
      // Waiters must settle even if a render above throws.
      refresh.finish({ error: error || new Error("目标列表加载失败") });
    }
    return true;
  }
  async function loadTargets(propagateError) {
    var sequence = beginSchedulingRefresh();
    try {
      // Pool pause lives on the roster, and another admin session may resume it
      // between refreshes. Reload it every time. A failed fetch marks the roster
      // unavailable (no snapshot, so cached pause is never treated as fact);
      // state.pools keeps the last list only so names can still be displayed.
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
  // （S5）与只读的 shadow_routing（Issue 212）。失败时清空快照，避免把过期
  // 运行态当成当前健康状态。返回 { runtime, shadow }；captureOnly 时由调用方
  // 经 publishSchedulingRefresh 一起发布。
  async function fetchSchedulingRuntime(captureOnly) {
    var epoch = ts.runtimeEpoch;
    var map = null;
    var shadow = null;
    try {
      var m = await api("/admin/metrics");
      map = {};
      if (Array.isArray(m.targets_runtime)) {
        m.targets_runtime.forEach(function (rt) {
          map[String(rt.name || "").toLowerCase()] = rt;
        });
      }
      shadow = Array.isArray(m.shadow_routing)
        ? m.shadow_routing.filter(function (s) { return s && typeof s === "object"; })
        : null;
    } catch (e) {
      // Stale snapshots would misreport health; metrics stay best-effort.
      map = null;
      shadow = null;
    }
    if (!captureOnly && epoch === ts.runtimeEpoch) {
      state.runtime = map;
      state.shadowRouting = map ? shadow : null;
      ts.runtimeEpoch++;
    }
    return { runtime: map, shadow: shadow };
  }
  return {
    shared: ts,
    loadTargets: loadTargets,
    beginSchedulingRefresh: beginSchedulingRefresh,
    schedulingRefreshCurrent: schedulingRefreshCurrent,
    awaitSchedulingRefresh: awaitSchedulingRefresh,
    targetPoolsAvailable: targetPoolsAvailable,
    validPoolRoster: validPoolRoster,
    publishSchedulingRefresh: publishSchedulingRefresh,
    failSchedulingRefresh: failSchedulingRefresh,
    fetchSchedulingRuntime: fetchSchedulingRuntime,
    performanceSummary: performanceSummary,
    performanceDetailHtml: performanceDetailHtml,
    formatPerfTTFT: formatPerfTTFT,
    formatPerfTPS: formatPerfTPS,
    targetHealth: app.bind("targetsHealth", "targetHealth"),
    targetHealthCounts: app.bind("targetsHealth", "targetHealthCounts"),
    filteredTargets: app.bind("targetsHealth", "filteredTargets"),
    renderTargetStatusFilters: app.bind("targetsList", "renderTargetStatusFilters"),
    setTargetFilter: app.bind("targetsList", "setTargetFilter"),
    switchTargetPageTab: app.bind("targetsList", "switchTargetPageTab"),
    openTargetDetail: app.bind("targetsDetail", "openTargetDetail"),
    closeTargetDetail: app.bind("targetsDetail", "closeTargetDetail"),
    switchTargetDetailTab: app.bind("targetsDetail", "switchTargetDetailTab"),
    refreshOpenTargetDetail: app.bind("targetsDetail", "refreshOpenTargetDetail"),
    refreshEditingTarget: app.bind("targetsKeyList", "refreshEditingTarget"),
    keyStatusColor: app.bind("targetsHealth", "keyStatusColor"),
    renderKeyList: app.bind("targetsKeyList", "renderKeyList"),
    addKeyToList: app.bind("targetsKeyList", "addKeyToList"),
    removeKeyFromList: app.bind("targetsKeyList", "removeKeyFromList"),
    renderTargets: app.bind("targetsList", "renderTargets"),
    hideModelHint: app.bind("targetsList", "hideModelHint"),
    showModelHint: app.bind("targetsList", "showModelHint"),
    updateTargetFormVisibility: app.bind("targetsModal", "updateTargetFormVisibility"),
    applyEndpointPlaceholder: app.bind("targetsModal", "applyEndpointPlaceholder"),
    renderTgtModelSuggestions: app.bind("targetsMappings", "renderTgtModelSuggestions"),
    addModelMappingRow: app.bind("targetsMappings", "addModelMappingRow"),
    testModelForRow: app.bind("targetsMappings", "testModelForRow"),
    collectModelMappings: app.bind("targetsMappings", "collectModelMappings"),
    addCustomHeaderRow: app.bind("targetsMappings", "addCustomHeaderRow"),
    collectCustomHeaders: app.bind("targetsMappings", "collectCustomHeaders"),
    addCustomBodyRow: app.bind("targetsMappings", "addCustomBodyRow"),
    collectCustomBody: app.bind("targetsMappings", "collectCustomBody"),
    collectCustomBodyRaw: app.bind("targetsMappings", "collectCustomBodyRaw"),
    openTargetModal: app.bind("targetsModal", "openTargetModal"),
    renderTargetPoolOptions: app.bind("targetsModal", "renderTargetPoolOptions"),
  };
});
