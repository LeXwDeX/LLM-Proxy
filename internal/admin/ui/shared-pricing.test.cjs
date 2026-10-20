"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function fixture() {
  const h = createHarness();
  const app = await h.run();
  app.modules.quota.loadQuotaEditorData = async () => {};
  app.state.targets = ["A", "B"].map(name => ({name, endpoint_type: "provider-new", model_mappings: [
    {upstream: "fable5"}, {upstream: "fable5-1"},
  ]}));
  return {h, app, pricing: app.modules.pricing};
}
function select(h, id, values) {
  const field = h.ids.get(id);
  field.value = values[0] || "";
  Array.from(field.options).forEach(option => { option.selected = values.includes(option.value); });
}
function fillRates(h) {
  for (const [id, value] of [["ptInput", "2"], ["ptOutput", "4"], ["ptCacheWrite", "0"], ["ptCacheRead", "1"]]) h.ids.get(id).value = value;
}
function writes(h) { return h.requests.filter(r => ["POST", "PUT"].includes(r.options?.method) && r.url.startsWith("/admin/data/pricing-templates")); }

test("one save binds both targets and both same-price models to one rule", async () => {
  const {h, pricing} = await fixture();
  await pricing.openPricingModal(null, null, false);
  select(h, "ptTarget", ["A", "B"]);
  h.ids.get("ptTarget").listeners.change[0].call(h.ids.get("ptTarget"));
  select(h, "ptModel", ["fable5", "fable5-1"]);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 1);
  const body = JSON.parse(writes(h)[0].options.body);
  assert.deepEqual(body.targets, ["A", "B"]);
  assert.deepEqual(body.models, ["fable5", "fable5-1"]);
  assert.equal(body.base_rates.cache_write_per_1m_tokens, 0);
});

test("editing a shared rule retains its complete locked scope", async () => {
  const {h, pricing} = await fixture();
  const source = {id: "shared", target: "A", model: "fable5", targets: ["A", "B"], models: ["fable5", "fable5-1"], mode: "flat", base_rates: {}};
  await pricing.openPricingModal(source, null, false);
  assert.equal(h.ids.get("ptTarget").disabled, true);
  assert.equal(h.ids.get("ptModel").disabled, true);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 1);
  assert.deepEqual(JSON.parse(writes(h)[0].options.body).models, source.models);
  assert.deepEqual(JSON.parse(writes(h)[0].options.body).targets, source.targets);
});

test("global fallback and specific targets cannot be mixed", async () => {
  const {h, pricing} = await fixture();
  await pricing.openPricingModal(null, null, false);
  select(h, "ptTarget", ["", "A"]);
  select(h, "ptModel", ["fable5"]);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 0);
});

test("saving lists unsupported target/model pairs instead of partially binding", async () => {
  const {h, app, pricing} = await fixture();
  app.state.targets[1].model_mappings = [{upstream: "fable5"}];
  await pricing.openPricingModal(null, null, false);
  select(h, "ptTarget", ["A", "B"]);
  h.ids.get("ptTarget").listeners.change[0].call(h.ids.get("ptTarget"));
  select(h, "ptModel", ["fable5-1"]);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 0);
  assert.match(h.ids.get("ptScopeNotice").textContent, /B.*fable5-1/);
});

test("a shared rule renders once with all selected targets and models", async () => {
  const {h, app, pricing} = await fixture();
  app.state.pricingTemplates = [{id: "shared", target: "A", model: "fable5", targets: ["A", "B"], models: ["fable5", "fable5-1"], mode: "flat", base_rates: {}}];
  pricing.renderPricingTemplates();
  const html = h.ids.get("modelCostsTableBody").innerHTML;
  assert.equal((html.match(/data-pricing-summary/g) || []).length, 1);
  assert.match(html, /fable5-1/);
  assert.match(html, /B/);
});

test("submission rejects any overlapping shared binding and names the conflicting pair", async () => {
  const {h, app, pricing} = await fixture();
  app.state.pricingTemplates = [{id:"existing", targets:["B"], models:["fable5-1"], mode:"flat", start:"2026-11-01T00:00:00Z", end:"2026-12-01T00:00:00Z"}];
  await pricing.openPricingModal(null, null, false);
  select(h, "ptTarget", ["A", "B"]);
  h.ids.get("ptTarget").listeners.change[0].call(h.ids.get("ptTarget"));
  select(h, "ptModel", ["fable5", "fable5-1"]);
  fillRates(h);
  h.ids.get("ptStart").value = "2026-11-10T00:00";
  h.ids.get("ptEnd").value = "2026-11-20T00:00";
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 0);
  assert.match(h.ids.get("ptScopeNotice").textContent, /B.*fable5-1.*重叠/);
});

