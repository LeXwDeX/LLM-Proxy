"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { createHarness } = require("../../../../scripts/testing/admin_ui_test.cjs");

test("model group sorting uses the visible label and keeps total-token tie breaking", async () => {
  const h = createHarness();
  const app = await h.run();
  const usage = app.modules.usage;
  assert.equal(usage.usageSortValue({ model_group: "company_general" }, "model_group"), "公司通用");
  const rows = [
    { model_group: "z-custom", model: "z-small", output_tokens: 1 },
    { model_group: "company_general", model: "company", output_tokens: 999 },
    { model_group: "z-custom", model: "z-large", output_tokens: 100 },
  ];
  usage.renderAggregateTable({ tbody: "usageByModelBody", rows, sort: {key: "model_group", dir: 1} });
  const html = h.document.getElementById("usageByModelBody").innerHTML;
  const names = [...html.matchAll(/<td>(z-small|company|z-large)<\/td>/g)].map(m => m[1]);
  const expected = rows.slice().sort((a,b) => usage.usageSortValue(a,"model_group").localeCompare(usage.usageSortValue(b,"model_group")) || b.output_tokens-a.output_tokens).map(r=>r.model);
  assert.deepEqual(names, expected);
  usage.renderAggregateTable({ tbody: "usageByModelBody", rows, sort: {key: "total", dir: -1} });
  assert.match(h.document.getElementById("usageByModelBody").innerHTML, /company.*z-large.*z-small/);
});

test("usage loading defaults to visible model group order, then total tokens descending", async () => {
  const h = createHarness();
  const app = await h.run();
  const rows = [
    { model_group: "订阅", model: "small", output_tokens: 10 },
    { model_group: "API", model: "middle", output_tokens: 20 },
    { model_group: "订阅", model: "large", output_tokens: 30 },
    { model_group: "company_general", model: "company", output_tokens: 40 },
    { model: "missing-group", output_tokens: 50 },
  ];
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => ({ok:true,status:200,text:async()=>JSON.stringify({by_model_group_model:rows})});
  await app.modules.usage.loadUsage();
  const html = h.document.getElementById("usageByModelBody").innerHTML;
  const names = [...html.matchAll(/<td>(small|middle|large|company|missing-group)<\/td>/g)].map(m=>m[1]);
  const label = r=>r.model_group === "company_general" ? "公司通用" : r.model_group || "-";
  assert.deepEqual(names, rows.slice().sort((a,b)=>label(a).localeCompare(label(b)) || b.output_tokens-a.output_tokens).map(r=>r.model));
  assert.ok(names.indexOf("large")+1 === names.indexOf("small"));
});
