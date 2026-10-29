"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {createHarness} = require("../../../scripts/testing/admin_ui_test.cjs");
const response = body => ({ok: true, status: 200, text: async () => JSON.stringify(body)});
const routes = () => ({"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []},
  "/admin/data/catalog": {models: []}, "/admin/data/clients": {clients: []}});
const settle = async () => { for (let i = 0; i < 80; i++) await Promise.resolve(); };

test("cannot reopen a client using its old ACL while its save is in flight", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/clients/A" && options?.method === "PUT"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  const old = {name: "A", access_key: "fixture", allowed_targets: ["old"]};
  app.state.clients = [old];
  await app.modules.clients.openClientModal(old);
  h.ids.get("clientTargetTags").querySelector("span[data-target]").listeners.click[0].call(h.ids.get("clientTargetTags").querySelector("span[data-target]"));
  const saving = h.ids.get("saveClientBtn").listeners.click[0]();
  app.modules.core.closeModal("clientModal");
  await app.modules.clients.openClientModal(old);
  assert.equal(h.ids.get("clientModal").classList.contains("open"), false, "old ACL must not be editable during its pending save");
  release(response({status: "ok"}));
  await saving;
  assert.equal(h.ids.get("clientModal").classList.contains("open"), false);
  assert.equal(h.requests.filter(req => req.url === "/admin/data/clients/A" && req.options?.method === "PUT").length, 1);
});

test("an open started before a client save cannot later install the old ACL", async () => {
  let releaseOpen, releaseSave, delayOpen = false, writes = 0;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) => {
    if (url === "/admin/data/clients/A" && options?.method === "PUT") {
      if (++writes === 1) return new Promise(resolve => {releaseSave = resolve;});
      return Promise.resolve(response({status: "ok"}));
    }
    if (url === "/admin/data/targets" && delayOpen) return new Promise(resolve => {releaseOpen = resolve;});
    return fallback(url, options);
  }});
  const app = await h.run();
  const client = {name: "A", access_key: "fixture", allowed_targets: ["old"]};
  app.state.clients = [client];
  await app.modules.clients.openClientModal(client);
  const tag = h.ids.get("clientTargetTags").querySelector("span[data-target]");
  tag.listeners.click[0].call(tag);
  delayOpen = true;
  const openOld = app.modules.clients.openClientModal(client);
  const save = h.ids.get("saveClientBtn").listeners.click[0]();
  delayOpen = false;
  releaseOpen(response({targets: []}));
  await openOld;
  releaseSave(response({status: "ok"}));
  await save;
  const put = h.requests.filter(req => req.url === "/admin/data/clients/A" && req.options?.method === "PUT");
  assert.equal(put.length, 1);
  assert.deepEqual(JSON.parse(put[0].options.body).allowed_targets, []);
  assert.equal(h.ids.get("clientModal").classList.contains("open"), false);
});

for (const mode of ["newer editor", "same editor draft"]) test(`model group save keeps ${mode}`, async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/model-groups/A" && options?.method === "PUT"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.quota.openModelGroupModal({name: "A", cost_coefficient: 1, daily_usd: 0, weekly_usd: 0, monthly_usd: 0});
  const save = h.ids.get("saveModelGroupBtn").listeners.click[0]();
  assert.ok(release);
  if (mode === "newer editor") await app.modules.quota.openModelGroupModal({name: "B", cost_coefficient: 2, daily_usd: 0, weekly_usd: 0, monthly_usd: 0});
  h.ids.get("modelGroupCoefficient").value = "3";
  if (mode === "same editor draft") h.ids.get("modelGroupModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), true);
  assert.equal(h.ids.get("modelGroupCoefficient").value, "3");
  if (mode === "newer editor") {
    assert.equal(h.ids.get("modelGroupName").value, "B");
    assert.equal(app.state.editingModelGroup.name, "B");
  }
});

