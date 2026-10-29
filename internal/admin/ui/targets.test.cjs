"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

function status(index, mask, blocked = false) {
  return { index, key_mask: mask, blocked, exhausted: false, status: blocked ? "已屏蔽" : "等待中" };
}
function target(overrides = {}) {
  return {
    name: "sample", endpoint_type: "provider-new", endpoint: "https://fixture.invalid",
    usage_adapter: "standard", has_api_key: true, api_key: "aaaa...1111",
    api_keys: ["bbbb...2222", "cccc...3333"], key_snapshot: "snapshot-1",
    key_runtime_indices: [0, 1, 2], key_statuses: [status(0, "aaaa...1111"), status(1, "bbbb...2222"), status(2, "cccc...3333")],
    ...overrides,
  };
}
const response = (body, statusCode = 200) => ({
  ok: statusCode >= 200 && statusCode < 300, status: statusCode,
  text: async () => JSON.stringify(body),
});
async function setup(initial = target(), intercept) {
  const harness = createHarness({
    routes: { "/admin/data/targets": { targets: [initial] }, "/admin/data/pools": { pools: [] } },
    metrics: { targets_runtime: [{ name: "sample", phase: "ready" }] },
    fetch: intercept ? (url, opts, fallback) => intercept(url, opts, fallback) : null,
  });
  const app = await harness.run();
  const messages = [];
  let confirmation;
  app.modules.core.toast = (msg, kind) => messages.push({ msg, kind });
  app.modules.core.showConfirm = (msg, cb) => { confirmation = { msg, cb }; };
  await app.modules.targets.loadTargets();
  return {
    harness, app, messages,
    async edit() { await app.modules.targets.openTargetModal(app.state.targets[0]); },
    action(dataset) {
      confirmation = null;
      harness.ids.get("keyCardList").listeners.click[0]({ target: { dataset } });
      assert.ok(confirmation, "action requires confirmation");
      return confirmation.cb();
    },
    save() { return harness.ids.get("saveTargetBtn").listeners.click[0](); },
    puts() { return harness.requests.filter(r => r.options?.method === "PUT"); },
  };
}
function putBody(request) { return JSON.parse(request.options.body); }
function keyHeader(request) { return request.options.headers["X-Target-Key-Snapshot"]; }

