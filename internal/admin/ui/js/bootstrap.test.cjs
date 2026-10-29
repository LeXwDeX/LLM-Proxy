"use strict";
// Startup resilience: a hung or dropped transport must never leave the admin
// page on "加载中…" forever (Issue #54). Uses the real scripts in HTML order.
const test = require("node:test");
const assert = require("node:assert/strict");
const {createHarness, schemaFixture} = require("../../../../scripts/testing/admin_ui_test.cjs");
const {fakeClock} = require("./fake-clock.cjs");

const hang = () => new Promise(() => {});
const SCHEMA = "/admin/data/schema";

function boot({fetch, schema, hash = "", omitScripts, withHistory = false} = {}) {
  const clock = fakeClock();
  const assigned = [];
  const location = {pathname: "/admin", search: "", hash, assign: (url) => assigned.push(url)};
  const urlCalls = [];
  const record = (kind) => (state, title, url) => {
    urlCalls.push([kind, url]);
    location.hash = url.includes("#") ? url.slice(url.indexOf("#")) : "";
  };
  const history = {replaceState: record("replace"), pushState: record("push")};
  const harness = createHarness({
    fetch,
    schema,
    timers: clock,
    omitScripts,
    history: withHistory ? history : undefined,
    location,
  });
  const app = harness.start();
  const count = (url) => harness.requests.filter((request) => request.url === url).length;
  return {app, clock, harness, assigned, count, location, urlCalls, ids: harness.ids};
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

const ACTIVITY = "/admin/api/activity";
const ME = "/admin/api/me";

test("overview and the trace probe start without waiting for /api/me; the layout stays inert until the role is known", async () => {
  const {app, clock, ids, count, harness} = boot({
    fetch: (url, options, fixture) => (url === ME ? hang() : fixture(url, options)),
  });
  await clock.advance(100);
  assert.ok(count(ACTIVITY) >= 1, "overview loads while /api/me is pending");
  assert.ok(count("/admin/data/trace?limit=1") >= 1, "trace probe runs in parallel with /api/me");
  assert.equal(app.state.ready, false);
  assert.equal(harness.document.querySelector(".layout").inert, true, "no control is reachable before the role is known");
  assert.equal(harness.document.body.classList.contains("readonly-mode"), true, "readonly until proven admin");
  assert.equal(ids.get("adminBootRetry").hidden, true);
});

test("a hung trace probe does not delay ready", async () => {
  const {app, clock} = boot({
    fetch: (url, options, fixture) => (url === "/admin/data/trace?limit=1" ? hang() : fixture(url, options)),
  });
  await clock.advance(100);
  assert.equal(app.state.ready, true);
});

test("the initial overview load is requested once", async () => {
  const {app, clock, count} = boot();
  await clock.advance(100);
  await app.bootPromise;
  assert.equal(count(ACTIVITY), 1);
});

test("a boot failure after initialize stops the already-started overview refresh", async () => {
  const {app, clock, ids} = boot({
    fetch: (url, options, fixture) =>
      url === ME ? Promise.resolve({ok: false, status: 500, text: async () => JSON.stringify({error: "me down"})}) : fixture(url, options),
  });
  await clock.advance(100);
  assert.equal(app.state.ready, false);
  assert.match(ids.get("adminBootMessage").textContent, /me down/);
  assert.equal(ids.get("adminBootRetry").hidden, false);
});

test("a module whose script failed to load is named in the boot banner", async () => {
  const {app, clock, ids} = boot({omitScripts: ["pages/users/users.js"]});
  await clock.advance(100);
  assert.equal(app.state.ready, false);
  const text = ids.get("adminBootMessage").textContent;
  assert.match(text, /users/);
  assert.match(text, /未加载|加载失败/);
  assert.doesNotMatch(text, /Cannot read|undefined/);
});

test("another missing module is reported by name", async () => {
  const {app, clock, ids} = boot({omitScripts: ["pages/quota/quota.js"]});
  await clock.advance(100);
  assert.equal(app.state.ready, false);
  assert.match(ids.get("adminBootMessage").textContent, /quota/);
});

test("a #page deep link opens that page after boot", async () => {
  const {app, clock} = boot({hash: "#clients"});
  await clock.advance(100);
  await app.bootPromise;
  assert.equal(app.state.currentPage, "clients");
});

test("a deep link is deferred until the role is known so the page renders with admin state", async () => {
  let releaseMe;
  const meGate = new Promise((resolve) => (releaseMe = resolve));
  const readonlyAtTargets = [];
  const {app, clock, count, harness} = boot({
    hash: "#targets",
    fetch: async (url, options, fixture) => {
      if (url === "/admin/data/targets")
        readonlyAtTargets.push(harness.document.body.classList.contains("readonly-mode"));
      if (url === ME) await meGate;
      return fixture(url, options);
    },
  });
  await clock.advance(100);
  assert.equal(count("/admin/data/targets"), 0, "no page data is rendered while the role is unknown");
  assert.equal(app.state.currentPage, "overview");
  releaseMe();
  await clock.advance(100);
  await app.bootPromise;
  assert.equal(app.state.currentPage, "targets");
  assert.ok(count("/admin/data/targets") >= 1);
  assert.deepEqual([...new Set(readonlyAtTargets)], [false], "targets rendered after readonly-mode was lifted");
});

test("a boot retry reloads the current non-overview page instead of restarting the overview", async () => {
  let failMe = true;
  const {app, clock, count} = boot({
    hash: "#clients",
    fetch: (url, options, fixture) =>
      url === ME && failMe
        ? Promise.resolve({ok: false, status: 500, text: async () => JSON.stringify({error: "me down"})})
        : fixture(url, options),
  });
  await clock.advance(100);
  assert.equal(app.state.ready, false);
  app.state.currentPage = "clients"; // as if a previous boot had already switched
  const before = count(ACTIVITY);
  failMe = false;
  await app.boot();
  assert.equal(count(ACTIVITY), before, "overview refresh is not restarted behind another page");
  assert.ok(count("/admin/data/clients") >= 1, "the visible page is (re)loaded once the role is known");
});

test("the missing-module banner names the module and the script that should define it", async () => {
  const {app, clock, ids} = boot({omitScripts: ["pages/users/users.js"]});
  await clock.advance(100);
  const text = ids.get("adminBootMessage").textContent;
  assert.match(text, /页面资源加载失败/);
  assert.match(text, /users/);
  assert.match(text, /\/admin\/ui\/js\/pages\/users\/users\.js/);
  assert.equal(ids.get("adminBootStatus").getAttribute("data-boot-error"), "modules", "tells the watchdog to yield");
});

test("user navigation pushes history entries; boot and corrections replace", async () => {
  const {app, clock, harness, urlCalls} = boot({hash: "#clients", withHistory: true});
  await clock.advance(100);
  await app.bootPromise;
  assert.deepEqual(urlCalls, [], "the boot deep link already matches the address bar: no history write");
  const item = harness.document.querySelectorAll(".sidebar-nav-item").find((el) => el.dataset.page === "usage");
  item.listeners.click[0]();
  assert.deepEqual(urlCalls, [["push", "/admin#usage"]]);
  assert.equal(app.state.currentPage, "usage");
});

test("hashchange (Back or a hand-edited hash) switches pages within the whitelist", async () => {
  const {app, clock, harness, location, urlCalls} = boot({withHistory: true});
  await clock.advance(100);
  await app.bootPromise;
  const fire = (hash) => {
    location.hash = hash;
    (harness.windowListeners.hashchange || []).forEach((fn) => fn({}));
  };
  fire("#audit");
  assert.equal(app.state.currentPage, "audit");
  fire("#nonsense");
  assert.equal(app.state.currentPage, "audit", "unknown pages are ignored");
  assert.equal(location.hash, "#audit", "and the hash is corrected to the page shown");
  fire("");
  assert.equal(app.state.currentPage, "overview", "an empty hash means the overview");
  assert.equal(urlCalls.filter(([kind]) => kind === "push").length, 0, "hashchange itself never pushes");
});

test("a readonly user on #users keeps the overview and the hash is corrected", async () => {
  const hash = "#users";
  const clock = fakeClock();
  const location = {pathname: "/admin", search: "", hash, assign() {}};
  const history = {replaceState: (s, t, url) => (location.hash = url.slice(url.indexOf("#"))), pushState() {}};
  const harness = createHarness({role: "readonly", timers: clock, history, location});
  const app = await harness.run();
  await clock.advance(100);
  assert.equal(app.state.currentPage, "overview");
  assert.equal(location.hash, "#overview");
  (harness.windowListeners.hashchange || []).forEach((fn) => { location.hash = "#users"; fn({}); });
  assert.equal(app.state.currentPage, "overview", "admin-only pages are not reachable through the hash");
  assert.equal(location.hash, "#overview");
});

test("a #users deep link waits for the admin role; readonly users stay on overview", async () => {
  const admin = boot({hash: "#users"});
  await admin.clock.advance(100);
  await admin.app.bootPromise;
  assert.equal(admin.app.state.currentPage, "users");
  const ro = createHarness({role: "readonly", timers: fakeClock(), location: {pathname: "/admin", search: "", hash: "#users", assign() {}}});
  const roApp = await ro.run();
  assert.equal(roApp.state.currentPage, "overview");
});
