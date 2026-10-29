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
  "quotaLevels": ["dictEmptyLabel", "dictOptionsHTML", "renderDictTables", "openJobLevelModal", "openDictModal"],
  "quotaSummary": ["quotaConvertedDetail", "quotaRowStatus", "quotaBarCell", "quotaGroupSummary", "renderQuotaSummaryFilters", "quotaTotalTok", "renderQuotaSummary"],
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
    document.getElementById("quotaSummaryTableHead").innerHTML =
      "<tr><th></th><th>客户端</th>" +
      quotaDimensions
        .map(function (dim) {
          return "<th>" + esc(dim.label) + "已用 / 可用</th>";
        })
        .join("") +
      "<th title='普通输入 + 输出 + 缓存读取 + 缓存写入 + 工具输入；缺失显示不可用'>近30天 Token</th>" +
      "<th title='聚合接口 estimated_cost 已含模型组扣费系数，前端不再次折算；返回 0 显示 $0.00，缺失显示不可用'>估算费用（含模型组系数）</th>" +
      "<th title='各模型组额度独立生效，不是共享额度池'>模型组</th><th>状态</th><th>操作</th></tr>";
    document.getElementById("jobLevelTableHead").innerHTML =
      "<tr><th>职级</th>" +
      quotaDimensions
        .map(function (dim) {
          return "<th>" + esc(dim.label) + "额度倍率</th>";
        })
        .join("") +
      "<th>操作</th></tr>";
  }
  renderQuotaDimensionControls();
  window.modelGroupModels = exported.modelGroupModels;
  window.modelGroupLabel = exported.modelGroupLabel;
  window.loadQuotaManagement = exported.loadQuotaManagement;
  exported.renderQuotaDimensionControls = renderQuotaDimensionControls;
  return exported;
});
