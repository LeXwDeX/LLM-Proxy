#!/usr/bin/env node
"use strict";
// Optional browser regression for Issue #93 (not run in CI): after the in-memory
// session disappears, a browser must not be able to replay a stored `302 /login`
// for /admin/ui/js/*.js on history navigation (HTML executed as JS -> SyntaxError
// -> boot stuck on "加载中…"). Builds and starts an isolated proxy (temp config and
// bbolt, fake upstreams), then drives Chromium and WebKit through Playwright:
//
//   NODE_PATH=$HOME/node_modules node scripts/testing/admin_auth_cache_browser_test.cjs
//
// ADMIN_SIM_ROOT=<dir> builds another source tree (e.g. the pre-fix commit).
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { manifest } = require("./admin_ui_scripts.cjs");
const { startStack, ADMIN_PASSWORD } = require("./admin_ui_sim.cjs");

function loadPlaywright() {
  for (const name of ["playwright", "playwright-core"]) {
    try { return require(name); } catch {}
  }
  throw new Error("playwright or playwright-core is not installed (set NODE_PATH)");
}

const SCRIPTS = manifest().map((s) => s.rel.replace(/\.js$/, ""));
const STATE = () => (document.getElementById("adminBootMessage") || {}).textContent +
  " ready=" + (window.AdminApp && window.AdminApp.state && window.AdminApp.state.ready);

