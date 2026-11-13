"use strict";
// Per-page behaviour of the shared table sort: each page orders its rows through createTableSort, keeps its
// own default order, sorts by the underlying value (not the display text) and keeps detail rows attached.
// Overview tables are covered next to their harness in js/overview.test.cjs.
const assert = require("node:assert/strict");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

const json = (value) => ({ ok: true, status: 200, text: async () => JSON.stringify(value) });
const flush = () => new Promise((resolve) => setImmediate(resolve));

// A click on a sortable <th>, delivered to the listeners registered on `id` (a <thead>, or a stable table body
// for headers that are regenerated with the rows). closest() answers only for the sortable-header selector, so
// the page's own row handlers on the same element ignore it, as they do a real header click.
function click(h, id, key) {
  const th = { getAttribute: (name) => (name === "data-sort" ? key : null) };
  const event = { target: { dataset: {}, closest: (selector) => (selector === "th[data-sort]" ? th : null) } };
  for (const listener of h.ids.get(id).listeners.click || []) listener(event);
}
const clicks = (h, id, key, times) => { for (let i = 0; i < times; i++) click(h, id, key); };
const html = (h, id) => h.ids.get(id).innerHTML;

async function boot(options) {
  const h = createHarness(options);
  const app = await h.run();
  return { h, app, state: app.state };
}

// ---------------------------------------------------------------- clients

test("clients: every column sorts by its underlying value, the default order is the name", async () => {
  const { h, app, state } = await boot();
  state.clients = [
    { name: "bob", ip_address: "10.0.0.9", access_key: "sk-a-AAAA-wxyz", allowed_targets: ["t1", "t2"], job_level: "L2" },
    { name: "alice", access_key: "sk-a-ZZZZ-abcd", job_level: "L1" },
    { name: "carol", ip_address: "10.0.0.10", access_key: "cccc-1111-2222-dddd", allowed_pools: ["p"] },
    { name: "dave", ip_address: "10.0.0.2", access_key: "dddd-1111-2222-eeee", allowed_targets: ["t1", "t2", "t3"] },
  ];
  state.clientUsage = {
    bob: { input_tokens: 5, output_tokens: 5, estimated_cost: 2 },
    carol: { input_tokens: 1000, estimated_cost: 1 },
    dave: { input_tokens: 70, estimated_cost: 12.5 },
  };
  app.modules.clients.renderClients();
  const order = () => [...html(h, "clientsTableBody").matchAll(/<tr><td>(\w+)<\/td>/g)].map((m) => m[1]);
  assert.deepEqual(order(), ["alice", "bob", "carol", "dave"], "default: by name");

  click(h, "clientsTableHead", "cost");
  assert.deepEqual(order(), ["alice", "carol", "bob", "dave"], "cost ascending (a client without usage costs 0)");
  click(h, "clientsTableHead", "cost");
  assert.deepEqual(order(), ["dave", "bob", "carol", "alice"], "cost descending is numeric, not the text 12.50 < 2.00");
  click(h, "clientsTableHead", "cost");
  assert.deepEqual(order(), ["alice", "bob", "carol", "dave"], "the third click restores the default");

  click(h, "clientsTableHead", "tokens");
  assert.deepEqual(order(), ["alice", "bob", "dave", "carol"], "tokens ascending: 0, 10, 70, 1000");
  click(h, "clientsTableHead", "targets");
  assert.deepEqual(order(), ["carol", "bob", "dave", "alice"], "authorised targets and pools counted; unrestricted access sorts as the most");
  click(h, "clientsTableHead", "ip");
  assert.deepEqual(order(), ["dave", "bob", "carol", "alice"], "10.0.0.2 < 10.0.0.9 < 10.0.0.10 numerically; the client without an IP is last");
  click(h, "clientsTableHead", "ip");
  assert.deepEqual(order(), ["carol", "bob", "dave", "alice"], "descending still puts the empty value last");
  click(h, "clientsTableHead", "quota");
  assert.deepEqual(order(), ["alice", "bob", "carol", "dave"], "job levels L1, L2, then clients without one");
});

test("clients: the Access Key column sorts by the masked text that is shown", async () => {
  const { h, app, state } = await boot();
  // Raw keys order c1 < c2, the masked texts (first 4 + last 4) order them the other way round.
  state.clients = [
    { name: "c1", access_key: "sk-a-AAAA-wxyz" },
    { name: "c2", access_key: "sk-a-ZZZZ-abcd" },
  ];
  app.modules.clients.renderClients();
  click(h, "clientsTableHead", "key");
  assert.deepEqual([...html(h, "clientsTableBody").matchAll(/<tr><td>(\w+)<\/td>/g)].map((m) => m[1]), ["c2", "c1"]);
});

test("clients: sorting keeps the edit and delete buttons on their own client", async () => {
  const { h, app, state } = await boot();
  state.clients = [{ name: "b", access_key: "k1" }, { name: "a", access_key: "k2" }];
  state.clientUsage = { b: { estimated_cost: 9 }, a: { estimated_cost: 1 } };
  app.modules.clients.renderClients();
  clicks(h, "clientsTableHead", "cost", 2);
  const rows = [...html(h, "clientsTableBody").matchAll(/<tr><td>(\w+)<\/td>[\s\S]*?data-edit-client='(\w+)'[\s\S]*?data-del-client='(\w+)'/g)];
  assert.deepEqual(rows.map((m) => [m[1], m[2], m[3]]), [["b", "b", "b"], ["a", "a", "a"]]);
});

