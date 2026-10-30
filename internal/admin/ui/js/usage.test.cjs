"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { assembleAdminHTML } = require("../../../../scripts/testing/assemble_admin_html.cjs");
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
  h.context.fetch = async () => response(aggregate({by_model_group_model:rows}));
  await app.modules.usage.loadUsage();
  const html = h.document.getElementById("usageByModelBody").innerHTML;
  const names = [...html.matchAll(/<td>(small|middle|large|company|missing-group)<\/td>/g)].map(m=>m[1]);
  const label = r=>r.model_group === "company_general" ? "公司通用" : r.model_group || "-";
  assert.deepEqual(names, rows.slice().sort((a,b)=>label(a).localeCompare(label(b)) || b.output_tokens-a.output_tokens).map(r=>r.model));
  assert.ok(names.indexOf("large")+1 === names.indexOf("small"));
});

function aggregate(overrides = {}) {
  return Object.assign({
    totals: {},
    buckets: [],
    by_client: [],
    by_model: [],
    by_model_group_model: [],
    by_target_model: [],
  }, overrides);
}
function response(data) {
  return {ok: true, status: 200, text: async () => JSON.stringify(data)};
}
async function settle() {
  for (let i = 0; i < 6; i++) await new Promise(resolve => setImmediate(resolve));
}
function click(element) {
  const listener = element.listeners.click && element.listeners.click[0];
  if (!listener) throw new Error("missing click listener");
  return listener({target: element, preventDefault() {}});
}
function change(element) {
  const listener = element.listeners.change && element.listeners.change[0];
  if (!listener) throw new Error("missing change listener");
  return listener({target: element});
}

test("summary strip shows seven values, total includes tool tokens and labels the discounted cost", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    totals: {
      requests: 3, input_tokens: 100, output_tokens: 20, cached_tokens: 30,
      cache_creation_tokens: 5, tool_use_prompt_tokens: 7,
      estimated_cost: 1.5, incomplete_usage_count: 0, incomplete_cost_count: 0,
    },
  }));
  await app.modules.usage.loadUsage();
  assert.equal(h.document.getElementById("usReqs").textContent, "3");
  assert.equal(h.document.getElementById("usInput").textContent, "100", "ordinary input stays distinct from cache");
  assert.equal(h.document.getElementById("usOutput").textContent, "20");
  assert.equal(h.document.getElementById("usCached").textContent, "30");
  assert.equal(h.document.getElementById("usCacheWrite").textContent, "5");
  assert.equal(h.document.getElementById("usTotal").textContent, "162", "total includes tool input");
  assert.match(h.document.getElementById("usCost").innerHTML, /\$1\.50/);
  assert.match(assembleAdminHTML(), /id="usCostLabel">估算费用<small>含模型组系数<\/small>/);
  const totals = app.modules.usage.usageTotals();
  app.modules.usage.addUsageTotals(totals, {input_tokens: 1, output_tokens: 2, cached_tokens: 3, cache_creation_tokens: 4, tool_use_prompt_tokens: 5});
  assert.equal(totals.tool_use_prompt_tokens, 5);
  assert.equal(app.modules.usage.usageTotalCells(totals).includes(">15<"), true);
});

test("summary distinguishes a real zero from missing aggregate totals", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, cache_creation_tokens: 0, tool_use_prompt_tokens: 0, estimated_cost: 0, incomplete_usage_count: 0, incomplete_cost_count: 0},
  }));
  await app.modules.usage.loadUsage();
  assert.equal(h.document.getElementById("usTotal").textContent, "0");
  assert.match(h.document.getElementById("usCost").innerHTML, /^\$0\.00$/);
  h.context.fetch = async () => response(aggregate({totals: {}}));
  await app.modules.usage.loadUsage();
  for (const id of ["usReqs", "usInput", "usOutput", "usCached", "usCacheWrite", "usTotal"]) {
    assert.equal(h.document.getElementById(id).textContent, "不可用", `${id} must not fabricate zero`);
  }
  assert.match(h.document.getElementById("usCost").innerHTML, /不可用/);
});

test("one aggregate request follows the selected range and filters for both tables and trend", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  const calls = [];
  h.context.fetch = async url => {
    calls.push(url);
    return response(aggregate({totals: {requests: 1, input_tokens: 1, output_tokens: 0, estimated_cost: 0}}));
  };
  await app.modules.usage.loadUsage();
  assert.equal(calls.length, 1, "trend and tables share one aggregate request");
  assert.match(calls[0], /group_by=hour/, "today uses the real hour bucket");
  const ranges = h.document.querySelectorAll(".time-range-btn");
  const sevenDays = ranges.find(button => button.dataset.range === "7d");
  click(sevenDays);
  await settle();
  assert.equal(calls.length, 2);
  assert.match(calls[1], /group_by=hour/, "7d fetches hourly buckets for local-day rollup");
  const oneHour = ranges.find(button => button.dataset.range === "1h");
  click(oneHour);
  await settle();
  assert.equal(calls.length, 3);
  assert.match(calls[2], /group_by=hour/);
  const clientFilter = h.document.getElementById("usageClientFilter");
  clientFilter.value = "tenant-a";
  change(clientFilter);
  await settle();
  assert.equal(calls.length, 4);
  assert.match(calls[3], /client_name=tenant-a/);
  assert.match(calls[3], /group_by=hour/);

  const custom = ranges.find(button => button.dataset.range === "custom");
  click(custom);
  const from = h.document.getElementById("usageFrom");
  const to = h.document.getElementById("usageTo");
  from.value = "2026-10-20T00:00";
  to.value = "2026-10-21T20:00";
  click(h.document.getElementById("applyCustomRange"));
  await settle();
  assert.equal(calls.length, 5);
  assert.match(calls[4], /group_by=hour/, "a custom range up to 48 hours keeps hour buckets");
  assert.match(calls[4], /client_name=tenant-a/, "selected filters stay applied");
  from.value = "2026-10-20T00:00";
  to.value = "2026-10-22T01:00";
  click(h.document.getElementById("applyCustomRange"));
  await settle();
  assert.equal(calls.length, 6);
  assert.match(calls[5], /group_by=hour/, "long custom ranges also fetch hourly buckets for local-day rollup");
});

test("custom range rejects an inverted boundary before any request and keeps filters", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  let calls = 0;
  h.context.fetch = async () => { calls++; return response(aggregate({})); };
  const ranges = h.document.querySelectorAll(".time-range-btn");
  click(ranges.find(button => button.dataset.range === "custom"));
  h.document.getElementById("usageClientFilter").value = "tenant-a";
  h.document.getElementById("usageFrom").value = "2026-10-22T00:00";
  h.document.getElementById("usageTo").value = "2026-10-20T00:00";
  click(h.document.getElementById("applyCustomRange"));
  await settle();
  assert.equal(calls, 0, "invalid custom range must not query the API");
  assert.equal(h.document.getElementById("usageClientFilter").value, "tenant-a");
  assert.match(h.document.getElementById("toastContainer").children.at(-1).textContent, /开始时间不能晚于结束时间/);
});

