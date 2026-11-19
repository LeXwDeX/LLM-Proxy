"use strict";
// Issue 225: the targets/pools page consumes /admin/events snapshots through a
// single foreground-only EventSource, with /admin/metrics polling as fallback.
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness, schemaFixture } = require("../../../scripts/testing/admin_ui_test.cjs");
const { fakeClock } = require("./js/fake-clock.cjs");

const tgt = (name, extra = {}) => ({
  name, endpoint_type: "provider-new", endpoint: "https://x.invalid", ...extra,
});
const runtime = (name, extra = {}) => ({
  name, phase: "ready", in_flight: 0, max_concurrency: 0, utilization: 0,
  affinity_hits: 0, breaker_trips: 0,
  window_15m: { in_flight_peak: 0, affinity_hits: 0, breaker_trips: 0 },
  ...extra,
});
const reply = (value, status = 200) => ({
  ok: status >= 200 && status < 300,
  status,
  text: async () => JSON.stringify(value),
});
const snapshot = (runtimeList, shadow) => ({
  generated_at: "2026-11-19T10:00:00Z",
  targets_runtime: runtimeList,
  ...(shadow === undefined ? {} : { shadow_routing: shadow }),
});
const backendMetrics = (h) =>
  h.requests.filter((item) => item.url === "/admin/metrics");
const dispatchDoc = (h, type, event) =>
  (h.documentListeners[type] || []).forEach((fn) => fn(event || {}));

// Records every EventSource the page creates so tests can drive its lifecycle.
function fakeEventSources(h) {
  const instances = [];
  class FakeEventSource {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.listeners = {};
      this.closed = false;
      instances.push(this);
    }
    addEventListener(type, fn) {
      (this.listeners[type] = this.listeners[type] || []).push(fn);
    }
    close() {
      this.closed = true;
      this.readyState = 2;
    }
    fire(type, event) {
      (this.listeners[type] || []).forEach((fn) => fn(event || {}));
    }
    open() {
      this.readyState = 1;
      this.fire("open");
    }
    emit(data) {
      this.open();
      this.fire("snapshot", { data: JSON.stringify(data) });
    }
    fail(readyState) {
      this.readyState = readyState;
      this.fire("error");
    }
  }
  h.context.EventSource = FakeEventSource;
  return instances;
}

async function setup({ role = "admin", installEvents = true, fetch, targets, pools, metrics } = {}) {
  const clock = fakeClock();
  const h = createHarness({
    role,
    timers: clock,
    schema: schemaFixture(),
    routes: {
      "/admin/data/targets": { targets: targets || [tgt("alpha"), tgt("beta")] },
      "/admin/data/pools": { pools: pools || [{ name: "perf", routing_strategy: "load" }] },
    },
    metrics: metrics || { targets_runtime: [] },
    ...(fetch ? { fetch } : {}),
  });
  const instances = installEvents ? fakeEventSources(h) : [];
  const app = await h.run();
  return { h, app, clock, instances };
}
async function enterTargets(app) {
  app.state.currentPage = "targets";
  // loadTargets subscribes before its first await.
  await app.modules.targets.loadTargets();
}
async function enterPools(app) {
  app.state.currentPage = "targets";
  await app.modules.targets.switchTargetPageTab("pools");
}

test("a snapshot frame updates runtime, shadow routing and the runtime epoch", async () => {
  const { h, app, instances } = await setup();
  await enterTargets(app);
  assert.equal(instances.length, 1, "one EventSource per page");
  assert.equal(instances[0].url, "/admin/events");
  const epoch = app.modules.targets.shared.runtimeEpoch;
  instances[0].emit(
    snapshot(
      [runtime("alpha", { phase: "muted", window_15m: { in_flight_peak: 4, affinity_hits: 1, breaker_trips: 2 } })],
      [{ pool: "perf", agree: 1, disagree: 0, members: [] }],
    ),
  );
  assert.equal(app.state.runtime.alpha.phase, "muted");
  assert.equal(app.state.runtime.alpha.window_15m.in_flight_peak, 4);
  assert.equal(app.state.shadowRouting.length, 1);
  assert.ok(app.modules.targets.shared.runtimeEpoch > epoch, "the epoch advances on change");
  assert.match(h.ids.get("targetsTableBody").innerHTML, /冷却/, "the visible list repaints");
  const badge = h.ids.get("targetsRuntimeStatus");
  assert.equal(badge.hidden, false);
  assert.equal(badge.textContent, "实时更新");
});

