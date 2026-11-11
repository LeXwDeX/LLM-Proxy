"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness, schemaFixture } = require("../../../../scripts/testing/admin_ui_test.cjs");

const MUTE_REASONS = [
  { value: "upstream_401", label: "401 冷却" },
  { value: "upstream_402", label: "402 冷却" },
  { value: "upstream_403", label: "403 冷却" },
];
const MIN = 60 * 1000;
const click = (dataset) => ({ target: { dataset, disabled: false, closest: () => null } });
const dispatch = (element, event) => (element.listeners.click || []).forEach((fn) => fn(event));
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function setup({ targets = [], runtime = [], schema = schemaFixture(), beforeLoad } = {}) {
  const h = createHarness({
    schema,
    routes: { "/admin/data/targets": { targets }, "/admin/data/pools": { pools: [] } },
    metrics: { targets_runtime: runtime },
  });
  const app = await h.run();
  if (beforeLoad) beforeLoad(h, app);
  await app.modules.targets.loadTargets();
  return { h, app };
}
const withReasons = () => ({ ...schemaFixture(), target_mute_reasons: MUTE_REASONS });
const tgt = (name, extra = {}) => ({ name, endpoint_type: "provider-new", endpoint: "https://x.invalid", ...extra });

test("formatMuteRemaining: <1 分, minutes up to 60, then hours", async () => {
  const { app } = await setup();
  const f = app.modules.targetsHealth.formatMuteRemaining;
  assert.equal(f(-5 * MIN), "<1 分");
  assert.equal(f(0), "<1 分");
  assert.equal(f(59 * 1000), "<1 分");
  assert.equal(f(MIN), "1 分");
  assert.equal(f(42 * MIN + 59 * 1000), "42 分");
  assert.equal(f(60 * MIN), "60 分");
  assert.equal(f(65 * MIN), "1 时 5 分");
  assert.equal(f(120 * MIN), "2 时");
  assert.equal(f(NaN), "<1 分");
});

test("targetMuteSummary builds the label from the schema and the title from status, code and deadline", async () => {
  const { app } = await setup({ schema: withReasons() });
  const summary = app.modules.targetsHealth.targetMuteSummary;
  const now = Date.parse("2026-01-01T00:00:00Z");
  const until = new Date(now + 42 * MIN + 10000);
  const full = summary({
    phase: "muted", mute_reason: "upstream_403", mute_code: "insufficient_quota", mute_status: 403,
    muted_until: until.toISOString(),
  }, now);
  assert.equal(full.text, "403 冷却 · 剩 42 分");
  assert.match(full.title, /^上游返回 403（insufficient_quota），\d{4}-\d\d-\d\d \d\d:\d\d:\d\d 后自动探测恢复$/);
  // No code: the bracket part disappears.
  const noCode = summary({ phase: "muted", mute_reason: "upstream_401", mute_status: 401, muted_until: until.toISOString() }, now);
  assert.match(noCode.title, /^上游返回 401，/);
  assert.doesNotMatch(noCode.title, /（/);
  // No deadline: no remaining time and no recovery sentence.
  const noUntil = summary({ phase: "muted", mute_reason: "upstream_402", mute_status: 402 }, now);
  assert.equal(noUntil.text, "402 冷却");
  assert.equal(noUntil.title, "上游返回 402");
  // Ordinary breaker cooldown and other phases are not rejection cooldowns.
  assert.equal(summary({ phase: "muted", muted_until: until.toISOString() }, now), null);
  assert.equal(summary({ phase: "ready", mute_reason: "upstream_403" }, now), null);
  assert.equal(summary(null, now), null);
});

test("an older schema without target_mute_reasons falls back to the generic label", async () => {
  const { app } = await setup(); // fixture schema carries no target_mute_reasons
  const s = app.modules.targetsHealth.targetMuteSummary({ phase: "muted", mute_reason: "upstream_403", mute_status: 403 }, 0);
  assert.equal(s.text, "冷却");
});

