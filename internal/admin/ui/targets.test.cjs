"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

function status(index, mask, blocked = false) {
  return { index, key_mask: mask, blocked, exhausted: false, status: blocked ? "已屏蔽" : "等待中" };
}
function target(overrides = {}) {
  return {
    name: "sample", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", image_operation: "image-new",
    usage_adapter: "standard", has_api_key: true, api_key: "aaaa...1111",
    api_keys: ["bbbb...2222", "cccc...3333"], etag: "t1-5", key_ids: ["kid-a", "kid-b", "kid-c"],
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

test("block refresh keeps removals and later additions, and the save still uses the draft revision", async () => {
  let releaseFetch;
  let delayRefresh = false;
  const f = await setup(target(), (url, opts, fallback) => {
    if (url.endsWith("/keys/id/kid-a/block")) return Promise.resolve(response({ ok: true }));
    if (url === "/admin/data/targets" && delayRefresh) return new Promise(resolve => { releaseFetch = resolve; });
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  delayRefresh = true;
  const pending = f.action({ blockKeyId: "kid-a" });
  for (let i = 0; i < 30 && !releaseFetch; i++) await Promise.resolve();
  assert.ok(releaseFetch, "block waits for the refreshed target");
  f.harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  f.app.modules.targets.addKeyToList();
  releaseFetch(response({ targets: [target({ key_statuses: [status(0, "aaaa...1111", true), status(1, "bbbb...2222"), status(2, "cccc...3333")] })] }));
  await pending;
  delayRefresh = false;
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.doesNotMatch(cards, /bbbb\.\.\.2222/, "removed key is not restored by refresh");
  assert.match(cards, /cccc\.\.\.3333/, "other saved key stays");
  assert.match(cards, /temp/, "draft key added while refreshing stays");
  await f.save();
  const [block, update] = f.puts();
  assert.equal(block.url, "/admin/data/targets/sample/keys/id/kid-a/block");
  assert.equal(putBody(update).api_key, "__existing_key_id__:kid-a");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_id__:kid-c", "temporary-test-key"]);
  assert.equal(update.options.headers["If-Match"], '"t1-5"');
});

test("a runtime reorder elsewhere cannot misattribute a draft: saved keys are referenced by ID", async () => {
  const masks = target({ api_key: "****", api_keys: ["****", "****"], key_statuses: [status(0, "****"), status(1, "****"), status(2, "****")] });
  const f = await setup(masks);
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRoute("/admin/data/targets", { targets: [target({ ...masks, key_runtime_indices: [2, 0, 1] })] });
  await f.app.modules.targets.loadTargets();
  await f.save();
  const update = f.puts()[0];
  assert.equal(putBody(update).api_key, "__existing_key_id__:kid-a");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_id__:kid-c"]);
});

test("a failed status refresh after blocking keeps the draft, flags stale status and still allows saving", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/keys/id/kid-a/block")
    ? Promise.resolve(response({ ok: true })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.action({ blockKeyId: "kid-a" });
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.doesNotMatch(cards, /bbbb\.\.\.2222/, "the removed key is not restored");
  assert.match(cards, /状态刷新失败/);
  assert.doesNotMatch(cards, /data-(?:un)?block-key-id/, "stale status offers no block actions");
  await f.save();
  const update = f.puts()[1];
  assert.equal(update.options.headers["If-Match"], '"t1-5"');
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_id__:kid-c"]);
});


test("equal masked slots are not deduplicated and each card keeps its own key ID", async () => {
  const slots = target({ has_api_key: false, api_key: "", api_keys: ["****", "****"], allow_bearer_passthrough: true,
    key_ids: [null, "kid-b", "kid-c"], key_runtime_indices: [null, 0, 1], key_statuses: [status(0, "first-mask"), status(1, "second-mask")] });
  const f = await setup(slots, (url, opts, fallback) => url.endsWith("/block")
    ? Promise.resolve(response({ ok: true })) : fallback(url, opts));
  await f.edit();
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.match(cards, /first-mask/);
  assert.match(cards, /second-mask/, "equal masked configuration values are not deduplicated");
  assert.match(cards, /data-block-key-id='kid-b'/);
  assert.match(cards, /data-block-key-id='kid-c'/);
  await f.action({ blockKeyId: "kid-c" });
  await f.save();
  const [block, update] = f.puts();
  assert.equal(block.url, "/admin/data/targets/sample/keys/id/kid-c/block");
  assert.equal(putBody(update).api_key, "", "empty primary slot remains empty");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_id__:kid-b", "__existing_key_id__:kid-c"]);
});

test("unusable key IDs make the key list read-only but other fields still save without any key field", async () => {
  const f = await setup(target({ key_ids: null }));
  await f.edit();
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.match(cards, /密钥列表暂不可编辑，其他字段可正常保存/);
  assert.match(cards, /aaaa\.\.\.1111/, "keys are still displayed");
  assert.doesNotMatch(cards, /data-block-key-id/, "no ID, no block action");
  f.app.modules.targets.removeKeyFromList(0);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /aaaa\.\.\.1111/, "removal is refused");
  f.harness.ids.get("tgtNewKeyInput").value = "typed-key";
  f.app.modules.targets.addKeyToList();
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /typed/, "adding is refused");
  f.harness.ids.get("tgtEndpoint").value = "https://other-field.invalid";
  await f.save();
  assert.equal(f.puts().length, 1);
  const body = putBody(f.puts()[0]);
  assert.equal(body.endpoint, "https://other-field.invalid");
  assert.equal("api_key" in body, false, "no api_key field at all: the server keeps the stored keys");
  assert.equal("api_keys" in body, false, "no api_keys field at all");
  assert.equal(ifMatch(f.puts()[0]), '"t1-5"');
  assert.ok(f.messages.some((m) => m.msg === "目标已更新" && m.kind === "success"));
});

test("malformed key ID lists never produce a PUT carrying key fields", async () => {
  for (const bad of [
    target({ key_ids: ["kid-a"] }),
    target({ key_ids: ["kid-a", "", "kid-c"] }),
    target({ key_ids: ["kid-a", 7, "kid-c"] }),
    target({ key_ids: "kid-a" }),
    target({ key_ids: undefined }),
    target({ key_ids: [null, "kid-b", "kid-c"] }), // null ID on a slot that has a real key
    target({ key_ids: ["kid-a", "kid-b", null] }),
    target({ api_keys: undefined, key_ids: ["kid-a"] }),
  ]) {
    const f = await setup(bad);
    await f.edit();
    await f.save();
    assert.equal(f.puts().length, 1, "other fields remain saveable");
    const body = putBody(f.puts()[0]);
    assert.ok(!("api_key" in body) && !("api_keys" in body), "unverified key identity must not carry key fields");
    assert.match(f.harness.ids.get("keyCardList").innerHTML, /密钥列表暂不可编辑/);
  }
});

test("unblock addresses the key by ID even when runtime indices are permuted", async () => {
  const initial = target({ key_runtime_indices: [2, 0, 1], key_statuses: [status(0, "bbbb...2222", true), status(1, "cccc...3333"), status(2, "aaaa...1111")] });
  const f = await setup(initial);
  await f.edit();
  f.app.modules.targets.removeKeyFromList(2);
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.match(cards, /data-unblock-key-id='kid-b'/, "the blocked runtime entry belongs to the second saved key");
  await f.action({ unblockKeyId: "kid-b" });
  await f.save();
  const [unblock, update] = f.puts();
  assert.equal(unblock.url, "/admin/data/targets/sample/keys/id/kid-b/unblock");
  assert.equal(unblock.options.headers["X-Target-Key-Snapshot"], undefined);
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_id__:kid-b"]);
});

test("a rejected block reports the error, is not retried and does not block saving", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/block")
    ? Promise.resolve(response({ error: "unknown key" }, 400)) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  await f.action({ blockKeyId: "kid-a" });
  assert.ok(f.messages.some(m => /屏蔽失败: unknown key/.test(m.msg) && m.kind === "error"));
  await f.save();
  assert.equal(f.puts().length, 2, "one block attempt, then the save");
  assert.deepEqual(putBody(f.puts()[1]).api_keys, ["__existing_key_id__:kid-c"]);
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

test("a model suggestion cannot add a mapping while a save is in flight", async () => {
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
  assert.ok(!f.harness.ids.get("model-mappings-container").children.some(row => row.querySelector(".upstream-input")?.value === "suggested-model"),
    "the form is read-only while saving");
  release(response({ ok: true }));
  await pending;
  assert.ok(f.messages.some(m => m.msg === "目标已更新" && m.kind === "success"));
  assert.ok(!f.messages.some(m => /未保存/.test(m.msg)));
});


test("a later successful refresh restores status actions without restoring removed keys", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/block")
    ? Promise.resolve(response({ ok: true })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.action({ blockKeyId: "kid-a" });
  f.harness.setRouteFailure("/admin/data/targets", false);
  f.harness.setRoute("/admin/data/targets", { targets: [target({ key_statuses: [status(0, "aaaa...1111", true), status(1, "bbbb...2222"), status(2, "cccc...3333")] })] });
  await f.app.modules.targets.loadTargets();
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.doesNotMatch(cards, /状态刷新失败|bbbb\.\.\.2222/);
  assert.match(cards, /data-unblock-key-id='kid-a'/);
});


test("while reloading, saved-key removal is disabled and the reload replaces the draft", async () => {
  let delay = false, release;
  const f = await setup(target(), (url, opts, fallback) => {
    if (url === "/admin/data/targets" && delay) return new Promise(resolve => { release = resolve; });
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  delay = true;
  const pending = f.action({ reloadTarget: "1" });
  for (let i = 0; i < 30 && !release; i++) await Promise.resolve();
  assert.ok(release);
  const before = f.harness.ids.get("keyCardList").innerHTML;
  f.app.modules.targets.removeKeyFromList(0);
  assert.equal(f.harness.ids.get("keyCardList").innerHTML, before, "saved-key removal is disabled until the reload completes");
  f.harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  f.app.modules.targets.addKeyToList();
  release(response({ targets: [target({ etag: "t1-6" })] }));
  await pending;
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /temp/, "a key typed in before the reload is discarded with the draft");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/, "removal choices are reset by the reload");
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
  f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-3" })] });
  await f.app.modules.targets.loadTargets();
  release(response({ targets: [target({ etag: "t1-2" })] }));
  await older;
  assert.equal(f.app.state.targets[0].etag, "t1-3");
});

test("a late save response cannot close a newly opened editor", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets/sample" && opts?.method === "PUT"
    ? new Promise(resolve => { release = resolve; }) : fallback(url, opts));
  await f.edit();
  const pending = f.save();
  assert.ok(release);
  await f.app.modules.targets.openTargetModal(target({ name: "other" }));
  f.harness.ids.get("tgtEndpoint").value = "https://other-draft.invalid";
  release(response({ ok: true }));
  await pending;
  assert.equal(f.app.state.editingTarget?.name, "other");
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://other-draft.invalid");
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

test("pool refresh failure preserves the editor selection and the revision-protected save", async () => {
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
  assert.equal(f.puts()[0].options.headers["If-Match"], '"t1-5"');
  assert.deepEqual(putBody(f.puts()[0]).api_keys, ["__existing_key_id__:kid-c"]);
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

test("cross-tab older responses cannot replace the newest targets, pools or runtime, nor the key draft", async (t) => {
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
        f.harness.setRoute("/admin/data/targets", { targets: [target({ pool: "pool-a", etag: "t1-6" })] });
        f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a", paused: false }] });
        if (olderTab === "pools") await f.app.modules.targets.loadTargets();
        else await f.app.modules.pools.loadPools();
        release(oldResult === "success"
          ? response({ targets_runtime: [{ name: "sample", phase: "offline", paused: true }] })
          : oldResult === "failure" ? response({ error: "offline" }, 503) : response({ pools: {} }));
        await older;
        assert.equal(f.app.state.targets[0].etag, "t1-6");
        assert.equal(f.app.state.runtime.sample.phase, "ready");
        assert.equal(f.app.state.pools[0].paused, false);
        assert.equal(f.app.modules.targets.targetHealth(f.app.state.targets[0]).key, "healthy");
        assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
        assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
        assert.match(f.harness.ids.get("keyCardList").innerHTML, /已在别处修改/, "the newer revision is surfaced, not applied to the draft");
        await f.save();
        assert.equal(f.puts().length, 1, "the server arbitrates the save");
        assert.equal(f.puts()[0].options.headers["If-Match"], '"t1-5"', "the draft keeps the revision it was based on");
        assert.deepEqual(putBody(f.puts()[0]).api_keys, ["__existing_key_id__:kid-c"]);
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


test("invalid pool-page members retain the cached draft and report unknown membership", async () => {
  const f = await setup(target({ pool: "pool-a" }));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.setRoute("/admin/data/pools", { pools: [{ name: "pool-a" }] });
  f.harness.setRoute("/admin/data/targets", { targets: [null] });
  await f.app.modules.pools.loadPools();
  assert.equal(f.app.state.targets[0].etag, "t1-5");
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

// ===== Revision / stable key ID contract =====
// Saving is guarded by the server (If-Match + __existing_key_id__), not by a local snapshot.
const isTargetPut = (url, opts) => url === "/admin/data/targets/sample" && opts?.method === "PUT";
const ifMatch = (request) => request.options.headers["If-Match"];
const clickRow = (f, dataset) => {
  for (const fn of f.harness.ids.get("targetsTableBody").listeners.click) fn({ target: { dataset, closest: () => null } });
};
const messageTexts = (f) => f.messages.map((m) => m.msg);

test("revision: save sends If-Match and stable key IDs, never the snapshot header", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? Promise.resolve(response({ ok: true, etag: "t1-6" })) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  await f.save();
  const [update] = f.puts();
  assert.equal(ifMatch(update), '"t1-5"');
  assert.equal(update.options.headers["X-Target-Key-Snapshot"], undefined);
  assert.equal(putBody(update).api_key, "__existing_key_id__:kid-a");
  assert.deepEqual(putBody(update).api_keys, ["__existing_key_id__:kid-c"]);
  assert.ok(f.messages.some((m) => m.msg === "目标已更新" && m.kind === "success"));
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), false);
});

test("revision: a target without a usable etag is never saved blind", async () => {
  const f = await setup(target({ etag: undefined }));
  await f.edit();
  await f.save();
  assert.equal(f.puts().length, 0, "no PUT without an If-Match precondition");
  assert.ok(f.messages.some((m) => /版本信息缺失/.test(m.msg) && m.kind === "error"));
});

test("revision: key IDs, not snapshot or runtime-index fields, decide whether editing is possible", async () => {
  const f = await setup(target({ key_runtime_indices: null, key_statuses: null }));
  await f.edit();
  await f.save();
  assert.equal(f.puts().length, 1, "missing runtime fields never block a save");
  assert.deepEqual(putBody(f.puts()[0]).api_keys, ["__existing_key_id__:kid-b", "__existing_key_id__:kid-c"]);
});

test("revision: an empty primary slot stays empty and empty extra slots are not sent", async () => {
  const f = await setup(target({ has_api_key: false, api_key: "", api_keys: ["****", "****"],
    key_ids: [null, "kid-b", null], key_runtime_indices: [null, 0, null], key_statuses: [status(0, "first-mask")] }));
  await f.edit();
  await f.save();
  const body = putBody(f.puts()[0]);
  assert.equal(body.api_key, "", "empty primary slot is not shifted onto an extra key");
  assert.deepEqual(body.api_keys, ["__existing_key_id__:kid-b"]);
});

test("revision: another target changing never blocks saving the one being edited", async () => {
  const f = await setup(target());
  await f.edit();
  f.harness.setRoute("/admin/data/targets", { targets: [target(), target({ name: "other", etag: "t1-9", paused: true })] });
  await f.app.modules.targets.loadTargets();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.equal(f.puts().length, 1);
  assert.equal(ifMatch(f.puts()[0]), '"t1-5"');
  assert.ok(f.messages.some((m) => m.msg === "目标已更新" && m.kind === "success"));
  assert.ok(!messageTexts(f).some((m) => /保存失败|快照|未保存/.test(m)), "no misleading error: " + messageTexts(f));
  assert.ok(!/已在别处修改/.test(f.harness.ids.get("keyCardList").innerHTML), "another target changing is not a change to this one");
});

test("revision: the edited target changing elsewhere shows a non-blocking warning and still saves with the draft revision", async () => {
  const f = await setup(target());
  await f.edit();
  f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-6", endpoint: "https://elsewhere.invalid" })] });
  await f.app.modules.targets.loadTargets();
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /已在别处修改/);
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.equal(f.puts().length, 1, "the server arbitrates, the UI does not block");
  assert.equal(ifMatch(f.puts()[0]), '"t1-5"', "the draft was based on revision 5");
});

test("revision: a 409 conflict keeps the modal and draft, explains, and is not retried", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? Promise.resolve(response({ error: "target changed; refresh targets", code: "target_revision_conflict" }, 409)) : fallback(url, opts));
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.equal(f.puts().length, 1);
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.doesNotMatch(f.harness.ids.get("keyCardList").innerHTML, /bbbb\.\.\.2222/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /该目标已被修改，请重新加载后再保存/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /data-reload-target/);
  assert.ok(f.messages.some((m) => /该目标已被修改，请重新加载后再保存/.test(m.msg) && m.kind === "error"));
  assert.ok(!messageTexts(f).some((m) => /保存失败/.test(m)), "a conflict is not a generic failure");
  assert.equal(f.harness.ids.get("tgtEndpoint").disabled, false, "the form is editable again");
});

test("revision: reloading after a conflict replaces the whole form with the server content", async () => {
  let conflict = true;
  const f = await setup(target(), (url, opts, fallback) => {
    if (isTargetPut(url, opts) && conflict) {
      return Promise.resolve(response({ error: "target changed; refresh targets", code: "target_revision_conflict" }, 409));
    }
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.ids.get("tgtNewKeyInput").value = "temporary-test-key";
  f.app.modules.targets.addKeyToList();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  conflict = false;
  f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-8", endpoint: "https://theirs.invalid", key_ids: ["kid-a", "kid-b2", "kid-c"] })] });
  await f.action({ reloadTarget: "1" });
  const cards = f.harness.ids.get("keyCardList").innerHTML;
  assert.doesNotMatch(cards, /该目标已被修改/, "the conflict notice is cleared");
  assert.match(cards, /bbbb\.\.\.2222/, "saved keys come back from the fresh target");
  assert.doesNotMatch(cards, /temp/, "unsaved new keys are discarded with the draft");
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://theirs.invalid", "the form shows the server's content, not the old draft");
  await f.save();
  const retry = f.puts()[1];
  assert.equal(ifMatch(retry), '"t1-8"');
  assert.equal(putBody(retry).endpoint, "https://theirs.invalid");
  assert.deepEqual(putBody(retry).api_keys, ["__existing_key_id__:kid-b2", "__existing_key_id__:kid-c"]);
});

test("revision: the form is disabled while saving and restored afterwards, and a late edit cannot turn success red", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? new Promise((resolve) => { release = resolve; }) : fallback(url, opts));
  await f.edit();
  const ids = f.harness.ids;
  const pending = f.save();
  assert.ok(release);
  for (const id of ["tgtName", "tgtEndpoint", "tgtNewKeyInput", "addKeyBtn", "saveTargetBtn", "tgtPool"]) {
    assert.equal(ids.get(id).disabled, true, id + " is disabled while saving");
  }
  assert.ok(ids.get("targetModal").querySelectorAll("input, select, textarea, button").every((el) => el.disabled),
    "every control in the modal is locked");
  assert.match(ids.get("keyCardList").innerHTML, /disabled/, "key card actions are locked too");
  // Even if an input event sneaks through, success stays green.
  for (const fn of ids.get("targetModal").listeners.input || []) fn({ target: ids.get("tgtEndpoint") });
  release(response({ ok: true, etag: "t1-6" }));
  await pending;
  assert.ok(f.messages.some((m) => m.msg === "目标已更新" && m.kind === "success"));
  assert.ok(!messageTexts(f).some((m) => /后续|未保存|保存失败/.test(m)), "no red misreport: " + messageTexts(f));
  assert.equal(ids.get("targetModal").classList.contains("open"), false);
  assert.ok(ids.get("targetModal").querySelectorAll("input, select, textarea, button").every((el) => !el.disabled),
    "controls are enabled again after the modal closes");
});

test("revision: a failed save unlocks the form and reports 保存失败", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? Promise.resolve(response({ error: "boom" }, 400)) : fallback(url, opts));
  await f.edit();
  await f.save();
  assert.ok(f.messages.some((m) => m.msg === "保存失败: boom" && m.kind === "error"));
  assert.equal(f.harness.ids.get("tgtEndpoint").disabled, false);
  assert.equal(f.harness.ids.get("saveTargetBtn").disabled, false);
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
});

test("revision: a successful save followed by a failed refresh never reports 保存失败", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? Promise.resolve(response({ ok: true, etag: "t1-6" })) : fallback(url, opts));
  await f.edit();
  f.harness.setRouteFailure("/admin/data/targets", true);
  await f.save();
  assert.ok(f.messages.some((m) => m.msg === "目标已更新" && m.kind === "success"));
  assert.ok(!messageTexts(f).some((m) => /保存失败/.test(m)), "refresh trouble is not a save failure: " + messageTexts(f));
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), false);
});

