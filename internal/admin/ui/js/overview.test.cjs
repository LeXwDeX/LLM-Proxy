"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
class Element {
  constructor(tag = "div") { this.tagName = tag.toUpperCase(); this.textContent = ""; this.innerHTML = ""; this.children = []; this.listeners = {}; this.attrs = {}; this.dataset = {}; this.hidden = false; this.value = ""; this.style = {}; }
  addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
  fire(name, event = {}) { return Promise.all((this.listeners[name] || []).map(fn => fn({target: this, preventDefault() {}, ...event}))); }
  setAttribute(key, value) { this.attrs[key] = String(value); }
  getAttribute(key) { return this.attrs[key]; }
  appendChild(node) { return this.insertBefore(node, null); }
  insertBefore(node, next) { if (node.parentNode) node.parentNode.removeChild(node); const i = this.children.indexOf(next); this.children.splice(i < 0 ? this.children.length : i, 0, node); node.parentNode = this; return node; }
  removeChild(node) { this.children.splice(this.children.indexOf(node), 1); node.parentNode = null; }
  remove() { this.parentNode?.removeChild(this); }
  focus() { this.focused = true; }
  closest() { return this.parentNode || null; }
  querySelector() { return null; }
  contains(node) { return node === this || this.children.some(child => child.contains(node)); }
  getBoundingClientRect() { return {left: 0, top: 0, width: 560, height: 230}; }
}
function activity(overrides = {}) {
  const series = [0, 1, 2].map(i => ({from: `2026-10-14T03:00:${i}0Z`, to: `2026-10-14T03:00:${i + 1}0Z`, partial: i === 0, active_requests_peak: i ? i + 2 : null, input_tokens: 10 * i, output_tokens: 4 * i, total_tokens: 14 * i, incomplete_usage_count: 0}));
  return {generated_at: "2026-10-14T03:00:30Z", window: {from: series[0].from, to: series[2].to, step_seconds: 10}, runtime: {since: series[0].from, complete: false, active_requests: 2, active_clients: 1, websocket_sessions: 3}, totals: {input_tokens: 30, output_tokens: 12, total_tokens: 42, incomplete_usage_count: 0}, series, clients: [{name: '<A & "B">', configured: false, active_requests: 2, websocket_sessions: 3, totals: {input_tokens: 30, output_tokens: 12, total_tokens: 42, incomplete_usage_count: 0}, series: series.map(b => ({...b, active_requests_peak: b.active_requests_peak == null ? null : 1}))}], ...overrides};
}
function completeStats(overrides = {}) {
  return {
    available: true, total: 0, success: 0, failure: 0, canceled: 0, record_failures: 0, success_rate: null,
    since: "2026-10-20T00:00:00Z", from: "2026-10-20T00:00:00Z", to: "2026-10-23T00:00:00Z", ...overrides,
  };
}
function completeRange(overrides = {}) {
  return {requests: 0, input_tokens: 0, output_tokens: 0, estimated_cost: 0, incomplete_usage_count: 0, incomplete_cost_count: 0, ...overrides};
}
function completeMetrics(overrides = {}) {
  return {
    uptime_seconds: 1,
    requests: {total: 0},
    persistence: {usage_async_pending: 0, usage_async_deferred: 0, request_stats_dropped: 0},
    observation_budget: {observation_budget_exhausted: {br: 0, zstd: 0}},
    ...overrides,
  };
}
function aggTotals(overrides = {}) {
  return {requests: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, cache_creation_tokens: 0, estimated_cost: 0, ...overrides};
}
// buckets is null on purpose: Go serializes an empty slice that way.
function aggregateResult(overrides = {}) {
  return {group_by: "hour", totals: aggTotals(), buckets: null, by_client: [], by_model: [], ...overrides};
}
function hourBucket(date, overrides = {}) {
  const start = new Date(date);
  return {bucket_start: start.toISOString(), bucket_end: new Date(start.getTime() + 3600000).toISOString(), ...aggTotals({requests: 1, input_tokens: 10, output_tokens: 2, estimated_cost: 0.5}), ...overrides};
}
function withTZ(zone, fn) {
  const previous = process.env.TZ;
  process.env.TZ = zone;
  const restore = () => { if (previous === undefined) delete process.env.TZ; else process.env.TZ = previous; };
  let result;
  try { result = fn(); } catch (error) { restore(); throw error; }
  if (result && typeof result.then === "function") return result.finally(restore);
  restore();
  return result;
}
const rangeKeys = ["today", "yesterday", "7d", "30d", "last_month"];
function barButton(key) {
  const button = new Element("button");
  button.dataset.overviewBar = key;
  button.attrs["aria-pressed"] = key === "requests" ? "true" : "false";
  return button;
}
function rangeButton(key) {
  const button = new Element("button");
  button.dataset.overviewRange = key;
  button.attrs["aria-pressed"] = "false";
  return button;
}
function harness({sortButtons = [], infoButtons = [], rangeButtons = [], barButtons = [], storage} = {}) {
  const elements = new Map(); const get = id => { if (!elements.has(id)) elements.set(id, new Element(id === "activityClientsBody" ? "tbody" : "div")); return elements.get(id); };
  const doc = {getElementById: get, querySelectorAll: selector => selector === "[data-activity-sort]" ? sortButtons : selector === "[data-activity-info]" ? infoButtons : selector === "[data-overview-range]" ? rangeButtons : selector === "[data-overview-bar]" ? barButtons : [], documentElement: {clientWidth: 390, clientHeight: 844}, createElement: tag => new Element(tag), listeners: {}, hidden: false, activeElement: null, addEventListener(name, fn) { this.listeners[name] = fn; }};
  let module; let answer = activity(); let overview = {}; let metrics = {}; let aggregate = aggregateResult(); let aggregateHandler; let handler; const calls = []; const timers = new Map(); let nextTimer = 0; let now = Date.now();
  class TestDate extends Date { static now() { return now; } }
  async function advance(ms) {
    for (let i = 0; i < 12; i++) await Promise.resolve();
    const end = now + ms;
    for (;;) {
      const next = [...timers].filter(([, timer]) => timer.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      now = next[1].at; timers.delete(next[0]); next[1].fn();
      for (let i = 0; i < 12; i++) await Promise.resolve();
    }
    now = end;
    for (let i = 0; i < 12; i++) await Promise.resolve();
  }
  const core = {esc: v => String(v).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;"), fmtNum: v => String(v || 0), fmtMoney: v => "$" + Number(v || 0).toFixed(2), fmtPct: v => v + "%", totalTok: d => (d.input_tokens || 0) + (d.output_tokens || 0) + (d.cached_tokens || 0) + (d.cache_creation_tokens || 0) + (d.tool_use_prompt_tokens || 0), schemaLabel: (list, value) => value, api: async (url, opts) => { calls.push({url, opts}); if (handler) return handler(url, opts); if (url.endsWith("activity")) return answer; if (url.endsWith("overview")) return overview; if (url.endsWith("/admin/metrics")) return metrics; if (url.includes("/admin/data/usage/aggregate")) return aggregateHandler ? aggregateHandler(url, opts) : aggregate; return overview; }};
  const state = {currentPage: "overview"};
  const context = {document: doc, AbortController, Date: TestDate, Map, setTimeout: (fn, delay) => {timers.set(++nextTimer, {fn, at: now + delay}); return nextTimer;}, clearTimeout: id => timers.delete(id), setInterval: () => 1, clearInterval() {}, AdminApp: {register: (name, factory) => {module = factory({state, bind: (m, n) => core[n]}); }}};
  if (storage !== undefined) context.localStorage = storage;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "overview.js"), "utf8"), context);
  return {module, get, doc, state, calls, timers, advance, setNow: value => now = new Date(value).getTime(), setActivity: a => answer = a, setOverview: o => overview = o, setMetrics: m => metrics = m, setAggregate: a => aggregate = a, setAggregateHandler: h => aggregateHandler = h, setHandler: h => handler = h};
}
test("homepage requests real activity, uses provided totals, and has no audit dependency", async () => {
  const h = harness(); await h.module.loadOverview();
  assert.ok(h.calls.some(c => c.url === "/admin/api/activity"));
  assert.ok(h.calls.every(c => !c.url.includes("audit")));
  assert.equal(h.get("activityActiveRequests").textContent, "2");
  assert.equal(h.get("activityTotalTokens").textContent, "42");
  assert.match(h.get("activityStatus").textContent, /采样/);
  assert.doesNotMatch(h.get("activityConcurrencyChart").innerHTML, /NaN|Infinity/);
});
test("polling merges initial and manual loads; stopping aborts and rejects a late response", async () => {
  const h = harness(); let resolve;
  h.setHandler(url => url.endsWith("activity") ? new Promise(r => resolve = r) : Promise.resolve({}));
  const load = h.module.loadOverview(); h.module.startOverviewAutoRefresh(); const duplicate = h.module.loadOverview();
  assert.equal(h.calls.filter(c => c.url.endsWith("activity")).length, 1);
  const signal = h.calls.find(c => c.url.endsWith("activity")).opts.signal;
  h.module.stopOverviewAutoRefresh(); assert.equal(signal.aborted, true);
  resolve(activity()); await Promise.all([load, duplicate]);
  assert.notEqual(h.get("activityActiveRequests").textContent, "2");
  assert.equal(h.timers.size, 0);
});
test("activity failures retain data and timestamp; first failure stays unavailable", async () => {
  const h = harness(); await h.module.loadOverview(); const time = h.get("activityUpdatedAt").textContent;
  h.setHandler(url => url.endsWith("activity") ? Promise.reject(Error("offline")) : Promise.resolve({}));
  await h.module.loadOverview();
  assert.equal(h.get("activityActiveRequests").textContent, "2"); assert.equal(h.get("activityUpdatedAt").textContent, time);
  assert.match(h.get("activityStatus").textContent, /更新失败.*上次/);
  const empty = harness(); empty.setHandler(() => Promise.reject(Error("offline"))); await empty.module.loadOverview();
  assert.equal(empty.get("activityActiveRequests").textContent, "不可用");
});
test("client selection changes both charts only; rows, search, and bucket focus survive refresh", async () => {
  const h = harness(); await h.module.loadOverview();
  const row = h.get("activityClientsBody").children[0]; const button = row.children[0].children[0];
  await button.fire("click");
  assert.match(h.get("activityChartScope").textContent, /A &/);
  assert.equal(h.get("activityActiveRequests").textContent, "2");
  await h.get("activityConcurrencyChart").fire("keydown", {key: "End"});
  assert.match(h.get("activityConcurrencyReadout").textContent, /1/);
  assert.match(h.get("activityTokenReadout").textContent, /20/);
  h.get("activityClientSearch").value = "A"; await h.get("activityClientSearch").fire("input");
  await h.module.loadOverview();
  assert.equal(h.get("activityClientsBody").children[0], row);
  assert.equal(h.get("activityClientSearch").value, "A");
  assert.match(h.get("activityTokenReadout").textContent, /20/);
  await h.get("activityResetScope").fire("click"); assert.equal(h.get("activityChartScope").textContent, "全站");
});
test("hidden pages abort polling and visible homepage resumes immediately", async () => {
  const h = harness(); h.module.startOverviewAutoRefresh(); await h.module.loadOverview(); const before = h.calls.length;
  h.doc.hidden = true; h.doc.listeners.visibilitychange(); assert.equal(h.timers.size, 0);
  h.doc.hidden = false; h.doc.listeners.visibilitychange(); await h.module.loadOverview(); assert.ok(h.calls.length > before);
  h.module.stopOverviewAutoRefresh();
});
test("72h history never falls back to usage events and preserves its warning contract", async () => {
  const h = harness(); h.setOverview({requests_72h: 900, success_rate: 100}); await h.module.loadOverview(true);
  assert.equal(h.get("ovSuccessRate").textContent, "不可用");
  h.setOverview({request_stats_72h: {available: true, total: 0, success_rate: null, since: "start", from: "from", to: "cutoff", record_failures: 0}}); await h.module.loadOverview(true);
  assert.equal(h.get("ovTotalReqs").textContent, "0"); assert.equal(h.get("ovSuccessRate").textContent, "--"); assert.match(h.get("ovRequestStatsStatus").textContent, /暂无完成样本/);
  h.setOverview({request_stats_72h: {available: true, total: 3, success_rate: 33.3, since: "start", from: "from", to: "cutoff", record_failures: 2}}); await h.module.loadOverview(true);
  assert.equal(h.get("ovSuccessRate").textContent, "33.3%"); assert.match(h.get("ovRequestStatsStatus").textContent, /2 次统计写入失败/);
  assert.match(h.get("activityHistoryTooltip").textContent, /start.*from.*cutoff/);
});
test("malformed samples retain the last valid snapshot; old samples only rewind after runtime reset", async () => {
  const h = harness(); await h.module.loadOverview(); const time = h.get("activityUpdatedAt").textContent;
  const broken = activity(); broken.clients[0].totals = null; h.setActivity(broken); await h.module.loadOverview();
  assert.equal(h.get("activityTotalTokens").textContent, "42"); assert.equal(h.get("activityUpdatedAt").textContent, time);
  assert.match(h.get("activityStatus").textContent, /更新失败/);
  const old = activity({generated_at: "2026-10-14T02:50:00Z", totals: {input_tokens: 10, output_tokens: 0, total_tokens: 10, incomplete_usage_count: 0}});
  h.setActivity(old); await h.module.loadOverview(); assert.equal(h.get("activityTotalTokens").textContent, "42");
  old.runtime = {...old.runtime, since: "2026-10-14T02:45:00Z"}; await h.module.loadOverview();
  assert.equal(h.get("activityTotalTokens").textContent, "10");
});
test("mobile plots keep readable axes, compact large values, and position partial buckets by actual time", async () => {
  const h = harness();
  for (const id of ["activityConcurrencyChart", "activityTokenChart"]) h.get(id).getBoundingClientRect = () => ({left: 0, top: 0, width: 290, height: 195});
  const data = activity(); data.window.from = "2026-10-14T03:00:05Z"; data.series[0].from = data.window.from;
  data.series[2].input_tokens = 9000000; data.clients[0].series = data.series.map(b => ({...b})); h.setActivity(data); await h.module.loadOverview();
  assert.equal(h.get("activityTokenChart").attrs.viewBox, "0 0 290 195");
  assert.match(h.get("activityTokenChart").innerHTML, /M<\/text>/);
  // The first partial bucket lasts 5s, so its midpoint is at 10% of the plot.
  await h.get("activityConcurrencyChart").fire("pointermove", {clientX: 67.4});
  assert.match(h.get("activityConcurrencyChart").innerHTML, /class="activity-cursor" x1="67.4"/);
  assert.match(h.get("activityConcurrencyReadout").textContent, /未采样/);
  await h.get("activityTokenChart").fire("keydown", {key: "End"});
  assert.match(h.get("activityTokenReadout").textContent, /9000000/);
});
test("leaving home cancels late results; resume refresh cannot be overwritten by the cancelled result", async () => {
  const h = harness(); const pending = [];
  h.setHandler(url => url.endsWith("activity") ? new Promise(resolve => pending.push(resolve)) : Promise.resolve({}));
  const oldLoad = h.module.loadOverview(); h.module.stopOverviewAutoRefresh();
  h.module.startOverviewAutoRefresh(); const newLoad = h.module.loadOverview();
  pending[1](activity()); await newLoad;
  pending[0](activity({totals: {input_tokens: 999, output_tokens: 0, total_tokens: 999, incomplete_usage_count: 0}})); await oldLoad;
  assert.equal(h.get("activityTotalTokens").textContent, "42"); h.module.stopOverviewAutoRefresh();
});

