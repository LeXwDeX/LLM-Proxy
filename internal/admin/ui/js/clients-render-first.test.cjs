"use strict";
// Issue #208: the clients list renders as soon as GET /admin/data/clients returns; the slow
// 30-day aggregate fills only the usage columns, can be aborted, and never blocks editing.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const failure = (status = 503, message = "offline") => ({
  ok: false, status, text: async () => JSON.stringify({error: message}),
});
const tick = async (n = 10) => { for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve)); };
const settled = async promise => { let done = false; promise.then(() => { done = true; }, () => { done = true; }); await tick(); return done; };
const field = (h, id) => h.ids.get(id);
const aggregateUrls = h => h.requests.map(r => r.url).filter(u => String(u).startsWith("/admin/data/usage/aggregate"));
const listUrls = h => h.requests.filter(r => r.url === "/admin/data/clients" && !r.options?.method);
const isAggregate = url => String(url).startsWith("/admin/data/usage/aggregate");
const pickerValues = (element, attr) =>
  element.children.filter(child => child.dataset[attr]).map(child => child.dataset[attr]);
const pickerText = (element, attr) =>
  element.children.filter(child => child.dataset[attr]).map(child => child.textContent).join(" ");

function routes(clients = [{name: "c1", access_key: "k1", allowed_targets: [], allowed_pools: []}]) {
  return {
    "/admin/data/targets": {targets: []},
    "/admin/data/pools": {pools: []},
    "/admin/data/clients": {clients},
  };
}

async function boot(options = {}) {
  const h = createHarness({routes: options.routes || routes(), ...(options.harness || {})});
  const app = await h.run();
  return {h, app};
}

function click(h, id, key) {
  const th = {getAttribute: name => (name === "data-sort" ? key : null)};
  const event = {target: {dataset: {}, closest: selector => (selector === "th[data-sort]" ? th : null)}};
  for (const listener of h.ids.get(id).listeners.click || []) listener(event);
}
const names = h => [...h.ids.get("clientsTableBody").innerHTML.matchAll(/<tr><td>(\w+)<\/td>/g)].map(m => m[1]);

test("the clients list renders before a pending 30-day aggregate resolves", async () => {
  let release = null;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => isAggregate(url)
        ? new Promise(resolve => { release = () => resolve(json({by_client: []})); })
        : fallback(url, options),
    },
  });
  const load = app.modules.clients.loadClients();
  await tick();
  assert.match(h.ids.get("clientsTableBody").innerHTML, /c1/, "the list does not wait for usage");
  assert.match(h.ids.get("clientsTableBody").innerHTML, /加载中/, "usage columns show the placeholder");
  assert.match(h.ids.get("clientsUsageStatusText").textContent, /加载中/);
  assert.equal(app.state.clientUsageStatus, "pending");
  assert.equal(await settled(load), true, "loadClients settles while the aggregate is still pending");
  release();
  await tick();
  assert.equal(app.state.clientUsageStatus, "ready");
  assert.equal(h.ids.get("clientsUsageStatus").style.display, "none", "the status bar hides once usage arrives");
});

test("a failed aggregate keeps the list editable and shows the failure placeholder with a retry", async () => {
  let usageUp = false;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => isAggregate(url) && !usageUp
        ? Promise.resolve(failure())
        : fallback(url, options),
    },
  });
  await app.modules.clients.loadClients();
  await tick();
  const body = h.ids.get("clientsTableBody").innerHTML;
  assert.match(body, /c1/, "the list stays usable");
  assert.match(body, /data-edit-client='c1'/);
  assert.match(body, /data-del-client='c1'/);
  assert.match(body, /加载失败/, "both usage columns say the load failed");
  assert.equal(app.state.clientUsageStatus, "error");
  assert.match(h.ids.get("clientsUsageStatusText").textContent, /失败/);
  assert.notEqual(h.ids.get("retryClientsUsageBtn").style.display, "none");
  click(h, "clientsTableHead", "cost");
  assert.deepEqual(names(h), ["c1"], "sorting in a non-ready state does not break the rows");

  const listBefore = listUrls(h).length;
  const aggregateBefore = aggregateUrls(h).length;
  usageUp = true;
  h.ids.get("retryClientsUsageBtn").listeners.click[0]();
  await tick();
  assert.equal(app.state.clientUsageStatus, "ready");
  assert.equal(aggregateUrls(h).length, aggregateBefore + 1, "retry resends only the aggregate");
  assert.equal(listUrls(h).length, listBefore, "retry does not reload the client list");
});

