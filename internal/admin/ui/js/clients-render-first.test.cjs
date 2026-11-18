"use strict";
// Issue #208: the clients list renders as soon as GET /admin/data/clients returns; the slow
// 30-day by-client summary fills only the usage columns, can be aborted, and never blocks editing.
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
const usageSummaryUrls = h => h.requests.map(r => r.url).filter(u => String(u).startsWith("/admin/data/usage/by-client"));
const listUrls = h => h.requests.filter(r => r.url === "/admin/data/clients" && !r.options?.method);
const isUsageSummary = url => String(url).startsWith("/admin/data/usage/by-client");
const pickerValues = (element, attr) =>
  element.children.filter(child => child.dataset[attr]).map(child => child.dataset[attr]);
const pickerText = (element, attr) =>
  element.children.filter(child => child.dataset[attr]).map(child => child.textContent).join(" ");
const toasts = h => h.ids.get("toastContainer").children.map(c => c.textContent);
const lastToast = h => toasts(h).at(-1);
const writeCount = (h, method) => h.requests.filter(r => r.options?.method === method).length;

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

test("the clients list renders before a pending 30-day usage summary resolves", async () => {
  let release = null;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => isUsageSummary(url)
        ? new Promise(resolve => { release = () => resolve(json({clients: []})); })
        : fallback(url, options),
    },
  });
  const load = app.modules.clients.loadClients();
  await tick();
  assert.match(h.ids.get("clientsTableBody").innerHTML, /c1/, "the list does not wait for usage");
  assert.match(h.ids.get("clientsTableBody").innerHTML, /加载中/, "usage columns show the placeholder");
  assert.match(h.ids.get("clientsUsageStatusText").textContent, /加载中/);
  assert.equal(app.state.clientUsageStatus, "pending");
  assert.equal(await settled(load), true, "loadClients settles while the usage summary is still pending");
  release();
  await tick();
  assert.equal(app.state.clientUsageStatus, "ready");
  assert.equal(h.ids.get("clientsUsageStatus").style.display, "none", "the status bar hides once usage arrives");
});

test("a failed usage summary keeps the list editable and shows the failure placeholder with a retry", async () => {
  let usageUp = false;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => isUsageSummary(url) && !usageUp
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
  const usageSummaryBefore = usageSummaryUrls(h).length;
  usageUp = true;
  h.ids.get("retryClientsUsageBtn").listeners.click[0]();
  await tick();
  assert.equal(app.state.clientUsageStatus, "ready");
  assert.equal(usageSummaryUrls(h).length, usageSummaryBefore + 1, "retry resends only the usage summary");
  assert.equal(listUrls(h).length, listBefore, "retry does not reload the client list");
});

test("usage summary completion replaces only the usage columns", async () => {
  let release = null;
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => isUsageSummary(url)
        ? new Promise(resolve => { release = () => resolve(json({
          clients: [{key: "c1", input_tokens: 1234, output_tokens: 0, estimated_cost: 5.5}],
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
  assert.match(after, /1,234|\b1234\b|1\.2k|1,234/i, "the token column shows the usage summary");
  assert.match(after, /data-edit-client='c1'/);
  assert.doesNotMatch(after, /加载中/, "the placeholder is gone");
  assert.equal(listUrls(h).length, 1, "usage never refetches the list");
});

test("a second loadClients aborts the previous usage summary silently", async () => {
  const summaries = [];
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => {
        if (!isUsageSummary(url)) return fallback(url, options);
        return new Promise((resolve, reject) => {
          summaries.push({options, resolve});
          options.signal.addEventListener("abort", () => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          });
        });
      },
    },
  });
  summaries.length = 0;
  h.requests.length = 0;
  await app.modules.clients.loadClients();
  assert.equal(summaries.length, 1);
  await app.modules.clients.loadClients();
  assert.equal(summaries.length, 2);
  assert.equal(summaries[0].options.signal.aborted, true, "the superseded usage summary is cancelled");
  summaries[1].resolve(json({clients: [{key: "c1", input_tokens: 7}]}));
  await tick();
  assert.equal(app.state.clientUsageStatus, "ready");
  assert.equal(app.state.clientUsage.c1.input_tokens, 7);
  const toasts = h.ids.get("toastContainer").children.map(c => c.textContent).filter(t => /失败/.test(t));
  assert.deepEqual(toasts, [], "AbortError is not reported as a failure");
});

test("leaving the page aborts the pending usage summary", async () => {
  const summaries = [];
  const {h, app} = await boot({
    harness: {
      fetch: (url, options, fallback) => {
        if (!isUsageSummary(url)) return fallback(url, options);
        return new Promise((resolve, reject) => {
          summaries.push({options, resolve});
          options.signal.addEventListener("abort", () => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          });
        });
      },
    },
  });
  summaries.length = 0;
  h.requests.length = 0;
  await app.modules.clients.loadClients();
  assert.equal(summaries.length, 1);
  for (const listener of h.windowListeners.pagehide || []) listener();
  assert.equal(summaries[0].options.signal.aborted, true);
  assert.equal(app.state.clientUsageStatus, "pending");
});

