"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {createHarness} = require("../../../scripts/testing/admin_ui_test.cjs");
const response = body => ({ok: true, status: 200, text: async () => JSON.stringify(body)});
const routes = () => ({"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []},
  "/admin/data/catalog": {models: []}, "/admin/data/clients": {clients: []}});

test("failed post-save quota refresh cannot reopen an old model-group coefficient", async () => {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  const group = {name: "A", cost_coefficient: 1, bindings: []};
  app.state.modelGroups = [group];
  h.setRouteFailure("/admin/data/quota/summary", true);
  await app.modules.quota.openModelGroupModal(group);
  h.ids.get("modelGroupCoefficient").value = "2";
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  assert.equal(h.requests.filter(r => r.url === "/admin/data/model-groups/A" && r.options?.method === "PUT").length, 1);
  assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), false);
  await app.modules.quota.openModelGroupModal(app.state.modelGroups[0]);
  if (h.ids.get("modelGroupModal").classList.contains("open")) {
    assert.equal(h.ids.get("modelGroupCoefficient").value, "2", "reopened editor cannot use the old coefficient");
    await h.ids.get("saveModelGroupBtn").listeners.click[0]();
    const puts = h.requests.filter(r => r.url === "/admin/data/model-groups/A" && r.options?.method === "PUT");
    assert.equal(JSON.parse(puts[1].options.body).cost_coefficient, 2);
  } else {
    await h.ids.get("saveModelGroupBtn").listeners.click[0]();
    assert.equal(h.requests.filter(r => r.url === "/admin/data/model-groups/A" && r.options?.method === "PUT").length, 1);
    assert.match(h.ids.get("modelGroupsBody").innerHTML, /过期.*刷新/);
    h.setRouteFailure("/admin/data/quota/summary", false);
    h.setRoute("/admin/data/quota/summary", {clients: []});
    h.setRoute("/admin/data/model-groups", {});
    await app.modules.quota.loadQuotaManagement();
    await app.modules.quota.openModelGroupModal(group);
    assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), false, "malformed refresh cannot authorize the old row");
    h.setRoute("/admin/data/model-groups", {model_groups: [{name: "A", cost_coefficient: "2", bindings: []}]});
    h.setRoute("/admin/data/job-levels", {job_levels: []});
    h.setRoute("/admin/data/project-groups", {project_groups: []});
    await app.modules.quota.loadQuotaManagement();
    await app.modules.quota.openModelGroupModal(app.state.modelGroups[0]);
    assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), true);
    assert.equal(h.ids.get("modelGroupCoefficient").value, "2");
    await h.ids.get("saveModelGroupBtn").listeners.click[0]();
    const puts = h.requests.filter(r => r.url === "/admin/data/model-groups/A" && r.options?.method === "PUT");
    assert.equal(puts.length, 2);
    assert.equal(JSON.parse(puts[1].options.body).cost_coefficient, 2);
  }
});

test("a changed model-group draft remains saveable after its earlier PUT and failed refresh", async () => {
  let release, writes = 0;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) => {
    if (url === "/admin/data/model-groups/A" && options?.method === "PUT" && ++writes === 1)
      return new Promise(resolve => {release = resolve;});
    return fallback(url, options);
  }});
  const app = await h.run();
  const group = {name: "A", cost_coefficient: 1, bindings: []};
  app.state.modelGroups = [group];
  h.setRouteFailure("/admin/data/quota/summary", true);
  await app.modules.quota.openModelGroupModal(group);
  h.ids.get("modelGroupCoefficient").value = "2";
  const save = h.ids.get("saveModelGroupBtn").listeners.click[0]();
  h.ids.get("modelGroupCoefficient").value = "3";
  h.ids.get("modelGroupModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), true);
  assert.equal(h.ids.get("modelGroupCoefficient").value, "3");
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  const puts = h.requests.filter(r => r.url === "/admin/data/model-groups/A" && r.options?.method === "PUT");
  assert.equal(puts.length, 2);
  assert.equal(JSON.parse(puts[1].options.body).cost_coefficient, 3);
});

