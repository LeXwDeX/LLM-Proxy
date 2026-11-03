"use strict";
// Issue #152: save locks must not wait for the slow 30-day aggregate, aggregate windows are
// minute-aligned (so the backend report cache can hit), and target pause/delete dedupe per row.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const tick = async (n = 10) => { for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve)); };
const field = (h, id) => h.ids.get(id);
const aggregateUrls = h => h.requests.map(r => r.url).filter(u => String(u).startsWith("/admin/data/usage/aggregate"));
// Holds every aggregate request until released.
function holdAggregate(h, body = {by_client: []}) {
  const base = h.context.fetch;
  const held = [];
  h.context.fetch = (url, options) => String(url).startsWith("/admin/data/usage/aggregate")
    ? new Promise(resolve => { h.requests.push({url, options}); held.push(() => resolve(json(body))); })
    : base(url, options);
  return {held, releaseAll: () => held.splice(0).forEach(r => r())};
}
const settled = async promise => { let done = false; promise.then(() => { done = true; }, () => { done = true; }); await tick(); return done; };

const quotaRoutes = () => ({
  "/admin/data/targets": {targets: []},
  "/admin/data/catalog": {models: []},
  "/admin/data/model-groups": {model_groups: []},
  "/admin/data/job-levels": {job_levels: []},
  "/admin/data/project-groups": {project_groups: []},
  "/admin/data/quota/summary": {clients: [{client: {name: "c1"}, status: {groups: {}}}]},
});

test("A2: loadQuotaManagement resolves before the aggregate; quotaUsageRefresh fills usage", async () => {
  const h = createHarness({routes: quotaRoutes()});
  const app = await h.run();
  const hold = holdAggregate(h, {by_client: [{key: "c1", input_tokens: 77}]});
  const load = app.modules.quota.loadQuotaManagement();
  assert.equal(await settled(load), true, "load must not wait for the 30-day aggregate");
  assert.equal(app.state.clientUsage30dPending, true);
  const shared = app.modules.quotaModel.shared;
  assert.ok(shared.quotaUsageRefresh && typeof shared.quotaUsageRefresh.then === "function");
  hold.releaseAll();
  await shared.quotaUsageRefresh;
  assert.equal(app.state.clientUsage30dPending, false);
  assert.equal(app.state.clientUsage30d.c1.input_tokens, 77);
});

test("A2: a superseded load never applies its stale usage", async () => {
  const h = createHarness({routes: quotaRoutes()});
  const app = await h.run();
  const hold = holdAggregate(h, {by_client: [{key: "c1", input_tokens: 1}]});
  const shared = app.modules.quotaModel.shared;
  await app.modules.quota.loadQuotaManagement();
  const first = shared.quotaUsageRefresh;
  await app.modules.quota.loadQuotaManagement();
  hold.releaseAll();
  await first;
  await shared.quotaUsageRefresh;
  assert.equal(app.state.clientUsage30dPending, false);
  assert.deepEqual(Object.keys(app.state.clientUsage30d), ["c1"]);
});

async function clientsFixture() {
  const clients = [{name: "c1", access_key: "k1", allowed_targets: [], allowed_pools: []}];
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []}, "/admin/data/clients": {clients}}});
  const app = await h.run();
  return {h, app, clients};
}
function fillClient(h) {
  field(h, "clientName").value = "c1";
  field(h, "clientAccessKey").value = "k1";
}

test("A3: client save releases its lock without waiting for the aggregate", async () => {
  const {h, app} = await clientsFixture();
  await app.modules.clients.loadClients();
  const hold = holdAggregate(h);
  await app.modules.clients.openClientModal(app.state.clients[0]);
  fillClient(h);
  const save = field(h, "saveClientBtn").listeners.click[0]();
  assert.equal(await settled(save), true, "save must not wait for the aggregate");
  const before = h.requests.filter(r => r.options?.method === "PUT").length;
  await app.modules.clients.openClientModal(app.state.clients[0]);
  fillClient(h);
  await field(h, "saveClientBtn").listeners.click[0]();
  assert.equal(h.requests.filter(r => r.options?.method === "PUT").length, before + 1, "second save is not blocked by 'saving'");
  hold.releaseAll();
  await tick();
});

test("A3: client save refreshes the list from the server (lightweight) before releasing", async () => {
  const {h, app} = await clientsFixture();
  await app.modules.clients.loadClients();
  holdAggregate(h);
  h.requests.length = 0;
  await app.modules.clients.openClientModal(app.state.clients[0]);
  fillClient(h);
  await field(h, "saveClientBtn").listeners.click[0]();
  const urls = h.requests.map(r => r.url);
  assert.ok(urls.indexOf("/admin/data/clients") > urls.findIndex((u, i) => h.requests[i].options?.method === "PUT"));
});

test("A11: aggregate windows are floored to the minute", async () => {
  const {h, app} = await clientsFixture();
  h.requests.length = 0;
  await app.modules.clients.loadClients();
  const urls = aggregateUrls(h);
  assert.equal(urls.length, 1);
  const p = new URLSearchParams(urls[0].split("?")[1]);
  for (const k of ["from", "to"]) {
    const d = new Date(p.get(k));
    assert.equal(d.getUTCSeconds() + d.getUTCMilliseconds(), 0, k + " must be minute aligned: " + p.get(k));
  }
});

test("A11: quota aggregate window is minute aligned too", async () => {
  const h = createHarness({routes: quotaRoutes()});
  const app = await h.run();
  h.requests.length = 0;
  await app.modules.quota.loadQuotaManagement();
  const p = new URLSearchParams(aggregateUrls(h)[0].split("?")[1]);
  for (const k of ["from", "to"]) {
    const d = new Date(p.get(k));
    assert.equal(d.getUTCSeconds() + d.getUTCMilliseconds(), 0, k);
  }
});

test("A11: open overview ranges floor 'to' to the minute", async () => {
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}}});
  const app = await h.run();
  const range = app.modules.overviewRange;
  const now = new Date(2026, 5, 15, 10, 20, 33, 456);
  const path = range.aggregatePath(range.rangeWindow("today", now));
  const to = new Date(new URLSearchParams(path.split("?")[1]).get("to"));
  assert.equal(to.getUTCSeconds() + to.getUTCMilliseconds(), 0);
  assert.equal(to.getTime(), new Date(2026, 5, 15, 10, 20).getTime());
});

test("A13: target pause and delete dedupe per row", async () => {
  const target = {name: "t1", endpoint_type: "openai", etag: "t1-abc", model_mappings: [{upstream: "m"}]};
  const h = createHarness({routes: {"/admin/data/targets": {targets: [target]}}});
  const app = await h.run();
  await app.modules.targets.loadTargets();
  const base = h.context.fetch;
  const pending = [];
  h.context.fetch = (url, options) => options?.method === "PUT" || options?.method === "DELETE"
    ? new Promise(resolve => { h.requests.push({url, options}); pending.push(() => resolve(json({}))); })
    : base(url, options);
  const body = field(h, "targetsTableBody");
  for (const [dataset, method] of [[{pauseTgt: "t1"}, "PUT"], [{delTgt: "t1"}, "DELETE"]]) {
    body.listeners.click.forEach(l => l({target: {dataset, closest: () => null}}));
    field(h, "confirmOkBtn").listeners.click[0]();
    body.listeners.click.forEach(l => l({target: {dataset, closest: () => null}}));
    field(h, "confirmOkBtn").listeners.click[0]();
    await tick();
    assert.equal(h.requests.filter(r => r.options?.method === method).length, 1, method + " must be sent once");
    pending.splice(0).forEach(r => r());
    await tick();
  }
});
