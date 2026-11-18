"use strict";
// An in-plan model group with no bindings grants no route (every request on it
// is rejected as unconfigured). The people summary must flag that configuration
// state instead of rendering the aggregate as healthy / unlimited.
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../../scripts/testing/admin_ui_test.cjs");

async function fixture() {
  const harness = createHarness();
  const app = await harness.run();
  return { harness, app, quota: app.modules.quota };
}

function unboundStatus() {
  return {
    job_level: "L1",
    quotas: { daily: { limit: 10, used: 0 }, weekly: { limit: 0, used: 0 }, monthly: { limit: 0, used: 0 } },
    groups: {
      plan: {
        in_plan: true,
        bound: false,
        cost_coefficient: 1,
        plan_coefficient: 1,
        rank_coefficients: { daily: 1, weekly: 1, monthly: 1 },
        base_limits: { daily: 10, weekly: 0, monthly: 0 },
        quotas: { daily: { limit: 10, used: 0 }, weekly: { limit: 0, used: 0 }, monthly: { limit: 0, used: 0 } },
      },
    },
  };
}

test("an in-plan group without bindings is flagged instead of healthy", async () => {
  const { quota } = await fixture();
  const row = quota.quotaRowStatus({ client: { name: "alice" }, status: unboundStatus() });
  assert.equal(row.key, "unbound");
  assert.equal(row.label, "未绑定目标");
  assert.equal(row.badge, "over");
});

test("a mixed plan flags the row when any in-plan group has no bindings", async () => {
  const { quota } = await fixture();
  const status = {
    groups: {
      bound: { in_plan: true, bound: true, quotas: { daily: { limit: 100, used: 0 } } },
      unbound: { in_plan: true, bound: false, quotas: { daily: { limit: 0, used: 0 } } },
    },
  };
  assert.equal(quota.quotaRowStatus({ client: {}, status }).key, "unbound");
});

test("bound groups and legacy responses keep the previous status semantics", async () => {
  const { quota } = await fixture();
  const bound = { groups: { g: { in_plan: true, bound: true, quotas: { daily: { limit: 0 } } } } };
  assert.equal(quota.quotaRowStatus({ client: {}, status: bound }).key, "unlimited");
  // Responses from an older backend have no bound field: do not misreport them.
  const legacy = { groups: { g: { in_plan: true, quotas: { daily: { limit: 100, used: 0 } } } } };
  assert.equal(quota.quotaRowStatus({ client: {}, status: legacy }).key, "ok");
});

test("summary row and detail render 未绑定目标 instead of 不限", async () => {
  const { harness, app, quota } = await fixture();
  const status = unboundStatus();
  app.state.quotaSummary = [{ client: { name: "alice", job_level: "L1" }, status }];
  quota.renderQuotaSummary();
  const html = harness.ids.get("quotaSummaryBody").innerHTML;
  assert.match(html, /未绑定目标/);
  assert.doesNotMatch(html, />不限</, "an unbound group must not render as unlimited");

  const detail = quota.quotaConvertedDetail(status);
  assert.match(detail, /未绑定目标的模型组/);
  assert.match(detail, /不会匹配任何请求路由/);
  assert.doesNotMatch(detail, /未加入模型组/);
});