// ---------------------------------------------------------------- model costs (pricing)

function pricingGroups(h, app) {
  const state = app.state;
  state.targets = [];
  state.pricingTemplates = [
    { id: "t1", target: "alpha", model: "m2", mode: "flat", base_rates: {} },
    { id: "t2", target: "alpha", model: "m2", mode: "schedule", periods: [], base_rates: {} },
    { id: "t3", target: "beta", model: "m1", mode: "flat", base_rates: {} },
    { id: "t4", target: "", model: "m3", mode: "flat", base_rates: {} },
  ];
  return () => {
    const rows = [];
    for (const part of html(h, "modelCostsTableBody").split("<tr ").slice(1)) {
      const name = /pricing-target-name'>([^<]*)</.exec(part);
      rows.push(part.startsWith("data-pricing-summary") ? name[1] : "·detail");
    }
    return rows;
  };
}

test("model costs: groups sort by Target, model, template count and mode; a group's detail row stays with it", async () => {
  const { h, app } = await boot();
  const order = pricingGroups(h, app);
  app.modules.pricing.renderPricingTemplates();
  assert.deepEqual(order(), ["默认", "alpha", "beta"], "default: the group key order");

  click(h, "modelCostsTableHead", "targets");
  assert.deepEqual(order(), ["默认", "alpha", "beta"], "Target ascending by the shown text (the global default is 默认; zh-CN collation puts Han before Latin)");
  click(h, "modelCostsTableHead", "targets");
  assert.deepEqual(order(), ["beta", "alpha", "默认"]);
  click(h, "modelCostsTableHead", "targets");

  click(h, "modelCostsTableHead", "count");
  assert.deepEqual(order(), ["默认", "beta", "alpha"], "count ascending: ties keep the default order");
  click(h, "modelCostsTableHead", "count");
  assert.deepEqual(order(), ["alpha", "默认", "beta"]);
  click(h, "modelCostsTableHead", "count");

  click(h, "modelCostsTableHead", "mode");
  assert.deepEqual(order(), ["默认", "beta", "alpha"], "groups with a scheduled template come after all-day groups");
  click(h, "modelCostsTableHead", "mode");
  click(h, "modelCostsTableHead", "models");
  assert.deepEqual(order(), ["beta", "alpha", "默认"], "models m1, m2, m3 ... ascending");
  click(h, "modelCostsTableHead", "models");
  click(h, "modelCostsTableHead", "models");

  // Expanding a group adds its detail row; sorting never separates the two.
  const { pricingGroupKey } = app.modules.pricingShared;
  app.state.pricingExpanded = { [pricingGroupKey(["alpha"], ["m2"])]: true };
  app.modules.pricing.renderPricingTemplates();
  assert.deepEqual(order(), ["默认", "alpha", "·detail", "beta"]);
  click(h, "modelCostsTableHead", "targets");
  assert.deepEqual(order(), ["默认", "alpha", "·detail", "beta"], "the detail row follows alpha");
  click(h, "modelCostsTableHead", "targets");
  assert.deepEqual(order(), ["beta", "alpha", "·detail", "默认"], "and still follows it when reversed");
});

// ---------------------------------------------------------------- quota

function quotaItem(name, extra = {}) {
  return {
    client: { name, ip_address: "10.0.0.1", job_level: "L1", project_group: "pg", ...extra.client },
    status: { job_level: "L1", quotas: {}, groups: {}, ...extra.status },
  };
}
function quotaFixture(state) {
  const group = (used, limit, extra = {}) => ({ in_plan: true, quotas: { daily: { used, limit } }, ...extra });
  state.quotaSummary = [
    quotaItem("alice", { status: { quotas: { daily: { used: 5, limit: 10 } }, groups: { g1: group(5, 10), g2: group(9, 10), g3: group(3, 0) } } }),
    quotaItem("bob", { status: { quotas: { daily: { used: 9, limit: 10 } }, groups: { g1: group(9, 10) } } }),
    quotaItem("carol", { client: { job_level: "" }, status: { job_level_missing: true, groups: {} } }),
    quotaItem("dave", { status: { quotas: { daily: { used: 1, limit: 100 } }, groups: { g1: group(1, 100), g2: group(0, 100) } } }),
    quotaItem("erin", { status: { quotas: {}, groups: {} } }),
  ];
  state.clientUsage30d = {
    alice: { input_tokens: 10, output_tokens: 5, estimated_cost: 1.5 },
    bob: { input_tokens: 100, estimated_cost: 0.25 },
    dave: { input_tokens: 40, estimated_cost: 30 },
  };
  state.clientUsage30dPending = false;
  state.clientUsage30dUnavailable = false;
  state.quotaExpanded = {};
}
const quotaOrder = (h) =>
  [...html(h, "quotaSummaryBody").matchAll(/data-quota-person='(\w+)'|data-quota-detail='(\w+)'/g)].map((m) => (m[1] ? m[1] : "·" + m[2]));

