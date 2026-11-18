"use strict";
// Issue 211: a client write whose outcome is unknown (timeout, network failure, any 5xx or a
// non-JSON error body) is reconciled by reading the client list back and judging by content.
// A deterministic 4xx with a JSON body stays a red failure and never triggers a readback.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");
const {fakeClock} = require("./fake-clock.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const httpError = (status = 500, message = "server exploded") => ({
  ok: false,
  status,
  text: async () => JSON.stringify({error: message}),
});
const hang = () => new Promise(() => {});
const tick = async (n = 10) => {
  for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve));
};
const field = (h, id) => h.ids.get(id);
const toasts = h => h.ids.get("toastContainer").children.map(c => ({
  msg: c.textContent,
  kind: String(c.className || "").replace(/^toast\s*/, ""),
}));
const lastToast = h => toasts(h).at(-1);
const listGets = h => h.requests.filter(r => r.url === "/admin/data/clients" && !r.options?.method);
const writeCount = (h, method) => h.requests.filter(r => r.options?.method === method).length;
const aggregateCount = h =>
  h.requests.filter(r => String(r.url).startsWith("/admin/data/usage/aggregate")).length;

const routes = (clients = []) => ({
  "/admin/data/targets": {targets: []},
  "/admin/data/pools": {pools: []},
  "/admin/data/clients": {clients},
});
const baseClient = (name = "c1", key = "k1") => ({
  name,
  access_key: key,
  ip_address: "",
  allowed_targets: [],
  allowed_pools: [],
});

async function boot({clients = [baseClient()], timers, fetch} = {}) {
  const h = createHarness({routes: routes(clients), timers, fetch});
  const app = await h.run();
  await app.modules.clients.loadClients();
  await tick();
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  h.requests.length = 0;
  return {h, app};
}

async function openEditor(h, app, client) {
  await app.modules.clients.openClientModal(client);
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
}

function fillForm(h, {name, key, ip}) {
  if (name !== undefined) field(h, "clientName").value = name;
  if (key !== undefined) field(h, "clientAccessKey").value = key;
  if (ip !== undefined) field(h, "clientIPAddress").value = ip;
}

function save(h) {
  return field(h, "saveClientBtn").listeners.click[0]();
}

function deleteRow(h, name) {
  h.ids.get("clientsTableBody").listeners.click[0]({
    target: {dataset: {delClient: name}},
  });
}

function confirmDelete(h) {
  h.ids.get("confirmOkBtn").listeners.click[0]();
}

function countRepaints(body) {
  let writes = 0;
  let html = body.innerHTML;
  Object.defineProperty(body, "innerHTML", {
    configurable: true,
    get: () => html,
    set: value => {
      writes++;
      html = value;
    },
  });
  return () => writes;
}

test("a timed-out PUT that the server applied is confirmed by one readback", async () => {
  const clock = fakeClock();
  const {h, app} = await boot({
    timers: clock,
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "PUT"
        ? hang()
        : fallback(url, options),
  });
  await openEditor(h, app, app.state.clients[0]);
  fillForm(h, {name: "c1", key: "k1", ip: "10.0.0.7"});
  h.setRoute("/admin/data/clients", {
    clients: [{...baseClient(), ip_address: "10.0.0.7"}],
  });
  const writes = countRepaints(h.ids.get("clientsTableBody"));
  const saving = save(h);
  await tick();
  assert.equal(writeCount(h, "PUT"), 1);
  await clock.advance(15000);
  await saving;
  assert.equal(writeCount(h, "PUT"), 1);
  assert.equal(listGets(h).length, 1, "the readback replaces the post-save list GET");
  assert.equal(aggregateCount(h), 0, "a save never requests the 30-day aggregate");
  assert.equal(lastToast(h).kind, "success");
  assert.equal(lastToast(h).msg, "已保存（已回读确认）");
  assert.ok(!toasts(h).some(t => /保存失败/.test(t.msg)), "an unconfirmed outcome is not a failure");
  assert.equal(field(h, "clientModal").classList.contains("open"), false);
  assert.equal(writes(), 1, "the readback list paints exactly once");
  assert.match(h.ids.get("clientsTableBody").innerHTML, /10\.0\.0\.7/);
  assert.equal(app.state.clients.length, 1);
  assert.equal(app.state.clients[0].ip_address, "10.0.0.7");
});