test("block refresh keeps removals and later additions, then saves with the proven successor", async () => {
  let releaseFetch;
  let delayRefresh = false;
  const f = await setup(target(), (url, opts, fallback) => {
    if (url.endsWith("/keys/0/block")) return Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" }));
    if (url === "/admin/data/targets" && delayRefresh) return new Promise(resolve => { releaseFetch = resolve; });
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  delayRefresh = true;
  const pending = f.action({ blockKeyIdx: "0" });
  for (let i = 0; i < 30 && !releaseFetch; i++) await Promise.resolve();
  assert.ok(releaseFetch, "block waits for the refreshed target");
  f.harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  f.app.modules.targets.addKeyToList();
  const refreshed = target({ key_snapshot: "snapshot-2", key_statuses: [status(0, "aaaa...1111", true), status(1, "bbbb...2222"), status(2, "cccc...3333")] });
  releaseFetch(response({ targets: [refreshed] }));
  await pending;
  delayRefresh = false;
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.doesNotMatch(cards, /bbbb\.\.\.2222/, "removed key is not restored by refresh");
  assert.match(cards, /cccc\.\.\.3333/, "other saved key stays");
  assert.match(cards, /temp/, "draft key added while refreshing stays");
  await f.save();
  const [block, update] = f.puts();
  assert.equal(keyHeader(block), "snapshot-1");
  assert.equal(keyHeader(update), "snapshot-2");
  assert.equal(putBody(update).api_key, "__existing_key_index__:0");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_index__:2", "temporary-test-key"]);
});

test("a later GET cannot rebase a draft onto a concurrent replacement or reorder", async () => {
  const masks = target({ api_key: "****", api_keys: ["****", "****"], key_statuses: [status(0, "****"), status(1, "****"), status(2, "****")] });
  const f = await setup(masks, (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRoute("/admin/data/targets", { targets: [target({ ...masks, key_snapshot: "snapshot-3", key_runtime_indices: [2, 0, 1] })] });
  await f.action({ blockKeyIdx: "0" });
  await f.save();
  assert.equal(f.puts().length, 1, "no stale or unconditional config update");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
  assert.equal((f.harness.ids.get("keyCardList").innerHTML.match(/data-remove-key-idx=/g) || []).length, 2, "draft removal remains intact");
});

test("failed refresh or missing successor never reconstructs the key draft", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ ok: true, key_snapshot: null })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.action({ blockKeyIdx: "0" });
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
  await f.save();
  assert.equal(f.puts().length, 1, "missing successor cannot authorize an update");
});

test("conflicts preserve draft and require confirmation without automatic retry", async () => {
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets/sample" && opts?.method === "PUT"
    ? Promise.resolve(response({ error: "target key snapshot changed; refresh targets" }, 409)) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  await f.save();
  assert.equal(f.puts().length, 1, "a conflict is not retried");
  assert.equal(keyHeader(f.puts()[0]), "snapshot-1");
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
});

test("configuration slots and runtime indices stay separate, even for empty and identical masked slots", async () => {
  const slots = target({ has_api_key: false, api_key: "", api_keys: ["****", "****"], allow_bearer_passthrough: true,
    key_runtime_indices: [null, 0, 1], key_statuses: [status(0, "first-mask"), status(1, "second-mask")] });
  const f = await setup(slots, (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" })) : fallback(url, opts));
  await f.edit();
  let cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.match(cards, /first-mask/);
  assert.match(cards, /second-mask/, "equal masked configuration values are not deduplicated");
  assert.match(cards, /data-block-key-idx='0'/, "first extra key maps to runtime index 0");
  f.harness.setRoute("/admin/data/targets", { targets: [target({ ...slots, key_snapshot: "snapshot-2", key_statuses: [status(0, "first-mask", true), status(1, "second-mask")] })] });
  await f.action({ blockKeyIdx: "0" });
  await f.save();
  const [block, update] = f.puts();
  assert.equal(block.url, "/admin/data/targets/sample/keys/0/block");
  assert.equal(putBody(update).api_key, "__existing_key_index__:0", "empty primary slot remains empty");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_index__:1", "__existing_key_index__:2"]);
});

test("an unavailable snapshot fails closed for existing target updates", async () => {
  const f = await setup(target({ key_snapshot: null, key_runtime_indices: null }));
  await f.edit();
  await f.save();
  assert.equal(f.puts().length, 0);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
});

test("a snapshot cannot authorize writes when slot fields or runtime mapping are incomplete", async () => {
  for (const bad of [
    target({ api_keys: undefined, key_runtime_indices: [0] }),
    target({ key_runtime_indices: [0, 99, 2] }),
    target({ key_runtime_indices: [0, null, "2"] }),
    target({ api_keys: ["bbbb...2222"], key_runtime_indices: [0, null], key_statuses: [status(0, "aaaa...1111")] }),
    target({ has_api_key: false }),
    target({ has_api_key: false, api_key: "", key_runtime_indices: [0, 1, 2] }),
  ]) {
    const f = await setup(bad);
    await f.edit();
    await f.save();
    assert.equal(f.puts().length, 0, "unverified slots and mapping must not produce a PUT");
    assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
  }
});

test("unblock uses the mapped runtime index and preserves the draft with its successor", async () => {
  const initial = target({ key_runtime_indices: [2, 0, 1], key_statuses: [status(0, "bbbb...2222", true), status(1, "cccc...3333"), status(2, "aaaa...1111")] });
  const f = await setup(initial, (url, opts, fallback) => url.endsWith("/keys/0/unblock")
    ? Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(2);
  f.harness.setRoute("/admin/data/targets", { targets: [target({ ...initial, key_snapshot: "snapshot-2", key_statuses: [status(0, "bbbb...2222"), status(1, "cccc...3333"), status(2, "aaaa...1111")] })] });
  await f.action({ unblockKeyIdx: "0" });
  await f.save();
  const [unblock, update] = f.puts();
  assert.equal(unblock.url, "/admin/data/targets/sample/keys/0/unblock");
  assert.equal(keyHeader(unblock), "snapshot-1");
  assert.equal(keyHeader(update), "snapshot-2");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_index__:1"]);
});

test("a rejected block does not retry or permit a later unconditional update", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ error: "target key snapshot changed; refresh targets" }, 409)) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  await f.action({ blockKeyIdx: "0" });
  await f.save();
  assert.equal(f.puts().length, 1);
  assert.equal(keyHeader(f.puts()[0]), "snapshot-1");
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
});