test("quota summary: dimensions sort by used / limit, usage by number, status by severity; empties stay last", async () => {
  const { h, app, state } = await boot();
  quotaFixture(state);
  app.modules.quota.renderQuotaSummary();
  assert.deepEqual(quotaOrder(h), ["alice", "bob", "carol", "dave", "erin"], "default: by name");

  click(h, "quotaSummaryTableHead", "dim_daily");
  // alice 50%, bob 90%, dave 1%; carol (no job level) and erin (not configured) have no limit: last.
  assert.deepEqual(quotaOrder(h), ["dave", "alice", "bob", "carol", "erin"]);
  click(h, "quotaSummaryTableHead", "dim_daily");
  assert.deepEqual(quotaOrder(h), ["bob", "alice", "dave", "carol", "erin"], "descending keeps the cells without a ratio last");
  click(h, "quotaSummaryTableHead", "dim_daily");
  assert.deepEqual(quotaOrder(h), ["alice", "bob", "carol", "dave", "erin"], "back to the default order");

  click(h, "quotaSummaryTableHead", "tokens");
  assert.deepEqual(quotaOrder(h), ["carol", "erin", "alice", "dave", "bob"], "30-day tokens as numbers (15, 40, 100)");
  click(h, "quotaSummaryTableHead", "cost");
  assert.deepEqual(quotaOrder(h), ["carol", "erin", "bob", "alice", "dave"], "30-day cost as numbers: 0.25 < 1.5 < 30");
  click(h, "quotaSummaryTableHead", "groups");
  assert.deepEqual(quotaOrder(h), ["bob", "dave", "alice", "carol", "erin"], "number of model groups; none is last");
  click(h, "quotaSummaryTableHead", "status");
  assert.deepEqual(quotaOrder(h), ["carol", "alice", "bob", "dave", "erin"], "severity: 职级未配置, 接近限额 (ties by name), 正常, 未配置");
  click(h, "quotaSummaryTableHead", "status");
  assert.deepEqual(quotaOrder(h), ["erin", "dave", "alice", "bob", "carol"]);
  click(h, "quotaSummaryTableHead", "status");
  click(h, "quotaSummaryTableHead", "name");
  click(h, "quotaSummaryTableHead", "name");
  assert.deepEqual(quotaOrder(h), ["erin", "dave", "carol", "bob", "alice"]);
});

test("quota summary: an expanded detail row stays attached to its client whatever the order", async () => {
  const { h, app, state } = await boot();
  quotaFixture(state);
  state.quotaExpanded = { alice: true, dave: true };
  app.modules.quota.renderQuotaSummary();
  assert.deepEqual(quotaOrder(h), ["alice", "·alice", "bob", "carol", "dave", "·dave", "erin"]);
  click(h, "quotaSummaryTableHead", "dim_daily");
  assert.deepEqual(quotaOrder(h), ["dave", "·dave", "alice", "·alice", "bob", "carol", "erin"]);
  click(h, "quotaSummaryTableHead", "dim_daily");
  assert.deepEqual(quotaOrder(h), ["bob", "alice", "·alice", "dave", "·dave", "carol", "erin"]);
  // The status filter applies before sorting and the detail rows still follow.
  h.ids.get("quotaSummaryStatusFilter").value = "warn";
  app.modules.quota.renderQuotaSummary();
  assert.deepEqual(quotaOrder(h), ["bob", "alice", "·alice"]);
});

test("quota summary: the per-group table in the details sorts by consumption ratio, all expanded rows alike", async () => {
  const { h, app, state } = await boot();
  quotaFixture(state);
  state.quotaExpanded = { alice: true, dave: true };
  const groups = (name) => {
    const body = html(h, "quotaSummaryBody");
    const start = body.indexOf("data-quota-detail='" + name + "'");
    const end = body.indexOf("</tr>", body.indexOf("</table>", start));
    return [...body.slice(start, end).matchAll(/<tr><th>(\w+)/g)].map((m) => m[1]);
  };
  app.modules.quota.renderQuotaSummary();
  assert.deepEqual(groups("alice"), ["g1", "g2", "g3"]);
  assert.deepEqual(groups("dave"), ["g1", "g2"]);
  assert.match(html(h, "quotaSummaryBody"), /<th data-sort='dim_daily'[^>]*>Schema日（已折算消耗 \/ 可用上限）<\/th>/, "the nested header is sortable");
  click(h, "quotaSummaryBody", "dim_daily"); // ascending
  assert.deepEqual(groups("alice"), ["g1", "g2", "g3"], "50% < 90%; the group without a limit is last");
  assert.deepEqual(groups("dave"), ["g2", "g1"], "0% < 1%");
  click(h, "quotaSummaryBody", "dim_daily"); // descending
  assert.deepEqual(groups("alice"), ["g2", "g1", "g3"]);
  assert.deepEqual(groups("dave"), ["g1", "g2"]);
  assert.deepEqual(quotaOrder(h), ["alice", "·alice", "bob", "carol", "dave", "·dave", "erin"], "the people table keeps its order");
  click(h, "quotaSummaryBody", "dim_daily");
  assert.deepEqual(groups("alice"), ["g1", "g2", "g3"], "default order again");
  click(h, "quotaSummaryBody", "name");
  click(h, "quotaSummaryBody", "name");
  assert.deepEqual(groups("alice"), ["g3", "g2", "g1"], "the group name column");
});