test("token and cost chart toggle redraws without a request and separates zero from missing", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  let calls = 0;
  h.context.fetch = async () => {
    calls++;
    return response(aggregate({
      totals: {input_tokens: 1, output_tokens: 1, estimated_cost: 0},
      buckets: [
        {bucket_start: "2026-10-25T01:00:00Z", input_tokens: 2, output_tokens: 3, estimated_cost: 0},
        {bucket_start: "2026-10-25T02:00:00Z", output_tokens: 5},
      ],
    }));
  };
  await app.modules.usage.loadUsage();
  const chart = h.document.getElementById("dailyChart");
  assert.match(chart.innerHTML, /01:00/);
  assert.match(chart.innerHTML, /UTC/);
  const before = calls;
  const costButton = h.document.getElementById("usageChartMetricCost");
  click(costButton);
  await settle();
  assert.equal(calls, before, "chart metric switch must not refetch");
  assert.equal(costButton.getAttribute("aria-pressed"), "true");
  assert.equal(h.document.getElementById("usageChartMetricTokens").getAttribute("aria-pressed"), "false");
  assert.match(chart.innerHTML, /\$0\.00/, "explicit zero cost stays visible");
  assert.match(chart.innerHTML, /费用: 不可用/, "missing cost is not rendered as zero");
  const tokenButton = h.document.getElementById("usageChartMetricTokens");
  click(tokenButton);
  await settle();
  assert.equal(calls, before);
  assert.match(chart.innerHTML, /5/);
  assert.equal(tokenButton.getAttribute("aria-pressed"), "true");
  assert.match(h.document.getElementById("usageChartNote").textContent, /UTC 小时/);
  assert.match(h.document.getElementById("usageChartNote").textContent, /边界小时/);
});

test("chart notes disclose hour bucket boundaries, local-day rollup and the 1h effective window", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({totals: {estimated_cost: 0}}));
  await app.modules.usage.loadUsage();
  const note = h.document.getElementById("usageChartNote");
  assert.match(note.textContent, /开始与结束时间所在整点小时/);
  assert.match(note.textContent, /整桶计入/);
  assert.match(note.textContent, /浏览器本地/);
  assert.match(note.textContent, /UTC/);
  const ranges = h.document.querySelectorAll(".time-range-btn");
  click(ranges.find(button => button.dataset.range === "1h"));
  await settle();
  assert.match(note.textContent, /近1小时/);
  assert.match(note.textContent, /实际覆盖/);
  assert.match(note.textContent, /个整点小时/);
  assert.match(note.textContent, /窗口外/);
  click(ranges.find(button => button.dataset.range === "7d"));
  await settle();
  assert.match(note.textContent, /浏览器本地日/);
  assert.match(note.textContent, /重新汇总/);
  assert.match(note.textContent, /整点小时桶/);
  assert.doesNotMatch(note.textContent, /UTC 自然日/);
  assert.match(note.textContent, /浏览器本地/);
  const html = assembleAdminHTML();
  const initial = /<p class="usage-chart-note" id="usageChartNote">([^<]*)<\/p>/.exec(html);
  assert.ok(initial, "initial chart note must exist");
  assert.match(initial[1], /开始与结束时间所在整点小时/);
  assert.match(initial[1], /浏览器本地日/);
});

test("three tabs are keyboard operable and the group tree uses by_target_model with tool tokens", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  const treeRow = {
    model_group: "g1", target: "target-1", model: "model-1",
    input_tokens: 10, output_tokens: 5, cached_tokens: 2, cache_creation_tokens: 1,
    tool_use_prompt_tokens: 3, estimated_cost: 1,
  };
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 1, estimated_cost: 1},
    by_target_model: [treeRow],
    by_model_group_model: [{model_group: "fallback-only", model: "fallback-model"}],
  }));
  await app.modules.usage.loadUsage();
  const groupTab = h.document.getElementById("usageTabGroup");
  const clientTab = h.document.getElementById("usageTabClient");
  const modelTab = h.document.getElementById("usageTabModel");
  assert.equal(groupTab.getAttribute("aria-selected"), "true");
  assert.equal(h.document.getElementById("usagePanelGroup").hidden, false);
  assert.equal(h.document.getElementById("usagePanelClient").hidden, true);
  const tabList = h.document.getElementById("usageTabList");
  tabList.listeners.keydown[0]({target: groupTab, key: "ArrowRight", preventDefault() {}});
  assert.equal(clientTab.getAttribute("aria-selected"), "true");
  assert.equal(h.document.getElementById("usagePanelClient").hidden, false);
  assert.equal(h.document.getElementById("usagePanelGroup").hidden, true);
  click(modelTab);
  assert.equal(modelTab.getAttribute("aria-selected"), "true");
  assert.equal(h.document.getElementById("usagePanelModel").hidden, false);
  const groupBody = h.document.getElementById("usageByModelGroupBody");
  assert.match(groupBody.innerHTML, /g1/);
  assert.doesNotMatch(groupBody.innerHTML, /fallback-only/, "group tree must not fall back to by_model_group_model");
  assert.doesNotMatch(groupBody.innerHTML, /↳ model-1/, "targets start collapsed");
  app.state.usageModelTargetExpanded["g1\u0000target-1"] = true;
  app.modules.usage.renderUsageModelGroups([treeRow]);
  assert.match(groupBody.innerHTML, /↳ model-1/);
  assert.match(groupBody.innerHTML, />21</, "group total includes tool input");

  const unsafe = Object.assign({}, treeRow, {model_group: "__proto__", target: "__proto__", model: "safe-model"});
  h.context.fetch = async () => response(aggregate({by_target_model: [unsafe], totals: {estimated_cost: 1}}));
  await app.modules.usage.loadUsage();
  assert.match(groupBody.innerHTML, /__proto__/, "group dictionaries must accept prototype names");
});