test("default order: enabled above paused, then name (numeric), then abnormal, then endpoint", async () => {
  const targets = [
    tgt("b-paused", { paused: true }),
    tgt("node10"),
    tgt("node9"),
    tgt("a-paused", { paused: true }),
    tgt("dup", { endpoint: "https://z.invalid" }),
    tgt("dup", { endpoint: "https://a.invalid" }),
    tgt("Alpha"),
  ];
  const { app } = await setup({ targets, runtime: [{ name: "dup", phase: "ready" }] });
  const names = app.modules.targets.filteredTargets().map((t) => t.name + "|" + t.endpoint.slice(8, 9));
  assert.deepEqual(Array.from(names.slice(0, 3)), ["Alpha|x", "dup|a", "dup|z"]);
  assert.deepEqual(Array.from(names.slice(3, 5)), ["node9|x", "node10|x"]);
  assert.deepEqual(Array.from(names.slice(5)), ["a-paused|x", "b-paused|x"]);
});

test("abnormal predicate: rejection cooldown, muted and half-open count; ready does not", async () => {
  const { app } = await setup();
  const h = app.modules.targetsHealth;
  assert.equal(h.targetRuntimeAbnormal({ phase: "half-open" }), true);
  assert.equal(h.targetRuntimeAbnormal({ phase: "muted" }), true);
  assert.equal(h.targetRuntimeAbnormal({ phase: "ready", mute_reason: "upstream_401" }), true);
  assert.equal(h.targetRuntimeAbnormal({ phase: "ready" }), false);
  assert.equal(h.targetRuntimeAbnormal(null), false);
  // Identical names (never expected) fall through to abnormal-first, then endpoint.
  app.state.runtime = { same: { phase: "ready" } };
  const a = { name: "same", endpoint: "https://b.invalid" };
  const b = { name: "same", endpoint: "https://a.invalid" };
  assert.equal(h.compareTargets(a, b) > 0, true, "endpoint breaks the tie");
});

test("the state filter still applies on top of the default order", async () => {
  const { app } = await setup({
    targets: [tgt("z-on"), tgt("a-off", { paused: true }), tgt("m-on")],
    runtime: [{ name: "z-on", phase: "ready" }, { name: "m-on", phase: "ready" }, { name: "a-off", phase: "ready" }],
  });
  app.modules.targets.setTargetFilter("paused");
  assert.deepEqual(Array.from(app.modules.targets.filteredTargets().map((t) => t.name)), ["a-off"]);
  app.modules.targets.setTargetFilter("all");
  assert.deepEqual(Array.from(app.modules.targets.filteredTargets().map((t) => t.name)), ["m-on", "z-on", "a-off"]);
});