test("a late open of the old group cannot survive a successful rename and refresh", async () => {
  let releaseOld, delay = false;
  const r = routes();
  r["/admin/data/quota/summary"] = {clients: []};
  r["/admin/data/model-groups"] = {model_groups: [{name: "B", cost_coefficient: 2}]};
  r["/admin/data/job-levels"] = {job_levels: []};
  r["/admin/data/project-groups"] = {project_groups: []};
  const h = createHarness({routes: r, fetch: (url, options, fallback) => {
    if (delay && url === "/admin/data/targets") {
      delay = false;
      return new Promise(resolve => {releaseOld = resolve;});
    }
    return fallback(url, options);
  }});
  const app = await h.run();
  const old = {name: "A", cost_coefficient: 1, bindings: []};
  app.state.modelGroups = [old];
  await app.modules.quota.openModelGroupModal(old);
  delay = true;
  const oldOpen = app.modules.quota.openModelGroupModal(old);
  assert.ok(releaseOld);
  h.ids.get("modelGroupName").value = "B";
  h.ids.get("modelGroupCoefficient").value = "2";
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  releaseOld(response({targets: []}));
  await oldOpen;
  assert.equal(h.ids.get("modelGroupModal").classList.contains("open"), false);
  assert.equal(app.state.modelGroups[0].name, "B");
});

test("a retained created-target draft cannot accept an unsaveable new key or post twice", async () => {
  let release;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/targets" && options?.method === "POST"
      ? new Promise(resolve => {release = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.targets.openTargetModal(null);
  h.ids.get("tgtName").value = "A";
  h.ids.get("tgtEndpoint").value = "https://before.invalid";
  h.ids.get("tgtUsageAdapter").value = "standard";
  h.ids.get("tgtNewKeyInput").value = "first-fixture-key";
  app.modules.targets.addKeyToList();
  const save = h.ids.get("saveTargetBtn").listeners.click[0]();
  h.ids.get("tgtEndpoint").value = "https://draft.invalid";
  h.ids.get("targetModal").listeners.input[0]();
  release(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("targetModal").classList.contains("open"), true);
  assert.equal(h.ids.get("tgtEndpoint").value, "https://draft.invalid");
  assert.equal(h.ids.get("tgtNewKeyInput").disabled, true);
  assert.equal(h.ids.get("addKeyBtn").disabled, true);
  h.ids.get("tgtNewKeyInput").value = "second-fixture-key";
  app.modules.targets.addKeyToList();
  assert.equal(h.ids.get("tgtNewKeyInput").value, "second-fixture-key");
  await h.ids.get("saveTargetBtn").listeners.click[0]();
  assert.equal(h.requests.filter(r => r.url === "/admin/data/targets" && r.options?.method === "POST").length, 1);
});

test("opening a new target replaces the retained created-target draft", async () => {
  let releaseSave;
  const h = createHarness({routes: routes(), fetch: (url, options, fallback) =>
    url === "/admin/data/targets" && options?.method === "POST"
      ? new Promise(resolve => {releaseSave = resolve;}) : fallback(url, options)});
  const app = await h.run();
  await app.modules.targets.openTargetModal(null);
  h.ids.get("tgtName").value = "A";
  h.ids.get("tgtEndpoint").value = "https://before.invalid";
  h.ids.get("tgtUsageAdapter").value = "standard";
  const save = h.ids.get("saveTargetBtn").listeners.click[0]();
  h.ids.get("tgtEndpoint").value = "https://draft.invalid";
  h.ids.get("targetModal").listeners.input[0]();
  releaseSave(response({status: "ok"}));
  await save;
  assert.equal(h.ids.get("tgtNewKeyInput").disabled, true, "retained draft stays locked until reopened");
  app.modules.targets.openTargetModal(null);
  assert.equal(h.ids.get("tgtNewKeyInput").disabled, false);
  assert.equal(h.ids.get("addKeyBtn").disabled, false);
  assert.equal(h.ids.get("tgtEndpoint").value, "", "the new editor starts from a clean form");
});
