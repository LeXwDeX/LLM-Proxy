"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const response = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});

test("an old quota load cannot erase a newly saved job level", async () => {
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}, "/admin/data/job-levels": {job_levels: []}}});
  const app = await h.run();
  const base = h.context.fetch;
  let finishOld;
  let delay = true;
  h.context.fetch = (url, options) => {
    if (String(url).includes("/admin/data/usage/aggregate") && delay) {
      delay = false;
      return new Promise(resolve => { finishOld = resolve; });
    }
    if (url === "/admin/data/job-levels" && options?.method === "POST") {
      h.setRoute(url, {job_levels: [{name: "new-level", coefficient_daily: 2}]});
      return Promise.resolve(response({}));
    }
    return base(url, options);
  };
  const old = app.modules.quota.loadQuotaManagement();
  await new Promise(resolve => setImmediate(resolve));
  h.ids.get("newJobLevelBtn").listeners.click[0]();
  h.ids.get("jobLevelName").value = "new-level";
  await h.ids.get("saveJobLevelBtn").listeners.click[0]();
  assert.match(h.ids.get("jobLevelsBody").innerHTML, /new-level/);
  finishOld(response({by_client: []}));
  await old;
  assert.match(h.ids.get("jobLevelsBody").innerHTML, /new-level/);
  assert.equal(app.state.jobLevels[0].name, "new-level");
});

test("an old failed quota load cannot replace the latest summary", async () => {
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}}});
  const app = await h.run();
  const base = h.context.fetch;
  let failOld;
  let delay = true;
  h.context.fetch = (url, options) => {
    if (url === "/admin/data/quota/summary" && delay) {
      delay = false;
      return new Promise(resolve => { failOld = resolve; });
    }
    return base(url, options);
  };
  const old = app.modules.quota.loadQuotaManagement();
  h.setRoute("/admin/data/quota/summary", {clients: [{client: {name: "new-client"}, status: {groups: {}}}]});
  await app.modules.quota.loadQuotaManagement();
  assert.match(h.ids.get("quotaSummaryBody").innerHTML, /new-client/);
  failOld({ok: false, status: 503, text: async () => '{"error":"old failure"}'});
  await old;
  assert.match(h.ids.get("quotaSummaryBody").innerHTML, /new-client/);
  assert.doesNotMatch(h.ids.get("quotaSummaryBody").innerHTML, /加载失败/);
});

test("an earlier dictionary load cannot overwrite a completed quota refresh", async () => {
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}, "/admin/data/job-levels": {job_levels: []}}});
  const app = await h.run();
  app.state.dictsLoaded = false;
  const base = h.context.fetch;
  let finishOld;
  let delay = true;
  h.context.fetch = (url, options) => {
    if (url === "/admin/data/project-groups" && delay) {
      delay = false;
      return new Promise(resolve => { finishOld = resolve; });
    }
    return base(url, options);
  };
  const old = app.modules.quota.loadDictOptions();
  await new Promise(resolve => setImmediate(resolve));
  h.setRoute("/admin/data/job-levels", {job_levels: [{name: "new-level"}]});
  await app.modules.quota.loadQuotaManagement();
  finishOld(response({project_groups: []}));
  await old;
  assert.equal(app.state.jobLevels[0].name, "new-level");
});

test("an invalidated dictionary load retries while the quota refresh is still pending", async () => {
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}, "/admin/data/job-levels": {job_levels: []}}});
  const app = await h.run();
  app.state.dictsLoaded = false;
  const base = h.context.fetch;
  let finishDict, finishQuota;
  let delayDict = true;
  h.context.fetch = (url, options) => {
    if (url === "/admin/data/project-groups" && delayDict) {
      delayDict = false;
      return new Promise(resolve => { finishDict = resolve; });
    }
    if (String(url).includes("/admin/data/usage/aggregate")) {
      return new Promise(resolve => { finishQuota = resolve; });
    }
    return base(url, options);
  };
  const old = app.modules.quota.loadDictOptions();
  await new Promise(resolve => setImmediate(resolve));
  h.setRoute("/admin/data/job-levels", {job_levels: [{name: "new-level"}]});
  const refreshing = app.modules.quota.loadQuotaManagement();
  finishDict(response({project_groups: []}));
  await old;
  assert.equal(app.state.dictsLoaded, true);
  assert.equal(app.state.jobLevels[0].name, "new-level");
  finishQuota(response({by_client: []}));
  await refreshing;
});
