"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

const response = body => ({ ok: true, status: 200, text: async () => JSON.stringify(body) });

for (const [name, start, paths] of [
  ["client list", app => app.modules.clients.loadClients(), ["targets", "pools"]],
  ["client picker", app => app.modules.clients.ensureClientPickerData(), ["targets", "pools"]],
  ["quota editor", app => app.modules.quota.loadQuotaEditorData(), ["targets"]],
]) {
  test(`${name}: an old response cannot replace the newer scheduling snapshot`, async () => {
    const pending = {};
    let delay = true;
    const harness = createHarness({
      routes: { "/admin/data/clients": { clients: [] }, "/admin/data/catalog": { models: [] },
        "/admin/data/targets": { targets: [{ name: "new-target", pool: "new-pool" }] },
        "/admin/data/pools": { pools: [{ name: "new-pool" }] } },
      fetch: (url, options, fallback) => {
        const resource = url.replace("/admin/data/", "");
        if (delay && paths.includes(resource)) {
          return new Promise(resolve => { pending[resource] = resolve; });
        }
        return fallback(url, options);
      },
    });
    const app = await harness.run();
    const old = start(app);
    for (let i = 0; i < 50 && paths.some(path => !pending[path]); i++) await Promise.resolve();
    for (const path of paths) assert.ok(pending[path], `old ${path} request started`);
    delay = false;
    await app.modules.pools.loadPools();
    assert.equal(app.state.targets[0].name, "new-target");
    assert.equal(app.state.pools[0].name, "new-pool");
    for (const path of paths) pending[path](response({ [path]: [{ name: `old-${path}` }] }));
    await old;
    assert.equal(app.state.targets[0].name, "new-target");
    assert.equal(app.state.pools[0].name, "new-pool");
  });
}

test("model editor follows multiple superseding refreshes", async () => {
  const pending = [];
  const harness = createHarness({
    routes: { "/admin/data/catalog": { models: [] }, "/admin/data/pools": { pools: [] } },
    fetch: (url, options, fallback) => url === "/admin/data/targets"
      ? new Promise(resolve => pending.push(resolve)) : fallback(url, options),
  });
  const app = await harness.run();
  const editor = app.modules.quota.openModelGroupModal({ name: "group", cost_coefficient: 1,
    bindings: [{ target: "latest" }] });
  const second = app.modules.pools.loadPools();
  pending[0](response({ targets: [{ name: "old" }] }));
  for (let i = 0; i < 50; i++) await Promise.resolve();
  const third = app.modules.targets.loadTargets();
  pending[1](response({ targets: [{ name: "middle" }] }));
  for (let i = 0; i < 50; i++) await Promise.resolve();
  assert.equal(harness.ids.get("modelGroupModal").classList.contains("open"), false);
  pending[2](response({ targets: [{ name: "latest" }] }));
  await Promise.all([editor, second, third]);
  assert.equal(harness.ids.get("modelGroupModal").classList.contains("open"), true);
  assert.match(harness.ids.get("modelGroupBindingEditor").innerHTML, /latest/);
  assert.doesNotMatch(harness.ids.get("modelGroupBindingEditor").innerHTML, /目标已删除/);
});

test("client picker waits for the current roster before rendering fallback options", async () => {
  const pending = [];
  const harness = createHarness({
    routes: { "/admin/data/pools": { pools: [{ name: "pool" }] } },
    fetch: (url, options, fallback) => url === "/admin/data/targets"
      ? new Promise(resolve => pending.push(resolve)) : fallback(url, options),
  });
  const app = await harness.run();
  const picker = app.modules.clients.ensureClientPickerData();
  const newer = app.modules.pools.loadPools();
  pending[0](response({ targets: [{ name: "old" }] }));
  for (let i = 0; i < 50; i++) await Promise.resolve();
  assert.equal(harness.ids.get("clientTargetTags").children.length, 0);
  pending[1](response({ targets: [{ name: "new" }] }));
  await Promise.all([picker, newer]);
  assert.deepEqual(harness.ids.get("clientTargetTags").children.map(el => el.dataset.target), ["new"]);
});