test("sort changes ordering without replacing client buttons and persists over new samples", async () => {
  const sort = new Element("button"); sort.dataset = {activitySort: "total_tokens", label: "5m 总量"}; sort.parentNode = new Element("th");
  const h = harness({sortButtons: [sort]}); const data = activity();
  data.clients.push({...data.clients[0], name: "Second", configured: true, active_requests: 0, totals: {...data.clients[0].totals, total_tokens: 99}});
  h.setActivity(data); await h.module.loadOverview(); const firstRow = h.get("activityClientsBody").children[0];
  await sort.fire("click"); assert.equal(h.get("activityClientsBody").children[1], firstRow); assert.equal(sort.parentNode.attrs["aria-sort"], "descending");
  await h.module.loadOverview(); assert.equal(h.get("activityClientsBody").children[1], firstRow);
  await sort.fire("click"); assert.equal(h.get("activityClientsBody").children[0], firstRow); assert.equal(sort.parentNode.attrs["aria-sort"], "ascending");
});
test("information tips update on hover, work on touch and focus, and close with Escape", async () => {
  const upper = new Element("button"); upper.dataset.activityInfo = "上方解释";
  const lower = new Element("button"); lower.dataset.activityInfo = "下方解释";
  const h = harness({infoButtons: [upper, lower]});
  await upper.fire("pointerenter", {pointerType: "mouse"}); assert.equal(h.get("activityTooltip").textContent, "上方解释");
  await lower.fire("pointerenter", {pointerType: "mouse"}); assert.equal(h.get("activityTooltip").textContent, "下方解释");
  h.doc.listeners.keydown({key: "Escape"}); assert.equal(h.get("activityTooltip").hidden, true);
  await upper.fire("pointerdown", {pointerType: "touch"}); await upper.fire("focus"); await upper.fire("click"); assert.equal(h.get("activityTooltip").hidden, false);
  await upper.fire("pointerdown", {pointerType: "touch"}); await upper.fire("click"); assert.equal(h.get("activityTooltip").hidden, true);
  await lower.fire("focus"); assert.equal(h.get("activityTooltip").hidden, false); await lower.fire("blur"); assert.equal(h.get("activityTooltip").hidden, true);
});

test("concurrency peaks render as bars and unknown samples leave gaps", async () => {
  const h = harness(); const data = activity();
  data.series[0].active_requests_peak = 3; data.series[1].active_requests_peak = null; data.series[2].active_requests_peak = 5;
  h.setActivity(data); await h.module.loadOverview();
  const svg = h.get("activityConcurrencyChart").innerHTML;
  assert.equal((svg.match(/<rect class="activity-concurrency-bar"/g) || []).length, 2);
  assert.doesNotMatch(svg, /activity-line activity-concurrency/);
  assert.doesNotMatch(svg, /<rect[^>]*class="activity-concurrency-bar"[^>]*height="0"/);
});


test("hung activity expires, preserves its last sample and recovers without overlapping polls", async () => {
  const h = harness(); await h.module.loadOverview(); const timestamp = h.get("activityUpdatedAt").textContent;
  h.setHandler(url => url.endsWith("activity") ? new Promise(() => {}) : Promise.resolve({}));
  h.module.startOverviewAutoRefresh(); const hung = h.module.loadOverview();
  const request = h.calls.filter(c => c.url.endsWith("activity")).at(-1);
  await h.advance(11999);
  assert.equal(request.opts.signal.aborted, false); assert.equal(h.get("refreshOverviewBtn").disabled, true);
  assert.equal(h.calls.filter(c => c.url.endsWith("activity")).length, 2);
  await h.advance(1);
  assert.equal(request.opts.signal.aborted, true); assert.equal(h.get("refreshOverviewBtn").disabled, false);
  assert.equal(h.get("page-overview").attrs["aria-busy"], "false");
  assert.equal(h.get("activityUpdatedAt").textContent, timestamp); assert.equal(h.get("activityTotalTokens").textContent, "42");
  assert.match(h.get("activityStatus").textContent, /超时.*上次/); await hung;
  h.setHandler(() => Promise.resolve(activity({runtime: {...activity().runtime, active_requests: 7}})));
  await h.advance(5000);
  assert.equal(h.get("activityActiveRequests").textContent, "7"); assert.equal(h.get("refreshOverviewBtn").disabled, false);
  assert.equal(h.calls.filter(c => c.url.endsWith("activity")).length, 3);
  assert.notEqual(h.get("activityStatus").dataset.state, "error"); h.module.stopOverviewAutoRefresh();
});
test("manual refresh works after timeout; cancelling hidden requests reports no error", async () => {
  const h = harness(); await h.module.loadOverview();
  h.setHandler(url => url.endsWith("activity") ? new Promise(() => {}) : Promise.resolve({}));
  const hung = h.module.loadOverview(); await h.advance(12000);
  assert.equal(h.get("refreshOverviewBtn").disabled, false); await hung;
  h.setHandler(() => Promise.resolve(activity()));
  await h.get("refreshOverviewBtn").fire("click"); await h.module.loadOverview();
  assert.notEqual(h.get("activityStatus").dataset.state, "error");
  h.setHandler(() => new Promise(() => {})); const cancelled = h.module.loadOverview(true);
  h.doc.hidden = true; h.doc.listeners.visibilitychange(); await h.advance(12000); await cancelled;
  assert.notEqual(h.get("activityStatus").dataset.state, "error"); assert.equal(h.timers.size, 0);
});
test("history has an independent deadline and retries while activity polling continues", async () => {
  const h = harness(); h.setHandler(url => url.endsWith("overview") ? new Promise(() => {}) : Promise.resolve(activity()));
  h.module.startOverviewAutoRefresh(); const hung = h.module.loadOverview(true); await h.advance(12000);
  assert.match(h.get("activityHistoryState").textContent, /超时/); assert.equal(h.get("activityTotalTokens").textContent, "42");
  assert.equal(h.calls.filter(c => c.url.endsWith("activity")).length, 3);
  assert.equal(h.calls.filter(c => c.url.endsWith("overview")).length, 1); await hung;
  h.setHandler(url => Promise.resolve(url.endsWith("overview") ? {request_stats_72h: {available: true, total: 4, success_rate: 100, record_failures: 0}} : activity()));
  await h.module.loadOverview(true); assert.equal(h.get("ovTotalReqs").textContent, "4");
  assert.equal(h.get("activityHistoryState").textContent, ""); h.module.stopOverviewAutoRefresh();
});
test("axes use useful headroom and nice ticks for zero, small and large peaks", async () => {
  for (const [peak, maximum] of [[0, 4], [1, 2], [10, 12], [12, 15], [36000, 40000], [9000000, 10000000]]) {
    const h = harness(); const data = activity();
    data.series = data.series.map(bucket => ({...bucket, active_requests_peak: peak, input_tokens: peak, output_tokens: 0}));
    data.clients[0].series = data.series.map(bucket => ({...bucket})); h.setActivity(data); await h.module.loadOverview();
    for (const id of ["activityConcurrencyChart", "activityTokenChart"]) {
      const labels = [...h.get(id).innerHTML.matchAll(/text-anchor="end">([^<]+)<\/text>/g)].map(match => match[1]).filter(label => !label.includes(":"));
      const label = maximum >= 1000000 ? maximum / 1000000 + "M" : maximum >= 1000 ? maximum / 1000 + "k" : String(maximum);
      assert.equal(labels.at(-1), label, `peak ${peak} axis maximum`);
      assert.ok(labels.length >= 3 && labels.length <= 6, `peak ${peak} tick count`);
      assert.doesNotMatch(h.get(id).innerHTML, /NaN|Infinity/);
    }
  }
});
test("nanosecond partial buckets are valid while equal and reversed same-ms intervals fail", async () => {
  const h = harness(); const data = activity();
  data.series[0].from = "2026-10-14T03:00:00.000123456Z"; data.series[0].to = "2026-10-14T03:00:00.000223456Z";
  data.series[2].from = "2026-10-14T03:00:30Z"; data.series[2].to = "2026-10-14T03:00:30.000123456Z";
  data.window.from = data.series[0].from; data.window.to = data.series[2].to;
  data.generated_at = data.window.to; data.clients[0].series = data.series.map(bucket => ({...bucket}));
  h.setActivity(data); await h.module.loadOverview(); assert.equal(h.get("activityTotalTokens").textContent, "42");
  assert.notEqual(h.get("activityStatus").dataset.state, "error"); assert.doesNotMatch(h.get("activityConcurrencyChart").innerHTML, /NaN|Infinity/);
  for (const to of [data.series[0].from, "2026-10-14T03:00:00.000023456Z"]) {
    const invalid = {...data, series: data.series.map((bucket, i) => i === 0 ? {...bucket, to} : bucket)};
    h.setActivity(invalid); await h.module.loadOverview(); assert.equal(h.get("activityStatus").dataset.state, "error");
    assert.equal(h.get("activityTotalTokens").textContent, "42");
  }
});


