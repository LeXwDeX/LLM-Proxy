"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { assembleAdminHTML } = require("../../../../scripts/testing/assemble_admin_html.cjs");
const test = require("node:test");
const { createHarness } = require("../../../../scripts/testing/admin_ui_test.cjs");
const adminHtml = assembleAdminHTML();
const plain = value => JSON.parse(JSON.stringify(value));
async function fixture() {
  const harness = createHarness();
  const app = await harness.run();
  return {harness, app, quota: app.modules.quota};
}

test("actual quota editor preserves neutral/unlimited/positive and rejects invalid coefficients", async () => {
  const {harness, app, quota} = await fixture();
  const input = {value: "", validity: {badInput: false}};
  const card = {dataset: {editGroupIndex: "0"}, querySelector: () => input};
  harness.document.querySelectorAll = () => [card];
  for (const [raw, expected] of [["", {name: "shared"}], ["0", {name: "shared", coefficient: 0}],
    ["0.8", {name: "shared", coefficient: 0.8}]]) {
    app.state.quotaEditGroups = [{name: "shared", coefficient: 0.5}];
    input.value = raw;
    assert.deepEqual(plain(quota.syncQuotaEditGroups()), [expected]);
  }
  for (const [raw, badInput] of [["-1", false], ["1e999", false], ["", true]]) {
    app.state.quotaEditGroups = [{name: "shared", coefficient: 0.5}];
    input.value = raw; input.validity.badInput = badInput;
    assert.equal(quota.syncQuotaEditGroups(), null);
    assert.deepEqual(plain(app.state.quotaEditGroups), [{name: "shared", coefficient: 0.5}]);
  }
});

test("quota preview separates discount from capacity and actual input updates it", async () => {
  const {harness, app, quota} = await fixture();
  app.state.modelGroups = [{name: "subscription", cost_coefficient: 0.5,
    daily_usd: 10, weekly_usd: 50, monthly_usd: 200}];
  app.state.jobLevels = [{name: "supervisor", coefficient_daily: 5, coefficient_weekly: 5, coefficient_monthly: 5}];
  const expected = "Schema日 $50 / Schema周 $250 / Schema月 $1000";
  assert.equal(quota.quotaEffectivePreview({name: "subscription", coefficient: 1}, app.state.jobLevels[0]), expected);
  app.state.quotaEditGroups = [{name: "subscription", coefficient: 0.5}];
  const preview = {textContent: ""}, input = {value: "1", validity: {badInput: false}};
  const card = {dataset: {editGroupIndex: "0"}, querySelector: selector => selector === "[data-effective-preview]" ? preview : input};
  harness.document.querySelectorAll = () => [card];
  harness.ids.get("quotaEditJobLevel").value = "supervisor";
  harness.ids.get("quotaEditGroupEditor").listeners.input[0]();
  assert.equal(preview.textContent, "可用额度上限预览：" + expected);
});

test("actual model-group save preserves zero coefficient and rejects negative coefficient", async () => {
  const {harness, app} = await fixture();
  app.modules.quota.loadQuotaManagement = async () => {};
  const field = id => harness.ids.get(id);
  field("modelGroupName").value = "shared";
  field("modelGroup-daily").value = "10";
  field("modelGroup-weekly").value = "";
  field("modelGroup-monthly").value = "";
  field("modelGroupCoefficient").value = "0";
  await field("saveModelGroupBtn").listeners.click[0]();
  const writes = () => harness.requests.filter(r => r.url === "/admin/data/model-groups" && r.options?.method === "POST");
  assert.deepEqual(JSON.parse(writes()[0].options.body), {name: "shared", cost_coefficient: 0,
    bindings: [], daily_usd: 10, weekly_usd: 0, monthly_usd: 0});
  field("modelGroupCoefficient").value = "-1";
  await field("saveModelGroupBtn").listeners.click[0]();
  assert.equal(writes().length, 1);
});