test("first and later target load failures are visible and never look current", async () => {
  const f = await setup();
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.app.modules.targets.loadTargets();
  assert.match(f.harness.ids.get("targetsTableBody").innerHTML, /失败.*上次|上次.*失败/);
  assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "attention");
  const empty = createHarness({ routes: { "/admin/data/pools": { pools: [] } } });
  const app = await empty.run();
  empty.setRouteFailure("/admin/data/targets", true);
  await app.modules.targets.loadTargets();
  assert.match(empty.ids.get("targetsTableBody").innerHTML, /加载.*失败/);
  assert.doesNotMatch(empty.ids.get("targetsTableBody").innerHTML, /暂无目标配置/);
});

test("malformed target responses retain the last good data and show a failure", async () => {
  const f = await setup();
  f.harness.setRoute("/admin/data/targets", { targets: [null] });
  await f.app.modules.targets.loadTargets();
  assert.equal(f.app.state.targets[0].name, "sample");
  assert.match(f.harness.ids.get("targetsTableBody").innerHTML, /失败.*上次|上次.*失败/);
});

test("incomplete multi-key status is attention, while single and zero keys keep the normal contract", async () => {
  const f = await setup();
  const health = f.app.modules.targets.targetHealth;
  assert.equal(health(target({ key_statuses: [] })).key, "attention");
  assert.equal(health(target({ key_statuses: [status(0, "aaaa...1111")] })).key, "attention");
  assert.equal(health(target({ key_statuses: [status(0, "aaaa...1111"), { index: 1 }, status(2, "cccc...3333")] })).key, "attention");
  assert.equal(health(target({ api_keys: [], key_runtime_indices: [null], key_statuses: [] })).key, "healthy");
  assert.equal(health(target({ has_api_key: false, api_key: "", api_keys: [], key_runtime_indices: [null], key_statuses: [] })).key, "healthy");
});

test("multi-key health checks missing non-empty slots without rejecting empty or duplicate slots", async () => {
  const f = await setup();
  const health = f.app.modules.targets.targetHealth;
  assert.equal(health(target({ api_keys: ["bbbb...2222"], key_runtime_indices: [0, null], key_statuses: [status(0, "aaaa...1111")] })).key, "attention");
  assert.equal(health(target({ api_keys: ["bbbb...2222"], key_runtime_indices: [0, null], key_statuses: [status(0, "aaaa...1111"), status(1, "bbbb...2222")] })).key, "attention", "unmapped status cannot repair a contradictory slot mapping");
  assert.equal(health(target({ api_keys: ["****"], key_runtime_indices: [0, null], key_statuses: [status(0, "aaaa...1111")] })).key, "healthy", "empty additional slot is masked as **** by the server");
  assert.equal(health(target({ api_keys: ["aaaa...1111"], key_runtime_indices: [0, 0], key_statuses: [status(0, "aaaa...1111")] })).key, "healthy", "duplicate keys share one runtime entry");
});

test("opening is synchronous, so the newest open owns the editor and its draft", async () => {
  const f = await setup(target());
  f.app.modules.targets.openTargetModal(target({ name: "A", endpoint: "https://a.invalid" }));
  f.app.modules.targets.openTargetModal(target({ name: "B", endpoint: "https://b.invalid" }));
  assert.equal(f.app.state.editingTarget.name, "B");
  assert.equal(f.harness.ids.get("tgtName").value, "B");
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://b.invalid");
});

