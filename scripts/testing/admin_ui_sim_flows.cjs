"use strict";
// Admin UI simulation flows, driven through the real pages (see
// admin_ui_sim.cjs). Every create/edit goes through the visible form and its
// save button; client traffic goes through the proxy to fake upstreams.
const assert = require("node:assert/strict");

const S = {
  pool: "sim-ui-pool",
  level: "sim-ui-level",
  project: "sim-ui-project",
  group: "sim-ui-quota-group",
  tgtOpenAI: "sim-ui-openai",
  tgtGLM: "sim-ui-glm",
  client: "sim-ui-client",
  clientKey: "sk-sim-ui-client-key",
  aliasOpenAI: "sim-ui-model",
  aliasGLM: "sim-ui-glm-model",
  upOpenAI: "Sim-Upstream-Model",
  keysOpenAI: ["sk-sim-ui-oa-1", "sk-sim-ui-oa-2"],
  keysGLM: ["sk-sim-ui-glm-1", "sk-sim-ui-glm-2"],
};

// In-page helpers, installed on every document.
const HELPERS = `
window.__sim = {
  $(sel) { const el = document.querySelector(sel); if (!el) throw new Error("missing " + sel); return el; },
  fill(sel, value) {
    const el = this.$(sel);
    el.focus();
    const proto = el.tagName === "TEXTAREA" ? HTMLTextAreaElement.prototype : el.tagName === "SELECT" ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, "value").set.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
    return true;
  },
  check(sel, on) { const el = this.$(sel); if (el.checked !== on) el.click(); return el.checked; },
  click(sel) { this.$(sel).click(); return true; },
  open(id) { const el = document.getElementById(id); return !!el && el.classList.contains("open"); },
  toasts() { return [...document.querySelectorAll("#toastContainer .toast")].map(t => t.className + "|" + t.textContent.trim()); },
  text(sel) { const el = document.querySelector(sel); return el ? el.textContent.trim() : ""; },
};
`;

async function install(page) {
  await page.send("Page.addScriptToEvaluateOnNewDocument", { source: HELPERS });
  await page.eval(HELPERS);
}

const js = (v) => JSON.stringify(v);

async function goto(page, name) {
  await page.eval(`__sim.click('.sidebar-nav-item[data-page=${js(name)}]')`);
  await page.waitFor(`document.getElementById(${js("page-" + name)}).classList.contains("active")`, { what: `page ${name} active` });
  if (name === "targets") {
    await page.eval(`__sim.click("#targetsMainTab")`);
    await page.waitFor(`!document.getElementById("targetsMainPanel").hidden`, { what: "targets tab active" });
  }
}

// expectToast waits for a toast containing text (success unless kind given).
async function expectToast(page, text, kind = "success") {
  await page.waitFor(`__sim.toasts().some(t => t.includes(${js(kind)}) && t.includes(${js(text)}))`,
    { what: `toast ${kind} "${text}"`, timeoutMs: 10000 }).catch(async (err) => {
    const seen = await page.eval("__sim.toasts()").catch(() => []);
    throw new Error(`${err.message}; toasts: ${JSON.stringify(seen)}`);
  });
}

async function confirmModal(page) {
  await page.waitFor(`__sim.open("confirmModal")`, { what: "confirm modal" });
  await page.eval(`__sim.click("#confirmOkBtn")`);
}

// adminAPI runs an authenticated admin data call from the page, simulating
// another operator changing state outside this browser's forms.
async function adminAPI(page, path, { method = "GET", body } = {}) {
  const init = { method, headers: { "X-Requested-With": "XMLHttpRequest" } };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  return page.eval(`fetch(${js(path)}, ${js(init)}).then(async (r) => {
    if (!r.ok) throw new Error(${js(`${method} ${path}`)} + " HTTP " + r.status + ": " + (await r.text()));
    return r.json();
  })`);
}

async function waitPoolMembers(page, pool, want, timeoutMs = 10000) {
  await page.waitFor(`(() => {
    const row = document.querySelector('[data-edit-pool=${js(pool)}]')?.closest("tr");
    if (!row) return false;
    const names = [...row.querySelectorAll(".pool-member-name")].map((el) => el.textContent.trim());
    const want = ${js(want)};
    return names.length === want.length && names.every((name, i) => name === want[i]);
  })()`, { what: `pool ${pool} members ${want.join(",") || "(none)"}`, timeoutMs });
}

// interceptTargetsFailure makes the member roster request fail so the page's
// explicit error path is exercised instead of rendering unknown as empty.
async function interceptTargetsFailure(page) {
  const handler = (msg) => {
    if (msg.method !== "Fetch.requestPaused") return;
    page.send("Fetch.fulfillRequest", {
      requestId: msg.params.requestId,
      responseCode: 503,
      responseHeaders: [{ name: "Content-Type", value: "application/json" }],
      body: Buffer.from(JSON.stringify({ error: "simulated targets failure" })).toString("base64"),
    }).catch(() => {});
  };
  page.listeners.push(handler);
  await page.send("Fetch.enable", { patterns: [{ urlPattern: "*/admin/data/targets*" }] });
  return {
    async stop() {
      page.listeners = page.listeners.filter((listener) => listener !== handler);
      await page.send("Fetch.disable");
    },
  };
}

// refreshOverview clicks the visible refresh control only when enabled and
// waits for the activity load it starts to finish. It does not sleep for a
// fixed interval.
async function refreshOverview(page) {
  await page.waitFor(`!document.getElementById("refreshOverviewBtn").disabled`,
    { what: "overview refresh enabled", timeoutMs: 10000 });
  await page.eval(`__sim.click("#refreshOverviewBtn")`);
  await page.waitFor(`document.getElementById("refreshOverviewBtn").disabled`,
    { what: "overview refresh running", timeoutMs: 2000 }).catch(() => {});
  await page.waitFor(`!document.getElementById("refreshOverviewBtn").disabled`,
    { what: "overview refresh finished", timeoutMs: 20000 });
}

// settleQuotaShot waits for the page's own transient UI before the harness
// captures this flow: toasts run out their real lifetime, and finite CSS
// transitions/animations finish. Infinite ambient animations may continue.
async function settleQuotaShot(page) {
  await page.waitFor(`document.querySelectorAll("#toastContainer .toast").length === 0`,
    { what: "quota toasts cleared", timeoutMs: 8000 });
  await page.waitFor(`document.getAnimations().every(a => !a.effect || a.effect.getTiming().iterations === Infinity || a.playState !== "running")`,
    { what: "quota transitions settled", timeoutMs: 5000 });
}

// scrollQuotaTableIntoView keeps mobile screenshots on the quota table when
// the formula and billing note push it below the fold. The page scrolls in
// .content, not in the window.
async function scrollQuotaTableIntoView(page) {
  await page.eval(`(() => {
    const table = document.querySelector("#quotaSummaryBody").closest("table");
    const scroller = document.querySelector(".content");
    const rect = table.getBoundingClientRect();
    const view = scroller.getBoundingClientRect();
    if (rect.top < view.top || rect.top > view.bottom - 160) {
      scroller.scrollTop += rect.top - view.top - 8;
    }
    return true;
  })()`);
  await page.waitFor(`(() => {
    const rect = document.querySelector("#quotaSummaryBody").closest("table").getBoundingClientRect();
    const view = document.querySelector(".content").getBoundingClientRect();
    return rect.top >= view.top - 1 && rect.top < view.bottom - 100;
  })()`, { what: "quota table in the mobile viewport", timeoutMs: 3000 });
}