test("history timeout preserves completed statistics and fees from the last successful load", async () => {
  const h = harness(); h.setOverview({request_stats_72h: {available: true, total: 4, success_rate: 100, record_failures: 0}, usage_summary: {today: {estimated_cost: 2}}});
  await h.module.loadOverview(true); const fees = h.get("overviewUsageSummary").innerHTML;
  h.setHandler(url => url.endsWith("overview") ? new Promise(() => {}) : Promise.resolve(activity()));
  const hung = h.module.loadOverview(true); await h.advance(12000); await hung;
  assert.equal(h.get("ovTotalReqs").textContent, "4"); assert.equal(h.get("overviewUsageSummary").innerHTML, fees);
  assert.match(h.get("ovRequestStatsStatus").textContent, /超时.*上次数据/);
});
test("a transport response arriving after timeout cannot overwrite the recovered sample", async () => {
  const h = harness(); await h.module.loadOverview(); let resolve;
  h.setHandler(url => url.endsWith("activity") ? new Promise(r => resolve = r) : Promise.resolve({}));
  const hung = h.module.loadOverview(); await h.advance(12000); await hung;
  h.setHandler(() => Promise.resolve(activity({runtime: {...activity().runtime, active_requests: 7}})));
  await h.module.loadOverview(); resolve(activity());
  for (let i = 0; i < 12; i++) await Promise.resolve();
  assert.equal(h.get("activityActiveRequests").textContent, "7");
});

test("range cards distinguish missing totals from real zero and reload only aggregate", () => withTZ("Asia/Shanghai", async () => {
  const ranges = rangeKeys.map(rangeButton);
  const h = harness({rangeButtons: ranges});
  h.setOverview({client_count: 2, targets: [], recent_audit: []});
  h.setNow(new Date(2026, 9, 26, 11));
  const byFrom = {
    [new Date(2026, 9, 26).toISOString()]: aggregateResult(),
    [new Date(2026, 9, 25).toISOString()]: aggregateResult({totals: aggTotals({requests: 7, input_tokens: 30, output_tokens: 12, tool_use_prompt_tokens: 5, estimated_cost: 1.23, incomplete_usage_count: 1})}),
    [new Date(2026, 9, 20).toISOString()]: {totals: null},
    [new Date(2026, 8, 27).toISOString()]: {totals: {}},
  };
  h.setAggregateHandler(url => byFrom[new URL(url, "http://local").searchParams.get("from")] || aggregateResult());
  await h.module.loadOverview();
  const activityCalls = () => h.calls.filter(c => c.url.endsWith("activity")).length;
  const initialActivity = activityCalls();
  assert.equal(h.get("ovRangeRequests").textContent, "0");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "0");
  assert.equal(h.get("ovRangeInputTokens").textContent, "0");
  assert.equal(h.get("ovRangeOutputTokens").textContent, "0");
  assert.equal(h.get("ovRangeCost").textContent, "$0.00");
  assert.equal(h.get("ovRangeStatus").dataset.state, "ready");
  assert.equal(ranges[0].attrs["aria-pressed"], "true");
  await ranges[1].fire("click"); await h.advance(0);
  assert.equal(h.get("ovRangeRequests").textContent, "7");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "47");
  assert.equal(h.get("ovRangeInputTokens").textContent, "35", "input includes the 5 tool input tokens so input + output = total");
  assert.equal(h.get("ovRangeOutputTokens").textContent, "12");
  assert.equal(h.get("ovRangeCost").textContent, "$1.23");
  assert.equal(ranges[1].attrs["aria-pressed"], "true");
  assert.equal(ranges[0].attrs["aria-pressed"], "false");
  assert.match(h.get("ovRangeStatus").textContent, /含模型组系数/);
  assert.match(h.get("ovRangeStatus").textContent, /1 条用量不完整/);
  assert.equal(h.get("ovRangeStatus").dataset.state, "partial");
  assert.equal(activityCalls(), initialActivity);
  assert.ok(h.calls.filter(c => c.url.includes("usage/aggregate")).every(c => c.url.includes("group_by=hour")));
  await ranges[2].fire("click"); await h.advance(0);
  assert.equal(h.get("ovRangeRequests").textContent, "不可用");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "不可用");
  assert.equal(h.get("ovRangeCost").textContent, "不可用");
  assert.match(h.get("ovRangeStatus").textContent, /不可用/);
  assert.match(h.get("ovRangeStatus").textContent, /7 天/);
  assert.equal(activityCalls(), initialActivity);
  await ranges[3].fire("click"); await h.advance(0);
  assert.equal(h.get("ovRangeRequests").textContent, "不可用");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "不可用");
  assert.equal(h.get("ovRangeCost").textContent, "不可用");
  assert.notEqual(h.get("ovRangeStatus").dataset.state, "ready");
  assert.match(h.get("ovRangeStatus").textContent, /不可用/);
  assert.equal(activityCalls(), initialActivity);
  await ranges[0].fire("click"); await h.advance(0);
  assert.equal(h.get("ovRangeStatus").dataset.state, "ready");
  h.setAggregateHandler(null);
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 1, incomplete_cost_count: 1})}));
  await h.module.loadOverview(true);
  assert.equal(h.get("ovRangeStatus").dataset.state, "partial");
  assert.match(h.get("ovRangeStatus").textContent, /1 条成本覆盖不完整/);
}));

test("legacy history table keeps missing periods and fields distinct from real zero", async () => {
  const h = harness();
  h.setOverview({
    usage_summary: {
      last_hour: {requests: 0, input_tokens: 0, output_tokens: 0, tool_use_prompt_tokens: 0, estimated_cost: 0},
      today: {requests: 5, input_tokens: 2, output_tokens: 3, tool_use_prompt_tokens: 4, estimated_cost: 0.5},
      yesterday: {},
      last_7_days: {requests: 4},
      last_30_days: null,
    },
  });
  await h.module.loadOverview(true);
  const html = h.get("overviewUsageSummary").innerHTML;
  assert.match(html, /<th scope="row">近1小时<\/th><td>0<\/td><td>0<\/td><td>\$0\.00<\/td>/);
  assert.match(html, /<th scope="row">今日<\/th><td>5<\/td><td>9<\/td><td>\$0\.50<\/td>/);
  assert.match(html, /<th scope="row">昨日<\/th><td>不可用<\/td><td>不可用<\/td><td>不可用<\/td>/);
  assert.match(html, /<th scope="row">近7天<\/th><td>4<\/td><td>不可用<\/td><td>不可用<\/td>/);
  assert.match(html, /<th scope="row">近30天<\/th><td>不可用<\/td><td>不可用<\/td><td>不可用<\/td>/);
});