test("saving fetches the client list once, never the usage summary, and repaints once", async () => {
  const {h, app} = await boot({
    routes: routes([{name: "c1", access_key: "k1", allowed_targets: [], allowed_pools: []}]),
  });
  await app.modules.clients.loadClients();
  await tick();
  await app.modules.clients.openClientModal(app.state.clients[0]);
  field(h, "clientName").value = "c1";
  field(h, "clientAccessKey").value = "k1";
  const listBefore = listUrls(h).length;
  const usageSummaryBefore = usageSummaryUrls(h).length;
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
  assert.equal(usageSummaryUrls(h).length, usageSummaryBefore, "saving never requests the usage summary");
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
      fetch: (url, options, fallback) => isUsageSummary(url)
        ? Promise.resolve(json({clients: byClient}))
        : fallback(url, options),
    },
  });
  await app.modules.clients.loadClients();
  await tick();
  assert.equal(app.state.clientUsage.c1.input_tokens, 5);
  const listBefore = listUrls(h).length;
  const usageSummaryBefore = usageSummaryUrls(h).length;
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
  assert.equal(usageSummaryUrls(h).length, usageSummaryBefore, "delete does not request the usage summary");
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
  app.state.clientListStatus = "ready";
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

// Issue 220: a list GET that failed is a state of its own. It must never read as an empty
// list, and the pagination must not invent a total.
test("a failed client list GET shows a retry row instead of an empty list", async () => {
  let listDown = true;
  const {h, app} = await boot({
    routes: routes([]),
    harness: {
      fetch: (url, options, fallback) => url === "/admin/data/clients" && !options?.method && listDown
        ? Promise.resolve(failure(503, "list offline"))
        : fallback(url, options),
    },
  });
  await app.modules.clients.loadClients();
  await tick();
  const body = h.ids.get("clientsTableBody").innerHTML;
  assert.match(body, /加载客户端失败，请重试/);
  assert.doesNotMatch(body, /暂无客户端/);
  assert.match(h.ids.get("clientsPageInfo").textContent, /未知/);
  assert.equal(h.ids.get("clientsPageNum").textContent, "0 / 0");
  assert.equal(h.ids.get("clientsPrevBtn").disabled, true);
  assert.equal(h.ids.get("clientsNextBtn").disabled, true);

  listDown = false;
  await app.modules.clients.loadClients();
  await tick();
  assert.match(h.ids.get("clientsTableBody").innerHTML, /暂无客户端/,
    "only a successful load may show the empty state");
  assert.equal(h.ids.get("clientsPageNum").textContent, "1 / 1");
});

// Issue 220: a create whose target/pool roster is still loading must not send an empty ACL
// (the server reads that as "allow every target").
test("a create waits for the target and pool roster before saving", async () => {
  let releaseTargets = null;
  const {h, app} = await boot({
    routes: routes([]),
    harness: {
      fetch: (url, options, fallback) => url === "/admin/data/targets"
        ? new Promise(resolve => { releaseTargets = () => resolve(json({targets: [{name: "t1"}]})); })
        : fallback(url, options),
    },
  });
  const opening = app.modules.clients.openClientModal(null);
  field(h, "clientName").value = "newc";
  field(h, "clientAccessKey").value = "nk";
  await field(h, "saveClientBtn").listeners.click[0]();
  assert.equal(writeCount(h, "POST"), 0, "no create while the picker data is pending");
  assert.equal(lastToast(h), "目标/部署池/职级数据加载中，请稍后再保存");
  assert.equal(field(h, "clientModal").classList.contains("open"), true);

  releaseTargets();
  await opening;
  await field(h, "saveClientBtn").listeners.click[0]();
  assert.equal(writeCount(h, "POST"), 1, "the create goes through once the data has arrived");
  assert.equal(field(h, "clientModal").classList.contains("open"), false);
});

