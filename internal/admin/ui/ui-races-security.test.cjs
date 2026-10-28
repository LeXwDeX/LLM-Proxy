"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {createHarness} = require("../../../scripts/testing/admin_ui_test.cjs");
const response = body => ({ok: true, status: 200, text: async () => JSON.stringify(body)});
const routes = () => ({"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []},
  "/admin/data/catalog": {models: []}, "/admin/data/clients": {clients: []}});

test("old client list response cannot restore an earlier ACL after a newer refresh", async () => {
  let release, delay = true;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    delay && url === "/admin/data/clients" ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  const old = app.modules.clients.loadClients();
  delay = false;
  h.setRoute("/admin/data/clients", {clients: [{name: "alice", allowed_targets: ["new"]}]});
  await app.modules.clients.loadClients();
  release(response({clients: [{name: "alice", allowed_targets: ["old"]}]}));
  await old;
  assert.deepEqual(Array.from(app.state.clients[0].allowed_targets), ["new"]);
  assert.match(h.ids.get("clientsTableBody").innerHTML, /new/);
});

test("saved ACL remains visible when the post-save refresh fails and an old read finishes", async () => {
  let release, first = true;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) => {
    if (url === "/admin/data/clients" && !options?.method) {
      if (first) { first = false; return new Promise(resolve => { release = resolve; }); }
      return Promise.resolve({ok: false, status: 503, text: async () => '{"error":"offline"}'});
    }
    return fallback(url, options);
  }});
  const app = await h.run();
  app.state.clients = [{name: "alice", access_key: "fixture", allowed_targets: ["old"]}];
  app.state.editingClient = app.state.clients[0];
  const old = app.modules.clients.loadClients();
  h.ids.get("clientTargetTags").querySelectorAll = () => [{dataset: {target: "new", selected: "1"}}];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  h.ids.get("clientName").value = "alice";
  h.ids.get("clientAccessKey").value = "fixture";
  await h.ids.get("saveClientBtn").listeners.click[0]();
  release(response({clients: [{name: "alice", access_key: "fixture", allowed_targets: ["old"]}]}));
  await old;
  assert.deepEqual(Array.from(app.state.clients[0].allowed_targets), ["new"]);
  assert.match(h.ids.get("clientsTableBody").innerHTML, /new/);
});

for (const [name, open, field, a, b] of [
  ["client", (app, item) => app.modules.clients.openClientModal(item), "clientName", {name: "A"}, {name: "B"}],
  ["model group", (app, item) => app.modules.quota.openModelGroupModal(item), "modelGroupName", {name: "A"}, {name: "B"}],
]) test(`${name} late opening cannot overwrite a newer draft`, async () => {
  let release, delay = true;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    delay && url === "/admin/data/targets" ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  const old = open(app, a);
  delay = false;
  await open(app, b);
  h.ids.get(field).value = "B draft";
  release(response({targets: []}));
  await old;
  assert.equal(h.ids.get(field).value, "B draft");
});

test("a client save finishing after another editor opens never closes that newer draft", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/clients/A" && options?.method === "PUT"
      ? new Promise(resolve => { release = resolve; }) : fallback(url, options)});
  const app = await h.run();
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  await app.modules.clients.openClientModal({name: "A", access_key: "fixture"});
  const save = h.ids.get("saveClientBtn").listeners.click[0]();
  assert.ok(release);
  await app.modules.clients.openClientModal({name: "B", access_key: "fixture"});
  h.ids.get("clientIPAddress").value = "B draft";
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("clientModal").classList.contains("open"), true);
  assert.equal(h.ids.get("clientName").value, "B");
  assert.equal(h.ids.get("clientIPAddress").value, "B draft");
});

test("a client save does not discard a later edit to the same draft", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/clients/A" && options?.method === "PUT"
      ? new Promise(resolve => { release = resolve; }) : fallback(url, options)});
  const app = await h.run();
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  await app.modules.clients.openClientModal({name: "A", access_key: "fixture"});
  const save = h.ids.get("saveClientBtn").listeners.click[0]();
  h.ids.get("clientIPAddress").value = "later draft";
  h.ids.get("clientModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("clientModal").classList.contains("open"), true);
  assert.equal(h.ids.get("clientIPAddress").value, "later draft");
});

test("a new client draft edited during creation cannot be submitted as another client", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/clients" && options?.method === "POST"
      ? new Promise(resolve => { release = resolve; }) : fallback(url, options)});
  const app = await h.run();
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  await app.modules.clients.openClientModal(null);
  h.ids.get("clientName").value = "A";
  h.ids.get("clientAccessKey").value = "fixture";
  const save = h.ids.get("saveClientBtn").listeners.click[0]();
  h.ids.get("clientName").value = "B draft";
  h.ids.get("clientModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("clientModal").classList.contains("open"), true);
  await h.ids.get("saveClientBtn").listeners.click[0]();
  assert.equal(h.requests.filter(req => req.options?.method === "PUT").length, 0);
  assert.equal(h.requests.filter(req => req.options?.method === "POST" && req.url === "/admin/data/clients").length, 1);
  assert.equal(h.ids.get("clientName").value, "B draft");
});