test("target health sorts unknown states last and degrades single and zero key targets", async () => {
  const target = (name, phase, extra = {}) => ({
    Name: name, Phase: phase, Paused: false, MutedUntil: "0001-01-01T00:00:00Z",
    TotalKeys: 2, ActiveKeys: 2, ExhaustedSubscriptionKeys: 0, ExhaustedAPIKeys: 0,
    RateLimitedKeys: 0, BlockedKeys: 0, OtherExhaustedKeys: 0,
    Capacity: 4, InFlight: 1, Utilization: 0.25,
    Window15m: {in_flight_peak: 2, affinity_hits: 1, breaker_trips: 0}, ...extra,
  });
  const h = harness();
  h.setOverview({
    client_count: 2,
    targets: [
      target("normal", "ready"),
      target("muted", "muted", {MutedUntil: "2099-01-01T00:00:00Z"}),
      target("half", "half-open"),
      target("offline", "offline", {Paused: true}),
      target("keyed", "ready", {TotalKeys: 3, ActiveKeys: 1, RateLimitedKeys: 2}),
      target("partial", "ready", {BlockedKeys: undefined}),
      target("unknown", "future-phase"),
      target("single", "ready", {TotalKeys: 1, ActiveKeys: 1, Endpoint: "https://secret.example/v1"}),
      target("zero", "ready", {TotalKeys: 0, ActiveKeys: 0}),
    ],
    usage_summary: {today: {requests: 1, input_tokens: 1, output_tokens: 0, estimated_cost: 0}},
    recent_audit: [],
  });
  h.setAggregate({totals: {}, by_client: [], by_model: []});
  await h.module.loadOverview();
  const names = [...h.get("targetHealthBody").innerHTML.matchAll(/data-target-name="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(names, ["muted", "half", "offline", "keyed", "partial", "normal", "single", "zero", "unknown"]);
  assert.match(h.get("targetHealthBody").innerHTML, /data-target-key-issue="true"[^>]*data-target-name="keyed"|data-target-name="keyed"[^>]*data-target-key-issue="true"/);
  assert.match(h.get("targetHealthBody").innerHTML, /data-target-key-issue="unknown"[^>]*data-target-name="partial"|data-target-name="partial"[^>]*data-target-key-issue="unknown"/);
  assert.match(h.get("targetHealthBody").innerHTML, /data-target-phase="muted"/);
  assert.match(h.get("targetHealthBody").innerHTML, /冷却至/);
  assert.match(h.get("targetHealthBody").innerHTML, /限流 2/);
  assert.match(h.get("targetHealthBody").innerHTML, /目标级/);
  assert.match(h.get("targetHealthBody").innerHTML, /Bearer|透传/);
  assert.match(h.get("targetHealthBody").innerHTML, /密钥状态不可用/);
  assert.match(h.get("overviewAttentionList").innerHTML, /partial[^<]*密钥状态未知/);
  assert.equal(h.get("chainTargets").textContent, "3 / 9");
  assert.equal(h.get("targetHealthCount").textContent, "9 个目标");
  assert.match(h.get("targetHealthStatus").textContent, /需要关注/);
  assert.match(h.get("targetHealthStatus").textContent, /状态未知/);
  assert.match(h.get("overviewAttentionList").innerHTML, /unknown[^<]*状态未知/);
  assert.doesNotMatch(h.get("targetHealth").innerHTML, /secret\.example/);
});

test("rankings order cost then requests then name and never fabricate zero", async () => {
  const h = harness();
  h.setOverview({client_count: 1, targets: [], usage_summary: {}, recent_audit: []});
  h.setAggregate({
    totals: {requests: 24, estimated_cost: 19},
    by_client: [
      {key: "c0", requests: 1, input_tokens: 1, output_tokens: 0, estimated_cost: 0},
      {key: "c1", requests: 10, input_tokens: 1, output_tokens: 0, estimated_cost: 5},
      {key: "c2", requests: 9, input_tokens: 1, output_tokens: 0, estimated_cost: 5},
      {key: "c3", requests: 12, input_tokens: 1, output_tokens: 0, tool_use_prompt_tokens: 6, estimated_cost: 5},
      {key: "c4", requests: 2, input_tokens: 1, output_tokens: 0, estimated_cost: 1},
      {key: "c5", requests: 100, input_tokens: 1, output_tokens: 0, estimated_cost: 3},
    ],
    by_model: [
      {key: "m-b", requests: 2, input_tokens: 1, output_tokens: 0, estimated_cost: 2},
      {key: "m-a", requests: 2, input_tokens: 1, output_tokens: 0, estimated_cost: 2},
      {key: "m-no-cost", requests: 3, input_tokens: 1, output_tokens: 0},
    ],
  });
  await h.module.loadOverview();
  const clients = [...h.get("topClientsBody").innerHTML.matchAll(/data-top-client="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(clients, ["c3", "c1", "c2", "c5", "c4", "c0"]);
  assert.match(h.get("topClientsBody").innerHTML, /\$5\.00/);
  assert.match(h.get("topClientsBody").innerHTML, /data-top-client="c3"><td>c3<\/td><td>12<\/td><td>7<\/td><td>\$5\.00<\/td><td>26\.3%<\/td>/);
  const models = [...h.get("topModelsBody").innerHTML.matchAll(/data-top-model="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(models, ["m-a", "m-b", "m-no-cost"]);
  assert.match(h.get("topModelsBody").innerHTML, /不可用/);
  assert.match(h.get("topClientsStatus").textContent, /含模型组系数/);
  assert.match(h.get("topClientsStatus").textContent, /占比/);
  assert.doesNotMatch(h.get("topClientsStatus").textContent, /UTC|group_by=day/);

  h.setAggregateHandler(() => Promise.reject(new Error("aggregate rejected")));
  await h.module.loadOverview(true);
  assert.match(h.get("topClientsStatus").textContent, /更新失败.*上次数据/);
  assert.equal(h.get("topClientsStatus").dataset.state, "partial");
  assert.match(h.get("topClientsBody").innerHTML, /data-top-client="c3"/);

  const failed = harness();
  failed.setAggregateHandler(() => Promise.reject(new Error("aggregate rejected")));
  await failed.module.loadOverview(true);
  assert.match(failed.get("topClientsStatus").textContent, /不可用/);
  assert.match(failed.get("topModelsStatus").textContent, /不可用/);
  assert.doesNotMatch(failed.get("topClientsBody").innerHTML, /\$0\.00|>0</);

  h.setAggregateHandler(null);
  h.setAggregate({totals: {}, by_client: [], by_model: []});
  await h.module.loadOverview(true);
  assert.match(h.get("topClientsBody").innerHTML, /暂无/);
  assert.match(h.get("topClientsStatus").textContent, /暂无可聚合/);
  assert.doesNotMatch(h.get("topClientsBody").innerHTML, /\$0\.00/);

  h.setAggregate({totals: {}});
  await h.module.loadOverview(true);
  assert.match(h.get("topClientsStatus").textContent, /缺少/);
  assert.match(h.get("topModelsStatus").textContent, /缺少/);
});

test("attention uses measured warnings and recent changes omit detail", async () => {
  const h = harness();
  h.setOverview({
    client_count: 1,
    targets: [
      {Name: "muted-target", Phase: "muted", MutedUntil: "2099-01-01T00:00:00Z", TotalKeys: 2, ActiveKeys: 1, RateLimitedKeys: 1},
      {Name: "offline-target", Phase: "offline", Paused: true, TotalKeys: 0, ActiveKeys: 0},
    ],
    usage_summary: {today: {requests: 3, input_tokens: 5, output_tokens: 1, estimated_cost: 0, incomplete_usage_count: 2, incomplete_cost_count: 1}},
    requests: {
      usage_record_failures: 2,
      usage_client_gone_no_usage: 1,
      usage_upstream_interrupted_no_usage: 1,
      request_stats_record_failures: 3,
    },
    request_stats_72h: {available: true, total: 10, success: 7, failure: 2, canceled: 1, record_failures: 0, since: "start", from: "from", to: "to"},
    recent_audit: [
      {id: "old", timestamp: "2026-10-14T02:00:00Z", actor: "alice", action: "create", object: "old-object", result: "ok", detail: "secret detail old"},
      {id: "new", timestamp: "2026-10-14T03:00:00Z", actor: "bob", action: "update", object: "new-object", result: "fail", detail: "secret detail new"},
    ],
  });
  h.setMetrics({uptime_seconds: 3660, requests: {total: 12}, persistence: {usage_async_pending: 2, usage_async_deferred: 1, request_stats_dropped: 3}, observation_budget: {observation_budget_exhausted: {br: 4, zstd: 0}}});
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 3, input_tokens: 5, output_tokens: 1, incomplete_usage_count: 2, incomplete_cost_count: 1})}));
  await h.module.loadOverview();
  const attention = h.get("overviewAttentionList").innerHTML;
  assert.match(attention, /muted-target/);
  assert.match(attention, /offline-target/);
  assert.match(attention, /用量写入积压 2 条/);
  assert.match(attention, /用量等待后台重试 1 条/);
  assert.match(attention, /请求完成统计队列丢弃 3 条/);
  assert.match(attention, /压缩响应观察预算拒绝 4 次/);
  assert.match(attention, /自启动累计 2 次用量写入失败/);
  assert.match(attention, /自启动累计 1 次客户端断开且未取得用量/);
  assert.match(attention, /自启动累计 1 次上游中断且未取得用量/);
  assert.match(attention, /自启动累计 3 次请求完成统计写入失败/);
  assert.match(attention, /72 小时完成样本中 2 个失败/);
  assert.match(attention, /72 小时完成样本中 1 个取消/);
  assert.doesNotMatch(attention, /成功率/);
  assert.match(attention, /2 条用量记录不完整/);
  assert.match(attention, /1 条成本覆盖不完整/);
  assert.match(h.get("overviewAttentionCount").textContent, /项/);
  const recent = h.get("recentChangesBody").innerHTML;
  assert.ok(recent.indexOf("new-object") < recent.indexOf("old-object"));
  assert.doesNotMatch(recent, /secret detail/);
  assert.match(h.get("recentChangesStatus").textContent, /2 条/);

  h.setMetrics({uptime_seconds: 1, requests: {total: 0}, persistence: {usage_async_pending: 0, usage_async_deferred: 0, request_stats_dropped: 0}});
  h.setOverview({
    client_count: 1,
    targets: [],
    usage_summary: {},
    requests: {usage_record_failures: 0, usage_client_gone_no_usage: 0, usage_upstream_interrupted_no_usage: 0, request_stats_record_failures: 0},
    request_stats_72h: {available: true, total: 0, failure: 0, canceled: 0},
    recent_audit: [],
  });
  h.setAggregate(aggregateResult());
  await h.module.loadOverview(true);
  assert.match(h.get("recentChangesBody").innerHTML, /暂无/);
  assert.match(h.get("overviewAttentionList").innerHTML, /暂无需要关注/);
  assert.doesNotMatch(h.get("overviewAttentionList").innerHTML, /自启动累计|72 小时完成样本/);
});

test("live status cards summarize available values and mark missing sources", async () => {
  const h = harness();
  h.setOverview({
    client_count: 3,
    targets: [
      {Name: "healthy", Phase: "ready", TotalKeys: 2, ActiveKeys: 2, RateLimitedKeys: 0, Capacity: 4, InFlight: 1, Window15m: {in_flight_peak: 1, affinity_hits: 0, breaker_trips: 0}},
      {Name: "limited", Phase: "ready", TotalKeys: 2, ActiveKeys: 1, RateLimitedKeys: 1, Capacity: 4, InFlight: 1, Window15m: {in_flight_peak: 1, affinity_hits: 0, breaker_trips: 0}},
    ],
    usage_summary: {today: {requests: 3, input_tokens: 10, output_tokens: 2, estimated_cost: 0}},
    recent_audit: [],
  });
  h.setMetrics(completeMetrics({uptime_seconds: 3660, requests: {total: 12}}));
  await h.module.loadOverview();
  assert.match(h.get("chainClients").textContent, /3 个配置客户端/);
  assert.match(h.get("chainClients").textContent, /活跃 1/);
  assert.equal(h.get("activityActiveRequests").textContent, "2");
  assert.doesNotMatch(h.get("chainProxy").textContent, /在途/, "in-flight has its own card");
  assert.match(h.get("chainProxy").textContent, /累计 12/);
  assert.match(h.get("chainProxy").textContent, /1 小时 1 分钟/);
  assert.match(h.get("chainTargets").textContent, /^\d+ \/ 2$/);
  assert.match(h.get("chainKeysDetail").textContent, /限流 1/);
  assert.equal(h.get("chainKeys").attrs["data-total"], "4");
  assert.equal(h.get("chainKeys").attrs["data-active"], "3");
  assert.equal(h.get("chainKeys").attrs["data-rate-limited"], "1");
  assert.equal(h.get("chainStatus").textContent, "实时状态已加载");

  h.setMetrics(completeMetrics({uptime_seconds: 0}));
  await h.module.loadOverview(true);
  assert.match(h.get("chainProxy").textContent, /运行 0 秒/);
  assert.equal(h.get("chainStatus").textContent, "实时状态已加载");

  h.setOverview({
    client_count: 3,
    targets: [{Name: "unknown-keys", Phase: "ready"}],
    usage_summary: {},
    recent_audit: [],
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("chainKeys").attrs["data-total"], "unavailable");
  assert.match(h.get("chainKeys").textContent, /不可用/);
  assert.match(h.get("chainStatus").textContent, /部分实时状态不可用/);

  h.setMetrics(null);
  await h.module.loadOverview(true);
  assert.match(h.get("chainStatus").textContent, /部分实时状态不可用/);
});

test("activity polling refreshes the live status cards without another overview request", async () => {
  const h = harness();
  h.setOverview({client_count: 3, targets: [], usage_summary: {}, recent_audit: []});
  h.setMetrics({uptime_seconds: 1, requests: {total: 1}, persistence: {}});
  await h.module.loadOverview();
  assert.match(h.get("chainClients").textContent, /活跃 1/);
  h.setActivity(activity({runtime: {...activity().runtime, active_clients: 5, active_requests: 6, websocket_sessions: 2}}));
  await h.module.loadOverview();
  assert.match(h.get("chainClients").textContent, /活跃 5/);
  assert.equal(h.get("activityActiveRequests").textContent, "6");
  assert.equal(h.calls.filter(c => c.url.endsWith("overview")).length, 1);
});

test("hidden overview aborts metrics and aggregate and reloads them on return", async () => {
  const h = harness(); const pending = [];
  h.setHandler(url => {
    if (url.endsWith("activity")) return Promise.resolve(activity());
    if (url.endsWith("overview")) return Promise.resolve({});
    if (url.endsWith("/admin/metrics") || url.includes("usage/aggregate")) return new Promise((resolve, reject) => pending.push({url, resolve, reject}));
    return Promise.resolve({});
  });
  const loading = h.module.loadOverview(true);
  const metricsCall = h.calls.find(c => c.url.endsWith("/admin/metrics"));
  const aggregateCall = h.calls.find(c => c.url.includes("usage/aggregate"));
  h.module.stopOverviewAutoRefresh();
  assert.equal(metricsCall.opts.signal.aborted, true);
  assert.equal(aggregateCall.opts.signal.aborted, true);
  await loading;
  h.setHandler(url => Promise.resolve(url.endsWith("activity") ? activity() : url.endsWith("overview") ? {} : url.endsWith("/admin/metrics") ? {uptime_seconds: 1} : {totals: {}, by_client: [], by_model: []}));
  h.module.startOverviewAutoRefresh();
  await h.module.loadOverview(true);
  assert.ok(h.calls.filter(c => c.url.endsWith("/admin/metrics")).length >= 2);
  assert.ok(h.calls.filter(c => c.url.includes("usage/aggregate")).length >= 2);
  h.module.stopOverviewAutoRefresh();
  assert.equal(pending.length, 2);
});

test("completed metrics and aggregate refetch after leaving and returning instead of hitting the cache", async () => {
  const h = harness();
  h.setOverview({
    client_count: 1,
    targets: [{Name: "healthy", Phase: "ready", TotalKeys: 2, ActiveKeys: 2, RateLimitedKeys: 0}],
    usage_summary: {},
    recent_audit: [],
  });
  h.setMetrics(completeMetrics({requests: {total: 7}}));
  h.setAggregate({
    totals: {requests: 1},
    by_client: [{key: "c1", requests: 1, input_tokens: 2, output_tokens: 1, estimated_cost: 0.5}],
    by_model: [{key: "m1", requests: 1, input_tokens: 2, output_tokens: 1, estimated_cost: 0.5}],
  });
  await h.module.loadOverview();
  const metricsCalls = () => h.calls.filter(c => c.url.endsWith("/admin/metrics")).length;
  const aggregateCalls = () => h.calls.filter(c => c.url.includes("usage/aggregate")).length;
  const metricsBefore = metricsCalls();
  const aggregateBefore = aggregateCalls();
  assert.equal(h.get("chainSummary").dataset.state, "ready");
  assert.equal(h.get("topClientsStatus").dataset.state, "ready");

  h.doc.hidden = true;
  h.module.stopOverviewAutoRefresh();
  h.doc.hidden = false;
  // Leaving keeps the last snapshots on screen (like activity and history); only the cache age is reset.
  assert.equal(h.get("chainSummary").dataset.state, "ready");
  assert.equal(h.get("topClientsStatus").dataset.state, "ready");
  assert.match(h.get("topClientsBody").innerHTML, /data-top-client="c1"/);

  h.module.startOverviewAutoRefresh();
  await h.module.loadOverview();
  assert.ok(metricsCalls() > metricsBefore, "metrics request must be sent again");
  assert.ok(aggregateCalls() > aggregateBefore, "aggregate request must be sent again");
  assert.equal(h.get("chainSummary").dataset.state, "ready");
  assert.equal(h.get("chainStatus").textContent, "实时状态已加载");
  assert.equal(h.get("topClientsStatus").dataset.state, "ready");
  assert.equal(h.get("topModelsStatus").dataset.state, "ready");
  h.module.stopOverviewAutoRefresh();
});

test("range token card explains that input includes cache and tool input", () => {
  const html = fs.readFileSync(path.join(__dirname, "..", "index.html"), "utf8");
  const card = html.match(/<article class="overview-card"><div class="overview-card-label"([^>]*)>总 Token<\/div><strong id="ovRangeTotalTokens">[\s\S]*?<\/article>/);
  assert.ok(card, "range total token card is present");
  assert.match(card[1], /title="[^"]*缓存读取[^"]*工具输入[^"]*"/);
  assert.match(card[1], /输入 \+ 输出 = 总 Token/);
  assert.match(card[0], /id="ovRangeInputTokens"/);
  assert.match(card[0], /id="ovRangeOutputTokens"/);
});

test("chain key summary refuses partial counters and keeps real zero", async () => {
  const h = harness();
  h.setMetrics(completeMetrics());
  h.setOverview({
    client_count: 1,
    targets: [
      {Name: "complete", Phase: "ready", TotalKeys: 2, ActiveKeys: 1, RateLimitedKeys: 1},
      {Name: "missing-active", Phase: "ready", TotalKeys: 2, RateLimitedKeys: 0},
    ],
    usage_summary: {},
    recent_audit: [],
  });
  await h.module.loadOverview();
  assert.equal(h.get("chainKeys").attrs["data-total"], "unavailable");
  assert.equal(h.get("chainKeys").attrs["data-active"], "unavailable");
  assert.equal(h.get("chainKeys").attrs["data-rate-limited"], "unavailable");
  assert.match(h.get("chainKeys").textContent, /不可用/);
  assert.equal(h.get("chainSummary").dataset.state, "partial");
  assert.match(h.get("chainStatus").textContent, /部分实时状态不可用/);

  h.setOverview({
    client_count: 1,
    targets: [
      {Name: "complete", Phase: "ready", TotalKeys: 2, ActiveKeys: 1, RateLimitedKeys: 1},
      {Name: "missing-rate", Phase: "ready", TotalKeys: 2, ActiveKeys: 2},
    ],
    usage_summary: {},
    recent_audit: [],
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("chainKeys").attrs["data-total"], "unavailable");
  assert.equal(h.get("chainKeys").attrs["data-active"], "unavailable");
  assert.equal(h.get("chainKeys").attrs["data-rate-limited"], "unavailable");
  assert.equal(h.get("chainSummary").dataset.state, "partial");

  h.setOverview({
    client_count: 1,
    targets: [{Name: "zero", Phase: "ready", TotalKeys: 0, ActiveKeys: 0, RateLimitedKeys: 0}],
    usage_summary: {},
    recent_audit: [],
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("chainKeys").textContent, "未配置独立密钥");
  assert.equal(h.get("chainKeys").attrs["data-total"], "0");
  assert.equal(h.get("chainKeys").attrs["data-active"], "0");
  assert.equal(h.get("chainKeys").attrs["data-rate-limited"], "0");
  assert.equal(h.get("chainSummary").dataset.state, "ready");

  h.setOverview({client_count: 1, targets: [], usage_summary: {}, recent_audit: []});
  await h.module.loadOverview(true);
  assert.equal(h.get("chainKeys").textContent, "暂无目标");
  assert.equal(h.get("chainKeys").attrs["data-total"], "0");
  assert.equal(h.get("chainKeys").attrs["data-active"], "0");
  assert.equal(h.get("chainKeys").attrs["data-rate-limited"], "0");
  assert.equal(h.get("chainSummary").dataset.state, "ready");
});

test("attention reports ready only when every consumed source is complete", async () => {
  const h = harness();
  const target = {Name: "target", Phase: "ready", TotalKeys: 0, ActiveKeys: 0, RateLimitedKeys: 0};
  const overview = () => ({
    client_count: 1,
    targets: [target],
    usage_summary: {today: completeRange()},
    request_stats_72h: completeStats(),
    recent_audit: [],
  });
  h.setOverview(overview());
  h.setMetrics(completeMetrics());
  await h.module.loadOverview();
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "ready");
  assert.equal(h.get("overviewAttentionStatus").textContent, "关注数据已加载");

  const scenario = async (change, note) => {
    h.setOverview(overview());
    h.setMetrics(completeMetrics());
    h.setAggregate(aggregateResult());
    change();
    await h.module.loadOverview(true);
    assert.equal(h.get("overviewAttentionStatus").dataset.state, "partial", note);
    assert.match(h.get("overviewAttentionStatus").textContent, /部分数据不可用/, note);
  };

  await scenario(() => h.setOverview({...overview(), request_stats_72h: undefined}), "missing request stats");
  await scenario(() => h.setOverview({...overview(), request_stats_72h: completeStats({available: false})}), "unavailable request stats");
  await scenario(() => h.setOverview({...overview(), request_stats_72h: completeStats({failure: undefined})}), "partial request stats");
  await scenario(() => h.setAggregate({by_client: [], by_model: []}), "missing selected range");
  await scenario(() => h.setAggregate(aggregateResult({totals: aggTotals({estimated_cost: undefined})})), "partial selected range");
  await scenario(() => h.setOverview({...overview(), targets: undefined}), "missing targets");
  await scenario(() => h.setMetrics(completeMetrics({observation_budget: undefined})), "missing observation budget");
  await scenario(() => h.setMetrics(completeMetrics({observation_budget: {observation_budget_exhausted: {br: 0}}})), "partial observation budget");
  await scenario(() => h.setMetrics(completeMetrics({requests: {}})), "missing requests total");
  await scenario(() => h.setMetrics(completeMetrics({uptime_seconds: undefined})), "missing uptime");
  await scenario(() => h.setMetrics(completeMetrics({persistence: {usage_async_pending: 0, usage_async_deferred: 0}})), "partial persistence");
  await scenario(() => h.setMetrics(completeMetrics({persistence: undefined})), "missing persistence");
  await scenario(() => {
    h.setOverview({...overview(), targets: [{Name: "partial", Phase: "ready", TotalKeys: 2, ActiveKeys: 2, RateLimitedKeys: 0}]});
    h.setMetrics(completeMetrics());
  }, "unknown key counters");

  h.setMetrics(completeMetrics({persistence: {usage_async_pending: 2, usage_async_deferred: 0, request_stats_dropped: 0}}));
  h.setOverview(overview());
  h.setAggregate(aggregateResult({totals: aggTotals({incomplete_usage_count: 2})}));
  await h.module.loadOverview(true);
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "ready");
  assert.match(h.get("overviewAttentionStatus").textContent, /已检查/);
  assert.match(h.get("overviewAttentionList").innerHTML, /用量写入积压 2 条/);
  assert.match(h.get("overviewAttentionList").innerHTML, /2 条用量记录不完整/);

  h.setMetrics(completeMetrics());
  h.setOverview(overview());
  h.setAggregate(aggregateResult());
  await h.module.loadOverview(true);
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "ready");

  h.setHandler(url => {
    if (url.endsWith("activity")) return Promise.resolve(activity());
    if (url.endsWith("overview")) return Promise.reject(new Error("offline"));
    if (url.endsWith("/admin/metrics")) return Promise.resolve(completeMetrics());
    return Promise.resolve({totals: {}, by_client: [], by_model: []});
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "partial");
  assert.match(h.get("overviewAttentionStatus").textContent, /部分数据不可用/);
  assert.match(h.get("overviewAttentionList").innerHTML, /暂无需要关注的问题/);

  // Every source unavailable from the start (a fresh page has no stale range data).
  const none = harness();
  none.setOverview({});
  none.setMetrics(null);
  none.setAggregateHandler(() => Promise.reject(new Error("offline")));
  await none.module.loadOverview(true);
  assert.equal(none.get("overviewAttentionStatus").dataset.state, "error");
  assert.match(none.get("overviewAttentionStatus").textContent, /不可用/);
});

test("a failed overview refresh keeps old panels but marks them partial", async () => {
  const h = harness();
  h.setOverview({
    client_count: 1,
    targets: [{Name: "target", Phase: "ready", TotalKeys: 0, ActiveKeys: 0, RateLimitedKeys: 0}],
    usage_summary: {today: completeRange()},
    request_stats_72h: completeStats(),
    recent_audit: [{id: "audit-1", timestamp: "2026-10-20T00:00:00Z", actor: "alice", action: "update", object: "target", result: "ok"}],
  });
  h.setMetrics(completeMetrics());
  await h.module.loadOverview();
  assert.equal(h.get("chainStatus").textContent, "实时状态已加载");

  h.setHandler(url => {
    if (url.endsWith("overview")) return Promise.reject(new Error("offline"));
    if (url.endsWith("activity")) return Promise.resolve(activity());
    if (url.endsWith("/admin/metrics")) return Promise.resolve(completeMetrics());
    return Promise.resolve({totals: {}, by_client: [], by_model: []});
  });
  await h.module.loadOverview(true);

  assert.equal(h.get("chainSummary").dataset.state, "partial");
  assert.match(h.get("chainStatus").textContent, /更新失败.*上次数据/);
  for (const id of ["targetHealthStatus", "recentChangesStatus"]) {
    assert.equal(h.get(id).dataset.state, "partial", id);
    assert.match(h.get(id).textContent, /更新失败.*上次数据/, id);
  }
  assert.match(h.get("targetHealthBody").innerHTML, /target/);
  assert.match(h.get("recentChangesBody").innerHTML, /audit-1/);
  assert.equal(h.get("chainTargets").textContent, "1 / 1");
  assert.doesNotMatch(h.get("ovRangeStatus").textContent, /更新失败/, "range cards come from the aggregate, not the overview response");

  const empty = harness();
  empty.setHandler(url => {
    if (url.endsWith("overview")) return Promise.reject(new Error("offline"));
    if (url.endsWith("activity")) return Promise.resolve(activity());
    if (url.endsWith("/admin/metrics")) return Promise.resolve(completeMetrics());
    return Promise.resolve({totals: {}});
  });
  await empty.module.loadOverview(true);
  assert.equal(empty.get("chainSummary").dataset.state, "error");
  assert.match(empty.get("chainStatus").textContent, /更新失败|不可用/);
  assert.equal(empty.get("targetHealthStatus").dataset.state, "error");
  assert.equal(empty.get("recentChangesStatus").dataset.state, "error");
  assert.match(empty.get("targetHealthBody").innerHTML, /不可用/);
  assert.match(empty.get("recentChangesBody").innerHTML, /不可用/);
});

test("empty target lists say no targets instead of claiming health or keyless mode", async () => {
  const h = harness();
  h.setOverview({
    client_count: 0,
    targets: [],
    usage_summary: {today: completeRange()},
    request_stats_72h: completeStats(),
    recent_audit: [],
  });
  h.setMetrics(completeMetrics());
  await h.module.loadOverview();
  assert.equal(h.get("targetHealthStatus").textContent, "暂无目标");
  assert.equal(h.get("targetHealthStatus").dataset.state, "ready");
  assert.match(h.get("targetHealthBody").innerHTML, /暂无已配置目标/);
  assert.equal(h.get("chainTargets").textContent, "暂无目标");
  assert.equal(h.get("chainKeys").textContent, "暂无目标");
  assert.doesNotMatch(h.get("chainTargets").textContent, /正常/);
  assert.doesNotMatch(h.get("chainKeys").textContent, /未配置独立密钥/);
  assert.equal(h.get("chainSummary").dataset.state, "ready");
});

test("history counters stay unavailable when the overview omits them", async () => {
  const h = harness();
  h.setOverview({
    usage_summary: {today: completeRange()},
    request_stats_72h: {
      available: true, success: 0, failure: 0, canceled: 0, success_rate: null,
      record_failures: undefined, since: undefined, from: undefined, to: undefined,
    },
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("ovTargets").textContent, "不可用");
  assert.equal(h.get("ovClients").textContent, "不可用");
  assert.equal(h.get("ovModelCosts").textContent, "不可用");
  assert.equal(h.get("ovTotalReqs").textContent, "不可用");
  assert.equal(h.get("ovSuccessRate").textContent, "--");
  assert.match(h.get("activityHistoryState").textContent, /完整性不可用/);
  assert.match(h.get("ovRequestStatsStatus").textContent, /总数不可用/);
  assert.match(h.get("ovRequestStatsStatus").textContent, /写入失败计数不可用/);
  assert.doesNotMatch(h.get("activityHistoryTooltip").textContent, /undefined/);

  h.setOverview({
    active_targets: 0, client_count: 0, model_cost_count: 0,
    usage_summary: {today: completeRange()},
    request_stats_72h: completeStats(),
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("ovTargets").textContent, "0");
  assert.equal(h.get("ovClients").textContent, "0");
  assert.equal(h.get("ovModelCosts").textContent, "0");
  assert.equal(h.get("ovTotalReqs").textContent, "0");
  assert.equal(h.get("activityHistoryState").textContent, "");
  assert.match(h.get("ovRequestStatsStatus").textContent, /暂无完成样本/);
});

test("chain shows partial metrics values but does not claim ready", async () => {
  const h = harness();
  h.setOverview({
    client_count: 1,
    targets: [{Name: "t", Phase: "ready", TotalKeys: 0, ActiveKeys: 0, RateLimitedKeys: 0}],
    usage_summary: {today: completeRange()},
    request_stats_72h: completeStats(),
    recent_audit: [],
  });
  h.setMetrics({
    uptime_seconds: 10,
    requests: {total: 12},
    persistence: {usage_async_pending: 0, usage_async_deferred: 0, request_stats_dropped: 0},
  });
  await h.module.loadOverview();
  assert.equal(h.get("chainSummary").dataset.state, "partial");
  assert.equal(h.get("chainStatus").textContent, "部分实时状态不可用");
  assert.match(h.get("chainProxy").textContent, /累计 12/);
  assert.match(h.get("chainProxy").textContent, /运行 10 秒/);
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "partial");

  h.setMetrics({
    uptime_seconds: 10,
    persistence: {usage_async_pending: 0, usage_async_deferred: 0, request_stats_dropped: 0},
    observation_budget: {observation_budget_exhausted: {br: 0, zstd: 0}},
  });
  await h.module.loadOverview(true);
  assert.equal(h.get("chainSummary").dataset.state, "partial");
  assert.match(h.get("chainProxy").textContent, /累计请求不可用/);
  assert.match(h.get("chainProxy").textContent, /运行 10 秒/);

  h.setMetrics(completeMetrics({uptime_seconds: 10, requests: {total: 12}}));
  await h.module.loadOverview(true);
  assert.equal(h.get("chainSummary").dataset.state, "ready");
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "ready");
});

test("unknown target phases never claim every target is normal", async () => {
  const h = harness();
  h.setOverview({
    client_count: 1,
    targets: [{Name: "mystery", Phase: "future-phase", TotalKeys: 0, ActiveKeys: 0, RateLimitedKeys: 0}],
    usage_summary: {today: completeRange()},
    request_stats_72h: completeStats(),
    recent_audit: [],
  });
  h.setMetrics(completeMetrics());
  await h.module.loadOverview();
  assert.equal(h.get("targetHealthStatus").dataset.state, "partial");
  assert.match(h.get("targetHealthStatus").textContent, /状态未知/);
  assert.doesNotMatch(h.get("targetHealthStatus").textContent, /均在正常状态/);
  assert.match(h.get("overviewAttentionList").innerHTML, /mystery[^<]*状态未知/);
  assert.equal(h.get("overviewAttentionStatus").dataset.state, "partial");
});

// ---- Range windows, local-day rollup and range-driven panels (Issue #53) ----

test("rangeWindow computes local boundaries; closed ranges end 1ms before the next boundary", () => withTZ("Asia/Shanghai", () => {
  const h = harness();
  const now = new Date(2026, 9, 26, 11, 15, 30);
  const today = h.module.rangeWindow("today", now);
  assert.equal(today.from.getTime(), new Date(2026, 9, 26).getTime());
  assert.equal(today.to.getTime(), now.getTime());
  assert.equal(today.granularity, "hour");
  assert.equal(today.closed, false);
  const yesterday = h.module.rangeWindow("yesterday", now);
  assert.equal(yesterday.from.getTime(), new Date(2026, 9, 25).getTime());
  assert.equal(yesterday.end.getTime(), new Date(2026, 9, 26).getTime());
  assert.equal(yesterday.to.getTime(), new Date(2026, 9, 26).getTime() - 1, "yesterday must not include today's first hour");
  assert.equal(yesterday.granularity, "hour");
  assert.equal(yesterday.closed, true);
  const week = h.module.rangeWindow("7d", now);
  assert.equal(week.from.getTime(), new Date(2026, 9, 20).getTime(), "7 local days including today");
  assert.equal(week.to.getTime(), now.getTime());
  assert.equal(week.granularity, "day");
  const month = h.module.rangeWindow("30d", now);
  assert.equal(month.from.getTime(), new Date(2026, 8, 27).getTime(), "30 local days including today");
  assert.equal(month.granularity, "day");
  const last = h.module.rangeWindow("last_month", now);
  assert.equal(last.from.getTime(), new Date(2026, 8, 1).getTime());
  assert.equal(last.end.getTime(), new Date(2026, 9, 1).getTime());
  assert.equal(last.to.getTime(), new Date(2026, 9, 1).getTime() - 1);
  assert.equal(last.granularity, "day");
  const january = h.module.rangeWindow("last_month", new Date(2027, 0, 5, 8));
  assert.equal(january.from.getTime(), new Date(2026, 11, 1).getTime(), "January rolls back to last December");
  assert.equal(january.to.getTime(), new Date(2027, 0, 1).getTime() - 1);
  assert.equal(h.module.rangeWindow("30d", new Date(2026, 2, 15, 9)).from.getTime(), new Date(2026, 1, 14).getTime(), "30 days crosses February");
  assert.equal(h.module.rangeWindow("1h", now), null, "the removed last-hour range is not accepted");
}));

test("rollupBuckets sums hourly buckets into local days and zero-fills only slots inside the range", () => withTZ("Asia/Shanghai", () => {
  const h = harness();
  const window = h.module.rangeWindow("7d", new Date(2026, 9, 26, 11, 15));
  const slots = h.module.rollupBuckets([
    hourBucket(new Date(2026, 9, 20, 0), {cached_tokens: 3}),
    hourBucket(new Date(2026, 9, 20, 23), {cached_tokens: 3, tool_use_prompt_tokens: 4}),
    hourBucket(new Date(2026, 9, 26, 11), {incomplete_usage_count: 1}),
  ], window);
  assert.equal(slots.length, 7);
  assert.deepEqual(Array.from(slots, slot => slot.start.getDate()), [20, 21, 22, 23, 24, 25, 26]);
  assert.equal(slots[0].requests, 2);
  assert.equal(slots[0].input, 30, "input includes cache read/write and tool input");
  assert.equal(slots[0].output, 4);
  assert.equal(slots[0].cost, 1);
  assert.equal(slots[3].requests, 0);
  assert.equal(slots[3].empty, true, "a day without buckets is a real empty day");
  assert.equal(slots[6].incompleteUsage, 1);
  const today = h.module.rangeWindow("today", new Date(2026, 9, 26, 3, 20));
  const hours = h.module.rollupBuckets([hourBucket(new Date(2026, 9, 26, 2))], today);
  assert.deepEqual(Array.from(hours, slot => slot.start.getHours()), [0, 1, 2, 3], "no zero buckets for future hours");
  assert.equal(hours[2].requests, 1);
  const yesterday = h.module.rangeWindow("yesterday", new Date(2026, 9, 26, 3, 20));
  const closed = h.module.rollupBuckets([hourBucket(new Date(2026, 9, 25, 23))], yesterday);
  assert.equal(closed.length, 24);
  assert.equal(closed[23].start.getHours(), 23);
  assert.equal(closed[23].requests, 1);
}));

test("rollupBuckets keeps half-hour zones and DST days inside the selected local days", () => {
  withTZ("Asia/Kolkata", () => {
    const h = harness();
    const window = h.module.rangeWindow("7d", new Date(2026, 9, 26, 11));
    // UTC truncation makes the first returned bucket start 30 minutes before local midnight.
    const early = new Date(window.from.getTime() - 30 * 60000);
    const slots = h.module.rollupBuckets([hourBucket(early)], window);
    assert.equal(slots.length, 7);
    assert.equal(slots[0].requests, 1, "a truncated leading bucket is kept in the first day");
  });
  withTZ("America/New_York", () => {
    const h = harness();
    const window = h.module.rangeWindow("7d", new Date(2026, 10, 3, 10));
    const slots = h.module.rollupBuckets([hourBucket(new Date(2026, 10, 1, 23))], window);
    assert.deepEqual(Array.from(slots, slot => slot.start.getDate()), [28, 29, 30, 31, 1, 2, 3]);
    assert.equal(slots[4].requests, 1, "the 25-hour DST day is still one local day");
  });
});

test("range data comes from one hourly aggregate request with local boundaries", () => withTZ("Asia/Shanghai", async () => {
  const ranges = rangeKeys.map(rangeButton);
  const h = harness({rangeButtons: ranges});
  h.setNow(new Date(2026, 9, 26, 11, 15, 30));
  await h.module.loadOverview();
  const aggregates = () => h.calls.filter(c => c.url.includes("/admin/data/usage/aggregate"));
  assert.equal(aggregates().length, 1);
  const first = new URL(aggregates()[0].url, "http://local");
  assert.equal(first.searchParams.get("group_by"), "hour");
  assert.equal(first.searchParams.get("from"), new Date(2026, 9, 26).toISOString());
  assert.equal(first.searchParams.get("to"), new Date(2026, 9, 26, 11, 15, 30).toISOString());
  assert.equal(ranges[0].attrs["aria-pressed"], "true");
  assert.ok(h.calls.every(c => !c.url.includes("group_by=day")));
  await ranges[1].fire("click"); await h.advance(0);
  const second = new URL(aggregates().at(-1).url, "http://local");
  assert.equal(second.searchParams.get("from"), new Date(2026, 9, 25).toISOString());
  assert.equal(second.searchParams.get("to"), new Date(new Date(2026, 9, 26).getTime() - 1).toISOString());
  assert.equal(ranges[1].attrs["aria-pressed"], "true");
  assert.equal(ranges[0].attrs["aria-pressed"], "false");
  await ranges[4].fire("click"); await h.advance(0);
  const third = new URL(aggregates().at(-1).url, "http://local");
  assert.equal(third.searchParams.get("from"), new Date(2026, 8, 1).toISOString());
  assert.equal(third.searchParams.get("to"), new Date(new Date(2026, 9, 1).getTime() - 1).toISOString());
  assert.match(h.get("ovRangeWindow").textContent, /UTC\+08:00/);
  assert.match(h.get("ovRangeWindow").textContent, /09-01 00:00/);
  assert.doesNotMatch(h.get("ovRangeWindow").textContent, /整点/);
}));

test("half-hour time zones explain that range edges follow whole UTC hours", () => withTZ("Asia/Kolkata", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setNow(new Date(2026, 9, 26, 11, 15));
  await h.module.loadOverview();
  assert.match(h.get("ovRangeWindow").textContent, /UTC\+05:30/);
  assert.match(h.get("ovRangeWindow").textContent, /整点/);
}));

test("switching ranges aborts the old aggregate and a late old response cannot overwrite the new range", () => withTZ("Asia/Shanghai", async () => {
  const ranges = rangeKeys.map(rangeButton);
  const h = harness({rangeButtons: ranges});
  h.setNow(new Date(2026, 9, 26, 11, 15));
  const pending = [];
  h.setAggregateHandler((url, opts) => new Promise(resolve => pending.push({url, opts, resolve})));
  const load = h.module.loadOverview();
  await ranges[1].fire("click");
  assert.equal(pending.length, 2);
  assert.equal(pending[0].opts.signal.aborted, true, "old range request is aborted");
  pending[1].resolve(aggregateResult({
    totals: aggTotals({requests: 5, input_tokens: 50, output_tokens: 10, estimated_cost: 2}),
    buckets: [hourBucket(new Date(2026, 9, 25, 9), {requests: 5, input_tokens: 50, output_tokens: 10, estimated_cost: 2})],
    by_client: [{key: "yesterday-client", ...aggTotals({requests: 5, input_tokens: 50, output_tokens: 10, estimated_cost: 2})}],
    by_model: [{key: "yesterday-model", ...aggTotals({requests: 5, input_tokens: 50, output_tokens: 10, estimated_cost: 2})}],
  }));
  await h.advance(0);
  pending[0].resolve(aggregateResult({totals: aggTotals({requests: 999}), by_client: [{key: "stale-client", ...aggTotals({requests: 999})}], by_model: []}));
  await load; await h.advance(0);
  assert.equal(h.get("ovRangeRequests").textContent, "5");
  assert.match(h.get("topClientsBody").innerHTML, /yesterday-client/);
  assert.doesNotMatch(h.get("topClientsBody").innerHTML, /stale-client/);
  assert.match(h.get("ovTokenTrend").getAttribute("aria-label"), /昨日/);
}));

test("cards, both trend charts and both rankings follow the selected range", () => withTZ("Asia/Shanghai", async () => {
  const ranges = rangeKeys.map(rangeButton);
  const h = harness({rangeButtons: ranges, barButtons: [barButton("requests"), barButton("cost")]});
  h.setNow(new Date(2026, 9, 26, 3, 20));
  h.setAggregateHandler(url => {
    const from = new URL(url, "http://local").searchParams.get("from");
    if (from === new Date(2026, 9, 26).toISOString()) return aggregateResult({
      totals: aggTotals({requests: 3, input_tokens: 30, output_tokens: 6, cached_tokens: 4, estimated_cost: 1.5}),
      buckets: [hourBucket(new Date(2026, 9, 26, 1), {requests: 3, input_tokens: 30, output_tokens: 6, cached_tokens: 4, estimated_cost: 1.5})],
      by_client: [{key: "alpha", ...aggTotals({requests: 3, input_tokens: 30, output_tokens: 6, cached_tokens: 4, estimated_cost: 1.5})}],
      by_model: [{key: "m-today", ...aggTotals({requests: 3, input_tokens: 30, output_tokens: 6, cached_tokens: 4, estimated_cost: 1.5})}],
    });
    return aggregateResult({
      totals: aggTotals({requests: 9, input_tokens: 90, output_tokens: 18, estimated_cost: 4}),
      buckets: [hourBucket(new Date(2026, 9, 21, 5), {requests: 4, input_tokens: 40, output_tokens: 8, estimated_cost: 1}), hourBucket(new Date(2026, 9, 25, 7), {requests: 5, input_tokens: 50, output_tokens: 10, estimated_cost: 3})],
      by_client: [{key: "alpha", ...aggTotals({requests: 4, estimated_cost: 1})}, {key: "beta", ...aggTotals({requests: 5, estimated_cost: 3})}],
      by_model: [{key: "m-a", ...aggTotals({requests: 9, estimated_cost: 4})}],
    });
  });
  await h.module.loadOverview();
  assert.equal(h.get("ovRangeRequests").textContent, "3");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "40");
  assert.equal(h.get("ovRangeInputTokens").textContent, "34", "input includes cached tokens so input + output = total");
  assert.equal(h.get("ovRangeOutputTokens").textContent, "6");
  assert.equal(h.get("ovRangeCost").textContent, "$1.50");
  assert.equal(h.get("ovRangeClients").textContent, "1");
  assert.equal(h.get("ovRangeModels").textContent, "1");
  assert.equal((h.get("ovBarTrend").innerHTML.match(/<rect class="overview-bar/g) || []).length, 1, "one non-zero hour bar");
  assert.match(h.get("ovTokenTrend").innerHTML, /activity-input/);
  assert.match(h.get("ovTokenTrend").innerHTML, /activity-output/);
  assert.match(h.get("ovTrendUnit").textContent, /小时/);
  await h.get("ovTokenTrend").fire("keydown", {key: "Home"});
  await h.get("ovTokenTrend").fire("keydown", {key: "ArrowRight"});
  for (const id of ["ovTokenTrendReadout", "ovBarTrendReadout"]) {
    const readout = h.get(id).textContent;
    assert.match(readout, /01:00/, id);
    assert.match(readout, /请求 3/, id);
    assert.match(readout, /输入 34/, id);
    assert.match(readout, /输出 6/, id);
    assert.match(readout, /\$1\.50/, id);
  }
  await ranges[2].fire("click"); await h.advance(0);
  assert.equal(h.get("ovRangeRequests").textContent, "9");
  assert.equal(h.get("ovRangeCost").textContent, "$4.00");
  assert.equal(h.get("ovRangeClients").textContent, "2");
  assert.match(h.get("ovTrendUnit").textContent, /天/);
  assert.equal((h.get("ovBarTrend").innerHTML.match(/<rect class="overview-bar/g) || []).length, 2);
  assert.match(h.get("ovBarTrend").getAttribute("aria-label"), /近 7 天/);
  assert.deepEqual([...h.get("topClientsBody").innerHTML.matchAll(/data-top-client="([^"]+)"/g)].map(m => m[1]), ["beta", "alpha"]);
  assert.deepEqual([...h.get("topModelsBody").innerHTML.matchAll(/data-top-model="([^"]+)"/g)].map(m => m[1]), ["m-a"]);
  const [, cost] = h.doc.querySelectorAll("[data-overview-bar]");
  await cost.fire("click");
  assert.equal(cost.attrs["aria-pressed"], "true");
  assert.match(h.get("ovBarTrend").getAttribute("aria-label"), /估算费用/);
  assert.match(h.get("ovBarTrend").innerHTML, /overview-bar-cost/);
  assert.match(h.get("ovBarTrend").innerHTML, /\$/);
  assert.doesNotMatch(h.get("ovBarTrend").innerHTML + h.get("ovTokenTrend").innerHTML, /NaN|Infinity|undefined/);
}));

test("range panels separate unavailable data from an empty range and flag incomplete usage", () => withTZ("Asia/Shanghai", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setNow(new Date(2026, 9, 26, 11));
  h.setAggregate(aggregateResult());
  await h.module.loadOverview();
  assert.equal(h.get("ovRangeRequests").textContent, "0");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "0");
  assert.equal(h.get("ovRangeCost").textContent, "$0.00");
  assert.equal(h.get("ovRangeClients").textContent, "0");
  assert.match(h.get("ovTrendStatus").textContent, /暂无/);
  assert.doesNotMatch(h.get("ovTrendStatus").textContent, /不可用/);
  assert.equal(h.get("ovRangeStatus").dataset.state, "ready");
  assert.match(h.get("ovRangeStatus").textContent, /估算/);
  assert.match(h.get("ovRangeStatus").textContent, /含模型组系数/);

  h.setAggregate({totals: {requests: 3}});
  await h.module.loadOverview(true);
  assert.equal(h.get("ovRangeRequests").textContent, "3");
  assert.equal(h.get("ovRangeTotalTokens").textContent, "不可用");
  assert.equal(h.get("ovRangeCost").textContent, "不可用");
  assert.equal(h.get("ovRangeClients").textContent, "不可用");
  assert.equal(h.get("ovRangeModels").textContent, "不可用");
  assert.match(h.get("ovTrendStatus").textContent, /不可用/);
  assert.match(h.get("ovTokenTrendReadout").textContent, /不可用/);
  assert.notEqual(h.get("ovRangeStatus").dataset.state, "ready");
  assert.match(h.get("topClientsStatus").textContent, /缺少/);

  h.setAggregate(aggregateResult({totals: aggTotals({requests: 2, incomplete_usage_count: 2, incomplete_cost_count: 1}), buckets: [hourBucket(new Date(2026, 9, 26, 9), {incomplete_usage_count: 2})]}));
  await h.module.loadOverview(true);
  assert.equal(h.get("ovRangeStatus").dataset.state, "partial");
  assert.match(h.get("ovRangeStatus").textContent, /2 条用量不完整/);
  assert.match(h.get("ovRangeStatus").textContent, /1 条成本覆盖不完整/);
  await h.get("ovTokenTrend").fire("keydown", {key: "End"});
  await h.get("ovTokenTrend").fire("keydown", {key: "ArrowLeft"});
  await h.get("ovTokenTrend").fire("keydown", {key: "ArrowLeft"});
  assert.match(h.get("ovTokenTrendReadout").textContent, /用量不完整/);

  h.setAggregateHandler(() => Promise.reject(new Error("offline")));
  await h.module.loadOverview(true);
  assert.equal(h.get("ovRangeRequests").textContent, "2", "a failed refresh of the same range keeps the last data");
  assert.match(h.get("ovRangeStatus").textContent, /更新失败.*上次数据/);
  assert.equal(h.get("ovRangeStatus").dataset.state, "partial");

  const empty = harness({rangeButtons: rangeKeys.map(rangeButton)});
  empty.setAggregateHandler(() => Promise.reject(new Error("offline")));
  await empty.module.loadOverview();
  for (const id of ["ovRangeRequests", "ovRangeTotalTokens", "ovRangeCost", "ovRangeClients", "ovRangeModels"]) assert.equal(empty.get(id).textContent, "不可用", id);
  assert.equal(empty.get("ovRangeStatus").dataset.state, "error");
  assert.match(empty.get("ovTrendStatus").textContent, /不可用/);
}));

test("a hung aggregate times out after 12s and keeps the last data of the same range", () => withTZ("Asia/Shanghai", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 4})}));
  await h.module.loadOverview();
  h.setAggregateHandler(() => new Promise(() => {}));
  const hung = h.module.loadOverview(true);
  const call = h.calls.filter(c => c.url.includes("usage/aggregate")).at(-1);
  await h.advance(11999);
  assert.equal(call.opts.signal.aborted, false);
  await h.advance(1); await hung;
  assert.equal(call.opts.signal.aborted, true);
  assert.equal(h.get("ovRangeRequests").textContent, "4");
  assert.match(h.get("ovRangeStatus").textContent, /超时.*上次数据/);
}));

