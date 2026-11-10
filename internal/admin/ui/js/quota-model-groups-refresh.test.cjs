"use strict";
// Issue #196: saving or deleting a model group must update the table at once, without a page
// reload, even when an unrelated request of the quota page reload fails.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const tick = async (n = 20) => { for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve)); };
const group = (name, daily) => ({name, cost_coefficient: 1, daily_usd: daily, weekly_usd: 0, monthly_usd: 0, bindings: []});
const routes = () => ({
  "/admin/data/targets": {targets: [{name: "t1", endpoint_type: "openai", model_mappings: [{upstream: "m1"}]}]},
  "/admin/data/catalog": {models: [{endpoint_type: "openai", model: "m1", display_name: "Model One"}]},
  "/admin/data/model-groups": {model_groups: [group("shared", 10)]},
  "/admin/data/job-levels": {job_levels: [{name: "Lead", coefficient_daily: 2}]},
  "/admin/data/project-groups": {project_groups: ["alpha"]},
  "/admin/data/quota/summary": {clients: []},
});

// setup boots the page and makes a write to /admin/data/model-groups[/name] replace the list
// that later GETs return with the given result.
async function setup(onWrite) {
  const h = createHarness({routes: routes()});
  const app = await h.run();
  await app.modules.quota.loadQuotaManagement();
  const base = h.context.fetch;
  h.writes = [];
  h.context.fetch = (url, options) => {
    const method = options?.method || "GET";
    if (String(url).startsWith("/admin/data/model-groups") && method !== "GET") {
      h.writes.push(method + " " + url);
      h.setRoute("/admin/data/model-groups", {model_groups: onWrite(method, url)});
      return Promise.resolve(json(method === "DELETE" ? {status: "ok", removed_from_clients: []} : {status: "ok"}));
    }
    return base(url, options);
  };
  return {h, app};
}

function fillGroup(h, name, daily) {
  h.ids.get("modelGroupName").value = name;
  h.ids.get("modelGroupCoefficient").value = "1";
  h.ids.get("modelGroup-daily").value = String(daily);
  h.ids.get("modelGroup-weekly").value = "0";
  h.ids.get("modelGroup-monthly").value = "0";
}

const failures = ["/admin/data/quota/summary", "/admin/data/targets"];

for (const failing of failures) {
  test(`creating a model group shows it at once while ${failing} fails`, async () => {
    const {h, app} = await setup(() => [group("shared", 10), group("grp-new", 5)]);
    await app.modules.quotaGroups.openModelGroupModal(null);
    fillGroup(h, "grp-new", 5);
    h.setRouteFailure(failing, true);
    await h.ids.get("saveModelGroupBtn").listeners.click[0]();
    await tick();
    assert.deepEqual(h.writes, ["POST /admin/data/model-groups"]);
    const body = h.ids.get("modelGroupsBody").innerHTML;
    assert.match(body, /grp-new/);
    assert.doesNotMatch(body, /已过期/);
    assert.deepEqual(app.state.modelGroups.map(g => g.name), ["shared", "grp-new"]);
  });

  test(`editing a model group shows the new values at once while ${failing} fails`, async () => {
    const {h, app} = await setup(() => [group("shared", 77)]);
    await app.modules.quotaGroups.openModelGroupModal(app.state.modelGroups[0]);
    fillGroup(h, "shared", 77);
    h.setRouteFailure(failing, true);
    await h.ids.get("saveModelGroupBtn").listeners.click[0]();
    await tick();
    assert.deepEqual(h.writes, ["PUT /admin/data/model-groups/shared"]);
    const body = h.ids.get("modelGroupsBody").innerHTML;
    assert.match(body, /\$77\.00/);
    assert.doesNotMatch(body, /已过期/);
    // The group can be edited again without a page reload.
    h.setRouteFailure(failing, false);
    await app.modules.quotaGroups.openModelGroupModal(app.state.modelGroups[0]);
    assert.equal(h.ids.get("modelGroup-daily").value, 77);
  });

  test(`deleting a model group removes its row at once while ${failing} fails`, async () => {
    const {h, app} = await setup(() => []);
    h.setRouteFailure(failing, true);
    const target = {dataset: {deleteModelGroup: "shared"}};
    h.ids.get("modelGroupsBody").listeners.click[0]({target});
    h.ids.get("confirmOkBtn").listeners.click[0]();
    await tick();
    assert.deepEqual(h.writes, ["DELETE /admin/data/model-groups/shared"]);
    assert.doesNotMatch(h.ids.get("modelGroupsBody").innerHTML, /shared/);
    assert.deepEqual(app.state.modelGroups, []);
  });
}

test("a new model group is offered in the client quota plan picker after saving", async () => {
  const {h, app} = await setup(() => [group("shared", 10), group("grp-new", 5)]);
  await app.modules.quotaGroups.openModelGroupModal(null);
  fillGroup(h, "grp-new", 5);
  h.setRouteFailure("/admin/data/quota/summary", true);
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  await tick();
  app.modules.quotaDrawer.openQuotaEditDrawer({client: {name: "c1", quota_groups: []}});
  assert.match(h.ids.get("quotaEditGroupPicker").innerHTML, /value='grp-new'/);
});

test("an invalid model group list after saving keeps the stale marker instead of erasing the table", async () => {
  const {h, app} = await setup(() => "broken");
  await app.modules.quotaGroups.openModelGroupModal(app.state.modelGroups[0]);
  fillGroup(h, "shared", 77);
  await h.ids.get("saveModelGroupBtn").listeners.click[0]();
  await tick();
  const body = h.ids.get("modelGroupsBody").innerHTML;
  assert.match(body, /shared/);
  assert.match(body, /已过期/);
});
