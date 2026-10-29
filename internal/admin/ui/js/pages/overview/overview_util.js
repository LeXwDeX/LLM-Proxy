"use strict";
// Overview helpers: DOM shortcuts, value predicates, target classification and chart geometry. No page state.
AdminApp.register("overviewUtil", function (app) {
  var state = app.state;
  var fmtNum = app.bind("core", "fmtNum");
  var totalTok = app.bind("core", "totalTok");
  var schemaLabel = null;
  try { schemaLabel = app.bind("schema", "schemaLabel"); } catch (error) { schemaLabel = null; }
  function el(id) { return document.getElementById(id); }
  function text(id, value) { el(id).textContent = value; }
  function visible() { return state.currentPage === "overview" && !document.hidden; }
  function clock(value) {
    var date = new Date(value);
    return isNaN(date.getTime()) ? String(value || "—") : date.toLocaleTimeString("zh-CN", {hour12: false});
  }
  function number(value) { return fmtNum(value); }
  function isObject(value) { return !!value && typeof value === "object" && !Array.isArray(value); }
  function hasFields(value) { return isObject(value) && Object.keys(value).length > 0; }
  function counter(value) { return typeof value === "number" && isFinite(value) && value >= 0; }
  function localDate(value) {
    var date = new Date(value);
    if (isNaN(date.getTime()) || date.getUTCFullYear() < 2000) return "—";
    function pad(value) { return String(value).padStart(2, "0"); }
    return date.getFullYear() + "-" + pad(date.getMonth() + 1) + "-" + pad(date.getDate()) + " " + pad(date.getHours()) + ":" + pad(date.getMinutes()) + ":" + pad(date.getSeconds());
  }
  function duration(seconds) {
    if (!counter(seconds)) return "不可用";
    var total = Math.floor(seconds);
    var hours = Math.floor(total / 3600);
    var minutes = Math.floor((total % 3600) / 60);
    if (hours > 0) return hours + " 小时 " + minutes + " 分钟";
    if (minutes > 0) return minutes + " 分钟";
    return total + " 秒";
  }
  function targetPhase(target) {
    if (target && target.Paused === true) return "offline";
    var phase = String(target && target.Phase || "").toLowerCase();
    if (phase === "ready" || phase === "muted" || phase === "half-open" || phase === "offline") return phase;
    if (target && target.Muted === true) return "muted";
    return "unknown";
  }
  function phaseLabel(target) {
    var phase = targetPhase(target);
    if (phase === "unknown") return "未知";
    if (schemaLabel) {
      try {
        var label = schemaLabel("target_phases", phase);
        if (label) return label;
      } catch (error) { /* Schema may be unavailable; a raw phase is still honest. */ }
    }
    return phase;
  }
  function keyIssue(target) {
    if (!counter(target && target.TotalKeys)) return "unknown";
    if (target.TotalKeys <= 1) return "false";
    if (counter(target.ActiveKeys) && target.ActiveKeys < target.TotalKeys) return "true";
    var failures = ["RateLimitedKeys", "BlockedKeys", "ExhaustedSubscriptionKeys", "ExhaustedAPIKeys", "OtherExhaustedKeys"];
    if (failures.some(function (key) { return counter(target[key]) && target[key] > 0; })) return "true";
    var complete = ["ActiveKeys"].concat(failures).every(function (key) { return counter(target[key]); });
    return complete ? "false" : "unknown";
  }
  function keySummary(target) {
    if (!counter(target.TotalKeys) || keyIssue(target) === "unknown") return "密钥状态不可用";
    if (target.TotalKeys === 0) return "未配置独立密钥，可能使用 Bearer/透传";
    if (target.TotalKeys === 1) return "目标级状态；无独立密钥池明细";
    var parts = ["可用 " + (counter(target.ActiveKeys) ? fmtNum(target.ActiveKeys) : "不可用") + " / " + fmtNum(target.TotalKeys)];
    if (counter(target.RateLimitedKeys) && target.RateLimitedKeys > 0) parts.push("限流 " + fmtNum(target.RateLimitedKeys));
    if (counter(target.BlockedKeys) && target.BlockedKeys > 0) parts.push("屏蔽 " + fmtNum(target.BlockedKeys));
    if (counter(target.ExhaustedSubscriptionKeys) && target.ExhaustedSubscriptionKeys > 0) parts.push("订阅额度耗尽 " + fmtNum(target.ExhaustedSubscriptionKeys));
    if (counter(target.ExhaustedAPIKeys) && target.ExhaustedAPIKeys > 0) parts.push("API 额度耗尽 " + fmtNum(target.ExhaustedAPIKeys));
    if (counter(target.OtherExhaustedKeys) && target.OtherExhaustedKeys > 0) parts.push("其他失效 " + fmtNum(target.OtherExhaustedKeys));
    return parts.join(" · ");
  }
  function targetRank(target) {
    var phase = targetPhase(target);
    if (phase === "muted") return 0;
    if (phase === "half-open") return 1;
    if (phase === "ready") {
      var issue = keyIssue(target);
      return issue === "true" ? 3 : issue === "unknown" ? 4 : 5;
    }
    // Paused (offline) targets are intentional, so they sort after everything else and are not "attention".
    if (phase === "offline") return 7;
    return 6;
  }
  function amount(value) { return counter(value) ? value : 0; }
  // Input shown on the overview includes cache read/write and tool input, so
  // input + output equals the total-token convention used by totalTok.
  function inputTotal(data) {
    return amount(data.input_tokens) + amount(data.cached_tokens) + amount(data.cache_creation_tokens) + amount(data.tool_use_prompt_tokens);
  }
  function compareTime(from, to) {
    var milliseconds = Date.parse(from) - Date.parse(to);
    if (milliseconds !== 0) return milliseconds;
    // Go emits RFC3339Nano; Date.parse truncates the remaining six digits.
    function submillisecond(value) {
      var fraction = /\.(\d+)(?:Z|[+-]\d{2}:\d{2})$/.exec(value);
      return fraction ? fraction[1].padEnd(9, "0").slice(3, 9) : "000000";
    }
    var first = submillisecond(from), second = submillisecond(to);
    return first < second ? -1 : first > second ? 1 : 0;
  }
  function pad(value) { return String(value).padStart(2, "0"); }
  function shortDate(date) { return pad(date.getMonth() + 1) + "-" + pad(date.getDate()); }
  function shortTime(date) { return pad(date.getHours()) + ":" + pad(date.getMinutes()); }
  function offsetMinutes(date) { return -date.getTimezoneOffset(); }
  function offsetLabel(date) {
    var minutes = offsetMinutes(date);
    var sign = minutes < 0 ? "-" : "+";
    minutes = Math.abs(minutes);
    return "UTC" + sign + pad(Math.floor(minutes / 60)) + ":" + pad(minutes % 60);
  }
  function chartAxis(value) {
    if (value <= 0) return {maximum: 4, step: 1, ticks: 4};
    var desiredStep = Math.max(1, value * 1.1 / 4);
    var scale = Math.pow(10, Math.floor(Math.log10(desiredStep)));
    var step = Math.ceil([1, 2, 2.5, 3, 5, 10].find(function (factor) { return factor * scale >= desiredStep; }) * scale);
    var ticks = Math.ceil(value * 1.1 / step);
    return {maximum: ticks * step, step: step, ticks: ticks};
  }
  function chartGeometry(chart) {
    var bounds = chart.getBoundingClientRect();
    var width = Math.max(160, bounds.width || 560);
    var height = Math.max(150, bounds.height || 230);
    return {width: width, height: height, left: 44, right: width - 12, top: 16, bottom: height - 42};
  }
  function compactNumber(value) {
    if (value >= 1000000000) return Number((value / 1000000000).toPrecision(3)) + "G";
    if (value >= 1000000) return Number((value / 1000000).toPrecision(3)) + "M";
    if (value >= 1000) return Number((value / 1000).toPrecision(3)) + "k";
    return number(value);
  }
  // Every overview chart draws bars of the same proportions; token charts stack
  // input (bottom) and output (top) so the bar height is the bucket total.
  function barWidth(band) { return Math.max(1.5, Math.min(28, band * 0.62)); }
  function barRect(className, left, width, base, value, y) {
    if (!isFinite(value) || value <= 0) return "";
    return '<rect class="' + className + '" x="' + left + '" y="' + y(base + value) + '" width="' + width + '" height="' + (y(base) - y(base + value)) + '" rx="2"/>';
  }
  function stackedBars(left, width, values, y) {
    var base = 0;
    return values.map(function (value, index) {
      var rect = barRect("activity-bar " + (index ? "activity-output" : "activity-input"), left, width, base, value, y);
      if (value > 0) base += value;
      return rect;
    }).join("");
  }
  return {amount: amount, barRect: barRect, barWidth: barWidth, chartAxis: chartAxis, chartGeometry: chartGeometry, clock: clock, compactNumber: compactNumber, compareTime: compareTime, counter: counter, duration: duration, el: el, inputTotal: inputTotal, isObject: isObject, keyIssue: keyIssue, keySummary: keySummary, localDate: localDate, number: number, offsetLabel: offsetLabel, offsetMinutes: offsetMinutes, phaseLabel: phaseLabel, shortDate: shortDate, shortTime: shortTime, stackedBars: stackedBars, targetPhase: targetPhase, targetRank: targetRank, text: text, visible: visible};
});