test("rankings show top ten with cost share and never fabricate a share", () => withTZ("Asia/Shanghai", async () => {
  const h = harness();
  const clients = Array.from({length: 12}, (_, i) => ({key: "c" + String(i).padStart(2, "0"), ...aggTotals({requests: i + 1, input_tokens: 1, estimated_cost: i})}));
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 78, estimated_cost: 66}), by_client: clients, by_model: [{key: "m-no-cost", requests: 3, input_tokens: 1, output_tokens: 0}]}));
  await h.module.loadOverview();
  const rows = [...h.get("topClientsBody").innerHTML.matchAll(/data-top-client="([^"]+)"/g)].map(m => m[1]);
  assert.equal(rows.length, 10);
  assert.deepEqual(rows.slice(0, 2), ["c11", "c10"]);
  assert.match(h.get("topClientsBody").innerHTML, /data-top-client="c11">.*16\.7%/);
  assert.match(h.get("topModelsBody").innerHTML, /data-top-model="m-no-cost">.*不可用/);
  assert.match(h.get("topClientsStatus").textContent, /前 10/);
  assert.match(h.get("topClientsStatus").textContent, /12/);
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 2, estimated_cost: 0}), by_client: [{key: "free", ...aggTotals({requests: 2, estimated_cost: 0})}]}));
  await h.module.loadOverview(true);
  assert.match(h.get("topClientsBody").innerHTML, /data-top-client="free">.*\$0\.00.*—/);
}));

