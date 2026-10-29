"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function saveRate(h, app) {
  app.modules.quota.loadQuotaEditorData = async () => {};
  app.state.targets = [{ name: "images", model_mappings: [{ upstream: "image-model" }] }];
  await app.modules.pricing.openPricingModal(null, null, false);
  h.ids.get("ptModel").options.find(option => option.value === "image-model").selected = true;
  for (const [id, value] of [["ptModel", "image-model"], ["ptInput", "2"], ["ptOutput", "4"], ["ptCacheWrite", "0"], ["ptCacheRead", "1"]]) h.ids.get(id).value = value;
  await h.ids.get("saveModelCostBtn").listeners.click[0]();
  return h.document.getElementById("toastContainer").children.at(-1).textContent;
}

test("pricing save mentions the background quota refresh only when the server reports it", async () => {
  const withRefresh = createHarness({ routes: { "/admin/data/pricing-templates": { ok: true, quota_refresh: "pending", templates: [] } } });
  assert.match(await saveRate(withRefresh, await withRefresh.run()), /模型计费已创建，额度将在后台刷新/);
  const without = createHarness({ routes: { "/admin/data/pricing-templates": { ok: true, templates: [] } } });
  const text = await saveRate(without, await without.run());
  assert.match(text, /模型计费已创建/);
  assert.doesNotMatch(text, /额度将在后台刷新/);
});
