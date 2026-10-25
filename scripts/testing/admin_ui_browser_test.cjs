"use strict";
// Optional browser QA: use installed Playwright/Chromium against a local mock server.
// No credentials or upstream model calls are used. The Node VM fixture is the CI baseline.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const http = require("node:http");
const { chromium } = require("playwright");
const { schemaFixture } = require("./admin_ui_test.cjs");
const root = path.resolve(__dirname, "../..");
const client = {
  name: "fixture-client",
  access_key: "fixture-only-key",
  allowed_targets: ["fixture-target"],
  allowed_pools: ["fixture-pool"],
  job_level: "fixture-level",
  project_group: "fixture-project",
  quota_groups: [{ name: "fixture-group", coefficient: 1 }],
};
const target = {
  name: "fixture-target",
  endpoint_type: "provider-new",
  usage_adapter: "standard",
  endpoint: "https://future.example",
  pool: "fixture-pool",
  model_prefix: "Vendor/",
  model_mappings: [
    {
      upstream: "Qwen-MixedCase-Long-Model-Name-To-Show-Full-Tooltip",
      fallback: "qwen-max",
    },
    { upstream: "Second-MixedCase-Model", fallback: "glm-flash" },
  ],
  api_keys: [],
  key_statuses: [],
};
const group = {
  name: "fixture-group",
  cost_coefficient: 1,
  daily_usd: 100,
  weekly_usd: 300,
  monthly_usd: 500,
  bindings: [{ target: "removed-target", model: "<unsafe-model>" }],
};
function aggregateFixture() {
  const now = new Date();
  const from = new Date(now.getTime() - 3600000);
  const totals = { requests: 1, input_tokens: 60, output_tokens: 20, estimated_cost: 0 };
  return {
    from: from.toISOString(), to: now.toISOString(), group_by: "day", totals, buckets: [],
    by_client: [{ key: client.name, ...totals }],
    by_model: [{ key: "fixture-model", ...totals }],
  };
}
function activityFixture() {
  const end = Math.floor(Date.now() / 10000) * 10000;
  const iso = ms => new Date(ms).toISOString();
  const series = Array.from({length:30}, (_, i) => ({
    from:iso(end - (30-i)*10000), to:iso(end-(29-i)*10000), partial:false,
    active_requests_peak:2, input_tokens:i===29?30:0, output_tokens:i===29?12:0,
    total_tokens:i===29?42:0, incomplete_usage_count:0,
  }));
  const totals={input_tokens:30,output_tokens:12,total_tokens:42,incomplete_usage_count:0};
  return {generated_at:iso(Date.now()),window:{from:series[0].from,to:iso(end),step_seconds:10},
    runtime:{since:iso(end-300000),complete:true,active_requests:2,active_clients:1,websocket_sessions:1},
    totals,series,clients:[{name:client.name,configured:true,active_requests:2,websocket_sessions:1,totals,series}]};
}
async function main() {
  let role = "admin",
    schemaOffline = false;
  const mutations = [],
    unknownRequests = [],
    errors = [],
    requests = [];
  const json = (res, code, data) => {
    res.writeHead(code, { "Content-Type": "application/json" });
    res.end(JSON.stringify(data));
  };
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, "http://localhost"),
      endpoint = url.pathname;
    requests.push(endpoint);
    if (endpoint === "/admin" || endpoint === "/admin/ui") {
      res.writeHead(200, { "Content-Type": "text/html" });
      res.end(fs.readFileSync(path.join(root, "internal/admin/ui/index.html")));
      return;
    }
    if (/^\/admin\/ui\/js\/[^/]+\.js$/.test(endpoint)) {
      res.writeHead(200, { "Content-Type": "text/javascript" });
      res.end(
        fs.readFileSync(
          path.join(root, "internal/admin/ui/js", path.basename(endpoint)),
        ),
      );
      return;
    }
    if (endpoint === "/favicon.ico") {
      res.writeHead(204);
      res.end();
      return;
    }
    if (req.method !== "GET") {
      let body = "";
      for await (const part of req) body += part;
      mutations.push({
        endpoint,
        method: req.method,
        payload: body ? JSON.parse(body) : {},
      });
      json(res, 200, {});
      return;
    }
    if (endpoint === "/admin/data/schema") {
      json(
        res,
        schemaOffline ? 404 : 200,
        schemaOffline
          ? { error: "fixture schema unavailable" }
          : schemaFixture(),
      );
      return;
    }
    const fixtures = {
      "/admin/api/me": { username: "fixture-admin", role },
      "/admin/api/overview": {},
      "/admin/api/activity": activityFixture(),
      "/admin/data/usage/aggregate": aggregateFixture(),
      "/admin/data/targets": { targets: [target, {...target, name: "z-fixture-target", model_prefix: "Other/"}] },
      "/admin/data/pools": {
        pools: [{ name: "fixture-pool", display_name: "Fixture-POOL", description: "Fixture pool" }],
      },
      "/admin/metrics": {
        uptime_seconds: 60,
        active_requests: 2,
        requests: { total: 1 },
        persistence: { usage_async_pending: 0, usage_async_deferred: 0, request_stats_dropped: 0 },
        targets_runtime: [
          {
            name: "fixture-target",
            phase: "ready",
            in_flight: 0,
            max_concurrency: 4,
          },
        ],
      },
      "/admin/data/clients": { clients: [client] },
      "/admin/data/model-groups": { model_groups: [group] },
      "/admin/data/job-levels": {
        job_levels: [
          {
            name: "fixture-level",
            coefficient_daily: 1,
            coefficient_weekly: 2,
            coefficient_monthly: 3,
          },
        ],
      },
      "/admin/data/project-groups": { project_groups: ["fixture-project"] },
      "/admin/data/catalog": { models: [] },
      "/admin/data/pricing-templates": {
        templates: [
          {
            id: "fixture-price",
            target: "fixture-target",
            model: target.model_mappings[0].upstream,
            mode: "flat",
            start: "0001-01-01T00:00:00Z",
            timezone: "+06:00",
            base_rates: {
              input_per_1m_tokens: 1.25,
              output_per_1m_tokens: 2.5,
            },
          },
          {
            id: "fixture-price-deleted",
            target: "removed-price-target",
            model: "DeletedMixedCase",
            mode: "flat",
            start: "0001-01-01T00:00:00Z",
            timezone: "+06:00",
            base_rates: {
              input_per_1m_tokens: 3.75,
              output_per_1m_tokens: 4.5,
            },
          },
        ],
      },
      "/admin/data/usage/aggregate": {
        by_client: [],
        by_model_group: [],
        rows: [],
      },
      "/admin/data/audit": { events: [] },
      "/admin/data/trace": { stats: {}, records: [] },
      "/admin/data/users": {
        users: [{ username: "fixture-admin", role: "admin" }],
      },
      "/admin/data/quota/summary": {
        clients: [
          {
            client,
            status: {
              job_level: "fixture-level",
              groups: {
                "fixture-group": {
                  in_plan: true,
                  quotas: {},
                  base_limits: { daily: 100 },
                },
              },
              quotas: {},
            },
          },
        ],
      },
    };
    if (endpoint.startsWith("/admin/data/catalog/")) {
      json(res, 200, { models: [] });
      return;
    }
    if (!Object.hasOwn(fixtures, endpoint)) {
      unknownRequests.push(endpoint);
      json(res, 404, { error: "unknown fixture endpoint" });
      return;
    }
    json(res, 200, fixtures[endpoint]);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const base = `http://127.0.0.1:${server.address().port}/admin`;
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({
      viewport: { width: 1440, height: 1050 },
    });
    page.on("pageerror", (error) => errors.push(error.message));
    await page.goto(base);
    await page.waitForFunction(() => window.AdminApp?.state.ready === true);
    await page.waitForFunction(() => document.getElementById("activityTotalTokens").textContent === "42");
    assert.equal(await page.locator("#activityActiveRequests").textContent(),"2","homepage uses current activity API counters");
    for (const name of [
      "overview",
      "targets",
      "clients",
      "model-costs",
      "quota",
      "model-catalog",
      "usage",
      "audit",
      "users",
      "trace",
    ]) {
      await page.locator(`.sidebar-nav-item[data-page="${name}"]`).click();
      await page.waitForFunction(
        (name) =>
          document.querySelector(`#page-${name}`).classList.contains("active"),
        name,
      );
    }
    await page.locator('[data-page="targets"]').click();
    await page.waitForLoadState("networkidle");
    await page.locator("#targetsTableBody [data-edit-tgt]").first().waitFor();
    // Pointer coordinates must cross the tooltip region and still hit the lower badge.
    await page.evaluate(() => {
      document.querySelector(".target-model-list").style.flexDirection =
        "column";
    });
    const badges = page.locator(".target-model-badge");
    await badges.nth(0).scrollIntoViewIfNeeded();
    await page.waitForTimeout(150);
    const first = await badges.nth(0).boundingBox(),
      second = await badges.nth(1).boundingBox();
    await page.mouse.move(
      first.x + Math.min(12, first.width / 2),
      first.y + first.height / 2,
    );
    await page
      .locator("#targetModelHint")
      .waitFor({ state: "visible", timeout: 2000 });
    assert.match(
      await page.locator("#targetModelHint").textContent(),
      /Qwen-MixedCase/,
    );
    assert.equal(
      await page
        .locator("#targetModelHint")
        .evaluate((element) => getComputedStyle(element).pointerEvents),
      "none",
    );
    await page.mouse.move(second.x + 12, second.y + second.height / 2);
    assert.match(
      await page.locator("#targetModelHint").textContent(),
      /Second-MixedCase/,
    );
    await page.mouse.click(second.x + 12, second.y + second.height / 2);
    assert.equal(
      await page
        .locator("#targetModelHint")
        .evaluate((element) => element.hidden),
      false,
    );
    await page.locator("#newTargetBtn").click();
    await page.locator("#targetModal.open").waitFor();
    assert.equal(await page.locator("#tgtType").inputValue(), "provider-new");
    assert.equal(await page.locator("#tgtUsageAdapter").inputValue(), "");
    assert.deepEqual(await page.locator("#tgtUsageAdapter option").evaluateAll((options) => options.map((o) => o.value)),
      ["", "standard", "glm", "deepseek", "qwen", "kimi"]);
    await page.locator("#tgtName").fill("fixture-new-target");
    await page.locator("#tgtEndpoint").fill("https://fixture.example");
    const beforeTargetWrites = mutations.length;
    await page.locator("#saveTargetBtn").click();
    await page.locator("#tgtUsageAdapterError").waitFor({ state: "visible" });
    assert.match(await page.locator("#tgtUsageAdapterError").textContent(), /请选择计费类型/);
    assert.equal(mutations.length, beforeTargetWrites, "missing choice sends no request");
    assert.equal(await page.locator("#tgtUsageAdapter").getAttribute("aria-invalid"), "true");
    await page.locator("#tgtUsageAdapter").selectOption("standard");
    assert.equal(await page.locator("#tgtUsageAdapterError").isVisible(), false);
    await page.locator("#saveTargetBtn").click();
    await page.locator("#targetModal.open").waitFor({ state: "hidden" });
    assert.equal(mutations.at(-1).payload.usage_adapter, "standard");
    assert.equal(mutations.length, 1, "new target submits exactly once");
    mutations.length = 0;
    await page.locator("#newTargetBtn").click();
    await page.locator("#targetModal.open").waitFor();
    assert.equal(await page.locator("#tgtUsageAdapter").inputValue(), "", "next new target resets choice");
    assert.equal(
      await page.locator("#tgtEndpoint").getAttribute("placeholder"),
      "https://future.example",
    );
    assert.match(
      await page.locator("#tgtCapabilities").textContent(),
      /新协议.*新请求/,
    );
    await page.locator("#tgtType").selectOption("provider-other");
    assert.equal(await page.locator("#tgtAuthModeGroup").isVisible(), false);
    await page
      .locator('#targetModal [data-close="targetModal"]')
      .first()
      .click();
    await page.locator("#targetsTableBody [data-edit-tgt]").first().click();
    await page.locator("#targetModal.open").waitFor();
    assert.equal(await page.locator("#tgtName").inputValue(), "fixture-target");
    assert.equal(await page.locator("#tgtUsageAdapter").inputValue(), "standard", "editing preserves choice");
    assert.equal(
      await page.locator(".upstream-input").first().inputValue(),
      target.model_mappings[0].upstream,
    );
    assert.equal(await page.locator("#tgtModelPrefix").inputValue(), "Vendor/");
    await page.locator("#tgtModelPrefix").fill("Vendor-New/");
    assert.match(await page.locator("#tgtModelPrefixPreview").textContent(), /Vendor-New\/Qwen-MixedCase/);
    await page.locator("#saveTargetBtn").click();
    await page.locator("#targetModal.open").waitFor({state:"hidden"});
    assert.equal(mutations.length, 1);
    assert.equal(mutations[0].payload.model_prefix, "Vendor-New/");
    mutations.length = 0;
    await page.locator("#openPoolsModalBtn").click();
    await page.locator("#poolsModal.open").waitFor();
    await page.locator("#newPoolBtn").click();
    await page.locator("#poolModal.open").waitFor();
    await page.locator('#poolModal [data-close="poolModal"]').first().click();
    await page.locator('[data-edit-pool="fixture-pool"]').click();
    assert.equal(await page.locator("#poolName").inputValue(), "Fixture-POOL");
    await page.locator("#poolName").fill("FIXTURE-Pool");
    await page.locator("#savePoolBtn").click();
    await page.locator("#poolModal.open").waitFor({state:"hidden"});
    assert.equal(mutations.length, 1);
    assert.equal(mutations[0].endpoint, "/admin/data/pools/fixture-pool");
    assert.equal(mutations[0].payload.display_name, "FIXTURE-Pool");
    mutations.length = 0;
    await page.locator('#poolsModal [data-close="poolsModal"]').first().click();
    await page.locator('[data-page="clients"]').click();
    await page.locator("#newClientBtn").click();
    await page.locator("#clientModal.open").waitFor();
    assert.equal(await page.locator("#clientJobLevel option").count(), 2);
    await page
      .locator('#clientModal [data-close="clientModal"]')
      .first()
      .click();
    await page.locator("[data-edit-client]").first().click();
    await page.locator("#clientModal.open").waitFor();
    assert.equal(
      await page.locator("#clientName").inputValue(),
      "fixture-client",
    );
    await page
      .locator('#clientModal [data-close="clientModal"]')
      .first()
      .click();
    await page.locator('[data-page="model-costs"]').click();
    await page.waitForLoadState("networkidle");
    assert.equal(
      await page.evaluate(() => window.AdminApp.state.pricingTemplates.length),
      2,
      "pricing page loads the real templates envelope",
    );
    await page.locator("[data-expand-pricing]").first().click();
    await page.locator('[data-edit-pricing="fixture-price"]').click();
    await page.locator("#modelCostModal.open").waitFor();
    assert.equal(await page.locator("#ptInput").inputValue(), "1.25");
    assert.equal(
      await page.locator("#ptModel").inputValue(),
      target.model_mappings[0].upstream,
    );
    assert.equal(await page.locator("#ptTarget").isDisabled(), true);
    assert.equal(await page.locator("#ptTargetSearch").isDisabled(), true);
    assert.equal(await page.locator("#ptModelSearch").isDisabled(), true);
    assert.ok(await page.locator("#ptTargetChoices input:disabled:checked").count());
    assert.match(await page.locator("#ptSelectionPreview").textContent(), /fixture-target \/ Qwen-MixedCase/);
    await page
      .locator('#modelCostModal [data-close="modelCostModal"]')
      .first()
      .click();
    await page
      .locator("[data-expand-pricing]")
      .filter({ hasText: "展开复制" })
      .click();
    await page.locator('[data-copy-pricing="fixture-price-deleted"]').click();
    await page.locator("#modelCostModal.open").waitFor();
    assert.equal(await page.locator("#ptInput").inputValue(), "3.75");
    assert.equal(await page.locator("#ptTarget").isDisabled(), false);
    await page
      .locator('#modelCostModal [data-close="modelCostModal"]')
      .first()
      .click();
    await page.locator("#newModelCostBtn").click();
    await page.locator("#modelCostModal.open").waitFor();
    assert.equal(await page.locator("#ptTimezone").inputValue(), "+06:00");
    await page.locator("#ptMode").selectOption("schedule");
    assert.equal(await page.locator("#ptPeriods").isVisible(), true);
    await page
      .locator('#modelCostModal [data-close="modelCostModal"]')
      .first()
      .click();
    await page.locator("#newModelCostBtn").click();
    await page.locator("#modelCostModal.open").waitFor();
    const targetCheckbox = value => page.locator('#ptTargetChoices input[value="'+value+'"]');
    const modelCheckbox = value => page.locator('#ptModelChoices input[value="'+value+'"]');
    assert.equal(await page.locator("#ptTarget").isVisible(), false);
    await targetCheckbox("fixture-target").check();
    assert.equal(await targetCheckbox("").isChecked(), false, "specific target clears global fallback");
    await targetCheckbox("").check();
    assert.equal(await targetCheckbox("fixture-target").isChecked(), false, "global fallback clears specific targets");
    // Clicking the label and keyboard Space both operate the native checkbox.
    await targetCheckbox("fixture-target").locator("..").click();
    await targetCheckbox("z-fixture-target").focus();
    await page.keyboard.press("Space");
    assert.equal(await targetCheckbox("z-fixture-target").isChecked(), true);
    assert.equal(await targetCheckbox("z-fixture-target").evaluate(el=>el === document.activeElement), true, "focus survives checkbox redraw");
    await page.keyboard.press("Space");
    assert.equal(await targetCheckbox("z-fixture-target").isChecked(), false);
    await page.keyboard.press("Space");
    const selectedModels = target.model_mappings.map(mapping => mapping.upstream);
    for (const model of selectedModels) await modelCheckbox(model).check();
    assert.equal(await page.locator("#ptSelectionPreview li").count(),4);
    assert.equal(await page.locator("#ptSelectionCount").textContent(),"4 个组合");
    await page.locator("#ptTargetSearch").fill("no-such-target");
    assert.equal(await page.locator("#ptTargetChoices label:visible").count(),0);
    assert.match(await page.locator("#ptTargetEmpty").textContent(), /没有匹配项/);
    assert.equal(await page.locator("#ptSelectionPreview li").count(),4,"search preserves all selected combinations");
    await page.locator("#ptTargetSearch").fill("");
    await page.locator("#ptModelSearch").fill("second");
    assert.equal(await page.locator("#ptModelChoices label:visible").count(),1);
    assert.equal(await page.locator("#ptSelectionPreview li").count(),4);
    await page.locator("#ptModelSearch").fill("");
    await modelCheckbox(selectedModels[1]).uncheck();
    assert.equal(await page.locator("#ptSelectionPreview li").count(),2);
    await modelCheckbox(selectedModels[1]).check();
    assert.match(await page.locator("#ptScopeNotice").textContent(), /2 个 Target.*2 个模型/);
    for (const [id, value] of [["ptInput","2"],["ptOutput","4"],["ptCacheWrite","0"],["ptCacheRead","1"]]) await page.locator("#"+id).fill(value);
    await page.locator("#ptStart").fill("2026-11-01T00:00");
    const screenshotDir = process.env.LLMS_UI_SCREENSHOT_DIR || os.tmpdir();
    fs.mkdirSync(screenshotDir,{recursive:true});
    await page.locator('#modelCostModal .modal-body').evaluate(el=>{el.scrollTop=0;});
    await page.screenshot({path:path.join(screenshotDir,"pricing-selection-desktop.png"),animations:"disabled"});
    await page.setViewportSize({width:390,height:1000});
    await page.screenshot({path:path.join(screenshotDir,"pricing-selection-mobile.png"),animations:"disabled"});
    assert.equal(await page.locator("#modelCostModal .modal").evaluate(el => el.getBoundingClientRect().width <= window.innerWidth),true);
    assert.equal(await page.locator("#modelCostModal .modal-body").evaluate(el=>el.scrollWidth<=el.clientWidth),true,"pricing editor has no horizontal overflow");
    await page.setViewportSize({width:1440,height:1050});
    await page.locator("#saveModelCostBtn").click();
    await page.locator("#modelCostModal.open").waitFor({state:"hidden"});
    assert.equal(mutations.length, 1, "shared price writes once");
    assert.deepEqual(mutations[0].payload.targets,["fixture-target","z-fixture-target"]);
    assert.deepEqual(mutations[0].payload.models,selectedModels);
    mutations.length = 0;
    // Build conditional and calendar prices through real controls, without JSON editing.
    await page.locator("#newModelCostBtn").click();
    await page.locator("#modelCostModal.open").waitFor();
    await targetCheckbox("fixture-target").check();
    await modelCheckbox(selectedModels[0]).check();
    assert.equal(await modelCheckbox(selectedModels[0]).isChecked(),true,"model selected for advanced creation");
    for (const [id,value] of [["ptInput","1"],["ptOutput","2"],["ptCacheWrite","0"],["ptCacheRead","1"],["ptStart","2029-01-01T00:00"]]) await page.locator("#"+id).fill(value);
    await page.locator("#ptMode").selectOption("schedule");
    await page.locator("#ptAdvancedPricing > summary").click();
    await page.locator("#ptVersion").fill("1");
    await page.locator("[data-pt-add-condition='base']").click();
    await page.locator("#pt-rule-base-0-id").fill("long-input");
    await page.locator("#pt-rule-base-0-min_input_tokens").fill("100");
    // The values and labels come from schema metadata, including future values.
    assert.ok(await page.locator("#pt-rule-base-0-tier option[value='future-tier']").count());
    await page.locator("#pt-rule-base-0-speed").selectOption("fast");
    await page.locator("#ptContextRules .pricing-context-rates > summary").click();
    await page.locator("#pt-rule-base-0-input_per_1m_tokens").fill("3");
    await page.locator("#pt-rule-base-0-image_input_per_1m_tokens").fill("0");
    await page.locator("#addPtPeriod").click();
    await page.locator("[data-pt-add-condition='period:0']").locator("..").locator("summary").first().click();
    await page.locator("[data-pt-add-condition='period:0']").click();
    await page.locator("#pt-rule-period-0-0-id").fill("peak-long");
    await page.locator("#pt-rule-period-0-0-min_input_tokens").fill("100");
    await page.locator("#addPtDateException").click();
    await page.locator("#pt-date-0-start").fill("2030-10-01");
    await page.locator("#pt-date-0-end").fill("2030-10-08");
    await page.locator("[data-pt-add-condition='date:0']").locator("..").locator("summary").first().click();
    await page.locator("[data-pt-add-condition='date:0']").click();
    await page.locator("#pt-rule-date-0-0-id").fill("holiday-fast");
    await page.locator("#pt-rule-date-0-0-tier").selectOption("fast");
    await page.setViewportSize({width:360,height:1000});
    await page.locator("#ptDateExceptionsGroup").scrollIntoViewIfNeeded();
    assert.equal(await page.locator("#modelCostModal .modal-body").evaluate(el=>el.scrollWidth<=el.clientWidth),true,"calendar and condition editor fits 360px");
    await page.screenshot({path:path.join(screenshotDir,"pricing-context-mobile.png"),animations:"disabled"});
    await page.setViewportSize({width:1440,height:1050});
    await page.locator("#ptAdvancedPricing").scrollIntoViewIfNeeded();
    await page.screenshot({path:path.join(screenshotDir,"pricing-context-desktop.png"),animations:"disabled"});
    await page.locator("#saveModelCostBtn").click();
    const pricingSaveMessage = await page.locator("#toastContainer").textContent();
    await page.locator("#modelCostModal.open").waitFor({state:"hidden",timeout:5000}).catch(async error => {
      throw new Error("advanced pricing save failed: " + pricingSaveMessage + " page errors: " + JSON.stringify(errors) + " selected: " + JSON.stringify(await page.evaluate(()=>({targets:window.AdminApp.modules.pricing.pricingSelected("ptTarget"),models:window.AdminApp.modules.pricing.pricingSelected("ptModel")}))), {cause:error});
    });
    assert.equal(mutations.length,1,"advanced creation writes once");
    const advancedPayload=mutations[0].payload;
    assert.equal(advancedPayload.version,1);
    assert.equal(advancedPayload.conditions[0].min_input_tokens,100);
    assert.equal(advancedPayload.conditions[0].speed,"fast");
    assert.equal(advancedPayload.conditions[0].rates.image_input_per_1m_tokens,0);
    assert.equal(advancedPayload.conditions[0].rates.explicit_cache_read_per_1m_tokens,null);
    assert.equal(advancedPayload.periods[0].conditions[0].id,"peak-long");
    assert.equal(advancedPayload.date_exceptions[0].end_date,"2030-10-08");
    assert.equal(advancedPayload.date_exceptions[0].conditions[0].service_tier,"fast");
    mutations.length=0;
    const advancedTemplate={...advancedPayload,id:"context-ui"};
    await page.evaluate(async template=>window.AdminApp.modules.pricing.openPricingModal(template,null,false),advancedTemplate);
    await page.locator("#ptAdvancedPricing > summary").click();
    await page.locator("#ptVersion").fill("2");
    await page.locator("#saveModelCostBtn").click();
    await page.locator("#modelCostModal.open").waitFor({state:"hidden"});
    assert.equal(mutations.length,1,"advanced edit writes once");
    assert.equal(mutations[0].method,"PUT");
    assert.equal(mutations[0].payload.version,2);
    assert.deepEqual(mutations[0].payload.conditions,advancedPayload.conditions);
    assert.deepEqual(mutations[0].payload.periods,advancedPayload.periods);
    assert.deepEqual(mutations[0].payload.date_exceptions,advancedPayload.date_exceptions);
    mutations.length=0;
    await page.evaluate(async template=>window.AdminApp.modules.pricing.openPricingModal(template,null,false),advancedTemplate);
    await page.locator("#ptCopyFromEditBtn").click();
    await targetCheckbox("z-fixture-target").check();
    await modelCheckbox(selectedModels[0]).check();
    await page.locator("#saveModelCostBtn").click();
    await page.locator("#modelCostModal.open").waitFor({state:"hidden"});
    assert.equal(mutations.length,1,"advanced copy writes once");
    assert.equal(mutations[0].method,"POST");
    assert.deepEqual(mutations[0].payload.conditions,advancedPayload.conditions);
    assert.deepEqual(mutations[0].payload.periods,advancedPayload.periods);
    assert.deepEqual(mutations[0].payload.date_exceptions,advancedPayload.date_exceptions);
    mutations.length=0;
    await page.locator('[data-page="quota"]').click();
    await page.locator("[data-edit-quota]").waitFor();
    await page.locator("[data-edit-quota]").first().click();
    await page.locator("#quotaEditDrawer.open").waitFor();
    assert.equal(
      await page.locator("#quotaEditGroupEditor .quota-target-deleted").count(),
      1,
    );
    await page
      .locator('#quotaEditDrawer [data-close="quotaEditDrawer"]')
      .first()
      .click();
    await page.locator('[data-quota-tab="policy"]').click();
    await page.locator("#newModelGroupBtn").click();
    await page.locator("#modelGroupModal.open").waitFor();
    assert.equal(await page.locator("#modelGroupDimensions input").count(), 3);
    await page
      .locator('#modelGroupModal [data-close="modelGroupModal"]')
      .first()
      .click();
    await page.locator('[data-quota-tab="settings"]').click();
    await page.locator("#newJobLevelBtn").click();
    await page.locator("#jobLevelModal.open").waitFor();
    assert.equal(await page.locator("#jobLevelDimensions input").count(), 3);
    await page
      .locator('#jobLevelModal [data-close="jobLevelModal"]')
      .first()
      .click();
    await page.locator("[data-edit-job-level]").first().click();
    await page.locator("#jobLevelModal.open").waitFor();
    assert.equal(await page.locator("#jobLevel-weekly").inputValue(), "2");
    await page
      .locator('#jobLevelModal [data-close="jobLevelModal"]')
      .first()
      .click();
    await page.locator('[data-quota-tab="batch"]').click();
    await page.locator("#quotaSyncMode").selectOption("job_level");
    assert.equal(
      await page.locator("#quotaSyncJobTargetWrap").isVisible(),
      true,
    );
    await page.locator('[data-page="model-catalog"]').click();
    await page.locator(".schema-summary-toggle").click();
    assert.equal(await page.locator("#adminSchemaSummary").isVisible(), true);
    assert.match(
      await page.locator("#adminSchemaSummary").textContent(),
      /请求格式.*新请求/s,
    );
    if (process.env.LLMS_UI_SCREENSHOT_DIR) {
      fs.mkdirSync(process.env.LLMS_UI_SCREENSHOT_DIR, { recursive: true });
      await page.screenshot({
        path: path.join(
          process.env.LLMS_UI_SCREENSHOT_DIR,
          "schema-desktop.png",
        ),
        fullPage: true,
        animations: "disabled",
      });
      await page.setViewportSize({ width: 390, height: 844 });
      await page.waitForTimeout(250);
      await page.screenshot({
        path: path.join(
          process.env.LLMS_UI_SCREENSHOT_DIR,
          "schema-mobile.png",
        ),
        fullPage: true,
        animations: "disabled",
      });
    }
    assert.equal(
      mutations.length,
      0,
      "navigation and editor inspection never call upstream or write API",
    );
    schemaOffline = true;
    await page.goto(base);
    await page.locator("#adminBootRetry").waitFor({ state: "visible" });
    assert.match(
      await page.locator("#adminBootMessage").textContent(),
      /加载失败.*fixture schema unavailable/,
    );
    await page.evaluate(() => document.getElementById("newTargetBtn").click());
    assert.equal(
      await page
        .locator("#targetModal")
        .evaluate((element) => element.classList.contains("open")),
      false,
    );
    schemaOffline = false;
    await page.locator("#adminBootRetry").click();
    await page.waitForFunction(() => window.AdminApp.state.ready);
    await page.close();
    role = "readonly";
    const readonly = await browser.newPage({
      viewport: { width: 1440, height: 1050 },
    });
    readonly.on("pageerror", (error) => errors.push(error.message));
    await readonly.goto(base);
    await readonly.waitForFunction(() => window.AdminApp?.state.ready);
    assert.equal(await readonly.locator("#nav-users").isVisible(), false);
    await readonly.locator('[data-page="model-catalog"]').click();
    await readonly.locator(".schema-summary-toggle").click();
    assert.equal(
      await readonly.locator("#adminSchemaSummary").isVisible(),
      true,
    );
    await readonly.locator('[data-page="targets"]').click();
    assert.equal(await readonly.locator("#newTargetBtn").isVisible(), false);
    await readonly.evaluate(() =>
      window.AdminApp.modules.targets.openTargetModal(null),
    );
    assert.equal(await readonly.locator("#targetModal.open").count(), 0);
    assert.equal(mutations.length, 0);
    assert.equal(
      requests.some((endpoint) => endpoint.endsWith("/test")),
      false,
      "no provider test endpoint called",
    );
    assert.deepEqual(
      unknownRequests,
      [],
      "browser fixture covers all requested API paths",
    );
    assert.deepEqual(
      errors,
      [],
      "all page navigation and editing paths execute without JS errors",
    );
    console.log(
      "PASS Chromium all pages/editors, Schema and retry, tooltip coordinates, Chinese labels, readonly",
    );
  } finally {
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
}
main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
