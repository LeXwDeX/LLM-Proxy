"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const { coreSource } = require("../../../../scripts/testing/admin_ui_scripts.cjs");
const path = require("node:path");
const {fakeClock} = require("./fake-clock.cjs");

function loadCoreEnv({fetch, clock, location, openOverlays = [], intervals} = {}) {
  function node(tag) {
    const listeners = {};
    return {
      tagName: tag, children: [], attrs: {}, listeners, style: {}, textContent: "",
      classList: {toggle() {}, contains() { return false; }, add() {}, remove() {}},
      addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
      setAttribute(k, v) { this.attrs[k] = v; },
      appendChild(child) { this.children.push(child); return child; },
      remove() { this.removed = true; },
    };
  }
  const toastContainer = node("div");
  const body = node("body");
  const byId = {toastContainer, confirmOkBtn: node("button")};
  const dirtyForms = [];
  const document = {
    body,
    documentElement: node("html"),
    hidden: false,
    getElementById: id => byId[id] || body.children.find(c => c.id === id && !c.removed) || null,
    querySelectorAll: sel => (/modal-overlay\.open/.test(sel) ? openOverlays : /data-dirty/.test(sel) ? dirtyForms : []),
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
    createElement: tag => node(tag),
    addEventListener(type, fn) { (document.listeners[type] = document.listeners[type] || []).push(fn); },
    listeners: {},
  };
  const winListeners = {};
  const sandbox = {
    document,
    localStorage: {getItem: () => null, setItem() {}},
    setTimeout: clock ? clock.setTimeout : () => 0,
    clearTimeout: clock ? clock.clearTimeout : () => {},
    setInterval: (fn, ms) => { (intervals || []).push({fn, ms}); return 1; },
    clearInterval() {},
    addEventListener(type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); },
    AbortController,
    fetch,
    location: location || {},
    console: {log() {}, warn() {}, error() {}, info() {}},
  };
  sandbox.window = sandbox;
  vm.runInNewContext(coreSource("core/core.js"), sandbox);
  return {core: sandbox.AdminApp.modules.core, sandbox, document, body, toastContainer, winListeners, openOverlays, dirtyForms};
}

function loadCore(options) {
  return loadCoreEnv(options).core;
}

function settle(promise) {
  const state = {done: false, value: undefined, error: undefined};
  promise.then(
    (value) => Object.assign(state, {done: true, value}),
    (error) => Object.assign(state, {done: true, error}),
  );
  return state;
}

function jsonResponse(status, body) {
  return {ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body)};
}

function withTZ(zone, fn) {
  const previous = process.env.TZ;
  process.env.TZ = zone;
  const restore = () => { if (previous === undefined) delete process.env.TZ; else process.env.TZ = previous; };
  let result;
  try { result = fn(); } catch (error) { restore(); throw error; }
  if (result && typeof result.then === "function") return result.finally(restore);
  restore();
  return result;
}

// A transport that never answers: models a keep-alive socket silently dropped by a middlebox.
function hangingFetch(calls) {
  return (url, init) => {
    calls.push({url, init});
    return new Promise(() => {});
  };
}

test("totalTok matches the backend activity contract and counts tool-use prompt tokens", () => {
  const core = loadCore();
  // internal/admin/handlers_activity.go: input = input + cached + cache_creation + tool_use_prompt.
  assert.equal(core.totalTok({
    input_tokens: 2,
    output_tokens: 3,
    cached_tokens: 5,
    cache_creation_tokens: 7,
    tool_use_prompt_tokens: 11,
  }), 28);
  assert.equal(core.totalTok({input_tokens: 2, output_tokens: 3, tool_use_prompt_tokens: 4}), 9);
  assert.equal(core.totalTok({}), 0);
});

test("core offsetMinutes reports negative and half-hour offsets east of UTC", async () => {
  await withTZ("America/New_York", async () => {
    const core = loadCore();
    assert.equal(core.offsetMinutes(new Date("2026-10-30T12:00:00Z")), -240, "EDT is four hours behind UTC");
  });
  await withTZ("Asia/Kolkata", async () => {
    const core = loadCore();
    assert.equal(core.offsetMinutes(new Date("2026-10-30T12:00:00Z")), 330, "IST is five and a half hours ahead of UTC");
  });
});

