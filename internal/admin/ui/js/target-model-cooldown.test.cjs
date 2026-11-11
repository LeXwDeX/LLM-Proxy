"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness, schemaFixture } = require("../../../../scripts/testing/admin_ui_test.cjs");

const MIN = 60 * 1000;

async function setup({ targets = [], runtime = [], schema = schemaFixture() } = {}) {
  const h = createHarness({
    schema,
    routes: { "/admin/data/targets": { targets }, "/admin/data/pools": { pools: [] } },
    metrics: { targets_runtime: runtime },
  });
  const app = await h.run();
  await app.modules.targets.loadTargets();
  return { h, app };
}
const tgt = (name, extra = {}) => ({ name, endpoint_type: "provider-new", endpoint: "https://x.invalid", ...extra });
const cooldown = (model, minutes, extra = {}) => ({
  model, until: new Date(Date.now() + minutes * MIN + 20000).toISOString(), status: 429, reason: "upstream_429", ...extra,
});

test("targetModelCooldownSummary lists models with remaining time, drops expired ones, null when none", async () => {
  const { app } = await setup();
  const summary = app.modules.targetsHealth.targetModelCooldownSummary;
  const now = Date.now();
  assert.equal(summary({ phase: "ready" }, now), null);
  assert.equal(summary({ phase: "ready", model_cooldowns: [] }, now), null);
  assert.equal(summary({ phase: "ready", model_cooldowns: null }, now), null);
  const s = summary({
    phase: "ready",
    model_cooldowns: [cooldown("qwen4-max-preview", 3), { model: "gone", until: new Date(now - 1000).toISOString(), status: 429 }, { model: "", until: new Date(now + MIN).toISOString() }],
  }, now);
  assert.equal(s.text, "部分模型限流");
  assert.match(s.title, /qwen4-max-preview · 上游 429 .* · 剩 3 分/);
  assert.doesNotMatch(s.title, /gone/);
  // An older server without the schema labels still renders (generic reason label).
  assert.match(s.title, /其余模型照常调度/);
});

test("list shows the model-limit tag in the scheduling column while the phase stays ready", async () => {
  const { h } = await setup({
    targets: [tgt("bailian"), tgt("plain")],
    runtime: [
      { name: "bailian", phase: "ready", window_15m: { in_flight_peak: 1, affinity_hits: 2, breaker_trips: 0 }, model_cooldowns: [cooldown("qwen4-max-preview", 2)] },
      { name: "plain", phase: "ready", window_15m: { in_flight_peak: 0, affinity_hits: 0, breaker_trips: 0 }, model_cooldowns: [] },
    ],
  });
  const rows = h.ids.get("targetsTableBody").innerHTML.split("<tr ");
  const limited = rows.find((r) => r.includes(">bailian<"));
  const plain = rows.find((r) => r.includes(">plain<"));
  assert.match(limited, /target-model-limit-tag[^>]*title='qwen4-max-preview/);
  assert.match(limited, />部分模型限流</);
  assert.doesNotMatch(limited, /data-unmute-tgt/, "a model limit is not a target cooldown");
  assert.match(limited, />正常</, "phase label unchanged");
  assert.doesNotMatch(plain, /部分模型限流/);
});

test("target detail names the limited models", async () => {
  const { h, app } = await setup({
    targets: [tgt("bailian")],
    runtime: [{ name: "bailian", phase: "ready", model_cooldowns: [cooldown("qwen4-max-preview", 4)] }],
  });
  assert.equal(app.modules.targetsDetail.openTargetDetail("bailian"), true);
  const html = h.ids.get("targetDetailOverview").innerHTML;
  assert.match(html, /部分模型限流/);
  assert.match(html, /qwen4-max-preview/);
});

test("the countdown timer also runs for a model-level cooldown", async () => {
  const intervals = [];
  const { app } = await (async () => {
    const h = createHarness({
      schema: schemaFixture(),
      routes: { "/admin/data/targets": { targets: [tgt("bailian")] }, "/admin/data/pools": { pools: [] } },
      metrics: { targets_runtime: [{ name: "bailian", phase: "ready", model_cooldowns: [cooldown("m", 5)] }] },
    });
    const application = await h.run();
    h.context.setInterval = (fn, ms) => { intervals.push({ fn, ms }); return intervals.length; };
    h.context.clearInterval = () => {};
    application.state.currentPage = "targets";
    await application.modules.targets.loadTargets();
    return { h, app: application };
  })();
  app.modules.targets.renderTargets();
  assert.equal(intervals.length, 1);
});

test("key card lists the models a key is rate limited for, while the key stays usable", async () => {
  const until = new Date(Date.now() + 3 * MIN).toISOString();
  const expired = new Date(Date.now() - MIN).toISOString();
  const target = {
    name: "sample", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", usage_adapter: "standard",
    has_api_key: true, api_key: "aaaa...1111", api_keys: ["bbbb...2222"], etag: "t1-1", key_ids: ["kid-a", "kid-b"],
    key_runtime_indices: [0, 1],
    key_statuses: [
      { index: 0, key_mask: "aaaa...1111", blocked: false, exhausted: false, status: "使用中",
        model_limits: [{ model: "qwen4-max-preview", until }, { model: "gone-model", until: expired }] },
      { index: 1, key_mask: "bbbb...2222", blocked: false, exhausted: false, status: "等待中" },
    ],
  };
  const h = createHarness({
    schema: schemaFixture(),
    routes: { "/admin/data/targets": { targets: [target] }, "/admin/data/pools": { pools: [] } },
    metrics: { targets_runtime: [{ name: "sample", phase: "ready" }] },
  });
  const app = await h.run();
  await app.modules.targets.loadTargets();
  await app.modules.targets.openTargetModal(app.state.targets[0]);
  const cards = h.ids.get("keyCardList").innerHTML;
  assert.match(cards, /key-model-limit-tag/);
  assert.match(cards, /模型限流：qwen4-max-preview/);
  assert.match(cards, /qwen4-max-preview · 剩 3 分/);
  assert.doesNotMatch(cards, /gone-model/, "expired limits are dropped");
  assert.equal((cards.match(/key-model-limit-tag/g) || []).length, 1, "only the limited key shows the tag");
});