test("an empty roster frame (targets_runtime: null) clears stale runtime", async () => {
  const { h, app, instances } = await setup();
  await enterTargets(app);
  instances[0].emit(snapshot([runtime("alpha", { phase: "muted" })]));
  assert.equal(app.state.runtime.alpha.phase, "muted");
  instances[0].emit({ generated_at: "2026-11-19T10:00:05Z", targets_runtime: null });
  assert.equal(Object.keys(app.state.runtime).length, 0, "a null roster means no runtime targets");
  assert.match(h.ids.get("targetsTableBody").innerHTML, /运行态不可用/);
});

test("a repeated frame is dropped without repainting or bumping the epoch", async () => {
  const { h, app, instances } = await setup();
  await enterTargets(app);
  const list = h.ids.get("targetsTableBody");
  instances[0].emit(snapshot([runtime("alpha", { phase: "muted" })]));
  const row = list.children[0];
  const epoch = app.modules.targets.shared.runtimeEpoch;
  instances[0].emit({
    generated_at: "2026-11-19T10:00:05Z",
    targets_runtime: [runtime("alpha", { phase: "muted" })],
  });
  assert.equal(list.children[0], row, "identical content keeps the rows in place");
  assert.equal(app.modules.targets.shared.runtimeEpoch, epoch, "no change, no epoch");
});

test("hiding the tab closes the stream and becoming visible reconnects", async () => {
  const { h, app, instances } = await setup();
  await enterTargets(app);
  instances[0].open();
  h.document.hidden = true;
  dispatchDoc(h, "visibilitychange");
  assert.equal(instances[0].closed, true);
  assert.equal(instances[0].readyState, 2);
  assert.equal(app.modules.targets.runtimeStreamStatus().mode, "idle");
  assert.equal(h.ids.get("targetsRuntimeStatus").hidden, true, "the badge hides with the stream");
  h.document.hidden = false;
  dispatchDoc(h, "visibilitychange");
  assert.equal(instances.length, 2, "a visible page builds a fresh stream");
  assert.equal(instances[1].url, "/admin/events");
  // pagehide is the same foreground-only rule; a bfcache restore reconnects.
  (h.windowListeners.pagehide || []).forEach((fn) => fn());
  assert.equal(instances[1].closed, true);
  (h.windowListeners.pageshow || []).forEach((fn) => fn());
  assert.equal(instances.length, 3);
});

test("leaving the targets page closes the stream; pointing at targets does not duplicate it", async () => {
  const { h, app, instances } = await setup();
  await enterTargets(app);
  h.context.location.hash = "#overview";
  app.state.currentPage = "overview";
  (h.windowListeners.hashchange || []).forEach((fn) => fn());
  assert.equal(instances[0].closed, true);
  const navClick = (page) => ({
    target: {
      closest: (selector) => (selector === ".sidebar-nav-item" ? { dataset: { page } } : null),
      contains: () => false,
    },
  });
  app.state.currentPage = "targets";
  h.context.location.hash = "#targets";
  dispatchDoc(h, "click", navClick("targets"));
  assert.equal(instances.length, 2, "the targets nav entry resubscribes");
  dispatchDoc(h, "click", navClick("targets"));
  assert.equal(instances.length, 2, "a second click reuses the open stream");
  dispatchDoc(h, "click", navClick("clients"));
  assert.equal(instances[1].closed, true, "another page tears the stream down");
});

test("a CONNECTING error lets EventSource retry instead of opening a second connection", async () => {
  const { h, app, clock, instances } = await setup();
  await enterTargets(app);
  const metricsBefore = backendMetrics(h).length;
  instances[0].fail(0);
  assert.equal(app.modules.targets.runtimeStreamStatus().mode, "disconnected");
  await clock.advance(60000);
  assert.equal(instances.length, 1, "the browser's own reconnect is left alone");
  assert.equal(backendMetrics(h).length, metricsBefore, "a transient error does not start polling");
});

test("a CLOSED stream after a 401 probe stops reconnecting and polling", async () => {
  const clock = fakeClock();
  let unauthorized = false;
  const h = createHarness({
    timers: clock,
    schema: schemaFixture(),
    routes: {
      "/admin/data/targets": { targets: [tgt("alpha")] },
      "/admin/data/pools": { pools: [{ name: "perf", routing_strategy: "load" }] },
    },
    metrics: { targets_runtime: [] },
    fetch: (url, options, fixture) =>
      url === "/admin/api/me" && unauthorized
        ? reply({ error: "unauthorized" }, 401)
        : fixture(url, options),
  });
  const instances = fakeEventSources(h);
  const app = await h.run();
  await enterTargets(app);
  unauthorized = true;
  instances[0].fail(2);
  await clock.flush();
  assert.equal(app.modules.targets.runtimeStreamStatus().mode, "stopped");
  assert.equal(instances[0].closed, true);
  const metrics = backendMetrics(h).length;
  await clock.advance(120000);
  assert.equal(instances.length, 1, "no reconnect after the session expired");
  assert.equal(backendMetrics(h).length, metrics, "no fallback polling either");
  assert.equal(h.ids.get("targetsRuntimeStatus").textContent, "实时更新已断开（会话需重新登录）");
});

