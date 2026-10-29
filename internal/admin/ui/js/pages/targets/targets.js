"use strict";
// Targets owns the shared page state, the scheduling refresh and the public exports.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targets", function (app) {
  app.requires(["targetsSave"]);
  var state = app.state;
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
  function publishSchedulingRefresh(sequence, targets, runtime, pools) {
    if (!schedulingRefreshCurrent(sequence)) return null;
    // The refresh settles with success only when every step below ran. A throw
    // propagates to failSchedulingRefresh, which settles it with the error.
    var refresh = ts.schedulingRefresh;
    if (!validTargets(targets)) throw new Error("目标列表格式无效");
    state.targets = targets.targets;
    state.runtime = runtime;
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
  // （S5）。失败时清空快照，避免把过期运行态当成当前健康状态。
  // captureOnly: return the snapshot to the caller (a refresh in progress
  // publishes it). Otherwise store it directly, unless a refresh published a
  // newer runtime while this request was in flight.
  async function fetchSchedulingRuntime(captureOnly) {
    var epoch = ts.runtimeEpoch;
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
    if (!captureOnly && epoch === ts.runtimeEpoch) {
      state.runtime = map;
      ts.runtimeEpoch++;
    }
    return map;
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
    openTargetModal: app.bind("targetsModal", "openTargetModal"),
    renderTargetPoolOptions: app.bind("targetsModal", "renderTargetPoolOptions"),
  };
});
