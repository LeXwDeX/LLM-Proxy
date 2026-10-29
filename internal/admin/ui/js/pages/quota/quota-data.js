"use strict";
// Quota data loading: editor prerequisites, page load, dictionary option loading, refresh and tabs.
AdminApp.register("quotaData", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var api = app.bind("core", "api");
  var refreshUsageModelGroupFilter = app.bind("usage", "refreshUsageModelGroupFilter");
  var loadTargets = app.bind("targets", "loadTargets");
  var awaitSchedulingRefresh = app.bind("targets", "awaitSchedulingRefresh");
  var renderModelGroups = app.bind("quotaGroups", "renderModelGroups");
  var renderDictTables = app.bind("quotaLevels", "renderDictTables");
  var renderQuotaSummaryFilters = app.bind("quotaSummary", "renderQuotaSummaryFilters");
  var renderQuotaSummary = app.bind("quotaSummary", "renderQuotaSummary");
  var renderQuotaSyncClients = app.bind("quotaSync", "renderQuotaSyncClients");
  var quotaDimensions = app.modules.quotaModel.quotaDimensions;
  var shared = app.modules.quotaModel.shared;
  async function loadQuotaEditorData() {
    var requests = [];
    requests.push(loadTargets(true));
    if (!state.catalogAllCache)
      requests.push(
        api("/admin/data/catalog").then(function (data) {
          state.catalogAllCache = Array.isArray(data.models) ? data.models : [];
        }),
      );
    await Promise.all(requests);
    await awaitSchedulingRefresh();
  }
  // Table bodies that start as "加载中..." in the page markup. A failed first load replaces them
  // with an error row so no table stays on the placeholder; tables that already hold data keep it.
  function showLoadFailure(id, colspan, message) {
    document.getElementById(id).innerHTML =
      "<tr class='empty-row'><td colspan='" + colspan + "'>加载失败: " + esc(message) + "</td></tr>";
  }
  function showLoadFailureIfLoading(id, colspan, message) {
    if (/加载中/.test(document.getElementById(id).innerHTML)) showLoadFailure(id, colspan, message);
  }
  function renderLoadedSections() {
    try {
      renderQuotaSummaryFilters();
      renderQuotaSummary();
    } catch (e) {
      showLoadFailure("quotaSummaryBody", quotaDimensions.length + 7, e.message);
    }
    try {
      renderQuotaSyncClients();
    } catch (e) {
      document.getElementById("quotaSyncTargetsPanel").innerHTML =
        "<span style='color:var(--text-muted);padding:4px 6px;display:block;'>加载失败: " +
        esc(e.message) +
        "</span>";
    }
    try {
      renderModelGroups();
    } catch (e) {
      showLoadFailure("modelGroupsBody", 5, e.message);
    }
    try {
      renderDictTables();
    } catch (e) {
      showLoadFailure("jobLevelsBody", quotaDimensions.length + 2, e.message);
      showLoadFailure("projectGroupsBody", 2, e.message);
    }
  }
  async function loadQuotaManagement() {
    var version = ++shared.quotaLoadVersion;
    ++shared.dictLoadVersion;
    // The 30-day aggregate scans many usage events and is far slower than the rest, so it is
    // fetched in parallel but never blocks the first render.
    var usagePromise = api(
      "/admin/data/usage/aggregate?group_by=day&from=" +
        encodeURIComponent(
          new Date(Date.now() - 30 * 24 * 3600 * 1000).toISOString(),
        ) +
        "&to=" +
        encodeURIComponent(new Date().toISOString()),
      { timeoutMs: 60000 },
    ).then(
      function (data) {
        return data;
      },
      function () {
        return { by_client: [], unavailable: true };
      },
    );
    try {
      var results = await Promise.all([
        api("/admin/data/quota/summary"),
        api("/admin/data/model-groups"),
        api("/admin/data/job-levels"),
        api("/admin/data/project-groups"),
        loadQuotaEditorData(),
      ]);
      if (version !== shared.quotaLoadVersion) return;
      if (Object.keys(shared.modelGroupNeedsRefresh).length &&
          (!results[1] || !Object.prototype.hasOwnProperty.call(results[1], "model_groups") ||
            (results[1].model_groups != null &&
              (!Array.isArray(results[1].model_groups) || !results[1].model_groups.every(function (group) {
                return group && typeof group.name === "string" && !!group.name;
              }))))) throw new Error("模型组列表格式无效");
      ++shared.dictLoadVersion;
      state.quotaSummary = results[0].clients || [];
      state.modelGroups = results[1].model_groups || [];
      shared.modelGroupNeedsRefresh = Object.create(null);
      state.modelGroupsLoaded = true;
      refreshUsageModelGroupFilter();
      state.jobLevels = Array.isArray(results[2].job_levels)
        ? results[2].job_levels
        : [];
      state.projectGroups = Array.isArray(results[3].project_groups)
        ? results[3].project_groups
        : [];
      state.dictsLoaded = true;
      state.clientUsage30d = {};
      state.clientUsage30dPending = true;
      state.clientUsage30dUnavailable = false;
    } catch (e) {
      if (version !== shared.quotaLoadVersion) return;
      showLoadFailure("quotaSummaryBody", quotaDimensions.length + 7, e.message);
      // Only tables still showing the page's initial placeholder are replaced; loaded or
      // annotated content (e.g. 已过期 markers) stays.
      showLoadFailureIfLoading("modelGroupsBody", 5, e.message);
      showLoadFailureIfLoading("jobLevelsBody", quotaDimensions.length + 2, e.message);
      showLoadFailureIfLoading("projectGroupsBody", 2, e.message);
      return;
    }
    renderLoadedSections();
    var usage = await usagePromise;
    if (version !== shared.quotaLoadVersion) return;
    state.clientUsage30d = {};
    state.clientUsage30dPending = false;
    state.clientUsage30dUnavailable = !!(usage && usage.unavailable);
    ((usage && usage.by_client) || []).forEach(function (item) {
      state.clientUsage30d[item.key] = item;
    });
    try {
      renderQuotaSummary();
    } catch (e) {
      showLoadFailure("quotaSummaryBody", quotaDimensions.length + 7, e.message);
    }
  }
  async function loadDictOptions() {
    if (state.dictsLoaded) return;
    var version = shared.dictLoadVersion;
    var results = await Promise.all([
      api("/admin/data/job-levels"),
      api("/admin/data/project-groups"),
    ]);
    if (version !== shared.dictLoadVersion) {
      if (!state.dictsLoaded) return loadDictOptions();
      return;
    }
    state.jobLevels = Array.isArray(results[0].job_levels)
      ? results[0].job_levels
      : [];
    state.projectGroups = Array.isArray(results[1].project_groups)
      ? results[1].project_groups
      : [];
    state.dictsLoaded = true;
  }
  document
    .getElementById("refreshQuotaBtn")
    .addEventListener("click", loadQuotaManagement);

  /* ===== Quota Tab Switching ===== */
  document.querySelectorAll(".quota-tab").forEach(function (tab) {
    tab.addEventListener("click", function () {
      document.querySelectorAll(".quota-tab").forEach(function (t) {
        t.classList.remove("active");
      });
      document.querySelectorAll(".quota-tab-panel").forEach(function (p) {
        p.classList.remove("active");
      });
      tab.classList.add("active");
      var panel = document.getElementById("quota-tab-" + tab.dataset.quotaTab);
      if (panel) panel.classList.add("active");
    });
  });
  return {
    loadQuotaEditorData: loadQuotaEditorData,
    loadQuotaManagement: loadQuotaManagement,
    loadDictOptions: loadDictOptions,
  };
});
