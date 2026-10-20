"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function fixture() {
  const harness = createHarness();
  const app = await harness.run();
  app.state.targets = [{name: "current", endpoint_type: "provider-new", model_mappings: [{upstream: "MixedCaseModel"}]}];
  app.modules.quota.loadQuotaEditorData = async () => {};
  return {harness, app, pricing: app.modules.pricing};
}

test("deleted target stays selected for editing and cannot be selected when copying", async () => {
  const {pricing} = await fixture();
  assert.equal(pricing.pricingTargetMissing("deleted"), true);
  assert.match(pricing.pricingTargetOptions("deleted", false), /value='deleted' selected/);
  assert.doesNotMatch(pricing.pricingTargetOptions("", true), /deleted/);
  assert.match(pricing.pricingTargetOptions("", true), /选择现存 Target/);
});

test("actual orphan copy workflow preserves prices and entered edits", async () => {
  const {harness, app, pricing} = await fixture();
  const source = {id: "old", target: "deleted", model: "MixedCaseModel", mode: "flat", base_rates: {
    input_per_1m_tokens: 0.06291, output_per_1m_tokens: 0.21047,
    cache_write_per_1m_tokens: 0.09786, cache_read_per_1m_tokens: 0.00854,
    explicit_cache_read_per_1m_tokens: 0.004, cache_write_5m_per_1m_tokens: 0,
    cache_write_1h_per_1m_tokens: 0.3,
  }};
  const field = id => harness.ids.get(id);
  await pricing.openPricingModal(source, null, false);
  assert.match(field("ptTarget").innerHTML, /value='deleted' selected/);
  assert.equal(field("ptTarget").disabled, true);
  assert.equal(field("ptInput").value, 0.06291);
  field("ptInput").value = "0.12345";
  field("ptTarget").focus = () => {};
  field("ptCopyFromEditBtn").listeners.click[0].call(field("ptCopyFromEditBtn"));
  assert.equal(field("ptInput").value, "0.12345");
  assert.equal(field("ptTarget").disabled, false);
  assert.equal(app.state.editingPricingTemplate, null);
  assert.equal(app.state.copyingPricingTemplate, source);
  await pricing.openPricingModal(source, null, true);
  assert.doesNotMatch(field("ptTarget").innerHTML, /deleted/);
  for (const [id, value] of [["ptInput", 0.06291], ["ptOutput", 0.21047],
    ["ptCacheWrite", 0.09786], ["ptCacheRead", 0.00854], ["ptExplicitRead", 0.004],
    ["ptWrite5m", 0], ["ptWrite1h", 0.3]]) assert.equal(field(id).value, value);
});
