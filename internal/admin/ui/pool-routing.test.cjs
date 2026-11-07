"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

function costPoolRows() {
  const harness = createHarness();
  return harness.run().then((app) => {
    app.state.pools = [
      { name: "ds", display_name: "DeepSeek 部署池", routing_strategy: "cost" },
      { name: "legacy" },
    ];
    app.state.targets = [];
    app.modules.pools.renderPools();
    return harness.ids.get("poolsTableBody").innerHTML;
  });
}

test("cost pool strategy badge carries an explicit visible style class", async () => {
  const rows = await costPoolRows();
  assert.match(
    rows,
    /<span class='quota-badge strategy'>Schema成本优先<\/span>/,
    "the strategy badge must not use a bare quota-badge span",
  );
});

test("strategy badge style defines foreground and background for both themes", () => {
  const css = fs.readFileSync(path.join(__dirname, "css/admin.css"), "utf8");
  const rule = css.match(/\.quota-badge\.strategy\s*\{([^}]*)\}/);
  assert.ok(rule, ".quota-badge.strategy rule is missing");
  assert.match(rule[1], /color\s*:\s*var\(--accent\)/);
  assert.match(rule[1], /background\s*:\s*var\(--accent-soft\)/);
  const light = css.match(/\.app\s*\{([^}]*)\}/);
  const dark = css.match(/\.app\.dark\s*\{([^}]*)\}/);
  assert.ok(light && dark, "theme token blocks are missing");
  for (const block of [light[1], dark[1]]) {
    assert.match(block, /--accent\s*:/);
    assert.match(block, /--accent-soft\s*:/);
  }
});

test("pool routing strategy round-trips through the modal and shows on cost pools", async () => {
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
  assert.match(rows, /Schema成本优先/);
  assert.equal((rows.match(/Schema成本优先/g) || []).length, 1, "load pools carry no strategy badge");

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
  assert.equal(field("poolRoutingStrategy").value, "load", "legacy pools default to load");

  pools.openPoolModal(null);
  assert.equal(field("poolRoutingStrategy").value, "load");
  field("poolName").value = "new-pool";
  field("poolRoutingStrategy").value = "cost";
  await field("savePoolBtn").listeners.click[0]();
  const create = calls.find((c) => c.options && c.options.method === "POST");
  assert.equal(create.url, "/admin/data/pools");
  assert.equal(create.options.body.routing_strategy, "cost");
});