test("a 500 whose server copy matches after normalization counts as saved", async () => {
  const {h, app} = await boot({
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "PUT"
        ? Promise.resolve(httpError())
        : fallback(url, options),
  });
  await openEditor(h, app, app.state.clients[0]);
  field(h, "clientTargetTags").querySelectorAll = () => [
    {dataset: {target: "Alpha", selected: "1"}},
    {dataset: {target: "alpha", selected: "1"}},
    {dataset: {target: " Beta ", selected: "1"}},
  ];
  field(h, "clientPoolTags").querySelectorAll = () => [
    {dataset: {pool: "PoolA", selected: "1"}},
  ];
  fillForm(h, {name: "c1", key: " k1 ", ip: " 10.0.0.9/24 "});
  h.setRoute("/admin/data/clients", {
    clients: [{
      name: " c1 ",
      access_key: "k1",
      ip_address: "10.0.0.0/24",
      allowed_targets: ["alpha", "beta"],
      allowed_pools: ["poola"],
      job_level: " L2 ",
      project_group: " group ",
    }],
  });
  field(h, "clientJobLevel").value = "L2";
  field(h, "clientProjectGroup").value = "group";
  await save(h);
  assert.equal(lastToast(h).kind, "success");
  assert.equal(lastToast(h).msg, "已保存（已回读确认）");
  assert.equal(field(h, "clientModal").classList.contains("open"), false);
  assert.equal(listGets(h).length, 1);
  assert.equal(aggregateCount(h), 0);
});

test("an unconfirmed PUT whose server copy differs keeps the draft and refreshes the table", async () => {
  const {h, app} = await boot({
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "PUT"
        ? Promise.resolve(httpError(502))
        : fallback(url, options),
  });
  await openEditor(h, app, app.state.clients[0]);
  fillForm(h, {name: "c1", key: "k1", ip: "10.0.0.7"});
  h.setRoute("/admin/data/clients", {
    clients: [{...baseClient(), ip_address: "10.9.9.9"}],
  });
  await save(h);
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "保存结果未确认，服务器内容与提交不一致，请刷新后重试");
  assert.equal(field(h, "clientModal").classList.contains("open"), true, "the draft stays open");
  assert.equal(field(h, "clientIPAddress").value, "10.0.0.7", "the form is never replaced by the readback");
  assert.match(h.ids.get("clientsTableBody").innerHTML, /10\.9\.9\.9/, "the table shows the server copy");
  assert.equal(app.state.clients[0].ip_address, "10.9.9.9");
  assert.equal(listGets(h).length, 1);
  assert.equal(writeCount(h, "PUT"), 1);
});

test("an unconfirmed PUT with a failed readback keeps the draft and the list, and says retry", async () => {
  let failReadback = false;
  const {h, app} = await boot({
    fetch: (url, options, fallback) => {
      if (url === "/admin/data/clients/c1" && options?.method === "PUT")
        return Promise.resolve(httpError(500));
      if (url === "/admin/data/clients" && !options?.method && failReadback)
        return Promise.resolve(httpError(503, "list offline"));
      return fallback(url, options);
    },
  });
  await openEditor(h, app, app.state.clients[0]);
  fillForm(h, {name: "c1", key: "k1", ip: "10.0.0.7"});
  const before = h.ids.get("clientsTableBody").innerHTML;
  failReadback = true;
  await save(h);
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "保存结果未确认，可重试");
  assert.equal(field(h, "clientModal").classList.contains("open"), true);
  assert.equal(field(h, "clientIPAddress").value, "10.0.0.7");
  assert.equal(h.ids.get("clientsTableBody").innerHTML, before, "a failed readback never clears the list");
  assert.equal(app.state.clients.length, 1);
  assert.equal(listGets(h).length, 1);
});

test("an unconfirmed POST that the server applied is treated as created", async () => {
  const {h, app} = await boot({
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients" && options?.method === "POST"
        ? Promise.resolve(httpError(500))
        : fallback(url, options),
  });
  await openEditor(h, app, null);
  fillForm(h, {name: "newc", key: "nk", ip: "10.0.0.9"});
  h.setRoute("/admin/data/clients", {
    clients: [baseClient(), {name: "newc", access_key: "nk", ip_address: "10.0.0.9", allowed_targets: [], allowed_pools: []}],
  });
  await save(h);
  assert.equal(writeCount(h, "POST"), 1);
  assert.equal(lastToast(h).kind, "success");
  assert.equal(lastToast(h).msg, "已保存（已回读确认）");
  assert.equal(field(h, "clientModal").classList.contains("open"), false);
  assert.deepEqual(Array.from(app.state.clients, c => c.name), ["c1", "newc"]);
  assert.equal(listGets(h).length, 1);
  assert.equal(aggregateCount(h), 0);
});