test("revision: creating a target succeeds green and the form is locked while the POST is pending", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => url === "/admin/data/targets" && opts?.method === "POST"
    ? new Promise((resolve) => { release = resolve; }) : fallback(url, opts));
  await f.app.modules.targets.openTargetModal(null);
  const ids = f.harness.ids;
  ids.get("tgtName").value = "fresh";
  ids.get("tgtEndpoint").value = "https://fresh.invalid";
  ids.get("tgtUsageAdapter").value = "standard";
  const pending = f.save();
  assert.ok(release);
  assert.equal(ids.get("tgtEndpoint").disabled, true);
  release(response({ ok: true, etag: "t1-1" }));
  await pending;
  assert.ok(f.messages.some((m) => m.msg === "目标已创建" && m.kind === "success"));
  assert.equal(ids.get("targetModal").classList.contains("open"), false);
});

test("revision: blocking a key uses the by-ID route without snapshot headers and keeps the edit revision", async () => {
  const f = await setup(target(), (url, opts, fallback) => url.endsWith("/block")
    ? Promise.resolve(response({ ok: true })) : fallback(url, opts));
  await f.edit();
  f.harness.setRoute("/admin/data/targets", { targets: [target({ key_statuses: [status(0, "aaaa...1111", true), status(1, "bbbb...2222"), status(2, "cccc...3333")] })] });
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /data-block-key-id='kid-a'/);
  await f.action({ blockKeyId: "kid-a" });
  const [block] = f.puts();
  assert.equal(block.url, "/admin/data/targets/sample/keys/id/kid-a/block");
  assert.equal(block.options.headers["X-Target-Key-Snapshot"], undefined);
  assert.equal(block.options.headers["If-Match"], undefined);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /data-unblock-key-id='kid-a'/, "status refreshed");
  await f.save();
  assert.equal(ifMatch(f.puts()[1]), '"t1-5"', "blocking does not move the edit revision");
});