test("quota summary separates unlimited, missing job and missing plan", async () => {
  const {quota} = await fixture();
  const groups = {shared: {in_plan: true, quotas: {daily: {limit: 0}}}};
  assert.equal(quota.quotaRowStatus({client: {}, status: {groups}}).key, "unlimited");
  assert.match(quota.quotaBarCell({limit: 0}), /不限/);
  assert.match(quota.quotaBarCell({limit: 0}, "职级未配置"), /职级未配置/);
  assert.equal(quota.quotaRowStatus({client: {}, status: {job_level_missing: true, groups}}).key, "joblevel");
  assert.equal(quota.quotaRowStatus({client: {}, status: {groups: {}}}).key, "none");
});

test("accounting failures display unavailable usage and escaped diagnostics", async () => {
  const {harness, app, quota} = await fixture();
  const status = {calibration_error: "<img src=x onerror=bad()>",
    exceeded: {scope: "accounting", dimension: "unavailable"}, groups: {}};
  const row = quota.quotaRowStatus({client: {name: "client"}, status});
  assert.equal(row.key, "accounting");
  assert.equal(row.label, "计费校准不可用");
  const details = quota.quotaConvertedDetail(status);
  assert.match(details, /&lt;img/);
  assert.doesNotMatch(details, /<img/);
  app.state.quotaSummary = [{client: {name: "client"}, status}];
  quota.renderQuotaSummary();
  assert.match(harness.ids.get("quotaSummaryBody").innerHTML, /校准不可用/);
  assert.doesNotMatch(harness.ids.get("quotaSummaryBody").innerHTML, /额度超限|>不限</);
});

function quotaClient(name, groups, quotas, extra = {}) {
  return {
    client: Object.assign({name, ip_address: name + "-ip", project_group: "P1", job_level: "L1"}, extra.client || {}),
    status: Object.assign({
      client: name,
      quotas: quotas || {},
      groups: groups || {},
      job_level: "L1",
    }, extra.status || {}),
  };
}
function finiteQuotas(limit, used) {
  return {daily: {limit, used}, weekly: {limit, used}, monthly: {limit, used}};
}
function labels(select) {
  return select.children
    .filter(option => option.tagName === "OPTION")
    .map(option => option.textContent.trim());
}
function statusRow(harness, name) {
  return harness.ids.get("quotaSummaryBody").innerHTML.split("</tr>")
    .find(row => row.includes("data-quota-person='" + name + "'")) || "";
}
function localStamp(iso) {
  const d = new Date(iso);
  const pad = n => String(n).padStart(2, "0");
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " +
    pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}
function prepareStatusOptions(harness) {
  const markup = adminHtml.match(/<select[^>]*id="quotaSummaryStatusFilter"[^>]*>([\s\S]*?)<\/select>/);
  assert.ok(markup, "page declares the quota status options");
  harness.ids.get("quotaSummaryStatusFilter").innerHTML = markup[1];
}
function summaryFixture() {
  return [
    quotaClient("alice", {shared: {in_plan: true, quotas: finiteQuotas(100, 0)}}, finiteQuotas(100, 0)),
    quotaClient("bob", {shared: {in_plan: true, quotas: finiteQuotas(100, 95)}}, finiteQuotas(100, 95), {client: {job_level: "L2"}}),
    quotaClient("carol", {shared: {in_plan: true, exceeded: true, quotas: finiteQuotas(100, 110)}}, finiteQuotas(100, 110), {client: {project_group: "P2"}}),
    quotaClient("dave", {shared: {in_plan: true, quotas: finiteQuotas(100, 1)}}, {}, {client: {project_group: "P2", job_level: "L2"}, status: {job_level_missing: true}}),
    quotaClient("erin", {shared: {in_plan: true, quotas: {daily: {limit: 0, used: 1}, weekly: {limit: 0, used: 1}, monthly: {limit: 0, used: 1}}}}, {daily: {limit: 0, used: 1}, weekly: {limit: 0, used: 1}, monthly: {limit: 0, used: 1}}, {client: {job_level: ""}}),
    quotaClient("frank", {}, {}, {client: {project_group: "", job_level: "L1"}}),
    quotaClient("grace", {shared: {in_plan: true, quotas: finiteQuotas(100, 1)}}, {}, {client: {project_group: "P2"}, status: {calibration_error: "pricing unavailable"}}),
  ];
}

