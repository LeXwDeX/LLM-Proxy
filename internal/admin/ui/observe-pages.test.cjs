"use strict";
// Issue #97: trace paging/filtering/races, audit detail and catalog robustness.
const test = require("node:test");
const assert = require("node:assert/strict");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

function json(data) { return {ok: true, status: 200, text: async () => JSON.stringify(data)}; }
function rec(id, client, code = 200, extra = {}) {
  return {trace_id: id, timestamp: "2026-10-29T01:00:00Z", client_name: client, target: "t-" + client, model: "m", status_code: code, duration_ms: 5, input_tokens: 1, output_tokens: 2, ...extra};
}
function ids(html) { return [...html.matchAll(/data-trace-id='([^']*)'/g)].map(m => m[1]); }
const tick = () => new Promise(resolve => setImmediate(resolve));
async function settle() { for (let i = 0; i < 8; i++) await tick(); }
function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return {promise, resolve}; }

async function traceHarness(pages) {
  const requests = [];
  const h = createHarness({fetch: async (url, options, fallback) => {
    if (url.startsWith("/admin/data/trace?")) { requests.push(url); const page = pages(url); return page instanceof Promise ? page : json(page); }
    return fallback(url, options);
  }});
  const app = await h.run();
  const trace = app.modules.audit_trace;
  const select = (id, value) => { h.document.getElementById(id).value = value; };
  return {h, app, trace, requests, select, body: () => h.document.getElementById("traceTableBody").innerHTML};
}

test("trace list requests the body-free projection and pages by the server's effective limit", async () => {
  const t = await traceHarness(url => {
    const offset = Number((/offset=(\d+)/.exec(url) || [0, 0])[1]);
    return {records: [rec("a" + offset, "alice")], count: 1, limit: 1, offset, stats: {}, enabled: true};
  });
  t.select("traceLimit", "99999");
  await t.trace.checkTraceEnabled().catch(() => {});
  t.requests.length = 0;
  t.app.state.traceEnabledForTest = true;
  await t.trace.loadTrace();
  assert.ok(t.requests[0], "list requested");
  assert.match(t.requests[0], /view=summary/, "list never ships bodies");
  await t.h.document.getElementById("traceLoadMore").listeners.click[0]();
  assert.match(t.requests[1], /offset=1(&|$)/, "offset advances by the server's effective limit, not the typed 99999");
});

test("trace filters apply to appended pages and empty placeholders never mix with rows", async () => {
  const t = await traceHarness(url => {
    const offset = Number((/offset=(\d+)/.exec(url) || [0, 0])[1]);
    return offset === 0
      ? {records: [rec("b1", "bob"), rec("b2", "bob")], count: 2, limit: 2, offset, stats: {}, enabled: true}
      : {records: [rec("b3", "bob"), rec("a1", "alice", 502)], count: 2, limit: 2, offset, stats: {}, enabled: true};
  });
  await t.trace.checkTraceEnabled().catch(() => {});
  t.select("traceLimit", "2");
  await t.trace.loadTrace();
  t.select("traceClientFilter", "alice");
  await t.trace.loadTrace();
  assert.match(t.body(), /empty-row/, "no matching rows on page one shows the placeholder");
  assert.equal(t.h.document.getElementById("traceLoadMore").style.display, "", "more pages remain, so loading continues");
  await t.h.document.getElementById("traceLoadMore").listeners.click[0]();
  assert.deepEqual(ids(t.body()), ["a1"], "only the matching record of the appended page is shown");
  assert.doesNotMatch(t.body(), /empty-row/, "the placeholder is removed once rows arrive");
  t.select("traceStatusFilter", "5xx");
  assert.deepEqual(t.trace.filterTraceRecords([rec("x", "alice", 200), rec("y", "alice", 503), rec("z", "bob", 503)]).map(r => r.trace_id), ["y"]);
});

test("stale trace loads and detail views cannot overwrite newer ones", async () => {
  const slow = deferred();
  let calls = 0;
  const t = await traceHarness(url => {
    if (!url.includes("offset")) return {records: [], stats: {}, enabled: true}; // enablement probe
    calls++;
    if (calls === 1) return slow.promise.then(json);
    return {records: [rec("new", "alice")], count: 1, limit: 50, offset: 0, stats: {}, enabled: true};
  });
  await t.trace.checkTraceEnabled();
  const first = t.trace.loadTrace();
  await t.trace.loadTrace();
  slow.resolve({records: [rec("old", "bob")], count: 1, limit: 50, offset: 0, stats: {}, enabled: true});
  await first; await settle();
  assert.deepEqual(ids(t.body()), ["new"], "the older response is dropped");

  const gates = {};
  const h = createHarness({fetch: async (url, options, fallback) => {
    if (url.startsWith("/admin/data/trace/")) { const id = decodeURIComponent(url.split("/").pop()); gates[id] = deferred(); return gates[id].promise.then(json); }
    return fallback(url, options);
  }});
  const app = await h.run();
  const a = app.modules.audit_trace.showTraceDetail("A");
  const b = app.modules.audit_trace.showTraceDetail("B");
  gates.B.resolve({trace_id: "B", timestamp: "2026-10-29T01:00:00Z", request_body: "{}", response_body: ""});
  await b; await settle();
  gates.A.resolve({trace_id: "A", timestamp: "2026-10-29T01:00:00Z", request_body: "{}", response_body: ""});
  await a; await settle();
  const body = h.document.getElementById("traceDetailBody").innerHTML;
  assert.match(body, />B</, "the latest detail stays");
  assert.doesNotMatch(body, />A</);
});