test("revision: unblocking uses the by-ID unblock route", async () => {
  const f = await setup(target({ key_statuses: [status(0, "aaaa...1111", true), status(1, "bbbb...2222"), status(2, "cccc...3333")] }));
  await f.edit();
  await f.action({ unblockKeyId: "kid-a" });
  assert.equal(f.puts()[0].url, "/admin/data/targets/sample/keys/id/kid-a/unblock");
});

test("revision: the list sends If-Match for pause, resume and delete", async () => {
  const f = await setup(target({ etag: "t1-7" }));
  const click = (dataset) => clickRow(f, dataset);
  // showConfirm is stubbed in setup; run the confirmed action for pause and delete.
  let confirmation;
  f.app.modules.core.showConfirm = (msg, cb) => { confirmation = cb; };
  click({ pauseTgt: "sample" });
  await confirmation();
  click({ delTgt: "sample" });
  await confirmation();
  click({ resumeTgt: "sample" });
  await new Promise((resolve) => setTimeout(resolve, 0));
  await Promise.resolve();
  const writes = f.harness.requests.filter((r) => ["PUT", "DELETE"].includes(r.options?.method));
  assert.deepEqual(writes.map((r) => r.url), [
    "/admin/data/targets/sample/pause", "/admin/data/targets/sample", "/admin/data/targets/sample/resume"]);
  for (const write of writes) assert.equal(ifMatch(write), '"t1-7"', write.url);
});

