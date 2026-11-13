"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const fs = require("node:fs");
const path = require("node:path");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

// An orphan binding (its target was deleted) can be removed by hand.
test("model group editor offers 移除 on a binding whose target was deleted", async () => {
  const harness = createHarness({
    routes: { "/admin/data/catalog": { models: [] }, "/admin/data/pools": { pools: [] }, "/admin/data/targets": { targets: [{ name: "kept" }] } },
  });
  const app = await harness.run();
  await app.modules.quota.openModelGroupModal({ name: "g", cost_coefficient: 1, bindings: [{ target: "gone" }, { target: "kept" }] });
  const html = harness.ids.get("modelGroupBindingEditor").innerHTML;
  const rows = html.split("<div class='quota-binding-row'").slice(1);
  assert.equal(rows.length, 2);
  assert.match(rows[0], /目标已删除/);
  assert.match(rows[0], /data-remove-model-binding/, "orphan row must be removable");
  assert.match(rows[1], /data-remove-model-binding/);
});

// There is no fallback quota group any more: no 兜底 badge, and the usage
// statistics bucket for unattributed history reads 未归组.
test("quota UI no longer shows a 兜底 fallback group", () => {
  const dir = path.join(__dirname, "js/pages/quota");
  for (const file of fs.readdirSync(dir)) {
    assert.doesNotMatch(fs.readFileSync(path.join(dir, file), "utf8"), /兜底|公司通用/, file);
  }
});

test("unattributed usage bucket is labelled 未归组", async () => {
  const app = await createHarness({}).run();
  assert.equal(app.modules.quota.modelGroupLabel("company_general"), "未归组");
  assert.equal(app.modules.quota.modelGroupLabel("plan"), "plan");
});
