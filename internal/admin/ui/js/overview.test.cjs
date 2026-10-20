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
function harness({sortButtons = [], infoButtons = []} = {}) {
  const elements = new Map(); const get = id => { if (!elements.has(id)) elements.set(id, new Element(id === "activityClientsBody" ? "tbody" : "div")); return elements.get(id); };
  const doc = {getElementById: get, querySelectorAll: selector => selector === "[data-activity-sort]" ? sortButtons : selector === "[data-activity-info]" ? infoButtons : [], documentElement: {clientWidth: 390, clientHeight: 844}, createElement: tag => new Element(tag), listeners: {}, hidden: false, activeElement: null, addEventListener(name, fn) { this.listeners[name] = fn; }};
  let module; let answer = activity(); let overview = {}; let handler; const calls = []; const timers = new Map(); let nextTimer = 0; let now = Date.now();
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
  const core = {esc: v => String(v).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;"), fmtNum: v => String(v || 0), fmtMoney: String, fmtPct: v => v + "%", totalTok: d => (d.input_tokens || 0) + (d.output_tokens || 0), api: async (url, opts) => { calls.push({url, opts}); if (handler) return handler(url, opts); return url.endsWith("activity") ? answer : overview; }};
  const state = {currentPage: "overview"};
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "overview.js"), "utf8"), {document: doc, AbortController, Date: TestDate, Map, setTimeout: (fn, delay) => {timers.set(++nextTimer, {fn, at: now + delay}); return nextTimer;}, clearTimeout: id => timers.delete(id), setInterval: () => 1, clearInterval() {}, AdminApp: {register: (name, factory) => {module = factory({state, bind: (m, n) => core[n]}); }}});
  return {module, get, doc, state, calls, timers, advance, setActivity: a => answer = a, setOverview: o => overview = o, setHandler: h => handler = h};
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

test("concurrency lines break at unknown samples instead of connecting across gaps", async () => {
  const h = harness(); const data = activity();
  data.series[0].active_requests_peak = 3; data.series[1].active_requests_peak = null; data.series[2].active_requests_peak = 5;
  h.setActivity(data); await h.module.loadOverview();
  const svg = h.get("activityConcurrencyChart").innerHTML;
  assert.equal((svg.match(/<circle class="activity-concurrency"/g) || []).length, 2);
  assert.doesNotMatch(svg, /<polyline class="activity-line activity-concurrency"/);
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
