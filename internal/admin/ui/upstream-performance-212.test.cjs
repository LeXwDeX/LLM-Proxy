"use strict";
// Issue 212 阶段 T3a：管理端只渲染 /admin/metrics 下发的 model_performance 与
// shadow_routing。缺失、空数组或旧版本字段时保持原布局，不发额外请求。
// Issue 227：所有目标相关 hint（模型 chip、状态/性能标签、池成员）共用一个
// body 挂载的 #targetHint，0 延迟显示、150ms 渐入、优先在触发器上方。
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
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
function targetHint(h) {
  return h.document.body.children.find((el) => el.id === "targetHint");
}
function nodeWith(root, attr, value) {
  return root.children.find((el) => el.getAttribute(attr) === value);
}
function fire(target, type, event) {
  for (const listener of target.listeners[type] || []) listener(event);
}

test("target list marks perf badges for the structured hint and drops the native title", async () => {
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
  const alphaBadge = alpha.match(/<span style='[^']*'[^>]*>Schema正常<\/span>/)[0];
  assert.doesNotMatch(alphaBadge, / title=/, "the perf badge no longer carries a native title");
  assert.match(alphaBadge, /data-perf-target='alpha'/);
  assert.match(alphaBadge, /tabindex='0'/);
  assert.match(alphaBadge, /cursor:help/);
  assert.doesNotMatch(alpha, /target-perf-summary|target-perf-chip|pool-member-perf/, "no separate performance row or chip");
  assert.doesNotMatch(alpha.replace(/<[^>]+>/g, ""), /TTFT|首字|样本/, "the visible row keeps no performance numbers");

  const beta = rowFor(html, "beta");
  assert.match(beta, /data-perf-target='beta'/, "a sampleless model still gets the hint trigger");
  assert.doesNotMatch(beta.replace(/<[^>]+>/g, ""), /TTFT|样本/, "the visible row keeps no performance numbers");

  assert.doesNotMatch(rowFor(html, "plain"), /data-perf-target|tabindex='0'|cursor:help|target-perf-summary|target-perf-chip/, "a target without samples keeps the old layout");
});