test("model request label follows the actual source and keeps the fallback explicit", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  const header = () => h.document.getElementById("usageModelRequestHeader");
  // 11 total requests = 7 success + 4 failed; only the model-group source is success-weighted.
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 11, estimated_cost: 10},
    by_model_group_model: [
      {model_group: "g", model: "a", requests: 5, estimated_cost: 5},
      {model_group: "g", model: "b", requests: 2, estimated_cost: 5},
    ],
    by_model: [{model: "a", requests: 6}, {model: "b", requests: 5}],
  }));
  await app.modules.usage.loadUsage();
  const modelBody = () => h.document.getElementById("usageByModelBody").innerHTML;
  assert.equal(header().textContent, "请求（成功）");
  assert.match(header().title, /成功请求权重（状态码 1–499）/);
  assert.equal(h.document.getElementById("usReqs").textContent, "11", "summary keeps all 11 requests while the model rows are success-weighted");
  assert.match(modelBody(), />5</);
  assert.match(modelBody(), />2</);
  assert.doesNotMatch(modelBody(), />11</, "primary model-group rows must not be replaced by all-request by_model rows");

  h.context.fetch = async () => response(aggregate({
    totals: {requests: 11, estimated_cost: 10},
    by_model_group_model: [],
    by_model: [{model: "a", requests: 6}, {model: "b", requests: 5}],
  }));
  await app.modules.usage.loadUsage();
  assert.equal(header().textContent, "请求权重");
  assert.match(header().title, /全部请求/);
  assert.match(modelBody(), />6</);
  assert.match(modelBody(), />5</);
  assert.doesNotMatch(header().title, /始终/);

  const labels = app.modules.usage.usageModelRequestHeader;
  const primaryHeader = labels("by_model_group_model", false);
  assert.equal(primaryHeader.label, "请求（成功）");
  assert.equal(primaryHeader.title, "成功请求权重（状态码 1–499），不是记录数");
  assert.equal(labels("by_model_group_model", true).label, "请求（成功）");
  assert.match(labels("by_model", true).title, /成功请求权重/);
  assert.match(labels("by_model", false).title, /全部请求/);
  assert.match(labels("none", false).title, /无模型明细数据/);
  app.modules.usage.setUsageModelRequestHeader("by_model", true);
  assert.equal(header().textContent, "请求（成功）");
  assert.match(header().title, /成功请求权重/);
  app.modules.usage.setUsageModelRequestHeader("by_model_group_model", false);
  const note = assembleAdminHTML();
  assert.match(note, /模型组明细为空时回退到按模型汇总/);
});

test("incomplete diagnostics distinguish usage from cost and include row-level counts", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  const usage = app.modules.usage;
  const usageOnly = usage.usageCostDenominator({estimated_cost: 10, incomplete_usage_count: 2});
  assert.equal(usageOnly.total, 10);
  assert.equal(usageOnly.usageIncomplete, true);
  assert.equal(usageOnly.costIncomplete, false);
  assert.equal(usageOnly.incomplete, true);
  assert.equal(usage.usageCostShare(1, usageOnly), "占比不可用（用量不完整）");
  assert.doesNotMatch(usage.usageCostShare(1, usageOnly), /费用不完整/);
  const costOnly = usage.usageCostDenominator({estimated_cost: 10, incomplete_cost_count: 1});
  assert.equal(usage.usageCostShare(1, costOnly), "占比不可用（费用不完整）");
  const both = usage.usageCostDenominator({estimated_cost: 10, incomplete_usage_count: 1, incomplete_cost_count: 1});
  assert.equal(usage.usageCostShare(1, both), "占比不可用（用量和费用不完整）");
  assert.match(usage.usageCostShare(1, {total: 0, usageIncomplete: false, costIncomplete: false}), /总费用为 0/);
  assert.match(usage.usageCostShare(null, costOnly), /^—$/);

  const complete = {total: 10, usageIncomplete: false, costIncomplete: false};
  const usageRow = usage.usageCostHtml({estimated_cost: 2, incomplete_usage_count: 1}, complete);
  assert.match(usageRow, /用量不完整 1 条记录/);
  assert.match(usageRow, /占比不可用（用量不完整）/);
  assert.doesNotMatch(usageRow, /费用不完整 0 条记录/);
  const costRow = usage.usageCostHtml({estimated_cost: 2, incomplete_cost_count: 3}, complete);
  assert.match(costRow, /费用不完整 3 条记录/);
  assert.match(costRow, /占比不可用（费用不完整）/);
  assert.doesNotMatch(costRow, /用量不完整 0 条记录/);
  const mixedRow = usage.usageCostHtml({estimated_cost: 2, incomplete_usage_count: 1}, {total: 10, usageIncomplete: false, costIncomplete: true});
  assert.match(mixedRow, /用量和费用不完整/);
  assert.match(usage.usageCostHtml({estimated_cost: 2, incomplete_cost_count: 1}, {total: 10, usageIncomplete: true, costIncomplete: false}), /用量和费用不完整/);
  assert.match(usage.usageCostHtml({estimated_cost: 2}, {total: 10, usageIncomplete: true, costIncomplete: true}), /用量和费用不完整/);
  assert.match(usage.usageCostHtml({estimated_cost: 0}, complete), /^\$0\.00/);
  assert.match(usage.usageCostHtml({estimated_cost: 0}, complete), /占比 0\.0%/);
  assert.match(usage.usageCostHtml({incomplete_usage_count: 1}, complete), /不可用/);
  assert.doesNotMatch(usage.usageCostHtml({incomplete_usage_count: 1}, complete), /\$0\.00/);

  h.context.fetch = async () => response(aggregate({
    totals: {estimated_cost: 10, incomplete_usage_count: 1},
    by_client: [
      {key: "usage-denominator", estimated_cost: 2, incomplete_usage_count: 1},
      {key: "complete-row", estimated_cost: 2.5},
    ],
  }));
  await app.modules.usage.loadUsage();
  const body = h.document.getElementById("usageByClientBody").innerHTML;
  assert.match(body, /占比不可用（用量不完整）/);
  assert.doesNotMatch(body, /占比不可用（费用不完整）/);

  h.context.fetch = async () => response(aggregate({
    totals: {estimated_cost: 10},
    by_client: [
      {key: "usage-row", estimated_cost: 2, incomplete_usage_count: 1},
      {key: "cost-row", estimated_cost: 2, incomplete_cost_count: 1},
      {key: "both-row", estimated_cost: 1, incomplete_usage_count: 1, incomplete_cost_count: 1},
      {key: "zero-row", estimated_cost: 0},
    ],
  }));
  await app.modules.usage.loadUsage();
  const render = id => {
    const html = h.document.getElementById("usageByClientBody").innerHTML;
    return html.split("</tr>").find(row => row.includes(id)) || "";
  };
  assert.match(render("usage-row"), /占比不可用（用量不完整）/);
  assert.match(render("cost-row"), /占比不可用（费用不完整）/);
  assert.match(render("both-row"), /占比不可用（用量和费用不完整）/);
  assert.match(render("zero-row"), /\$0\.00/);
});