test("quota page presents the approved formula and keeps the billing note static", async () => {
  await fixture();
  const formula = adminHtml.match(/id="quotaFormula"[\s\S]*?<\/section>/);
  assert.ok(formula, "quota formula strip is part of the page");
  for (const text of ["模型组基数", "职级系数", "个人系数", "扣费系数", "不限", "未配置", "不乘入", "独立限额组", "不是共享额度池"])
    assert.match(formula[0], new RegExp(text), "formula explains " + text);
  assert.match(formula[0], /模型组基数、职级系数、个人系数和模型组扣费系数均为正数时才生成正上限/);
  assert.match(formula[0], /扣费系数为 0 时该组不限/);
  assert.match(formula[0], /扣费系数[^。；]*不乘入上限公式/);
  assert.doesNotMatch(formula[0], /模型组基数、职级系数和个人系数均为正数时生成正上限/,
    "the three-factor copy omits the backend cost-coefficient condition");
  const billing = adminHtml.match(/id="quotaBillingBasis"[^>]*>([\s\S]*?)<\/(?:p|div)>/)[1];
  for (const text of ["官方", "目录默认价", "不估算 token", "模型组扣费系数"])
    assert.match(billing, new RegExp(text), "billing basis keeps " + text);
});

test("quota detail names the cost-coefficient condition for a positive capacity", async () => {
  const {quota} = await fixture();
  const quotas = {daily: {limit: 0, used: 0}, weekly: {limit: 0, used: 0}, monthly: {limit: 0, used: 0}};
  const status = {job_level: "L1", groups: {
    shared: {in_plan: true, cost_coefficient: 0, plan_coefficient: 1,
      rank_coefficients: {daily: 1, weekly: 1, monthly: 1},
      base_limits: {daily: 10, weekly: 50, monthly: 200}, quotas},
  }};
  const detail = quota.quotaConvertedDetail(status);
  assert.match(detail, /扣费系数只用于消耗折算，不乘入可用上限；但扣费系数为 0 时该组不限/);
  assert.match(detail, /可用上限：基数 [^<]*扣费系数为 0 时该组不限[^<]*；折算消耗/);
  assert.match(detail, /折算消耗：原价 × 扣费系数 0/);
});

test("quota detail explains an empty group list instead of returning blank copy", async () => {
  const {quota} = await fixture();
  assert.match(quota.quotaConvertedDetail({job_level: "L1", groups: {}}), /未加入模型组/);
  assert.match(quota.quotaConvertedDetail({groups: {}}), /暂无用量/);
  const missingLevel = quota.quotaConvertedDetail({job_level_missing: true, groups: {}});
  assert.match(missingLevel, /未绑定职级/);
  assert.doesNotMatch(missingLevel, /未加入模型组|暂无用量/);
});

test("quota token total reuses the shared total including tool input", async () => {
  const {quota} = await fixture();
  assert.equal(quota.quotaTotalTok({input_tokens: 10, output_tokens: 5, cached_tokens: 2, cache_creation_tokens: 3, tool_use_prompt_tokens: 7}), 27);
});