test("braille spinner never leaks intervals and detail labels count real bytes", async () => {
  const h = createHarness();
  const live = new Set(); let next = 0;
  h.context.setInterval = () => { live.add(++next); return next; };
  h.context.clearInterval = id => { live.delete(id); };
  const app = await h.run();
  const trace = app.modules.audit_trace;
  const baseline = live.size; // intervals owned by the shell, not the spinner
  trace.startBrailleSpinner("a"); trace.startBrailleSpinner("b"); trace.stopBrailleSpinner();
  assert.equal(live.size, baseline, "starting twice must not orphan the first interval");

  const h2 = createHarness({fetch: async (url, options, fallback) => url.startsWith("/admin/data/trace/") ? json({trace_id: "Z", timestamp: "2026-10-29T01:00:00Z", request_body: "请求", response_body: "ok"}) : fallback(url, options)});
  const app2 = await h2.run();
  await app2.modules.audit_trace.showTraceDetail("Z");
  const html = h2.document.getElementById("traceDetailBody").innerHTML;
  assert.match(html, /Request Body \(6 bytes\)/, "two CJK characters are six UTF-8 bytes");
  assert.match(html, /Response Body \(2 bytes\)/);
});

test("trace probe honours enabled=false from the server", async () => {
  const t = await traceHarness(() => ({records: [], count: 0, limit: 1, offset: 0, stats: {total_records: 0}, enabled: false}));
  await t.trace.checkTraceEnabled();
  assert.equal(t.h.document.getElementById("nav-trace").style.display, "none", "a disabled trace store keeps the nav entry hidden");
});

test("audit rows expose the full detail and loads do not overlap", async () => {
  let calls = 0; const gate = deferred();
  const h = createHarness({fetch: async (url, options, fallback) => {
    if (url.startsWith("/admin/data/audit")) { calls++; return gate.promise.then(json); }
    return fallback(url, options);
  }});
  const app = await h.run();
  const audit = app.modules.audit_trace;
  const first = audit.loadAudit(); const second = audit.loadAudit();
  assert.equal(calls, 1, "a refresh while one is in flight reuses it");
  gate.resolve({events: [{timestamp: "2026-10-29T01:00:00Z", actor: "a", action: "x", object: "o", result: "ok", detail: "x".repeat(300) + "<b>"}]});
  await Promise.all([first, second]);
  const html = h.document.getElementById("auditTableBody").innerHTML;
  assert.match(html, /title='x{300}&lt;b&gt;'/, "the cell title carries the whole detail, escaped");
  const click = h.document.getElementById("auditTableBody").listeners.click?.[0];
  assert.ok(click, "detail cells are expandable");
  const cell = {dataset: {}, style: {}};
  click({target: {closest: () => cell}});
  assert.equal(cell.dataset.expanded, "true");
  assert.equal(cell.style.whiteSpace, "normal");
  click({target: {closest: () => cell}});
  assert.equal(cell.dataset.expanded, "false");
});

test("catalog escapes capability labels, surfaces load errors and has an honest empty range", async () => {
  const h = createHarness({fetch: async (url, options, fallback) => {
    if (url === "/admin/data/catalog") return json({models: [{endpoint_type: "openai", model: "m", capabilities: ["<img src=x onerror=alert(1)>"]}]});
    if (url.startsWith("/admin/data/catalog/")) return {ok: false, status: 503, text: async () => JSON.stringify({error: "catalog offline"})};
    return fallback(url, options);
  }});
  const app = await h.run();
  const catalog = app.modules.catalog;
  await catalog.loadModelCatalog();
  const rows = h.document.getElementById("catalogTableBody").innerHTML;
  assert.doesNotMatch(rows, /<img/, "capability text is escaped");
  assert.match(rows, /&lt;img/);
  const models = await catalog.loadCatalog("openai", {force: true});
  assert.equal(models.length, 0);
  const toasts = h.document.getElementById("toastContainer").children.map(c => c.textContent).join("|");
  assert.match(toasts, /模型目录/);
  h.document.getElementById("catalogSearch").value = "no-such-model";
  catalog.applyCatalogFilter();
  assert.doesNotMatch(h.document.getElementById("catalogPageInfo").textContent, /1~0/);
  assert.match(h.document.getElementById("catalogPageInfo").textContent, /共 0 个模型/);
});
