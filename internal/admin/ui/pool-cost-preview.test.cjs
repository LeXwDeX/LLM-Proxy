"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

const flush = () => new Promise((resolve) => setImmediate(resolve));

const pools = [
  { name: "ds", routing_strategy: "cost" },
  { name: "load-pool", routing_strategy: "load" },
];
const targets = [
  {
    name: "a",
    pool: "ds",
    endpoint_type: "openai",
    endpoint: "https://example.com",
    usage_adapter: "standard",
    model_mappings: [{ upstream: "Upstream-A", fallback: "alias-a" }],
  },
  {
    name: "b",
    pool: "ds",
    endpoint_type: "openai",
    endpoint: "https://example.com",
    usage_adapter: "standard",
    model_mappings: [{ upstream: "Upstream-B", fallback: "alias-b" }],
  },
];

function previewResponse(overrides = {}) {
  return {
    pool: "ds",
    routing_strategy: "cost",
    active: true,
    paused: false,
    model: "alias-a",
    generated_at: "2026-11-07T16:00:00Z",
    pricing_available: true,
    pricing_error: "",
    tolerance: 0.05,
    reference_tokens: { input: 1000000, cached_input: 1000000, output: 200000 },
    members: [
      {
        target: "a",
        upstream: "Upstream-A",
        resolved_via: "alias",
        eligible: true,
        paused: false,
        priced: true,
        estimated_cost_usd: 14.7,
        tier: 1,
        cheapest: false,
        reason: "",
        phase: "ready",
        in_flight: 0,
        max_concurrency: 0,
        affinity_soft_cap_reached: false,
      },
      {
        target: "b",
        upstream: "Upstream-B",
        resolved_via: "alias",
        eligible: true,
        paused: false,
        priced: true,
        estimated_cost_usd: 9.45,
        tier: 0,
        cheapest: true,
        reason: "",
        phase: "ready",
        in_flight: 0,
        max_concurrency: 0,
        affinity_soft_cap_reached: false,
      },
    ],
    ...overrides,
  };
}

async function setup(options = {}) {
  const harness = createHarness(options);
  const app = await harness.run();
  const field = (id) => harness.ids.get(id);
  app.state.pools = pools;
  app.state.targets = targets;
  app.modules.pools.renderPools();
  const body = field("poolsTableBody");
  const click = (dataset) => body.listeners.click[0]({ target: { dataset } });
  return { harness, app, field, body, click };
}

test("only cost pools expose the preview action", async () => {
  const { field } = await setup();
  const rows = field("poolsTableBody").innerHTML;
  assert.match(rows, /data-cost-preview-pool='ds'/);
  assert.doesNotMatch(rows, /data-cost-preview-pool='load-pool'/);
});

test("cost preview expands, defaults to the first request name and renders the API table", async () => {
  const { app, field, click } = await setup();
  const calls = [];
  app.modules.core.api = async (url) => {
    calls.push(url);
    return previewResponse();
  };

  click({ costPreviewPool: "ds" });
  await flush();

  assert.equal(calls.length, 1);
  assert.match(calls[0], /\/admin\/data\/pools\/ds\/cost-routing\?model=alias-a/);
  assert.match(field("poolsTableBody").innerHTML, /pool-cost-row/);
  assert.equal(field("poolCostModel").value, "alias-a");
  assert.ok(field("poolCostModel").getAttribute("list"), "model input has a datalist");

  const result = field("poolCostResult").innerHTML;
  assert.match(result, /Upstream-A/);
  assert.match(result, /Upstream-B/);
  assert.match(result, /\$9\.450000/);
  assert.match(result, /最便宜层/);
  assert.match(result, /pool-cost-cheapest/);
  assert.match(result, /估算按选路口径/);
  assert.match(result, /100 万未缓存输入/);
});