test("quota status filter reuses existing options and counts the current search/project/job scope", async () => {
  const {harness, app, quota} = await fixture();
  prepareStatusOptions(harness);
  app.state.quotaSummary = summaryFixture();
  quota.renderQuotaSummary();
  assert.deepEqual(labels(harness.ids.get("quotaSummaryStatusFilter")), [
    "全部状态 (7)", "正常 (1)", "接近限额 (1)", "职级未配置 (1)", "计费校准不可用 (1)",
    "模型组超限 (1)", "不限 (1)", "未配置 (1)",
  ]);

  harness.ids.get("quotaSummaryProjectFilter").value = "P1";
  quota.renderQuotaSummary();
  assert.deepEqual(labels(harness.ids.get("quotaSummaryStatusFilter")), [
    "全部状态 (3)", "正常 (1)", "接近限额 (1)", "职级未配置 (0)", "计费校准不可用 (0)",
    "模型组超限 (0)", "不限 (1)", "未配置 (0)",
  ]);
  harness.ids.get("quotaSummaryJobFilter").value = "L1";
  quota.renderQuotaSummary();
  assert.deepEqual(labels(harness.ids.get("quotaSummaryStatusFilter")), [
    "全部状态 (1)", "正常 (1)", "接近限额 (0)", "职级未配置 (0)", "计费校准不可用 (0)",
    "模型组超限 (0)", "不限 (0)", "未配置 (0)",
  ]);
  harness.ids.get("quotaSearch").value = "alice";
  quota.renderQuotaSummary();
  assert.match(harness.ids.get("quotaSummaryBody").innerHTML, /alice/);
  assert.doesNotMatch(harness.ids.get("quotaSummaryBody").innerHTML, /data-quota-person='bob'/);

  harness.ids.get("quotaSearch").value = "";
  harness.ids.get("quotaSummaryProjectFilter").value = "";
  harness.ids.get("quotaSummaryJobFilter").value = "";
  harness.ids.get("quotaSummaryStatusFilter").value = "group";
  quota.renderQuotaSummary();
  assert.match(harness.ids.get("quotaSummaryBody").innerHTML, /data-quota-person='carol'/);
  assert.doesNotMatch(harness.ids.get("quotaSummaryBody").innerHTML, /data-quota-person='alice'/);
  assert.match(labels(harness.ids.get("quotaSummaryStatusFilter")).join("|"), /全部状态 \(7\)/,
    "filtering by status does not change the other-filter counts");
});