test("the selected range is remembered and unusable storage falls back to today", () => withTZ("Asia/Shanghai", async () => {
  const saved = [];
  const storage = {getItem: () => "last_month", setItem: (key, value) => saved.push([key, value])};
  const ranges = rangeKeys.map(rangeButton);
  const h = harness({rangeButtons: ranges, storage});
  h.setNow(new Date(2026, 9, 26, 11));
  await h.module.loadOverview();
  assert.equal(ranges[4].attrs["aria-pressed"], "true");
  const url = new URL(h.calls.find(c => c.url.includes("usage/aggregate")).url, "http://local");
  assert.equal(url.searchParams.get("from"), new Date(2026, 8, 1).toISOString());
  await ranges[2].fire("click");
  assert.equal(saved.at(-1)[1], "7d");
  const broken = {getItem() { throw new Error("denied"); }, setItem() { throw new Error("denied"); }};
  const b = harness({rangeButtons: rangeKeys.map(rangeButton), storage: broken});
  await b.module.loadOverview();
  assert.equal(b.doc.querySelectorAll("[data-overview-range]")[0].attrs["aria-pressed"], "true");
  await b.doc.querySelectorAll("[data-overview-range]")[1].fire("click");
  assert.equal(b.doc.querySelectorAll("[data-overview-range]")[1].attrs["aria-pressed"], "true");
  const stale = harness({rangeButtons: rangeKeys.map(rangeButton), storage: {getItem: () => "1h", setItem() {}}});
  await stale.module.loadOverview();
  assert.equal(stale.doc.querySelectorAll("[data-overview-range]")[0].attrs["aria-pressed"], "true", "an obsolete saved key falls back to today");
}));

