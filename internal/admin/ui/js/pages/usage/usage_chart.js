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
        ? "按查询时的时间范围和筛选 · 按 UTC 小时聚合；开始与结束时间所在整点小时均整桶计入（含边界小时），因此边界桶可能包含范围外记录。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。横轴小时按浏览器本地时间标注，标题保留对应 UTC 小时。"
        : "按查询时的时间范围和筛选 · 所选范围内的小时用量按 UTC 自然日分组；开始与结束边界所在整点小时整桶计入，不扩展到完整起止 UTC 日。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。横轴按 UTC 日标注（标 UTC）。";
  }
  function pad2(value) { return String(value).padStart(2, "0"); }
  // Hour buckets are labelled in browser-local time (the title keeps the UTC
  // hour); day buckets are UTC days and the label says so.
  function usageChartLabel(start, groupBy) {
    var text = String(start || "");
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2})/.exec(text);
    if (!match) return text || "—";
    if (groupBy !== "hour") return match[2] + "-" + match[3] + " (UTC)";
    var date = new Date(Date.parse(text));
    if (isNaN(date.getTime())) return match[2] + "-" + match[3] + " " + match[4] + ":00";
    return pad2(date.getMonth() + 1) + "-" + pad2(date.getDate()) + " " + pad2(date.getHours()) + ":" + pad2(date.getMinutes());
  }
  function usageChartTitleTime(start, groupBy) {
    var text = String(start || "");
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(text);
    if (!match) return text || "—";
    var utc = groupBy === "hour"
      ? "UTC " + match[2] + "-" + match[3] + " " + match[4] + ":" + match[5]
      : "UTC " + match[2] + "-" + match[3];
    return groupBy === "hour" ? "本地 " + usageChartLabel(start, groupBy) + "（" + utc + "）" : utc + " 日";
  }
  // zeroFillBuckets adds an empty bucket for every hour (or UTC day) of the
  // queried range that has no row, so gaps are drawn as real zeros.
  function zeroFillBuckets(buckets, groupBy, range) {
    var step = groupBy === "hour" ? 3600000 : 86400000;
    var from = range && Date.parse(range.from), to = range && Date.parse(range.to);
    var known = Object.create(null);
    var starts = [];
    buckets.forEach(function (bucket) {
      var at = Date.parse(bucket.bucket_start);
      if (isFinite(at)) { known[Math.floor(at / step) * step] = true; starts.push(at); }
    });
    if (!isFinite(from) || !isFinite(to) || to < from) return buckets;
    var first = Math.floor(from / step) * step, last = Math.floor(to / step) * step;
    if ((last - first) / step > 800) return buckets;
    var filled = buckets.slice();
    for (var at = first; at <= last; at += step) {
      if (!known[at]) filled.push({bucket_start: new Date(at).toISOString(), zeroFilled: true});
    }
    return filled;
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
    if (S.usageSnapshot) renderDailyChart(S.usageChartBuckets, S.usageChartGroupBy, S.usageChartRange);
  }
  function renderDailyChart(buckets, groupBy, range) {
    var chart = document.getElementById("dailyChart");
    buckets = Array.isArray(buckets) ? buckets : [];
    groupBy = groupBy || S.usageChartGroupBy || "day";
    S.usageChartBuckets = buckets;
    S.usageChartGroupBy = groupBy;
    S.usageChartRange = range || null;
    if (!buckets.length) {
      chart.innerHTML =
        "<div style='color:var(--text-muted);padding:32px;text-align:center;width:100%;'>当前时间段内暂无统计数据</div>";
      return;
    }
    var isCost = S.usageChartMetric === "cost";
    var rows = zeroFillBuckets(buckets, groupBy, range)
      .sort(function (a, b) {
        return String(a.bucket_start).localeCompare(String(b.bucket_start));
      })
      .map(function (b) {
        return {
          label: usageChartLabel(b.bucket_start, groupBy),
          time: usageChartTitleTime(b.bucket_start, groupBy),
          requests: b.zeroFilled ? 0 : b.requests,
          value: b.zeroFilled ? 0 : usageChartValue(b, S.usageChartMetric),
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
          r.time +
          "\n" +
          metricTitle +
          "\n请求权重: " +
          usageCell(r.requests);
        var aria =
          r.time +
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
