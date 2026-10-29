"use strict";
// Users page regressions: change-password in-flight guard and the toggle using fresh state.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const tick = async (n = 5) => { for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve)); };

test("changing the password twice in flight sends one request", async () => {
  const h = createHarness();
  await h.run();
  const field = id => h.ids.get(id);
  field("cpOldPassword").value = "old-secret";
  field("cpNewPassword").value = "new-secret";
  field("cpConfirmPassword").value = "new-secret";
  const base = h.context.fetch;
  let release, started = 0;
  h.context.fetch = (url, options) => url === "/admin/api/change-password"
    ? new Promise(resolve => { started++; release = () => resolve(json({})); h.requests.push({url, options}); })
    : base(url, options);
  const first = field("savePasswordBtn").listeners.click[0]();
  const second = field("savePasswordBtn").listeners.click[0]();
  await tick();
  assert.equal(started, 1);
  assert.equal(field("savePasswordBtn").disabled, true);
  release();
  await first; await second;
  assert.equal(field("savePasswordBtn").disabled, false);
});

test("disabling a user is decided from the latest list, not the state at click time", async () => {
  const h = createHarness({routes: {"/admin/data/users": {users: [{username: "bob", role: "readonly", disabled: false}]}}});
  const app = await h.run();
  const users = app.modules.users;
  await users.loadUsers();
  const toggle = users.toggleUserDisabled("bob");
  // While the confirmation is open someone else disables bob and the list refreshes.
  h.setRoute("/admin/data/users", {users: [{username: "bob", role: "readonly", disabled: true}]});
  await users.loadUsers();
  h.ids.get("confirmOkBtn").listeners.click[0]();
  await toggle;
  await tick(10);
  const puts = h.requests.filter(r => r.options?.method === "PUT" && r.url === "/admin/data/users/bob");
  assert.equal(puts.length, 0, "a changed state must not be flipped back by the stale confirmation");
});