test("quota summary renders one accessible bar per schema dimension with threshold and zero semantics", async () => {
  const {harness, app, quota} = await fixture();
  app.state.quotaSummary = summaryFixture();
  quota.renderQuotaSummary();
  const alice = statusRow(harness, "alice");
  assert.equal((alice.match(/role='progressbar'/g) || []).length, 3, "three finite dimensions render bars");
  assert.match(alice, /aria-valuenow='0'/);
  assert.match(alice, /\$0\.00 \/ \$100\.00/);
  assert.match(alice, /role='progressbar'[^>]*aria-label='[^']*Schema日[^']*已用[^']*0%'/);
  assert.match(statusRow(harness, "bob"), /接近限额/);
  assert.match(statusRow(harness, "bob"), /q-over/);
  assert.match(quota.quotaBarCell({limit: 100, used: 75}), /q-warn/);
  assert.match(quota.quotaBarCell({limit: 100, used: 90}), /q-over/);
  assert.match(statusRow(harness, "carol"), /q-over/);
  assert.match(statusRow(harness, "carol"), /超出上限/);
  const erin = statusRow(harness, "erin");
  assert.match(erin, /不限/);
  assert.doesNotMatch(erin, /role='progressbar'/);
  assert.match(erin, /aria-label='[^']*Schema日[^']*不限/);
  assert.equal((erin.match(/quota-bar-inner/g) || []).length, 3, "unlimited keeps three bars");
  assert.match(statusRow(harness, "dave"), /职级未配置/);
  assert.equal((statusRow(harness, "dave").match(/quota-bar-inner/g) || []).length, 3, "missing job keeps three bars");
  assert.match(statusRow(harness, "frank"), /未配置/);
  assert.equal((statusRow(harness, "frank").match(/quota-bar-inner/g) || []).length, 3, "unconfigured keeps three bars");
  assert.match(harness.ids.get("quotaSummaryTableHead").innerHTML, /估算费用（含模型组系数）/);
  assert.match(harness.ids.get("quotaSummaryTableHead").innerHTML, /Schema日已用 \/ 可用/);
});

test("quota summary discloses estimated cost, incomplete records and missing usage instead of fabricating zero", async () => {
  const {harness, app, quota} = await fixture();
  app.state.quotaSummary = summaryFixture();
  app.state.clientUsage30d = {
    alice: {input_tokens: 10, output_tokens: 5, cached_tokens: 2, cache_creation_tokens: 3, tool_use_prompt_tokens: 7,
      estimated_cost: 0, incomplete_usage_count: 2, incomplete_cost_count: 1},
    bob: {input_tokens: 1, estimated_cost: null, incomplete_cost_count: 3},
    carol: {input_tokens: 4, estimated_cost: 2.5, incomplete_usage_count: 1},
    dave: {estimated_cost: 1.5, incomplete_usage_count: 2},
  };
  quota.renderQuotaSummary();
  const cell = (name, cls) => (statusRow(harness, name).match(
    new RegExp("<td class='quota-number " + cls + "'>([\\s\\S]*?)<\\/td>")) || [])[1] || "";
  const alice = statusRow(harness, "alice");
  assert.equal(cell("alice", "quota-token-cell"),
    "27<small class='quota-usage-note'>用量不完整 2 条记录</small>", "tokens include tool input");
  assert.equal(cell("alice", "quota-cost-cell"),
    "$0.00<small class='quota-cost-note'>费用不完整 1 条记录</small>");
  assert.match(alice, /用量不完整 2 条记录/);
  assert.match(alice, /费用不完整 1 条记录/);
  assert.match(cell("bob", "quota-cost-cell"), /^<span class='quota-usage-unavailable'>不可用<\/span>/);
  assert.doesNotMatch(cell("bob", "quota-cost-cell"), /\$0\.00/);
  assert.match(cell("bob", "quota-cost-cell"), /费用不完整 3 条记录/,
    "a missing cost still discloses its incomplete record count");
  assert.equal(cell("bob", "quota-token-cell"), "1");
  assert.match(cell("carol", "quota-cost-cell"), /^\$2\.50/);
  assert.match(cell("carol", "quota-token-cell"), /用量不完整 1 条记录/);
  assert.match(cell("dave", "quota-token-cell"), /^<span class='quota-usage-unavailable'>不可用<\/span>/);
  assert.match(cell("dave", "quota-token-cell"), /用量不完整 2 条记录/,
    "missing tokens still disclose their incomplete record count");
  assert.equal(cell("frank", "quota-cost-cell"), "$0.00", "a client with no aggregate row has zero recorded usage");
  app.state.clientUsage30dUnavailable = true;
  quota.renderQuotaSummary();
  assert.match(statusRow(harness, "alice"), /不可用/);
  assert.match(statusRow(harness, "alice"), /30 天用量/);
});

test("a failed 30-day aggregate load is explicit while quota rows still load", async () => {
  const harness = createHarness({ routes: { "/admin/data/targets": { targets: [] } } });
  const app = await harness.run();
  const quota = app.modules.quota;
  const baseFetch = harness.context.fetch;
  harness.context.fetch = async (url, options) => {
    if (String(url).includes("/admin/data/usage/aggregate"))
      return {ok: false, status: 503, text: async () => JSON.stringify({error: "aggregate offline"})};
    return baseFetch(url, options);
  };
  harness.setRoute("/admin/data/quota/summary", {clients: [
    quotaClient("alice", {shared: {in_plan: true, quotas: finiteQuotas(100, 1)}}, finiteQuotas(100, 1)),
  ]});
  await quota.loadQuotaManagement();
  assert.equal(app.state.clientUsage30dUnavailable, true);
  assert.match(harness.ids.get("quotaSummaryBody").innerHTML, /alice/);
  assert.match(statusRow(harness, "alice"), /30 天用量加载失败/);
});

test("reset time appears only inside per-group details and never as a uniform summary value", async () => {
  const {harness, app, quota} = await fixture();
  const daily = "2026-10-26T00:00:00Z", weekly = "2026-11-02T00:00:00Z", monthly = "2026-11-01T00:00:00Z";
  const groups = {
    shared: {in_plan: true, base_limits: {daily: 100, weekly: 500, monthly: 1000},
      rank_coefficients: {daily: 1, weekly: 1, monthly: 1}, plan_coefficient: 1, cost_coefficient: 1,
      quotas: {daily: {limit: 100, used: 10, resets_at: daily}, weekly: {limit: 500, used: 20, resets_at: weekly}, monthly: {limit: 1000, used: 30, resets_at: monthly}}},
    fallback: {in_plan: false, quotas: {daily: {limit: 0, used: 2, resets_at: "2026-10-27T00:00:00Z"}}},
  };
  const status = {client: "alice", quotas: {daily: {limit: 100, used: 10, resets_at: daily}}, groups};
  const detail = quota.quotaConvertedDetail(status);
  assert.match(detail, /独立限额组/);
  assert.match(detail, /不是共享额度池/);
  assert.ok(detail.includes(localStamp(daily)), "daily reset is shown per group");
  assert.ok(detail.includes(localStamp(weekly)), "weekly reset is shown per group");
  assert.ok(detail.includes(localStamp(monthly)), "monthly reset is shown per group");
  assert.ok(detail.includes(localStamp("2026-10-27T00:00:00Z")), "fallback group has its own reset");
  assert.match(detail, /本地时间/);
  assert.equal(quota.quotaResetText("0001-01-01T00:00:00Z"), "", "Go zero time is not a real reset");
  assert.doesNotMatch(detail, /0001-01-01/);

  app.state.quotaSummary = [quotaClient("alice", groups, {daily: {limit: 100, used: 10}})];
  app.state.quotaExpanded = {};
  quota.renderQuotaSummary();
  assert.doesNotMatch(harness.ids.get("quotaSummaryBody").innerHTML, /下次重置/,
    "collapsed rows do not show a reset time");
  app.state.quotaExpanded = {alice: true};
  quota.renderQuotaSummary();
  assert.match(harness.ids.get("quotaSummaryBody").innerHTML, /下次重置/);
  assert.ok(harness.ids.get("quotaSummaryBody").innerHTML.includes(localStamp(weekly)));
});

test("quota summary stays read-only and renders every Schema dimension", async () => {
  const readonly = createHarness({role: "readonly"});
  const readonlyApp = await readonly.run();
  readonlyApp.state.quotaSummary = [quotaClient("alice", {shared: {in_plan: true, quotas: finiteQuotas(100, 1)}}, finiteQuotas(100, 1))];
  readonlyApp.modules.quota.renderQuotaSummary();
  assert.match(readonly.ids.get("quotaSummaryBody").innerHTML, /readonly-hide/);
  readonlyApp.modules.quota.openQuotaEditDrawer(readonlyApp.state.quotaSummary[0]);
  assert.equal(readonly.ids.get("quotaEditDrawer").classList.contains("open"), false,
    "readonly users cannot open the quota drawer");

  const {schemaFixture} = require("../../../../scripts/testing/admin_ui_test.cjs");
  const schema = schemaFixture();
  schema.quota_dimensions.push({value: "yearly", label: "Schema年", base_field: "yearly_usd", job_coefficient_field: "coefficient_yearly"});
  const extended = createHarness({schema});
  const extendedApp = await extended.run();
  const all = {daily: {limit: 10, used: 1}, weekly: {limit: 10, used: 1}, monthly: {limit: 10, used: 1}, yearly: {limit: 10, used: 1}};
  extendedApp.state.quotaSummary = [{
    client: {name: "schema-client"},
    status: {client: "schema-client", quotas: all, groups: {g: {in_plan: true, quotas: all}}},
  }];
  extendedApp.modules.quota.renderQuotaSummary();
  assert.equal((extended.ids.get("quotaSummaryBody").innerHTML.match(/role='progressbar'/g) || []).length, 4,
    "all dimensions come from Schema, not a hardcoded three-item list");
  assert.match(extended.ids.get("quotaSummaryTableHead").innerHTML, /Schema年已用 \/ 可用/);
});