// Issue 220: the server may commit the client while its authorization reload is still pending.
// The readback matches, but the write must not report a green "saved" confirmation yet.
test("an unconfirmed save with auth_reload_pending warns instead of confirming", async () => {
  const {h, app} = await boot({
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients" && options?.method === "POST"
        ? Promise.resolve({
          ok: false,
          status: 503,
          text: async () => JSON.stringify({error: "auth reload pending", code: "auth_reload_pending"}),
        })
        : fallback(url, options),
  });
  await openEditor(h, app, null);
  fillForm(h, {name: "newc", key: "nk", ip: "10.0.0.9"});
  h.setRoute("/admin/data/clients", {
    clients: [baseClient(), {name: "newc", access_key: "nk", ip_address: "10.0.0.9", allowed_targets: [], allowed_pools: []}],
  });
  await save(h);
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "数据已保存，但鉴权尚未生效；请稍后刷新或重试确认");
  assert.ok(!toasts(h).some(t => t.msg === "已保存（已回读确认）"),
    "a pending auth reload is never a green confirmation");
  assert.deepEqual(Array.from(app.state.clients, c => c.name), ["c1", "newc"],
    "the applied save is still adopted");
  assert.equal(listGets(h).length, 1, "the readback is the only list GET");
});

test("an unconfirmed POST that never reached the server stays open for a retry", async () => {
  const {h, app} = await boot({
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients" && options?.method === "POST"
        ? Promise.resolve(httpError(500))
        : fallback(url, options),
  });
  await openEditor(h, app, null);
  fillForm(h, {name: "newc", key: "nk", ip: "10.0.0.9"});
  await save(h);
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "保存结果未确认，可重试");
  assert.equal(field(h, "clientModal").classList.contains("open"), true);
  assert.equal(field(h, "clientName").value, "newc", "the draft is kept");
  assert.deepEqual(Array.from(app.state.clients, c => c.name), ["c1"]);
  assert.equal(listGets(h).length, 1);
});

test("a timed-out DELETE that the server applied is confirmed by one readback", async () => {
  const clock = fakeClock();
  const {h, app} = await boot({
    clients: [baseClient("c1"), baseClient("c2", "k2")],
    timers: clock,
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "DELETE"
        ? hang()
        : fallback(url, options),
  });
  app.state.clientUsage = {c1: {input_tokens: 5}, c2: {input_tokens: 6}};
  h.setRoute("/admin/data/clients", {clients: [baseClient("c2", "k2")]});
  const writes = countRepaints(h.ids.get("clientsTableBody"));
  deleteRow(h, "c1");
  confirmDelete(h);
  await tick();
  assert.equal(writeCount(h, "DELETE"), 1);
  await clock.advance(15000);
  await tick();
  assert.equal(writeCount(h, "DELETE"), 1);
  assert.equal(listGets(h).length, 1, "the readback is the only list GET");
  assert.equal(aggregateCount(h), 0);
  assert.equal(lastToast(h).kind, "success");
  assert.equal(lastToast(h).msg, "已删除客户端: c1");
  assert.doesNotMatch(h.ids.get("clientsTableBody").innerHTML, /data-del-client='c1'/);
  assert.equal(Object.prototype.hasOwnProperty.call(app.state.clientUsage, "c1"), false);
  assert.equal(writes(), 1, "the readback list paints exactly once");
});

test("an unconfirmed DELETE that is still on the server refreshes and warns", async () => {
  const {h, app} = await boot({
    clients: [baseClient("c1"), baseClient("c2", "k2")],
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "DELETE"
        ? Promise.resolve(httpError(503))
        : fallback(url, options),
  });
  app.state.clientUsage = {c1: {input_tokens: 5}};
  h.setRoute("/admin/data/clients", {
    clients: [{...baseClient("c1"), ip_address: "10.4.4.4"}, baseClient("c2", "k2")],
  });
  deleteRow(h, "c1");
  confirmDelete(h);
  await tick();
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "删除结果未确认，列表已刷新");
  assert.match(h.ids.get("clientsTableBody").innerHTML, /10\.4\.4\.4/, "the table takes the readback copy");
  assert.equal(app.state.clientUsage.c1.input_tokens, 5, "a client that still exists keeps its usage");
  assert.equal(listGets(h).length, 1);
  assert.equal(writeCount(h, "DELETE"), 1);
});