test("revision: a list action hitting a revision conflict tells the user and refreshes the list", async () => {
  const f = await setup(target({ etag: "t1-7" }), (url, opts, fallback) => url.endsWith("/pause")
    ? Promise.resolve(response({ error: "target changed; refresh targets", code: "target_revision_conflict" }, 409)) : fallback(url, opts));
  let confirmation;
  f.app.modules.core.showConfirm = (msg, cb) => { confirmation = cb; };
  clickRow(f, { pauseTgt: "sample" });
  const before = f.harness.requests.filter((r) => r.url === "/admin/data/targets" && !r.options?.method).length;
  f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-9" })] });
  await confirmation();
  assert.ok(f.messages.some((m) => /已被修改|已变化/.test(m.msg) && m.kind === "error"), messageTexts(f).join("|"));
  assert.ok(f.harness.requests.filter((r) => r.url === "/admin/data/targets" && !r.options?.method).length > before, "list reloaded");
  assert.equal(f.app.state.targets[0].etag, "t1-9");
});

test("revision: no snapshot machinery is left in the targets page", () => {
  const { pageSource } = require("../../../scripts/testing/admin_ui_scripts.cjs");
  const source = pageSource("targets");
  for (const word of ["usableKeySnapshot", "editingKeyNeedsReview", "editingKeySnapshot", "editingKeyIndices",
    "sameKeyIndices", "editingDraftRevision", "X-Target-Key-Snapshot", "__existing_key_index__", "key_snapshot"]) {
    assert.ok(!source.includes(word), word + " must be gone");
  }
});

