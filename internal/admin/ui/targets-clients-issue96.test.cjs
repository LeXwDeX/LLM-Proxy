"use strict";
// Issue #96: targets / pools / clients page defects found by the frontend audit.
// Every test fails against the pre-fix pages for the reason it names.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");
const { createHarness, withETag } = require("../../../scripts/testing/admin_ui_test.cjs");

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const plain = (value) => JSON.parse(JSON.stringify(value)); // values built inside the vm context
const reply = (body) => ({ ok: true, status: 200, text: async () => JSON.stringify(body) });

async function setup(options = {}) {
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [] }, "/admin/data/pools": { pools: [] }, ...(options.routes || {}) },
    ...(options.harness || {}),
  });
  const app = await harness.run();
  return { harness, app, el: (id) => harness.ids.get(id) };
}
const saved = (extra = {}) => withETag({
  name: "saved", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", usage_adapter: "standard", ...extra,
}, 1);

test("P1-1: opening the target modal needs no catalog download and no await", async () => {
  const { harness, app, el } = await setup();
  harness.requests.length = 0;
  const opening = app.modules.targets.openTargetModal(null);
  assert.equal(el("targetModal").classList.contains("open"), true, "modal opens before any await");
  await opening;
  assert.deepEqual(harness.requests.filter((r) => r.url === "/admin/data/catalog"), [], "no full-catalog fetch");
});

test("P1-2: the client modal opens without the catalog and loads targets once", async () => {
  const { harness, app, el } = await setup();
  harness.setRouteFailure("/admin/data/catalog", true);
  harness.requests.length = 0;
  await app.modules.clients.openClientModal(null);
  assert.equal(el("clientModal").classList.contains("open"), true, "catalog failure cannot block the client modal");
  assert.deepEqual(harness.requests.filter((r) => r.url === "/admin/data/catalog"), []);
  assert.equal(harness.requests.filter((r) => r.url === "/admin/data/targets").length, 1, "one targets refresh");
});

test("P1-3: custom header and body rows keep entity-looking text unchanged", async () => {
  const { harness, app, el } = await setup();
  const document = harness.context.document;
  const original = document.querySelectorAll.bind(document);
  document.querySelectorAll = (selector) =>
    selector === "#custom-headers-container > div" ? el("custom-headers-container").children
      : selector === "#custom-body-container > div" ? el("custom-body-container").children
        : original(selector);
  const targets = app.modules.targets;
  targets.addCustomHeaderRow("X-A&amp;B", 'a&amp;b "q" <i>');
  assert.deepEqual(plain(targets.collectCustomHeaders()), { "X-A&amp;B": 'a&amp;b "q" <i>' });
  targets.addCustomBodyRow("f&amp;g", '"a&amp;b"');
  assert.deepEqual(plain(targets.collectCustomBody()), { "f&amp;g": "a&amp;b" });
});

test("P1-4: a failed model test reports the status_code the server sends", async () => {
  const { harness, app, el } = await setup();
  await app.modules.targets.openTargetModal(saved({ model_mappings: [{ upstream: "m" }] }));
  const row = el("model-mappings-container").children[0];
  const button = row.children.find((child) => child.className?.includes("test-model-btn"));
  harness.context.fetch = async () => reply({ success: false, error: "bad gateway", status_code: 502 });
  app.modules.targets.testModelForRow(button, "m");
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(el("toastContainer").children.at(-1).textContent, /bad gateway.*HTTP 502/);
});

test("P2: a throwing render cannot leave the scheduling refresh unsettled", async () => {
  const { app } = await setup({ routes: { "/admin/data/targets": { targets: [{ name: "a" }] } } });
  app.modules.clients.onSchedulingRefresh = () => { throw new Error("render boom"); };
  await app.modules.targets.loadTargets(true).catch(() => {});
  const outcome = await Promise.race([
    app.modules.targets.awaitSchedulingRefresh().then(() => "settled", () => "settled"),
    sleep(100).then(() => "hung"),
  ]);
  assert.equal(outcome, "settled");
});