test("renders the API model and reference tokens instead of hard-coded values", async () => {
  const { app, field, click } = await setup();
  app.modules.core.api = async () =>
    previewResponse({
      model: "custom-model",
      reference_tokens: { input: 2000000, cached_input: 0, output: 100000 },
    });
  click({ costPreviewPool: "ds" });
  await flush();
  const result = field("poolCostResult").innerHTML;
  assert.match(result, /请求名 custom-model/);
  assert.match(result, /200 万未缓存输入/);
  assert.match(result, /0 万缓存读/);
  assert.match(result, /10 万输出/);
  assert.doesNotMatch(result, /100 万未缓存输入/);
});

test("renders unpriced and pricing-unavailable states", async () => {
  const { app, field, click } = await setup();
  const base = previewResponse().members[0];

  app.modules.core.api = async () =>
    previewResponse({
      members: [
        {
          ...base,
          target: "unpriced",
          priced: false,
          estimated_cost_usd: null,
          tier: null,
          cheapest: false,
          reason: "unpriced",
        },
      ],
    });
  click({ costPreviewPool: "ds" });
  await flush();
  let result = field("poolCostResult").innerHTML;
  assert.match(result, /无价格/);
  assert.match(result, /—/);

  app.modules.core.api = async () =>
    previewResponse({
      pricing_available: false,
      pricing_error: "pricing down",
      members: [
        {
          ...base,
          priced: false,
          estimated_cost_usd: null,
          tier: null,
          cheapest: false,
          reason: "pricing_unavailable",
        },
      ],
    });
  click({ costPreviewRun: "ds" });
  await flush();
  result = field("poolCostResult").innerHTML;
  assert.match(result, /定价源不可用：pricing down/);
  assert.match(result, /价格源不可用/);
});