test("selecting a model suggestion during save keeps the new mapping as an unsaved draft", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets/sample" && opts?.method === "PUT"
    ? new Promise(resolve => { release = resolve; }) : fallback(url, opts));
  f.app.state.catalogCache["provider-new"] = [{ model: "suggested-model" }];
  await f.edit();
  for (let i = 0; i < 30 && !f.harness.ids.get("tgtModelSuggestions").children.length; i++) await Promise.resolve();
  const suggestion = f.harness.ids.get("tgtModelSuggestions").children[0];
  assert.ok(suggestion);
  const pending = f.save();
  suggestion.listeners.click[0]();
  assert.ok(f.harness.ids.get("model-mappings-container").children.some(row => row.querySelector(".upstream-input")?.value === "suggested-model"));
  release(response({ ok: true }));
  await pending;
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.ok(f.messages.some(m => /后续.*未保存/.test(m.msg)));
});

test("a proven successor permits saving the unchanged draft after a failed status refresh", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.action({ blockKeyIdx: "0" });
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /状态刷新失败/);
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  await f.save();
  assert.equal(keyHeader(f.puts()[1]), "snapshot-2");
  assert.deepEqual(putBody(f.puts()[1]).api_keys, ["__existing_key_index__:2"]);
});

test("a later successful refresh restores status actions without restoring removed keys", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.action({ blockKeyIdx: "0" });
  f.harness.setRouteFailure("/admin/data/targets", false);
  f.harness.setRoute("/admin/data/targets", { targets: [target({ key_snapshot: "snapshot-2", key_statuses: [status(0, "aaaa...1111", true), status(1, "bbbb...2222"), status(2, "cccc...3333")] })] });
  await f.app.modules.targets.loadTargets();
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.doesNotMatch(cards, /状态刷新失败|bbbb\.\.\.2222/);
  assert.match(cards, /data-unblock-key-idx='0'/);
});

test("explicit re-confirmation reloads saved slots and keeps newly entered keys and other form fields", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/keys/0/block")
    ? Promise.resolve(response({ ok: true, key_snapshot: null })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  f.app.modules.targets.addKeyToList();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  f.harness.setRoute("/admin/data/targets", { targets: [target({ key_snapshot: "snapshot-3" })] });
  await f.action({ blockKeyIdx: "0" });
  await f.action({ reloadKeyDraft: "1" });
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/, "confirmed reload resets only saved-key removals");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /temp/, "newly entered key stays");
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  f.app.modules.targets.removeKeyFromList(1);
  await f.save();
  const update = f.puts()[1];
  assert.equal(keyHeader(update), "snapshot-3");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_index__:2", "temporary-test-key"]);
});

test("re-confirmation does not accept a saved-key removal while the new snapshot is loading", async () => {
  let delay = false, release;
  const f = await setup(target(), (url, opts, fallback) => {
    if (url.endsWith("/keys/0/block")) return Promise.resolve(response({ ok: true, key_snapshot: null }));
    if (url === "/admin/data/targets" && delay) return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  await f.action({ blockKeyIdx: "0" });
  delay = true;
  const pending = f.action({ reloadKeyDraft: "1" });
  for (let i = 0; i < 30 && !release; i++) await Promise.resolve();
  assert.ok(release);
  const before = f.harness.ids.get("keyCardList").innerHTML;
  f.app.modules.targets.removeKeyFromList(0);
  assert.equal(f.harness.ids.get("keyCardList").innerHTML, before, "old slot removal is disabled until review is complete");
  f.harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  f.app.modules.targets.addKeyToList();
  release(response({ targets: [target({ key_snapshot: "snapshot-3" })] }));
  await pending;
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /temp/, "newly added key survives the reload");
});

test("a slower target refresh cannot overwrite a newer successful refresh", async () => {
  let delay = false, release;
  const f = await setup(target(), (url, opts, fallback) => {
    if (url === "/admin/data/targets" && delay) return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  delay = true;
  const older = f.app.modules.targets.loadTargets();
  for (let i = 0; i < 30 && !release; i++) await Promise.resolve();
  assert.ok(release);
  delay = false;
  f.harness.setRoute("/admin/data/targets", { targets: [target({ key_snapshot: "snapshot-3" })] });
  await f.app.modules.targets.loadTargets();
  release(response({ targets: [target({ key_snapshot: "snapshot-2" })] }));
  await older;
  assert.equal(f.app.state.targets[0].key_snapshot, "snapshot-3");
});

test("a late save response cannot close a newly opened editor", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets/sample" && opts?.method === "PUT"
    ? new Promise(resolve => { release = resolve; }) : fallback(url, opts));
  await f.edit();
  const pending = f.save();
  assert.ok(release);
  await f.app.modules.targets.openTargetModal(target({ name: "other", key_snapshot: "other-snapshot" }));
  f.harness.ids.get("tgtEndpoint").value = "https://other-draft.invalid";
  release(response({ ok: true }));
  await pending;
  assert.equal(f.app.state.editingTarget?.name, "other");
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://other-draft.invalid");
});

test("changes made while a save is in flight remain visible and are not declared saved", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets/sample" && opts?.method === "PUT"
    ? new Promise(resolve => { release = resolve; }) : fallback(url, opts));
  await f.edit();
  const pending = f.save();
  f.harness.ids.get("tgtEndpoint").value = "https://later-draft.invalid";
  for (const fn of f.harness.ids.get("targetModal").listeners.input || []) fn({ target: f.harness.ids.get("tgtEndpoint") });
  release(response({ ok: true }));
  await pending;
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://later-draft.invalid");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
  assert.ok(f.messages.some(m => /后续.*未保存/.test(m.msg)));
});