test("cost shares use the current filtered aggregate total and degrade honestly", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 2, estimated_cost: 10, incomplete_cost_count: 0},
    by_client: [{key: "a", estimated_cost: 2.5}, {key: "b", estimated_cost: 0}, {key: "partial", estimated_cost: 1, incomplete_cost_count: 1}],
    by_model_group_model: [{model_group: "g", model: "m", estimated_cost: 2.5}],
    by_target_model: [{model_group: "g", target: "t", model: "m", estimated_cost: 2.5}],
  }));
  await app.modules.usage.loadUsage();
  const clientBody = h.document.getElementById("usageByClientBody");
  assert.match(clientBody.innerHTML, /占比 25\.0%/);
  assert.match(clientBody.innerHTML, /\$0\.00/);
  assert.match(clientBody.innerHTML, /占比不可用（费用不完整）/);
  h.context.fetch = async () => response(aggregate({
    totals: {estimated_cost: 0, incomplete_cost_count: 0},
    by_client: [{key: "a", estimated_cost: 0}],
  }));
  await app.modules.usage.loadUsage();
  assert.match(clientBody.innerHTML, /占比不可用/, "zero denominator cannot produce a share");
  h.context.fetch = async () => response(aggregate({
    totals: {estimated_cost: 10, incomplete_cost_count: 1},
    by_client: [{key: "a", estimated_cost: 2.5}],
  }));
  await app.modules.usage.loadUsage();
  assert.match(clientBody.innerHTML, /占比不可用.*费用不完整|费用不完整.*占比不可用/);
  h.context.fetch = async () => response(aggregate({
    totals: {},
    by_client: [{key: "a"}],
  }));
  await app.modules.usage.loadUsage();
  assert.match(clientBody.innerHTML, /不可用/);
  assert.doesNotMatch(clientBody.innerHTML, /占比 0\.0%/);
});

test("tree subtotals do not turn a missing model cost into zero or a complete share", async () => {
  const h = createHarness();
  const app = await h.run();
  const usage = app.modules.usage;
  const totals = usage.usageTotals();
  usage.addUsageTotals(totals, {estimated_cost: 2});
  usage.addUsageTotals(totals, {});
  usage.addUsageTotals(totals, {estimated_cost: 3});
  assert.equal(totals.estimated_cost, null);
  const explicitZero = usage.usageTotals();
  usage.addUsageTotals(explicitZero, {estimated_cost: 0});
  usage.addUsageTotals(explicitZero, {estimated_cost: 2});
  assert.equal(explicitZero.estimated_cost, 2, "an explicit zero stays numeric");
  usage.renderUsageModelGroups([{model_group: "missing", target: "t", model: "m", input_tokens: 1}], {total: 10});
  const body = h.document.getElementById("usageByModelGroupBody").innerHTML;
  assert.match(body, /不可用/);
  assert.doesNotMatch(body, /\$0\.00|占比 0\.0%/);
});

test("billing basis explains the model-group tree request weight and cost denominator honestly", () => {
  const html = assembleAdminHTML();
  const basis = /id="usageBillingBasis"[\s\S]*?<details class="usage-basis-details">([\s\S]*?)<\/details>/.exec(html);
  assert.ok(basis, "usage billing basis details must exist");
  const text = basis[1];
  assert.match(text, /费用占比的分母是当前筛选的估算总费用/);
  assert.match(text, /模型组树的请求数按成功请求加权，费用和 Token 统计全部已记录用量/);
  assert.match(text, /缺少可用价格时不补算费用/);
  assert.match(text, /费用占比在下列情况不可用：总费用为零或缺失，或用量、费用不完整/);
  assert.match(text, /总费用与 Token 总量仍按已记录值展示/);
  assert.doesNotMatch(text, /模型组树的请求和费用只统计成功请求/);
});

test("a stale aggregate response cannot overwrite a newer filter selection", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  const pending = [];
  h.context.fetch = url => new Promise(resolve => pending.push({url, resolve}));
  h.document.getElementById("usageClientFilter").value = "old-client";
  const first = app.modules.usage.loadUsage();
  for (let i = 0; i < 20 && pending.length < 1; i++) await settle();
  h.document.getElementById("usageClientFilter").value = "new-client";
  const second = app.modules.usage.loadUsage();
  for (let i = 0; i < 20 && !pending.some(item => item.url.includes("new-client")); i++) await settle();
  pending.filter(item => item.url.includes("new-client")).forEach(item => item.resolve(response(aggregate({
    totals: {estimated_cost: 1},
    by_client: [{key: "new-client", estimated_cost: 1}],
  }))));
  await second;
  pending.filter(item => item.url.includes("old-client")).forEach(item => item.resolve(response(aggregate({
    totals: {estimated_cost: 1},
    by_client: [{key: "old-client", estimated_cost: 1}],
  }))));
  await first;
  await settle();
  const body = h.document.getElementById("usageByClientBody").innerHTML;
  assert.match(body, /new-client/);
  assert.doesNotMatch(body, /old-client/);
});

function usageLoadStatus(h) {
  const container = h.document.querySelector(".time-range-bar").children
    .find(child => child.getAttribute("data-usage-status") === "true");
  assert.ok(container, "usage must have a persistent, visible load status");
  const message = container.children.find(child => child.getAttribute("role") === "status");
  const retry = container.children.find(child => child.tagName === "BUTTON");
  assert.ok(message, "load status must be announced");
  assert.ok(retry, "the failure state must offer a retry button");
  return {container, message, retry};
}

test("first usage failure replaces loading placeholders and retry recovers without fabricated values", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  for (const id of ["usageByModelGroupBody", "usageByClientBody", "usageByModelBody", "dailyChart"]) {
    h.document.getElementById(id).innerHTML = "加载中..."; // HTML's initial placeholders
  }
  h.context.fetch = async () => ({ok: false, status: 503, text: async () => '{"error":"离线"}'});
  await app.modules.usage.loadUsage();

  for (const id of ["usageByModelGroupBody", "usageByClientBody", "usageByModelBody", "dailyChart"]) {
    const html = h.document.getElementById(id).innerHTML;
    assert.match(html, /加载失败/, `${id} must not keep loading`);
    assert.doesNotMatch(html, /加载中|暂无统计数据|当前时间段无数据/);
  }
  const {container, message, retry} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /加载失败/);
  assert.match(message.textContent, /无.*数据/);
  assert.equal(retry.hidden, false);
  for (const id of ["usReqs", "usInput", "usOutput", "usCached", "usCacheWrite", "usTotal"]) {
    assert.equal(h.document.getElementById(id).textContent, "不可用");
  }
  assert.match(h.document.getElementById("usCost").innerHTML, /不可用/);
  assert.doesNotMatch(h.document.getElementById("usCost").innerHTML, /\$0\.00/);
  click(h.document.getElementById("usageChartMetricCost"));
  assert.match(h.document.getElementById("dailyChart").innerHTML, /加载失败/, "metric changes must not turn failure into an empty result");

  h.context.fetch = async () => response(aggregate({totals: {requests: 9, estimated_cost: 1.25}}));
  click(retry);
  await settle();
  assert.equal(container.hidden, true);
  assert.equal(h.document.getElementById("usReqs").textContent, "9");
  assert.match(h.document.getElementById("usCost").innerHTML, /\$1\.25/);
  assert.match(h.document.getElementById("usageByClientBody").innerHTML, /当前时间段无数据/);
});