test("rename binds retained draft to the saved model group", async () => {
  let release, first = true;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/model-groups/A" && options?.method === "PUT" && first
      ? (first = false, new Promise(resolve => {release = resolve;})) : fallback(url, options)});
  const app = await h.run();
  await app.modules.quota.openModelGroupModal({name: "A", cost_coefficient: 1, daily_usd: 0, weekly_usd: 0, monthly_usd: 0});
  h.ids.get("modelGroupName").value = "B";
  const save = h.ids.get("saveModelGroupBtn").listeners.click[0]();
  h.ids.get("modelGroupCoefficient").value = "3";
  h.ids.get("modelGroupModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("modelGroupCoefficient").value, "3");
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  const writes = h.requests.filter(req => req.options?.method === "PUT" && String(req.url).startsWith("/admin/data/model-groups/"));
  assert.deepEqual(writes.map(req => req.url), ["/admin/data/model-groups/A", "/admin/data/model-groups/B"]);
  assert.equal(JSON.parse(writes[1].options.body).cost_coefficient, 3);
});

test("a pending model group save cannot reopen the same group with its old values", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/model-groups/A" && options?.method === "PUT"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  const group = {name: "A", cost_coefficient: 1, daily_usd: 0, weekly_usd: 0, monthly_usd: 0};
  await app.modules.quota.openModelGroupModal(group);
  const save = h.ids.get("saveModelGroupBtn").listeners.click[0]();
  app.modules.core.closeModal("modelGroupModal");
  await app.modules.quota.openModelGroupModal(group);
  assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), false);
  release(response({status: "ok"}));
  await save;
});

test("model group creation preserves later edits and does not repeat POST", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/model-groups" && options?.method === "POST"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.quota.openModelGroupModal(null);
  for (const dim of ["daily", "weekly", "monthly"]) h.ids.get("modelGroup-" + dim).value = "0"; // bases must be explicit
  h.ids.get("modelGroupName").value = "A";
  h.ids.get("modelGroupCoefficient").value = "1";
  const save = h.ids.get("saveModelGroupBtn").listeners.click[0]();
  h.ids.get("modelGroupCoefficient").value = "2";
  h.ids.get("modelGroupModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), true);
  assert.equal(h.ids.get("modelGroupCoefficient").value, "2");
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  assert.equal(h.requests.filter(req => req.url === "/admin/data/model-groups" && req.options?.method === "POST").length, 1);
});

test("a key already entered before creation must be added before POST", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  await app.modules.targets.openTargetModal(null);
  h.ids.get("tgtName").value = "target";
  h.ids.get("tgtEndpoint").value = "https://fixture.invalid";
  h.ids.get("tgtUsageAdapter").value = "standard";
  h.ids.get("tgtNewKeyInput").value = "pending-secret-key";
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.equal(h.requests.filter(req => req.url === "/admin/data/targets" && req.options?.method === "POST").length, 0);
  assert.equal(h.ids.get("tgtNewKeyInput").value, "pending-secret-key");
});

test("open pool selector reflects failure and recovery without losing the user's selection", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  await app.modules.clients.openClientModal({name: "A", access_key: "fixture", allowed_pools: ["saved"]});
  await settle();
  const box = h.ids.get("clientPoolTags");
  let tag = box.querySelector("span[data-pool]");
  assert.match(tag.textContent, /已删除/);
  assert.equal(tag.dataset.selected, "1");
  h.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  tag = box.querySelector("span[data-pool]");
  assert.match(tag.textContent, /不可用/);
  assert.doesNotMatch(tag.textContent, /已删除/);
  assert.equal(tag.dataset.selected, "1");
  h.setRouteFailure("/admin/data/pools", false);
  h.setRoute("/admin/data/pools", {pools: [{name: "saved", display_name: "恢复池"}, {name: "other"}]});
  await app.modules.targets.loadTargets();
  tag = box.querySelectorAll("span[data-pool]").find(item => item.dataset.pool === "saved");
  assert.match(tag.textContent, /恢复池/);
  assert.doesNotMatch(tag.textContent, /不可用|已删除/);
  assert.equal(tag.dataset.selected, "1");
  assert.equal(box.querySelectorAll("span[data-pool]").length, 2);
});