test("P2: only the newest model suggestion request may render", async () => {
  const { harness, app, el } = await setup();
  const pending = {};
  harness.context.fetch = (url) => new Promise((resolve) => { pending[url] = resolve; });
  const targets = app.modules.targets;
  const first = targets.renderTgtModelSuggestions("provider-new");
  const second = targets.renderTgtModelSuggestions("provider-other");
  pending["/admin/data/catalog/provider-other"](reply({ models: [{ model: "newest" }] }));
  await second;
  pending["/admin/data/catalog/provider-new"](reply({ models: [{ model: "stale" }] }));
  await first;
  assert.deepEqual(el("tgtModelSuggestions").children.map((c) => c.textContent), ["newest"]);
});

test("P2: health is computed once per target per render", async () => {
  const { app } = await setup();
  const count = 40;
  app.state.targets = Array.from({ length: count }, (_, i) => ({ name: "t" + i, endpoint_type: "provider-new", has_api_key: true, key_statuses: [] }));
  let reads = 0;
  app.state.runtime = new Proxy({}, { get() { reads++; return { phase: "ready" }; } });
  app.modules.targets.renderTargets();
  assert.ok(reads <= 3 * count, `runtime read ${reads} times for ${count} targets`);
});

test("P2: opening the editor renders the key list once", async () => {
  const { app, el } = await setup();
  const list = el("keyCardList");
  let writes = 0, html = "";
  Object.defineProperty(list, "innerHTML", { get: () => html, set: (v) => { writes++; html = v; } });
  await app.modules.targets.openTargetModal(saved());
  assert.equal(writes, 1);
});

test("P2: pool member counts are computed once per render", async () => {
  const { app } = await setup();
  const members = Array.from({ length: 30 }, (_, i) => ({ name: "t" + i, pool: "p" + (i % 20) }));
  app.state.pools = Array.from({ length: 20 }, (_, i) => ({ name: "p" + i }));
  let reads = 0;
  Object.defineProperty(app.state, "targets", { configurable: true, get() { reads++; return members; }, set() {} });
  app.modules.targets.renderTargetPoolOptions("");
  assert.ok(reads <= 2, `target options read state.targets ${reads} times`);
  reads = 0;
  app.modules.clients.renderClientPoolTags([]);
  assert.ok(reads <= 2, `client pool tags read state.targets ${reads} times`);
});

function gatedFetch(harness, matcher, counter) {
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  harness.context.fetch = async (url, options) => {
    if (matcher(url, options)) { counter.n++; await gate; }
    return reply({ pools: [], targets: [], recovered: 0 });
  };
  return release;
}
const clickEvent = (dataset) => ({ target: { dataset, disabled: false, closest: () => null } });
const dispatch = (element, event) => (element.listeners.click || []).forEach((fn) => fn(event));

test("P2: pool save ignores a second click while a save is in flight", async () => {
  const { harness, app, el } = await setup();
  app.modules.pools.openPoolModal(null);
  el("poolName").value = "double";
  const counter = { n: 0 };
  const release = gatedFetch(harness, (url, o) => o?.method === "POST" && url === "/admin/data/pools", counter);
  const first = el("savePoolBtn").listeners.click[0]();
  const second = el("savePoolBtn").listeners.click[0]();
  release();
  await Promise.all([first, second]);
  assert.equal(counter.n, 1);
});

test("P2: resuming a target or a pool twice sends one request", async () => {
  const { harness, app, el } = await setup();
  app.state.targets = [{ name: "t1", etag: "t1-1" }]; // row writes always carry an If-Match
  for (const [bodyId, dataset, suffix] of [
    ["targetsTableBody", { resumeTgt: "t1" }, "/admin/data/targets/t1/resume"],
    ["poolsTableBody", { resumePool: "p1" }, "/admin/data/pools/p1/resume"],
  ]) {
    const counter = { n: 0 };
    const release = gatedFetch(harness, (url, o) => o?.method === "PUT" && url === suffix, counter);
    dispatch(el(bodyId), clickEvent(dataset));
    dispatch(el(bodyId), clickEvent(dataset));
    release();
    await sleep(10);
    assert.equal(counter.n, 1, suffix);
  }
  assert.ok(app);
});

test("P2: wake-up has a finite deadline and cannot run twice at once", async () => {
  const delays = [];
  const { harness, el } = await setup({ harness: { timers: { setTimeout: (fn, ms) => { delays.push(ms); return 1; }, clearTimeout() {} } } });
  const counter = { n: 0 };
  const release = gatedFetch(harness, (url, o) => o?.method === "POST" && url === "/admin/data/targets/t1/keys/wakeup", counter);
  delays.length = 0;
  for (let i = 0; i < 2; i++) {
    dispatch(el("targetsTableBody"), clickEvent({ wakeupTarget: "t1" }));
    el("confirmOkBtn").listeners.click[0]();
    await sleep(0);
  }
  assert.equal(counter.n, 1, "one wake-up in flight");
  assert.ok(delays.some((ms) => Number.isFinite(ms) && ms > 0), `deadline timers: ${delays}`);
  release();
  await sleep(10);
});

