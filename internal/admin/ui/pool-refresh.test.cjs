"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

function target(name, pool) {
  return {
    name,
    pool,
    endpoint_type: "provider-new",
    endpoint: "https://fixture.invalid",
    usage_adapter: "standard",
    key_statuses: [],
  };
}

test("re-entering the targets page refreshes a visible pools tab", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [] },
      "/admin/data/pools": { pools: [] },
    },
  });
  const app = await harness.run();
  const calls = [];
  app.modules.targets.loadTargets = async () => {
    calls.push("targets");
  };
  app.modules.pools.loadPools = async () => {
    calls.push("pools");
  };
  const panel = harness.ids.get("targetsPoolsPanel");
  panel.hidden = false;
  app.modules.navigation.loadPageData("targets");
  assert.deepEqual(calls, ["pools"], "visible pools tab reloads pools");
  panel.hidden = true;
  app.modules.navigation.loadPageData("targets");
  assert.deepEqual(calls, ["pools", "targets"], "targets tab still reloads targets");
});

test("pool refresh reloads targets so member chips track membership changes", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [target("member", "pool-a")] },
      "/admin/data/pools": { pools: [{ name: "pool-a" }, { name: "pool-b" }] },
    },
  });
  const app = await harness.run();
  await app.modules.pools.loadPools();
  assert.match(
    harness.ids.get("poolsTableBody").innerHTML,
    /pool-member-name'>member/,
    "initial pool lists its member",
  );
  harness.setRoute("/admin/data/targets", {
    targets: [target("member", "pool-b")],
  });
  await app.modules.pools.loadPools();
  assert.equal(app.state.targets[0].pool, "pool-b", "target cache is refreshed");
  const rows = harness.ids.get("poolsTableBody").innerHTML.split("</tr>");
  const rowA = rows.find((row) => row.includes("data-edit-pool='pool-a'"));
  const rowB = rows.find((row) => row.includes("data-edit-pool='pool-b'"));
  assert.ok(rowA && rowB, "both pools are rendered");
  assert.match(rowA, /无成员/, "previous pool loses the member chip");
  assert.match(rowB, /pool-member-name'>member/, "new pool gains the member chip");
});

test("member fetch failure is reported instead of rendering an empty pool", async () => {
  const harness = createHarness({
    routes: { "/admin/data/pools": { pools: [{ name: "pool-a" }] } },
  });
  const app = await harness.run();
  harness.setRouteFailure("/admin/data/targets", true);
  const messages = [];
  app.modules.core.toast = (msg, kind) => messages.push({ msg, kind });
  await app.modules.pools.loadPools();
  assert.ok(
    messages.some(
      (entry) => entry.kind === "error" && /加载部署池失败/.test(entry.msg),
    ),
    "failure is surfaced to the user",
  );
  const body = harness.ids.get("poolsTableBody").innerHTML;
  assert.doesNotMatch(body, /无成员/, "unknown membership is not shown as empty");
  assert.match(body, /加载失败: route failed/, "failure reason is shown");
  assert.match(body, /刷新/, "retry guidance is shown");
});

test("a later pool load failure replaces the stale table with retry guidance", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [target("member", "pool-a")] },
      "/admin/data/pools": { pools: [{ name: "pool-a" }] },
    },
  });
  const app = await harness.run();
  await app.modules.pools.loadPools();
  assert.match(
    harness.ids.get("poolsTableBody").innerHTML,
    /data-edit-pool='pool-a'/,
    "first load renders the pool",
  );
  harness.setRouteFailure("/admin/data/pools", true);
  await app.modules.pools.loadPools();
  const body = harness.ids.get("poolsTableBody").innerHTML;
  assert.match(body, /加载失败: route failed/, "failure replaces the rows");
  assert.match(body, /刷新/, "retry guidance is shown");
  assert.doesNotMatch(body, /data-edit-pool/, "stale rows are not presented as current");
  assert.doesNotMatch(body, /无成员/, "stale membership is not rendered");
});

test("failed pool option fetch keeps saved membership when editing", async () => {
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [target("member", "pool-a")] } },
  });
  const app = await harness.run();
  harness.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  assert.equal(app.state.pools.length, 0, "pool roster is unavailable");
  const saved = app.state.targets[0];
  await app.modules.targets.openTargetModal(saved);
  assert.ok(
    harness.ids
      .get("tgtPool")
      .options.some((option) => option.value === "pool-a"),
    "existing membership stays selectable",
  );
  assert.equal(
    harness.ids.get("tgtPool").value,
    "pool-a",
    "existing membership is selected",
  );
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assert.ok(put, "target update was submitted");
  assert.equal(
    JSON.parse(put.options.body).pool,
    "pool-a",
    "save does not silently remove the target from its pool",
  );
});

test("stale roster after a failed refresh cannot erase a newly assigned pool", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [target("member", "pool-a")] },
      "/admin/data/pools": { pools: [{ name: "pool-a" }] },
    },
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  assert.equal(app.state.pools[0].name, "pool-a", "roster A is cached");
  // Another session assigns a newly created pool B; the next roster fetch fails.
  harness.setRoute("/admin/data/targets", {
    targets: [target("member", "pool-b")],
  });
  harness.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  assert.equal(app.state.targets[0].pool, "pool-b", "target payload is fresh");
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  assert.equal(
    harness.ids.get("tgtPool").value,
    "pool-b",
    "stale roster absence does not hide the saved membership",
  );
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assert.equal(
    JSON.parse(put.options.body).pool,
    "pool-b",
    "save keeps the externally assigned pool",
  );
});

test("roster absence never erases a saved membership by itself", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [target("member", "pool-b")] },
      "/admin/data/pools": { pools: [{ name: "other-pool" }] },
    },
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  assert.equal(
    harness.ids.get("tgtPool").value,
    "pool-b",
    "missing roster entry is not treated as deletion proof",
  );
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assert.equal(JSON.parse(put.options.body).pool, "pool-b");
});

test("explicitly choosing no pool still unbinds on save", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [target("member", "pool-b")] },
      "/admin/data/pools": { pools: [{ name: "pool-b" }] },
    },
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  harness.ids.get("tgtPool").value = "";
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assert.equal(JSON.parse(put.options.body).pool, "", "explicit removal is kept");
});
