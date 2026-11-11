"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function setup() {
  const harness = createHarness();
  const app = await harness.run();
  return { harness, app, el: (id) => harness.ids.get(id) };
}

const pools = () => [
  { name: "legacy", effective_routing_strategy: "cost", member_capacity_advised: true, members_without_capacity: ["alpha", "b<script>"] },
  { name: "healthy", routing_strategy: "cost", effective_routing_strategy: "cost", member_capacity_advised: true, members_without_capacity: [] },
  { name: "perf", routing_strategy: "load", effective_routing_strategy: "load", member_capacity_advised: false, members_without_capacity: [] },
];

test("pool rows warn about members without max_concurrency, escaped, and stay non-blocking", async () => {
  const { app, el } = await setup();
  app.state.pools = pools();
  app.state.targets = [];
  app.modules.pools.renderPools();
  const rows = el("poolsTableBody").innerHTML.split("</tr>");
  const byName = (name) => rows.find((r) => r.includes(`>${name}<`) || r.includes(`data-edit-pool='${name}'`));
  const legacy = byName("legacy");
  assert.match(legacy, /pool-capacity-warn/);
  assert.match(legacy, /2 个成员未设并发上限，成本优先下流量会集中到最便宜成员，建议配置/);
  assert.match(legacy, /alpha/);
  assert.match(legacy, /b&lt;script&gt;/, "member names are escaped");
  assert.doesNotMatch(legacy, /b<script>/);
  assert.match(legacy, /data-edit-pool='legacy'/, "row actions stay available");
  assert.doesNotMatch(byName("healthy"), /pool-capacity-warn/);
  assert.doesNotMatch(byName("perf"), /pool-capacity-warn/);
});

test("pool rows without the server field show no warning", async () => {
  const { app, el } = await setup();
  app.state.pools = [{ name: "old" }];
  app.state.targets = [];
  app.modules.pools.renderPools();
  assert.doesNotMatch(el("poolsTableBody").innerHTML, /pool-capacity-warn/);
});

test("target editor hints at max_concurrency only for unlimited members of advised pools", async () => {
  const { app, el } = await setup();
  app.state.pools = pools();
  const hint = el("tgtMaxConcurrencyHint");
  assert.ok(hint, "hint element exists");
  const target = { name: "alpha", endpoint_type: "openai", endpoint: "https://fixture.invalid", usage_adapter: "standard", pool: "legacy" };

  await app.modules.targets.openTargetModal(target);
  assert.equal(hint.hidden, false, "cost pool + empty limit shows the hint");
  assert.match(hint.textContent, /未设并发上限，成本优先下流量会集中到最便宜成员，建议配置/);

  el("tgtMaxConcurrency").value = "8";
  el("tgtMaxConcurrency").listeners.input[0]();
  assert.equal(hint.hidden, true, "a configured limit hides the hint");
  el("tgtMaxConcurrency").value = "0";
  el("tgtMaxConcurrency").listeners.input[0]();
  assert.equal(hint.hidden, false, "0 means unlimited");

  el("tgtPool").value = "perf";
  el("tgtPool").listeners.change[0]();
  assert.equal(hint.hidden, true, "load pool does not need a limit");
  el("tgtPool").value = "";
  el("tgtPool").listeners.change[0]();
  assert.equal(hint.hidden, true, "no pool, no hint");
  el("tgtPool").value = "healthy";
  el("tgtPool").listeners.change[0]();
  assert.equal(hint.hidden, false, "switching to a cost pool shows it");

  el("tgtPaused").checked = true;
  el("tgtPaused").listeners.change[0]();
  assert.equal(hint.hidden, true, "paused members are not counted");

  await app.modules.targets.openTargetModal({ ...target, max_concurrency: 3 });
  assert.equal(hint.hidden, true, "reopening resets from the saved target");
  await app.modules.targets.openTargetModal(null);
  assert.equal(hint.hidden, true, "new target without a pool has no hint");
});