// ===== Unconfirmed saves, late failures and server warnings =====
const networkFailure = () => Promise.reject(new TypeError("Failed to fetch"));

test("revision: a network error whose save did not land keeps the draft and says it can be retried", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts) ? networkFailure() : fallback(url, opts));
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(f.messages.some((m) => m.msg === "保存未完成，可重试" && m.kind === "warn"), JSON.stringify(f.messages));
  assert.ok(!messageTexts(f).some((m) => /保存失败/.test(m)));
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.equal(f.harness.ids.get("tgtEndpoint").disabled, false);
});

test("revision: a failure arriving after the editor moved on is still reported", async () => {
  let release;
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? new Promise((resolve) => { release = resolve; }) : fallback(url, opts));
  await f.edit();
  const pending = f.save();
  assert.ok(release);
  await f.app.modules.targets.openTargetModal(target({ name: "other" }));
  release(response({ error: "boom" }, 400));
  await pending;
  assert.ok(f.messages.some((m) => /sample/.test(m.msg) && /boom/.test(m.msg) && m.kind === "error"), JSON.stringify(f.messages));
  assert.equal(f.app.state.editingTarget?.name, "other", "the new editor is untouched");
});

test("revision: a saved target with a pending auth reload shows a warning, not a failure", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? Promise.resolve(response({ ok: true, etag: "t1-6", warning: "auth reload pending" })) : fallback(url, opts));
  await f.edit();
  await f.save();
  assert.ok(f.messages.some((m) => /已保存/.test(m.msg) && /auth reload pending/.test(m.msg) && m.kind === "warn"), JSON.stringify(f.messages));
  assert.ok(!messageTexts(f).some((m) => /保存失败/.test(m)));
  assert.ok(!f.messages.some((m) => m.kind === "success"));
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), false, "the data is saved, the modal closes");
});

test("revision: a row action with a pending auth reload warns instead of claiming plain success", async () => {
  const f = await setup(target({ etag: "t1-7" }), (url, opts, fallback) => url.endsWith("/pause")
    ? Promise.resolve(response({ ok: true, warning: "auth reload pending" })) : fallback(url, opts));
  let confirmation;
  f.app.modules.core.showConfirm = (msg, cb) => { confirmation = cb; };
  clickRow(f, { pauseTgt: "sample" });
  await confirmation();
  assert.ok(f.messages.some((m) => /已暂停目标/.test(m.msg) && /auth reload pending/.test(m.msg) && m.kind === "warn"), JSON.stringify(f.messages));
  assert.ok(!f.messages.some((m) => m.kind === "success"));
});