test("a fatal stream failure polls /admin/metrics and retries SSE with a backoff", async () => {
  let probeFails = false;
  const { h, app, clock, instances } = await setup({
    fetch: (url, options, fixture) =>
      url === "/admin/api/me" && probeFails
        ? reply({ error: "probe failed" }, 500)
        : fixture(url, options),
  });
  await enterTargets(app);
  probeFails = true;
  instances[0].fail(2);
  await clock.advance(0);
  const polls = backendMetrics(h);
  const fallback = polls[polls.length - 1];
  assert.equal(fallback.options.headers["X-Admin-Background"], "1", "fallback polls are background");
  assert.equal(app.modules.targets.runtimeStreamStatus().polling, true);
  assert.equal(instances.length, 1);
  await clock.advance(5000);
  assert.equal(instances.length, 2, "SSE is retried after the first backoff step");
});

test("without EventSource the page polls every 30s only while visible", async () => {
  const { h, app, clock } = await setup({ installEvents: false });
  await enterTargets(app);
  await clock.advance(0);
  let polls = backendMetrics(h);
  const baseline = polls.length;
  assert.ok(baseline >= 2, "loadTargets plus the first fallback poll");
  assert.equal(polls[polls.length - 1].options.headers["X-Admin-Background"], "1");
  assert.equal(app.modules.targets.runtimeStreamStatus().mode, "polling");
  await clock.advance(30000);
  polls = backendMetrics(h);
  assert.equal(polls.length, baseline + 1, "one poll per 30s");
  h.document.hidden = true;
  dispatchDoc(h, "visibilitychange");
  const hiddenCount = polls.length;
  await clock.advance(90000);
  assert.equal(backendMetrics(h).length, hiddenCount, "a hidden tab does not poll");
  h.document.hidden = false;
  dispatchDoc(h, "visibilitychange");
  await clock.advance(0);
  assert.ok(backendMetrics(h).length > hiddenCount, "becoming visible resumes polling");
});

test("an open editor defers the repaint until the next frame after it closes", async () => {
  const { h, app, instances } = await setup();
  await enterTargets(app);
  instances[0].emit(snapshot([runtime("alpha", { phase: "ready" })]));
  const list = h.ids.get("targetsTableBody");
  const row = list.children[0];
  h.ids.get("targetModal").classList.add("open");
  instances[0].emit(snapshot([runtime("alpha", { phase: "muted" })]));
  assert.equal(app.state.runtime.alpha.phase, "muted", "state still follows the frame");
  assert.equal(list.children[0], row, "the open editor keeps its DOM");
  h.ids.get("targetModal").classList.remove("open");
  // The identical frame is enough: the deferred render is still pending.
  instances[0].emit(snapshot([runtime("alpha", { phase: "muted" })]));
  assert.notEqual(list.children[0], row, "closing the editor releases the repaint");
  assert.match(list.innerHTML, /冷却/);
});

test("pools keep their HTML when a frame changes nothing and skip an open cost preview", async () => {
  const { h, app, instances } = await setup({
    targets: [tgt("alpha", { pool: "perf" })],
    pools: [{ name: "perf", routing_strategy: "cost" }],
  });
  await enterPools(app);
  const body = h.ids.get("poolsTableBody");
  const row = body.children[0];
  app.modules.pools.renderPools();
  assert.equal(body.children[0], row, "identical pool markup is not rewritten");
  const click = (target) => (body.listeners.click || []).forEach((fn) => fn({ target }));
  click({ dataset: { costPreviewPool: "perf" } });
  assert.match(body.innerHTML, /poolCostModel/, "the cost preview opened");
  const withPreview = body.children[0];
  instances[0].emit(snapshot([runtime("alpha", { in_flight: 7 })]));
  assert.equal(app.state.runtime.alpha.in_flight, 7, "the frame is applied");
  assert.equal(body.children[0], withPreview, "the open preview is not rebuilt");
});

test("a read-only role still consumes snapshots and shows the live badge", async () => {
  const { h, app, instances } = await setup({ role: "readonly" });
  await enterTargets(app);
  assert.equal(app.modules.users.isReadonlyMode(), true);
  instances[0].emit(snapshot([runtime("alpha", { phase: "muted" })]));
  assert.equal(app.state.runtime.alpha.phase, "muted");
  assert.equal(h.ids.get("targetsRuntimeStatus").textContent, "实时更新");
});