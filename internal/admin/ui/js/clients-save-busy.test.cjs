"use strict";
// Issue #240: while a client save is in flight the 保存 button is disabled and says 保存中…, so a
// slow response no longer looks like a frozen dialog. It is restored on success and on failure.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const tick = async (n = 10) => { for (let i = 0; i < n; i++) await new Promise(resolve => setImmediate(resolve)); };
const field = (h, id) => h.ids.get(id);

async function boot() {
  const clients = [{name: "c1", access_key: "k1", allowed_targets: [], allowed_pools: []}];
  const h = createHarness({routes: {"/admin/data/targets": {targets: []}, "/admin/data/pools": {pools: []}, "/admin/data/clients": {clients}}});
  const app = await h.run();
  await app.modules.clients.loadClients();
  await tick();
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  return {h, app};
}

// Holds the client PUT until released, then answers with `response`.
function holdPut(h, response) {
  const base = h.context.fetch;
  const held = [];
  h.context.fetch = (url, options) => options?.method === "PUT"
    ? new Promise(resolve => { h.requests.push({url, options}); held.push(() => resolve(response)); })
    : base(url, options);
  return {held, release: () => held.splice(0).forEach(r => r())};
}

async function openAndFill(h, app) {
  await app.modules.clients.openClientModal(app.state.clients[0]);
  h.ids.get("clientTargetTags").querySelectorAll = () => [];
  h.ids.get("clientPoolTags").querySelectorAll = () => [];
  field(h, "clientName").value = "c1";
  field(h, "clientAccessKey").value = "k1";
}

test("save button is disabled and labelled 保存中… while the PUT is in flight, then restored", async () => {
  const {h, app} = await boot();
  const btn = field(h, "saveClientBtn");
  const idleLabel = "保存";
  await openAndFill(h, app);
  const hold = holdPut(h, json({status: "ok", quota_refresh: "pending"}));
  const saving = btn.listeners.click[0]();
  await tick();
  assert.equal(hold.held.length, 1, "the PUT is in flight");
  assert.equal(btn.disabled, true, "button disabled while saving");
  assert.equal(btn.textContent, "保存中…");
  hold.release();
  await saving;
  assert.equal(btn.disabled, false, "button re-enabled after success");
  assert.equal(btn.textContent, idleLabel);
});

test("save button is restored when the PUT fails", async () => {
  const {h, app} = await boot();
  const btn = field(h, "saveClientBtn");
  await openAndFill(h, app);
  const hold = holdPut(h, {ok: false, status: 400, text: async () => JSON.stringify({error: "bad client"})});
  const saving = btn.listeners.click[0]();
  await tick();
  assert.equal(btn.disabled, true);
  assert.equal(btn.textContent, "保存中…");
  hold.release();
  await saving;
  assert.equal(btn.disabled, false, "button re-enabled after a failed save");
  assert.equal(btn.textContent, "保存");
});
