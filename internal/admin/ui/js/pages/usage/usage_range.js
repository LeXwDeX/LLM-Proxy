"use strict";
// Usage time range helpers: preset windows and aggregate bucket granularity.
AdminApp.register("usage_range", function (app) {
  function getTimeRange(range) {
    var now = new Date();
    var to = now;
    var from;
    switch (range) {
      case "1h":
        from = new Date(now.getTime() - 3600 * 1000);
        break;
      case "today":
        from = new Date(now.getFullYear(), now.getMonth(), now.getDate());
        break;
      case "yesterday":
        from = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
        to = new Date(now.getFullYear(), now.getMonth(), now.getDate());
        break;
      case "7d":
        from = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
        from.setHours(0, 0, 0, 0);
        break;
      case "30d":
        from = new Date(now.getTime() - 30 * 24 * 3600 * 1000);
        from.setHours(0, 0, 0, 0);
        break;
      default:
        from = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
    }
    return { from: from.toISOString(), to: to.toISOString() };
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