test("one #targetHint serves perf badges: same-tick hover, top placement, structured card", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha")],
    runtimeList: [
      runtime("alpha", {
        model_performance: [
          perfModel("model-a"),
          perfModel("model-b", {
            ttft: { p50: 900, p90: 1000, samples: 2, insufficient: true },
            tps: { p50: 20, p90: 30, samples: 2, insufficient: true },
            insufficient: true,
          }),
          perfModel("model-c", { non_stream: { samples: 0, avg_ms: 0, avg_output_tokens: 0 } }),
          perfModel("model-d"),
        ],
      }),
    ],
  });
  await app.modules.targets.loadTargets();
  const tbody = h.ids.get("targetsTableBody");
  const trigger = nodeWith(tbody, "data-perf-target", "alpha");
  const hint = targetHint(h);
  assert.ok(trigger, "the perf badge exposes data-perf-target");
  assert.ok(hint, "only the shared #targetHint is mounted on body");
  assert.equal(
    h.document.body.children.filter((el) => el.getAttribute && el.getAttribute("role") === "tooltip").length,
    1,
    "a single tooltip component exists",
  );
  assert.equal(h.document.body.children.some((el) => el.id === "targetModelHint"), false, "the old model hint is gone");
  assert.equal(h.document.body.children.some((el) => el.id === "targetPerfHint"), false, "no second perf overlay exists");
  assert.equal(typeof app.modules.targets.showHint, "function", "targets exports showHint(trigger, html)");
  assert.equal(typeof app.modules.targets.hideHint, "function", "targets exports hideHint()");
  assert.equal(app.modules.targets.showTargetHint, undefined, "no second show API");
  assert.equal(app.modules.targets.hideTargetHint, undefined, "no second hide API");
  assert.equal(hint.hidden, true, "the hint starts hidden");
  assert.equal(hint.getAttribute("role"), "tooltip");
  assert.ok(hint.getAttribute("class").includes("target-hint"));

  trigger.getBoundingClientRect = () => ({ left: 300, top: 700, width: 60, height: 20 });
  hint.getBoundingClientRect = () => ({ left: 0, top: 0, width: 400, height: 120 });
  fire(tbody, "mouseover", { target: trigger });
  assert.equal(hint.hidden, false, "mouseover shows the hint in the same tick");
  assert.equal(trigger.getAttribute("aria-describedby"), "targetHint");
  assert.equal(hint.getAttribute("data-placement"), "top", "the hint prefers the space above the trigger");
  assert.equal(hint.style.top, 700 - 120 - 10 + "px", "the card sits just above the trigger");
  for (const part of [
    "上游性能估计", "近 5 分钟 · 真实流量", "模型", "首字 p50·p90",
    "吞吐 p50·p90", "样本 TTFT/TPS", "状态", "target-hint-arrow",
  ]) {
    assert.ok(hint.innerHTML.includes(part), `the hint renders ${part}`);
  }
  assert.match(hint.innerHTML, /model-a/);
  assert.match(hint.innerHTML, /model-b/);
  assert.match(hint.innerHTML, /model-c/);
  assert.doesNotMatch(hint.innerHTML, /model-d/, "only three model rows fit");
  assert.match(hint.innerHTML, /还有 1 个模型，可在目标详情查看/);
  assert.match(hint.innerHTML, /1\.2s · 2\.5s/, "TTFT p50 · p90");
  assert.match(hint.innerHTML, /45 · 70/, "TPS p50 · p90");
  assert.match(hint.innerHTML, /32 \/ 32/, "TTFT/TPS samples");
  assert.match(hint.innerHTML, /样本不足/);
  assert.match(hint.innerHTML, /惩罚样本 2/);
  assert.match(hint.innerHTML, /非流式 3 次 · 均值 2\.0s \/ 100 tokens/);
  assert.match(hint.innerHTML, /更新 /);

  trigger.getBoundingClientRect = () => ({ left: 300, top: 5, width: 60, height: 20 });
  fire(tbody, "focusin", { target: trigger });
  assert.equal(hint.getAttribute("data-placement"), "bottom", "below is used only without room above");

  fire(tbody, "mouseout", { target: trigger, relatedTarget: null });
  assert.equal(hint.hidden, true, "mouseout closes the hint");
  assert.equal(trigger.getAttribute("aria-describedby"), null, "closing drops aria-describedby");

  fire(tbody, "focusin", { target: trigger });
  assert.equal(hint.hidden, false, "keyboard focus opens the hint");
  fire(tbody, "focusout", { target: trigger });
  assert.equal(hint.hidden, true, "focusout closes the hint");

  fire(tbody, "focusin", { target: trigger });
  for (const listener of h.windowListeners.scroll || []) listener({});
  assert.equal(hint.hidden, true, "scroll closes the hint");

  fire(tbody, "click", { target: trigger, pointerType: "touch" });
  assert.equal(hint.hidden, false, "a touch tap opens the hint in the same tick");
  fire(tbody, "click", { target: trigger, pointerType: "touch" });
  assert.equal(hint.hidden, true, "a second tap closes it");

  fire(tbody, "focusin", { target: trigger });
  for (const listener of h.windowListeners.resize || []) listener({});
  assert.equal(hint.hidden, true, "resize closes the hint");

  fire(tbody, "focusin", { target: trigger });
  assert.equal(hint.hidden, false);
  await app.modules.targetsList.switchTargetPageTab("pools");
  assert.equal(hint.hidden, true, "switching page tabs closes the hint");
});

test("the shared hint fades in over 150ms", () => {
  const css = fs.readFileSync(path.join(__dirname, "css", "admin.css"), "utf8");
  assert.match(
    css,
    /\.target-hint \{[^}]*transition:opacity 150ms ease, transform 150ms ease/,
    "the show transition is exactly 150ms",
  );
  assert.match(css, /\.target-hint\[data-placement="top"\] \.target-hint-arrow[^}]*border-top:6px solid #333/, "arrow points down when above");
});