async function fakeControl(base, body) {
  const r = await fetch(base + "/_control", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  assert.equal(r.status, 200, "fake /_control");
}

async function fakeRequests(base) {
  return (await fetch(base + "/_requests")).json();
}

const list = [
  {
    name: "login-page-light",
    async run({ page, stack }) {
      await page.navigate(stack.base + "/login");
      await page.waitFor(`!!document.querySelector("form.login-card")`, { what: "login card" });
      const state = await page.eval(`(() => {
        const user = document.querySelector('input[name=username]');
        const pass = document.querySelector('input[name=password]');
        return {
          text: document.body.innerText,
          username: user ? user.value : null,
          password: pass ? pass.value : null,
          passwordType: pass ? pass.type : null,
          cards: document.querySelectorAll("form.login-card").length,
          scripts: document.querySelectorAll("script, link").length,
          width: document.documentElement.scrollWidth,
          client: document.documentElement.clientWidth,
        };
      })()`);
      assert.ok(state.text.includes("后台管理登录"), "login heading is visible");
      assert.ok(state.text.includes("LLMs Proxy"), "admin identity is visible");
      assert.ok(!state.text.includes("请输入独立后台账号密码"), "helper marketing copy is removed");
      assert.ok(!state.text.includes("登录成功后将进入"), "redirect helper copy is removed");
      assert.equal(state.username, "", "username is not prefilled");
      assert.equal(state.password, "", "password is not prefilled");
      assert.equal(state.passwordType, "password", "password input stays masked");
      assert.equal(state.cards, 1, "login page has one centered card");
      assert.equal(state.scripts, 0, "login page loads no external scripts or styles");
      assert.ok(state.width <= state.client + 1, `login page overflows: ${state.width}/${state.client}`);
    },
  },
  {
    name: "login-page-error-light",
    async run({ page, stack }) {
      await page.navigate(stack.base + "/login");
      await page.waitFor(`!!document.querySelector("form.login-card")`, { what: "login card" });
      await page.eval(`(() => {
        const form = document.querySelector("form.login-card");
        const user = document.querySelector('input[name=username]');
        const pass = document.querySelector('input[name=password]');
        user.focus();
        user.value = "admin";
        user.dispatchEvent(new Event("input", { bubbles: true }));
        pass.focus();
        pass.value = "wrong-password-not-secret";
        pass.dispatchEvent(new Event("input", { bubbles: true }));
        form.requestSubmit();
        return true;
      })()`);
      await page.waitFor(`!!document.querySelector('.login-error[role="alert"]')`, { what: "login error alert", timeoutMs: 10000 });
      const state = await page.eval(`(() => ({
        error: document.querySelector('.login-error[role="alert"]').textContent,
        username: document.querySelector('input[name=username]').value,
        password: document.querySelector('input[name=password]').value,
      }))()`);
      assert.match(state.error, /用户名或密码错误/);
      assert.equal(state.username, "", "failed login does not echo the username");
      assert.equal(state.password, "", "failed login does not echo the password");
    },
  },
  {
    // Screenshots are captured after run() returns, so viewport/media overrides
    // stay active until the next flow restores them.
    name: "login-page-mobile-390",
    async run({ page, stack }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
      try {
        await page.navigate(stack.base + "/login");
        await page.waitFor(`!!document.querySelector("form.login-card")`, { what: "mobile login card" });
        const width = await page.eval(`({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth})`);
        assert.ok(width.scroll <= width.client + 1, `mobile login overflows: ${width.scroll}/${width.client}`);
      } catch (err) {
        await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
        throw err;
      }
    },
  },
  {
    name: "login-page-short-390x560",
    async run({ page, stack }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 560, deviceScaleFactor: 1, mobile: true });
      try {
        await page.navigate(stack.base + "/login");
        await page.waitFor(`!!document.querySelector("form.login-card")`, { what: "short login card" });
        const width = await page.eval(`({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth})`);
        assert.ok(width.scroll <= width.client + 1, `short login overflows: ${width.scroll}/${width.client}`);
        const fits = await page.eval(`(() => {
          const card = document.querySelector("form.login-card").getBoundingClientRect();
          return card.top >= 0 && card.bottom <= document.documentElement.scrollHeight + 1;
        })()`);
        assert.ok(fits, "short login card is reachable without clipping");
      } catch (err) {
        await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
        throw err;
      }
    },
  },
  {
    name: "login-page-dark",
    async run({ page, stack }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
      await page.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
      try {
        await page.navigate(stack.base + "/login");
        await page.waitFor(`matchMedia("(prefers-color-scheme: dark)").matches`, { what: "dark color scheme" });
        await page.waitFor(`getComputedStyle(document.documentElement).getPropertyValue("--bg").trim() === "#0D0F12"`,
          { what: "dark login tokens" });
        await page.waitFor(`getComputedStyle(document.body).backgroundColor === "rgb(13, 15, 18)"`,
          { what: "dark login surface" });
      } catch (err) {
        await page.send("Emulation.setEmulatedMedia", { media: "", features: [] });
        throw err;
      }
    },
  },
  {
    name: "login", critical: true,
    async run({ page, stack, ADMIN_PASSWORD }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
      await page.send("Emulation.setEmulatedMedia", { media: "", features: [] });
      await install(page);
      await page.navigate(stack.base + "/login");
      await page.eval(`__sim.fill('input[name=username]', 'admin') && __sim.fill('input[name=password]', ${js(ADMIN_PASSWORD)})`);
      const loaded = page.waitEvent("Page.loadEventFired");
      await page.eval(`__sim.click('button[type=submit]')`);
      await loaded;
      await page.waitFor(`location.pathname.startsWith("/admin") && window.AdminApp && AdminApp.state.ready === true`,
        { what: "admin SPA ready", timeoutMs: 15000 });
    },
  },
  {
    name: "login-rejects-wrong-password",
    async run({ page, stack }) {
      // Separate check on a fresh login page; the session cookie stays valid.
      const r = await fetch(stack.base + "/login", { method: "POST", redirect: "manual",
        headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "username=admin&password=wrong&next=%2Fadmin" });
      const body = await r.text();
      assert.ok(r.status !== 302 && body.includes("用户名或密码错误"), `status ${r.status}`);
    },
  },
  {
    name: "create-pool", critical: true,
    async run({ page }) {
      await goto(page, "targets");
      await page.eval(`__sim.click("#targetsPoolsTab")`);
      await page.waitFor(`!document.getElementById("targetsPoolsPanel").hidden`, { what: "pools tab active" });
      await page.eval(`__sim.click("#newPoolBtn")`);
      await page.waitFor(`__sim.open("poolModal")`);
      await page.eval(`__sim.fill("#poolName", ${js(S.pool)}) && __sim.fill("#poolDescription", "UI simulation pool")`);
      await page.eval(`__sim.click("#savePoolBtn")`);
      await expectToast(page, "部署池已创建");
      await page.waitFor(`[...document.querySelectorAll("#poolsTableBody td.pool-name")].some(td => td.textContent.includes(${js(S.pool)}))`, { what: "pool row" });
      await page.eval(`__sim.click("#targetsMainTab")`);
    },
  },
  {
    name: "create-job-level", critical: true,
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="settings"]')`);
      await page.eval(`__sim.click("#newJobLevelBtn")`);
      await page.waitFor(`__sim.open("jobLevelModal")`);
      await page.eval(`__sim.fill("#jobLevelName", ${js(S.level)}) && __sim.fill("#jobLevel-daily", "1") && __sim.fill("#jobLevel-weekly", "1") && __sim.fill("#jobLevel-monthly", "1")`);
      await page.eval(`__sim.click("#saveJobLevelBtn")`);
      await expectToast(page, "职级已保存");
      await page.waitFor(`__sim.text("#jobLevelsBody").includes(${js(S.level)})`, { what: "job level row" });
    },
  },
  {
    name: "create-project-group",
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="settings"]')`);
      await page.eval(`__sim.click("#newProjectGroupBtn")`);
      await page.waitFor(`__sim.open("dictModal")`);
      await page.eval(`__sim.fill("#dictEntryName", ${js(S.project)})`);
      await page.eval(`__sim.click("#saveDictEntryBtn")`);
      await expectToast(page, "项目组已保存");
      await page.waitFor(`__sim.text("#projectGroupsBody").includes(${js(S.project)})`, { what: "project group row" });
    },
  },
  {
    name: "create-quota-model-group",
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="policy"]')`);
      await page.eval(`__sim.click("#newModelGroupBtn")`);
      await page.waitFor(`__sim.open("modelGroupModal")`);
      await page.eval(`__sim.fill("#modelGroupName", ${js(S.group)}) && __sim.fill("#modelGroupCoefficient", "1") && __sim.fill("#modelGroup-daily", "100") && __sim.fill("#modelGroup-weekly", "500") && __sim.fill("#modelGroup-monthly", "1000")`);
      await page.eval(`__sim.click("#saveModelGroupBtn")`);
      await expectToast(page, "模型组已保存");
      await page.waitFor(`__sim.text("#modelGroupsBody").includes(${js(S.group)})`, { what: "model group row" });
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="people"]')`);
    },
  },
  {
    name: "target-form-requires-usage-adapter", critical: true,
    async run({ page, stack }) {
      await goto(page, "targets");
      await page.eval(`__sim.click("#newTargetBtn")`);
      await page.waitFor(`__sim.open("targetModal")`);
      assert.equal(await page.eval(`__sim.$("#tgtUsageAdapter").value`), "", "new target has no default");
      await page.eval(`__sim.fill("#tgtName", "sim-ui-unselected") && __sim.fill("#tgtEndpoint", ${js(stack.fakes[0] + "/openai/v1")})`);
      await page.eval(`__sim.click("#saveTargetBtn")`);
      await page.waitFor(`!__sim.$("#tgtUsageAdapterError").hidden && __sim.text("#tgtUsageAdapterError").includes("请选择计费类型")`);
      const targets = await page.eval(`fetch("/admin/data/targets").then(r => r.json()).then(d => d.targets)`);
      assert.ok(!targets.some(t => t.name === "sim-ui-unselected"), "missing choice created no target");
      assert.ok(await page.eval(`__sim.open("targetModal")`), "invalid form stays open");
      await page.eval(`closeModal("targetModal"), true`);
    },
  },
  {
    name: "create-target-openai-ws", critical: true,
    async run({ page, stack }) {
      await createTarget(page, { name: S.tgtOpenAI, type: "openai", endpoint: stack.fakes[0] + "/openai/v1", pool: S.pool,
        keys: S.keysOpenAI, upstream: S.upOpenAI, alias: S.aliasOpenAI, usageAdapter: "standard", preferWS: true });
    },
  },
  {
    name: "create-target-glm", critical: true,
    async run({ page, stack }) {
      await createTarget(page, { name: S.tgtGLM, type: "openai", endpoint: stack.fakes[1] + "/glm/api/paas/v4",
        keys: S.keysGLM, upstream: "glm-4.6", alias: S.aliasGLM, usageAdapter: "glm" });
    },
  },
  {
    name: "target-edit-preserves-usage-adapter",
    async run({ page }) {
      await goto(page, "targets");
      for (const [name, value] of [[S.tgtOpenAI, "standard"], [S.tgtGLM, "glm"]]) {
        await page.eval(`__sim.click('[data-edit-tgt=${js(name)}]')`);
        await page.waitFor(`__sim.open("targetModal")`);
        assert.equal(await page.eval(`__sim.$("#tgtUsageAdapter").value`), value, `stored adapter for ${name}`);
        await page.eval(`closeModal("targetModal"), true`);
      }
    },
  },
  {
    name: "target-form-hides-ws-toggle-for-claude",
    async run({ page }) {
      await page.eval(`__sim.click("#newTargetBtn")`);
      await page.waitFor(`__sim.open("targetModal")`);
      await page.eval(`__sim.fill("#tgtType", "openai")`);
      const shownOpenAI = await page.eval(`getComputedStyle(__sim.$("#tgtPreferWSGroup")).display !== "none"`);
      await page.eval(`__sim.fill("#tgtType", "claude")`);
      const shownClaude = await page.eval(`getComputedStyle(__sim.$("#tgtPreferWSGroup")).display !== "none"`);
      await page.eval(`closeModal("targetModal"), true`);
      assert.ok(shownOpenAI && !shownClaude, `prefer_websocket toggle openai=${shownOpenAI} claude=${shownClaude}`);
    },
  },
  {
    name: "create-client", critical: true,
    async run({ page }) {
      await goto(page, "clients");
      await page.eval(`AdminApp.state.dictsLoaded = false, true`);
      await page.eval(`__sim.click("#newClientBtn")`);
      await page.waitFor(`__sim.open("clientModal") && [...document.querySelectorAll("#clientJobLevel option")].some(o => o.value === ${js(S.level)}) && [...document.querySelectorAll("#clientProjectGroup option")].some(o => o.value === ${js(S.project)})`,
        { what: "client modal with job and project options" });
      await page.eval(`__sim.fill("#clientName", ${js(S.client)}) && __sim.fill("#clientAccessKey", ${js(S.clientKey)}) && __sim.fill("#clientJobLevel", ${js(S.level)}) && __sim.fill("#clientProjectGroup", ${js(S.project)})`);
      await page.waitFor(`document.querySelector('#clientPoolTags span[data-pool=${js(S.pool)}]') && document.querySelector('#clientTargetTags span[data-target=${js(S.tgtGLM)}]')`,
        { what: "pool/target chips" });
      await page.eval(`__sim.click('#clientPoolTags span[data-pool=${js(S.pool)}]') && __sim.click('#clientTargetTags span[data-target=${js(S.tgtGLM)}]')`);
      await page.eval(`__sim.click("#saveClientBtn")`);
      await expectToast(page, "客户端已创建");
      await page.waitFor(`[...document.querySelectorAll("#clientsTableBody tr td:first-child")].some(td => td.textContent.includes(${js(S.client)}))`, { what: "client row" });
    },
  },
  {
    name: "client-traffic-through-ui-config", critical: true,
    async run({ stack, clientCall }) {
      // OpenAI target (pooled upstream WS, prefer_websocket on) and GLM target.
      for (let i = 0; i < 3; i++) {
        const r = await clientCall(stack.base, S.clientKey, "/v1/responses", { model: S.aliasOpenAI,
          input: [{ role: "user", content: [{ type: "input_text", text: "[[fake usage.in=100 usage.out=20 usage.cached=40]]" }] }] });
        assert.equal(r.status, 200, `responses ${i}: ${r.text.slice(0, 200)}`);
      }
      const chat = await clientCall(stack.base, S.clientKey, "/v1/chat/completions", { model: S.aliasGLM,
        messages: [{ role: "user", content: "[[fake usage.in=50 usage.out=10]]" }] });
      assert.equal(chat.status, 200, chat.text.slice(0, 200));
      const recs = await fakeRequests(stack.fakes[0]);
      const ws = recs.filter((r) => r.websocket && r.model === S.upOpenAI && r.outcome === "ok");
      assert.ok(ws.length === 3, `fake-a saw ${ws.length} upstream WS turns with the original-case model, want 3`);
      // Key 1 of the GLM target hits GLM's 5-hour quota code (1308).
      await fakeControl(stack.fakes[1], { keys: { [S.keysGLM[0]]: { preset: "glm_1308" } } });
      for (let i = 0; i < 3; i++) {
        const r = await clientCall(stack.base, S.clientKey, "/v1/chat/completions", { model: S.aliasGLM, messages: [{ role: "user", content: "hi" }] });
        assert.equal(r.status, 200, `glm after 1308 ${i}: ${r.text.slice(0, 200)}`);
      }
    },
  },
  {
    name: "usage-page-shows-billed-tokens",
    async run({ page }) {
      await goto(page, "usage");
      await page.eval(`__sim.click('.time-range-btn[data-range="today"]')`);
      await page.waitFor(`[...document.querySelectorAll("#usageByClientBody tr")].some(tr => tr.textContent.includes(${js(S.client)}))`,
        { what: "client row in usage", timeoutMs: 15000 });
      // Deterministic fake usage: 3 Responses turns x (in 100, cached 40,
      // out 20) + GLM chats (in 50/out 10, then 3 x "hi" = 8 in / 16 out).
      // Billed input excludes cache reads: 3*60 + 50 + 24 = 254.
      const want = { "#usReqs": 7, "#usInput": 254, "#usOutput": 118, "#usCached": 120, "#usCacheWrite": 0 };
      const expr = Object.entries(want).map(([sel, n]) => `Number(__sim.text(${js(sel)}).replace(/[^0-9]/g, "")) === ${n}`).join(" && ");
      // Usage is persisted asynchronously and the page only loads on demand:
      // re-click the range (the UI's refresh) until the totals settle.
      const deadline = Date.now() + 15000;
      let ok = false;
      while (!ok && Date.now() < deadline) {
        await page.eval(`__sim.click('.time-range-btn[data-range="today"]')`);
        ok = await page.waitFor(expr, { timeoutMs: 1500 }).then(() => true, () => false);
      }
      if (!ok) {
        const got = await page.eval(`${js(Object.keys(want))}.map(s => s + "=" + __sim.text(s))`);
        throw new Error(`usage totals never reached ${JSON.stringify(want)}; got ${got.join(" ")}`);
      }
    },
  },
  {
    name: "overview-counts-requests",
    async run({ page }) {
      await goto(page, "overview");
      const readOverview = () => page.eval(`fetch("/admin/api/overview").then(r => r.json())`);
      // request_stats_72h excludes the current unfinished minute by design, so
      // the UI must equal the closed-minute snapshot, not a wall-clock request
      // count. Usage events are recorded immediately; they prove the traffic
      // independently of that snapshot.
      const deadline = Date.now() + 30000;
      let overview, stats, usageToday, uiText, uiTotal;
      await refreshOverview(page);
      for (;;) {
        overview = await readOverview();
        stats = overview && overview.request_stats_72h;
        usageToday = overview && overview.usage_summary && overview.usage_summary.today;
        uiText = (await page.eval(`__sim.text("#ovTotalReqs")`)).trim();
        uiTotal = Number(uiText.replace(/[^0-9]/g, ""));
        const statsReady = stats && stats.available === true &&
          Number.isSafeInteger(stats.total) && stats.total >= 0;
        const usageReady = usageToday && Number.isSafeInteger(usageToday.requests) &&
          usageToday.requests >= 7;
        const uiReady = statsReady && /^[0-9][0-9,]*$/.test(uiText) &&
          uiTotal === stats.total;
        if (statsReady && usageReady && uiReady) break;
        if (Date.now() >= deadline) break;
        await refreshOverview(page);
      }
      assert.ok(stats, "overview response contains request_stats_72h");
      assert.equal(stats.available, true, "request statistics are available");
      assert.ok(Number.isSafeInteger(stats.total) && stats.total >= 0,
        `closed-minute total must be a non-negative integer: ${JSON.stringify(stats)}`);
      assert.ok(usageToday && Number.isSafeInteger(usageToday.requests) && usageToday.requests >= 7,
        `sent traffic missing from usage_summary today: ${JSON.stringify(usageToday)}`);
      assert.ok(/^[0-9][0-9,]*$/.test(uiText), `overview total is not numeric: "${uiText}"`);
      assert.equal(uiTotal, stats.total,
        `overview must show closed-minute total ${stats.total}; UI shows "${uiText}"`);
    },
  },
  {
    name: "overview-chain-range-and-ranking",
    async run({ page }) {
      await goto(page, "overview");
      // The overview is cached for 60s by design; force one fresh read before
      // asserting the targets created by earlier flows.
      await page.waitFor(`!document.getElementById("refreshOverviewBtn").disabled`, { what: "overview refresh enabled", timeoutMs: 10000 });
      await page.eval(`__sim.click("#refreshOverviewBtn")`);
      await page.waitFor(`document.querySelectorAll("#targetHealthBody tr[data-target-name]").length >= 2`,
        { what: "overview target health rows", timeoutMs: 15000 });
      const chain = await page.eval(`(() => {
        const keys = document.getElementById("chainKeys");
        return {
          total: Number(keys.dataset.total),
          active: Number(keys.dataset.active),
          rateLimited: Number(keys.dataset.rateLimited),
          clients: document.getElementById("chainClients").textContent,
          status: document.getElementById("chainStatus").textContent,
        };
      })()`);
      assert.equal(chain.total, S.keysOpenAI.length + S.keysGLM.length);
      assert.ok(chain.active >= 1 && chain.active <= chain.total, `active keys ${chain.active}/${chain.total}`);
      assert.ok(chain.rateLimited >= 1, `rate-limited keys ${chain.rateLimited}`);
      assert.match(chain.clients, /1 个配置客户端/);
      assert.ok(chain.status.length > 0);
      const limited = await page.eval(`(() => {
        const row = [...document.querySelectorAll("#targetHealthBody tr[data-target-name]")]
          .find(tr => tr.dataset.targetName === ${js(S.tgtGLM)});
        return row ? {issue: row.dataset.targetKeyIssue, text: row.textContent} : null;
      })()`);
      assert.ok(limited, "GLM target row is rendered");
      assert.equal(limited.issue, "true", "GLM row exposes its key issue");
      assert.match(limited.text, /限流/);
      await page.eval(`__sim.click('[data-overview-range="7d"]')`);
      await page.waitFor(`document.querySelector('[data-overview-range="7d"]').getAttribute("aria-pressed") === "true" && document.querySelectorAll("#topClientsBody tr[data-top-client]").length > 0`,
        { what: "7d range cards and top clients", timeoutMs: 15000 });
      const range = await page.eval(`({
        status: document.getElementById("ovRangeStatus").textContent,
        cost: document.getElementById("ovRangeCost").textContent,
        requests: document.getElementById("ovRangeRequests").textContent,
        clients: [...document.querySelectorAll("#topClientsBody tr[data-top-client]")].map(tr => tr.dataset.topClient),
        models: [...document.querySelectorAll("#topModelsBody tr[data-top-model]")].map(tr => tr.dataset.topModel),
      })`);
      assert.match(range.status, /含模型组系数/);
      assert.notEqual(range.requests, "不可用");
      assert.notEqual(range.cost, "不可用");
      assert.ok(range.clients.includes(S.client), `top clients ${range.clients.join(",")}`);
      assert.ok(range.models.length > 0, "top models are derived from aggregate by_model");
      assert.ok(range.models.every(model => model && model !== "不可用"), `top models ${range.models.join(",")}`);
    },
  },
  {
    name: "overview-attention-and-recent-changes",
    async run({ page }) {
      await goto(page, "overview");
      await page.waitFor(`!document.getElementById("refreshOverviewBtn").disabled`, { what: "overview refresh enabled", timeoutMs: 10000 });
      await page.eval(`__sim.click("#refreshOverviewBtn")`);
      await page.waitFor(`document.querySelectorAll("#recentChangesBody tr[data-audit-id]").length > 0`,
        { what: "recent audit rows", timeoutMs: 15000 });
      assert.equal(await page.eval(`document.querySelectorAll("#recentChangesBody tr:first-child td").length`), 5,
        "recent changes render only timestamp/actor/action/object/result");
      await page.waitFor(`document.querySelectorAll("#overviewAttentionList li[data-attention-kind]").length > 0`,
        { what: "attention items", timeoutMs: 10000 });
      const attention = await page.eval(`document.getElementById("overviewAttentionList").textContent`);
      assert.ok(attention.includes(S.tgtGLM), `attention list: ${attention}`);
      const status = await page.eval(`document.getElementById("overviewAttentionStatus").textContent`);
      assert.ok(status.length > 0, "attention status is present");
    },
  },
  {
    name: "overview-mobile-no-overflow",
    async run({ page }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
      try {
        await goto(page, "overview");
        await page.waitFor(`!document.getElementById("refreshOverviewBtn").disabled`, { what: "overview refresh enabled", timeoutMs: 10000 });
        await page.eval(`__sim.click("#refreshOverviewBtn")`);
        await page.waitFor(`document.querySelectorAll("#targetHealthBody tr[data-target-name]").length >= 2`,
          { what: "mobile overview", timeoutMs: 15000 });
        const width = await page.eval(`({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth})`);
        assert.ok(width.scroll <= width.client + 1, `mobile overview overflows: ${width.scroll}/${width.client}`);
      } finally {
        await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
      }
    },
  },
  {
    name: "target-table-shows-glm-key-exhausted",
    async run({ page }) {
      await goto(page, "targets");
      await page.waitFor(`[...document.querySelectorAll("#targetsTableBody tr")].some(tr => tr.querySelector('td.target-text[title=${js(S.tgtGLM)}]') && /限流|超限|冷却|1 /.test(tr.innerText))`,
        { what: "GLM row shows a limited key", timeoutMs: 10000 });
    },
  },
  {
    name: "target-health-filter-and-detail-drawer",
    async run({ page }) {
      await goto(page, "targets");
      await page.eval(`__sim.click('#targetStatusFilters [data-target-filter="attention"]')`);
      await page.waitFor(`document.querySelector('#targetStatusFilters [data-target-filter="attention"]').getAttribute("aria-pressed") === "true"`,
        { what: "attention filter pressed" });
      const visibleHealth = await page.eval(`[...document.querySelectorAll("#targetsTableBody tr[data-target-health]")].map(tr => tr.dataset.targetHealth)`);
      assert.ok(visibleHealth.length > 0, "attention filter keeps abnormal targets");
      assert.ok(visibleHealth.every(value => value === "attention"), "attention filter hides healthy and paused targets");
      assert.ok(await page.eval(`!!document.querySelector('#targetsTableBody td.target-text[title=${js(S.tgtGLM)}]')`),
        "exhausted-key target is shown under the attention filter");
      await page.eval(`__sim.click('#targetStatusFilters [data-target-filter="all"]')`);
      await page.eval(`__sim.click('[data-detail-tgt=${js(S.tgtGLM)}]')`);
      await page.waitFor(`__sim.open("targetDetailDrawer")`, { what: "target detail drawer" });
      const detailTabs = await page.eval(`[...document.querySelectorAll("#targetDetailTabs [data-target-detail-tab]")].map(button => button.dataset.targetDetailTab)`);
      assert.deepEqual(detailTabs, ["overview", "keys", "models", "advanced"]);
      await page.eval(`__sim.click('#targetDetailTabs [data-target-detail-tab="keys"]')`);
      const detailKeys = await page.eval(`__sim.text("#targetDetailKeys")`);
      assert.ok(detailKeys.includes("sk-"), "key detail lists masked keys");
      assert.ok(!detailKeys.includes(S.keysGLM[0]), "key detail never shows a plaintext key");
      await page.eval(`__sim.click('#targetDetailTabs [data-target-detail-tab="models"]')`);
      await page.waitFor(`document.querySelectorAll("#targetDetailModels .target-model-badge").length > 0`, { what: "detail model badges" });
      await page.eval(`__sim.click('#targetDetailTabs [data-target-detail-tab="advanced"]')`);
      assert.match(await page.eval(`__sim.text("#targetDetailAdvanced")`), /用量适配/);
      await page.eval(`closeModal("targetDetailDrawer"), true`);
    },
  },
  {
    name: "key-block-and-unblock-in-edit-modal",
    async run({ page }) {
      await page.eval(`__sim.click('[data-edit-tgt=${js(S.tgtOpenAI)}]')`);
      await page.waitFor(`__sim.open("targetModal") && document.querySelector('#keyCardList [data-block-key-idx="1"]')`, { what: "edit modal key cards" });
      await page.eval(`__sim.click('#keyCardList [data-block-key-idx="1"]')`);
      await confirmModal(page);
      await expectToast(page, "已屏蔽 Key");
      await page.waitFor(`document.querySelector('#keyCardList [data-unblock-key-idx="1"]')`, { what: "unblock button after block" });
      await page.eval(`__sim.click('#keyCardList [data-unblock-key-idx="1"]')`);
      await confirmModal(page);
      await expectToast(page, "已解除屏蔽");
    },
  },
  {
    name: "test-model-button-hits-fake",
    async run({ page, stack }) {
      const before = (await fakeRequests(stack.fakes[0])).length;
      if (!(await page.eval(`__sim.open("targetModal")`))) {
        await page.eval(`__sim.click('[data-edit-tgt=${js(S.tgtOpenAI)}]')`);
        await page.waitFor(`__sim.open("targetModal")`);
      }
      await page.waitFor(`document.querySelector("#model-mappings-container .test-model-btn")`, { what: "test button" });
      await page.eval(`__sim.click("#model-mappings-container .test-model-btn")`);
      try {
        // The product reports the actual HTTP probe result, not a generic
        // "test succeeded" message.
        await expectToast(page, "HTTP 检测通过");
      } catch (err) {
        const afterFailure = (await fakeRequests(stack.fakes[0])).length;
        throw new Error(`${err.message}; fake requests ${before} -> ${afterFailure}`);
      }
      const after = (await fakeRequests(stack.fakes[0])).length;
      assert.ok(after > before, "fake-a received no test-model request");
      await page.eval(`closeModal("targetModal"), true`);
    },
  },
  {
    name: "pause-and-resume-target",
    async run({ page, stack, clientCall }) {
      await goto(page, "targets");
      await page.eval(`__sim.click('button.target-toggle[data-pause-tgt=${js(S.tgtOpenAI)}]')`);
      await confirmModal(page);
      await expectToast(page, "已暂停目标");
      const paused = await clientCall(stack.base, S.clientKey, "/v1/responses", { model: S.aliasOpenAI, input: [] });
      assert.notEqual(paused.status, 200, "paused target still served traffic");
      await page.waitFor(`document.querySelector('button.target-toggle[data-resume-tgt=${js(S.tgtOpenAI)}]')`, { what: "resume toggle" });
      await page.eval(`__sim.click('button.target-toggle[data-resume-tgt=${js(S.tgtOpenAI)}]')`);
      await expectToast(page, "已恢复目标");
      const resumed = await clientCall(stack.base, S.clientKey, "/v1/responses", { model: S.aliasOpenAI,
        input: [{ role: "user", content: [{ type: "input_text", text: "hi" }] }] });
      assert.equal(resumed.status, 200, `after resume: ${resumed.text.slice(0, 200)}`);
    },
  },
  {
    name: "pool-pause-pauses-members-and-resume-restores",
    critical: true,
    async run({ page }) {
      await goto(page, "targets");
      await page.eval(`__sim.click("#targetsPoolsTab")`);
      await page.waitFor(`!document.getElementById("targetsPoolsPanel").hidden`, { what: "pools tab active" });
      const readPool = () => page.eval(`(() => {
        const tr = document.querySelector('[data-pause-pool=${js(S.pool)}]')?.closest("tr")
          || document.querySelector('[data-resume-pool=${js(S.pool)}]')?.closest("tr");
        if (!tr) return null;
        return {
          paused: !!tr.querySelector('[data-resume-pool=${js(S.pool)}]'),
          status: tr.querySelector(".pool-status")?.textContent.trim() || "",
          members: [...tr.querySelectorAll(".pool-member-name")].map(el => el.textContent.trim()),
        };
      })()`);
      const storedTargetPaused = () => page.eval(`fetch("/admin/data/targets").then(r => r.json())
        .then(d => d.targets.find(t => t.name === ${js(S.tgtOpenAI)})?.paused)`);
      // loadPools renders pool rows before its member targets and runtime finish
      // loading. Wait for the actual member chip instead of reading a partial row.
      const waitPoolMembers = () => page.waitFor(`(() => {
        const tr = document.querySelector('[data-pause-pool=${js(S.pool)}]')?.closest("tr")
          || document.querySelector('[data-resume-pool=${js(S.pool)}]')?.closest("tr");
        if (!tr) return false;
        const names = [...tr.querySelectorAll(".pool-member-name")].map(el => el.textContent.trim());
        return names.length === 1 && names[0] === ${js(S.tgtOpenAI)};
      })()`, { what: "loaded pool member chips", timeoutMs: 10000 });
      await waitPoolMembers();
      const before = await readPool();
      assert.ok(before, "member pool row is rendered");
      assert.equal(before.paused, false, "pool starts running");
      assert.match(before.status, /运行中/);
      assert.deepEqual(before.members, [S.tgtOpenAI], "pool membership before pause");
      assert.equal(await storedTargetPaused(), false, "target starts unpaused in storage");

      await page.eval(`__sim.click('[data-pause-pool=${js(S.pool)}]')`);
      await confirmModal(page);
      await expectToast(page, "已暂停池");
      await page.waitFor(`document.querySelector('[data-resume-pool=${js(S.pool)}]')`, { what: "pause button flips to resume" });
      await waitPoolMembers();
      const paused = await readPool();
      assert.equal(paused.paused, true, "pool row reports paused");
      assert.match(paused.status, /已暂停/);
      assert.deepEqual(paused.members, before.members, "pause keeps pool membership");

      // The target page must show the pool member as effectively paused
      // without turning it into an individually saved target pause.
      await page.eval(`__sim.click("#targetsMainTab")`);
      await page.waitFor(`!document.getElementById("targetsMainPanel").hidden`, { what: "targets tab active" });
      await page.eval(`__sim.click("#refreshTargetsBtn")`);
      await page.waitFor(`(() => {
        const toggle = document.querySelector('[data-pause-tgt=${js(S.tgtOpenAI)}][aria-checked="true"]');
        const tr = toggle && toggle.closest("tr");
        return !!tr && tr.dataset.targetHealth === "paused";
      })()`, { what: "pool-paused member in target list", timeoutMs: 10000 });
      const memberWhilePaused = await page.eval(`(() => {
        const toggle = document.querySelector('[data-pause-tgt=${js(S.tgtOpenAI)}]');
        const tr = toggle && toggle.closest("tr");
        const badge = tr?.querySelector(".target-health-badge");
        return toggle && tr ? {
          checked: toggle.getAttribute("aria-checked"),
          ownResumeControl: !!tr.querySelector('[data-resume-tgt=${js(S.tgtOpenAI)}]'),
          switchText: toggle.textContent.trim(),
          switchLabel: toggle.getAttribute("aria-label") || "",
          health: tr.dataset.targetHealth,
          badge: badge?.textContent.trim() || "",
          badgeTitle: badge?.getAttribute("title") || "",
        } : null;
      })()`);
      assert.ok(memberWhilePaused, "pool member row and own switch are rendered");
      assert.equal(memberWhilePaused.health, "paused", "pool member is shown as paused");
      assert.match(memberWhilePaused.badge, /已暂停/);
      assert.match(memberWhilePaused.badgeTitle, /部署池/,
        "effective pause exposes the pool as its reason");
      assert.equal(memberWhilePaused.checked, "true",
        "pool pause must not flip the member's own pause switch");
      assert.equal(memberWhilePaused.ownResumeControl, false,
        "pool pause must not turn the target switch into a pool resume control");
      assert.match(memberWhilePaused.switchText, /目标启用/);
      assert.match(memberWhilePaused.switchLabel, /未暂停/);
      assert.equal(await storedTargetPaused(), false,
        "pool pause must not save a target-level pause");
      const pausedCount = await page.eval(`Number(__sim.text('#targetStatusFilters [data-target-filter="paused"] .target-status-count'))`);
      assert.ok(pausedCount >= 1, `paused filter count ${pausedCount}`);

      await page.eval(`__sim.click("#targetsPoolsTab")`);
      await page.waitFor(`!document.getElementById("targetsPoolsPanel").hidden`, { what: "pools tab active again" });
      await page.eval(`__sim.click('[data-resume-pool=${js(S.pool)}]')`);
      await expectToast(page, "已恢复池");
      await page.waitFor(`document.querySelector('[data-pause-pool=${js(S.pool)}]')`, { what: "resume button flips to pause" });
      await waitPoolMembers();
      const resumed = await readPool();
      assert.equal(resumed.paused, false, "pool row reports running");
      assert.match(resumed.status, /运行中/);
      assert.deepEqual(resumed.members, before.members, "resume keeps pool membership");

      await page.eval(`__sim.click("#targetsMainTab")`);
      await page.waitFor(`!document.getElementById("targetsMainPanel").hidden`, { what: "targets tab active after resume" });
      await page.eval(`__sim.click("#refreshTargetsBtn")`);
      await page.waitFor(`(() => {
        const toggle = document.querySelector('[data-pause-tgt=${js(S.tgtOpenAI)}][aria-checked="true"]');
        const tr = toggle && toggle.closest("tr");
        return !!tr && tr.dataset.targetHealth !== "paused";
      })()`, { what: "member restored to running", timeoutMs: 10000 });
      assert.equal(await storedTargetPaused(), false,
        "pool pause/resume must not persist an individual target pause");
    },
  },
  {
    name: "delete-empty-pool-only",
    async run({ page }) {
      await goto(page, "targets");
      await page.eval(`__sim.click("#targetsPoolsTab")`);
      await page.waitFor(`!document.getElementById("targetsPoolsPanel").hidden`, { what: "pools tab active" });
      await page.eval(`__sim.click("#newPoolBtn")`);
      await page.waitFor(`__sim.open("poolModal")`, { what: "pool modal" });
      await page.eval(`__sim.fill("#poolName", "sim-ui-empty-pool") && __sim.fill("#poolDescription", "temporary empty pool")`);
      await page.eval(`__sim.click("#savePoolBtn")`);
      await expectToast(page, "部署池已创建");
      await page.waitFor(`document.querySelector('[data-del-pool="sim-ui-empty-pool"]')`, { what: "empty pool row" });
      assert.equal(await page.eval(`fetch("/admin/data/targets").then(r => r.json())
        .then(d => d.targets.some(t => String(t.pool || "").toLowerCase() === "sim-ui-empty-pool"))`),
        false, "temporary pool is empty before delete");
      await page.eval(`__sim.click('[data-del-pool="sim-ui-empty-pool"]')`);
      await confirmModal(page);
      await expectToast(page, "已删除池");
      await page.waitFor(`!document.querySelector('[data-del-pool="sim-ui-empty-pool"]')`, { what: "empty pool row gone" });
      const pools = await page.eval(`fetch("/admin/data/pools").then(r => r.json()).then(d => d.pools.map(p => p.name))`);
      assert.ok(!pools.includes("sim-ui-empty-pool"), "temporary pool was deleted");
      assert.ok(pools.includes(S.pool), "member pool was not deleted");
      await page.eval(`__sim.click("#targetsMainTab")`);
    },
  },
  {
    // Another operator can change membership while this page is off-screen or
    // idle. Re-entering and refreshing must both reload the roster, and a
    // failed roster fetch must read as an error, never as "无成员".
    name: "pool-refresh-tracks-external-membership",
    async run({ page }) {
      const tempPool = "sim-ui-refresh-pool";
      const targetPath = "/admin/data/targets/" + encodeURIComponent(S.tgtOpenAI);
      let intercept = null;
      let failure = null;
      try {
        await goto(page, "targets");
        await page.eval(`__sim.click("#targetsPoolsTab")`);
        await page.waitFor(`!document.getElementById("targetsPoolsPanel").hidden`, { what: "pools tab active" });
        await waitPoolMembers(page, S.pool, [S.tgtOpenAI]);

        // Leave with the pools tab selected, then change state via the API.
        await page.eval(`__sim.click('.sidebar-nav-item[data-page="overview"]')`);
        await page.waitFor(`document.getElementById("page-overview").classList.contains("active")`,
          { what: "overview page active" });
        await adminAPI(page, "/admin/data/pools", { method: "POST", body: { name: tempPool } });
        await adminAPI(page, targetPath, { method: "PUT", body: { pool: tempPool } });
        await page.eval(`__sim.click('.sidebar-nav-item[data-page="targets"]')`);
        await page.waitFor(`document.getElementById("page-targets").classList.contains("active") && !document.getElementById("targetsPoolsPanel").hidden`,
          { what: "returned to visible pools tab" });
        await waitPoolMembers(page, tempPool, [S.tgtOpenAI]);
        assert.equal(
          await page.eval(`document.querySelector('[data-edit-pool=${js(S.pool)}]')?.closest("tr").querySelector(".pool-members").textContent.trim()`),
          "无成员",
          "returning reloads the old pool as empty",
        );

        // A visible tab must refresh membership instead of reusing its cache.
        await adminAPI(page, targetPath, { method: "PUT", body: { pool: S.pool } });
        await page.eval(`__sim.click("#refreshPoolsBtn")`);
        await waitPoolMembers(page, S.pool, [S.tgtOpenAI]);
        await waitPoolMembers(page, tempPool, []);

        // Unknown membership is a failure, not an empty pool.
        intercept = await interceptTargetsFailure(page);
        await page.eval(`__sim.click("#refreshPoolsBtn")`);
        await page.waitFor(`__sim.text("#poolsTableBody").includes("加载失败: simulated targets failure")`,
          { what: "explicit pool load failure", timeoutMs: 10000 });
        const table = await page.eval(`__sim.text("#poolsTableBody")`);
        assert.doesNotMatch(table, /无成员/, "failed member fetch is not rendered as empty");
        assert.match(table, /请点击「刷新」重试/, "failure offers retry guidance");
      } catch (err) {
        failure = err;
      }
      try {
        if (intercept) await intercept.stop();
        await adminAPI(page, targetPath, { method: "PUT", body: { pool: S.pool } });
        await adminAPI(page, "/admin/data/pools/" + encodeURIComponent(tempPool), { method: "DELETE" })
          .catch((err) => {
            if (!/HTTP 404/.test(err.message)) throw err;
          });
        if (!(await page.eval(`document.getElementById("page-targets").classList.contains("active")`))) {
          await goto(page, "targets");
        }
        if (await page.eval(`document.getElementById("targetsPoolsPanel").hidden`)) {
          await page.eval(`__sim.click("#targetsPoolsTab")`);
          await page.waitFor(`!document.getElementById("targetsPoolsPanel").hidden`,
            { what: "pools tab active for cleanup" });
        }
        await page.eval(`__sim.click("#refreshPoolsBtn")`);
        await waitPoolMembers(page, S.pool, [S.tgtOpenAI]);
        await page.waitFor(`!document.querySelector('[data-edit-pool=${js(tempPool)}]')`,
          { what: "temporary pool removed", timeoutMs: 10000 });
      } catch (err) {
        if (!failure) failure = err;
      }
      if (failure) throw failure;
    },
  },
  {
    name: "trace-list-and-detail",
    async run({ page }) {
      await page.waitFor(`getComputedStyle(document.getElementById("nav-trace")).display !== "none"`, { what: "trace nav visible" });
      await goto(page, "trace");
      await page.eval(`__sim.click("#refreshTraceBtn")`);
      await page.waitFor(`document.querySelector("#traceTableBody button[data-trace-id]")`, { what: "trace rows", timeoutMs: 10000 });
      await page.eval(`__sim.click("#traceTableBody button[data-trace-id]")`);
      await page.waitFor(`document.getElementById("traceDetailModal").style.display === "flex" && __sim.text("#traceDetailBody").length > 20`, { what: "trace detail" });
      await page.eval(`closeTraceDetail(), true`);
    },
  },
  {
    name: "quota-redesign-people-formula-status-filters",
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="people"]')`);
      await page.waitFor(`__sim.text("#quotaSummaryBody").includes(${js(S.client)})`, { what: "client in quota people", timeoutMs: 15000 });
      const formula = await page.eval(`__sim.text("#quotaFormula")`);
      for (const text of ["模型组基数", "职级系数", "个人系数", "扣费系数", "不是共享额度池"])
        assert.ok(formula.includes(text), `quota formula explains ${text}`);
      const billing = await page.eval(`__sim.text("#quotaBillingBasis")`);
      for (const text of ["官方", "目录默认价", "不估算 token", "模型组扣费系数"])
        assert.ok(billing.includes(text), `quota billing basis keeps ${text}`);
      await page.waitFor(`(() => {
        const select = document.getElementById("quotaSummaryStatusFilter");
        return select.options.length > 1 && [...select.options].every(o => /\\(\\d+\\)$/.test(o.textContent.trim()));
      })()`, { what: "status counts in existing options", timeoutMs: 10000 });

      const picked = await page.eval(`(() => {
        const select = document.getElementById("quotaSummaryStatusFilter");
        const option = [...select.options].find(o => o.value && Number((o.textContent.match(/\\((\\d+)\\)$/ ) || [])[1]) > 0);
        if (!option) return null;
        const label = option.textContent.replace(/\\s*\\(\\d+\\)$/, "");
        select.value = option.value;
        select.dispatchEvent(new Event("change", { bubbles: true }));
        return { value: option.value, label };
      })()`);
      assert.ok(picked, "one quota status has a positive count");
      await page.waitFor(`document.querySelectorAll("#quotaSummaryBody tr[data-quota-person]").length > 0`, { what: "status-filtered quota rows" });
      const badges = await page.eval(`[...document.querySelectorAll("#quotaSummaryBody tr[data-quota-person]")].map(row => {
        const cells = row.querySelectorAll("td");
        return cells[cells.length - 2] ? cells[cells.length - 2].textContent.trim() : "";
      })`);
      assert.ok(badges.length > 0 && badges.every(text => text === picked.label),
        `status filter ${picked.label} showed ${JSON.stringify(badges)}`);
      await page.eval(`(() => { const s = document.getElementById("quotaSummaryStatusFilter"); s.value = ""; s.dispatchEvent(new Event("change", {bubbles:true})); })()`);

      await page.eval(`__sim.fill("#quotaSearch", ${js(S.client)})`);
      await page.waitFor(`document.querySelectorAll("#quotaSummaryBody tr[data-quota-person]").length === 1 && __sim.text("#quotaSummaryStatusFilter").includes("全部状态 (1)")`,
        { what: "search narrows rows and status counts" });
      await page.eval(`__sim.fill("#quotaSearch", "")`);

      await page.waitFor(`[...document.querySelectorAll("#quotaSummaryProjectFilter option")].some(o => o.value === ${js(S.project)})`, { what: "project filter option" });
      await page.eval(`__sim.fill("#quotaSummaryProjectFilter", ${js(S.project)})`);
      await page.waitFor(`document.querySelectorAll("#quotaSummaryBody tr[data-quota-person]").length === 1`, { what: "project filter keeps the client" });
      await page.eval(`__sim.fill("#quotaSummaryProjectFilter", "")`);

      await page.waitFor(`[...document.querySelectorAll("#quotaSummaryJobFilter option")].some(o => o.value === ${js(S.level)})`, { what: "job filter option" });
      await page.eval(`__sim.fill("#quotaSummaryJobFilter", ${js(S.level)})`);
      await page.waitFor(`document.querySelectorAll("#quotaSummaryBody tr[data-quota-person]").length === 1`, { what: "job filter keeps the client" });
      await page.eval(`__sim.fill("#quotaSummaryJobFilter", "")`);

      await page.waitFor(`/^[0-9]/.test(__sim.text("#quotaSummaryBody tr[data-quota-person] .quota-token-cell")) && __sim.text("#quotaSummaryBody tr[data-quota-person] .quota-cost-cell").startsWith("$")`,
        { what: "quota usage cells are available", timeoutMs: 15000 });
      const tokenCell = await page.eval(`__sim.text("#quotaSummaryBody tr[data-quota-person] .quota-token-cell")`);
      assert.match(tokenCell, /^[0-9]/, `quota token cell is numeric: ${tokenCell}`);
      const costCell = await page.eval(`__sim.text("#quotaSummaryBody tr[data-quota-person] .quota-cost-cell")`);
      assert.match(costCell, /^\$/, `quota cost cell is an amount: ${costCell}`);
      assert.ok(await page.eval(`!!document.querySelector("#quotaSummaryTableHead th[title*='estimated_cost']")`),
        "estimated cost header discloses the model coefficient");
    },
  },
  {
    name: "quota-redesign-detail-and-drawer",
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="people"]')`);
      await page.waitFor(`document.querySelector('[data-edit-quota=${js(S.client)}]')`, { what: "quota edit action", timeoutMs: 15000 });
      await page.eval(`__sim.click('[data-edit-quota=${js(S.client)}]')`);
      await page.waitFor(`__sim.open("quotaEditDrawer")`, { what: "quota edit drawer" });
      assert.equal(await page.eval(`__sim.$("#quotaEditJobLevel").value`), S.level);
      await page.waitFor(`[...document.querySelectorAll("#quotaEditGroupPicker option")].some(o => o.value === ${js(S.group)})`,
        { what: "model group in quota drawer" });
      await page.eval(`__sim.fill("#quotaEditGroupPicker", ${js(S.group)})`);
      await page.waitFor(`document.querySelector("#quotaEditGroupEditor [data-edit-group-index='0']")`, { what: "selected group card" });
      await page.eval(`__sim.click("#saveQuotaEditBtn")`);
      await expectToast(page, "额度已更新");
      await page.waitFor(`!__sim.open("quotaEditDrawer")`, { what: "quota drawer closed after save" });
      // The save handler closes the drawer before reloading the summary. Wait
      // for the reloaded state that carries the saved group; the row still on
      // screen at this point is the pre-save one, which has no reset time.
      await page.waitFor(`(() => {
        const item = (AdminApp.state.quotaSummary || []).find(x => (x.client || {}).name === ${js(S.client)});
        const group = item && item.status && item.status.groups && item.status.groups[${js(S.group)}];
        return !!group && !!group.quotas;
      })()`, { what: "reloaded quota summary contains the saved group", timeoutMs: 15000 });
      await page.eval(`__sim.click('[data-expand-quota=${js(S.client)}]')`);
      await page.waitFor(`document.querySelector('[data-quota-detail=${js(S.client)}]')`, { what: "expanded group details" });
      await page.waitFor(`document.querySelector('[data-quota-detail=${js(S.client)}] .quota-reset')`, { what: "group reset time" });
      const detail = await page.eval(`__sim.text('[data-quota-detail=${js(S.client)}]')`);
      assert.match(detail, /独立限额组/, "detail explains independent limit groups");
      assert.match(detail, /不是共享额度池/, "detail denies a shared pool");
      assert.match(detail, /下次重置/, "detail shows the group reset time");
      assert.match(detail, /本地时间/, "reset time is labelled local");
      assert.doesNotMatch(detail, /0001-01-01/, "zero Go time is not shown");
      const fit = await page.eval(`(() => {
        const box = document.querySelector('[data-quota-detail=${js(S.client)}] .quota-detail-scroll');
        return {scroll: box.scrollWidth, client: box.clientWidth};
      })()`);
      assert.ok(fit.scroll <= fit.client + 1,
        `quota detail table overflows its cell: ${fit.scroll}/${fit.client}`);
      await settleQuotaShot(page);
    },
  },
  {
    name: "quota-redesign-tabs-and-drawer",
    async run({ page }) {
      await goto(page, "quota");
      const tab = async (name) => {
        await page.eval(`__sim.click('.quota-tab[data-quota-tab=${js(name)}]')`);
        await page.waitFor(`document.getElementById("quota-tab-${name}").classList.contains("active")`, { what: `quota tab ${name}` });
      };
      await tab("policy");
      await page.waitFor(`__sim.text("#modelGroupsBody").includes(${js(S.group)})`, { what: "policy tab group row" });
      await tab("batch");
      assert.ok(await page.eval(`getComputedStyle(document.getElementById("syncQuotaBtn")).display !== "none"`), "batch sync is visible to admin");
      await tab("settings");
      await page.waitFor(`__sim.text("#jobLevelsBody").includes(${js(S.level)})`, { what: "settings tab job level row" });
      await tab("people");
      await page.waitFor(`document.querySelector('[data-edit-quota=${js(S.client)}]')`, { what: "people tab quota row" });
      await page.eval(`__sim.click('[data-edit-quota=${js(S.client)}]')`);
      await page.waitFor(`__sim.open("quotaEditDrawer")`, { what: "quota drawer opens from people tab" });
      await page.eval(`closeModal("quotaEditDrawer"), true`);
      await page.waitFor(`!__sim.open("quotaEditDrawer")`, { what: "quota drawer closes" });
      await settleQuotaShot(page);
    },
  },
  {
    name: "quota-redesign-desktop-dark",
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`(() => {
        if (!document.documentElement.classList.contains("dark")) document.getElementById("themeToggle").click();
        return true;
      })()`);
      await page.waitFor(`document.documentElement.classList.contains("dark")`, { what: "dark theme applied" });
      await page.waitFor(`__sim.text("#quotaFormula").includes("模型组基数")`, { what: "dark quota formula" });
      const shot = await page.eval(`({width: document.documentElement.scrollWidth, client: document.documentElement.clientWidth})`);
      assert.ok(shot.width <= shot.client + 1, `dark quota desktop overflows: ${shot.width}/${shot.client}`);
      await settleQuotaShot(page);
    },
  },
  {
    name: "quota-redesign-mobile-390-light",
    async run({ page }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
      try {
        await page.eval(`(() => {
          if (document.documentElement.classList.contains("dark")) document.getElementById("themeToggle").click();
          return true;
        })()`);
        await page.waitFor(`!document.documentElement.classList.contains("dark")`, { what: "light theme for mobile screenshot" });
        await page.waitFor(`document.querySelector('[data-expand-quota=${js(S.client)}]')`, { what: "mobile quota row", timeoutMs: 10000 });
        const width = await page.eval(`({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth})`);
        assert.ok(width.scroll <= width.client + 1, `mobile quota overflows: ${width.scroll}/${width.client}`);
        assert.ok(await page.eval(`getComputedStyle(document.getElementById("quotaFormula")).display !== "none"`), "mobile formula is visible");
        await scrollQuotaTableIntoView(page);
        await settleQuotaShot(page);
      } catch (err) {
        await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
        throw err;
      }
    },
  },
  {
    name: "quota-redesign-mobile-390-dark",
    async run({ page }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
      try {
        await page.eval(`(() => {
          if (!document.documentElement.classList.contains("dark")) document.getElementById("themeToggle").click();
          return true;
        })()`);
        await page.waitFor(`document.documentElement.classList.contains("dark")`, { what: "mobile dark theme applied" });
        await page.waitFor(`document.querySelector('[data-expand-quota=${js(S.client)}]')`, { what: "mobile dark quota row", timeoutMs: 10000 });
        const width = await page.eval(`({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth})`);
        assert.ok(width.scroll <= width.client + 1, `mobile dark quota overflows: ${width.scroll}/${width.client}`);
        assert.ok(await page.eval(`getComputedStyle(document.getElementById("quotaFormula")).display !== "none"`), "mobile dark formula is visible");
        await scrollQuotaTableIntoView(page);
        await settleQuotaShot(page);
      } catch (err) {
        await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
        throw err;
      }
    },
  },
  {
    name: "quota-redesign-restore-desktop-light",
    async run({ page }) {
      await page.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
      await page.eval(`(() => {
        if (document.documentElement.classList.contains("dark")) document.getElementById("themeToggle").click();
        return true;
      })()`);
      await page.waitFor(`!document.documentElement.classList.contains("dark")`, { what: "light theme restored" });
      await page.waitFor(`__sim.text("#quotaFormula").includes("模型组基数")`, { what: "light quota formula" });
      await page.eval(`document.querySelector(".content").scrollTop = 0, true`);
      await settleQuotaShot(page);
    },
  },
  {
    name: "delete-client-with-confirm",
    async run({ page, stack, clientCall }) {
      await goto(page, "clients");
      await page.waitFor(`document.querySelector('[data-del-client=${js(S.client)}]')`, { what: "delete client button" });
      await page.eval(`__sim.click('[data-del-client=${js(S.client)}]')`);
      await confirmModal(page);
      await expectToast(page, "已删除客户端");
      await page.waitFor(`!document.querySelector('[data-del-client=${js(S.client)}]')`,
        { what: "deleted client row gone", timeoutMs: 10000 });
      const r = await clientCall(stack.base, S.clientKey, "/v1/chat/completions", { model: S.aliasGLM, messages: [] });
      assert.equal(r.status, 401, `deleted client key still accepted: ${r.status}`);
    },
  },
];

