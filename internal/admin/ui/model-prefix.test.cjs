"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness, withETag, simulateTargetUpdate} = require("../../../scripts/testing/admin_ui_test.cjs");

test("target editor loads, previews and saves the exact outbound prefix", async () => {
  const saved = {name:"saved",endpoint_type:"provider-new",endpoint:"https://fixture.invalid",usage_adapter:"standard",model_prefix:"Vendor/",model_mappings:[{upstream:"fable5"}]};
  const h = createHarness({routes: {"/admin/data/targets": {targets: [withETag(saved, 1)]}}});
  const app = await h.run();
  const el = id => h.ids.get(id);
  await app.modules.targets.openTargetModal(withETag(saved, 1));
  assert.equal(el("tgtModelPrefix")?.value, "Vendor/");
  assert.match(el("tgtModelPrefixPreview").textContent, /Vendor\/fable5/);
  el("tgtModelPrefix").value = "NewVendor/";
  el("tgtModelPrefix").listeners.input[0]();
  assert.match(el("tgtModelPrefixPreview").textContent, /NewVendor\/fable5/);
  simulateTargetUpdate(h, withETag(saved, 1), withETag({...saved, model_prefix: "NewVendor/"}, 2));
  await el("saveTargetBtn").listeners.click[0]();
  const write = h.requests.find(r => r.url === "/admin/data/targets/saved" && r.options?.method === "PUT");
  assert.equal(write.options.headers["If-Match"], '"t1-1"');
  assert.equal(JSON.parse(write.options.body).api_key, "__existing_key_id__:kid-fixture-0");
  assert.equal(JSON.parse(write.options.body).model_prefix, "NewVendor/");
  assert.equal(app.state.targets[0].etag, "t1-2");
  await app.modules.targets.openTargetModal(null);
  assert.equal(el("tgtModelPrefix").value, "");
});
