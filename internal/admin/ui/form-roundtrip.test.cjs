"use strict";
const assert = require("node:assert/strict");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function main() {
  const harness = createHarness();
  const app = await harness.run();
  // Model-specific billing must remain model-specific after a mapping is removed.
  app.state.targets = [{ name: "current", model_mappings: [{ upstream: "AvailableModel" }] }];
  const options = app.modules.quota.quotaModelOptions("current", "RemovedModel");
  assert.match(options, /value='RemovedModel' selected/,
    "editing a historical binding must preserve its model instead of widening to the whole target");
  assert.match(app.modules.quota.quotaModelOptions("CURRENT", "AvailableModel"),
    /value='AvailableModel' selected/, "target lookup follows case-insensitive identity");
  assert.match(app.modules.quota.quotaTargetOptions("CURRENT"), /value='current' selected/,
    "binding editor keeps a target selected across identity case differences");
  for (const [containerId, field, render, collect, stateKey] of [
    ["clientTargetTags", "target", "renderClientTargetTags", "getSelectedClientTargets", "targets"],
    ["clientPoolTags", "pool", "renderClientPoolTags", "getSelectedClientPools", "pools"],
  ]) {
    const container = harness.ids.get(containerId);
    container.querySelectorAll = () => container.children.filter(child => child.dataset[field]);
    app.state[stateKey] = [{ name: "current" }, { name: "constructor" }];
    app.modules.clients[render](["Deleted<Resource>"]);
    assert.deepEqual(Array.from(app.modules.clients[collect]()), ["Deleted<Resource>"],
      "stale ACL grants must not disappear and become unrestricted on an unrelated save");
    const stale = container.children.find(child => child.dataset[field] === "Deleted<Resource>");
    assert.ok(stale.textContent.includes("Deleted<Resource>"), "stale name is DOM text");
    assert.ok(stale.textContent.includes("已删除"), "stale grant is visibly marked");
    app.modules.clients[render](["current"]);
    assert.deepEqual(Array.from(app.modules.clients[collect]()), ["current"],
      "Object prototype names must not add unauthorized grants");
  }
  app.state.editingClient = { name: "restricted" };
  app.modules.clients.renderClientTargetTags(["DeletedTarget"]);
  app.modules.clients.renderClientPoolTags(["DeletedPool"]);
  harness.ids.get("clientName").value = "restricted";
  harness.ids.get("clientAccessKey").value = "local-test-key";
  await harness.ids.get("saveClientBtn").listeners.click[0]();
  const write = harness.requests.find(request => request.options?.method === "PUT" &&
    request.url === "/admin/data/clients/restricted");
  assert.ok(write, "actual save handler posts the edited client");
  const payload = JSON.parse(write.options.body);
  assert.deepEqual(payload.allowed_targets, ["DeletedTarget"]);
  assert.deepEqual(payload.allowed_pools, ["DeletedPool"]);
  console.log("PASS model bindings and client ACL picker preserve existing restrictions");
}
main().catch(error => { console.error(error); process.exitCode = 1; });