test("failed filtered refresh marks only the real last snapshot stale and identifies its range and filters", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  app.state.modelGroups = [{name: "组-旧"}, {name: "组-新"}];
  const originalQuery = h.document.querySelectorAll;
  let selectedGroups = [{checked: true, value: "组-旧"}];
  h.document.querySelectorAll = selector => selector.startsWith("#usageModelGroupFilterMenu ")
    ? selectedGroups : originalQuery(selector);
  const urls = [];
  h.document.getElementById("usageClientFilter").value = "旧客户端";
  h.context.fetch = async url => {
    urls.push(url);
    return response(aggregate({totals: {requests: 3, estimated_cost: 2.75},
      by_client: [{key: "旧客户端", estimated_cost: 2.75}]}));
  };
  await app.modules.usage.loadUsage();
  const query = new URLSearchParams(urls[0].split("?")[1]);
  const oldBody = h.document.getElementById("usageByClientBody").innerHTML;
  const oldCost = h.document.getElementById("usCost").innerHTML;
  const {container, message, retry} = usageLoadStatus(h);
  assert.equal(container.hidden, true);

  let finish;
  h.document.getElementById("usageClientFilter").value = "新客户端";
  selectedGroups = [{checked: true, value: "组-新"}];
  h.context.fetch = () => new Promise(resolve => { finish = resolve; });
  const refreshing = app.modules.usage.loadUsage();
  await settle();
  assert.equal(container.hidden, false, "old data is marked while the new request is pending");
  assert.equal(retry.hidden, true);
  assert.equal(retry.style.display, "none", "button CSS must not override the hidden state");
  assert.match(h.document.getElementById("usageSummaryCards").getAttribute("aria-label"), /过期/);
  assert.match(message.textContent, /过期|上次/);
  for (const expected of [query.get("from"), query.get("to"), "旧客户端", "组-旧"]) {
    assert.ok(message.textContent.includes(expected), `last snapshot must identify ${expected}`);
  }
  finish({ok: false, status: 503, text: async () => '{"error":"离线"}'});
  await refreshing;
  assert.match(message.textContent, /加载失败/);
  assert.match(message.textContent, /过期/);
  assert.equal(retry.hidden, false);
  assert.equal(h.document.getElementById("usageByClientBody").innerHTML, oldBody);
  assert.equal(h.document.getElementById("usCost").innerHTML, oldCost);

  h.context.fetch = async url => {
    urls.push(url);
    return response(aggregate({totals: {requests: 5, estimated_cost: 4},
      by_client: [{key: "新客户端", estimated_cost: 4}]}));
  };
  click(retry);
  await settle();
  assert.equal(container.hidden, true, "only a successful current request clears stale status");
  assert.equal(h.document.getElementById("usageSummaryCards").getAttribute("aria-label"), "当前筛选汇总");
  assert.match(h.document.getElementById("usageByClientBody").innerHTML, /新客户端/);
  assert.doesNotMatch(h.document.getElementById("usageByClientBody").innerHTML, /旧客户端/);
  assert.match(h.document.getElementById("usCost").innerHTML, /\$4\.00/);
  const retried = new URLSearchParams(urls.at(-1).split("?")[1]);
  assert.equal(retried.get("client_name"), "新客户端");
  assert.equal(retried.get("model_group"), "组-新");
});

test("late success and late failure cannot replace the latest load status", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  const pending = [];
  h.context.fetch = url => new Promise(resolve => pending.push({url, resolve}));
  const client = h.document.getElementById("usageClientFilter");
  client.value = "old";
  const old = app.modules.usage.loadUsage();
  await settle();
  client.value = "new";
  const latest = app.modules.usage.loadUsage();
  await settle();
  pending[1].resolve({ok: false, status: 503, text: async () => '{"error":"new failed"}'});
  await latest;
  const {container, message} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /加载失败/);
  pending[0].resolve(response(aggregate({totals: {requests: 42, estimated_cost: 3}})));
  await old;
  assert.match(message.textContent, /加载失败/);
  assert.equal(h.document.getElementById("usReqs").textContent, "不可用");

  pending.length = 0;
  client.value = "old";
  const failingOld = app.modules.usage.loadUsage();
  await settle();
  client.value = "new";
  const successfulNew = app.modules.usage.loadUsage();
  await settle();
  pending[1].resolve(response(aggregate({totals: {requests: 7, estimated_cost: 0}})));
  await successfulNew;
  pending[0].resolve({ok: false, status: 503, text: async () => '{"error":"old failed"}'});
  await failingOld;
  assert.equal(container.hidden, true);
  assert.equal(h.document.getElementById("usReqs").textContent, "7");
  assert.match(h.document.getElementById("usCost").innerHTML, /\$0\.00/);
});

test("choosing a custom range invalidates the previous in-flight selection before apply", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  let finish;
  h.context.fetch = () => new Promise(resolve => { finish = resolve; });
  const old = app.modules.usage.loadUsage();
  await settle();
  click(h.document.querySelectorAll(".time-range-btn").find(button => button.dataset.range === "custom"));
  finish(response(aggregate({totals: {requests: 42, estimated_cost: 3}})));
  await old;
  assert.notEqual(h.document.getElementById("usReqs").textContent, "42");
  assert.doesNotMatch(h.document.getElementById("usCost").innerHTML, /\$3\.00/);
  const {container, message} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /自定义|未应用/);
});

test("invalid aggregate responses are failures, not empty or zero-cost snapshots", async () => {
  for (const invalid of [null, [], {raw: "not json"}, {}, aggregate({totals: null}), aggregate({by_client: {}}), aggregate({buckets: [null]})]) {
    const h = createHarness();
    const app = await h.run();
    app.state.modelGroupsLoaded = true;
    h.context.fetch = async () => response(invalid);
    await app.modules.usage.loadUsage();
    assert.match(h.document.getElementById("usageByClientBody").innerHTML, /加载失败/);
    const {container, message} = usageLoadStatus(h);
    assert.equal(container.hidden, false);
    assert.match(message.textContent, /加载失败/);
    assert.match(h.document.getElementById("usCost").innerHTML, /不可用/);
    assert.doesNotMatch(h.document.getElementById("usCost").innerHTML, /\$0\.00/);
  }
});

