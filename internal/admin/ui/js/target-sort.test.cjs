"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness, schemaFixture } = require("../../../../scripts/testing/admin_ui_test.cjs");
const { assembleAdminHTML } = require("../../../../scripts/testing/assemble_admin_html.cjs");

async function setup(targets, runtime = []) {
  const h = createHarness({
    schema: schemaFixture(),
    routes: { "/admin/data/targets": { targets }, "/admin/data/pools": { pools: [] } },
    metrics: { targets_runtime: runtime },
  });
  const app = await h.run();
  await app.modules.targets.loadTargets();
  return app;
}
const tgt = (name, extra = {}) => ({ name, endpoint_type: "provider-new", endpoint: "https://x.invalid", ...extra });
const names = (app) => Array.from(app.modules.targets.filteredTargets().map((t) => t.name));

test("targets table headers except 操作 are sortable", () => {
  const html = assembleAdminHTML();
  const head = html.slice(html.indexOf('<table class="targets-table">'), html.indexOf('id="targetsTableBody"'));
  for (const key of ["status", "name", "keys", "type", "pool", "scheduling", "models"]) {
    assert.match(head, new RegExp('data-sort="' + key + '"'), key + " header is sortable");
  }
  assert.doesNotMatch(head, /data-sort="[^"]*"[^>]*>操作/);
});

test("clicking a header sorts asc, then desc, then back to the default order", async () => {
  const app = await setup([
    tgt("b", { model_mappings: [{ upstream: "m1" }, { upstream: "m2" }] }),
    tgt("c", { paused: true, model_mappings: [{ upstream: "m1" }] }),
    tgt("a", { model_mappings: [] }),
  ]);
  const list = app.modules.targetsList;
  assert.deepEqual(names(app), ["a", "b", "c"]);
  list.setTargetSort("models");
  assert.deepEqual(names(app), ["a", "c", "b"]);
  list.setTargetSort("models");
  assert.deepEqual(names(app), ["b", "c", "a"]);
  list.setTargetSort("models");
  assert.deepEqual(names(app), ["a", "b", "c"], "third click restores default order");
  list.setTargetSort("name");
  list.setTargetSort("name");
  assert.deepEqual(names(app), ["c", "b", "a"]);
});

test("each sortable column has a stable comparator", async () => {
  const app = await setup(
    [
      tgt("k3", { has_api_key: true, api_keys: ["x", "y"], endpoint_type: "openai", pool: "zeta" }),
      tgt("k1", { has_api_key: true, endpoint_type: "claude", paused: true }),
      tgt("k2", { has_api_key: true, api_keys: ["x"], endpoint_type: "gemini", pool: "alpha" }),
    ],
    [
      { name: "k3", phase: "ready", window_15m: { in_flight_peak: 1, affinity_hits: 0, breaker_trips: 0 } },
      { name: "k2", phase: "ready", window_15m: { in_flight_peak: 9, affinity_hits: 0, breaker_trips: 0 } },
    ],
  );
  const list = app.modules.targetsList;
  list.setTargetSort("keys");
  assert.deepEqual(names(app), ["k1", "k2", "k3"]);
  list.setTargetSort("pool"); // unpooled targets sort last in ascending order
  assert.deepEqual(names(app), ["k2", "k3", "k1"]);
  list.setTargetSort("scheduling"); // no runtime data sorts last
  assert.deepEqual(names(app), ["k3", "k2", "k1"]);
  list.setTargetSort("status");
  assert.equal(names(app)[2], "k1", "paused sorts after active targets");
  list.setTargetSort("type");
  assert.equal(names(app).length, 3);
});