test("revision: a row action on a target without an etag is refused", async () => {
  const f = await setup(target({ etag: undefined }));
  clickRow(f, { resumeTgt: "sample" });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(f.harness.requests.filter((r) => r.options?.method === "PUT").length, 0);
  assert.ok(f.messages.some((m) => /缺少版本信息/.test(m.msg) && m.kind === "error"));
});

// ===== Unconfirmed outcomes are judged by the server's content =====
const htmlResponse = (statusCode) => ({
  ok: false, status: statusCode, text: async () => "<html><body>" + statusCode + " Gateway Timeout</body></html>",
});

test("unconfirmed: the classification covers timeouts, network errors, 5xx and non-JSON bodies, not plain 4xx", async () => {
  const f = await setup();
  const { isUnconfirmedOutcome } = f.app.modules.core;
  const named = (name, extra = {}) => Object.assign(new Error("x"), { name }, extra);
  assert.equal(isUnconfirmedOutcome(named("TimeoutError")), true);
  assert.equal(isUnconfirmedOutcome(named("NetworkError", { isNetworkError: true })), true);
  for (const status of [500, 502, 503, 504]) assert.equal(isUnconfirmedOutcome(named("Error", { status })), true, String(status));
  assert.equal(isUnconfirmedOutcome(named("Error", { status: 400, invalidBody: true })), true);
  assert.equal(isUnconfirmedOutcome(named("Error", { status: 400 })), false);
  assert.equal(isUnconfirmedOutcome(named("Error", { status: 409, code: "target_revision_conflict" })), false);
  assert.equal(isUnconfirmedOutcome(named("TypeError")), false, "a bug in page code is not a network failure");
  assert.equal(isUnconfirmedOutcome(null), false);
});

test("unconfirmed: api() marks error bodies that are not JSON", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts) ? Promise.resolve(htmlResponse(504)) : fallback(url, opts));
  let error;
  try { await f.app.modules.core.api("/admin/data/targets/sample", { method: "PUT", body: {} }); } catch (e) { error = e; }
  assert.equal(error.status, 504);
  assert.equal(error.invalidBody, true);
  assert.equal(f.app.modules.core.isUnconfirmedOutcome(error), true);
});

test("unconfirmed: a gateway 504 HTML page while the server did commit reads as saved", async () => {
  const f = await setup(target(), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-6", endpoint: "https://draft.invalid" })] });
      return Promise.resolve(htmlResponse(504));
    }
    return fallback(url, opts);
  });
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(f.messages.some((m) => m.msg === "已保存" && m.kind === "success"), JSON.stringify(f.messages));
  assert.ok(!f.messages.some((m) => m.kind === "error" || /保存失败/.test(m.msg)), JSON.stringify(f.messages));
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), false);
  assert.equal(f.app.state.targets[0].etag, "t1-6");
});

test("unconfirmed: a network error while someone else changed the target keeps the draft and enters the conflict flow", async () => {
  const f = await setup(target(), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-9", endpoint: "https://theirs.invalid" })] });
      return networkFailure();
    }
    return fallback(url, opts);
  });
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid", "the form is not replaced");
  assert.equal(f.app.state.editingTarget.etag, "t1-5", "still based on the revision the draft was made from");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /该目标已被修改，请重新加载后再保存/);
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /data-reload-target/);
  assert.ok(!messageTexts(f).some((m) => /保存失败|已保存/.test(m)), messageTexts(f).join("|"));
  assert.ok(!f.messages.some((m) => m.msg === "已保存"));
});

test("unconfirmed: a 503 that wrote nothing keeps the draft and says it can be retried", async () => {
  const f = await setup(target(), (url, opts, fallback) => isTargetPut(url, opts)
    ? Promise.resolve(response({ error: "unavailable" }, 503)) : fallback(url, opts));
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(f.messages.some((m) => m.msg === "保存未完成，可重试" && m.kind === "warn"), JSON.stringify(f.messages));
  assert.ok(!f.messages.some((m) => m.kind === "error"));
  assert.equal(f.harness.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true);
});

test("unconfirmed: a changed key list is not mistaken for the submitted one", async () => {
  const f = await setup(target(), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      // Committed, but with a key the draft had removed still present.
      f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-6", endpoint: "https://draft.invalid" })] });
      return Promise.resolve(htmlResponse(502));
    }
    return fallback(url, opts);
  });
  await f.edit();
  f.app.modules.targets.removeKeyFromList(1);
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(!f.messages.some((m) => m.msg === "已保存"), "the server still has kid-b, so the draft was not what was saved");
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /data-reload-target/);
});

test("unconfirmed: creating a target - found with the same content counts as created, other content is a name clash", async () => {
  for (const same of [true, false]) {
    const f = await setup(target({ name: "existing", etag: "t1-1" }), (url, opts, fallback) => {
      if (url === "/admin/data/targets" && opts?.method === "POST") {
        f.harness.setRoute("/admin/data/targets", { targets: [target({ name: "fresh-one", etag: "t1-2", endpoint: same ? "https://new.invalid" : "https://other.invalid", api_key: "sk-n...-new", key_ids: ["kid-n"], api_keys: [] })] });
        return networkFailure();
      }
      return fallback(url, opts);
    });
    await f.app.modules.targets.openTargetModal(null);
    f.harness.ids.get("tgtName").value = "fresh-one";
    f.harness.ids.get("tgtUsageAdapter").value = "standard";
    f.harness.ids.get("tgtEndpoint").value = "https://new.invalid";
    f.harness.ids.get("tgtNewKeyInput").value = "sk-new-key-new";
    f.harness.ids.get("addKeyBtn").listeners.click[0]();
    await f.save();
    if (same) {
      assert.ok(f.messages.some((m) => m.msg === "已保存" && m.kind === "success"), JSON.stringify(f.messages));
      assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), false);
    } else {
      assert.ok(f.messages.some((m) => /同名目标/.test(m.msg) && m.kind === "warn"), JSON.stringify(f.messages));
      assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), true, "the draft stays");
      assert.ok(!f.messages.some((m) => m.msg === "已保存"));
    }
  }
});