test("an empty open pool selector changes between empty and unavailable without inventing grants", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  await app.modules.clients.openClientModal({name: "A", access_key: "fixture"});
  await settle();
  const box = h.ids.get("clientPoolTags");
  assert.match(box.innerHTML, /暂无部署池/);
  h.setRouteFailure("/admin/data/pools", true);
  await app.modules.targets.loadTargets();
  assert.match(box.innerHTML, /不可用/);
  assert.doesNotMatch(box.innerHTML, /暂无部署池/);
  h.setRouteFailure("/admin/data/pools", false);
  await app.modules.targets.loadTargets();
  assert.match(box.innerHTML, /暂无部署池/);
  assert.equal(box.querySelectorAll("span[data-pool]").length, 0);
});

test("pool refresh preserves a change made while its request is pending", async () => {
  let release, delay = false;
  const r = routes(); r["/admin/data/pools"] = {pools: [{name: "pool-a"}, {name: "pool-b"}]};
  const h = createHarness({routes: r, fetch: (url, options, fallback) =>
    delay && url === "/admin/data/pools" ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.clients.openClientModal({name: "A", access_key: "fixture", allowed_pools: ["pool-a"]});
  await settle();
  delay = true;
  const pending = app.modules.targets.loadTargets();
  const box = h.ids.get("clientPoolTags");
  const tag = box.querySelectorAll("span[data-pool]").find(item => item.dataset.pool === "pool-b");
  tag.listeners.click[0].call(tag);
  release(response({pools: [{name: "pool-a"}, {name: "pool-b"}]}));
  await pending;
  const selected = box.querySelectorAll("span[data-pool]").filter(item => item.dataset.selected === "1").map(item => item.dataset.pool);
  assert.deepEqual(selected, ["pool-a", "pool-b"]);
});

test("creation locks key entry during POST and unlocks it on failure", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/targets" && options?.method === "POST"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.targets.openTargetModal(null);
  h.ids.get("tgtName").value = "target";
  h.ids.get("tgtEndpoint").value = "https://fixture.invalid";
  h.ids.get("tgtUsageAdapter").value = "standard";
  h.ids.get("tgtNewKeyInput").value = "first-secret-key";
  app.modules.targets.addKeyToList();
  const save = h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.ok(release);
  assert.equal(h.ids.get("tgtNewKeyInput").disabled, true);
  assert.equal(h.ids.get("addKeyBtn").disabled, true);
  h.ids.get("tgtNewKeyInput").value = "second-secret-key";
  app.modules.targets.addKeyToList();
  assert.equal(h.ids.get("tgtNewKeyInput").value, "second-secret-key", "a blocked addition retains its input");
  release({ok: false, status: 503, text: async () => '{"error":"offline"}'});
  await save;
  assert.equal(h.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(h.ids.get("tgtNewKeyInput").disabled, false);
  assert.equal(h.ids.get("tgtNewKeyInput").value, "second-secret-key");
});

test("successful target creation keeps keys locked until its single POST completes", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/targets" && options?.method === "POST"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.targets.openTargetModal(null);
  h.ids.get("tgtName").value = "target";
  h.ids.get("tgtEndpoint").value = "https://fixture.invalid";
  h.ids.get("tgtUsageAdapter").value = "standard";
  h.ids.get("tgtNewKeyInput").value = "first-secret-key";
  app.modules.targets.addKeyToList();
  const saving = h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.equal(h.ids.get("tgtNewKeyInput").disabled, true);
  assert.match(h.ids.get("keyCardList").innerHTML, /创建中.*不能修改密钥/);
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  release(response({status: "ok"}));
  await saving;
  assert.equal(h.requests.filter(req => req.url === "/admin/data/targets" && req.options?.method === "POST").length, 1);
  assert.equal(h.ids.get("targetModal").classList.contains("open"), false);
});