for (const [name, open, modal, delayedUrl] of [
  ["model group", app => app.modules.quota.openModelGroupModal(null), "modelGroupModal", "/admin/data/catalog"],
  ["client", app => app.modules.clients.openClientModal(null), "clientModal", "/admin/data/job-levels"],
]) {
  test(`${name} editor rechecks scheduling after other editor data loads`, async () => {
    let releaseOther, releaseNew;
    let delayTargets = false;
    const harness = createHarness({
      routes: { "/admin/data/targets": { targets: [{ name: "old" }] },
        "/admin/data/pools": { pools: [] }, "/admin/data/catalog": { models: [] },
        "/admin/data/job-levels": { job_levels: [] } },
      fetch: (url, options, fallback) => {
        if (url === delayedUrl) return new Promise(resolve => { releaseOther = resolve; });
        if (delayTargets && url === "/admin/data/targets") return new Promise(resolve => { releaseNew = resolve; });
        return fallback(url, options);
      },
    });
    const app = await harness.run();
    const editor = open(app);
    for (let i = 0; i < 100 && (!releaseOther || !app.state.targets.length); i++) await Promise.resolve();
    assert.ok(releaseOther);
    assert.equal(app.state.targets[0].name, "old", "initial scheduling request has published");
    delayTargets = true;
    const newer = app.modules.pools.loadPools();
    assert.ok(releaseNew);
    releaseOther(response(delayedUrl.endsWith("catalog") ? { models: [] } : { job_levels: [] }));
    for (let i = 0; i < 50; i++) await Promise.resolve();
    assert.equal(harness.ids.get(modal).classList.contains("open"), false);
    releaseNew(response({ targets: [{ name: "new" }] }));
    await Promise.all([editor, newer]);
    assert.equal(harness.ids.get(modal).classList.contains("open"), true);
    assert.equal(app.state.targets[0].name, "new");
  });
}

test("new quota refresh prevents an older pool page response from replacing it", async () => {
  let release;
  let delay = true;
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [{ name: "new-target" }] },
      "/admin/data/pools": { pools: [{ name: "new-pool" }] }, "/admin/data/catalog": { models: [] } },
    fetch: (url, options, fallback) => {
      if (delay && url === "/admin/data/targets") return new Promise(resolve => { release = resolve; });
      return fallback(url, options);
    },
  });
  const app = await harness.run();
  const old = app.modules.pools.loadPools();
  assert.ok(release);
  delay = false;
  await app.modules.quota.loadQuotaEditorData();
  release(response({ targets: [{ name: "old-target" }] }));
  await old;
  assert.equal(app.state.targets[0].name, "new-target");
  assert.equal(app.state.pools[0].name, "new-pool");
});

test("quota refresh propagates current target failures and retains the previous snapshot", async () => {
  const harness = createHarness({ routes: {
    "/admin/data/targets": { targets: [{ name: "saved-target" }] },
    "/admin/data/pools": { pools: [{ name: "saved-pool" }] },
    "/admin/data/catalog": { models: [] },
  } });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  harness.setRouteFailure("/admin/data/targets", true);
  await assert.rejects(app.modules.quota.loadQuotaEditorData(), /route failed/);
  assert.equal(app.state.targets[0].name, "saved-target");
  assert.equal(app.state.pools[0].name, "saved-pool");
  assert.match(harness.ids.get("targetsTableBody").innerHTML, /失败/);
});

test("a stale quota failure cannot invalidate a newer target snapshot", async () => {
  let release;
  let delay = true;
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [{ name: "new-target" }] },
      "/admin/data/pools": { pools: [{ name: "new-pool" }] }, "/admin/data/catalog": { models: [] } },
    fetch: (url, options, fallback) => {
      if (delay && url === "/admin/data/targets") return new Promise(resolve => { release = resolve; });
      return fallback(url, options);
    },
  });
  const app = await harness.run();
  const old = app.modules.quota.loadQuotaEditorData();
  delay = false;
  await app.modules.targets.loadTargets();
  release({ ok: false, status: 503, text: async () => '{"error":"old failure"}' });
  await old;
  assert.equal(app.state.targets[0].name, "new-target");
  assert.doesNotMatch(harness.ids.get("targetsTableBody").innerHTML, /加载.*失败/);
});

