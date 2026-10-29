"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {createHarness, withETag} = require("../../../scripts/testing/admin_ui_test.cjs");

const badRosters = [
  {pools: [null]}, {pools: [{name: ""}]}, {pools: [{name: "   "}]},
  {pools: [[]]}, {pools: [{name: "ok"}, null]}, {pools: [{name: "ok", paused: "false"}]},
];
const target = withETag({name: "member", pool: "pool-a", endpoint_type: "provider-new",
  endpoint: "https://fixture.invalid", usage_adapter: "standard"}, 1);
const routes = () => ({"/admin/data/targets": {targets: [target]},
  "/admin/data/pools": {pools: [{name: "pool-a", paused: false}]}, "/admin/data/catalog": {models: []}});

test("malformed pool entries fail the first load instead of displaying an empty roster", async () => {
  for (const invalid of badRosters) {
    const r = routes(); r["/admin/data/pools"] = invalid;
    const h = createHarness({routes: r});
    const app = await h.run();
    await app.modules.pools.loadPools();
    assert.match(h.ids.get("poolsTableBody").innerHTML, /加载失败/);
    assert.doesNotMatch(h.ids.get("poolsTableBody").innerHTML, /暂无部署池|运行中|无成员/);
    assert.equal(app.modules.targets.targetPoolsAvailable(), false);
  }
});

test("malformed pool entries invalidate cross-tab state and preserve selections and drafts until recovery", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  await app.modules.pools.loadPools();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  h.ids.get("tgtEndpoint").value = "https://target-draft.invalid";
  await app.modules.clients.openClientModal({name: "client", access_key: "fixture", allowed_pools: ["pool-a"]});
  app.modules.pools.openPoolModal(app.state.pools[0]);
  h.ids.get("poolDescription").value = "pool draft";
  for (const invalid of badRosters) {
    h.setRoute("/admin/data/pools", invalid);
    await app.modules.pools.loadPools();
    assert.match(h.ids.get("poolsTableBody").innerHTML, /加载失败/);
    assert.doesNotMatch(h.ids.get("poolsTableBody").innerHTML, /运行中|无成员|暂无部署池/);
    assert.equal(app.state.pools[0].name, "pool-a");
    assert.equal(app.state.targets[0].name, "member");
    assert.equal(app.modules.targets.targetPoolsAvailable(), false);
    assert.match(h.ids.get("targetsTableBody").innerHTML, /失败|不可用|过期/);
    assert.match(h.ids.get("tgtPool").getAttribute("aria-label"), /不可用|过期/);
    assert.equal(h.ids.get("tgtEndpoint").value, "https://target-draft.invalid");
    assert.equal(h.ids.get("poolDescription").value, "pool draft");
    const tag = h.ids.get("clientPoolTags").querySelector("span[data-pool]");
    assert.match(tag.textContent, /不可用/);
    assert.equal(tag.dataset.selected, "1");
  }
  h.setRoute("/admin/data/pools", {pools: [{name: "pool-a", paused: false}]});
  await app.modules.pools.loadPools();
  assert.match(h.ids.get("poolsTableBody").innerHTML, /运行中/);
  assert.equal(app.modules.targets.targetPoolsAvailable(), true);
  const tag = h.ids.get("clientPoolTags").querySelector("span[data-pool]");
  assert.doesNotMatch(tag.textContent, /不可用|已删除/);
  assert.equal(tag.dataset.selected, "1");
  assert.equal(h.ids.get("poolDescription").value, "pool draft");
  assert.equal(h.ids.get("tgtEndpoint").value, "https://target-draft.invalid");
  h.setRoute("/admin/data/pools", {pools: []});
  await app.modules.pools.loadPools();
  assert.match(h.ids.get("poolsTableBody").innerHTML, /暂无部署池/);
  assert.equal(app.state.pools.length, 0);
  assert.equal(app.modules.targets.targetPoolsAvailable(), true);
});

test("a target-page refresh with a malformed roster also invalidates an existing pool table", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  await app.modules.pools.loadPools();
  assert.match(h.ids.get("poolsTableBody").innerHTML, /运行中/);
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  h.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await app.modules.clients.openClientModal({name: "client", access_key: "fixture", allowed_pools: ["pool-a"]});
  h.setRoute("/admin/data/pools", {pools: [null]});
  await app.modules.targets.loadTargets();
  assert.match(h.ids.get("poolsTableBody").innerHTML, /失败|不可用|过期/);
  assert.doesNotMatch(h.ids.get("poolsTableBody").innerHTML, /运行中|暂无部署池/);
  assert.equal(app.state.pools[0].name, "pool-a");
  assert.match(h.ids.get("targetsTableBody").innerHTML, /部署池.*不可用/);
  assert.match(h.ids.get("tgtPool").getAttribute("aria-label"), /不可用|过期/);
  assert.equal(h.ids.get("tgtEndpoint").value, "https://draft.invalid");
  let tag = h.ids.get("clientPoolTags").querySelector("span[data-pool]");
  assert.match(tag.textContent, /不可用/);
  assert.equal(tag.dataset.selected, "1");
  h.setRoute("/admin/data/pools", {pools: [{name: "pool-a", paused: false}]});
  await app.modules.targets.loadTargets();
  assert.match(h.ids.get("poolsTableBody").innerHTML, /运行中/);
  tag = h.ids.get("clientPoolTags").querySelector("span[data-pool]");
  assert.doesNotMatch(tag.textContent, /不可用/);
  assert.equal(tag.dataset.selected, "1");
  assert.equal(h.ids.get("tgtEndpoint").value, "https://draft.invalid");
});

test("a late malformed pool response cannot invalidate a newer valid cross-tab refresh", async () => {
  let release, delay = false;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    delay && url === "/admin/data/pools" ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.pools.loadPools();
  delay = true;
  const old = app.modules.pools.loadPools();
  assert.ok(release);
  delay = false;
  await app.modules.targets.loadTargets();
  release({ok: true, status: 200, text: async () => JSON.stringify({pools: [null]})});
  await old;
  assert.equal(app.modules.targets.targetPoolsAvailable(), true);
  assert.equal(app.state.pools[0].name, "pool-a");
  assert.match(h.ids.get("poolsTableBody").innerHTML, /运行中/);
  assert.doesNotMatch(h.ids.get("poolsTableBody").innerHTML, /加载失败/);
});
