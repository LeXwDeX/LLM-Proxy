"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

test("pool list shows the strategy label in the failover-logic column and no status badge", async () => {
  const harness = createHarness();
  const app = await harness.run();
  app.state.pools = [
    { name: "ds", display_name: "DeepSeek 部署池", description: "生产池", routing_strategy: "cost" },
    { name: "perf", routing_strategy: "load" },
    { name: "legacy" },
  ];
  app.state.targets = [];
  app.modules.pools.renderPools();
  const rows = harness.ids.get("poolsTableBody").innerHTML;
  const labels = rows.match(/<td class='pool-strategy'>([^<]*)<\/td>/g);
  // rows are sorted by pool name: ds, legacy, perf
  assert.deepEqual(labels, [
    "<td class='pool-strategy'>Schema成本优先</td>",
    "<td class='pool-strategy'>Schema成本优先</td>",
    "<td class='pool-strategy'>Schema性能优先</td>",
  ]);
  assert.doesNotMatch(rows, /quota-badge strategy/, "the status column no longer carries a strategy badge");
  assert.match(rows, /<td class='pool-name' title='生产池'>/, "description moves to the name tooltip");
  assert.doesNotMatch(rows, /pool-description/);
});

test("pool routing strategy round-trips through the modal and defaults to cost", async () => {
  const harness = createHarness();
  const app = await harness.run();
  const field = (id) => harness.ids.get(id);
  const pools = app.modules.pools;
  app.state.pools = [
    { name: "ds", display_name: "DeepSeek 部署池", routing_strategy: "cost" },
    { name: "legacy" },
  ];
  app.state.targets = [];
  pools.renderPools();
  const rows = field("poolsTableBody").innerHTML;

  const calls = [];
  app.modules.core.api = async (url, options) => {
    calls.push({ url, options });
    return { pools: app.state.pools };
  };

  pools.openPoolModal(app.state.pools[0]);
  assert.equal(field("poolRoutingStrategy").value, "cost");
  await field("savePoolBtn").listeners.click[0]();
  assert.equal(calls[0].options.body.routing_strategy, "cost");

  pools.openPoolModal(app.state.pools[1]);
  assert.equal(field("poolRoutingStrategy").value, "cost", "legacy pools default to cost");

  pools.openPoolModal(null);
  assert.equal(field("poolRoutingStrategy").value, "cost", "new pools default to cost");
  field("poolName").value = "new-pool";
  field("poolRoutingStrategy").value = "load";
  await field("savePoolBtn").listeners.click[0]();
  const create = calls.find((c) => c.options && c.options.method === "POST");
  assert.equal(create.url, "/admin/data/pools");
  assert.equal(create.options.body.routing_strategy, "load");
});