test("live status cards merge the former chain summary and show the 72h success rate", async () => {
  const h = harness();
  h.setOverview({
    client_count: 3,
    targets: [
      {Name: "healthy", Phase: "ready", TotalKeys: 2, ActiveKeys: 2, RateLimitedKeys: 0, BlockedKeys: 0, ExhaustedSubscriptionKeys: 0, ExhaustedAPIKeys: 0, OtherExhaustedKeys: 0},
      {Name: "limited", Phase: "ready", TotalKeys: 2, ActiveKeys: 1, RateLimitedKeys: 1},
    ],
    request_stats_72h: completeStats({total: 8, success: 6, failure: 2, success_rate: 75}),
    recent_audit: [],
  });
  h.setMetrics(completeMetrics());
  await h.module.loadOverview();
  assert.equal(h.get("activityActiveRequests").textContent, "2");
  assert.equal(h.get("activityWebsocketSessions").textContent, "3");
  assert.equal(h.get("chainTargets").textContent, "1 / 2");
  assert.equal(h.get("chainKeys").textContent, "3 / 4");
  assert.match(h.get("chainKeysDetail").textContent, /限流 1/);
  assert.equal(h.get("ovLiveSuccessRate").textContent, "75%");
  assert.match(h.get("ovLiveSuccessDetail").textContent, /8/);
  const missing = harness();
  missing.setOverview({});
  await missing.module.loadOverview();
  assert.equal(missing.get("ovLiveSuccessRate").textContent, "不可用");
  assert.equal(missing.get("chainTargets").textContent, "不可用");
});

