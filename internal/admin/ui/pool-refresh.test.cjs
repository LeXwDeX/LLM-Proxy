"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness, withKeySnapshot, simulateTargetUpdate } = require("../../../scripts/testing/admin_ui_test.cjs");

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

function editableTarget(name, pool, snapshot) {
  return withKeySnapshot(target(name, pool), snapshot);
}

function assertConditionalPut(put, snapshot) {
  assert.ok(put, "target update was submitted");
  assert.equal(put.options.headers["X-Target-Key-Snapshot"], snapshot);
  assert.equal(JSON.parse(put.options.body).api_key, "__existing_key_index__:0");
  assert.deepEqual(JSON.parse(put.options.body).api_keys, []);
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
    routes: { "/admin/data/targets": { targets: [editableTarget("member", "pool-a", "pool-snapshot-1")] } },
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
  simulateTargetUpdate(harness, saved, editableTarget("member", "pool-a", "pool-snapshot-2"));
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assert.ok(put, "target update was submitted");
  assertConditionalPut(put, "pool-snapshot-1");
  assert.equal(app.state.targets[0].key_snapshot, "pool-snapshot-2");
  assert.equal(
    JSON.parse(put.options.body).pool,
    "pool-a",
    "save does not silently remove the target from its pool",
  );
});

test("stale roster after a failed refresh cannot erase a newly assigned pool", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [editableTarget("member", "pool-a", "pool-snapshot-a")] },
      "/admin/data/pools": { pools: [{ name: "pool-a" }] },
    },
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  assert.equal(app.state.pools[0].name, "pool-a", "roster A is cached");
  // Another session assigns a newly created pool B; the next roster fetch fails.
  harness.setRoute("/admin/data/targets", {
    targets: [editableTarget("member", "pool-b", "pool-snapshot-b")],
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
  simulateTargetUpdate(harness, app.state.targets[0], editableTarget("member", "pool-b", "pool-snapshot-b-next"));
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assertConditionalPut(put, "pool-snapshot-b");
  assert.equal(app.state.targets[0].key_snapshot, "pool-snapshot-b-next");
  assert.equal(
    JSON.parse(put.options.body).pool,
    "pool-b",
    "save keeps the externally assigned pool",
  );
});

test("roster absence never erases a saved membership by itself", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [editableTarget("member", "pool-b", "pool-snapshot-1")] },
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
  simulateTargetUpdate(harness, app.state.targets[0], editableTarget("member", "pool-b", "pool-snapshot-2"));
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assertConditionalPut(put, "pool-snapshot-1");
  assert.equal(app.state.targets[0].key_snapshot, "pool-snapshot-2");
  assert.equal(JSON.parse(put.options.body).pool, "pool-b");
});

test("explicitly choosing no pool still unbinds on save", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/targets": { targets: [editableTarget("member", "pool-b", "pool-snapshot-1")] },
      "/admin/data/pools": { pools: [{ name: "pool-b" }] },
    },
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  harness.ids.get("tgtPool").value = "";
  simulateTargetUpdate(harness, app.state.targets[0], editableTarget("member", "", "pool-snapshot-2"));
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const put = harness.requests.findLast(
    (request) =>
      request.url === "/admin/data/targets/member" &&
      request.options?.method === "PUT",
  );
  assertConditionalPut(put, "pool-snapshot-1");
  assert.equal(app.state.targets[0].key_snapshot, "pool-snapshot-2");
  assert.equal(JSON.parse(put.options.body).pool, "", "explicit removal is kept");
});

test("an absent snapshot cannot authorize a target update", async () => {
  const saved = editableTarget("member", "pool-a", "unused-snapshot");
  delete saved.key_snapshot;
  const harness = createHarness({ routes: { "/admin/data/targets": { targets: [saved] } } });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  assert.equal(harness.requests.filter((r) => r.options?.method === "PUT").length, 0);
  assert.equal(harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.match(harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
});

test("a rejected stale snapshot preserves the draft and is not retried", async () => {
  const saved = editableTarget("member", "pool-a", "expired-snapshot");
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [saved] } },
    fetch: (url, options, fallback) => url === "/admin/data/targets/member" && options?.method === "PUT"
      ? Promise.resolve({ ok: false, status: 409, text: async () => JSON.stringify({ error: "snapshot changed" }) })
      : fallback(url, options),
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const writes = harness.requests.filter((r) => r.options?.method === "PUT");
  assert.equal(writes.length, 1);
  assertConditionalPut(writes[0], "expired-snapshot");
  assert.equal(harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.match(harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
});

test("a null successor cannot authorize a draft after a key operation", async () => {
  const saved = editableTarget("member", "pool-a", "pool-snapshot-1");
  const fresh = editableTarget("member", "pool-a", "pool-snapshot-2");
  fresh.key_statuses[0].blocked = true;
  fresh.key_statuses[0].status = "已屏蔽";
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [saved] } },
    fetch: (url, options, fallback) => {
      if (url === "/admin/data/targets/member/keys/0/block" && options?.method === "PUT") {
        harness.setRoute("/admin/data/targets", { targets: [fresh] });
        return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify({ ok: true, key_snapshot: null }) });
      }
      return fallback(url, options);
    },
  });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  app.modules.targets.addKeyToList();
  let confirm;
  app.modules.core.showConfirm = (_message, callback) => { confirm = callback; };
  harness.ids.get("keyCardList").listeners.click[0]({ target: { dataset: { blockKeyIdx: "0" } } });
  assert.ok(confirm, "blocking requires confirmation");
  await confirm();
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const writes = harness.requests.filter((r) => r.options?.method === "PUT");
  assert.equal(writes.length, 1, "the later GET cannot authorize a config update");
  assert.equal(writes[0].url, "/admin/data/targets/member/keys/0/block");
  assert.equal(writes[0].options.headers["X-Target-Key-Snapshot"], "pool-snapshot-1");
  assert.match(harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
  assert.match(harness.ids.get("keyCardList").innerHTML, /temp/, "the added key remains in the draft");
});
