"use strict";
// Quota page regressions: bad numeric input, explicit-zero base, in-flight guards, failed-load
// tables, summary-before-usage rendering, no-job-level preview, case-insensitive job levels,
// debounced search and indexed model labels. Uses the real scripts in the vm fixture.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");
const {fakeClock} = require("./fake-clock.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const tick = async (n = 5) => { for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve)); };
const routes = () => ({"/admin/data/targets": {targets: [{name: "t1", endpoint_type: "openai", model_mappings: [{upstream: "m1"}]}]},
  "/admin/data/catalog": {models: [{endpoint_type: "openai", model: "m1", display_name: "Model One"}]},
  "/admin/data/model-groups": {model_groups: [{name: "shared", cost_coefficient: 1, daily_usd: 10, weekly_usd: 0, monthly_usd: 0, bindings: []}]},
  "/admin/data/job-levels": {job_levels: [{name: "Lead", coefficient_daily: 2}]},
  "/admin/data/project-groups": {project_groups: ["alpha"]},
  "/admin/data/quota/summary": {clients: []}});
async function fixture(options = {}) {
  const h = createHarness({routes: routes(), ...options});
  const app = await h.run();
  return {h, app, quota: app.modules.quota};
}
const field = (h, id) => h.ids.get(id);
const sent = (h, method, url) => h.requests.filter(r => r.options?.method === method && r.url === url);
// Hold one write request until released so a second click can be attempted in flight.
function holdWrites(h, matcher) {
  const base = h.context.fetch;
  const state = {release: null, started: 0};
  h.context.fetch = (url, options) => matcher(url, options)
    ? new Promise(resolve => { state.started++; state.release = () => resolve(json({})); h.requests.push({url, options}); })
    : base(url, options);
  return state;
}

test("job level coefficients: blank is omitted, 0 is kept, unparseable text is rejected", async () => {
  const {h} = await fixture();
  field(h, "jobLevelName").value = "Senior";
  field(h, "jobLevel-daily").value = "";
  field(h, "jobLevel-weekly").value = "0";
  field(h, "jobLevel-monthly").value = "2.5";
  await field(h, "saveJobLevelBtn").listeners.click[0]();
  assert.deepEqual(JSON.parse(sent(h, "POST", "/admin/data/job-levels")[0].options.body),
    {name: "Senior", coefficient_weekly: 0, coefficient_monthly: 2.5});
  field(h, "jobLevel-monthly").value = "";
  field(h, "jobLevel-monthly").validity = {badInput: true};
  await field(h, "saveJobLevelBtn").listeners.click[0]();
  assert.equal(sent(h, "POST", "/admin/data/job-levels").length, 1, "badInput must not silently mean ×1");
  field(h, "jobLevel-monthly").validity = {badInput: false};
  field(h, "jobLevel-monthly").value = "1abc";
  await field(h, "saveJobLevelBtn").listeners.click[0]();
  assert.equal(sent(h, "POST", "/admin/data/job-levels").length, 1, "partially numeric text is not a number");
});

test("model group bases need an explicit value; 0 means unlimited and is sent as 0", async () => {
  const {h} = await fixture();
  field(h, "modelGroupName").value = "grp";
  field(h, "modelGroupCoefficient").value = "1";
  field(h, "modelGroup-daily").value = "5";
  field(h, "modelGroup-weekly").value = "";
  field(h, "modelGroup-monthly").value = "0";
  await field(h, "saveModelGroupBtn").listeners.click[0]();
  assert.equal(sent(h, "POST", "/admin/data/model-groups").length, 0, "blank base must not silently become unlimited");
  field(h, "modelGroup-weekly").value = "0";
  await field(h, "saveModelGroupBtn").listeners.click[0]();
  assert.deepEqual(JSON.parse(sent(h, "POST", "/admin/data/model-groups")[0].options.body),
    {name: "grp", cost_coefficient: 1, bindings: [], daily_usd: 5, weekly_usd: 0, monthly_usd: 0});
});

test("model group unparseable base or coefficient is rejected", async () => {
  const {h} = await fixture();
  for (const [id, label] of [["modelGroup-weekly", "base"], ["modelGroupCoefficient", "coefficient"]]) {
    field(h, "modelGroupName").value = "grp";
    field(h, "modelGroupCoefficient").value = "1";
    for (const dim of ["daily", "weekly", "monthly"]) field(h, "modelGroup-" + dim).value = "1";
    field(h, id).value = "";
    field(h, id).validity = {badInput: true};
    await field(h, "saveModelGroupBtn").listeners.click[0]();
    assert.equal(sent(h, "POST", "/admin/data/model-groups").length, 0, label);
    field(h, id).validity = {badInput: false};
  }
});

test("saving a job level twice in flight sends one request", async () => {
  const {h} = await fixture();
  field(h, "jobLevelName").value = "Senior";
  const hold = holdWrites(h, (url, o) => o?.method === "POST" && url === "/admin/data/job-levels");
  const first = field(h, "saveJobLevelBtn").listeners.click[0]();
  const second = field(h, "saveJobLevelBtn").listeners.click[0]();
  await tick();
  assert.equal(hold.started, 1);
  assert.equal(field(h, "saveJobLevelBtn").disabled, true);
  hold.release();
  await first; await second;
  assert.equal(field(h, "saveJobLevelBtn").disabled, false);
});

test("saving a dictionary entry twice in flight sends one request", async () => {
  const {h, app} = await fixture();
  await app.modules.quota.openDictModal("project", null);
  field(h, "dictEntryName").value = "beta";
  const hold = holdWrites(h, (url, o) => o?.method === "POST" && url === "/admin/data/project-groups");
  const first = field(h, "saveDictEntryBtn").listeners.click[0]();
  const second = field(h, "saveDictEntryBtn").listeners.click[0]();
  await tick();
  assert.equal(hold.started, 1);
  hold.release();
  await first; await second;
  assert.equal(field(h, "saveDictEntryBtn").disabled, false);
});

test("saving the quota drawer twice in flight sends one request", async () => {
  const {h, app, quota} = await fixture();
  quota.openQuotaEditDrawer({client: {name: "c1", job_level: "Lead", quota_groups: []}});
  const hold = holdWrites(h, (url, o) => o?.method === "PUT" && url === "/admin/data/quota/c1");
  const first = field(h, "saveQuotaEditBtn").listeners.click[0]();
  const second = field(h, "saveQuotaEditBtn").listeners.click[0]();
  await tick();
  assert.equal(hold.started, 1);
  hold.release();
  await first; await second;
  assert.equal(field(h, "saveQuotaEditBtn").disabled, false);
  assert.ok(app);
});

test("confirming quota sync twice in flight sends one request", async () => {
  const {h} = await fixture();
  const sources = field(h, "quotaSyncSource");
  sources.value = "c1";
  field(h, "quotaSyncMode").value = "job_level";
  field(h, "quotaSyncJobTarget").value = "Lead";
  const hold = holdWrites(h, (url, o) => o?.method === "POST" && url === "/admin/data/quota/c1/sync");
  field(h, "syncQuotaBtn").listeners.click[0]();
  field(h, "confirmOkBtn").listeners.click[0]();
  field(h, "syncQuotaBtn").listeners.click[0]();
  field(h, "confirmOkBtn").listeners.click[0]();
  await tick();
  assert.equal(hold.started, 1);
  hold.release();
  await tick(10);
  assert.equal(field(h, "syncQuotaBtn").disabled, false);
});

test("a failed quota load leaves no table on 加载中", async () => {
  const {h, quota} = await fixture();
  for (const id of ["modelGroupsBody", "jobLevelsBody", "projectGroupsBody"])
    field(h, id).innerHTML = "<tr class='empty-row'><td>加载中...</td></tr>";
  h.setRouteFailure("/admin/data/quota/summary", true);
  await quota.loadQuotaManagement();
  for (const id of ["quotaSummaryBody", "modelGroupsBody", "jobLevelsBody", "projectGroupsBody"]) {
    assert.doesNotMatch(field(h, id).innerHTML, /加载中/, id);
    assert.match(field(h, id).innerHTML, /加载失败/, id);
  }
});

test("the summary renders before the 30-day usage arrives, then fills in", async () => {
  const {h, app, quota} = await fixture();
  h.setRoute("/admin/data/quota/summary", {clients: [{client: {name: "c1", job_level: "Lead"}, status: {groups: {}}}]});
  const base = h.context.fetch;
  let release;
  h.context.fetch = (url, options) => String(url).includes("/admin/data/usage/aggregate")
    ? new Promise(resolve => { release = () => resolve(json({by_client: [{key: "c1", input_tokens: 1200, estimated_cost: 3.5}]})); })
    : base(url, options);
  const load = quota.loadQuotaManagement();
  await tick(20);
  assert.match(field(h, "quotaSummaryBody").innerHTML, /c1/, "people rows appear while usage is still pending");
  assert.match(field(h, "quotaSummaryBody").innerHTML, /加载中/, "usage cells say loading, not unavailable");
  assert.doesNotMatch(field(h, "quotaSummaryBody").innerHTML, /不可用/);
  assert.match(field(h, "modelGroupsBody").innerHTML, /shared/);
  release();
  await load;
  assert.match(field(h, "quotaSummaryBody").innerHTML, /1,?200/);
  assert.equal(app.state.clientUsage30dUnavailable, false);
});

test("quota preview without a job level never shows a positive limit", async () => {
  const {app, quota} = await fixture();
  app.state.modelGroups = [{name: "subscription", cost_coefficient: 1, daily_usd: 10, weekly_usd: 50, monthly_usd: 200}];
  const preview = quota.quotaEffectivePreview({name: "subscription", coefficient: 1}, null);
  assert.doesNotMatch(preview, /\$/);
  assert.match(preview, /未绑/);
  const withLevel = quota.quotaEffectivePreview({name: "subscription", coefficient: 1}, {name: "L", coefficient_daily: 2});
  assert.match(withLevel, /\$20/);
});

test("job level options match case-insensitively without duplicates", async () => {
  const {quota} = await fixture();
  const html = quota.dictOptionsHTML([{name: "Lead"}, {name: "Staff"}], "lead", "（空）");
  assert.equal((html.match(/<option/g) || []).length, 3);
  assert.match(html, /<option value='Lead' selected>/);
  const unknown = quota.dictOptionsHTML([{name: "Lead"}], "Ghost", "（空）");
  assert.match(unknown, /<option value='Ghost' selected>/);
});

test("summary search re-renders once per pause, not per keystroke", async () => {
  const clock = fakeClock();
  const {h, app} = await fixture({timers: clock});
  app.state.quotaSummary = [{client: {name: "alpha"}, status: {groups: {}}}, {client: {name: "beta"}, status: {groups: {}}}];
  const body = field(h, "quotaSummaryBody");
  let renders = 0;
  let html = "";
  Object.defineProperty(body, "innerHTML", {get: () => html, set: v => { renders++; html = v; }});
  const search = field(h, "quotaSearch");
  for (const text of ["a", "al", "alp"]) { search.value = text; search.listeners.input[0](); }
  assert.equal(renders, 0, "no synchronous re-render per keystroke");
  await clock.advance(1000);
  assert.equal(renders, 1);
  assert.match(html, /alpha/);
  assert.doesNotMatch(html, /beta/);
});

test("model labels come from an index that follows catalog changes", async () => {
  const {app, quota} = await fixture();
  const target = {endpoint_type: "openai"};
  let scans = 0;
  const entry = {endpoint_type: "openai", get model() { scans++; return "m1"; }, display_name: "One"};
  app.state.catalogAllCache = [entry];
  assert.equal(quota.quotaModelLabel(target, "m1"), "m1 · One");
  const afterFirst = scans;
  for (let i = 0; i < 50; i++) quota.quotaModelLabel(target, "m1");
  assert.equal(scans, afterFirst, "repeated lookups must not rescan the catalog");
  app.state.catalogAllCache = [{endpoint_type: "openai", model: "m1", display_name: "Two"}];
  assert.equal(quota.quotaModelLabel(target, "m1"), "m1 · Two");
});
