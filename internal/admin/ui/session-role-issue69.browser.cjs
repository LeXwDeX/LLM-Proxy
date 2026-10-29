"use strict";
// Real browser, embedded UI and in-memory responses; no service is started.
const {test, before, after} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { assembleAdminHTML } = require("../../../scripts/testing/assemble_admin_html.cjs");
const { manifest } = require("../../../scripts/testing/admin_ui_scripts.cjs");
const playwright = require("playwright");
const {schemaFixture} = require("../../../scripts/testing/admin_ui_test.cjs");

const html = assembleAdminHTML();
const scripts = manifest(html).map(s => s.source);
const engine = process.env.UI_TEST_WEBKIT === "1" ? "webkit" : "chromium";
let browser;
before(async () => { browser = await playwright[engine].launch({headless: true}); });
after(async () => { if (browser) await browser.close(); });

async function pageWithFailedRole(width) {
  const page = await browser.newPage({viewport: {width, height: 844}});
  await page.setContent(html.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, ""));
  await page.evaluate(schema => {
    window.fixtureRole = "fail";
    window.fixtureRequests = [];
    window.fetch = async (input, options = {}) => {
      const url = String(input);
      window.fixtureRequests.push({url, method: options.method || "GET"});
      let body = {}, status = 200;
      if (url === "/admin/data/schema") body = schema;
      else if (url === "/admin/api/me") {
        if (window.fixtureRole === "fail") { body = {error: "role offline"}; status = 503; }
        else body = {authenticated: true, username: "local-user", role: window.fixtureRole};
      } else if (url === "/admin/data/targets") body = {targets: []};
      else if (url === "/admin/data/pools") body = window.fixturePools || {pools: []};
      else if (url === "/admin/data/model-groups") body = {model_groups: [
        {name: "shared", cost_coefficient: 1, weekly_usd: 50, monthly_usd: 200, bindings: []},
      ]};
      else if (url === "/admin/metrics") body = {targets_runtime: []};
      return new Response(JSON.stringify(body), {status, headers: {"Content-Type": "application/json"}});
    };
  }, schemaFixture());
  for (const source of scripts) await page.addScriptTag({content: source});
  await page.evaluate(() => AdminApp.bootPromise);
  return page;
}

for (const width of [390, 1200]) {
  test(`${engine} ${width}px: role failure blocks interaction and readonly recovery preserves password changes`, async () => {
    const page = await pageWithFailedRole(width);
    try {
      assert.equal(await page.locator(".layout").evaluate(el => el.inert), true);
      assert.equal(await page.locator("#newTargetBtn").evaluate(el => getComputedStyle(el).display), "none");
      assert.match(await page.locator("#adminBootMessage").innerText(), /role offline/);
      await page.evaluate(() => { window.fixtureRole = "readonly"; });
      await page.locator("#adminBootRetry").click();
      await page.waitForFunction(() => AdminApp.state.ready);
      assert.equal(await page.locator(".layout").evaluate(el => el.inert), false);
      assert.equal(await page.locator("#newTargetBtn").evaluate(el => getComputedStyle(el).display), "none");
      assert.equal(await page.locator("#nav-users").isVisible(), false);
      await page.locator("#changePasswordBtn").click();
      await page.locator("#cpOldPassword").fill("old-pass");
      await page.locator("#cpNewPassword").fill("new-pass");
      await page.locator("#cpConfirmPassword").fill("new-pass");
      await page.locator("#savePasswordBtn").click();
      assert.equal(await page.evaluate(() => window.fixtureRequests.some(r =>
        r.url === "/admin/api/change-password" && r.method === "POST")), true);
    } finally { await page.close(); }
  });
}

test(`${engine}: admin recovery permits editing and displays pool and quota boundaries`, async () => {
  const page = await pageWithFailedRole(1200);
  try {
    await page.evaluate(() => { window.fixtureRole = "admin"; window.fixturePools = {pools: null}; });
    await page.locator("#adminBootRetry").click();
    await page.waitForFunction(() => AdminApp.state.ready);
    assert.equal(await page.locator("#nav-users").isVisible(), true);
    await page.locator(".sidebar-nav-item[data-page='targets']").click();
    assert.equal(await page.locator("#newTargetBtn").isVisible(), true);
    await page.locator("#targetsPoolsTab").click();
    await page.waitForFunction(() => document.getElementById("poolsTableBody").textContent.includes("加载失败"));
    await page.evaluate(() => { window.fixturePools = {pools: []}; });
    await page.locator("#refreshPoolsBtn").click();
    await page.waitForFunction(() => document.getElementById("poolsTableBody").textContent.includes("暂无部署池"));
    await page.locator(".sidebar-nav-item[data-page='quota']").click();
    await page.locator("[data-quota-tab='policy']").click();
    await page.waitForFunction(() => document.getElementById("modelGroupsBody").textContent.includes("该周期上限不限"));
    assert.match(await page.locator("#modelGroupsBody").innerText(), /Schema日 \$0\.00（已绑定职级时该周期上限不限）/);
    assert.match(await page.locator("#modelGroupsBody").innerText(), /Schema周 \$50\.00/);
  } finally { await page.close(); }
});