test("aggregate completion replaces only the usage columns", async () => {
  let release = null;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => isAggregate(url)
        ? new Promise(resolve => { release = () => resolve(json({
          by_client: [{key: "c1", input_tokens: 1234, output_tokens: 0, estimated_cost: 5.5}],
        })); })
        : fallback(url, options),
    },
  });
  await app.modules.clients.loadClients();
  await tick();
  const before = h.ids.get("clientsTableBody").innerHTML;
  release();
  await tick();
  const after = h.ids.get("clientsTableBody").innerHTML;
  assert.equal(app.state.clientUsage.c1.input_tokens, 1234);
  assert.notEqual(after, before);
  assert.match(after, /1,234|\b1234\b|1\.2k|1,234/i, "the token column shows the aggregate");
  assert.match(after, /data-edit-client='c1'/);
  assert.doesNotMatch(after, /加载中/, "the placeholder is gone");
  assert.equal(listUrls(h).length, 1, "usage never refetches the list");
});

test("a second loadClients aborts the previous aggregate silently", async () => {
  const aggregates = [];
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => {
        if (!isAggregate(url)) return fallback(url, options);
        return new Promise((resolve, reject) => {
          aggregates.push({options, resolve});
          options.signal.addEventListener("abort", () => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          });
        });
      },
    },
  });
  aggregates.length = 0;
  h.requests.length = 0;
  await app.modules.clients.loadClients();
  assert.equal(aggregates.length, 1);
  await app.modules.clients.loadClients();
  assert.equal(aggregates.length, 2);
  assert.equal(aggregates[0].options.signal.aborted, true, "the superseded aggregate is cancelled");
  aggregates[1].resolve(json({by_client: [{key: "c1", input_tokens: 7}]}));
  await tick();
  assert.equal(app.state.clientUsageStatus, "ready");
  assert.equal(app.state.clientUsage.c1.input_tokens, 7);
  const toasts = h.ids.get("toastContainer").children.map(c => c.textContent).filter(t => /失败/.test(t));
  assert.deepEqual(toasts, [], "AbortError is not reported as a failure");
});

test("leaving the page aborts the pending aggregate", async () => {
  const aggregates = [];
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => {
        if (!isAggregate(url)) return fallback(url, options);
        return new Promise((resolve, reject) => {
          aggregates.push({options, resolve});
          options.signal.addEventListener("abort", () => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          });
        });
      },
    },
  });
  aggregates.length = 0;
  h.requests.length = 0;
  await app.modules.clients.loadClients();
  assert.equal(aggregates.length, 1);
  for (const listener of h.windowListeners.pagehide || []) listener();
  assert.equal(aggregates[0].options.signal.aborted, true);
  assert.equal(app.state.clientUsageStatus, "pending");
});

test("saving fetches the client list once, never the aggregate, and repaints once", async () => {
  const {h, app} = await boot({
    routes: routes([{name: "c1", access_key: "k1", allowed_targets: [], allowed_pools: []}]),
  });
  await app.modules.clients.loadClients();
  await tick();
  await app.modules.clients.openClientModal(app.state.clients[0]);
  field(h, "clientName").value = "c1";
  field(h, "clientAccessKey").value = "k1";
  const listBefore = listUrls(h).length;
  const aggregateBefore = aggregateUrls(h).length;
  const body = h.ids.get("clientsTableBody");
  let writes = 0;
  let html = body.innerHTML;
  Object.defineProperty(body, "innerHTML", {
    configurable: true,
    get: () => html,
    set: value => { writes++; html = value; },
  });
  // The server-normalized record differs from the optimistic draft, so the one repaint is visible.
  h.setRoute("/admin/data/clients", {
    clients: [{name: "c1", access_key: "k1", ip_address: "10.0.0.2", allowed_targets: [], allowed_pools: []}],
  });
  await field(h, "saveClientBtn").listeners.click[0]();
  assert.equal(listUrls(h).length, listBefore + 1, "exactly one list GET after saving");
  assert.equal(aggregateUrls(h).length, aggregateBefore, "saving never requests the aggregate");
  assert.equal(writes, 1, "the table repaints at most once");
  assert.match(html, /10\.0\.0\.2/);
});

