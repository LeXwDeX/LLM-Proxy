"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function fixture() {
  const h = createHarness();
  const app = await h.run();
  app.modules.quota.loadQuotaEditorData = async () => {};
  app.state.targets = [
    { name: "A", endpoint_type: "provider-new", model_mappings: [{ upstream: "old-model" }] },
    { name: "B", endpoint_type: "provider-new", model_mappings: [{ upstream: "new-model" }] },
    { name: "C", endpoint_type: "provider-new", model_mappings: [{ upstream: "old-model" }, { upstream: "other-model" }] },
  ];
  return { h, app, pricing: app.modules.pricing };
}
function toggleChoice(h, id, value, checked) {
  h.ids.get(id + "Choices").listeners.change[0]({ target: { type: "checkbox", value, checked } });
}
function fillRates(h) {
  for (const [id, value] of [["ptInput", "1"], ["ptOutput", "2"], ["ptCacheWrite", "0"], ["ptCacheRead", "0.1"]]) h.ids.get(id).value = value;
}
function writes(h) {
  return h.requests.filter(r => ["POST", "PUT"].includes(r.options?.method) && r.url.startsWith("/admin/data/pricing-templates"));
}
const source = { id: "normal", targets: ["A"], models: ["old-model"], mode: "flat", base_rates: {
  input_per_1m_tokens: 1, output_per_1m_tokens: 2, cache_write_per_1m_tokens: 0, cache_read_per_1m_tokens: 0.1,
}};

test("normal templates expose a copy action in the expanded detail", async () => {
  const { h, app, pricing } = await fixture();
  app.state.pricingTemplates = [source];
  const key = app.modules.pricingShared.pricingGroupKey(source.targets, source.models);
  app.state.pricingExpanded = { [key]: true };
  pricing.renderPricingTemplates();
  const html = h.ids.get("modelCostsTableBody").innerHTML;
  assert.match(html, /data-copy-pricing='normal'/);
  assert.match(html, /复制到其他目标/);
});

test("copy drops a source model the selected target cannot resolve", async () => {
  const { h, pricing } = await fixture();
  await pricing.openPricingModal(source, null, true);
  toggleChoice(h, "ptTarget", "B", true);
  assert.deepEqual(Array.from(pricing.pricingSelected("ptModel")), []);
  assert.match(h.ids.get("ptModelChoices").innerHTML, /new-model/);
  assert.doesNotMatch(h.ids.get("ptScopeNotice").textContent, /未配置此模型/);

  toggleChoice(h, "ptModel", "new-model", true);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 1);
  const body = JSON.parse(writes(h)[0].options.body);
  assert.deepEqual(body.targets, ["B"]);
  assert.deepEqual(body.models, ["new-model"]);
});

test("copy keeps a source model the selected target declares", async () => {
  const { h, pricing } = await fixture();
  await pricing.openPricingModal(source, null, true);
  toggleChoice(h, "ptTarget", "C", true);
  assert.deepEqual(Array.from(pricing.pricingSelected("ptModel")), ["old-model"]);
  assert.doesNotMatch(h.ids.get("ptScopeNotice").textContent, /未配置此模型/);

  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 1);
  const body = JSON.parse(writes(h)[0].options.body);
  assert.deepEqual(body.targets, ["C"]);
  assert.deepEqual(body.models, ["old-model"]);
});