test("button-driven form edits made during save also prevent closing the draft", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets/sample" && opts?.method === "PUT"
    ? new Promise(resolve => { release = resolve; }) : fallback(url, opts));
  await f.edit();
  const pending = f.save();
  const button = { id: "add-model-mapping" };
  for (const fn of f.harness.ids.get("targetModal").listeners.click || []) fn({ target: { closest: () => button } });
  release(response({ ok: true }));
  await pending;
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.ok(f.messages.some(m => /后续.*未保存/.test(m.msg)));
});

test("failed or malformed pool refresh is visibly unavailable without using stale pause", async () => {
  for (const malformed of [false, true]) {
    const f = await setup(target({ pool: "pool-a" }));
    f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: true }] });
    f.harness.setMetrics({ targets_runtime: [] });
    await f.app.modules.targets.loadTargets();
    assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "paused");
    if (malformed) f.harness.setRoute("/admin/data/pools", { pools: {} });
    else f.harness.setRouteFailure("/admin/data/pools", true);
    await f.app.modules.targets.loadTargets();
    const health = f.app.modules.targets.targetHealth(f.app.state.targets[0]);
    assert.equal(health.key, "attention");
    assert.match(f.harness.ids.get("targetsTableBody").innerHTML, /部署池.*(?:失败|不可用)/);
    assert.equal(f.app.state.pools[0].name, "pool-a", "cached options remain available for drafts");
    f.harness.setMetrics({ targets_runtime: [{ name: "sample", phase: "offline", paused: true }] });
    await f.app.modules.targets.loadTargets();
    const paused = f.app.modules.targets.targetHealth(f.app.state.targets[0]);
    assert.equal(paused.key, "paused", "fresh runtime remains authoritative");
    assert.doesNotMatch(paused.reason, /所属部署池已暂停/);
    f.harness.setMetrics({ targets_runtime: [{ name: "sample", phase: "ready" }] });
    await f.app.modules.targets.loadTargets();
    assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "healthy");
    f.harness.setRouteFailure("/admin/data/pools", false);
    f.harness.setRoute("/admin/data/pools", { pools: [] });
    f.harness.setMetrics({ targets_runtime: [] });
    await f.app.modules.targets.loadTargets();
    assert.equal(f.app.state.pools.length, 0, "empty roster is a successful response");
    assert.doesNotMatch(f.harness.ids.get("targetsTableBody").innerHTML, /部署池数据不可用/);
    assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "attention");
    f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: true }] });
    await f.app.modules.targets.loadTargets();
    assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "paused");
  }
});

