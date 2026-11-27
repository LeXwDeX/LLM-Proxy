"use strict";
// 模型组页顶部提示「尚未纳入管理的模型」：额度只由模型组绑定授予，没有绑定的
// (目标, 上游模型) 在准入时一律 403 quota_not_configured，所以要在页面上显式列出，
// 避免新增目标/模型后忘记加入模型组。
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const group = (name, bindings) => ({name, cost_coefficient: 1, daily_usd: 10, weekly_usd: 0, monthly_usd: 0, bindings});
const target = (name, upstreams, extra = {}) => Object.assign(
  {name, endpoint_type: "openai", model_mappings: upstreams.map(upstream => ({upstream}))}, extra);

async function boot({targets, groups}) {
  const h = createHarness({routes: {
    "/admin/data/targets": {targets},
    "/admin/data/catalog": {models: []},
    "/admin/data/model-groups": {model_groups: groups},
    "/admin/data/job-levels": {job_levels: []},
    "/admin/data/project-groups": {project_groups: []},
    "/admin/data/quota/summary": {clients: []},
  }});
  const app = await h.run();
  await app.modules.quota.loadQuotaManagement();
  return {h, app, html: () => h.ids.get("unmanagedModelsNotice").innerHTML};
}

test("列出没有任何绑定覆盖的 (目标, 模型)，已绑定的不出现", async () => {
  const {html} = await boot({
    targets: [target("t1", ["m-bound", "m-free"]), target("t2", ["x1"]), target("t3", ["y1", "y2"])],
    groups: [
      group("g1", [{target: "t1", model: "m-bound"}]),
      group("g2", [{target: "T3"}]), // 整目标绑定，大小写不敏感
    ],
  });
  const out = html();
  assert.match(out, /尚未纳入管理/);
  assert.match(out, /t1/);
  assert.match(out, /m-free/);
  assert.doesNotMatch(out, /m-bound/);
  assert.match(out, /t2/);
  assert.match(out, /x1/);
  assert.doesNotMatch(out, /t3|y1|y2/);
});

test("没有模型映射且无整目标绑定的目标按「全部模型」列出", async () => {
  const {html} = await boot({targets: [target("bare", [])], groups: []});
  assert.match(html(), /bare/);
  assert.match(html(), /全部模型/);
});

test("精确绑定不覆盖同目标的其他模型，整目标绑定覆盖全部", async () => {
  const {html} = await boot({
    targets: [target("t1", ["a", "b"])],
    groups: [group("g", [{target: "t1", model: " A "}])], // trim + 大小写不敏感
  });
  assert.doesNotMatch(html(), />a</i);
  assert.match(html(), /b/);
});

test("全部纳入管理时不显示提示", async () => {
  const {html} = await boot({
    targets: [target("t1", ["m1"])],
    groups: [group("g", [{target: "t1"}])],
  });
  assert.equal(html(), "");
});

test("已暂停的目标与已暂停的模型映射不计入", async () => {
  const {html} = await boot({
    targets: [
      target("off", ["m1"], {paused: true}),
      {name: "t1", endpoint_type: "openai", model_mappings: [{upstream: "live"}, {upstream: "stopped", paused: true}]},
    ],
    groups: [],
  });
  const out = html();
  assert.doesNotMatch(out, /off/);
  assert.match(out, /live/);
  assert.doesNotMatch(out, /stopped/);
});

test("模型组尚未加载成功时不提示，避免误报", async () => {
  const h = createHarness({routes: {
    "/admin/data/targets": {targets: [target("t1", ["m1"])]},
    "/admin/data/catalog": {models: []},
    "/admin/data/model-groups": {model_groups: []},
    "/admin/data/job-levels": {job_levels: []},
    "/admin/data/project-groups": {project_groups: []},
    "/admin/data/quota/summary": {clients: []},
  }});
  const app = await h.run();
  app.state.modelGroupsLoaded = false;
  app.state.targets = [target("t1", ["m1"])];
  app.modules.quotaGroups.renderModelGroups();
  assert.equal(h.ids.get("unmanagedModelsNotice").innerHTML, "");
});

test("名称与模型名经过转义", async () => {
  const {html} = await boot({targets: [target("t<b>", ["m<i>"])], groups: []});
  assert.doesNotMatch(html(), /<b>|<i>/);
  assert.match(html(), /&lt;b&gt;/);
});