async function runBrowser(pw, name, base) {
  const browser = await pw[name].launch({ headless: true });
  try {
    const page = await (await browser.newContext()).newPage();
    const pageErrors = [];
    page.on("pageerror", (e) => pageErrors.push(e.message));
    await page.goto(base + "/login");

    // 1. Logged-out subresource requests get a non-redirect, non-cacheable 401.
    const probe = await page.evaluate(async () => {
      const r = await fetch("/admin/ui/js/core/core.js", { redirect: "manual" });
      return { status: r.status, type: r.type, cc: r.headers.get("cache-control") };
    });
    assert.equal(probe.status, 401, name + ": logged-out script must be 401, got " + JSON.stringify(probe));
    assert.match(probe.cc || "", /no-store/, name + ": 401 must be no-store");

    // 2. Load every script tag while logged out (what a stale page/tab does).
    await page.evaluate((names) => Promise.all(names.map((n) => new Promise((resolve) => {
      const s = document.createElement("script");
      s.src = "/admin/ui/js/" + n + ".js";
      s.onload = s.onerror = resolve;
      document.head.appendChild(s);
    }))), SCRIPTS);
    pageErrors.length = 0;

    // 3. Log in without navigating, then return to /admin via history navigation.
    await page.evaluate((pw) => fetch("/login", {
      method: "POST", redirect: "manual",
      body: new URLSearchParams({ username: "admin", password: pw, next: "/admin" }),
    }), ADMIN_PASSWORD);
    await page.evaluate(() => history.pushState({}, "", "/admin"));
    await page.goto(base + "/healthz");
    await page.goBack();
    await page.waitForFunction(
      () => window.AdminApp && window.AdminApp.state && window.AdminApp.state.ready === true,
      null, { timeout: 15000 },
    ).catch(async () => {
      throw new Error(name + ": boot did not reach ready after history back: " + (await page.evaluate(STATE)) +
        " pageerrors=" + JSON.stringify(pageErrors.slice(0, 3)));
    });
    assert.deepEqual(pageErrors, [], name + ": page errors after history back");

    // 4. Served script URLs carry a per-response nonce and security headers.
    const hdr = await page.evaluate(async () => {
      const r = await fetch("/admin", { credentials: "same-origin" });
      const html = await r.text();
      return { cc: r.headers.get("cache-control"), xfo: r.headers.get("x-frame-options"),
        nosniff: r.headers.get("x-content-type-options"),
        srcs: [...html.matchAll(/src="(\/admin\/ui\/js\/[^"]+)"/g)].map((m) => m[1]) };
    });
    assert.match(hdr.cc, /no-store/);
    assert.equal(hdr.xfo, "DENY");
    assert.equal(hdr.nosniff, "nosniff");
    assert.equal(hdr.srcs.length, SCRIPTS.length);
    assert.ok(hdr.srcs.every((s) => /\?v=.+-[0-9a-f]{16}$/.test(s)), name + ": unversioned script src " + hdr.srcs[0]);

    // 5. Logged-out top-level navigation still redirects to login with next.
    await page.evaluate(() => fetch("/logout", { method: "POST", redirect: "manual" }));
    const nav = await page.goto(base + "/admin/ui?tab=keys");
    assert.match(page.url(), /\/login\?next=%2Fadmin%2Fui%3Ftab%3Dkeys/, name + ": navigation not redirected: " + page.url());
    assert.ok(nav.ok());

    // 6. Boot watchdog: a script that fails to load must not leave "加载中…" forever.
    await page.goto(base + "/login");
    await page.evaluate((pw) => fetch("/login", { method: "POST", redirect: "manual",
      body: new URLSearchParams({ username: "admin", password: pw, next: "/admin" }) }), ADMIN_PASSWORD);
    await page.route("**/admin/ui/js/core/schema.js*", (route) => route.abort());
    await page.goto(base + "/admin");
    await page.waitForFunction(
      () => /页面资源加载失败/.test((document.getElementById("adminBootMessage") || {}).textContent || ""),
      null, { timeout: 8000 });
    assert.equal(await page.evaluate(() => document.getElementById("adminBootRetry").hidden), false);
    assert.match(await page.evaluate(() => document.getElementById("adminBootMessage").textContent), /schema[^]*\/admin\/ui\/js\/core\/schema\.js/, "boot banner names the missing module and its script");
    await page.unroute("**/admin/ui/js/core/schema.js*");
    await Promise.all([page.waitForNavigation(), page.click("#adminBootRetry")]);
    await page.waitForFunction(() => window.AdminApp && window.AdminApp.state && window.AdminApp.state.ready === true, null, { timeout: 15000 });
    await shellUx(page, name, base, pageErrors);
    console.log("PASS " + name);
  } finally {
    await browser.close();
  }
}

const loginViaFetch = (page) => page.evaluate((pw) => fetch("/login", { method: "POST", redirect: "manual",
  body: new URLSearchParams({ username: "admin", password: pw, next: "/admin" }) }), ADMIN_PASSWORD);
const bannerText = (page, id) => page.evaluate((i) => { const e = document.getElementById(i); return e ? e.textContent : null; }, id);

// Issue #99: session expiry must not discard drafts, deep links survive login,
// and a redeploy is surfaced. Expects a logged-in, booted /admin page.
async function shellUx(page, name, base, pageErrors) {
  const ready = () => page.waitForFunction(() => window.AdminApp && window.AdminApp.state && window.AdminApp.state.ready === true, null, { timeout: 15000 });
  pageErrors.length = 0;

  // 1. Open the target modal, lose the session, click save: banner shown, modal and draft kept.
  await page.click('.sidebar-nav-item[data-page="targets"]');
  await page.click("#newTargetBtn");
  await page.waitForSelector("#targetModal.open");
  await page.fill("#tgtName", "draft-keep");
  await page.fill("#tgtEndpoint", "https://draft.example.invalid");
  await page.selectOption("#tgtUsageAdapter", "standard");
  await page.fill("#tgtNewKeyInput", "sk-draft-not-real");
  await page.click("#addKeyBtn");
  await page.evaluate(() => fetch("/logout", { method: "POST", redirect: "manual" }));
  // The real save request must be what hits the 401 (no client-validation stop, no api() fallback).
  const saveRequest = page.waitForResponse((r) => r.url().includes("/admin/data/targets") && r.request().method() === "POST");
  await page.click("#saveTargetBtn");
  assert.equal((await saveRequest).status(), 401, name + ": save request must be sent and rejected with 401");
  await page.waitForFunction(() => !!document.getElementById("sessionExpiredBanner"), null, { timeout: 5000 });
  assert.match(await bannerText(page, "sessionExpiredBanner"), /会话已过期，请在新标签页登录后重试/, name + ": banner text");
  const link = await page.evaluate(() => { const a = document.querySelector("#sessionExpiredBanner a"); return a && { href: a.getAttribute("href"), target: a.target, text: a.textContent }; });
  assert.deepEqual(link, { href: "/login?next=/admin", target: "_blank", text: "打开登录页" });
  assert.equal(new URL(page.url()).pathname, "/admin", name + ": page must not navigate away: " + page.url());
  assert.equal(await page.evaluate(() => document.getElementById("targetModal").classList.contains("open")), true, name + ": modal stays open");
  assert.equal(await page.inputValue("#tgtName"), "draft-keep", name + ": draft kept");
  assert.equal(await page.inputValue("#tgtEndpoint"), "https://draft.example.invalid", name + ": endpoint draft kept");

  // 2. Re-login elsewhere (same cookie jar), the next successful call clears the banner.
  await loginViaFetch(page);
  await page.evaluate(() => window.api("/admin/api/me"));
  assert.equal(await bannerText(page, "sessionExpiredBanner"), null, name + ": banner removed after recovery");
  await page.evaluate(() => window.closeModal("targetModal"));

  // 3. Redeploy detection: a changed /api/me version shows the reload banner.
  await page.route("**/admin/api/me", async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    await route.fulfill({ response: res, json: Object.assign(body, { version: "redeployed-999" }) });
  });
  await page.evaluate(() => window.api("/admin/api/me"));
  assert.match(await bannerText(page, "adminStaleBanner") || "", /后台已更新，请刷新页面/, name + ": stale banner");
  await page.unroute("**/admin/api/me");

  // 4. Deep link: logged-out navigation to /admin#clients returns there after login.
  await page.evaluate(() => fetch("/logout", { method: "POST", redirect: "manual" }));
  await page.goto(base + "/healthz"); // leave the document: a hash-only goto would not reload
  await page.goto(base + "/admin#clients");
  assert.match(page.url(), /\/login\?next=%2Fadmin/, name + ": redirected to login: " + page.url());
  await page.fill("#username", "admin");
  await page.fill("#password", ADMIN_PASSWORD);
  await Promise.all([page.waitForURL(/\/admin#clients$/), page.click("button[type=submit]")]);
  await ready();
  assert.equal(await page.evaluate(() => window.AdminApp.state.currentPage), "clients", name + ": deep link opens clients");
  await page.click('.sidebar-nav-item[data-page="targets"]');
  assert.equal(await page.evaluate(() => location.hash), "#targets", name + ": hash follows navigation");
  assert.deepEqual(pageErrors, [], name + ": page errors in shell UX flow");
}

async function main() {
  const pw = loadPlaywright();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "llms-auth-cache-"));
  let stack, failed = 0;
  try {
    stack = await startStack(dir);
    for (const name of ["chromium", "webkit"]) {
      try {
        await runBrowser(pw, name, stack.base);
      } catch (err) {
        failed++;
        console.log("FAIL " + name + " — " + String(err.message).split("\n")[0]);
      }
    }
  } finally {
    if (stack) stack.stop();
    fs.rmSync(dir, { recursive: true, force: true });
  }
  process.exitCode = failed ? 1 : 0;
}

main().catch((err) => { console.error(err); process.exitCode = 2; });