test("the performance table wraps cells instead of letting wide values overlap", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha")],
    runtimeList: [
      runtime("alpha", {
        model_performance: [
          perfModel("model-a", { ttft: { p50: 1200, p90: 2500, samples: 643 }, tps: { p50: 300, p90: 400, samples: 636 } }),
        ],
      }),
    ],
  });
  await app.modules.targets.loadTargets();
  const tbody = h.ids.get("targetsTableBody");
  fire(tbody, "mouseover", { target: nodeWith(tbody, "data-perf-target", "alpha") });
  const hint = targetHint(h);

  const css = fs.readFileSync(path.join(__dirname, "css", "admin.css"), "utf8");
  // A table cell ignores overflow/text-overflow, so clipping the cell itself
  // still paints past the fixed column: the name needs a block child.
  const cellRule = /\.target-hint-table th, \.target-hint-table td \{([^}]*)\}/.exec(css);
  assert.ok(cellRule, "the shared cell rule exists");
  assert.doesNotMatch(cellRule[1], /white-space:\s*nowrap/, "cells wrap rather than overflow their column");
  const clipRule = /\.target-hint-model-name \{([^}]*)\}/.exec(css);
  assert.ok(clipRule, "the model name is clipped by its own block element");
  assert.match(clipRule[1], /overflow:\s*hidden/);
  assert.match(clipRule[1], /text-overflow:\s*ellipsis/);

  assert.match(
    hint.innerHTML,
    /<td class='target-hint-model'><span class='target-hint-model-name'>model-a<\/span><\/td>/,
    "the long model name renders inside the ellipsized block",
  );
  assert.match(hint.innerHTML, /300 · 400/, "TPS p50 · p90 without the repeated unit");
  assert.match(hint.innerHTML, /吞吐 p50·p90 \(tok\/s\)/, "the throughput unit is stated once in the header");
  assert.match(hint.innerHTML, /1\.2s · 2\.5s/, "TTFT keeps its own unit");
});

test("model chips in the list and detail use the same hint and keep no native title", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha", {
      model_mappings: [{ upstream: "up-model-a", fallback: "alias-a" }],
    })],
    runtimeList: [runtime("alpha")],
  });
  await app.modules.targets.loadTargets();
  const hint = targetHint(h);
  const tbody = h.ids.get("targetsTableBody");
  const listBadge = tbody.children.find((el) => el.classList.contains("target-model-badge"));
  assert.ok(listBadge, "the list renders a model chip");
  assert.equal(listBadge.getAttribute("title"), null, "list chips keep no native title");
  fire(tbody, "mouseover", { target: listBadge });
  assert.equal(hint.hidden, false, "the list chip hint shows in the same tick");
  assert.match(hint.innerHTML, /target-hint-text/);
  assert.match(hint.innerHTML, /up-model-a \(alias-a\)/);
  assert.equal(listBadge.getAttribute("aria-describedby"), "targetHint");
  fire(tbody, "mouseout", { target: listBadge, relatedTarget: null });
  assert.equal(hint.hidden, true);

  assert.equal(app.modules.targetsDetail.openTargetDetail("alpha"), true);
  app.modules.targetsDetail.switchTargetDetailTab("models");
  const detail = h.ids.get("targetDetailModels");
  const detailBadge = detail.children.find((el) => el.classList.contains("target-model-badge"));
  assert.ok(detailBadge, "the detail renders a model chip");
  assert.equal(detailBadge.getAttribute("title"), null, "detail chips drop the native title");
  fire(detail, "mouseover", { target: detailBadge });
  assert.equal(hint.hidden, false, "the detail chip uses the same #targetHint");
  assert.match(hint.innerHTML, /up-model-a \(alias-a\)/);
});

