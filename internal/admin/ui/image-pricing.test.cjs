"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

test("image prices retain six decimals, zero and missing values in edit and copy", async () => {
  const h = createHarness();
  const app = await h.run();
  app.modules.quota.loadQuotaEditorData = async () => {};
  const source = {id:"image", model:"image-model", mode:"flat", base_rates:{
    image_input_per_1m_tokens: 1.234567, cached_image_input_per_1m_tokens: 0, image_output_per_1m_tokens: 9.876543,
  }, periods:[{weekdays:[1], start:"00:01", end:"00:02", rates:{image_input_per_1m_tokens: 0.123456, image_output_per_1m_tokens:0}}]};
  for (const copy of [false,true]) {
    await app.modules.pricing.openPricingModal(source,null,copy);
    for (const [id,value] of [["ptImageInput",1.234567],["ptCachedImageInput",0],["ptImageOutput",9.876543]])
      assert.equal(h.ids.get(id)?.value,value);
    assert.equal(app.state.ptPeriods[0].rates.image_input_per_1m_tokens,0.123456);
    assert.equal(app.state.ptPeriods[0].rates.cached_image_input_per_1m_tokens,null);
    assert.equal(app.state.ptPeriods[0].rates.image_output_per_1m_tokens,0);
  }
  await app.modules.pricing.openPricingModal(null,null,false);
  assert.equal(h.ids.get("ptImageInput").value,"");
  assert.equal(h.ids.get("ptCachedImageInput").value,"");
});

test("save writes image prices without converting missing values to zero", async () => {
  const h = createHarness();
  const app = await h.run();
  app.modules.quota.loadQuotaEditorData = async () => {};
  app.state.targets = [{name:"images", model_mappings:[{upstream:"image-model"}]}];
  await app.modules.pricing.openPricingModal(null,null,false);
  // Select an actual option, as the browser does for the multi-select.
  h.ids.get("ptModel").options.find(option => option.value === "image-model").selected = true;
  for (const [id,value] of [["ptModel","image-model"],["ptInput","2"],["ptOutput","4"],["ptCacheWrite","0"],["ptCacheRead","1"],
    ["ptImageInput","1.234567"],["ptCachedImageInput","0"],["ptImageOutput",""]]) h.ids.get(id).value=value;
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  const write=h.requests.find(r=>r.options?.method==="POST" && r.url==="/admin/data/pricing-templates");
  assert.ok(write,"pricing form was submitted");
  const rates=JSON.parse(write.options.body).base_rates;
  assert.equal(rates.image_input_per_1m_tokens,1.234567);
  assert.equal(rates.cached_image_input_per_1m_tokens,0);
  assert.equal(rates.image_output_per_1m_tokens,null);
  await app.modules.pricing.openPricingModal(null,null,false);
  h.ids.get("ptImageInput").value="-1";
  h.requests.length=0;
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  assert.equal(h.requests.some(r=>r.options?.method==="POST"),false);
});
