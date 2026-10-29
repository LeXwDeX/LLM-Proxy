"use strict";
// Usage chart: metric toggle, bucket labels and the bar chart.
AdminApp.register("usage_chart", function (app) {
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var S = app.bind("usage_state", "store")();
  var usageCell = app.bind("usage_tables", "usageCell");
  var usageNumber = app.bind("usage_tables", "usageNumber");
  var usageTokensAvailable = app.bind("usage_tables", "usageTokensAvailable");
  function setUsageChartNote(groupBy) {
    var note = document.getElementById("usageChartNote");
    if (!note) return;
    note.textContent =
      groupBy === "hour"
        ? "按查询时的时间范围和筛选 · 按 UTC 小时聚合；开始与结束时间所在整点小时均整桶计入（含边界小时），因此边界桶可能包含范围外记录。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。"
        : "按查询时的时间范围和筛选 · 所选范围内的小时用量按 UTC 自然日分组；开始与结束边界所在整点小时整桶计入，不扩展到完整起止 UTC 日。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。";
  }
  function usageChartLabel(start, groupBy) {
    var text = String(start || "");
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2})/.exec(text);
    if (!match) return text || "—";
    return groupBy === "hour"
      ? match[2] + "-" + match[3] + " " + match[4] + ":00"
      : match[2] + "-" + match[3];
  }
  function usageChartValue(bucket, metric) {
    if (metric === "cost") return usageNumber(bucket.estimated_cost);
    return usageTokensAvailable(bucket) ? totalTok(bucket) : null;
  }
  function setUsageChartMetric(metric) {
    S.usageChartMetric = metric === "cost" ? "cost" : "tokens";
    [
      ["usageChartMetricTokens", "tokens"],
      ["usageChartMetricCost", "cost"],
    ].forEach(function (item) {
      var button = document.getElementById(item[0]);
      if (!button) return;
      var active = item[1] === S.usageChartMetric;
      button.setAttribute("aria-pressed", String(active));
      button.classList.toggle("active", active);
    });
    if (S.usageSnapshot) renderDailyChart(S.usageChartBuckets, S.usageChartGroupBy);
  }
  function renderDailyChart(buckets, groupBy) {
    var chart = document.getElementById("dailyChart");
    buckets = Array.isArray(buckets) ? buckets : [];
    groupBy = groupBy || S.usageChartGroupBy || "day";
    S.usageChartBuckets = buckets;
    S.usageChartGroupBy = groupBy;
    if (!buckets.length) {
      chart.innerHTML =
        "<div style='color:var(--text-muted);padding:32px;text-align:center;width:100%;'>当前时间段内暂无统计数据</div>";
      return;
    }
    var isCost = S.usageChartMetric === "cost";
    var rows = buckets
      .slice()
      .sort(function (a, b) {
        return String(a.bucket_start).localeCompare(String(b.bucket_start));
      })
      .map(function (b) {
        return {
          label: usageChartLabel(b.bucket_start, groupBy),
          requests: b.requests,
          value: usageChartValue(b, S.usageChartMetric),
        };
      });
    var maxValue =
      rows.reduce(function (m, r) {
        return r.value === null ? m : Math.max(m, r.value);
      }, 0) || 1;
    chart.innerHTML = rows
      .map(function (r) {
        var missing = r.value === null;
        var h = missing
          ? 0
          : Math.max(4, Math.round((r.value / maxValue) * 180));
        var valueText = missing
          ? "不可用"
          : isCost
            ? fmtMoney(r.value)
            : fmtNum(r.value);
        var metricTitle = isCost
          ? "费用: " + valueText
          : "总 Token: " + valueText;
        var title =
          "UTC " +
          r.label +
          "\n" +
          metricTitle +
          "\n请求权重: " +
          usageCell(r.requests);
        var aria =
          "UTC " +
          r.label +
          "，" +
          (isCost ? "费用 " : "总 Token ") +
          valueText +
          "，请求权重 " +
          usageCell(r.requests);
        return (
          "<div class='chart-bar-group' tabindex='0' role='img' title='" +
          esc(title) +
          "' aria-label='" +
          esc(aria) +
          "'>" +
          "<div class='chart-value'>" +
          esc(valueText) +
          "</div>" +
          "<div class='chart-bar" +
          (missing ? " usage-chart-missing" : "") +
          "' style='height:" +
          h +
          "px'></div>" +
          "<div class='chart-label'>" +
          esc(r.label) +
          "</div>" +
          "</div>"
        );
      })
      .join("");
  }

  document
    .getElementById("usageChartMetricTokens")
    .addEventListener("click", function () {
      setUsageChartMetric("tokens");
    });
  document
    .getElementById("usageChartMetricCost")
    .addEventListener("click", function () {
      setUsageChartMetric("cost");
    });
  return {renderDailyChart: renderDailyChart, setUsageChartMetric: setUsageChartMetric, setUsageChartNote: setUsageChartNote};
});