test("unconfirmed: a delete whose response was lost but which took effect reads as deleted", async () => {
  const f = await setup(target({ etag: "t1-7" }), (url, opts, fallback) => {
    if (opts?.method === "DELETE") {
      f.harness.setRoute("/admin/data/targets", { targets: [] });
      return networkFailure();
    }
    return fallback(url, opts);
  });
  let confirmation;
  f.app.modules.core.showConfirm = (msg, cb) => { confirmation = cb; };
  clickRow(f, { delTgt: "sample" });
  await confirmation();
  assert.ok(f.messages.some((m) => m.msg === "已删除目标: sample" && m.kind === "success"), JSON.stringify(f.messages));
  assert.ok(!f.messages.some((m) => m.kind === "error"));
  assert.equal(f.app.state.targets.length, 0, "the list was read back");
});

test("unconfirmed: a pause answered 503 that did not take effect warns and refreshes, never red", async () => {
  for (const effective of [false, true]) {
    const f = await setup(target({ etag: "t1-7" }), (url, opts, fallback) => {
      if (url.endsWith("/pause")) {
        if (effective) f.harness.setRoute("/admin/data/targets", { targets: [target({ etag: "t1-8", paused: true })] });
        return Promise.resolve(response({ error: "unavailable" }, 503));
      }
      return fallback(url, opts);
    });
    let confirmation;
    f.app.modules.core.showConfirm = (msg, cb) => { confirmation = cb; };
    clickRow(f, { pauseTgt: "sample" });
    const before = f.harness.requests.filter((r) => r.url === "/admin/data/targets" && !r.options?.method).length;
    await confirmation();
    assert.ok(!f.messages.some((m) => m.kind === "error"), JSON.stringify(f.messages));
    assert.ok(f.harness.requests.filter((r) => r.url === "/admin/data/targets" && !r.options?.method).length > before, "list read back");
    if (effective) assert.ok(f.messages.some((m) => m.msg === "已暂停目标: sample" && m.kind === "success"), JSON.stringify(f.messages));
    else assert.ok(f.messages.some((m) => m.msg === "操作结果未确认，列表已刷新" && m.kind === "warn"), JSON.stringify(f.messages));
  }
});

test("unconfirmed: a definite 4xx on a row action is still a red failure", async () => {
  const f = await setup(target({ etag: "t1-7" }), (url, opts, fallback) => url.endsWith("/pause")
    ? Promise.resolve(response({ error: "nope" }, 400)) : fallback(url, opts));
  let confirmation;
  f.app.modules.core.showConfirm = (msg, cb) => { confirmation = cb; };
  clickRow(f, { pauseTgt: "sample" });
  await confirmation();
  assert.ok(f.messages.some((m) => m.msg === "暂停失败: nope" && m.kind === "error"), JSON.stringify(f.messages));
});

// The harness resolves document.querySelectorAll against a flat element list,
// so the real DOM's "#custom-body-container > div" needs an explicit shim.
function useCustomBodyRows(f) {
  const document = f.harness.context.document;
  const original = document.querySelectorAll.bind(document);
  document.querySelectorAll = (selector) =>
    selector === "#custom-body-container > div"
      ? f.harness.ids.get("custom-body-container").children
      : original(selector);
}

// Issue #112: the custom body editor must not round-trip JSON values through
// JavaScript numbers. A value above Number.MAX_SAFE_INTEGER changes digits
// during JSON.parse/JSON.stringify even when the operator edits nothing.
test("unmodified custom body rows save their original JSON text byte for byte", async () => {
  const originalRaw = {
    seed: "12345678901234567890",
    temperature: "0.7",
    label: "\"ok\"",
    nested: "{\"id\":9007199254740993}",
    enabled: "true",
    empty: "null",
  };
  // custom_body is what JSON.parse (and therefore the old editor) sees: the
  // large integers are already rounded. custom_body_raw is the exact text.
  const rawTargets = JSON.stringify({
    targets: [target({
      custom_body: {
        seed: 12345678901234567000,
        temperature: 0.7,
        label: "ok",
        nested: { id: 9007199254740992 },
        enabled: true,
        empty: null,
      },
      custom_body_raw: originalRaw,
    })],
    count: 1,
  });
  const f = await setup(target(), (url, opts, fallback) =>
    url === "/admin/data/targets" && !opts?.method
      ? Promise.resolve({ ok: true, status: 200, text: async () => rawTargets })
      : fallback(url, opts));
  useCustomBodyRows(f);
  await f.edit();
  const rows = f.harness.ids.get("custom-body-container").children;
  const values = Object.fromEntries(rows.map((row) => [row.children[0].value, row.children[1].value]));
  assert.deepEqual(values, originalRaw, "opening the editor shows the exact stored JSON text");
  await f.save();
  const update = f.puts()[0];
  assert.ok(update, "the unmodified target was saved");
  const body = putBody(update);
  assert.deepEqual(body.custom_body_raw, originalRaw, "the save request carries every value verbatim");
  assert.equal("custom_body" in body, false, "the lossy parsed representation is not sent");
  assert.match(update.options.body, /12345678901234567890/);
  assert.match(update.options.body, /9007199254740993/);
  assert.doesNotMatch(update.options.body, /12345678901234567000|9007199254740992/,
    "no value was re-serialized from a JavaScript number");
});