test("disjoint periods and fallback plus a dated rule remain valid", async () => {
  const {h, app, pricing} = await fixture();
  app.state.pricingTemplates = [
    {id:"base", targets:["A","B"], models:["fable5","fable5-1"], mode:"flat"},
    {id:"dated", targets:["B"], models:["fable5-1"], mode:"flat", start:"2026-10-01T00:00:00Z", end:"2026-10-10T00:00:00Z"},
  ];
  await pricing.openPricingModal(null, null, false);
  select(h, "ptTarget", ["A", "B"]);
  h.ids.get("ptTarget").listeners.change[0].call(h.ids.get("ptTarget"));
  select(h, "ptModel", ["fable5", "fable5-1"]);
  fillRates(h);
  h.ids.get("ptStart").value = "2026-11-10T00:00";
  h.ids.get("ptEnd").value = "2026-11-20T00:00";
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 1);
});

function toggleChoice(h, id, value, checked) {
  const list = h.ids.get(id + "Choices");
  assert.ok(list, "checkbox list exists");
  list.listeners.change[0]({target: {type: "checkbox", value, checked}});
}

test("checkbox selection keeps the full preview and makes global fallback exclusive", async () => {
  const {h, pricing} = await fixture();
  await pricing.openPricingModal(null, null, false);
  assert.match(h.ids.get("ptTargetChoices")?.innerHTML || "", /type='checkbox'/);
  toggleChoice(h, "ptTarget", "A", true);
  toggleChoice(h, "ptTarget", "B", true);
  toggleChoice(h, "ptModel", "fable5", true);
  toggleChoice(h, "ptModel", "fable5-1", true);
  assert.deepEqual(Array.from(pricing.pricingSelected("ptTarget")), ["A", "B"]);
  const preview = h.ids.get("ptSelectionPreview").innerHTML;
  for (const pair of ["A / fable5", "A / fable5-1", "B / fable5", "B / fable5-1"]) assert.ok(preview.includes(pair));
  assert.equal(h.ids.get("ptSelectionCount").textContent, "4 个组合");
  toggleChoice(h, "ptModel", "fable5-1", false);
  assert.doesNotMatch(h.ids.get("ptSelectionPreview").innerHTML, /fable5-1/);
  toggleChoice(h, "ptTarget", "", true);
  assert.deepEqual(Array.from(pricing.pricingSelected("ptTarget")), [""]);
  assert.match(h.ids.get("ptSelectionPreview").innerHTML, /全局兜底 \/ fable5/);
});

test("target changes retain selected models and expose unsupported combinations", async () => {
  const {h, app, pricing} = await fixture();
  app.state.targets[1].model_mappings = [{upstream: "fable5"}];
  await pricing.openPricingModal(null, null, false);
  toggleChoice(h, "ptTarget", "A", true);
  toggleChoice(h, "ptModel", "fable5-1", true);
  toggleChoice(h, "ptTarget", "B", true);
  toggleChoice(h, "ptTarget", "A", false);
  assert.deepEqual(Array.from(pricing.pricingSelected("ptModel")), ["fable5-1"]);
  assert.match(h.ids.get("ptSelectionPreview").innerHTML, /B \/ fable5-1/);
  assert.match(h.ids.get("ptScopeNotice").textContent, /B \/ fable5-1.*未配置/);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 0);
});

test("locked orphan scope stays readable and copying focuses the editable target search", async () => {
  const {h, pricing} = await fixture();
  const source = {id:"gone", target:"gone<&>", model:"Former/Model", mode:"flat", base_rates:{input_per_1m_tokens:2}};
  await pricing.openPricingModal(source, null, false);
  assert.equal(h.ids.get("ptTargetSearch").disabled, true);
  assert.equal(h.ids.get("ptModelSearch").disabled, true);
  assert.match(h.ids.get("ptTargetChoices").innerHTML, /checked disabled/);
  assert.match(h.ids.get("ptSelectionPreview").innerHTML, /gone&lt;&amp;&gt; \/ Former\/Model/);
  fillRates(h);
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(writes(h).length, 1, "orphan edit retains its existing save exception");
  await pricing.openPricingModal(source, null, false);
  h.ids.get("ptInput").value = "7.5";
  await h.ids.get("ptCopyFromEditBtn").listeners.click[0].call(h.ids.get("ptCopyFromEditBtn"));
  assert.equal(h.ids.get("ptTargetSearch").disabled, false);
  assert.equal(h.ids.get("ptTargetSearch").focused, true);
  assert.equal(h.ids.get("ptModelSearch").disabled, true);
  assert.equal(h.ids.get("ptSelectionCount").textContent, "0 个组合");
  assert.equal(h.ids.get("ptInput").value, "7.5");
  assert.match(h.ids.get("ptModelEmpty").textContent, /先勾选 Target/);
});