test("deleting removes the client locally with one repaint and no requests", async () => {
  const byClient = [{key: "c1", input_tokens: 5}, {key: "c2", input_tokens: 6}];
  const {h, app} = await boot({
    routes: routes([
      {name: "c1", access_key: "k1", allowed_targets: [], allowed_pools: []},
      {name: "c2", access_key: "k2", allowed_targets: [], allowed_pools: []},
    ]),
    harness: {
      fetch: (url, options, fallback) => isAggregate(url)
        ? Promise.resolve(json({by_client: byClient}))
        : fallback(url, options),
    },
  });
  await app.modules.clients.loadClients();
  await tick();
  assert.equal(app.state.clientUsage.c1.input_tokens, 5);
  const listBefore = listUrls(h).length;
  const aggregateBefore = aggregateUrls(h).length;
  const body = h.ids.get("clientsTableBody");
  let writes = 0;
  let html = body.innerHTML;
  Object.defineProperty(body, "innerHTML", {
    configurable: true,
    get: () => html,
    set: value => { writes++; html = value; },
  });
  body.listeners.click[0]({target: {dataset: {delClient: "c1"}}});
  await h.ids.get("confirmOkBtn").listeners.click[0]();
  await tick();
  assert.equal(writes, 1, "one repaint after deleting");
  assert.doesNotMatch(html, /c1/);
  assert.match(html, /c2/);
  assert.equal(Object.prototype.hasOwnProperty.call(app.state.clientUsage, "c1"), false,
    "the deleted client's usage entry is dropped too");
  assert.equal(listUrls(h).length, listBefore, "delete does not reload the list");
  assert.equal(aggregateUrls(h).length, aggregateBefore, "delete does not request the aggregate");
});

test("the client modal opens immediately while targets are still loading", async () => {
  let release = null;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => url === "/admin/data/targets"
        ? new Promise(resolve => { release = () => resolve(json({targets: [{name: "t1", endpoint_type: "provider-new"}]})); })
        : fallback(url, options),
    },
  });
  const opening = app.modules.clients.openClientModal(null);
  assert.equal(h.ids.get("clientModal").classList.contains("open"), true, "the modal opens before any data arrives");
  assert.match(h.ids.get("clientTargetTags").innerHTML, /加载中/);
  assert.match(h.ids.get("clientPoolTags").innerHTML, /加载中/);
  assert.match(h.ids.get("clientJobLevel").innerHTML, /加载中/);
  release();
  await opening;
  assert.deepEqual(pickerValues(h.ids.get("clientTargetTags"), "target"), ["t1"]);
  assert.doesNotMatch(h.ids.get("clientJobLevel").innerHTML, /加载中/);
});

test("background picker data never overrides tags the user already clicked", async () => {
  let release = null;
  let holdTargets = false;
  const {h, app} = await boot({
    routes: routes([{name: "c1", access_key: "k1"}]),
    harness: {
      fetch: (url, options, fallback) => url === "/admin/data/targets" && holdTargets
        ? new Promise(resolve => { release = () => resolve(json({targets: [{name: "t2", endpoint_type: "provider-new"}]})); })
        : fallback(url, options),
    },
  });
  await app.modules.targets.loadTargets();
  app.state.targets = [{name: "t1", endpoint_type: "provider-new"}];
  app.state.pools = [{name: "p1"}];
  app.modules.clients.renderClients();
  holdTargets = true;
  const opening = app.modules.clients.openClientModal(null);
  assert.deepEqual(pickerValues(h.ids.get("clientTargetTags"), "target"), ["t1"],
    "existing targets are shown at once");
  const targetTag = h.ids.get("clientTargetTags").children.find(el => el.dataset.target === "t1");
  targetTag.listeners.click[0].call(targetTag);
  h.ids.get("clientModal").listeners.click[0]({target: targetTag});
  assert.equal(targetTag.dataset.selected, "1");
  release();
  await opening;
  assert.deepEqual(pickerValues(h.ids.get("clientTargetTags"), "target"), ["t1"],
    "the background refresh keeps the clicked selection");
  assert.equal(app.modules.clients.getSelectedClientTargets().join(","), "t1");
});

test("identical client markup is not written to the table twice", async () => {
  const {h, app} = await boot();
  const body = h.ids.get("clientsTableBody");
  let writes = 0;
  let html = body.innerHTML;
  Object.defineProperty(body, "innerHTML", {
    configurable: true,
    get: () => html,
    set: value => { writes++; html = value; },
  });
  app.state.clients = [{name: "c1", access_key: "k1"}];
  app.state.clientUsageStatus = "ready";
  app.state.clientUsage = {};
  app.modules.clients.renderClients();
  app.modules.clients.renderClients();
  assert.equal(writes, 1, "unchanged markup is skipped");
  app.state.clients = [];
  app.modules.clients.renderClients();
  app.modules.clients.renderClients();
  assert.equal(writes, 2, "the empty state goes through the same dedupe");
  assert.match(html, /暂无客户端/);
});