test("pool refresh failure preserves the editor selection and snapshot-protected save", async () => {
  const f = await setup(target({ pool: "pool-a" }));
  f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: true }] });
  await f.app.modules.targets.loadTargets();
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRouteFailure("/admin/data/pools", true);
  await f.app.modules.targets.loadTargets();
  assert.equal(f.harness.ids.get("tgtPool").value, "pool-a");
  assert.match(f.harness.ids.get("tgtPool").getAttribute("aria-label"), /不可用|过期/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /部署池数据不可用|选项可能过期/);
  await f.save();
  assert.equal(putBody(f.puts()[0]).pool, "pool-a");
  assert.equal(keyHeader(f.puts()[0]), "snapshot-1");
  assert.deepEqual(putBody(f.puts()[0]).api_keys, ["__existing_key_index__:2"]);
});

test("late pool refresh cannot replace a newer failed or successful response", async () => {
  let delay = false, release;
  const f = await setup(target({ pool: "pool-a" }), (url, opts, fallback) => {
    if (url === "/admin/data/pools" && delay) return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  f.harness.setMetrics({ targets_runtime: [] });
  delay = true;
  const older = f.app.modules.targets.loadTargets();
  for (let i = 0; i < 30 && !release; i++) await Promise.resolve();
  assert.ok(release);
  delay = false;
  f.harness.setRouteFailure("/admin/data/pools", true);
  await f.app.modules.targets.loadTargets();
  release(response({ pools: [{ name: "pool-a", paused: true }] }));
  await older;
  assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "attention");
  assert.match(f.harness.ids.get("targetsTableBody").innerHTML, /部署池.*(?:失败|不可用)/);
});

test("late failure cannot override a newer roster or user pool selection", async () => {
  let delay = false, release;
  const f = await setup(target({ pool: "pool-a" }), (url, opts, fallback) => {
    if (url === "/admin/data/pools" && delay) return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a" }, { name: "pool-b" }] });
  await f.app.modules.targets.loadTargets();
  await f.edit();
  delay = true;
  const older = f.app.modules.targets.loadTargets();
  for (let i = 0; i < 30 && !release; i++) await Promise.resolve();
  assert.ok(release);
  f.harness.ids.get("tgtPool").value = "pool-b";
  delay = false;
  f.harness.setMetrics({ targets_runtime: [] });
  f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: true }] });
  await f.app.modules.targets.loadTargets();
  release(response({ error: "unavailable" }, 503));
  await older;
  assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "paused");
  assert.equal(f.harness.ids.get("tgtPool").value, "pool-b");
  assert.match(f.harness.ids.get("tgtPool").getAttribute("aria-label"), /过期/);
  await f.save();
  assert.equal(putBody(f.puts()[0]).pool, "pool-b");
});

test("readonly users see unavailable pools without opening an editor", async () => {
  const harness = createHarness({ role: "readonly", routes: {
    "/admin/data/targets": { targets: [target({ pool: "pool-a" })] },
    "/admin/data/pools": { pools: [{ name: "pool-a", paused: true }] },
  } });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  harness.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  assert.equal(app.modules.targets.targetHealth(app.state.targets[0]).key, "attention");
  assert.match(harness.ids.get("targetsTableBody").innerHTML, /部署池数据不可用/);
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  assert.equal(harness.ids.get("targetModal").classList.contains("open"), false);
});

