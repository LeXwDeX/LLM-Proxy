"use strict";
// Issue 212 阶段 T3a：管理端只渲染 /admin/metrics 下发的 model_performance 与
// shadow_routing。缺失、空数组或旧版本字段时保持原布局，不发额外请求。
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness, schemaFixture } = require("../../../scripts/testing/admin_ui_test.cjs");

const tgt = (name, extra = {}) => ({
  name, endpoint_type: "provider-new", endpoint: "https://x.invalid", ...extra,
});
const runtime = (name, extra = {}) => ({
  name, phase: "ready",
  window_15m: { in_flight_peak: 0, affinity_hits: 0, breaker_trips: 0 },
  ...extra,
});
const perfModel = (model, overrides = {}) => {
  const ttft = overrides.ttft || { p50: 1200, p90: 2500, samples: 32, insufficient: false };
  const tps = overrides.tps || { p50: 45, p90: 70, samples: 32, insufficient: false };
  return {
    target: overrides.target || "alpha",
    upstream_model: model,
    ttft,
    tps,
    penalty_samples: overrides.penalty_samples === undefined ? 2 : overrides.penalty_samples,
    penalty_ttft: overrides.penalty_ttft || { p50: 5000, p90: 5500, samples: 2, insufficient: false },
    fast_cancel_samples: 1,
    insufficient: overrides.insufficient === undefined ? !!(ttft.insufficient || tps.insufficient) : overrides.insufficient,
    updated_at: overrides.updated_at || "2026-11-18T10:00:00Z",
    non_stream: overrides.non_stream || { samples: 3, avg_ms: 2000, avg_output_tokens: 100 },
  };
};
async function setup({ targets = [], pools = [], runtimeList = [], shadow, metrics } = {}) {
  const h = createHarness({
    schema: schemaFixture(),
    routes: { "/admin/data/targets": { targets }, "/admin/data/pools": { pools } },
    metrics: metrics || (shadow === undefined ? { targets_runtime: runtimeList } : { targets_runtime: runtimeList, shadow_routing: shadow }),
  });
  const app = await h.run();
  return { h, app };
}
function rowFor(html, name) {
  return html.split("<tr ").find((row) => row.includes(">" + name + "<"));
}

test("target list adds the representative model's compact performance line and title", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha"), tgt("beta"), tgt("plain")],
    runtimeList: [
      runtime("alpha", {
        model_performance: [
          perfModel("model-small", {
            ttft: { p50: 900, p90: 1000, samples: 2, insufficient: true },
            tps: { p50: 20, p90: 30, samples: 2, insufficient: true },
            insufficient: true,
          }),
          perfModel("model-big"),
        ],
      }),
      runtime("beta", {
        model_performance: [
          perfModel("cold-model", {
            ttft: { p50: 0, p90: 0, samples: 0, insufficient: true },
            tps: { p50: 0, p90: 0, samples: 0, insufficient: true },
            insufficient: true,
            non_stream: { samples: 0, avg_ms: 0, avg_output_tokens: 0 },
          }),
        ],
      }),
      runtime("plain"),
    ],
  });
  await app.modules.targets.loadTargets();
  const html = h.ids.get("targetsTableBody").innerHTML;
  const alpha = rowFor(html, "alpha");
  assert.match(alpha, /<div class='target-perf-summary'[^>]*>TTFT 1\.2s · 45 tok\/s · 32 样本<\/div>/);
  assert.match(alpha, /title='[^']*model-big[^']*model-small/, "the title lists every model");
  assert.match(alpha, /TTFT p50 1\.2s \/ p90 2\.5s/);
  assert.match(alpha, /TPS p50 45 tok\/s \/ p90 70 tok\/s/);
  assert.match(alpha, /惩罚样本 2/);
  assert.match(alpha, /非流式 3 次，均值 2\.0s \/ 100 tokens/);
  assert.match(alpha, /更新 /);
  assert.doesNotMatch(alpha, /样本不足<\/span>/, "the representative model has enough samples");

  const beta = rowFor(html, "beta");
  assert.match(beta, /TTFT — · — · 0 样本/);
  assert.match(beta, /样本不足<\/span>/);

  assert.doesNotMatch(rowFor(html, "plain"), /target-perf-summary/, "a target without samples keeps the old layout");
});

test("the representative model is the one with most samples, ties by upstream name", async () => {
  const { app } = await setup();
  const summary = app.modules.targets.performanceSummary({
    model_performance: [
      perfModel("zeta", { ttft: { p50: 3000, p90: 4000, samples: 32, insufficient: false }, tps: { p50: 10, p90: 20, samples: 32, insufficient: false } }),
      perfModel("aardvark", { ttft: { p50: 800, p90: 900, samples: 32, insufficient: false }, tps: { p50: 50, p90: 60, samples: 32, insufficient: false } }),
      perfModel("tiny", { ttft: { p50: 500, p90: 600, samples: 3, insufficient: true }, tps: { p50: 90, p90: 99, samples: 3, insufficient: true } }),
    ],
  });
  assert.match(summary.text, /^TTFT 800ms · 50 tok\/s · 32 样本$/);
  assert.doesNotMatch(summary.text, /tiny/);
  assert.match(summary.title, /aardvark[\s\S]*tiny[\s\S]*zeta/, "the title is ordered by upstream model");
});