test("api times out after the 15s default when fetch never settles", async () => {
  const clock = fakeClock();
  const calls = [];
  const core = loadCore({fetch: hangingFetch(calls), clock});
  const result = settle(core.api("/admin/data/schema"));
  await clock.advance(14999);
  assert.equal(result.done, false, "still waiting before the default deadline");
  await clock.advance(1);
  assert.equal(result.done, true, "default deadline settles the request");
  assert.equal(result.error.name, "TimeoutError");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].init.signal.aborted, true, "the real fetch is aborted on timeout");
  assert.equal(calls[0].init.timeoutMs, undefined, "timeoutMs is not forwarded to fetch");
  assert.equal(clock.pending(), 0, "no timer survives the request");
});

test("api timeoutMs overrides the default and 0 disables the deadline", async () => {
  const clock = fakeClock();
  const core = loadCore({fetch: hangingFetch([]), clock});
  const short = settle(core.api("/a", {timeoutMs: 50}));
  await clock.advance(50);
  assert.equal(short.error && short.error.name, "TimeoutError");
  const unbounded = settle(core.api("/b", {timeoutMs: 0}));
  await clock.advance(10 * 60 * 1000);
  assert.equal(unbounded.done, false, "timeoutMs: 0 waits for the caller to finish");
  assert.equal(clock.pending(), 0, "no deadline timer is armed");
});

test("api deadline also covers reading the response body", async () => {
  const clock = fakeClock();
  let bodySignal = null;
  const core = loadCore({
    clock,
    fetch: async (url, init) => {
      bodySignal = init.signal;
      return {ok: true, status: 200, text: () => new Promise(() => {})};
    },
  });
  const result = settle(core.api("/admin/api/me", {timeoutMs: 100}));
  await clock.advance(100);
  assert.equal(result.error && result.error.name, "TimeoutError");
  assert.equal(bodySignal.aborted, true);
});

test("api caller signal aborts with AbortError semantics", async () => {
  const clock = fakeClock();
  const calls = [];
  const core = loadCore({fetch: hangingFetch(calls), clock});
  const controller = new AbortController();
  const result = settle(core.api("/admin/api/activity", {signal: controller.signal, timeoutMs: 12000}));
  await clock.flush();
  assert.notEqual(calls[0].init.signal, undefined);
  controller.abort();
  await clock.flush();
  assert.equal(result.done, true, "caller abort settles even if the transport ignores it");
  assert.equal(result.error.name, "AbortError");
  assert.equal(calls[0].init.signal.aborted, true, "caller abort reaches the real fetch");
  assert.equal(clock.pending(), 0, "deadline timer is cleared after abort");

  const pre = new AbortController();
  pre.abort();
  const early = settle(core.api("/x", {signal: pre.signal}));
  await clock.flush();
  assert.equal(early.error && early.error.name, "AbortError", "already-aborted signal rejects immediately");
});

test("api keeps successful JSON and HTTP error semantics", async () => {
  const clock = fakeClock();
  const core = loadCore({
    clock,
    fetch: async (url) => (url === "/ok" ? jsonResponse(200, {value: 1}) : jsonResponse(503, {error: "offline"})),
  });
  assert.deepEqual(JSON.parse(JSON.stringify(await core.api("/ok"))), {value: 1});
  await assert.rejects(core.api("/bad"), /offline/);
  assert.equal(clock.pending(), 0);
});

test("api redirects to /login once when the session is gone", async () => {
  const clock = fakeClock();
  const assigned = [];
  const core = loadCore({
    clock,
    location: {pathname: "/admin/", search: "?tab=keys", hash: "#targets", assign: (url) => assigned.push(url)},
    fetch: async () => jsonResponse(401, {error: "unauthorized"}),
  });
  await assert.rejects(core.api("/admin/api/activity"), /unauthorized/);
  await assert.rejects(core.api("/admin/data/targets"), /unauthorized/);
  assert.deepEqual(
    assigned,
    ["/login?next=" + encodeURIComponent("/admin/?tab=keys#targets")],
    "concurrent pollers trigger a single navigation back to the current page",
  );
});

test("api does not redirect for other auth failures or status codes", async () => {
  const clock = fakeClock();
  const assigned = [];
  const responses = {
    "/admin/api/change-password": jsonResponse(403, {error: "原密码验证失败"}),
    "/admin/api/me": jsonResponse(401, {error: "not authenticated"}),
    "/admin/data/users": jsonResponse(403, {error: "unauthorized"}),
    "/admin/data/targets": jsonResponse(500, {error: "unauthorized"}),
  };
  const core = loadCore({
    clock,
    location: {assign: (url) => assigned.push(url)},
    fetch: async (url) => responses[url],
  });
  for (const url of Object.keys(responses)) await assert.rejects(core.api(url));
  assert.deepEqual(assigned, []);
});