test("an unconfirmed DELETE with a failed readback keeps the list and says retry", async () => {
  let failReadback = false;
  const {h, app} = await boot({
    clients: [baseClient("c1")],
    fetch: (url, options, fallback) => {
      if (url === "/admin/data/clients/c1" && options?.method === "DELETE")
        return Promise.resolve(httpError(500));
      if (url === "/admin/data/clients" && !options?.method && failReadback)
        return Promise.resolve(httpError(503, "list offline"));
      return fallback(url, options);
    },
  });
  const before = h.ids.get("clientsTableBody").innerHTML;
  failReadback = true;
  deleteRow(h, "c1");
  confirmDelete(h);
  await tick();
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "删除结果未确认，可重试");
  assert.equal(h.ids.get("clientsTableBody").innerHTML, before);
  assert.deepEqual(Array.from(app.state.clients, c => c.name), ["c1"]);
  assert.equal(listGets(h).length, 1);
});

test("a stale readback never overwrites a newer list load", async () => {
  let releaseReadback = null;
  let holdNextList = false;
  const {h, app} = await boot({
    clients: [baseClient()],
    fetch: (url, options, fallback) => {
      if (url === "/admin/data/clients/c1" && options?.method === "PUT")
        return Promise.resolve(httpError(500));
      if (url === "/admin/data/clients" && !options?.method && holdNextList) {
        holdNextList = false;
        return new Promise(resolve => {
          releaseReadback = resolve;
        });
      }
      return fallback(url, options);
    },
  });
  await openEditor(h, app, app.state.clients[0]);
  fillForm(h, {name: "c1", key: "k1"});
  holdNextList = true;
  const saving = save(h);
  await tick();
  assert.ok(releaseReadback, "the readback is pending");
  h.setRoute("/admin/data/clients", {
    clients: [{name: "c1", access_key: "k1", allowed_targets: ["new"], allowed_pools: []}],
  });
  await app.modules.clients.loadClients();
  await tick();
  assert.match(h.ids.get("clientsTableBody").innerHTML, /new/);
  // The older readback returns after the newer load; it must be dropped, not written back.
  releaseReadback(json({
    clients: [{name: "c1", access_key: "k1", allowed_targets: ["old"], allowed_pools: []}],
  }));
  await saving;
  await tick();
  assert.deepEqual(Array.from(app.state.clients[0].allowed_targets), ["new"]);
  assert.match(h.ids.get("clientsTableBody").innerHTML, /new/);
  assert.doesNotMatch(h.ids.get("clientsTableBody").innerHTML, /old/);
  assert.equal(lastToast(h).kind, "warn");
  assert.equal(lastToast(h).msg, "保存结果未确认，可重试");
  assert.equal(field(h, "clientModal").classList.contains("open"), true);
  assert.equal(field(h, "clientAccessKey").value, "k1", "the draft is untouched");
});

test("a deterministic 4xx save failure stays red and never reads the list back", async () => {
  const {h, app} = await boot({
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "PUT"
        ? Promise.resolve(httpError(400, "access_key must not be empty"))
        : fallback(url, options),
  });
  await openEditor(h, app, app.state.clients[0]);
  fillForm(h, {name: "c1", key: "k1"});
  await save(h);
  assert.equal(lastToast(h).kind, "error");
  assert.equal(lastToast(h).msg, "保存失败: access_key must not be empty");
  assert.equal(listGets(h).length, 0, "a definite failure needs no readback");
  assert.equal(field(h, "clientModal").classList.contains("open"), true);
});

test("a deterministic 4xx delete failure stays red and never reads the list back", async () => {
  const {h, app} = await boot({
    clients: [baseClient("c1")],
    fetch: (url, options, fallback) =>
      url === "/admin/data/clients/c1" && options?.method === "DELETE"
        ? Promise.resolve(httpError(404, "client not found"))
        : fallback(url, options),
  });
  deleteRow(h, "c1");
  confirmDelete(h);
  await tick();
  assert.equal(lastToast(h).kind, "error");
  assert.equal(lastToast(h).msg, "删除失败: client not found");
  assert.equal(listGets(h).length, 0);
  assert.match(h.ids.get("clientsTableBody").innerHTML, /data-del-client='c1'/);
});