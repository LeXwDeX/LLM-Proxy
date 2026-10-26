"use strict";
// Startup resilience: a hung or dropped transport must never leave the admin
// page on "加载中…" forever (Issue #54). Uses the real scripts in HTML order.
const test = require("node:test");
const assert = require("node:assert/strict");
const {createHarness, schemaFixture} = require("../../../../scripts/testing/admin_ui_test.cjs");
const {fakeClock} = require("./fake-clock.cjs");

const hang = () => new Promise(() => {});
const SCHEMA = "/admin/data/schema";

function boot({fetch, schema} = {}) {
  const clock = fakeClock();
  const assigned = [];
  const harness = createHarness({
    fetch,
    schema,
    timers: clock,
    location: {pathname: "/admin", search: "", hash: "", assign: (url) => assigned.push(url)},
  });
  const app = harness.start();
  const count = (url) => harness.requests.filter((request) => request.url === url).length;
  return {app, clock, harness, assigned, count, ids: harness.ids};
}

test("boot gives up with a usable retry button when every schema request hangs", async () => {
  let schemaHangs = true;
  const {app, clock, ids, count} = boot({
    fetch: (url, options, fixture) => (url === SCHEMA && schemaHangs ? hang() : fixture(url, options)),
  });
  await clock.advance(15000);
  assert.equal(count(SCHEMA), 1);
  assert.match(ids.get("adminBootMessage").textContent, /重试/, "retry progress is visible while waiting");
  await clock.advance(60000);
  assert.equal(app.state.ready, false);
  assert.equal(ids.get("adminBootStatus").hidden, false);
  assert.equal(ids.get("adminBootRetry").hidden, false, "manual retry is offered after bounded attempts");
  assert.match(ids.get("adminBootMessage").textContent, /超时/);
  assert.equal(count(SCHEMA), 3, "one request plus two automatic retries");

  schemaHangs = false;
  ids.get("adminBootRetry").listeners.click[0]();
  await clock.flush();
  await app.bootPromise;
  assert.equal(count(SCHEMA), 4, "manual retry is not blocked by a stale in-flight boot");
  assert.equal(app.state.ready, true);
  assert.equal(ids.get("adminBootStatus").hidden, true);
});

test("boot retries a timed-out schema request with a fresh fetch and becomes ready", async () => {
  let calls = 0;
  const {app, clock, ids, count, harness} = boot({
    fetch: (url, options, fixture) => (url === SCHEMA && ++calls === 1 ? hang() : fixture(url, options)),
  });
  await clock.advance(15000 + 5000);
  assert.equal(app.state.ready, true);
  assert.equal(ids.get("adminBootStatus").hidden, true);
  assert.equal(count(SCHEMA), 2);
  const schemaRequests = harness.requests.filter((request) => request.url === SCHEMA);
  assert.equal(schemaRequests[0].options.signal.aborted, true, "the hung attempt is aborted");
  assert.notEqual(schemaRequests[0].options.signal, schemaRequests[1].options.signal, "each attempt is a new fetch");
});

test("boot retries network failures (TypeError)", async () => {
  let calls = 0;
  const {app, clock, count} = boot({
    fetch: (url, options, fixture) => {
      if (url === SCHEMA && ++calls === 1) return Promise.reject(new TypeError("Failed to fetch"));
      return fixture(url, options);
    },
  });
  await clock.advance(5000);
  assert.equal(app.state.ready, true);
  assert.equal(count(SCHEMA), 2);
});

test("boot does not auto-retry deterministic schema failures", async () => {
  const invalid = schemaFixture();
  delete invalid.usage_adapters;
  const {app, clock, ids, count} = boot({schema: invalid});
  await clock.advance(60000);
  assert.equal(app.state.ready, false);
  assert.equal(count(SCHEMA), 1, "validation errors are not retried");
  assert.equal(ids.get("adminBootRetry").hidden, false);
  assert.match(ids.get("adminBootMessage").textContent, /usage_adapters/);

  const offline = boot({
    fetch: (url, options, fixture) =>
      url === SCHEMA
        ? Promise.resolve({ok: false, status: 503, text: async () => JSON.stringify({error: "schema offline"})})
        : fixture(url, options),
  });
  await offline.clock.advance(60000);
  assert.equal(offline.count(SCHEMA), 1, "HTTP errors are not retried");
  assert.equal(offline.ids.get("adminBootRetry").hidden, false);
});

test("an expired session during boot navigates to /login once", async () => {
  const {app, clock, count, assigned} = boot({
    fetch: () => Promise.resolve({ok: false, status: 401, text: async () => JSON.stringify({error: "unauthorized"})}),
  });
  await clock.advance(60000);
  assert.equal(app.state.ready, false);
  assert.deepEqual(assigned, ["/login?next=%2Fadmin"]);
  assert.equal(count(SCHEMA), 1, "401 is not retried");
});

test("a hung /admin/api/me cannot stall boot forever", async () => {
  const {app, clock, ids, count} = boot({
    fetch: (url, options, fixture) => (url === "/admin/api/me" ? hang() : fixture(url, options)),
  });
  await clock.advance(80000);
  assert.equal(count("/admin/api/me"), 3, "session info is retried a bounded number of times");
  assert.equal(app.state.ready, false, "editing stays disabled while the role is unknown");
  assert.equal(ids.get("adminBootRetry").hidden, false);
  assert.match(ids.get("adminBootMessage").textContent, /超时/);
});

test("a timed-out /admin/api/me is retried and the role still applies", async () => {
  let calls = 0;
  const {app, clock, count} = boot({
    fetch: (url, options, fixture) => {
      if (url !== "/admin/api/me") return fixture(url, options);
      if (++calls === 1) return hang();
      return Promise.resolve({ok: true, status: 200, text: async () => JSON.stringify({username: "ro", role: "readonly"})});
    },
  });
  await clock.advance(15000 + 5000);
  assert.equal(count("/admin/api/me"), 2);
  assert.equal(app.state.ready, true);
  assert.equal(app.modules.users.isReadonlyMode(), true, "readonly role from the retried request is enforced");
});

test("a hung trace probe only disables the optional trace page", async () => {
  const TRACE = "/admin/data/trace?limit=1";
  const {app, clock, ids, count} = boot({
    fetch: (url, options, fixture) => (url === TRACE ? hang() : fixture(url, options)),
  });
  await clock.advance(15000 * 3 + 5000);
  assert.equal(count(TRACE), 3);
  assert.equal(app.state.ready, true, "trace is optional and does not block startup");
  assert.equal(ids.get("adminBootStatus").hidden, true);
  assert.notEqual(ids.get("nav-trace").style.display, "", "trace entry stays hidden");
});