const banner = (env, id) => env.body.children.find(c => c.id === id && !c.removed);
const unauthorized = () => jsonResponse(401, {error: "unauthorized"});

test("401 with an open modal keeps the page: banner + SessionExpiredError, no navigation", async () => {
  const assigned = [];
  const env = loadCoreEnv({
    clock: fakeClock(),
    location: {pathname: "/admin", search: "", hash: "#targets", assign: (u) => assigned.push(u)},
    fetch: async () => unauthorized(),
    openOverlays: [{id: "targetModal"}],
  });
  let error;
  try { await env.core.api("/admin/data/targets", {method: "PUT", body: {name: "x"}}); } catch (e) { error = e; }
  assert.ok(error, "the call rejects");
  assert.equal(error.name, "SessionExpiredError");
  assert.match(error.message, /会话已过期/);
  assert.deepEqual(assigned, [], "draft pages must not navigate away");
  const b = banner(env, "sessionExpiredBanner");
  assert.ok(b, "persistent banner is shown");
  assert.match(b.textContent || b.children.map(c => c.textContent).join(""), /会话已过期，请在新标签页登录后重试/);
  const link = b.children.find(c => c.tagName === "a");
  assert.equal(link.href, "/login?next=/admin");
  assert.equal(link.target, "_blank");
  assert.equal(link.textContent, "打开登录页");
  await assert.rejects(env.core.api("/admin/data/targets"), (e) => e.name === "SessionExpiredError");
  assert.equal(env.body.children.filter(c => c.id === "sessionExpiredBanner").length, 1, "banner is not duplicated");
});

test("401 with a dirty form also keeps the page", async () => {
  const assigned = [];
  const env = loadCoreEnv({clock: fakeClock(), location: {assign: (u) => assigned.push(u)}, fetch: async () => unauthorized()});
  env.core.setFormDirty("clientForm", true);
  await assert.rejects(env.core.api("/admin/data/clients"), (e) => e.name === "SessionExpiredError");
  assert.deepEqual(assigned, []);
  env.core.setFormDirty("clientForm", false);
  await assert.rejects(env.core.api("/admin/data/clients"), /unauthorized/);
  assert.equal(assigned.length, 1, "once the form is clean the redirect applies");
});

test("a successful call after re-login removes the session banner", async () => {
  let ok = false;
  const env = loadCoreEnv({
    clock: fakeClock(), location: {assign() {}}, openOverlays: [{}],
    fetch: async () => (ok ? jsonResponse(200, {a: 1}) : unauthorized()),
  });
  await assert.rejects(env.core.api("/admin/data/x"));
  assert.ok(banner(env, "sessionExpiredBanner"));
  ok = true;
  await env.core.api("/admin/data/x");
  assert.equal(banner(env, "sessionExpiredBanner"), undefined);
});

test("login redirect lock resets when navigation does not happen", async () => {
  const clock = fakeClock();
  const assigned = [];
  const core = loadCore({clock, location: {pathname: "/admin", assign: (u) => assigned.push(u)}, fetch: async () => unauthorized()});
  await assert.rejects(core.api("/admin/data/a"));
  await assert.rejects(core.api("/admin/data/b"));
  assert.equal(assigned.length, 1);
  await clock.advance(6000);
  await assert.rejects(core.api("/admin/data/c"));
  assert.equal(assigned.length, 2, "a later 401 can redirect again after the lock expired");
});

test("background option sets X-Admin-Background and is not forwarded to fetch", async () => {
  const calls = [];
  const core = loadCore({clock: fakeClock(), fetch: async (url, init) => { calls.push(init); return jsonResponse(200, {}); }});
  await core.api("/admin/api/me", {background: true});
  await core.api("/admin/api/me");
  assert.equal(calls[0].headers["X-Admin-Background"], "1");
  assert.equal("background" in calls[0], false);
  assert.equal(calls[1].headers["X-Admin-Background"], undefined);
});

test("only network failures and timeouts are transient; programming TypeErrors are not", async () => {
  const clock = fakeClock();
  const core = loadCore({clock, fetch: () => Promise.reject(new TypeError("Failed to fetch"))});
  let network;
  try { await core.api("/admin/api/me"); } catch (e) { network = e; }
  assert.equal(core.isTransientError(network), true, "fetch rejection is a network failure");
  assert.equal(core.isTransientError(new TypeError("x is not a function")), false);
  assert.equal(core.isTransientError(new Error("请求失败 (HTTP 500)")), false);
  const hung = loadCore({clock, fetch: () => new Promise(() => {})});
  const result = settle(hung.api("/admin/api/me", {timeoutMs: 100}));
  await clock.advance(100);
  assert.equal(hung.isTransientError(result.error), true);
});