test("aggregate missing a required dimension fails on the first load", async () => {
  for (const key of ["totals", "buckets", "by_client", "by_model"]) {
    const h = createHarness();
    const app = await h.run();
    app.state.modelGroupsLoaded = true;
    const invalid = aggregate({totals: {requests: 7, estimated_cost: 2}});
    delete invalid[key];
    h.context.fetch = async () => response(invalid);
    await app.modules.usage.loadUsage();
    const {container, message, retry} = usageLoadStatus(h);
    assert.equal(container.hidden, false, `missing ${key}`);
    assert.match(message.textContent, /加载失败/);
    assert.equal(retry.hidden, false);
    assert.match(h.document.getElementById("usageByClientBody").innerHTML, /加载失败/);
    assert.equal(h.document.getElementById("usReqs").textContent, "不可用");
  }
});

test("incomplete aggregate refresh retains the previous snapshot as stale", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 3, estimated_cost: 5},
    by_client: [{key: "previous", estimated_cost: 5}],
  }));
  await app.modules.usage.loadUsage();
  const body = h.document.getElementById("usageByClientBody").innerHTML;
  h.context.fetch = async () => response({totals: {requests: 99, estimated_cost: 99}});
  await app.modules.usage.loadUsage();
  const {container, message, retry} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /加载失败.*过期/);
  assert.equal(retry.hidden, false);
  assert.equal(h.document.getElementById("usageByClientBody").innerHTML, body);
  assert.equal(h.document.getElementById("usReqs").textContent, "3");
});

test("aggregate accepts null required slices and omitted optional dimensions", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response({
    totals: {requests: 0, estimated_cost: 0}, buckets: null, by_client: null, by_model: null,
  });
  await app.modules.usage.loadUsage();
  assert.equal(usageLoadStatus(h).container.hidden, true);
  assert.equal(h.document.getElementById("usReqs").textContent, "0");
  assert.match(h.document.getElementById("usageByClientBody").innerHTML, /当前时间段无数据/);
});

test("model group load failure is visible and retry fetches groups before the aggregate", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = false;
  app.state.modelGroupsPromise = null;
  const calls = [];
  let failGroups = true;
  h.context.fetch = async url => {
    calls.push(url);
    if (url === "/admin/data/model-groups") {
      if (failGroups) return {ok: false, status: 503, text: async () => '{"error":"groups offline"}'};
      return response({model_groups: []});
    }
    return response(aggregate({totals: {requests: 2, estimated_cost: 3}}));
  };
  await app.modules.usage.loadUsage();
  const {container, message, retry} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /加载失败/);
  assert.deepEqual(calls, ["/admin/data/model-groups"]);
  assert.match(h.document.getElementById("usageByClientBody").innerHTML, /加载失败/);
  failGroups = false;
  click(retry);
  await settle();
  assert.equal(container.hidden, true);
  assert.equal(calls[1], "/admin/data/model-groups");
  assert.match(calls[2], /^\/admin\/data\/usage\/aggregate/);
  assert.equal(h.document.getElementById("usReqs").textContent, "2");
});

test("a late usage model-group response cannot overwrite a completed quota refresh", async t => {
  for (const oldFails of [false, true]) {
    await t.test(oldFails ? "old failure" : "old success", async () => {
      const h = createHarness({routes: {"/admin/data/targets": {targets: []}}});
      const app = await h.run();
      app.state.modelGroupsLoaded = false;
      const base = h.context.fetch;
      let finishOld;
      let first = true;
      h.context.fetch = (url, options) => {
        if (url === "/admin/data/model-groups" && first) {
          first = false;
          return new Promise(resolve => { finishOld = resolve; });
        }
        if (String(url).includes("/admin/data/usage/aggregate")) return Promise.resolve(response(aggregate()));
        return base(url, options);
      };
      const old = app.modules.usage.loadUsage();
      await settle();
      h.setRoute("/admin/data/model-groups", {model_groups: [{name: "new-group"}]});
      await app.modules.quota.loadQuotaManagement();
      assert.equal(app.state.modelGroups[0].name, "new-group");
      finishOld(oldFails ? {ok: false, status: 503, text: async () => '{"error":"offline"}'}
        : response({model_groups: [{name: "old-group"}]}));
      await old;
      assert.equal(app.state.modelGroups[0].name, "new-group");
      const labels = h.document.getElementById("usageModelGroupFilterMenu").children
        .map(child => child.children[1].textContent);
      assert.ok(labels.includes("new-group"));
      assert.ok(!labels.includes("old-group"));
      assert.equal(usageLoadStatus(h).container.hidden, true);
    });
  }
});

test("a failed quota refresh does not discard a pending usage model-group load", async () => {
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}}});
  const app = await h.run();
  app.state.modelGroupsLoaded = false;
  const base = h.context.fetch;
  let finishOld;
  let first = true;
  h.context.fetch = (url, options) => {
    if (url === "/admin/data/model-groups" && first) {
      first = false;
      return new Promise(resolve => { finishOld = resolve; });
    }
    if (String(url).includes("/admin/data/usage/aggregate")) return Promise.resolve(response(aggregate()));
    return base(url, options);
  };
  const old = app.modules.usage.loadUsage();
  await settle();
  h.setRouteFailure("/admin/data/quota/summary", true);
  await app.modules.quota.loadQuotaManagement();
  finishOld(response({model_groups: [{name: "usage-group"}]}));
  await old;
  assert.equal(app.state.modelGroups[0].name, "usage-group");
  assert.equal(usageLoadStatus(h).container.hidden, true);
});

test("render failure after a successful load restores all of the earlier snapshot", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 3, estimated_cost: 5},
    by_client: [{key: "previous", estimated_cost: 5}],
    by_target_model: [{model_group: "previous-group", target: "t", model: "m", estimated_cost: 5}],
    buckets: [{bucket_start: "2026-10-25T01:00:00Z", input_tokens: 2}],
  }));
  await app.modules.usage.loadUsage();
  const before = Object.fromEntries(["usCost", "usageByClientBody", "usageByModelGroupBody", "dailyChart"]
    .map(id => [id, h.document.getElementById(id).innerHTML]));
  h.context.fetch = async () => response(aggregate({
    totals: {requests: 99, estimated_cost: 99},
    by_client: [{key: "new", estimated_cost: 99}],
    by_target_model: [{model_group: {toString: null, valueOf: null}, target: "t", model: "m"}],
  }));
  await app.modules.usage.loadUsage();
  const {container, message} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /加载失败.*过期/);
  assert.equal(h.document.getElementById("usReqs").textContent, "3");
  for (const [id, html] of Object.entries(before)) {
    assert.equal(h.document.getElementById(id).innerHTML, html, `${id} must restore the last real data`);
  }
});

