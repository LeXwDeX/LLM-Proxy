"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

test("zero is displayed as a base value with a clear unlimited-period explanation", async () => {
  const harness = createHarness();
  const app = await harness.run();
  app.state.modelGroups = [{name: "shared", cost_coefficient: 1,
    daily_usd: 0, weekly_usd: 50, monthly_usd: 200, bindings: []}];
  app.modules.quota.renderModelGroups();
  const row = harness.ids.get("modelGroupsBody").innerHTML;
  assert.match(row, /Schema日 \$0\.00（已绑定职级时该周期上限不限）/);
  assert.match(row, /Schema周 \$50\.00/);
  assert.doesNotMatch(row, /Schema周 \$50\.00（已绑定职级时该周期上限不限）/);
  assert.match(harness.ids.get("modelGroupDimensions").innerHTML, /必填；0 = 已绑定职级时该周期不限/);
  const detail = app.modules.quota.quotaConvertedDetail({groups: {shared: {
    in_plan: true, base_limits: {daily: 0, weekly: 50, monthly: 200},
    quotas: {daily: {used: 2, limit: 0}, weekly: {used: 2, limit: 0}},
    rank_coefficients: {daily: 1, weekly: 0, monthly: 1},
    plan_coefficient: 1, cost_coefficient: 1,
  }}});
  assert.match(detail, /基数是配置值，已绑定职级时基数为 0 表示该周期上限不限/);
  assert.match(detail, /未绑定职级的请求被拒绝/);
  assert.match(detail, /2 \/ 不限/);
  assert.match(detail, /可用上限：基数 0 × 职级倍率 1/);
  assert.match(detail, /可用上限：基数 50 × 职级倍率 0/);
});