test("a stale server version raises a non-blocking reload banner", async () => {
  let version = "20261029.1";
  const env = loadCoreEnv({clock: fakeClock(), location: {reload() {}}, fetch: async () => jsonResponse(200, {username: "u", role: "admin", version})});
  await env.core.api("/admin/api/me");
  assert.equal(banner(env, "adminStaleBanner"), undefined, "first sight only records the version");
  await env.core.api("/admin/api/me");
  assert.equal(banner(env, "adminStaleBanner"), undefined, "same version -> no banner");
  version = "20261029.2";
  await env.core.api("/admin/api/me");
  const b = banner(env, "adminStaleBanner");
  assert.ok(b, "version change shows the banner");
  assert.match(b.textContent || b.children.map(c => c.textContent).join(""), /后台已更新，请刷新页面/);
  assert.ok(b.children.some(c => c.tagName === "button"), "reload button");
  assert.equal(env.body.children.filter(c => c.id === "adminStaleBanner").length, 1);
});

test("version watch polls /api/me in the background on an interval and on becoming visible", async () => {
  const intervals = [];
  const calls = [];
  const env = loadCoreEnv({clock: fakeClock(), intervals, fetch: async (url, init) => { calls.push({url, init}); return jsonResponse(200, {version: "v1"}); }});
  env.core.startVersionWatch();
  assert.equal(intervals.length, 1);
  assert.ok(intervals[0].ms >= 60000);
  await intervals[0].fn();
  assert.equal(calls[0].url, "/admin/api/me");
  assert.equal(calls[0].init.headers["X-Admin-Background"], "1");
  env.document.hidden = true;
  env.document.listeners.visibilitychange.forEach(fn => fn());
  assert.equal(calls.length, 1, "hidden tabs do not poll");
  env.document.hidden = false;
  env.document.listeners.visibilitychange.forEach(fn => fn());
  assert.equal(calls.length, 2);
});

test("global error handlers log and toast with throttling, ignoring expired-session rejections", async () => {
  const clock = fakeClock();
  const env = loadCoreEnv({clock, fetch: async () => jsonResponse(200, {})});
  const toasts = () => env.toastContainer.children.length;
  env.winListeners.error.forEach(fn => fn({message: "boom", error: new Error("boom")}));
  env.winListeners.error.forEach(fn => fn({message: "boom2", error: new Error("boom2")}));
  assert.equal(toasts(), 1, "bursts are throttled to one toast");
  await clock.advance(6000);
  env.winListeners.unhandledrejection.forEach(fn => fn({reason: new Error("nope")}));
  assert.equal(toasts(), 2);
  await clock.advance(6000);
  const expired = new Error("x"); expired.name = "SessionExpiredError";
  env.winListeners.unhandledrejection.forEach(fn => fn({reason: expired}));
  assert.equal(toasts(), 2, "the banner already explains an expired session");
});

test("a background request that gets 401 never navigates: banner + SessionExpiredError", async () => {
  const assigned = [];
  const env = loadCoreEnv({clock: fakeClock(), location: {pathname: "/admin", assign: (u) => assigned.push(u)}, fetch: async () => unauthorized()});
  await assert.rejects(env.core.api("/admin/api/me", {background: true}), (e) => e.name === "SessionExpiredError");
  assert.deepEqual(assigned, [], "an inline-form editor must not be bounced by a poll");
  assert.ok(banner(env, "sessionExpiredBanner"));
  await assert.rejects(env.core.api("/admin/data/x"), /unauthorized/);
  assert.equal(assigned.length, 1, "foreground calls without unsaved UI still redirect");
});

test("global handlers ignore benign ResizeObserver noise and script-load failures owned by the boot watchdog", () => {
  const env = loadCoreEnv({clock: fakeClock(), fetch: async () => jsonResponse(200, {})});
  const fire = (event) => env.winListeners.error.forEach(fn => fn(event));
  fire({message: "ResizeObserver loop completed with undelivered notifications."});
  fire({message: "ResizeObserver loop limit exceeded"});
  fire({message: "Script error", filename: "http://h/admin/ui/js/pages/users/users.js?v=1-ab", error: Object.assign(new Error("x"), {name: "SyntaxError"})});
  fire({message: "Unexpected token '<'", filename: "http://h/admin/ui/js/core/schema.js"});
  assert.equal(env.toastContainer.children.length, 0);
  fire({message: "boom", error: new Error("boom")});
  assert.equal(env.toastContainer.children.length, 1, "real errors are still reported");
});
