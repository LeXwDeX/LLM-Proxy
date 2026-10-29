"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
const {fakeClock} = require("./fake-clock.cjs");

function loadCore({fetch, clock, location} = {}) {
  function node() {
    return {
      classList: {toggle() {}, contains() { return false; }},
      addEventListener() {},
      setAttribute() {},
      appendChild() {},
      remove() {},
    };
  }
  const document = {
    documentElement: node(),
    getElementById: id => (id === "toastContainer" || id === "confirmOkBtn" ? node() : null),
    querySelectorAll: () => [],
    createElement: () => node(),
  };
  const sandbox = {
    document,
    localStorage: {getItem: () => null, setItem() {}},
    setTimeout: clock ? clock.setTimeout : () => 0,
    clearTimeout: clock ? clock.clearTimeout : () => {},
    AbortController,
    fetch,
    location: location || {},
    console,
  };
  sandbox.window = sandbox;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "core", "core.js"), "utf8"), sandbox);
  return sandbox.AdminApp.modules.core;
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
