"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../scripts/testing/admin_ui_test.cjs");

test("target editor loads, previews and saves the exact outbound prefix", async () => {
  const h = createHarness();
  const app = await h.run();
  const el = id => h.ids.get(id);
  await app.modules.targets.openTargetModal({name:"saved",endpoint_type:"provider-new",endpoint:"https://fixture.invalid",usage_adapter:"standard",model_prefix:"Vendor/",model_mappings:[{upstream:"fable5"}]});
  assert.equal(el("tgtModelPrefix")?.value, "Vendor/");
  assert.match(el("tgtModelPrefixPreview").textContent, /Vendor\/fable5/);
  el("tgtModelPrefix").value = "NewVendor/";
  el("tgtModelPrefix").listeners.input[0]();
  assert.match(el("tgtModelPrefixPreview").textContent, /NewVendor\/fable5/);
  await el("saveTargetBtn").listeners.click[0]();
  const write = h.requests.find(r => r.url === "/admin/data/targets/saved" && r.options?.method === "PUT");
  assert.equal(JSON.parse(write.options.body).model_prefix, "NewVendor/");
  await app.modules.targets.openTargetModal(null);
  assert.equal(el("tgtModelPrefix").value, "");
});