test("P2: the clients page does not reload targets only because there are no pools", async () => {
  const { harness, app } = await setup({ routes: { "/admin/data/clients": { clients: [{ name: "c1", access_key: "k" }] } } });
  await app.modules.targets.loadTargets(); // roster available, just empty
  app.state.targets = [{ name: "t1" }];
  app.state.pools = [];
  harness.requests.length = 0;
  await app.modules.clients.loadClients();
  assert.deepEqual(harness.requests.filter((r) => r.url === "/admin/data/targets"), []);
});

test("P2: a failed targets prefetch does not blank the clients table", async () => {
  const { harness, app, el } = await setup({ routes: { "/admin/data/clients": { clients: [{ name: "c1", access_key: "k" }] } } });
  harness.setRouteFailure("/admin/data/targets", true);
  app.state.targets = [];
  await app.modules.clients.loadClients();
  assert.equal(app.state.clients.length, 1);
  assert.match(el("clientsTableBody").innerHTML, /c1/);
});

test("P2: the empty clients row spans every column", async () => {
  const { app, el } = await setup();
  const partial = fs.readFileSync(path.join(__dirname, "partials/pages/clients.html"), "utf8");
  const columns = (partial.match(/<th[\s>]/g) || []).length;
  assert.ok(columns > 5);
  app.state.clients = [];
  app.modules.clients.renderClients();
  assert.match(el("clientsTableBody").innerHTML, new RegExp(`colspan=['"]${columns}['"]`));
});

test("P2: an identical refresh leaves the targets table DOM untouched", async () => {
  const { app, el } = await setup();
  app.state.targets = [{ name: "t1", endpoint_type: "provider-new", has_api_key: true, key_statuses: [] }];
  const body = el("targetsTableBody");
  let writes = 0, html = "";
  Object.defineProperty(body, "innerHTML", { get: () => html, set: (v) => { writes++; html = v; } });
  app.modules.targets.renderTargets();
  app.modules.targets.renderTargets();
  assert.equal(writes, 1, "same markup is not written twice");
  app.state.targets[0].name = "t2";
  app.modules.targets.renderTargets();
  assert.equal(writes, 2, "changed markup is written");
});

test("review: the clients list retries the pools roster when it is unavailable", async () => {
  const { harness, app } = await setup({ routes: { "/admin/data/clients": { clients: [] }, "/admin/data/targets": { targets: [{ name: "t1" }] } } });
  harness.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  assert.equal(app.modules.targets.targetPoolsAvailable(), false);
  harness.setRouteFailure("/admin/data/pools", false);
  harness.requests.length = 0;
  await app.modules.clients.loadClients();
  assert.ok(harness.requests.some((r) => r.url === "/admin/data/pools"), "pools fetched again");
  assert.equal(app.modules.targets.targetPoolsAvailable(), true);
  // isolation: a failing retry must not blank the list
  harness.setRouteFailure("/admin/data/pools", true);
  await app.modules.clients.loadClients();
  assert.ok(Array.isArray(app.state.clients));
});

test("review: a second wake-up says one is running; a timeout explains the server may still probe", async () => {
  const { harness, el } = await setup();
  const counter = { n: 0 };
  const release = gatedFetch(harness, (url, o) => o?.method === "POST" && url === "/admin/data/targets/t1/keys/wakeup", counter);
  const confirm = async () => {
    dispatch(el("targetsTableBody"), clickEvent({ wakeupTarget: "t1" }));
    el("confirmOkBtn").listeners.click[0]();
    await sleep(0);
  };
  await confirm();
  await confirm();
  assert.match(el("toastContainer").children.at(-1).textContent, /唤醒进行中/);
  release();
  await sleep(10);
  harness.context.fetch = async () => {
    const e = new Error("请求超时（600 秒无响应）");
    e.name = "TimeoutError";
    throw e;
  };
  await confirm();
  await sleep(10);
  assert.match(el("toastContainer").children.at(-1).textContent, /服务端.*仍在探测.*进行中/);
});
