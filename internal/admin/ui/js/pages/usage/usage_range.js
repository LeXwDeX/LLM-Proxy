"use strict";
// Usage time range helpers: preset windows and aggregate bucket granularity.
AdminApp.register("usageRange", function (app) {
  var rangeWindow = app.bind("overview", "rangeWindow");
  // Presets share the overview's local-day windows (overview.rangeWindow):
  // 7d is today plus the six previous local days, 30d today plus 29, and closed
  // ranges end 1ms before the next boundary so the aggregate API's inclusive end
  // hour never picks up the next period. Only "1h" is a rolling window.
  function getTimeRange(range) {
    var now = new Date();
    if (range === "1h") {
      return { from: new Date(now.getTime() - 3600 * 1000).toISOString(), to: now.toISOString() };
    }
    var window = rangeWindow(range === "today" || range === "yesterday" || range === "7d" || range === "30d" ? range : "7d", now);
    return { from: window.from.toISOString(), to: window.to.toISOString() };
  }

  function usageGroupBy(rangeName, range) {
    if (rangeName === "7d" || rangeName === "30d") return "day";
    if (rangeName === "custom") {
      var from = Date.parse(range.from);
      var to = Date.parse(range.to);
      return to - from <= 48 * 3600 * 1000 ? "hour" : "day";
    }
    return "hour";
  }
  return {getTimeRange: getTimeRange, usageGroupBy: usageGroupBy};
});
