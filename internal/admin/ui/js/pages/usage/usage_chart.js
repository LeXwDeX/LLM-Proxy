"use strict";
// Usage chart: metric toggle, bucket labels and the bar chart.
AdminApp.register("usageChart", function (app) {
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var S = app.bind("usageState", "store")();
  var usageCell = app.bind("usageTables", "usageCell");
  var usageNumber = app.bind("usageTables", "usageNumber");
  var usageTokensAvailable = app.bind("usageTables", "usageTokensAvailable");
  function pad2(value) { return String(value).padStart(2, "0"); }
  var localDayFillMax = 800;
  function localDayStart(ms) {
    var at = new Date(ms);
    return new Date(at.getFullYear(), at.getMonth(), at.getDate()).getTime();
  }
  function localDaySpan(fromMs, toMs) {
    if (!isFinite(fromMs) || !isFinite(toMs) || toMs < fromMs) return 0;
    var from = new Date(fromMs), to = new Date(toMs);
    var fromDay = Date.UTC(from.getFullYear(), from.getMonth(), from.getDate());
    var toDay = Date.UTC(to.getFullYear(), to.getMonth(), to.getDate());
    return Math.round((toDay - fromDay) / 86400000) + 1;
  }
  function localHourStamp(ms) {
    var at = new Date(ms);
    return (
      pad2(at.getMonth() + 1) + "-" + pad2(at.getDate()) + " " +
      pad2(at.getHours()) + ":" + pad2(at.getMinutes())
    );
  }
  // hourCoverage spells out what the API really covers for a rolling window:
  // the whole UTC hour of each boundary is included.
  function hourCoverage(range) {
    var from = range && Date.parse(range.from), to = range && Date.parse(range.to);
    if (!isFinite(from) || !isFinite(to) || to < from) return "";
    var first = Math.floor(from / 3600000) * 3600000;
    var last = Math.floor(to / 3600000) * 3600000 + 3600000;
    var count = Math.round((last - first) / 3600000);
    return "本次实际覆盖本地 " + localHourStamp(first) + " 至 " + localHourStamp(last) +
      "（" + count + " 个整点小时）";
  }
  function setUsageChartNote(groupBy, rangeName, range) {
    var note = document.getElementById("usageChartNote");
    if (!note) return;
    if (groupBy === "hour" && rangeName === "1h") {
      var coverage = hourCoverage(range);
      note.textContent =
        "「近1小时」为滚动查询窗口；服务端按整点小时桶统计" +
        (coverage ? "，" + coverage : "") +
        "，边界桶可能包含所选 1 小时窗口外的记录。横轴小时按浏览器本地时间标注，标题保留对应 UTC 小时。";
      return;
    }
    if (groupBy !== "hour" && localDaySpan(range && Date.parse(range.from), range && Date.parse(range.to)) > localDayFillMax) {
      note.textContent =
        "按查询时的时间范围和筛选 · 服务端返回 UTC 整点小时桶，图表按浏览器本地日重新汇总；区间超过 " +
        localDayFillMax +
        " 个本地日，仅绘制有数据的本地日，不补齐空白日。今日/昨日/近 7/30 天范围按浏览器本地时间选取，横轴按本地日标注。";
      return;
    }
    note.textContent =
      groupBy === "hour"
        ? "按查询时的时间范围和筛选 · 按 UTC 小时聚合；开始与结束时间所在整点小时均整桶计入（含边界小时），因此边界桶可能包含范围外记录。今日/昨日/近 7/30 天范围按浏览器本地时间选取，桶边界按 UTC。横轴小时按浏览器本地时间标注，标题保留对应 UTC 小时。"
        : "按查询时的时间范围和筛选 · 服务端返回 UTC 整点小时桶，图表按浏览器本地日重新汇总；今日/昨日/近 7/30 天范围按浏览器本地时间选取，柱状图为本地自然日。横轴按本地日标注。";
  }
  // Hour buckets are labelled in browser-local time (the title keeps the UTC
  // hour); day buckets are already browser-local days.
  function usageChartLabel(start, groupBy) {
    var text = String(start || "");
    var date = new Date(Date.parse(text));
    if (groupBy !== "hour") {
      if (isNaN(date.getTime())) return text || "—";
      return pad2(date.getMonth() + 1) + "-" + pad2(date.getDate());
    }
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2})/.exec(text);
    if (!match) return text || "—";
    if (isNaN(date.getTime())) return match[2] + "-" + match[3] + " " + match[4] + ":00";
    return pad2(date.getMonth() + 1) + "-" + pad2(date.getDate()) + " " + pad2(date.getHours()) + ":" + pad2(date.getMinutes());
  }
  function usageChartTitleTime(start, groupBy) {
    var text = String(start || "");
    var date = new Date(Date.parse(text));
    if (groupBy !== "hour") {
      if (isNaN(date.getTime())) return text || "—";
      return "本地 " + date.getFullYear() + "-" + pad2(date.getMonth() + 1) + "-" + pad2(date.getDate()) + " 日";
    }
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(text);
    if (!match) return text || "—";
    var utc = "UTC " + match[2] + "-" + match[3] + " " + match[4] + ":" + match[5];
    return "本地 " + usageChartLabel(start, groupBy) + "（" + utc + "）";
  }
  // zeroFillBuckets adds an empty bucket for every UTC hour of the queried
  // range that has no row, so gaps are drawn as real zeros.
  function zeroFillBuckets(buckets, range) {
    var step = 3600000;
    var from = range && Date.parse(range.from), to = range && Date.parse(range.to);
    var known = Object.create(null);
    buckets.forEach(function (bucket) {
      var at = Date.parse(bucket.bucket_start);
      if (isFinite(at)) known[Math.floor(at / step) * step] = true;
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
  var chartSumFields = [
    "requests",
    "input_tokens",
    "output_tokens",
    "cached_tokens",
    "cache_creation_tokens",
    "explicit_cache_read_tokens",
    "reasoning_tokens",
    "cache_creation_5m_tokens",
    "cache_creation_1h_tokens",
    "tool_use_prompt_tokens",
    "image_input_tokens",
    "cached_image_input_tokens",
    "image_output_tokens",
    "incomplete_usage_count",
    "incomplete_cost_count",
  ];
  function addRolledBucket(target, source) {
    chartSumFields.forEach(function (key) {
      if (source[key] === undefined || source[key] === null) return;
      var value = usageNumber(source[key]);
      if (value === null) return;
      target[key] = (usageNumber(target[key]) || 0) + value;
    });
    var cost = usageNumber(source.estimated_cost);
    if (cost === null) {
      target.costMissing = true;
    } else {
      target.estimated_cost = (usageNumber(target.estimated_cost) || 0) + cost;
    }
    if (target.costMissing) target.estimated_cost = null;
  }
  // localDaySlots lists every browser-local calendar day touched by the range,
  // so a 7-day range always yields seven slots regardless of the UTC offset.
  function localDaySlots(fromMs, toMs) {
    var slots = [];
    var cursor = new Date(fromMs);
    for (var i = 0; i < localDayFillMax; i++) {
      var start = new Date(cursor.getFullYear(), cursor.getMonth(), cursor.getDate());
      if (start.getTime() > toMs) break;
      slots.push({start: start, at: start.getTime(), totals: {}});
      cursor = new Date(cursor.getFullYear(), cursor.getMonth(), cursor.getDate() + 1);
    }
    return slots;
  }
  // rollupLocalDays folds UTC hour buckets into browser-local days. Buckets that
  // start before the first slot (UTC truncation in half-hour zones) stay in the
  // first slot instead of creating an extra bar outside the queried range. Ranges
  // longer than localDayFillMax skip the zero-filled calendar and keep one slot
  // per day that actually has buckets, so late usage never folds into day 800.
  function rollupLocalDays(buckets, range) {
    var from = range && Date.parse(range.from), to = range && Date.parse(range.to);
    if (!isFinite(from) || !isFinite(to) || to < from) {
      var times = [];
      buckets.forEach(function (bucket) {
        var at = Date.parse(bucket && bucket.bucket_start);
        if (isFinite(at)) times.push(at);
      });
      if (!times.length) return [];
      from = Math.min.apply(null, times);
      to = Math.max.apply(null, times);
    }
    var fill = localDaySpan(from, to) <= localDayFillMax ? localDaySlots(from, to) : [];
    var byDay = Object.create(null);
    var slots = [];
    fill.forEach(function (slot) {
      byDay[slot.at] = slot;
      slots.push(slot);
    });
    var firstDay = localDayStart(from);
    function slotFor(moment) {
      var dayAt = localDayStart(isFinite(moment) ? Math.max(moment, firstDay) : firstDay);
      var slot = byDay[dayAt];
      if (!slot) {
        slot = {start: new Date(dayAt), at: dayAt, totals: {}};
        byDay[dayAt] = slot;
        slots.push(slot);
      }
      return slot;
    }
    buckets.forEach(function (bucket) {
      var slot = slotFor(Date.parse(bucket && bucket.bucket_start));
      slot.seen = true;
      addRolledBucket(slot.totals, bucket || {});
    });
    return slots
      .sort(function (a, b) {
        return a.at - b.at;
      })
      .map(function (slot) {
        var out = {bucket_start: slot.start.toISOString()};
        Object.keys(slot.totals).forEach(function (key) {
          if (key !== "costMissing") out[key] = slot.totals[key];
        });
        if (!slot.seen) out.zeroFilled = true;
        return out;
      });
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
    if (S.usageSnapshot) {
      renderDailyChart(S.usageChartBuckets, S.usageChartGroupBy, S.usageChartRange, S.usageChartSourceGroupBy);
    }
  }
  function renderDailyChart(buckets, groupBy, range, bucketGroupBy) {
    var chart = document.getElementById("dailyChart");
    buckets = Array.isArray(buckets) ? buckets : [];
    groupBy = groupBy || S.usageChartGroupBy || "day";
    var sourceGroupBy = bucketGroupBy || S.usageChartSourceGroupBy || (groupBy === "day" ? "hour" : groupBy);
    S.usageChartBuckets = buckets;
    S.usageChartGroupBy = groupBy;
    S.usageChartSourceGroupBy = sourceGroupBy;
    S.usageChartRange = range || null;
    if (!buckets.length) {
      chart.innerHTML =
        "<div style='color:var(--text-muted);padding:32px;text-align:center;width:100%;'>当前时间段内暂无统计数据</div>";
      return;
    }
    var isCost = S.usageChartMetric === "cost";
    var drawn = groupBy === "day" && sourceGroupBy === "hour"
      ? rollupLocalDays(buckets, range)
      : groupBy === "hour"
        ? zeroFillBuckets(buckets, range)
        : buckets;
    var rows = drawn
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