test("unavailable pool roster never claims empty or deletion and retains the saved ACL", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  h.setRouteFailure("/admin/data/pools", true);
  await app.modules.clients.openClientModal({name: "alice", access_key: "fixture", allowed_pools: ["saved-pool"]});
  const tags = h.ids.get("clientPoolTags");
  assert.doesNotMatch(tags.children.map(e => e.textContent).join(" "), /已删除/);
  assert.match(tags.children.map(e => e.textContent).join(" ") + tags.innerHTML, /不可用|失败|未知/);
  assert.equal(tags.children.find(e => e.dataset.pool === "saved-pool").dataset.selected, "1");
  tags.querySelectorAll = () => tags.children.filter(e => e.dataset.pool);
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  await h.ids.get("saveClientBtn").listeners.click[0]();
  const write = h.requests.find(req => req.options?.method === "PUT" && req.url === "/admin/data/clients/alice");
  assert.deepEqual(JSON.parse(write.options.body).allowed_pools, ["saved-pool"]);
  app.modules.clients.renderClientPoolTags([]);
  assert.doesNotMatch(tags.innerHTML, /暂无部署池/);
  assert.match(tags.innerHTML, /不可用|失败|未知/);
});

test("a failed later roster refresh marks cached pools unavailable for readonly client rows", async () => {
  const r = routes();
  r["/admin/data/pools"] = {pools: [{name: "pool-a", display_name: "Pool A"}]};
  const h = createHarness({role: "readonly", routes: r});
  const app = await h.run();
  await app.modules.targets.loadTargets();
  app.state.clients = [{name: "alice", allowed_pools: ["pool-a"]}];
  app.state.clientUsage = {};
  h.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  app.modules.clients.renderClients();
  const html = h.ids.get("clientsTableBody").innerHTML;
  assert.match(html, /Pool A/);
  assert.match(html, /部署池信息不可用/);
  assert.doesNotMatch(html, /部署池已删除/);
});

test("a malformed pool roster is unknown and cannot erase a saved grant", async () => {
  const r = routes(); r["/admin/data/pools"] = {pools: [null]};
  const h = createHarness({routes: r});
  const app = await h.run();
  await app.modules.clients.openClientModal({name: "alice", access_key: "fixture", allowed_pools: ["saved"]});
  const tags = h.ids.get("clientPoolTags");
  assert.match(tags.children.map(e => e.textContent).join(" "), /不可用/);
  assert.doesNotMatch(tags.children.map(e => e.textContent).join(" "), /已删除/);
  assert.equal(tags.children.find(e => e.dataset.pool === "saved").dataset.selected, "1");
});

test("single key with a null runtime index remains saveable with its snapshot", async () => {
  const saved = {name: "single", endpoint_type: "provider-new", endpoint: "https://fixture.invalid",
    usage_adapter: "standard", has_api_key: true, api_key: "long...1234", api_keys: [],
    key_snapshot: "snapshot", key_runtime_indices: [null]};
  const r = routes(); r["/admin/data/targets"] = {targets: [saved]};
  const h = createHarness({routes: r});
  const app = await h.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(saved);
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  const put = h.requests.find(req => req.options?.method === "PUT");
  assert.ok(put);
  assert.equal(put.options.headers["X-Target-Key-Snapshot"], "snapshot");
  assert.equal(JSON.parse(put.options.body).api_key, "__existing_key_index__:0");
});

test("single effective key with duplicate or empty slots keeps its exact saved slot references", async () => {
  const saved = {name: "duplicate", endpoint_type: "provider-new", endpoint: "https://fixture.invalid",
    usage_adapter: "standard", has_api_key: true, api_key: "long...1234", api_keys: ["long...1234", "****"],
    key_snapshot: "snapshot", key_runtime_indices: [null, null, null]};
  const r = routes(); r["/admin/data/targets"] = {targets: [saved]};
  const h = createHarness({routes: r, metrics: {targets_runtime: [{name: "duplicate", phase: "ready"}]}});
  const app = await h.run();
  await app.modules.targets.loadTargets();
  assert.equal(app.modules.targets.targetHealth(saved).key, "healthy", "single effective key has no key pool to report");
  await app.modules.targets.openTargetModal(saved);
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  const put = h.requests.find(req => req.options?.method === "PUT");
  assert.ok(put);
  assert.equal(put.options.headers["X-Target-Key-Snapshot"], "snapshot");
  assert.deepEqual(JSON.parse(put.options.body).api_keys, ["__existing_key_index__:1", "__existing_key_index__:2"]);
});

test("creating a target preserves edits made while POST is pending and cannot POST twice", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/targets" && options?.method === "POST" ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.targets.openTargetModal(null);
  h.ids.get("tgtName").value = "new-target";
  h.ids.get("tgtEndpoint").value = "https://before.invalid";
  h.ids.get("tgtUsageAdapter").value = "standard";
  const save = h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.ok(release);
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.equal(h.requests.filter(req => req.options?.method === "POST" && req.url === "/admin/data/targets").length, 1);
  h.ids.get("tgtEndpoint").value = "https://draft.invalid";
  h.ids.get("targetModal").listeners.input[0]();
  release(response({ok: true}));
  await save;
  assert.equal(h.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(h.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.match(h.ids.get("keyCardList").innerHTML, /已创建.*未保存/);
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.equal(h.requests.filter(req => req.options?.method === "POST" && req.url === "/admin/data/targets").length, 1);
});