test("target details distinguish readonly hidden fields from admin configured and empty fields", async () => {
  for (const role of ["readonly", "admin"]) {
    const configured = role === "readonly"
      ? target({ endpoint: "https://upstream.example", custom_headers: null, custom_body: null })
      : target({ endpoint: "https://upstream.example/private/path", custom_headers: { "X-Example": "private-header" }, custom_body: { option: "private-body" } });
    const harness = createHarness({ role, routes: { "/admin/data/targets": { targets: [configured] }, "/admin/data/pools": { pools: [] } } });
    const app = await harness.run();
    await app.modules.targets.loadTargets();
    app.modules.targets.openTargetDetail("sample");
    const advanced = harness.ids.get("targetDetailAdvanced").innerHTML;
    const overview = harness.ids.get("targetDetailOverview").innerHTML;
    const table = harness.ids.get("targetsTableBody").innerHTML;
    if (role === "readonly") {
      assert.match(advanced, /自定义请求头<\/dt><dd>[^<]*不可查看/);
      assert.match(advanced, /自定义请求体<\/dt><dd>[^<]*不可查看/);
      assert.match(overview, /https:\/\/upstream\.example.*仅显示协议和主机/);
      assert.match(table, /https:\/\/upstream\.example.*仅显示协议和主机/);
      app.state.targets[0].custom_headers = { "X-Example": "private-header" };
      app.state.targets[0].custom_body = { option: "private-body" };
      app.modules.targets.openTargetDetail("sample");
      assert.doesNotMatch(harness.ids.get("targetDetailAdvanced").innerHTML, /X-Example|option|private-header|private-body/);
    } else {
      assert.match(advanced, /X-Example/);
      assert.match(advanced, /option/);
      assert.doesNotMatch(advanced, /private-header|private-body|不可查看/);
      assert.match(overview, /https:\/\/upstream\.example\/private\/path/);
      assert.doesNotMatch(overview, /仅显示协议和主机/);
      assert.doesNotMatch(table, /仅显示协议和主机/);
      app.state.targets[0].custom_headers = {};
      app.state.targets[0].custom_body = null;
      app.modules.targets.openTargetDetail("sample");
      assert.match(harness.ids.get("targetDetailAdvanced").innerHTML, /自定义请求头<\/dt><dd>未配置/);
      assert.match(harness.ids.get("targetDetailAdvanced").innerHTML, /自定义请求体<\/dt><dd>未配置/);
      app.state.targets[0].endpoint = "";
      app.modules.targets.renderTargets();
      assert.match(harness.ids.get("targetDetailOverview").innerHTML, /Endpoint<\/dt><dd>未配置/);
      assert.match(harness.ids.get("targetsTableBody").innerHTML, /未配置 Endpoint/);
    }
  }
});

test("readonly unavailable endpoint is not described as unconfigured", async () => {
  const harness = createHarness({ role: "readonly", routes: {
    "/admin/data/targets": { targets: [target({ endpoint: "", custom_headers: null, custom_body: null })] },
    "/admin/data/pools": { pools: [] },
  } });
  const app = await harness.run();
  await app.modules.targets.loadTargets();
  app.modules.targets.openTargetDetail("sample");
  assert.match(harness.ids.get("targetDetailOverview").innerHTML, /Endpoint<\/dt><dd>[^<]*不可查看/);
  assert.match(harness.ids.get("targetsTableBody").innerHTML, /Endpoint 不可查看/);
  assert.doesNotMatch(harness.ids.get("targetsTableBody").innerHTML, /未配置 Endpoint/);
});

test("initially unavailable roster preserves saved membership and permits explicit unbinding", async () => {
  const f = await setup(target({ pool: "missing-from-cache" }), (url, opts, fallback) =>
    url === "/admin/data/pools" ? Promise.resolve(response({}, 503)) : fallback(url, opts));
  await f.edit();
  assert.equal(f.harness.ids.get("tgtPool").value, "missing-from-cache");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /部署池数据不可用/);
  f.harness.ids.get("tgtPool").value = "";
  await f.save();
  assert.equal(putBody(f.puts()[0]).pool, "");
});

