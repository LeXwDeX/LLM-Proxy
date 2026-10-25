"use strict";
// Admin UI simulation flows, driven through the real pages (see
// admin_ui_sim.cjs). Every create/edit goes through the visible form and its
// save button; client traffic goes through the proxy to fake upstreams.
const assert = require("node:assert/strict");

const S = {
  pool: "sim-ui-pool",
  level: "sim-ui-level",
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

async function fakeControl(base, body) {
  const r = await fetch(base + "/_control", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  assert.equal(r.status, 200, "fake /_control");
}

async function fakeRequests(base) {
  return (await fetch(base + "/_requests")).json();
}

const list = [
  {
    name: "login", critical: true,
    async run({ page, stack, ADMIN_PASSWORD }) {
      await install(page);
      await page.navigate(stack.base + "/login");
      await page.eval(`__sim.fill('input[name=username]', 'admin') && __sim.fill('input[name=password]', ${js(ADMIN_PASSWORD)})`);
      const loaded = page.waitEvent("Page.loadEventFired");
      await page.eval(`__sim.click('form.card button[type=submit]')`);
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
      await page.eval(`__sim.click("#openPoolsModalBtn")`);
      await page.waitFor(`__sim.open("poolsModal")`);
      await page.eval(`__sim.click("#newPoolBtn")`);
      await page.waitFor(`__sim.open("poolModal")`);
      await page.eval(`__sim.fill("#poolName", ${js(S.pool)}) && __sim.fill("#poolDescription", "UI simulation pool")`);
      await page.eval(`__sim.click("#savePoolBtn")`);
      await expectToast(page, "部署池已创建");
      await page.waitFor(`[...document.querySelectorAll("#poolsTableBody td.pool-name")].some(td => td.textContent.includes(${js(S.pool)}))`, { what: "pool row" });
      await page.eval(`closeModal("poolsModal"), true`);
    },
  },
  {
    name: "create-job-level", critical: true,
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="settings"]')`);
      await page.eval(`__sim.click("#newJobLevelBtn")`);
      await page.waitFor(`__sim.open("jobLevelModal")`);
      await page.eval(`__sim.fill("#jobLevelName", ${js(S.level)}) && __sim.fill("#jobLevel-daily", "0") && __sim.fill("#jobLevel-weekly", "0") && __sim.fill("#jobLevel-monthly", "0")`);
      await page.eval(`__sim.click("#saveJobLevelBtn")`);
      await expectToast(page, "职级已保存");
      await page.waitFor(`__sim.text("#jobLevelsBody").includes(${js(S.level)})`, { what: "job level row" });
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
      await page.waitFor(`__sim.open("clientModal") && [...document.querySelectorAll("#clientJobLevel option")].some(o => o.value === ${js(S.level)})`,
        { what: "client modal with job levels" });
      await page.eval(`__sim.fill("#clientName", ${js(S.client)}) && __sim.fill("#clientAccessKey", ${js(S.clientKey)}) && __sim.fill("#clientJobLevel", ${js(S.level)})`);
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
      await page.eval(`__sim.click("#refreshOverviewBtn")`);
      await page.waitFor(`Number(__sim.text("#ovTotalReqs").replace(/[^0-9]/g, "")) >= 7`, { what: "#ovTotalReqs >= 7", timeoutMs: 15000 });
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
      assert.match(range.status, /官方基础价/);
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
      await expectToast(page, "测试成功");
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
    name: "quota-people-lists-client",
    async run({ page }) {
      await goto(page, "quota");
      await page.eval(`__sim.click('.quota-tab[data-quota-tab="people"]')`);
      await page.waitFor(`__sim.text("#quotaSummaryBody").includes(${js(S.client)})`, { what: "client in quota people", timeoutMs: 10000 });
    },
  },
  {
    name: "delete-client-with-confirm",
    async run({ page, stack, clientCall }) {
      await goto(page, "clients");
      await page.waitFor(`document.querySelector('[data-del-client=${js(S.client)}]')`, { what: "delete client button" });
      await page.eval(`__sim.click('[data-del-client=${js(S.client)}]')`);
      await confirmModal(page);
      await expectToast(page, "删除");
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
