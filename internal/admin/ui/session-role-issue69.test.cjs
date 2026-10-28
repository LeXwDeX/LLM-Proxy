"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");
const { fakeClock } = require("./js/fake-clock.cjs");

const ME = "/admin/api/me";
const response = (value, status = 200) => ({
  ok: status >= 200 && status < 300, status,
  text: async () => typeof value === "string" ? value : JSON.stringify(value),
});

async function failedBoot(firstResponse) {
  const clock = fakeClock();
  let current = firstResponse;
  const harness = createHarness({timers: clock, fetch: (url, opts, fixture) =>
    url === ME ? current() : fixture(url, opts)});
  const app = harness.start();
  await clock.advance(80000);
  assert.equal(app.state.ready, false);
  assert.equal(harness.document.querySelector(".layout").inert, true,
    "the initialized page is not interactive until the role is known");
  assert.equal(app.modules.users.isReadonlyMode(), true);
  assert.equal(harness.ids.get("adminBootStatus").hidden, false);
  assert.equal(harness.ids.get("adminBootRetry").hidden, false);
  assert.notEqual(harness.ids.get("nav-users").style.display, "");
  assert.match(harness.ids.get("adminBootMessage").textContent, /加载失败/);
  return {app, clock, harness, setResponse: fn => {current = fn;}};
}

test("write controls stay hidden while the role request is pending", () => {
  const harness = createHarness({fetch: (url, opts, fixture) =>
    url === ME ? new Promise(() => {}) : fixture(url, opts)});
  harness.start();
  assert.equal(harness.document.body.classList.contains("readonly-mode"), true);
  assert.equal(harness.document.querySelector(".layout").inert, true);
});

test("an unrecognized role is not an administrator", async () => {
  await failedBoot(() => response({username: "alice", role: "future-role"}));
});

test("an unauthorized role request retains the login redirect and blocks editing", async () => {
  const clock = fakeClock();
  const assigned = [];
  const harness = createHarness({timers: clock,
    location: {pathname: "/admin/ui", search: "?tab=keys", hash: "", assign: url => assigned.push(url)},
    fetch: (url, opts, fixture) => url === ME ? response({error: "unauthorized"}, 401) : fixture(url, opts)});
  const app = harness.start();
  await clock.advance(80000);
  assert.equal(app.state.ready, false);
  assert.equal(harness.document.querySelector(".layout").inert, true);
  assert.equal(app.modules.users.isReadonlyMode(), true);
  assert.deepEqual(assigned, ["/login?next=%2Fadmin%2Fui%3Ftab%3Dkeys"]);
});

test("role HTTP failures and invalid bodies block editing and offer recovery", async () => {
  for (const bad of [
    () => response({error: "session unavailable"}, 503),
    () => response(null),
    () => response({username: "alice", role: "unknown"}),
    () => response({username: "alice"}),
    () => response({username: "", role: "admin"}),
    () => response({username: 42, role: "admin"}),
    () => response({username: "alice", role: "admin", authenticated: false}),
    () => response("not json"),
  ]) {
    const {app, clock, harness, setResponse} = await failedBoot(bad);
    assert.equal(harness.requests.filter(r => r.url === ME).length, 1,
      "deterministic HTTP and validation failures are not automatically retried");
    setResponse(() => response({username: "alice", role: "admin", authenticated: true}));
    harness.ids.get("adminBootRetry").listeners.click[0]();
    await clock.flush();
    await app.bootPromise;
    assert.equal(app.state.ready, true);
    assert.equal(harness.document.querySelector(".layout").inert, false);
    assert.equal(app.modules.users.isReadonlyMode(), false);
    assert.equal(harness.ids.get("adminBootStatus").hidden, true);
    assert.equal(harness.ids.get("nav-users").style.display, "");
  }
});

test("role timeout is bounded and a readonly recovery retains the password exception", async () => {
  const {app, clock, harness, setResponse} = await failedBoot(() => new Promise(() => {}));
  assert.equal(harness.requests.filter(r => r.url === ME).length, 3);
  assert.match(harness.ids.get("adminBootMessage").textContent, /超时/);
  setResponse(() => response({username: "reader", role: "readonly", authenticated: true}));
  harness.ids.get("adminBootRetry").listeners.click[0]();
  await clock.flush();
  await app.bootPromise;
  assert.equal(app.state.ready, true);
  assert.equal(harness.document.querySelector(".layout").inert, false);
  assert.equal(app.modules.users.isReadonlyMode(), true);
  assert.notEqual(harness.ids.get("nav-users").style.display, "");
  harness.ids.get("changePasswordBtn").listeners.click[0]();
  assert.equal(harness.ids.get("changePasswordModal").classList.contains("open"), true);
  harness.ids.get("cpOldPassword").value = "old-pass";
  harness.ids.get("cpNewPassword").value = "new-pass";
  harness.ids.get("cpConfirmPassword").value = "new-pass";
  await harness.ids.get("savePasswordBtn").listeners.click[0]();
  assert.ok(harness.requests.some(r => r.url === "/admin/api/change-password" && r.options.method === "POST"));
});