test("quota model groups: sorting keeps each edit button on its own group", async () => {
  const { h, app, state } = await boot();
  state.modelGroups = [
    { name: "beta", cost_coefficient: 1, daily_usd: 0, bindings: [{ target: "t2", model: "m" }] },
    { name: "alpha", cost_coefficient: 0.5, daily_usd: 5, bindings: [{ target: "t1" }] },
    { name: "company_general", cost_coefficient: 2, daily_usd: 2, bindings: [] },
  ];
  state.targets = [];
  app.modules.quota.renderModelGroups();
  const rows = () => [...html(h, "modelGroupsBody").matchAll(/<tr><td><strong>([^<（]+)[\s\S]*?data-edit-model-group='(\d+)'/g)]
    .map((m) => [m[1], state.modelGroups[Number(m[2])].name]);
  const names = () => rows().map((row) => row[0]);
  assert.deepEqual(names(), ["beta", "alpha", "未归组"], "default: the order the server returned");
  click(h, "modelGroupsTableHead", "name");
  assert.deepEqual(names(), ["未归组", "alpha", "beta"], "the visible label sorts (未归组), not company_general");
  assert.ok(rows().every(([shown, edited]) => shown === edited || (shown === "未归组" && edited === "company_general")), "edit buttons still address their own group");
  click(h, "modelGroupsTableHead", "name");
  click(h, "modelGroupsTableHead", "name");
  click(h, "modelGroupsTableHead", "coefficient");
  assert.deepEqual(names(), ["alpha", "beta", "未归组"], "deduction coefficient 0.5 < 1 < 2");
  click(h, "modelGroupsTableHead", "coefficient");
  assert.deepEqual(names(), ["未归组", "beta", "alpha"]);
  click(h, "modelGroupsTableHead", "coefficient");
  click(h, "modelGroupsTableHead", "base");
  assert.deepEqual(names(), ["未归组", "alpha", "beta"], "a base of 0 is unlimited and sorts as the largest");
  assert.ok(rows().every(([shown, edited]) => shown === edited || (shown === "未归组" && edited === "company_general")));
  click(h, "modelGroupsTableHead", "base");
  assert.deepEqual(names(), ["beta", "alpha", "未归组"]);
  click(h, "modelGroupsTableHead", "base");
  click(h, "modelGroupsTableHead", "models");
  assert.deepEqual(names(), ["alpha", "beta", "未归组"], "bound targets: t1, t2, none (empty list last)");
});

test("quota job levels and project groups sort; a multiplier of 0 (unlimited) is the largest", async () => {
  const { h, app, state } = await boot();
  state.jobLevels = [{ name: "L1", coefficient_daily: 2 }, { name: "L2", coefficient_daily: 0 }, { name: "L3" }, "legacy"];
  state.projectGroups = ["zeta", "alpha", "Beta"];
  app.modules.quota.renderDictTables();
  const levels = () => [...html(h, "jobLevelsBody").matchAll(/<tr><td><strong>(\w+)<\/strong>/g)].map((m) => m[1]);
  const projects = () => [...html(h, "projectGroupsBody").matchAll(/<tr><td><strong>(\w+)<\/strong>/g)].map((m) => m[1]);
  assert.deepEqual(levels(), ["L1", "L2", "L3", "legacy"]);
  click(h, "jobLevelTableHead", "dim_daily");
  assert.deepEqual(levels(), ["L3", "legacy", "L1", "L2"], "a missing multiplier is shown as ×1 and sorts as 1; 0 is last");
  click(h, "jobLevelTableHead", "dim_daily");
  assert.deepEqual(levels(), ["L2", "L1", "L3", "legacy"]);
  click(h, "jobLevelTableHead", "dim_daily");
  click(h, "jobLevelTableHead", "name");
  click(h, "jobLevelTableHead", "name");
  assert.deepEqual(levels(), ["legacy", "L3", "L2", "L1"]);
  assert.deepEqual(projects(), ["zeta", "alpha", "Beta"], "default: the server order");
  click(h, "projectGroupsTableHead", "name");
  assert.deepEqual(projects(), ["alpha", "Beta", "zeta"]);
  click(h, "projectGroupsTableHead", "name");
  assert.deepEqual(projects(), ["zeta", "Beta", "alpha"]);
  assert.deepEqual(levels(), ["legacy", "L3", "L2", "L1"], "each table keeps its own sort");
});

// ---------------------------------------------------------------- model catalog