async function createTarget(page, t) {
  await goto(page, "targets");
  await page.eval(`__sim.click("#newTargetBtn")`);
  await page.waitFor(`__sim.open("targetModal")`, { what: "target modal" });
  await page.eval(`__sim.fill("#tgtType", ${js(t.type)}) && __sim.fill("#tgtName", ${js(t.name)}) && __sim.fill("#tgtEndpoint", ${js(t.endpoint)})`);
  if (t.pool) await page.eval(`__sim.fill("#tgtPool", ${js(t.pool)})`);
  assert.ok(t.usageAdapter, "target fixture must choose its billing type explicitly");
  await page.eval(`__sim.fill("#tgtUsageAdapter", ${js(t.usageAdapter)})`);
  for (const key of t.keys) {
    await page.eval(`__sim.fill("#tgtNewKeyInput", ${js(key)}) && __sim.click("#addKeyBtn")`);
  }
  await page.waitFor(`document.querySelectorAll("#keyCardList > *").length >= ${t.keys.length}`, { what: "key cards" });
  await page.eval(`(() => { const row = document.querySelector("#model-mappings-container .model-mapping-row");
    __sim.fill("#model-mappings-container .model-mapping-row .upstream-input", ${js(t.upstream)});
    __sim.fill("#model-mappings-container .model-mapping-row .fallback-input", ${js(t.alias)}); return !!row; })()`);
  if (t.preferWS) {
    await page.waitFor(`getComputedStyle(__sim.$("#tgtPreferWSGroup")).display !== "none"`, { what: "prefer_websocket toggle visible" });
    await page.eval(`__sim.check("#tgtPreferWS", true)`);
  }
  await page.eval(`__sim.click("#saveTargetBtn")`);
  await expectToast(page, "目标已创建");
  await page.waitFor(`document.querySelector('#targetsTableBody td.target-text[title=${js(t.name)}]')`, { what: `target row ${t.name}` });
}

module.exports = { list, S };
