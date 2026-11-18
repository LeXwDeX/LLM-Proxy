"use strict";
// Issue 210: the clients list searches over its displayed fields and pages in the browser.
// The render order is fixed: filter -> sort the whole match set -> slice the current page.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");
const {fakeClock} = require("./fake-clock.cjs");

const tick = async (n = 10) => { for (let i = 0; i < n; i++) await new Promise((resolve) => setImmediate(resolve)); };

// c0001 .. cNNNN, with the displayed fields the search is required to cover.
function clients(count) {
  return Array.from({length: count}, (_, i) => ({
    name: "c" + String(i + 1).padStart(4, "0"),
    access_key: "key-" + String(i + 1).padStart(4, "0") + "-zzzz",
    ip_address: "10.0.0." + ((i % 200) + 1),
    job_level: "L" + ((i % 3) + 1),
    allowed_targets: [],
    allowed_pools: [],
  }));
}
const names = (h) => [...h.ids.get("clientsTableBody").innerHTML.matchAll(/<tr><td>(\w+)<\/td>/g)].map((m) => m[1]);
const pageNum = (h) => h.ids.get("clientsPageNum").textContent;

// A click on a sortable <th> delivered to the clients header's listeners, as table-sort-pages.test.cjs does.
function sortBy(h, key) {
  const th = {getAttribute: (name) => (name === "data-sort" ? key : null)};
  const event = {target: {dataset: {}, closest: (selector) => (selector === "th[data-sort]" ? th : null)}};
  for (const listener of h.ids.get("clientsTableHead").listeners.click || []) listener(event);
}
async function boot(options = {}) {
  const h = createHarness(options);
  const app = await h.run();
  return {h, app};
}
function seed(app, list, usage = {}) {
  app.state.clients = list;
  app.state.clientUsage = usage;
  app.state.clientUsageStatus = "ready";
}
function type(h, value) {
  const input = h.ids.get("clientsSearch");
  input.value = value;
  for (const listener of input.listeners.input || []) listener();
}
// Count table-body writes without letting the getter lie to the page's dedupe cache.
function watchBody(h) {
  const body = h.ids.get("clientsTableBody");
  const counter = {writes: 0, html: body.innerHTML};
  Object.defineProperty(body, "innerHTML", {
    configurable: true,
    get: () => counter.html,
    set: (value) => { counter.writes++; counter.html = value; },
  });
  return counter;
}

test("search matches name, IP, masked key and job level case-insensitively, once per debounce", async () => {
  const clock = fakeClock();
  const {h, app} = await boot({timers: clock});
  seed(app, [
    {name: "Alpha", ip_address: "10.1.2.3", access_key: "sk-abcd-1234-WXYZ", job_level: "L3"},
    {name: "Beta", ip_address: "10.9.9.9", access_key: "zzzz-0000-1111-ABCD", job_level: "L1"},
  ]);
  app.modules.clients.renderClients();
  const body = watchBody(h);

  type(h, "alpha");
  type(h, "BETA"); // the burst restarts the timer: only the last value renders
  assert.equal(body.writes, 0, "typing alone does not render");
  await clock.advance(200);
  assert.equal(body.writes, 1, "one render for the whole burst");
  assert.deepEqual(names(h), ["Beta"]);

  type(h, "10.1.2.3");
  await clock.advance(200);
  assert.deepEqual(names(h), ["Alpha"], "IP address matches");

  type(h, "wxyz");
  await clock.advance(200);
  assert.deepEqual(names(h), ["Alpha"], "the masked Access Key matches, case-insensitively");

  type(h, "l3");
  await clock.advance(200);
  assert.deepEqual(names(h), ["Alpha"], "job level matches");

  type(h, "");
  await clock.advance(200);
  assert.deepEqual(names(h), ["Alpha", "Beta"], "clearing the box restores every client");
});

test("2000 clients render one page and sorting orders the whole match set before slicing", async () => {
  const {h, app} = await boot();
  seed(app, clients(2000));
  app.modules.clients.renderClients();
  assert.equal(names(h).length, 50, "DOM rows never exceed the page size");
  assert.equal(pageNum(h), "1 / 40");
  assert.equal(names(h)[0], "c0001", "default order: name ascending");

  sortBy(h, "name");
  assert.equal(names(h)[0], "c0001", "ascending");
  sortBy(h, "name");
  assert.deepEqual(names(h).slice(0, 3), ["c2000", "c1999", "c1998"],
    "descending starts at the end of all 2000 rows, not of the first page");
  assert.equal(pageNum(h), "1 / 40");
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "2 / 40");
  assert.equal(names(h)[0], "c1950", "the next page continues the sorted whole list");
});

