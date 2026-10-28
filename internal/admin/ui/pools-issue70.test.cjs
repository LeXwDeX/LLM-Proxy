"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

test("invalid pool lists are errors, preserve cached data and the editing draft, and can recover", async () => {
  const harness = createHarness({routes: {
    "/admin/data/pools": {pools: [{name: "one"}]},
    "/admin/data/targets": {targets: []},
  }});
  const app = await harness.run();
  await app.modules.pools.loadPools();
  app.modules.pools.openPoolModal(app.state.pools[0]);
  harness.ids.get("poolDescription").value = "unsaved draft";
  for (const bad of [null, {}, {pools: null}, {pools: {}}, {pools: "not-array"}]) {
    harness.setRoute("/admin/data/pools", bad);
    await app.modules.pools.loadPools();
    assert.match(harness.ids.get("poolsTableBody").innerHTML, /加载失败/);
    assert.doesNotMatch(harness.ids.get("poolsTableBody").innerHTML, /暂无部署池/);
    assert.equal(app.state.pools[0].name, "one", "invalid data does not replace the cached roster");
    assert.equal(harness.ids.get("poolDescription").value, "unsaved draft");
  }
  harness.setRoute("/admin/data/pools", {pools: []});
  await app.modules.pools.loadPools();
  assert.match(harness.ids.get("poolsTableBody").innerHTML, /暂无部署池/);
  assert.equal(app.state.pools.length, 0, "a real empty array replaces the stale roster");
  assert.equal(harness.ids.get("poolDescription").value, "unsaved draft");
});