test("trend charts redraw on resize and keep readable axes at mobile width", () => withTZ("Asia/Shanghai", async () => {
  const h = harness();
  for (const id of ["ovTokenTrend", "ovBarTrend"]) h.get(id).getBoundingClientRect = () => ({left: 0, top: 0, width: 300, height: 195});
  h.setNow(new Date(2026, 9, 26, 11));
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 1}), buckets: [hourBucket(new Date(2026, 9, 26, 3), {input_tokens: 9000000})]}));
  await h.module.loadOverview();
  assert.equal(h.get("ovTokenTrend").attrs.viewBox, "0 0 300 195");
  assert.match(h.get("ovTokenTrend").innerHTML, /M<\/text>/);
  const labels = [...h.get("ovBarTrend").innerHTML.matchAll(/class="activity-axis"[^>]*text-anchor="(?:start|middle|end)">(\d\d:\d\d)<\/text>/g)].map(m => m[1]);
  assert.ok(labels.length >= 2 && labels.length <= 4, `x labels ${labels.join(",")}`);
  await h.get("ovBarTrend").fire("pointermove", {clientX: 299});
  assert.match(h.get("ovBarTrendReadout").textContent, /11:00/);
}));

test("leaving and returning keeps the last range and metrics values until fresh data arrives", () => withTZ("Asia/Shanghai", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setOverview({client_count: 1, targets: [], recent_audit: []});
  h.setMetrics(completeMetrics({requests: {total: 7}}));
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 4, estimated_cost: 2}), by_client: [{key: "old-client", ...aggTotals({requests: 4, estimated_cost: 2})}], by_model: []}));
  h.module.startOverviewAutoRefresh();
  await h.module.loadOverview();
  assert.equal(h.get("ovRangeRequests").textContent, "4");
  assert.match(h.get("chainProxy").textContent, /累计 7/);
  h.doc.hidden = true; h.doc.listeners.visibilitychange();
  const pending = [];
  h.setAggregateHandler(() => new Promise(resolve => pending.push(resolve)));
  h.setHandler(url => url.endsWith("/admin/metrics") ? new Promise(() => {}) : url.endsWith("activity") ? Promise.resolve(activity()) : url.endsWith("overview") ? Promise.resolve({client_count: 1, targets: [], recent_audit: []}) : new Promise(resolve => pending.push(resolve)));
  h.doc.hidden = false; h.doc.listeners.visibilitychange();
  await h.advance(0);
  assert.equal(pending.length, 1, "returning forces a fresh aggregate request");
  assert.equal(h.get("ovRangeRequests").textContent, "4", "old card value stays while the new request is in flight");
  assert.equal(h.get("ovRangeCost").textContent, "$2.00");
  assert.match(h.get("topClientsBody").innerHTML, /data-top-client="old-client"/);
  assert.doesNotMatch(h.get("topClientsStatus").textContent, /正在加载|不可用/);
  assert.notEqual(h.get("ovRangeStatus").dataset.state, "error");
  assert.match(h.get("chainProxy").textContent, /累计 7/, "metrics stay visible while the refetch is pending");
  pending[0](aggregateResult({totals: aggTotals({requests: 6}), by_client: [{key: "new-client", ...aggTotals({requests: 6})}], by_model: []}));
  await h.advance(0);
  assert.equal(h.get("ovRangeRequests").textContent, "6");
  assert.match(h.get("topClientsBody").innerHTML, /new-client/);
  h.module.stopOverviewAutoRefresh();
}));

test("overview requests rely only on the page deadline, not the core api default timeout", async () => {
  const h = harness();
  await h.module.loadOverview(true);
  for (const url of ["/admin/api/activity", "/admin/api/overview", "/admin/metrics", "/admin/data/usage/aggregate"]) {
    const call = h.calls.find(c => c.url.startsWith(url));
    assert.ok(call, url);
    assert.equal(call.opts.timeoutMs, 0, url + " disables the core api timeout");
    assert.ok(call.opts.signal, url + " keeps the page abort signal");
  }
});

test("client activity table spans the table grid and keeps its scroll width at every viewport", () => {
  const html = fs.readFileSync(path.join(__dirname, "..", "index.html"), "utf8");
  const start = html.indexOf("    #page-overview { --activity-secondary");
  const css = html.slice(start, html.indexOf("</style>", start));
  // Rules outside any @media block apply at every width.
  const base = css.replace(/@media[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}/g, "");
  assert.match(base, /#page-overview \.overview-tables > \.activity-clients \{[^}]*grid-column:1 \/ -1/);
  assert.match(base, /#page-overview \.activity-client-table \{[^}]*min-width:580px/);
  assert.match(base, /#page-overview \.activity-client-table td:first-child \{[^}]*position:sticky/);
  const twoColumns = css.match(/@media\(min-width:(\d+)px\) \{\s*#page-overview \.overview-tables \{ grid-template-columns:repeat\(2/);
  assert.ok(twoColumns, "two-column table grid breakpoint exists");
  assert.equal(Number(twoColumns[1]), 1280);
});

test("table panels keep DOM order equal to visual order and leave no grid holes", () => {
  const html = fs.readFileSync(path.join(__dirname, "..", "index.html"), "utf8");
  const start = html.indexOf("    #page-overview { --activity-secondary");
  const css = html.slice(start, html.indexOf("</style>", start));
  assert.doesNotMatch(css, /grid-auto-flow:[^;}]*dense/, "dense packing would reorder panels visually but not for Tab or screen readers");
  const grid = html.slice(html.indexOf('<div class="overview-tables">'), html.indexOf('<details class="activity-history">'));
  const panels = [...grid.matchAll(/<section class="overview-panel([^"]*)"(?: id="([^"]+)")?/g)].map(m => m[2] || m[1].trim());
  assert.deepEqual(panels, ["topClients", "topModels", "targetHealth", "overviewAttention", "recentChanges", "activity-clients"]);
  // Five panels precede the full-width activity table; the fifth spans two columns,
  // which completes the last row in both the two-column and three-column grids.
  const multi = css.match(/@media\(min-width:1280px\) \{([^}]*\}[^}]*)\}/);
  assert.ok(multi, "multi-column media block exists");
  assert.match(multi[1], /#page-overview \.overview-tables > #recentChanges \{ grid-column:span 2;/);
});

test("after crossing local midnight a failed refresh keeps the snapshot's own window and says so", () => withTZ("Asia/Shanghai", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setNow(new Date(2026, 9, 25, 23, 50));
  h.setAggregate(aggregateResult({
    totals: aggTotals({requests: 400, estimated_cost: 8}),
    buckets: [hourBucket(new Date(2026, 9, 25, 22), {requests: 400, estimated_cost: 8})],
    by_client: [{key: "night-client", ...aggTotals({requests: 400, estimated_cost: 8})}],
    by_model: [],
  }));
  await h.module.loadOverview();
  assert.match(h.get("ovRangeWindow").textContent, /10-25 00:00 – 10-25 23:50/);
  h.setNow(new Date(2026, 9, 26, 8, 0));
  const pending = [];
  h.setAggregateHandler(() => new Promise((resolve, reject) => pending.push({resolve, reject})));
  const refresh = h.module.loadOverview(true);
  assert.match(h.get("ovRangeWindow").textContent, /10-25 00:00 – 10-25 23:50/, "while the new request is in flight the note still describes the shown data");
  pending[0].reject(new Error("offline"));
  await refresh; await h.advance(0);
  const note = h.get("ovRangeWindow").textContent;
  assert.match(note, /10-25 00:00 – 10-25 23:50/, "note describes the snapshot, not the new day");
  assert.doesNotMatch(note, /10-26 00:00/);
  assert.match(note, /更新失败/);
  assert.equal(h.get("ovRangeRequests").textContent, "400");
  assert.match(h.get("ovRangeStatus").textContent, /更新失败，显示 10-25 00:00 – 10-25 23:50 的上次数据/);
  assert.match(h.get("topClientsStatus").textContent, /10-25 00:00 – 10-25 23:50 的上次数据/);
  assert.match(h.get("ovTrendStatus").textContent, /10-25 00:00 – 10-25 23:50 的上次数据/);
  // The trend axis stays on the snapshot's own day: 24 hourly slots of 10-25, none of 10-26.
  assert.match(h.get("ovBarTrend").getAttribute("aria-label"), /共 24 个时间桶/);
  await h.get("ovBarTrend").fire("keydown", {key: "End"});
  assert.match(h.get("ovBarTrendReadout").textContent, /^10-25 23:00/);
  await h.get("ovBarTrend").fire("keydown", {key: "Home"});
  assert.match(h.get("ovBarTrendReadout").textContent, /^10-25 00:00/);
  // Success switches to the new window.
  h.setAggregateHandler(null);
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 3})}));
  await h.module.loadOverview(true);
  assert.match(h.get("ovRangeWindow").textContent, /10-26 00:00 – 10-26 08:00/);
  assert.doesNotMatch(h.get("ovRangeWindow").textContent, /更新失败/);
  assert.equal(h.get("ovRangeRequests").textContent, "3");
  assert.match(h.get("ovBarTrend").getAttribute("aria-label"), /共 9 个时间桶/);
}));

test("a failed refresh within the same window keeps the plain stale wording", () => withTZ("Asia/Shanghai", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setNow(new Date(2026, 9, 26, 8, 0));
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 2, incomplete_usage_count: 1})}));
  await h.module.loadOverview();
  assert.match(h.get("ovRangeWindow").textContent, /10-26 00:00 – 10-26 08:00（截至当前）/);
  assert.match(h.get("overviewAttentionList").innerHTML, /当前区间有 1 条用量记录不完整</);
  h.setNow(new Date(2026, 9, 26, 9, 30));
  h.setAggregateHandler(() => Promise.reject(new Error("offline")));
  await h.module.loadOverview(true);
  assert.match(h.get("ovRangeStatus").textContent, /更新失败，显示上次数据/);
  assert.doesNotMatch(h.get("ovRangeWindow").textContent, /更新失败/);
  // The shown data stops at the snapshot's end, so the note must not claim "up to now".
  assert.match(h.get("ovRangeWindow").textContent, /10-26 00:00 – 10-26 08:00（截至 08:00）/);
  assert.doesNotMatch(h.get("ovRangeWindow").textContent, /截至当前/);
  assert.match(h.get("overviewAttentionList").innerHTML, /当前区间有 1 条用量记录不完整（更新失败，基于 10-26 00:00 – 10-26 08:00 的上次数据）/);
}));

test("attention marks incomplete usage from a snapshot kept across midnight", () => withTZ("Asia/Shanghai", async () => {
  const h = harness({rangeButtons: rangeKeys.map(rangeButton)});
  h.setNow(new Date(2026, 9, 25, 23, 50));
  h.setAggregate(aggregateResult({totals: aggTotals({requests: 400, incomplete_usage_count: 2, incomplete_cost_count: 1})}));
  await h.module.loadOverview();
  h.setNow(new Date(2026, 9, 26, 8, 0));
  h.setAggregateHandler(() => new Promise((resolve, reject) => setTimeout(() => reject(Object.assign(new Error("timeout"), {name: "TimeoutError"})), 0)));
  const refresh = h.module.loadOverview(true);
  await h.advance(0); await refresh; await h.advance(0);
  const attention = h.get("overviewAttentionList").innerHTML;
  assert.match(attention, /当前区间有 2 条用量记录不完整（更新超时，基于 10-25 00:00 – 10-25 23:50 的上次数据）/);
  assert.match(attention, /当前区间有 1 条成本覆盖不完整（更新超时，基于 10-25 00:00 – 10-25 23:50 的上次数据）/);
  assert.match(h.get("ovRangeWindow").textContent, /（截至 23:50）/);
}));