test("invalid custom apply invalidates an earlier pending custom request", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({totals: {requests: 4, estimated_cost: 1}}));
  await app.modules.usage.loadUsage();
  click(h.document.querySelectorAll(".time-range-btn").find(button => button.dataset.range === "custom"));
  const from = h.document.getElementById("usageFrom");
  const to = h.document.getElementById("usageTo");
  from.value = "2026-10-20T00:00";
  to.value = "2026-10-21T00:00";
  let finish;
  h.context.fetch = () => new Promise(resolve => { finish = resolve; });
  const pending = app.modules.usage.loadUsage();
  await settle();
  from.value = "2026-10-22T00:00";
  await app.modules.usage.loadUsage();
  finish(response(aggregate({totals: {requests: 99, estimated_cost: 99}})));
  await pending;
  const {container, message, retry} = usageLoadStatus(h);
  assert.equal(container.hidden, false);
  assert.match(message.textContent, /自定义时间无效.*过期/);
  assert.equal(retry.hidden, true);
  assert.equal(h.document.getElementById("usReqs").textContent, "4");
  assert.match(h.document.getElementById("usCost").innerHTML, /\$1\.00/);
});

// ===== Issue #97 =====
function withTZ(zone, fn) {
  const previous = process.env.TZ;
  process.env.TZ = zone;
  const restore = () => { if (previous === undefined) delete process.env.TZ; else process.env.TZ = previous; };
  let result;
  try { result = fn(); } catch (error) { restore(); throw error; }
  if (result && typeof result.then === "function") return result.finally(restore);
  restore();
  return result;
}

test("usage presets use the same local-day windows as the overview range helper", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  const usage = app.modules.usage;
  const overview = app.modules.overviewRange;
  const now = new Date();
  for (const key of ["today", "yesterday", "7d", "30d"]) {
    const window = overview.rangeWindow(key, now);
    const range = usage.getTimeRange(key);
    assert.equal(range.from, window.from.toISOString(), key + " starts at the same local midnight");
    if (window.closed) assert.equal(range.to, window.to.toISOString(), key + " ends 1ms before the next boundary");
  }
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  assert.equal(usage.getTimeRange("7d").from, new Date(today.getFullYear(), today.getMonth(), today.getDate() - 6).toISOString(), "7d is today plus six previous local days");
  assert.equal(usage.getTimeRange("30d").from, new Date(today.getFullYear(), today.getMonth(), today.getDate() - 29).toISOString(), "30d is today plus 29 previous local days");
  const hour = usage.getTimeRange("1h");
  assert.ok(Math.abs((Date.parse(hour.to) - Date.parse(hour.from)) - 3600000) < 1000, "1h stays a rolling hour");
}));

test("daily chart zero-fills missing hour buckets inside the queried range and labels hours in local time", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  app.modules.usage.renderDailyChart([
    {bucket_start: "2026-10-26T16:00:00Z", input_tokens: 10, output_tokens: 5},
    {bucket_start: "2026-10-26T19:00:00Z", input_tokens: 1, output_tokens: 1},
  ], "hour", {from: "2026-10-26T16:30:00Z", to: "2026-10-26T19:10:00Z"});
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-27 00:00", "10-27 01:00", "10-27 02:00", "10-27 03:00"], "every hour of the range is drawn, in local time");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["15", "0", "0", "2"], "gaps are real zeros, not missing");
  assert.match(chart.innerHTML, /本地 10-27 00:00/);
  assert.match(chart.innerHTML, /UTC 10-26 16:00/);
}));

test("daily chart rolls hourly buckets into exactly seven local days in UTC+8", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  // Range: 2026-10-24 00:00 through 2026-10-30 13:00 local (UTC+8).
  app.modules.usage.renderDailyChart([
    {bucket_start: "2026-10-24T15:00:00Z", input_tokens: 1, estimated_cost: 0},
    {bucket_start: "2026-10-24T16:00:00Z", input_tokens: 2, estimated_cost: 0},
    {bucket_start: "2026-10-29T16:00:00Z", input_tokens: 4, estimated_cost: 0},
  ], "day", {from: "2026-10-23T16:00:00Z", to: "2026-10-30T05:00:00Z"}, "hour");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-24", "10-25", "10-26", "10-27", "10-28", "10-29", "10-30"], "exactly seven local-day bars, no extra UTC day");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["1", "2", "0", "0", "0", "0", "4"], "hours are summed into the local day containing their start");
  assert.match(chart.innerHTML, /本地 2026-10-25 日/);
  assert.doesNotMatch(chart.innerHTML, /\(UTC\)/);
}));

test("daily chart rolls hourly buckets into exactly seven local days in a western timezone", () => withTZ("America/Los_Angeles", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  // Range: 2026-10-24 00:00 through 2026-10-30 13:00 local (UTC-7, before DST ends).
  app.modules.usage.renderDailyChart([
    {bucket_start: "2026-10-24T07:00:00Z", input_tokens: 1, estimated_cost: 0},
    {bucket_start: "2026-10-25T06:00:00Z", input_tokens: 2, estimated_cost: 0},
    {bucket_start: "2026-10-25T07:00:00Z", input_tokens: 4, estimated_cost: 0},
    {bucket_start: "2026-10-30T19:00:00Z", input_tokens: 8, estimated_cost: 0},
  ], "day", {from: "2026-10-24T07:00:00Z", to: "2026-10-30T20:00:00Z"}, "hour");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-24", "10-25", "10-26", "10-27", "10-28", "10-29", "10-30"], "exactly seven local-day bars, no extra trailing UTC day");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["3", "4", "0", "0", "0", "0", "8"], "UTC hours after local midnight stay on the correct local day");
  assert.match(chart.innerHTML, /本地 2026-10-24 日/);
}));

test("daily chart keeps late buckets on their real local date beyond 800 days", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  // 810 local days: 2023-01-01 through 2025-03-20 (UTC+8). A late bucket must
  // not be folded into the old 800-day cutoff (2025-03-10).
  app.modules.usage.renderDailyChart([
    {bucket_start: "2025-03-20T01:00:00Z", input_tokens: 100, estimated_cost: 0},
  ], "day", {from: "2022-12-31T16:00:00Z", to: "2025-03-20T15:59:59Z"}, "hour");
  const bars = chart.innerHTML.split("<div class='chart-bar-group'").slice(1);
  assert.equal(bars.length, 1, "long ranges draw only the data day, never a capped calendar");
  assert.equal(
    bars.filter(bar => bar.includes("本地 2025-03-10 日") && /chart-value'>100</.test(bar)).length,
    0,
    "late usage is not folded into the 800th day"
  );
  const late = bars.filter(bar => bar.includes("本地 2025-03-20 日"));
  assert.equal(late.length, 1, "the late bucket keeps its own local date");
  assert.match(late[0], /chart-value'>100</, "the late bucket stays on the last day");
  app.modules.usageChart.setUsageChartNote("day", "custom", {from: "2022-12-31T16:00:00Z", to: "2025-03-20T15:59:59Z"});
  assert.match(h.document.getElementById("usageChartNote").textContent, /仅绘制有数据的本地日/, "long ranges disclose that empty days are omitted");
}));