test("edited custom body rows are syntax-checked without re-serializing valid JSON", async () => {
  const f = await setup(target());
  useCustomBodyRows(f);
  await f.edit();
  f.app.modules.targets.addCustomBodyRow("seed", "9007199254740993");
  f.app.modules.targets.addCustomBodyRow("enabled", "false");
  f.app.modules.targets.addCustomBodyRow("plain", "hello");
  await f.save();
  const body = putBody(f.puts()[0]);
  assert.deepEqual(body.custom_body_raw, {
    seed: "9007199254740993",
    enabled: "false",
    plain: "\"hello\"",
  });
  assert.equal("custom_body" in body, false);
});

// The server stores custom_body values as json.RawMessage, so encoding/json
// compacts whitespace and normalizes some string escapes (not number tokens)
// on write. An unconfirmed save compares the submitted text with that stored
// copy and must treat both JSON forms as the same content without parsing the
// integer into a JavaScript number.
test("unconfirmed: custom body whitespace and escapes still match the stored copy", async () => {
  const originalRaw = {
    seed: '{ "z": 1, "id": 9007199254740993, "list": [ 1, { "b": 2, "a": 3 } ], "text": "a b\\n\\"c\\"\\\\d" }',
    forms: '[ 1e+2, 2.0, -0 ]',
    tag: '{"label":"<tag>&"}',
  };
  const storedRaw = {
    seed: '{"id":9007199254740993,"list":[1,{"a":3,"b":2}],"text":"a b\\u000a\\u0022c\\u0022\\u005cd","z":1}',
    forms: '[1e+2,2.0,-0]',
    tag: '{"label":"\\u003ctag\\u003e\\u0026"}',
  };
  const f = await setup(target({
    custom_body: { seed: "", forms: "", tag: "" },
    custom_body_raw: originalRaw,
  }), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      f.harness.setRoute("/admin/data/targets", { targets: [target({
        etag: "t1-6",
        endpoint: "https://draft.invalid",
        custom_body: { seed: "", forms: "", tag: "" },
        custom_body_raw: storedRaw,
      })] });
      return Promise.resolve(htmlResponse(504));
    }
    return fallback(url, opts);
  });
  useCustomBodyRows(f);
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(f.messages.some((m) => m.msg === "已保存" && m.kind === "success"), messageTexts(f).join("|"));
  assert.ok(!messageTexts(f).some((m) => /已被修改|保存失败/.test(m)), messageTexts(f).join("|"));
  assert.equal(f.harness.ids.get("targetModal").classList.contains("open"), false);
});

test("unconfirmed: a different custom body number is not mistaken for the submitted one", async () => {
  const f = await setup(target({
    custom_body: { seed: { id: 9007199254740992 } },
    custom_body_raw: { seed: '{"id":9007199254740993}' },
  }), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      f.harness.setRoute("/admin/data/targets", { targets: [target({
        etag: "t1-6",
        endpoint: "https://draft.invalid",
        custom_body: { seed: { id: 9007199254740994 } },
        custom_body_raw: { seed: '{"id":9007199254740994}' },
      })] });
      return Promise.resolve(htmlResponse(504));
    }
    return fallback(url, opts);
  });
  useCustomBodyRows(f);
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(!f.messages.some((m) => m.msg === "已保存"), messageTexts(f).join("|"));
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /该目标已被修改，请重新加载后再保存/);
});

// JSON.parse creates a normal own data property for "__proto__"; constructing
// these fixtures with object literals would invoke the prototype setter and
// drop the member before the page ever sees it.
const protoMap = (value) => JSON.parse('{"__proto__":' + JSON.stringify(value) + '}');

test("custom body __proto__ field survives save and unconfirmed reconciliation", async () => {
  const submittedBody = protoMap({ nested: 1 });
  const submittedRaw = protoMap('{"nested":1}');
  const f = await setup(target({
    custom_body: submittedBody,
    custom_body_raw: submittedRaw,
  }), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      f.harness.setRoute("/admin/data/targets", { targets: [target({
        etag: "t1-6",
        endpoint: "https://draft.invalid",
        custom_body: submittedBody,
        custom_body_raw: submittedRaw,
      })] });
      return Promise.resolve(htmlResponse(504));
    }
    return fallback(url, opts);
  });
  useCustomBodyRows(f);
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  const update = f.puts()[0];
  assert.ok(update, "custom body __proto__ target was saved");
  const body = putBody(update);
  assert.equal(Object.prototype.hasOwnProperty.call(body.custom_body_raw, "__proto__"), true,
    "save request keeps the __proto__ field");
  assert.equal(body.custom_body_raw["__proto__"], '{"nested":1}');
  assert.ok(f.messages.some((m) => m.msg === "已保存" && m.kind === "success"), messageTexts(f).join("|"));
  assert.ok(!messageTexts(f).some((m) => /已被修改|保存失败/.test(m)), messageTexts(f).join("|"));
});

test("unconfirmed: a different custom body __proto__ value is not mistaken for the submitted one", async () => {
  const submittedBody = protoMap({ nested: 1 });
  const submittedRaw = protoMap('{"nested":1}');
  const storedBody = protoMap({ nested: 2 });
  const storedRaw = protoMap('{"nested":2}');
  const f = await setup(target({
    custom_body: submittedBody,
    custom_body_raw: submittedRaw,
  }), (url, opts, fallback) => {
    if (isTargetPut(url, opts)) {
      f.harness.setRoute("/admin/data/targets", { targets: [target({
        etag: "t1-6",
        endpoint: "https://draft.invalid",
        custom_body: storedBody,
        custom_body_raw: storedRaw,
      })] });
      return Promise.resolve(htmlResponse(504));
    }
    return fallback(url, opts);
  });
  useCustomBodyRows(f);
  await f.edit();
  f.harness.ids.get("tgtEndpoint").value = "https://draft.invalid";
  await f.save();
  assert.ok(!f.messages.some((m) => m.msg === "已保存"), messageTexts(f).join("|"));
  assert.match(f.harness.ids.get("keyCardList").innerHTML, /该目标已被修改，请重新加载后再保存/);
});
