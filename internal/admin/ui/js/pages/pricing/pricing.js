"use strict";
// Pricing facade: re-exports the public surface that other pages bind to ("pricing".*).
// Implementation lives in the pricing-*.js modules loaded before it.
AdminApp.register("pricing", function (app) {
  var parts = {
    "pricingShared": ["pricingTargets", "pricingModels", "pricingSelected", "pricingGroupKey", "pricingAttrKey", "pricingDetailId", "pricingParseAttrKey", "pricingStartIsZero", "pricingIsTemplate0", "pricingRate", "pricingWeekdays", "pricingRatesHtml", "pricingRateGridHtml", "pricingRangeHtml", "toLocalInputValue", "pricingTargetExists", "pricingTargetMissing", "pricingTargetOptions", "pricingModelLabel"],
    "pricingRates": ["renderPricingContext", "validatePricingContext", "syncPricingContextInput", "pricingContextClick", "conditionList"],
    "pricingPeriods": ["updatePricingModeUI", "renderPricingPeriods", "syncPricingPeriod", "collectPricingPeriods"],
    "pricingList": ["renderPricingDetail", "renderPricingTemplates", "pricingTargetMeta", "loadPricingTemplates", "deletePricingTemplate"],
    "pricingModal": ["updatePricingScopeNotice", "renderPricingModelSelect", "openPricingModal"],
  };
  var exported = {};
  Object.keys(parts).forEach(function (module) {
    parts[module].forEach(function (name) {
      var fn = app.modules[module][name];
      if (typeof fn !== "function") throw new Error("模块导出缺失: " + module + "." + name);
      exported[name] = fn;
    });
  });
  return exported;
});