test("daily chart keeps seven local days and discloses whole-hour bucket drift in a half-hour zone", () => withTZ("Asia/Kolkata", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  // Local 2026-10-24 00:00 through 2026-10-30 23:59:59 (UTC+05:30).
  const range = {from: "2026-10-23T18:30:00Z", to: "2026-10-30T18:29:59Z"};
  app.modules.usage.renderDailyChart([
    {bucket_start: "2026-10-24T18:00:00Z", input_tokens: 10, estimated_cost: 0},
    {bucket_start: "2026-10-25T18:00:00Z", input_tokens: 100, estimated_cost: 0},
  ], "day", range, "hour");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-24", "10-25", "10-26", "10-27", "10-28", "10-29", "10-30"], "a half-hour offset still yields exactly seven local-day bars");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["10", "100", "0", "0", "0", "0", "0"], "whole UTC hours are grouped by the local date of their start");
  app.modules.usageChart.setUsageChartNote("day", "custom", range);
  const note = h.document.getElementById("usageChartNote").textContent;
  assert.match(note, /不是整点偏移/, "the note names the non-whole-hour offset");
  assert.match(note, /相邻日/, "the note warns that boundary usage may land on an adjacent local day");
}));

test("custom range ending at local midnight discloses whole-hour boundaries and keeps the adjacent day bar", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    group_by: "hour",
    buckets: [
      {bucket_start: "2026-10-17T17:00:00Z", input_tokens: 5, estimated_cost: 0},
      {bucket_start: "2026-10-20T16:00:00Z", input_tokens: 999, estimated_cost: 0},
    ],
  }));
  const ranges = h.document.querySelectorAll(".time-range-btn");
  click(ranges.find(button => button.dataset.range === "custom"));
  h.document.getElementById("usageFrom").value = "2026-10-18T00:00";
  h.document.getElementById("usageTo").value = "2026-10-21T00:00";
  click(h.document.getElementById("applyCustomRange"));
  await settle();
  const chart = h.document.getElementById("dailyChart");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-18", "10-19", "10-20", "10-21"], "the server's inclusive boundary hour draws the adjacent local day");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["5", "0", "0", "999"]);
  const note = h.document.getElementById("usageChartNote").textContent;
  assert.match(note, /开始与结束边界所在整点小时整桶计入/, "the day note restores the whole-hour boundary disclosure");
  assert.match(note, /不扩展到完整起止 UTC 日/, "the note does not claim full UTC-day coverage");
  assert.doesNotMatch(note, /不是整点偏移/, "a whole-hour offset does not warn about half-hour drift");
}));

test("custom multi-day range re-aggregates hourly buckets into local days through loadUsage", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  app.state.modelGroupsLoaded = true;
  h.context.fetch = async () => response(aggregate({
    group_by: "hour",
    buckets: [
      {bucket_start: "2026-10-19T16:00:00Z", input_tokens: 10, estimated_cost: 0},
      {bucket_start: "2026-10-20T16:00:00Z", input_tokens: 20, estimated_cost: 0},
      {bucket_start: "2026-10-21T16:00:00Z", input_tokens: 30, estimated_cost: 0},
    ],
  }));
  const ranges = h.document.querySelectorAll(".time-range-btn");
  click(ranges.find(button => button.dataset.range === "custom"));
  h.document.getElementById("usageFrom").value = "2026-10-20T00:00";
  h.document.getElementById("usageTo").value = "2026-10-22T01:00";
  click(h.document.getElementById("applyCustomRange"));
  await settle();
  const chart = h.document.getElementById("dailyChart");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-20", "10-21", "10-22"], "loadUsage keeps the hourly response and draws local-day labels");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["10", "20", "30"], "loadUsage sums hourly buckets into the day containing their local start");
}));

test("local-day zero fill stops exactly after 800 days and longer ranges draw only data days", () => withTZ("Asia/Shanghai", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  const cases = [
    {days: 799, to: "2025-03-09T15:59:59Z", bars: 799},
    {days: 800, to: "2025-03-10T15:59:59Z", bars: 800},
    {days: 801, to: "2025-03-11T15:59:59Z", bars: 1},
  ];
  for (const item of cases) {
    app.modules.usage.renderDailyChart([
      {bucket_start: "2023-01-01T01:00:00Z", input_tokens: item.days, estimated_cost: 0},
    ], "day", {from: "2022-12-31T16:00:00Z", to: item.to}, "hour");
    const bars = chart.innerHTML.split("<div class='chart-bar-group'").slice(1);
    assert.equal(bars.length, item.bars, item.days + " local days draw " + item.bars + " bars");
  }
}));

test("daily chart keeps spring-forward days to seven local bars", () => withTZ("America/New_York", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  // Local 2026-03-04 00:00 EST through 2026-03-10 23:59:59 EDT: 2026-03-08 has 23 hours.
  app.modules.usage.renderDailyChart([
    {bucket_start: "2026-03-04T05:00:00Z", input_tokens: 1, estimated_cost: 0},
    {bucket_start: "2026-03-08T07:00:00Z", input_tokens: 2, estimated_cost: 0},
    {bucket_start: "2026-03-09T04:00:00Z", input_tokens: 4, estimated_cost: 0},
  ], "day", {from: "2026-03-04T05:00:00Z", to: "2026-03-11T03:59:59Z"}, "hour");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["03-04", "03-05", "03-06", "03-07", "03-08", "03-09", "03-10"], "the 23-hour day is still one local day");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["1", "0", "0", "0", "2", "4", "0"]);
}));

test("daily chart sums both repeated hours on a fall-back day", () => withTZ("America/New_York", async () => {
  const h = createHarness();
  const app = await h.run();
  const chart = h.document.getElementById("dailyChart");
  // Local 2026-10-28 00:00 EDT through 2026-11-03 23:59:59 EST: 2026-11-01 has 25 hours.
  app.modules.usage.renderDailyChart([
    {bucket_start: "2026-10-28T04:00:00Z", input_tokens: 1, estimated_cost: 0},
    {bucket_start: "2026-11-01T05:00:00Z", input_tokens: 2, estimated_cost: 0},
    {bucket_start: "2026-11-01T06:00:00Z", input_tokens: 4, estimated_cost: 0},
    {bucket_start: "2026-11-02T05:00:00Z", input_tokens: 8, estimated_cost: 0},
  ], "day", {from: "2026-10-28T04:00:00Z", to: "2026-11-04T04:59:59Z"}, "hour");
  const labels = [...chart.innerHTML.matchAll(/class='chart-label'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(labels, ["10-28", "10-29", "10-30", "10-31", "11-01", "11-02", "11-03"], "the 25-hour day is still one local day");
  const values = [...chart.innerHTML.matchAll(/class='chart-value'>([^<]*)</g)].map(m => m[1]);
  assert.deepEqual(values, ["1", "0", "0", "0", "6", "8", "0"], "both 01:00 hours land in the same local day");
}));