test("preview result escapes configured names and the model", async () => {
  const { app, field, click } = await setup();
  app.modules.core.api = async () =>
    previewResponse({
      model: "'><img src=model>",
      members: [
        {
          ...previewResponse().members[0],
          target: "<img src=target>",
          upstream: "'><img src=upstream>",
        },
      ],
    });
  click({ costPreviewPool: "ds" });
  await flush();
  const result = field("poolCostResult").innerHTML;
  assert.doesNotMatch(result, /<img /);
  assert.match(result, /&lt;img src=target&gt;/);
  assert.match(result, /&#39;&gt;&lt;img src=upstream&gt;/);
});

test("switching the request name and refreshing reuse the sequence guard", async () => {
  const { app, field, click } = await setup();
  const calls = [];
  app.modules.core.api = async (url) => {
    calls.push(url);
    return previewResponse();
  };

  click({ costPreviewPool: "ds" });
  await flush();
  field("poolCostModel").value = "alias-b";
  click({ costPreviewRun: "ds" });
  await flush();

  assert.equal(calls.length, 2);
  assert.match(calls[1], /model=alias-b/);
  assert.match(field("poolCostResult").innerHTML, /Upstream-B/);
});

test("a stale preview response cannot overwrite a newer one", async () => {
  const { app, field, click } = await setup();
  const pending = [];
  app.modules.core.api = (url) =>
    new Promise((resolve) => pending.push({ url, resolve }));

  click({ costPreviewPool: "ds" }); // call 1, keeps pending
  await flush();
  field("poolCostModel").value = "alias-b";
  click({ costPreviewRun: "ds" }); // call 2
  await flush();
  assert.equal(pending.length, 2);

  pending[1].resolve(
    previewResponse({
      model: "alias-b",
      members: [
        {
          ...previewResponse().members[1],
          target: "new-target",
          upstream: "New-Upstream",
        },
      ],
    }),
  );
  await flush();
  pending[0].resolve(
    previewResponse({
      members: [
        {
          ...previewResponse().members[0],
          target: "old-target",
          upstream: "Old-Upstream",
        },
      ],
    }),
  );
  await flush();

  const result = field("poolCostResult").innerHTML;
  assert.match(result, /New-Upstream/);
  assert.doesNotMatch(result, /Old-Upstream/, "the first response is stale");
});

test("preview errors are shown in the result area", async () => {
  const { app, field, click } = await setup();
  app.modules.core.api = async () => {
    throw new Error("preview offline");
  };
  click({ costPreviewPool: "ds" });
  await flush();
  assert.match(field("poolCostResult").innerHTML, /预览失败: preview offline/);
});

test("collapsing the row removes the detail content", async () => {
  const { app, field, click } = await setup();
  app.modules.core.api = async () => previewResponse();
  click({ costPreviewPool: "ds" });
  await flush();
  assert.match(field("poolsTableBody").innerHTML, /pool-cost-row/);
  click({ costPreviewPool: "ds" });
  assert.doesNotMatch(field("poolsTableBody").innerHTML, /pool-cost-row/);
});

test("a pool refresh collapses an open preview", async () => {
  const harness = createHarness({
    routes: {
      "/admin/data/pools": { pools },
      "/admin/data/targets": { targets },
    },
  });
  const app = await harness.run();
  const field = (id) => harness.ids.get(id);
  app.state.pools = pools;
  app.state.targets = targets;
  app.modules.pools.renderPools();
  app.modules.core.api = async (url) => {
    if (url.startsWith("/admin/data/pools/")) return previewResponse();
    if (url === "/admin/data/pools") return { pools };
    if (url === "/admin/data/targets") return { targets };
    if (url === "/admin/metrics") return { targets_runtime: [] };
    return {};
  };
  field("poolsTableBody").listeners.click[0]({
    target: { dataset: { costPreviewPool: "ds" } },
  });
  await flush();
  assert.match(field("poolsTableBody").innerHTML, /pool-cost-row/);

  await app.modules.pools.loadPools();
  await flush();
  assert.doesNotMatch(field("poolsTableBody").innerHTML, /pool-cost-row/);
});

test("readonly users can open the read-only preview", async () => {
  const { app, field, click } = await setup({ role: "readonly" });
  assert.match(field("poolsTableBody").innerHTML, /data-cost-preview-pool='ds'/);
  app.modules.core.api = async () => previewResponse();
  click({ costPreviewPool: "ds" });
  await flush();
  assert.match(field("poolCostResult").innerHTML, /Upstream-B/);
});

test("preview suggestions include paused declarations so their reason is visible", async () => {
  const { app, field, click } = await setup();
  app.state.targets = targets.concat([
    {
      name: "paused-only",
      pool: "ds",
      model_mappings: [
        { upstream: "Paused-Upstream", fallback: "paused-alias", paused: true },
      ],
    },
  ]);
  app.modules.pools.renderPools();
  app.modules.core.api = async () => previewResponse();
  click({ costPreviewPool: "ds" });
  await flush();
  const html = field("poolsTableBody").innerHTML;
  assert.match(html, /<option value='paused-alias'>/);
  assert.match(html, /<option value='Paused-Upstream'>/);
});

test("aria-controls is present only while the detail row exists", async () => {
  const { app, field, click } = await setup();
  app.modules.core.api = async () => previewResponse();
  assert.doesNotMatch(field("poolsTableBody").innerHTML, /aria-controls='poolCostDetail'/);
  click({ costPreviewPool: "ds" });
  await flush();
  assert.match(
    field("poolsTableBody").innerHTML,
    /aria-expanded='true'[^>]*aria-controls='poolCostDetail'/,
  );
  click({ costPreviewPool: "ds" });
  assert.doesNotMatch(field("poolsTableBody").innerHTML, /aria-controls='poolCostDetail'/);
});

test("the preview table keeps theme-colored cheapest and secondary text", () => {
  const css = fs.readFileSync(path.join(__dirname, "css/admin.css"), "utf8");
  assert.match(css, /\.pool-cost-cheapest td\s*\{[^}]*background\s*:\s*var\(--ok-soft\)/);
  assert.match(css, /\.pool-cost-secondary\s*\{[^}]*color\s*:\s*var\(--ink-3\)/);
});