test("a create is blocked when the dictionaries failed to load", async () => {
  const {h, app} = await boot({
    routes: routes([]),
    harness: {
      fetch: (url, options, fallback) => url === "/admin/data/job-levels"
        ? Promise.resolve(failure(503, "dicts offline"))
        : fallback(url, options),
    },
  });
  await app.modules.clients.openClientModal(null);
  field(h, "clientName").value = "newc";
  field(h, "clientAccessKey").value = "nk";
  await field(h, "saveClientBtn").listeners.click[0]();
  assert.equal(writeCount(h, "POST"), 0);
  assert.equal(lastToast(h), "目标/部署池/职级数据加载中，请稍后再保存");
  assert.ok(toasts(h).some(t => /加载客户端编辑数据失败/.test(t)), "the failed load is reported");
  assert.match(field(h, "clientJobLevel").innerHTML, /职级加载失败/);
  assert.match(field(h, "clientProjectGroup").innerHTML, /项目组加载失败/);
});

// Issue 220: while the dictionaries reload, the editor shows the loading state instead of
// cached options that the fresh list may already have removed.
test("a dictionary reload hides stale options until the fresh list lands", async () => {
  let releaseDicts = null;
  const {h, app} = await boot({
    routes: routes([]),
    harness: {
      fetch: (url, options, fallback) => url === "/admin/data/job-levels"
        ? new Promise(resolve => { releaseDicts = () => resolve(json({job_levels: ["L9"]})); })
        : fallback(url, options),
    },
  });
  app.state.jobLevels = ["L1"];
  app.state.projectGroups = ["g1"];
  app.state.dictsLoaded = false;
  const opening = app.modules.clients.openClientModal(null);
  assert.match(field(h, "clientJobLevel").innerHTML, /加载中/);
  assert.doesNotMatch(field(h, "clientJobLevel").innerHTML, /L1/);
  releaseDicts();
  await opening;
  assert.match(field(h, "clientJobLevel").innerHTML, /L9/);
  assert.doesNotMatch(field(h, "clientProjectGroup").innerHTML, /加载中/,
    "the settled dictionary state replaces the loading label");
  assert.doesNotMatch(field(h, "clientJobLevel").innerHTML, /加载中/);
});

// Issue 220: an edit never has to trust an empty picker: it falls back to the saved record, so a
// failed background refresh cannot silently widen the ACL or clear the saved job level.
test("editing keeps the saved ACL and job level when the background data fails", async () => {
  const saved = {
    name: "c1",
    access_key: "k1",
    job_level: "L2",
    project_group: "g1",
    allowed_targets: ["saved-t"],
    allowed_pools: ["saved-p"],
  };
  let failEditorData = false;
  const {h, app} = await boot({
    routes: routes([saved]),
    harness: {
      fetch: (url, options, fallback) => {
        if (failEditorData && (
          url === "/admin/data/targets" ||
          url === "/admin/data/job-levels" ||
          url === "/admin/data/project-groups"
        )) return Promise.resolve(failure(503, "offline"));
        return fallback(url, options);
      },
    },
  });
  await app.modules.clients.loadClients();
  await tick();
  field(h, "clientTargetTags").querySelectorAll = () => [];
  field(h, "clientPoolTags").querySelectorAll = () => [];
  failEditorData = true;
  await app.modules.clients.openClientModal(app.state.clients[0]);
  await field(h, "saveClientBtn").listeners.click[0]();
  const put = h.requests.find(r => r.options?.method === "PUT" && r.url === "/admin/data/clients/c1");
  assert.ok(put, "an edit still saves with the saved record as its fallback");
  const payload = JSON.parse(put.options.body);
  assert.deepEqual(payload.allowed_targets, ["saved-t"]);
  assert.deepEqual(payload.allowed_pools, ["saved-p"]);
  assert.equal(payload.job_level, "L2");
  assert.equal(payload.project_group, "g1");
});