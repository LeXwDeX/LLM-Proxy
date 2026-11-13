"use strict";
// Quota facade: owns the quota dimension controls and re-exports the public surface that
// other pages bind to ("quota".*). Implementation lives in the quota-*.js modules loaded before it.
AdminApp.register("quota", function (app) {
  var esc = app.bind("core", "esc");
  var quotaDimensions = app.modules.quotaModel.quotaDimensions;
  var parts = {
  "quotaModel": ["modelGroupModels", "modelGroupLabel", "quotaTargetByName", "quotaModelLabel", "quotaTargetOptions", "quotaModelOptions", "globalQuotaGroupByName", "jobLevelEntryName", "jobLevelByName", "jobLevelCoef", "jobLevelCoefText", "quotaNumber", "quotaResetText"],
  "quotaData": ["loadQuotaEditorData", "loadQuotaManagement", "loadDictOptions"],
  "quotaGroups": ["renderModelGroups", "renderModelGroupBindings", "collectModelGroupBindings", "openModelGroupModal"],
  "quotaLevels": ["dictEmptyLabel", "dictOptionsHTML", "renderDictTables", "paintJobLevelSort", "openJobLevelModal", "openDictModal"],
  "quotaSummary": ["quotaConvertedDetail", "quotaRowStatus", "quotaBarCell", "quotaGroupSummary", "renderQuotaSummaryFilters", "paintQuotaSummarySort", "quotaTotalTok", "renderQuotaSummary"],
  "quotaDrawer": ["openQuotaEditDrawer", "renderQuotaEditGroups", "quotaEffectivePreview", "syncQuotaEditGroups"],
  "quotaSync": ["renderQuotaSyncClients", "applyQuotaSyncMode", "updateQuotaSyncTargetsLabel", "renderQuotaSyncTargets"],
  };
  var exported = {};
  Object.keys(parts).forEach(function (module) {
    parts[module].forEach(function (name) {
      var fn = app.modules[module][name];
      if (typeof fn !== "function") throw new Error("模块导出缺失: " + module + "." + name);
      exported[name] = fn;
    });
  });
  function renderQuotaDimensionControls() {
    [
      ["jobLevelDimensions", "jobLevel", "可用额度倍率"],
      ["modelGroupDimensions", "modelGroup", "额度基数 ($)"],
    ].forEach(function (spec) {
      var container = document.getElementById(spec[0]);
      container.innerHTML = quotaDimensions
        .map(function (dim) {
          return (
            "<div class='form-group' style='margin:0;'><label style='font-size:12px;'>" +
            esc(dim.label + spec[2]) +
            "</label><input class='form-control readonly-disable' id='" +
            esc(spec[1] + "-" + dim.key) +
            "' type='number' min='0' step='0.01' placeholder='" +
            (spec[1] === "jobLevel" ? "留空 = 1；0 = 不限" : "必填；0 = 已绑定职级时该周期不限") +
            "'" + (spec[1] === "modelGroup" ? " required" : "") + "></div>"
          );
        })
        .join("");
    });
    // Sort keys (data-sort) match the getters in quota-summary.js / quota-levels.js: name, dim_<key>, ...
    // Each header is replaced here, so the owning sorter repaints it afterwards.
    document.getElementById("quotaSummaryTableHead").innerHTML =
      "<tr><th data-sort-none></th><th data-sort='name'>客户端</th>" +
      quotaDimensions
        .map(function (dim) {
          return "<th data-sort='dim_" + esc(dim.key) + "' title='按已用占可用上限的比例排序；不限与不可用排在最后'>" + esc(dim.label) + "已用 / 可用</th>";
        })
        .join("") +
      "<th data-sort='tokens' title='普通输入 + 输出 + 缓存读取 + 缓存写入 + 工具输入；缺失显示不可用'>近30天 Token</th>" +
      "<th data-sort='cost' title='聚合接口 estimated_cost 已含模型组扣费系数，前端不再次折算；返回 0 显示 $0.00，缺失显示不可用'>估算费用（含模型组系数）</th>" +
      "<th data-sort='groups' title='各模型组额度独立生效，不是共享额度池'>模型组</th><th data-sort='status' title='按严重程度排序：计费校准不可用、职级未配置、模型组超限、接近限额、正常、不限、未配置'>状态</th><th data-sort-none>操作</th></tr>";
    exported.paintQuotaSummarySort();
    document.getElementById("jobLevelTableHead").innerHTML =
      "<tr><th data-sort='name'>职级</th>" +
      quotaDimensions
        .map(function (dim) {
          return "<th data-sort='dim_" + esc(dim.key) + "' title='缺省为 ×1；0 表示不限，排在最大'>" + esc(dim.label) + "额度倍率</th>";
        })
        .join("") +
      "<th data-sort-none>操作</th></tr>";
    exported.paintJobLevelSort();
  }
  renderQuotaDimensionControls();
  window.modelGroupModels = exported.modelGroupModels;
  window.modelGroupLabel = exported.modelGroupLabel;
  window.loadQuotaManagement = exported.loadQuotaManagement;
  exported.renderQuotaDimensionControls = renderQuotaDimensionControls;
  return exported;
});