test("target detail shows the per-model performance table and a note without samples", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha"), tgt("plain")],
    runtimeList: [
      runtime("alpha", {
        model_performance: [
          perfModel("model-big"),
          perfModel("model-small", {
            ttft: { p50: 900, p90: 1000, samples: 2, insufficient: true },
            tps: { p50: 20, p90: 30, samples: 2, insufficient: true },
            insufficient: true,
          }),
        ],
      }),
      runtime("plain"),
    ],
  });
  await app.modules.targets.loadTargets();
  assert.equal(app.modules.targetsDetail.openTargetDetail("alpha"), true);
  const detail = h.ids.get("targetDetailOverview").innerHTML;
  assert.match(detail, /target-perf-table/);
  assert.match(detail, /model-big/);
  assert.match(detail, /model-small/);
  assert.match(detail, /TTFT p50 \/ p90/);
  assert.match(detail, /TPS p50 \/ p90/);
  assert.match(detail, /样本 \(TTFT\/TPS\)/);
  assert.match(detail, /非流式/);
  assert.match(detail, /样本不足/);

  assert.equal(app.modules.targetsDetail.openTargetDetail("plain"), true);
  assert.match(h.ids.get("targetDetailOverview").innerHTML, /暂无性能样本/);
});

test("a load pool shows the shadow summary and per-member shadow selections", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha", { pool: "perf" }), tgt("beta", { pool: "perf" })],
    pools: [{ name: "perf", routing_strategy: "load" }],
    runtimeList: [
      runtime("alpha", { model_performance: [perfModel("model-a")] }),
      runtime("beta", { model_performance: [perfModel("model-b")] }),
    ],
    shadow: [
      {
        pool: "perf",
        agree: 7,
        disagree: 3,
        members: [
          { target: "alpha", shadow_selections: 7, share: 0.7 },
          { target: "beta", shadow_selections: 3, share: 0.3 },
        ],
      },
    ],
  });
  await app.modules.pools.loadPools();
  assert.equal(app.state.shadowRouting.length, 1, "loadPools publishes the shadow snapshot");
  const html = h.ids.get("poolsTableBody").innerHTML;
  assert.match(html, /pool-shadow-summary[^>]*>影子一致 7 \/ 不一致 3</);
  assert.match(html, /影子选中 7（70%）/);
  assert.match(html, /影子选中 3（30%）/);
  assert.match(html, /pool-member-perf/);
  assert.match(html, /TTFT 1\.2s · 45 tok\/s · 32 样本/);
});

test("a cost pool keeps chips-only model info and never shows shadow statistics", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha", { pool: "cheap" })],
    pools: [{ name: "cheap", routing_strategy: "cost" }],
    runtimeList: [runtime("alpha", { model_performance: [perfModel("model-a")] })],
    shadow: [
      { pool: "cheap", agree: 9, disagree: 1, members: [{ target: "alpha", shadow_selections: 9, share: 0.9 }] },
    ],
  });
  await app.modules.pools.loadPools();
  const html = h.ids.get("poolsTableBody").innerHTML;
  assert.doesNotMatch(html, /pool-shadow-summary/);
  assert.doesNotMatch(html, /影子选中/);
  assert.match(html, /pool-member-perf/, "member performance still shows");
});

test("missing, empty or malformed performance fields degrade to the old layout", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha", { pool: "perf" })],
    pools: [{ name: "perf", routing_strategy: "load" }],
    metrics: {
      targets_runtime: [
        runtime("alpha", {
          model_performance: [null, { upstream_model: "" }, { upstream_model: "ok", ttft: null, tps: null, non_stream: null, insufficient: true }],
        }),
      ],
    },
  });
  await app.modules.pools.loadPools();
  assert.equal(app.state.shadowRouting, null, "a server without shadow_routing publishes nothing");
  const html = h.ids.get("poolsTableBody").innerHTML;
  assert.doesNotMatch(html, /pool-shadow-summary/);
  assert.match(html, /TTFT — · — · 0 样本/, "malformed entries still render safely");
  assert.match(html, /样本不足/);

  app.modules.targets.renderTargets();
  const rows = h.ids.get("targetsTableBody").innerHTML;
  assert.match(rows, /TTFT — · — · 0 样本/);
});

test("metrics without model_performance render no summary or detail rows", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha")],
    runtimeList: [runtime("alpha")],
  });
  await app.modules.targets.loadTargets();
  assert.doesNotMatch(h.ids.get("targetsTableBody").innerHTML, /target-perf-summary/);
  assert.equal(app.modules.targetsDetail.openTargetDetail("alpha"), true);
  assert.match(h.ids.get("targetDetailOverview").innerHTML, /暂无性能样本/);
});