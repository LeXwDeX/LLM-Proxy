"use strict";
// Usage shared state: one explicit object owned by this module; the other usage modules read and write it through store().
AdminApp.register("usage_state", function (app) {
  var S = {
    usageChartMetric: "tokens",
    usageChartBuckets: [],
    usageChartGroupBy: "day",
    usageChartRange: null,
    usageLoadVersion: 0,
    usageSnapshot: null,
    usageDenominator: { total: null, usageIncomplete: false, costIncomplete: false },
    activeRange: "today",
    activeUsageTab: "group",
  };
  return {store: function () { return S; }};
});