for (const [name, start, initialTargets] of [
  // The list prefetches targets only when none are loaded (#96), so it starts empty.
  ["client list", app => app.modules.clients.loadClients(), []],
  ["client picker", app => app.modules.clients.ensureClientPickerData(), [{ name: "member" }]],
]) {
  test(`${name}: an old pool-only fallback cannot replace a newer roster`, async () => {
    let release;
    let delay = true;
    const harness = createHarness({
      routes: { "/admin/data/targets": { targets: [{ name: "member", pool: "new-pool" }] },
        "/admin/data/pools": { pools: [{ name: "new-pool" }] }, "/admin/data/clients": { clients: [] } },
      fetch: (url, options, fallback) => {
        if (delay && url === "/admin/data/pools") return new Promise(resolve => { release = resolve; });
        return fallback(url, options);
      },
    });
    const app = await harness.run();
    app.state.targets = initialTargets;
    const old = start(app);
    assert.ok(release);
    delay = false;
    await app.modules.pools.loadPools();
    release(response({ pools: [{ name: "old-pool" }] }));
    await old;
    assert.equal(app.state.pools[0].name, "new-pool");
    assert.equal(app.state.targets[0].pool, "new-pool");
  });
}

for (const [name, open, modal] of [
  ["client", app => app.modules.clients.openClientModal(null), "clientModal"],
  ["model group", app => app.modules.quota.openModelGroupModal({ name: "group", cost_coefficient: 1,
    bindings: [{ target: "new-target" }] }), "modelGroupModal"],
]) {
  test(`${name} editor waits for the newer pool refresh before using target options`, async () => {
    const pending = [];
    const harness = createHarness({
      routes: { "/admin/data/catalog": { models: [] }, "/admin/data/pools": { pools: [] } },
      fetch: (url, options, fallback) => url === "/admin/data/targets"
        ? new Promise(resolve => pending.push(resolve)) : fallback(url, options),
    });
    const app = await harness.run();
    app.state.targets = [{ name: "old-target" }];
    const editor = open(app);
    assert.equal(pending.length, 1);
    const newer = app.modules.pools.loadPools();
    assert.equal(pending.length, 2);
    pending[0](response({ targets: [{ name: "old-target" }] }));
    for (let i = 0; i < 50; i++) await Promise.resolve();
    assert.equal(harness.ids.get(modal).classList.contains("open"), false,
      "the editor must not open with an obsolete snapshot while the newer request is pending");
    pending[1](response({ targets: [{ name: "new-target" }] }));
    await Promise.all([editor, newer]);
    assert.equal(harness.ids.get(modal).classList.contains("open"), true);
    if (name === "client") {
      assert.deepEqual(harness.ids.get("clientTargetTags").children.map(el => el.dataset.target), ["new-target"]);
    } else {
      assert.match(harness.ids.get("modelGroupBindingEditor").innerHTML, /new-target/);
      assert.doesNotMatch(harness.ids.get("modelGroupBindingEditor").innerHTML, /目标已删除/);
    }
  });

  test(`${name} editor stays closed and reports failure when its newer refresh fails`, async () => {
    const pending = [];
    const harness = createHarness({
      routes: { "/admin/data/catalog": { models: [] }, "/admin/data/pools": { pools: [] } },
      fetch: (url, options, fallback) => url === "/admin/data/targets"
        ? new Promise(resolve => pending.push(resolve)) : fallback(url, options),
    });
    const app = await harness.run();
    const messages = [];
    app.modules.core.toast = (message, kind) => messages.push({ message, kind });
    app.state.targets = [{ name: "old-target" }];
    const editor = open(app);
    const newer = app.modules.pools.loadPools();
    pending[0](response({ targets: [{ name: "old-target" }] }));
    for (let i = 0; i < 50; i++) await Promise.resolve();
    pending[1]({ ok: false, status: 503, text: async () => '{"error":"new failure"}' });
    await Promise.all([editor, newer]);
    assert.equal(harness.ids.get(modal).classList.contains("open"), false);
    assert.ok(messages.some(m => m.kind === "error" && /new failure/.test(m.message)), "failure is visible");
  });
}