test("model catalog: the whole filtered list is sorted before paging; missing values are last, $0 is a value", async () => {
  const models = [];
  for (let i = 1; i <= 35; i++) {
    models.push({
      endpoint_type: i % 2 ? "provider-new" : "provider-other",
      model: "m" + String(i).padStart(2, "0"),
      display_name: i === 7 ? "" : "Model " + i,
      context_window: i === 3 ? 0 : i * 1000,
      capabilities: i % 3 ? ["vision"] : [],
      default_cost: i === 5 ? { input_per_1m_tokens: 0 } : i === 6 ? {} : { input_per_1m_tokens: i / 10 },
    });
  }
  const { h, app, state } = await boot();
  state.catalogAllCache = models;
  await app.modules.catalog.loadModelCatalog();
  const page = () => [...html(h, "catalogTableBody").matchAll(/>(m\d\d)<\/td>/g)].map((m) => m[1]);
  assert.equal(page().length, 30);
  assert.deepEqual(page().slice(0, 2), ["m01", "m02"], "default: the catalog order");

  click(h, "catalogTableHead", "model");
  click(h, "catalogTableHead", "model");
  assert.equal(page()[0], "m35", "descending starts from the last model of the whole list, not of the page");
  assert.equal(h.ids.get("catalogPageNum").textContent, "1 / 2");
  await h.ids.get("catalogNextBtn").listeners.click[0]();
  assert.deepEqual(page(), ["m05", "m04", "m03", "m02", "m01"], "the second page continues the sorted list");
  assert.equal(h.ids.get("catalogPageNum").textContent, "2 / 2");
  click(h, "catalogTableHead", "model"); // default again
  assert.equal(h.ids.get("catalogPageNum").textContent, "1 / 2", "changing the sort returns to the first page");

  click(h, "catalogTableHead", "context");
  assert.deepEqual(page().slice(0, 3), ["m01", "m02", "m04"], "context window ascending; m03 has none and sorts last");
  assert.equal(page().includes("m03"), false, "...on the second page");
  click(h, "catalogTableHead", "context");
  assert.deepEqual(page().slice(0, 2), ["m35", "m34"]);
  click(h, "catalogTableHead", "context");

  click(h, "catalogTableHead", "input_price");
  assert.deepEqual(page().slice(0, 4), ["m05", "m01", "m02", "m03"], "an explicit $0 comes first; m06 has no price and is last");
  click(h, "catalogTableHead", "input_price");
  assert.equal(page()[0], "m35");
  click(h, "catalogTableHead", "input_price");

  click(h, "catalogTableHead", "capabilities");
  assert.ok(page().slice(0, 3).every((m) => Number(m.slice(1)) % 3 !== 0), "models with a capability come before those without");
  click(h, "catalogTableHead", "display");
  click(h, "catalogTableHead", "display");
  assert.equal(page()[0], "m35", "display names sort as text with digit runs compared numerically: Model 35 before Model 9");
  assert.equal(page().includes("m07"), false, "m07 has no display name: it stays out of the first page");
  // A keyword filter applies before the sort.
  h.ids.get("catalogSearch").value = "m1";
  app.modules.catalog.applyCatalogFilter();
  assert.ok(page().every((m) => m.startsWith("m1")));
  click(h, "catalogTableHead", "display");
  assert.equal(page()[0], "m10", "default order within the filtered list");
});

// ---------------------------------------------------------------- users, audit, trace

test("users: columns sort, the order survives a reload and row actions still address their own user", async () => {
  const users = [
    { username: "zed", role: "readonly", disabled: false, updated_at: "2026-10-01T00:00:00Z" },
    { username: "amy", role: "admin", disabled: true, updated_at: "2026-10-03T00:00:00Z" },
    { username: "bob", role: "readonly", disabled: false },
  ];
  const { h, app } = await boot({ routes: { "/admin/data/users": { users } } });
  await app.modules.users.loadUsers();
  const order = () => [...html(h, "usersTableBody").matchAll(/<tr><td>(\w+)<\/td>/g)].map((m) => m[1]);
  assert.deepEqual(order(), ["zed", "amy", "bob"], "default: the server's order");
  click(h, "usersTableHead", "username");
  assert.deepEqual(order(), ["amy", "bob", "zed"]);
  click(h, "usersTableHead", "username");
  assert.deepEqual(order(), ["zed", "bob", "amy"]);
  click(h, "usersTableHead", "username");
  click(h, "usersTableHead", "role");
  assert.deepEqual(order(), ["amy", "zed", "bob"], "admins first, ties in the server's order");
  click(h, "usersTableHead", "status");
  assert.deepEqual(order(), ["zed", "bob", "amy"], "enabled before disabled");
  click(h, "usersTableHead", "updated");
  assert.deepEqual(order(), ["zed", "amy", "bob"], "by time; a user without one is last");
  click(h, "usersTableHead", "updated");
  assert.deepEqual(order(), ["amy", "zed", "bob"], "descending keeps it last");
  await app.modules.users.loadUsers();
  assert.deepEqual(order(), ["amy", "zed", "bob"], "a reload keeps the chosen order");
  assert.match(html(h, "usersTableBody"), /data-edit-user='zed'/);
  assert.equal(app.modules.users.findCachedUser("zed").username, "zed", "the cache keeps the server order for lookups");
});

test("audit: the loaded events sort by time, actor and other columns; a failed load is not revived by a sort", async () => {
  const events = [
    { timestamp: "2026-10-29T03:00:00Z", actor: "carol", action: "update", object: "t1", result: "ok", detail: "d" },
    { timestamp: "2026-10-29T01:00:00Z", operator: "alice", action: "create", target: "t0", status: "ok", details: "x" },
    { timestamp: "2026-10-29T02:00:00Z", actor: "bob", action: "delete", object: "t2", result: "failed" },
    { timestamp: "not a time", actor: "dave", action: "noop", object: "t3", result: "ok" },
  ];
  const { h, app } = await boot({ routes: { "/admin/data/audit?limit=50": { events } } });
  await app.modules.audit_trace.loadAudit();
  const actors = () => [...html(h, "auditTableBody").matchAll(/<tr><td>[^<]*<\/td><td>([^<]*)<\/td>/g)].map((m) => m[1]);
  assert.deepEqual(actors(), ["carol", "alice", "bob", "dave"], "default: the server's order");
  click(h, "auditTableHead", "time");
  assert.deepEqual(actors(), ["alice", "bob", "carol", "dave"], "oldest first; an unparseable time is last");
  click(h, "auditTableHead", "time");
  assert.deepEqual(actors(), ["carol", "bob", "alice", "dave"], "newest first, the unparseable one still last");
  click(h, "auditTableHead", "time");
  click(h, "auditTableHead", "actor");
  assert.deepEqual(actors(), ["alice", "bob", "carol", "dave"]);
  click(h, "auditTableHead", "action");
  assert.deepEqual(actors(), ["alice", "bob", "dave", "carol"], "create, delete, noop, update");
  click(h, "auditTableHead", "detail");
  assert.deepEqual(actors(), ["carol", "alice", "bob", "dave"], "events with a detail first (d, x); the others keep the server's order, last");
  h.setRouteFailure("/admin/data/audit?limit=50", true);
  await app.modules.audit_trace.loadAudit();
  assert.match(html(h, "auditTableBody"), /加载失败/);
  click(h, "auditTableHead", "actor");
  assert.match(html(h, "auditTableBody"), /加载失败/, "sorting does not bring the old events back");
});

