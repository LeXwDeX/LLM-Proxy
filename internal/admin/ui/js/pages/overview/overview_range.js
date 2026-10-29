"use strict";
// Overview range selection helpers: range options, persisted choice, local-day windows and bucket rollup.
AdminApp.register("overviewRange", function (app) {
  var S = app.bind("overviewState", "store")();
  var amount = app.bind("overviewUtil", "amount");
  var counter = app.bind("overviewUtil", "counter");
  var inputTotal = app.bind("overviewUtil", "inputTotal");
  var shortDate = app.bind("overviewUtil", "shortDate");
  var shortTime = app.bind("overviewUtil", "shortTime");
  // Overview-only presentation keys; they are not Schema business enums.
  var rangeOptions = [
    {key: "today", label: "今日"},
    {key: "yesterday", label: "昨日"},
    {key: "7d", label: "近 7 天"},
    {key: "30d", label: "近 30 天"},
    {key: "last_month", label: "上个自然月"},
  ];
  var rangeStorageKey = "llmproxy.overview.range";
  S.selectedRange = readStoredRange();
  function rangeOption(key) {
    return rangeOptions.find(function (option) { return option.key === key; }) || null;
  }
  // localStorage may be missing or throw (private mode, blocked site data).
  function rangeStorage() {
    try { return typeof localStorage !== "undefined" && localStorage ? localStorage : null; } catch (error) { return null; }
  }
  function readStoredRange() {
    try {
      var storage = rangeStorage();
      var saved = storage ? storage.getItem(rangeStorageKey) : null;
      return rangeOption(saved) ? saved : "today";
    } catch (error) { return "today"; }
  }
  function storeRange(key) {
    try {
      var storage = rangeStorage();
      if (storage) storage.setItem(rangeStorageKey, key);
    } catch (error) { /* Remembering the range is a convenience only. */ }
  }
  // rangeWindow returns browser-local boundaries. The aggregate API truncates
  // from/to to whole UTC hours and includes the end hour, so closed ranges send
  // `to = end - 1ms` and never pick up the next period's first hour.
  function rangeWindow(key, now) {
    var option = rangeOption(key);
    if (!option) return null;
    var current = new Date(now instanceof Date ? now.getTime() : Number(now));
    var year = current.getFullYear(), month = current.getMonth(), day = current.getDate();
    var from, end = null, granularity = "hour";
    if (key === "today") from = new Date(year, month, day);
    else if (key === "yesterday") { from = new Date(year, month, day - 1); end = new Date(year, month, day); }
    else if (key === "7d") { from = new Date(year, month, day - 6); granularity = "day"; }
    else if (key === "30d") { from = new Date(year, month, day - 29); granularity = "day"; }
    else { from = new Date(year, month - 1, 1); end = new Date(year, month, 1); granularity = "day"; }
    return {key: key, label: option.label, from: from, to: end ? new Date(end.getTime() - 1) : current, end: end || current, closed: !!end, granularity: granularity};
  }
  function aggregatePath(window) {
    return "/admin/data/usage/aggregate?group_by=hour&from=" + encodeURIComponent(window.from.toISOString()) + "&to=" + encodeURIComponent(window.to.toISOString());
  }
  function emptySlot(start, end) {
    return {start: start, end: end, requests: 0, input: 0, output: 0, cost: 0, incompleteUsage: 0, incompleteCost: 0, buckets: 0, empty: true, missing: false};
  }
  // rollupBuckets turns hourly aggregate buckets into display slots: whole UTC
  // hours for single-day ranges, browser-local days for multi-day ranges.
  // Empty slots are created only inside [window.from, window.to], so future
  // hours of today are never drawn as zero. A bucket that starts before the
  // first slot (UTC truncation in half-hour zones) is kept in the first slot.
  function rollupBuckets(buckets, window) {
    var slots = [];
    var fromMs = window.from.getTime(), toMs = window.to.getTime();
    if (window.granularity === "day") {
      for (var i = 0; i < 400; i++) {
        var start = new Date(window.from.getFullYear(), window.from.getMonth(), window.from.getDate() + i);
        if (start.getTime() > toMs) break;
        slots.push(emptySlot(start, new Date(start.getFullYear(), start.getMonth(), start.getDate() + 1)));
      }
    } else {
      var hour = 3600000;
      for (var at = Math.floor(fromMs / hour) * hour; at <= toMs; at += hour) slots.push(emptySlot(new Date(at), new Date(at + hour)));
    }
    if (!slots.length) return slots;
    (Array.isArray(buckets) ? buckets : []).forEach(function (bucket) {
      var started = Date.parse(bucket && bucket.bucket_start);
      var index = 0;
      if (isFinite(started)) {
        var moment = Math.max(started, slots[0].start.getTime());
        for (var j = slots.length - 1; j >= 0; j--) {
          if (slots[j].start.getTime() <= moment) { index = j; break; }
        }
      }
      var slot = slots[index];
      bucket = bucket || {};
      slot.empty = false;
      slot.buckets++;
      if (!isFinite(started) || !["requests", "input_tokens", "output_tokens", "estimated_cost"].every(function (key) { return counter(bucket[key]); })) slot.missing = true;
      slot.requests += amount(bucket.requests);
      slot.input += inputTotal(bucket);
      slot.output += amount(bucket.output_tokens);
      slot.cost += amount(bucket.estimated_cost);
      slot.incompleteUsage += amount(bucket.incomplete_usage_count);
      slot.incompleteCost += amount(bucket.incomplete_cost_count);
    });
    return slots;
  }
  function windowSpan(window) {
    var year = new Date(Date.now()).getFullYear();
    var withYear = window.from.getFullYear() !== year || window.end.getFullYear() !== year;
    function stamp(date) { return (withYear ? date.getFullYear() + "-" : "") + shortDate(date) + " " + shortTime(date); }
    return stamp(window.from) + " – " + stamp(window.end);
  }
  return {aggregatePath: aggregatePath, rangeOption: rangeOption, rangeWindow: rangeWindow, rollupBuckets: rollupBuckets, storeRange: storeRange, windowSpan: windowSpan};
});