test("list shows a bad-colour cooldown tag with the unmute button; an ordinary cooldown keeps its phase tag", async () => {
  const until = new Date(Date.now() + 42 * MIN + 30000).toISOString();
  const { h } = await setup({
    schema: withReasons(),
    targets: [tgt("rejected"), tgt("breaker")],
    runtime: [
      { name: "rejected", phase: "muted", mute_reason: "upstream_403", mute_code: "forbidden", mute_status: 403, muted_until: until },
      { name: "breaker", phase: "muted", muted_until: until },
    ],
  });
  const rows = h.ids.get("targetsTableBody").innerHTML.split("<tr ");
  const breaker = rows.find((r) => r.includes(">breaker<"));
  const rejected = rows.find((r) => r.includes(">rejected<"));
  assert.match(rejected, /403 冷却 · 剩 42 分/);
  assert.match(rejected, /title='上游返回 403（forbidden），/);
  assert.match(rejected, /var\(--bad\)/);
  assert.match(rejected, /class='btn btn-sm readonly-hide' data-unmute-tgt='rejected'/);
  assert.doesNotMatch(breaker, /data-unmute-tgt/);
  assert.doesNotMatch(breaker, /剩/);
  assert.match(breaker, />冷却</);
});

test("the target detail overview names the cooldown reason and remaining time", async () => {
  const until = new Date(Date.now() + 5 * MIN + 30000).toISOString();
  const { h, app } = await setup({
    schema: withReasons(),
    targets: [tgt("rejected")],
    runtime: [{ name: "rejected", phase: "muted", mute_reason: "upstream_402", mute_status: 402, muted_until: until }],
  });
  assert.equal(app.modules.targetsDetail.openTargetDetail("rejected"), true);
  const html = h.ids.get("targetDetailOverview").innerHTML;
  assert.match(html, /402 冷却 · 剩 5 分/);
  assert.match(html, /上游返回 402/);
});

test("unmute button: confirm, POST without If-Match, success toast and list refresh", async () => {
  const until = new Date(Date.now() + 10 * MIN).toISOString();
  const { h, app } = await setup({
    schema: withReasons(),
    targets: [tgt("rejected")],
    runtime: [{ name: "rejected", phase: "muted", mute_reason: "upstream_403", mute_status: 403, muted_until: until }],
  });
  const messages = [];
  app.modules.core.toast = (msg, kind) => messages.push({ msg, kind });
  let confirm;
  app.modules.core.showConfirm = (msg, cb) => { confirm = { msg, cb }; };
  dispatch(h.ids.get("targetsTableBody"), click({ unmuteTgt: "rejected" }));
  assert.ok(confirm && /解除.*冷却/.test(confirm.msg), "asks for confirmation first");
  h.requests.length = 0;
  h.setMetrics({ targets_runtime: [{ name: "rejected", phase: "ready" }] });
  await confirm.cb();
  const post = h.requests.find((r) => r.url === "/admin/data/targets/rejected/unmute");
  assert.ok(post, "unmute endpoint called");
  assert.equal(post.options.method, "POST");
  assert.ok(!post.options.headers || !post.options.headers["If-Match"], "no If-Match for a runtime-only action");
  assert.ok(h.requests.some((r) => r.url === "/admin/data/targets"), "list refreshed");
  assert.deepEqual(messages.map((m) => m.kind), ["success"]);
  assert.doesNotMatch(h.ids.get("targetsTableBody").innerHTML, /data-unmute-tgt/);
});

test("unmute failure shows an error toast and does not refresh", async () => {
  const until = new Date(Date.now() + 10 * MIN).toISOString();
  const { h, app } = await setup({
    targets: [tgt("rejected")],
    runtime: [{ name: "rejected", phase: "muted", mute_reason: "upstream_403", muted_until: until }],
  });
  const messages = [];
  app.modules.core.toast = (msg, kind) => messages.push({ msg, kind });
  let confirm;
  app.modules.core.showConfirm = (msg, cb) => { confirm = { msg, cb }; };
  h.context.fetch = async (url) => ({
    ok: false, status: 404, text: async () => JSON.stringify({ error: 'target "rejected" not found' }),
  });
  dispatch(h.ids.get("targetsTableBody"), click({ unmuteTgt: "rejected" }));
  await confirm.cb();
  assert.deepEqual(messages.map((m) => m.kind), ["error"]);
  assert.match(messages[0].msg, /解除冷却失败/);
});

test("countdown timer: runs only while a rejection cooldown is shown, repaints without requests, stops on leaving the page", async () => {
  const until = new Date(Date.now() + 10 * MIN).toISOString();
  const intervals = [];
  const cleared = [];
  const { h, app } = await setup({
    targets: [tgt("rejected")],
    runtime: [{ name: "rejected", phase: "muted", mute_reason: "upstream_403", mute_status: 403, muted_until: until }],
    beforeLoad: (harness, application) => {
      harness.context.setInterval = (fn, ms) => { intervals.push({ fn, ms }); return intervals.length; };
      harness.context.clearInterval = (id) => { cleared.push(id); };
      application.state.currentPage = "targets";
    },
  });
  app.modules.targets.renderTargets();
  assert.equal(intervals.length, 1, "one timer, not one per render");
  assert.equal(intervals[0].ms, 30000);
  h.requests.length = 0;
  intervals[0].fn();
  assert.deepEqual(h.requests, [], "a tick sends no request");
  // Leaving the page: the next tick clears the timer.
  app.state.currentPage = "overview";
  intervals[0].fn();
  assert.deepEqual(cleared, [1]);
  // Coming back with the cooldown gone: no new timer.
  app.state.currentPage = "targets";
  app.state.runtime = { rejected: { phase: "ready" } };
  app.modules.targets.renderTargets();
  assert.equal(intervals.length, 1);
});