function traceRecord(id, extra = {}) {
  return { trace_id: id, timestamp: "2026-10-29T01:00:00Z", client_name: "c", target: "t", model: "m", status_code: 200, duration_ms: 10, input_tokens: 1, output_tokens: 2, ...extra };
}
test("trace: every loaded record sorts, including records appended by 'load earlier'", async () => {
  const { h, app } = await boot();
  const trace = app.modules.audit_trace;
  const ids = () => [...html(h, "traceTableBody").matchAll(/data-trace-id='([^']*)'/g)].map((m) => m[1]);
  trace.renderTraceTable([
    traceRecord("a", { status_code: 200, duration_ms: 30, timestamp: "2026-10-29T03:00:00Z", model: "gpt-9" }),
    traceRecord("b", { status_code: 500, duration_ms: 10, timestamp: "2026-10-29T01:00:00Z", model: "gpt-10", input_tokens: 0, output_tokens: 0 }),
    traceRecord("c", { status_code: 404, duration_ms: 20, timestamp: "2026-10-29T02:00:00Z", model: "gpt-2", input_tokens: 5, output_tokens: 5 }),
  ], {});
  assert.deepEqual(ids(), ["a", "b", "c"], "default: the server's order");
  click(h, "traceTableHead", "status");
  assert.deepEqual(ids(), ["a", "c", "b"]);
  trace.appendTraceRecords([traceRecord("d", { status_code: 201 }), traceRecord("e", { status_code: 503 })]);
  assert.deepEqual(ids(), ["a", "d", "c", "b", "e"], "appended records join the sorted list");
  click(h, "traceTableHead", "status");
  assert.deepEqual(ids(), ["e", "b", "c", "d", "a"]);
  click(h, "traceTableHead", "status");
  assert.deepEqual(ids(), ["a", "b", "c", "d", "e"], "the default order is the order records were loaded");
  click(h, "traceTableHead", "duration");
  assert.deepEqual(ids().slice(0, 3), ["b", "d", "e"], "10 ms first; equal durations keep the order they were loaded in");
  click(h, "traceTableHead", "time");
  assert.deepEqual(ids().slice(0, 2), ["b", "d"], "ties on the time keep the default order");
  click(h, "traceTableHead", "time");
  assert.equal(ids()[0], "a", "newest first");
  click(h, "traceTableHead", "time");
  click(h, "traceTableHead", "tokens");
  assert.equal(ids().at(-1), "b", "a record without token counts is last");
  click(h, "traceTableHead", "tokens");
  assert.equal(ids().at(-1), "b", "...in both directions");
  click(h, "traceTableHead", "tokens");
  click(h, "traceTableHead", "model");
  assert.deepEqual(ids().slice(0, 3), ["c", "a", "b"], "gpt-2 < gpt-9 < gpt-10: digits compare numerically");
  trace.renderTraceTable([], {});
  assert.match(html(h, "traceTableBody"), /暂无请求记录/);
  click(h, "traceTableHead", "model");
  assert.match(html(h, "traceTableBody"), /暂无请求记录/, "an empty table stays on its message");
  trace.appendTraceRecords([traceRecord("x")]);
  assert.deepEqual(ids(), ["x"], "the placeholder never stays mixed with real rows");
});

// ---------------------------------------------------------------- pools