test("cross-tab older responses cannot replace the newest targets, pools, runtime or key draft", async (t) => {
  for (const olderTab of ["pools", "targets"]) {
    for (const oldResult of ["success", "failure", "invalid"]) {
      await t.test(`${olderTab} then other tab, old ${oldResult}`, async () => {
        let delay = false, release;
        const delayedURL = oldResult === "success" ? "/admin/metrics" : "/admin/data/pools";
        const f = await setup(target({ pool: "pool-a" }), (url, opts, fallback) => {
          if (delay && url === delayedURL) return new Promise(resolve => { release = resolve; });
          return fallback(url, opts);
        });
        await f.edit();
        f.app.modules.targets.removeKeyFromList(1);
        f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
        f.harness.setMetrics({ targets_runtime: [{ name: "sample", phase: "offline", paused: true }] });
        f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: true }] });
        delay = true;
        const older = olderTab === "pools" ? f.app.modules.pools.loadPools() : f.app.modules.targets.loadTargets();
        assert.ok(release);
        delay = false;
        f.harness.setMetrics({ targets_runtime: [{ name: "sample", phase: "ready" }] });
        f.harness.setRoute("/admin/data/targets", { targets: [target({ pool: "pool-a", key_snapshot: "snapshot-2" })] });
        f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: false }] });
        if (olderTab === "pools") await f.app.modules.targets.loadTargets();
        else await f.app.modules.pools.loadPools();
        release(oldResult === "success"
          ? response({ targets_runtime: [{ name: "sample", phase: "offline", paused: true }] })
          : oldResult === "failure" ? response({ error: "offline" }, 503) : response({ pools: {} }));
        await older;
        assert.equal(f.app.state.targets[0].key_snapshot, "snapshot-2");
        assert.equal(f.app.state.runtime.sample.phase, "ready");
        assert.equal(f.app.state.pools[0].paused, false);
        assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "healthy");
        assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
        assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
        assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
        await f.save();
        assert.equal(f.puts().length, 0, "new snapshot cannot be paired with an old draft");
      });
    }
  }
});

test("a newer failed pool refresh remains visible after an older target success", async () => {
  let delay = false, release;
  const f = await setup(target({ pool: "pool-a" }), (url, opts, fallback) => {
    if (delay && url === "/admin/metrics") return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  delay = true;
  const older = f.app.modules.targets.loadTargets();
  assert.ok(release);
  delay = false;
  f.harness.setRouteFailure("/admin/data/pools", true);
  await f.app.modules.pools.loadPools();
  release(response({ targets_runtime: [{ name: "sample", phase: "offline" }] }));
  await older;
  assert.match(f.harness.ids.get("poolsTableBody").innerHTML, /加载失败/);
  assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "attention");
});

test("a newer pool snapshot arriving during a key operation cannot authorize the older draft", async () => {
  let delay = false, release;
  const f = await setup(target({ pool: "pool-a" }), (url, opts, fallback) => {
    if (url.endsWith("/keys/0/block")) return Promise.resolve(response({ ok: true, key_snapshot: "snapshot-2" }));
    if (delay && url === "/admin/data/targets") return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  delay = true;
  const pending = f.action({ blockKeyIdx: "0" });
  for (let i = 0; i < 30 && !release; i++) await Promise.resolve();
  assert.ok(release);
  delay = false;
  f.harness.setRoute("/admin/data/targets", { targets: [target({ pool: "pool-a", key_snapshot: "snapshot-3" })] });
  await f.app.modules.pools.loadPools();
  release(response({ targets: [target({ pool: "pool-a", key_snapshot: "snapshot-2" })] }));
  await pending;
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /重新.*确认|确认.*重新/);
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  await f.save();
  assert.equal(f.puts().length, 1, "only the key operation may be sent");
});

test("invalid pool-page members retain the cached draft and report unknown membership", async () => {
  const f = await setup(target({ pool: "pool-a" }));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a" }] });
  f.harness.setRoute("/admin/data/targets", { targets: [null] });
  await f.app.modules.pools.loadPools();
  assert.equal(f.app.state.targets[0].key_snapshot, "snapshot-1");
  assert.equal(f.harness.ids.get("tgtPool").value, "pool-a");
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  assert.match(f.harness.ids.get("poolsTableBody").innerHTML, /加载失败/);
  assert.doesNotMatch(f.harness.ids.get("poolsTableBody").innerHTML, /无成员/);
  assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "attention");
});

test("late standalone metrics cannot overwrite a newer pool refresh", async () => {
  let delay = false, release;
  const f = await setup(target({ pool: "pool-a" }), (url, opts, fallback) => {
    if (delay && url === "/admin/metrics") return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  delay = true;
  const older = f.app.modules.targets.fetchSchedulingRuntime();
  assert.ok(release);
  delay = false;
  f.harness.setMetrics({ targets_runtime: [{ name: "sample", phase: "ready" }] });
  f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a" }] });
  await f.app.modules.pools.loadPools();
  release(response({ targets_runtime: [{ name: "sample", phase: "offline" }] }));
  await older;
  assert.equal(f.app.state.runtime.sample.phase, "ready");
});
