"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../../scripts/testing/admin_ui_test.cjs");
const plain = value => JSON.parse(JSON.stringify(value));
async function fixture() {
  const harness = createHarness();
  const app = await harness.run();
  return {harness, app, quota: app.modules.quota};
}

test("actual quota editor preserves neutral/unlimited/positive and rejects invalid coefficients", async () => {
  const {harness, app, quota} = await fixture();
  const input = {value: "", validity: {badInput: false}};
  const card = {dataset: {editGroupIndex: "0"}, querySelector: () => input};
  harness.document.querySelectorAll = () => [card];
  for (const [raw, expected] of [["", {name: "shared"}], ["0", {name: "shared", coefficient: 0}],
    ["0.8", {name: "shared", coefficient: 0.8}]]) {
    app.state.quotaEditGroups = [{name: "shared", coefficient: 0.5}];
    input.value = raw;
    assert.deepEqual(plain(quota.syncQuotaEditGroups()), [expected]);
  }
  for (const [raw, badInput] of [["-1", false], ["1e999", false], ["", true]]) {
    app.state.quotaEditGroups = [{name: "shared", coefficient: 0.5}];
    input.value = raw; input.validity.badInput = badInput;
    assert.equal(quota.syncQuotaEditGroups(), null);
    assert.deepEqual(plain(app.state.quotaEditGroups), [{name: "shared", coefficient: 0.5}]);
  }
});

test("quota preview separates discount from capacity and actual input updates it", async () => {
  const {harness, app, quota} = await fixture();
  app.state.modelGroups = [{name: "subscription", cost_coefficient: 0.5,
    daily_usd: 10, weekly_usd: 50, monthly_usd: 200}];
  app.state.jobLevels = [{name: "supervisor", coefficient_daily: 5, coefficient_weekly: 5, coefficient_monthly: 5}];
  const expected = "Schema日 $50 / Schema周 $250 / Schema月 $1000";
  assert.equal(quota.quotaEffectivePreview({name: "subscription", coefficient: 1}, app.state.jobLevels[0]), expected);
  app.state.quotaEditGroups = [{name: "subscription", coefficient: 0.5}];
  const preview = {textContent: ""}, input = {value: "1", validity: {badInput: false}};
  const card = {dataset: {editGroupIndex: "0"}, querySelector: selector => selector === "[data-effective-preview]" ? preview : input};
  harness.document.querySelectorAll = () => [card];
  harness.ids.get("quotaEditJobLevel").value = "supervisor";
  harness.ids.get("quotaEditGroupEditor").listeners.input[0]();
  assert.equal(preview.textContent, "可用额度上限预览：" + expected);
});

test("actual model-group save preserves zero coefficient and rejects negative coefficient", async () => {
  const {harness, app} = await fixture();
  app.modules.quota.loadQuotaManagement = async () => {};
  const field = id => harness.ids.get(id);
  field("modelGroupName").value = "shared";
  field("modelGroup-daily").value = "10";
  field("modelGroup-weekly").value = "";
  field("modelGroup-monthly").value = "";
  field("modelGroupCoefficient").value = "0";
  await field("saveModelGroupBtn").listeners.click[0]();
  const writes = () => harness.requests.filter(r => r.url === "/admin/data/model-groups" && r.options?.method === "POST");
  assert.deepEqual(JSON.parse(writes()[0].options.body), {name: "shared", cost_coefficient: 0,
    bindings: [], daily_usd: 10, weekly_usd: 0, monthly_usd: 0});
  field("modelGroupCoefficient").value = "-1";
  await field("saveModelGroupBtn").listeners.click[0]();
  assert.equal(writes().length, 1);
});

test("quota summary separates unlimited, missing job and missing plan", async () => {
  const {quota} = await fixture();
  const groups = {shared: {in_plan: true, quotas: {daily: {limit: 0}}}};
  assert.equal(quota.quotaRowStatus({client: {}, status: {groups}}).key, "unlimited");
  assert.match(quota.quotaBarCell({limit: 0}), /不限/);
  assert.match(quota.quotaBarCell({limit: 0}, "职级未配置"), /职级未配置/);
  assert.equal(quota.quotaRowStatus({client: {}, status: {job_level_missing: true, groups}}).key, "joblevel");
  assert.equal(quota.quotaRowStatus({client: {}, status: {groups: {}}}).key, "none");
});

test("accounting failures display unavailable usage and escaped diagnostics", async () => {
  const {harness, app, quota} = await fixture();
  const status = {calibration_error: "<img src=x onerror=bad()>",
    exceeded: {scope: "accounting", dimension: "unavailable"}, groups: {}};
  const row = quota.quotaRowStatus({client: {name: "client"}, status});
  assert.equal(row.key, "accounting");
  assert.equal(row.label, "计费校准不可用");
  const details = quota.quotaConvertedDetail(status);
  assert.match(details, /&lt;img/);
  assert.doesNotMatch(details, /<img/);
  app.state.quotaSummary = [{client: {name: "client"}, status}];
  quota.renderQuotaSummary();
  assert.match(harness.ids.get("quotaSummaryBody").innerHTML, /校准不可用/);
  assert.doesNotMatch(harness.ids.get("quotaSummaryBody").innerHTML, /额度超限|>不限</);
});