test("search and sort return to the first page, and the page clamps at both boundaries", async () => {
  const clock = fakeClock();
  const {h, app} = await boot({timers: clock});
  seed(app, clients(120));
  app.modules.clients.renderClients();
  assert.equal(pageNum(h), "1 / 3");

  h.ids.get("clientsNextBtn").listeners.click[0]();
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "3 / 3");
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "3 / 3", "next stops at the last page");
  sortBy(h, "name");
  assert.equal(pageNum(h), "1 / 3", "changing the sort returns to page 1");

  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "2 / 3");
  type(h, "c01");
  await clock.advance(200);
  assert.equal(pageNum(h), "1 / 1", "searching returns to page 1 (c0100..c0120 match)");
  assert.equal(names(h)[0], "c0100");

  type(h, "");
  await clock.advance(200);
  h.ids.get("clientsNextBtn").listeners.click[0]();
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "3 / 3");
  app.state.clients = clients(40);
  app.modules.clients.renderClients();
  assert.equal(pageNum(h), "1 / 1", "a shrunken list clamps the page to the last existing one");
});

test("saving keeps the current search, sort and page", async () => {
  const clock = fakeClock();
  const list = clients(60);
  const {h, app} = await boot({
    timers: clock,
    routes: {"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []}, "/admin/data/clients": {clients: list}},
  });
  await app.modules.clients.loadClients();
  await tick();
  type(h, "c0");
  await clock.advance(200);
  sortBy(h, "name");
  assert.equal(pageNum(h), "1 / 2");
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "2 / 2");
  assert.deepEqual(names(h).slice(0, 2), ["c0051", "c0052"]);

  const editing = app.state.clients[0];
  await app.modules.clients.openClientModal(editing);
  h.ids.get("clientName").value = editing.name;
  h.ids.get("clientAccessKey").value = editing.access_key;
  await h.ids.get("saveClientBtn").listeners.click[0]();
  await tick();
  assert.equal(h.ids.get("clientsSearch").value, "c0", "the search term survives the save");
  assert.equal(pageNum(h), "2 / 2", "the page survives the save");
  assert.deepEqual(names(h).slice(0, 2), ["c0051", "c0052"], "the chosen order survives the save");
});

test("deleting the last row of a page falls back to the previous page", async () => {
  const list = clients(51);
  const {h, app} = await boot({
    routes: {"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []}, "/admin/data/clients": {clients: list}},
  });
  await app.modules.clients.loadClients();
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "2 / 2");
  assert.deepEqual(names(h), ["c0051"]);
  const body = h.ids.get("clientsTableBody");
  body.listeners.click[0]({target: {dataset: {delClient: "c0051"}}});
  await h.ids.get("confirmOkBtn").listeners.click[0]();
  await tick();
  assert.equal(pageNum(h), "1 / 1", "the emptied page falls back");
  assert.equal(names(h).length, 50);
  assert.doesNotMatch(body.innerHTML, /c0051/);
});

test("readonly users can search and page while row writes stay hidden", async () => {
  const clock = fakeClock();
  const list = clients(60);
  const {h, app} = await boot({
    role: "readonly",
    timers: clock,
    routes: {"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []}, "/admin/data/clients": {clients: list}},
  });
  await app.modules.clients.loadClients();
  assert.equal(app.modules.users.isReadonlyMode(), true);
  assert.equal(h.ids.get("clientsSearch").classList.contains("readonly-hide"), false, "search stays available");
  assert.equal(h.ids.get("clientsNextBtn").classList.contains("readonly-hide"), false, "paging stays available");
  assert.match(h.ids.get("clientsTableBody").innerHTML,
    /class='btn btn-sm readonly-hide' data-edit-client='c0001'/);
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "2 / 2");
  type(h, "c0060");
  await clock.advance(200);
  assert.deepEqual(names(h), ["c0060"]);
});

test("a search without matches says so, and clearing it restores every client", async () => {
  const clock = fakeClock();
  const {h, app} = await boot({timers: clock});
  seed(app, clients(2));
  app.modules.clients.renderClients();
  type(h, "no-such-client");
  await clock.advance(200);
  assert.match(h.ids.get("clientsTableBody").innerHTML, /无匹配客户端/);
  assert.doesNotMatch(h.ids.get("clientsTableBody").innerHTML, /暂无客户端/);
  assert.equal(pageNum(h), "1 / 1");
  assert.equal(h.ids.get("clientsPrevBtn").disabled, true);
  assert.equal(h.ids.get("clientsNextBtn").disabled, true);
  type(h, "");
  await clock.advance(200);
  assert.deepEqual(names(h), ["c0001", "c0002"]);
  assert.doesNotMatch(h.ids.get("clientsTableBody").innerHTML, /无匹配客户端/);
});

test("the page-size selector re-pages from the first page", async () => {
  const {h, app} = await boot();
  seed(app, clients(120));
  app.modules.clients.renderClients();
  assert.equal(names(h).length, 50, "the default page size is 50");
  assert.equal(pageNum(h), "1 / 3");
  h.ids.get("clientsNextBtn").listeners.click[0]();
  assert.equal(pageNum(h), "2 / 3");
  const size = h.ids.get("clientsPageSize");
  size.value = "20";
  for (const listener of size.listeners.change || []) listener();
  assert.equal(pageNum(h), "1 / 6", "20 per page and back to the first page");
  assert.equal(names(h).length, 20);
  size.value = "200";
  for (const listener of size.listeners.change || []) listener();
  assert.equal(pageNum(h), "1 / 1");
  assert.equal(names(h).length, 120);
});