test("a load pool member chip opens the shared hint with status, performance and shadow", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha", { pool: "perf" }), tgt("beta", { pool: "perf" })],
    pools: [{ name: "perf", routing_strategy: "load" }],
    runtimeList: [
      runtime("alpha", {
        in_flight: 2,
        max_concurrency: 8,
        utilization: 0.25,
        affinity_hits: 4,
        breaker_trips: 1,
        model_performance: [perfModel("model-a")],
      }),
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
  const body = h.ids.get("poolsTableBody");
  const html = body.innerHTML;
  assert.match(html, /pool-shadow-summary[^>]*>影子一致 7 \/ 不一致 3</);
  assert.doesNotMatch(html, /pool-member-perf/, "performance text is no longer resident on the chip");
  assert.doesNotMatch(html.replace(/<[^>]+>/g, ""), /TTFT/, "the visible chip text carries no performance numbers");
  const chip = nodeWith(body, "data-pool-target", "alpha");
  assert.ok(chip, "the member chip names its target for the hint");
  assert.equal(chip.getAttribute("title"), null, "pool chips drop the native title");

  const hint = targetHint(h);
  fire(body, "mouseover", { target: chip });
  assert.equal(hint.hidden, false, "pool chip hover opens the shared hint immediately");
  assert.match(hint.innerHTML, /target-hint-title/);
  assert.match(hint.innerHTML, />alpha</);
  assert.match(hint.innerHTML, /状态 Schema正常/);
  assert.match(hint.innerHTML, /在途 2\/8（25%）/);
  assert.match(hint.innerHTML, /亲和命中 4/);
  assert.match(hint.innerHTML, /熔断 1/);
  assert.match(hint.innerHTML, /上游性能估计/);
  assert.match(hint.innerHTML, /首字 p50·p90/);
  assert.match(hint.innerHTML, /model-a/);
  assert.match(hint.innerHTML, /影子选中 7（70%）/);
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
  const body = h.ids.get("poolsTableBody");
  const html = body.innerHTML;
  assert.doesNotMatch(html, /pool-shadow-summary/);
  assert.doesNotMatch(html, /影子选中/);
  assert.doesNotMatch(html, /pool-member-perf/);
  const chip = nodeWith(body, "data-pool-target", "alpha");
  assert.equal(chip.getAttribute("title"), null, "cost pool chips drop the native title too");
  const hint = targetHint(h);
  fire(body, "mouseover", { target: chip });
  assert.equal(hint.hidden, false, "cost pool chip hover opens the hint");
  assert.match(hint.innerHTML, /上游性能估计/);
  assert.match(hint.innerHTML, /TTFT|首字/);
  assert.match(hint.innerHTML, /模型 /, "cost pools keep the model mapping line");
  assert.doesNotMatch(hint.innerHTML, /影子选中/, "cost pools never render shadow statistics");
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
  const body = h.ids.get("poolsTableBody");
  assert.doesNotMatch(body.innerHTML, /pool-shadow-summary/);
  assert.doesNotMatch(body.innerHTML.replace(/<[^>]+>/g, ""), /TTFT|样本不足/, "the visible chip text stays performance-free");
  const hint = targetHint(h);
  const chip = nodeWith(body, "data-pool-target", "alpha");
  fire(body, "mouseover", { target: chip });
  assert.equal(hint.hidden, false, "malformed entries still open the shared hint");
  assert.match(hint.innerHTML, /ok/, "the usable model name survives");
  assert.match(hint.innerHTML, /样本不足/);
  assert.match(hint.innerHTML, /—/, "missing percentiles render as a dash");
  fire(body, "mouseout", { target: chip, relatedTarget: null });

  app.modules.targets.renderTargets();
  const tbody = h.ids.get("targetsTableBody");
  const rows = tbody.innerHTML;
  assert.doesNotMatch(rows, /target-perf-summary|target-perf-chip/, "a sampleless model adds no performance row");
  assert.doesNotMatch(rows, /title='[^']*TTFT/, "no performance breakdown in a native title");
  assert.doesNotMatch(rows.replace(/<[^>]+>/g, ""), /TTFT|样本不足/, "no performance text is visible");
  const trigger = nodeWith(tbody, "data-perf-target", "alpha");
  fire(tbody, "mouseover", { target: trigger });
  assert.equal(hint.hidden, false, "malformed entries still open the shared hint");
  assert.match(hint.innerHTML, /ok/, "the usable model name survives");
  assert.match(hint.innerHTML, /样本不足/);
});

test("metrics without model_performance render no summary or detail rows", async () => {
  const { h, app } = await setup({
    targets: [tgt("alpha"), tgt("empty")],
    runtimeList: [runtime("alpha"), runtime("empty", { model_performance: [] })],
  });
  await app.modules.targets.loadTargets();
  assert.doesNotMatch(h.ids.get("targetsTableBody").innerHTML, /target-perf-summary|target-perf-chip|cursor:help|data-perf-target/);
  const hint = targetHint(h);
  fire(h.ids.get("targetsTableBody"), "mouseover", {
    target: { closest: () => null, getAttribute: () => null },
  });
  assert.equal(hint.hidden, true, "no trigger means no hint");
  assert.equal(app.modules.targetsDetail.openTargetDetail("alpha"), true);
  assert.match(h.ids.get("targetDetailOverview").innerHTML, /暂无性能样本/);
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
  assert.equal(summary.insufficient, false);
  assert.match(summary.title, /^aardvark[\s\S]*\ntiny[\s\S]*\nzeta/, "the title is ordered by upstream model");
  assert.match(summary.title, /aardvark · TTFT p50 800ms \/ p90 900ms/);
  assert.match(summary.title, /tiny[\s\S]*样本不足/, "insufficient models stay marked in the title");

  const cold = app.modules.targets.performanceSummary({
    model_performance: [
      perfModel("cold", {
        ttft: { p50: 0, p90: 0, samples: 0, insufficient: true },
        tps: { p50: 0, p90: 0, samples: 0, insufficient: true },
        insufficient: true,
      }),
    ],
  });
  assert.equal(cold.insufficient, true);
  assert.match(cold.title, /样本不足$/, "the representative warning ends the title");
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