test("pools: columns sort and the cost-preview table sorts the last response without a request", async () => {
  const { h, app, state } = await boot();
  state.pools = [
    { name: "p1", routing_strategy: "cost" },
    { name: "p2", routing_strategy: "load", paused: true },
    { name: "p3", routing_strategy: "cost" },
  ];
  const mapped = (name, pool) => ({ name, pool, model_mappings: [{ upstream: "U-" + name, fallback: "alias" }] });
  state.targets = [mapped("t1", "p1"), mapped("t2", "p1"), mapped("t3", "p3")];
  app.modules.pools.renderPools();
  const order = () => [...html(h, "poolsTableBody").matchAll(/class='pool-name'[^>]*>([^<]*)</g)].map((m) => m[1]);
  assert.deepEqual(order(), ["p1", "p2", "p3"], "default: by name");
  click(h, "poolsTableHead", "members");
  assert.deepEqual(order(), ["p2", "p3", "p1"], "member count ascending (0, 1, 2)");
  click(h, "poolsTableHead", "members");
  assert.deepEqual(order(), ["p1", "p3", "p2"]);
  click(h, "poolsTableHead", "members");
  click(h, "poolsTableHead", "status");
  assert.deepEqual(order(), ["p1", "p3", "p2"], "running pools before the paused one");
  click(h, "poolsTableHead", "strategy");
  assert.deepEqual(order(), ["p1", "p3", "p2"], "成本优先 before 性能优先 (ties keep the name order)");
  click(h, "poolsTableHead", "strategy");
  assert.deepEqual(order(), ["p2", "p1", "p3"]);
  click(h, "poolsTableHead", "strategy");
  click(h, "poolsTableHead", "name");
  click(h, "poolsTableHead", "name");
  assert.deepEqual(order(), ["p3", "p2", "p1"]);

  // An open cost preview is a detail row that follows its pool.
  const calls = [];
  app.modules.core.api = async (url) => {
    calls.push(url);
    const member = (target, cost, tier) => ({ target, upstream: "u-" + target, resolved_via: "alias", eligible: true, priced: cost !== null, estimated_cost_usd: cost, tier, phase: "ready", in_flight: 0, max_concurrency: 0 });
    return { pool: "p1", routing_strategy: "cost", active: true, paused: false, model: "alias", generated_at: "now", pricing_available: true, tolerance: 0.05, reference_tokens: { input: 1, cached_input: 1, output: 1 }, members: [member("a", 14.7, 1), member("b", 9.45, 0), member("c", null, null)] };
  };
  h.ids.get("poolsTableBody").listeners.click[0]({ target: { dataset: { costPreviewPool: "p1" } } });
  await flush();
  assert.equal(calls.length, 1);
  const layout = () => [...html(h, "poolsTableBody").matchAll(/class='pool-name'[^>]*>([^<]*)<|id='poolCostDetail'/g)].map((m) => m[1] || "·preview");
  assert.deepEqual(layout(), ["p3", "p2", "p1", "·preview"]);
  h.ids.get("poolCostModel").value = "typed-alias";
  clicks(h, "poolsTableHead", "name", 2); // descending → default → ascending
  assert.deepEqual(layout(), ["p1", "·preview", "p2", "p3"], "the preview row stays under p1");
  assert.equal(h.ids.get("poolCostModel").value, "typed-alias", "the request name typed into the preview survives the sort");
  click(h, "poolsTableHead", "name");
  assert.deepEqual(layout(), ["p3", "p2", "p1", "·preview"], "...and under p1 when the order is reversed");
  clicks(h, "poolsTableHead", "name", 2);
  const members = () => [...html(h, "poolCostResult").matchAll(/<tr class='[^']*'><td>(\w+)<\/td>/g)].map((m) => m[1]);
  assert.deepEqual(members(), ["a", "b", "c"], "the preview keeps the server's order by default");
  assert.match(html(h, "poolCostResult"), /<th data-sort='cost'>/);
  click(h, "poolsTableBody", "cost");
  assert.deepEqual(members(), ["b", "a", "c"], "estimated cost ascending; the unpriced member is last");
  click(h, "poolsTableBody", "cost");
  assert.deepEqual(members(), ["a", "b", "c"], "descending keeps it last");
  click(h, "poolsTableBody", "tier");
  assert.deepEqual(members(), ["b", "a", "c"], "tier 0 before tier 1, no tier last");
  assert.equal(calls.length, 1, "sorting the preview does not request it again");
});

// ---------------------------------------------------------------- usage

const aggregate = (overrides = {}) => ({ totals: {}, buckets: [], by_client: [], by_model: [], by_model_group_model: [], by_target_model: [], ...overrides });

test("usage: client and model tables keep their default order and cycle through the shared sorter", async () => {
  const { h, app, state } = await boot();
  state.modelGroupsLoaded = true;
  const clients = [
    { key: "alice", requests: 5, input_tokens: 100, output_tokens: 0, estimated_cost: 3 },
    { key: "bob", requests: 50, input_tokens: 10, output_tokens: 0, estimated_cost: 9 },
    { key: "carol", input_tokens: 40, output_tokens: 0, estimated_cost: 3 },
    { key: "dave", requests: 1, estimated_cost: null },
  ];
  h.context.fetch = async () => json(aggregate({ by_client: clients }));
  await app.modules.usage.loadUsage();
  const order = () => [...html(h, "usageByClientBody").matchAll(/<tr><td>(\w+)<\/td>/g)].map((m) => m[1]);
  assert.deepEqual(order(), ["bob", "alice", "carol", "dave"], "default: cost descending, then total tokens descending; no cost is last");
  click(h, "usageByClientTableHead", "requests");
  assert.deepEqual(order(), ["dave", "alice", "bob", "carol"], "requests ascending; carol reports none: last");
  click(h, "usageByClientTableHead", "requests");
  assert.deepEqual(order(), ["bob", "alice", "dave", "carol"]);
  click(h, "usageByClientTableHead", "requests");
  assert.deepEqual(order(), ["bob", "alice", "carol", "dave"], "third click: the default order again");
  click(h, "usageByClientTableHead", "key");
  click(h, "usageByClientTableHead", "key");
  assert.deepEqual(order(), ["dave", "carol", "bob", "alice"]);
  click(h, "usageByClientTableHead", "total");
  assert.deepEqual(order(), ["bob", "carol", "alice", "dave"], "total tokens ascending: 10, 40, 100; dave reports none and is last");
  click(h, "usageByClientTableHead", "cost");
  assert.deepEqual(order(), ["alice", "carol", "bob", "dave"], "cost ascending; alice and carol tie and keep the default order; no cost is last");
});

test("usage: the model-group tree sorts every level by the same column", async () => {
  const { h, app, state } = await boot();
  state.modelGroupsLoaded = true;
  state.usageModelTargetExpanded = Object.create(null);
  state.usageModelTargetExpanded["G1\x00t1"] = true;
  const rows = [
    { model_group: "G1", target: "t1", model: "m1", input_tokens: 10, estimated_cost: 1 },
    { model_group: "G1", target: "t1", model: "m2", input_tokens: 50, estimated_cost: 0.5 },
    { model_group: "G1", target: "t2", model: "m3", input_tokens: 5, estimated_cost: 8 },
    { model_group: "G2", target: "t9", model: "m9", input_tokens: 20, estimated_cost: 2 },
  ];
  h.context.fetch = async () => json(aggregate({ by_target_model: rows }));
  await app.modules.usage.loadUsage();
  const tree = () => [...html(h, "usageByModelGroupBody").matchAll(/usage-(group|target|model)-row'><td[^>]*>(?:<button[\s\S]*?<\/button> )?(?:<strong>)?(?:↳ )?([\w]+)/g)].map((m) => m[2]);
  assert.deepEqual(tree(), ["G1", "t1", "m2", "m1", "t2", "G2", "t9"], "default: total tokens descending at each level");
  click(h, "usageByModelGroupTableHead", "total");
  assert.deepEqual(tree(), ["G2", "t9", "G1", "t2", "t1", "m1", "m2"], "total ascending at each level");
  click(h, "usageByModelGroupTableHead", "total");
  assert.deepEqual(tree(), ["G1", "t1", "m2", "m1", "t2", "G2", "t9"], "descending is the default order here");
  click(h, "usageByModelGroupTableHead", "total");
  click(h, "usageByModelGroupTableHead", "cost");
  assert.deepEqual(tree(), ["G2", "t9", "G1", "t1", "m2", "m1", "t2"], "cost ascending: G2 2 < G1 9.5; t1 1.5 < t2 8; m2 0.5 < m1 1");
  click(h, "usageByModelGroupTableHead", "cost");
  assert.deepEqual(tree(), ["G1", "t2", "t1", "m1", "m2", "G2", "t9"]);
  click(h, "usageByModelGroupTableHead", "cost");
  click(h, "usageByModelGroupTableHead", "model_group");
  assert.deepEqual(tree(), ["G1", "t1", "m1", "m2", "t2", "G2", "t9"], "names ascending at each level");
  click(h, "usageByModelGroupTableHead", "model_group");
  assert.deepEqual(tree(), ["G2", "t9", "G1", "t2", "t1", "m2", "m1"]);
  // Expanding or collapsing keeps the chosen order.
  h.ids.get("usageByModelGroupBody").listeners.click[0]({ target: { closest: () => ({ dataset: { usageExpandKind: "group", usageExpandKey: "G2" } }) } });
  assert.deepEqual(tree(), ["G2", "G1", "t2", "t1", "m2", "m1"], "a collapsed group hides its targets, the order holds");
});

// ---------------------------------------------------------------- schema summary

test("schema summary: the reference table sorts by any of its columns and repaints its regenerated header", async () => {
  const { h } = await boot();
  const rows = () => [...html(h, "adminSchemaSummary").matchAll(/<tr><td>([^<]*)<\/td><td><code>([^<]*)<\/code><\/td><td>([^<]*)<\/td><\/tr>/g)].map((m) => ({ category: m[1], value: m[2], detail: m[3] }));
  const collator = new Intl.Collator("zh-CN", { numeric: true, sensitivity: "base" });
  const before = rows();
  assert.ok(before.length > 20 && before[0].category === "上游类型", "default: the order the summary is built in");
  const ordered = (list, key, dir) => list.every((row, i) => i === 0 || collator.compare(list[i - 1][key], row[key]) * dir <= 0);
  assert.equal(ordered(before, "value", 1), false, "the default order is not already sorted by value");
  click(h, "adminSchemaSummary", "value");
  assert.equal(rows().length, before.length);
  assert.ok(ordered(rows(), "value", 1), "ascending by the shown value");
  const header = h.ids.get("adminSchemaSummary").querySelectorAll("th[data-sort]").find((th) => th.getAttribute("data-sort") === "value");
  assert.equal(header.getAttribute("aria-sort"), "ascending", "the rebuilt header shows the sort");
  assert.ok(header.classList.contains("sorted-asc"));
  click(h, "adminSchemaSummary", "value");
  assert.ok(ordered(rows(), "value", -1), "descending");
  click(h, "adminSchemaSummary", "value");
  assert.deepEqual(rows(), before, "the third click restores the default order");
  click(h, "adminSchemaSummary", "detail");
  assert.ok(ordered(rows(), "detail", 1));
  click(h, "adminSchemaSummary", "category");
  assert.ok(ordered(rows(), "category", 1));
  const columns = h.ids.get("adminSchemaSummary").querySelectorAll("th[data-sort]").map((th) => th.getAttribute("data-sort"));
  assert.deepEqual(columns, ["category", "value", "detail"]);
});
