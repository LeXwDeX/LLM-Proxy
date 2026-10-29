"use strict";
// Pricing page regressions: period 24:00, bad numeric input, in-flight guards, load/open version
// guards, seconds preservation and rate display precision. Uses the real scripts in the vm fixture.
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../../scripts/testing/admin_ui_test.cjs");

const target = {name: "t1", endpoint_type: "openai", model_mappings: [{upstream: "m1"}]};
const json = value => ({ok: true, status: 200, text: async () => JSON.stringify(value)});
const tick = () => new Promise(resolve => setImmediate(resolve));

async function fixture(extraRoutes = {}) {
  const h = createHarness({routes: {"/admin/data/targets": {targets: [target]},
    "/admin/data/catalog": {models: []}, "/admin/data/pricing-templates": {templates: []}, ...extraRoutes}});
  const app = await h.run();
  return {h, app, pricing: app.modules.pricing};
}
const posts = h => h.requests.filter(r => r.url === "/admin/data/pricing-templates" && r.options?.method === "POST");
const puts = h => h.requests.filter(r => String(r.url).startsWith("/admin/data/pricing-templates/") && r.options?.method === "PUT");
function selectScope(h) {
  const t = h.ids.get("ptTarget");
  t.options.forEach(o => { o.selected = false; });
  t.options.find(o => o.value === "t1").selected = true;
  t.listeners.change[0]();
  h.ids.get("ptModel").options.find(o => o.value === "m1").selected = true;
  h.ids.get("ptModel").listeners.change[0]();
}
function fillRates(h, rates = {}) {
  const defaults = {ptInput: "1", ptOutput: "2", ptCacheWrite: "0", ptCacheRead: "0.5"};
  for (const [id, value] of Object.entries({...defaults, ...rates})) h.ids.get(id).value = value;
}
async function openNew(h, pricing) {
  await pricing.openPricingModal(null, null);
  selectScope(h);
}
const save = h => h.ids.get("saveModelCostBtn").listeners.click[0]();

test("flat create submits explicit 0 and null for blank optional prices", async () => {
  const {h, pricing} = await fixture();
  await openNew(h, pricing);
  fillRates(h, {ptExplicitRead: "0", ptWrite5m: "", ptImageInput: "0"});
  await save(h);
  assert.equal(posts(h).length, 1);
  const body = JSON.parse(posts(h)[0].options.body);
  assert.deepEqual(body.base_rates, {
    input_per_1m_tokens: 1, output_per_1m_tokens: 2, cache_write_per_1m_tokens: 0, cache_read_per_1m_tokens: 0.5,
    explicit_cache_read_per_1m_tokens: 0, cache_write_5m_per_1m_tokens: null, cache_write_1h_per_1m_tokens: null,
    image_input_per_1m_tokens: 0, cached_image_input_per_1m_tokens: null, image_output_per_1m_tokens: null,
  });
});

test("optional price with unparseable input is rejected instead of becoming null", async () => {
  const {h, pricing} = await fixture();
  await openNew(h, pricing);
  fillRates(h);
  h.ids.get("ptWrite5m").value = "";
  h.ids.get("ptWrite5m").validity = {badInput: true};
  await save(h);
  assert.equal(posts(h).length, 0);
  h.ids.get("ptWrite5m").validity = {badInput: false};
  await save(h);
  assert.equal(posts(h).length, 1);
});

test("required price with unparseable input is rejected", async () => {
  const {h, pricing} = await fixture();
  await openNew(h, pricing);
  fillRates(h);
  h.ids.get("ptOutput").value = "";
  h.ids.get("ptOutput").validity = {badInput: true};
  await save(h);
  assert.equal(posts(h).length, 0);
});

async function openSchedule(h, pricing, period) {
  const template = {id: "tpl-1", targets: ["t1"], models: ["m1"], mode: "schedule", timezone: "+08:00",
    start: "2026-01-01T00:00:00Z", version: 1, base_rates: {input_per_1m_tokens: 1, output_per_1m_tokens: 1,
      cache_write_per_1m_tokens: 1, cache_read_per_1m_tokens: 1},
    periods: [period]};
  await pricing.openPricingModal(template, null);
  fillRates(h); // a real input stringifies numeric values; the fixture elements do not
  return installPeriodCards(h);
}
// The fixture DOM is flat, so rebuild period cards from the rendered markup: each card exposes its
// inputs by data attribute the way the real querySelector would. Tests mutate the returned inputs.
function installPeriodCards(h) {
  const html = h.ids.get("ptPeriods").innerHTML;
  const cards = html.split("data-pt-period='").slice(1).map(segment => {
    const index = segment.slice(0, segment.indexOf("'"));
    const inputs = {};
    for (const m of segment.matchAll(/<input\b([^>]*)>/g)) {
      const key = (m[1].match(/data-pt-(\w+)/) || [])[1];
      if (!key || key === "wd") continue;
      inputs[key.toLowerCase()] = {value: ((m[1].match(/value='([^']*)'/) || [])[1]) || "", validity: {badInput: false}};
    }
    const weekdays = [...segment.matchAll(/data-pt-wd='(\d)'( checked)?/g)].filter(m => m[2]).map(m => ({dataset: {ptWd: m[1]}}));
    return {dataset: {ptPeriod: index}, inputs,
      querySelectorAll: selector => selector === "[data-pt-wd]:checked" ? weekdays : [],
      querySelector(selector) { return inputs[selector.replace(/^\[data-pt-|\]$/g, "").toLowerCase()] || null; }};
  });
  const original = h.document.querySelectorAll;
  h.document.querySelectorAll = selector => selector === "#ptPeriods [data-pt-period]" ? cards : original(selector);
  return cards;
}
const period = (start, end) => ({weekdays: [1, 2], start, end, rates: {input_per_1m_tokens: 3, output_per_1m_tokens: 3,
  cache_write_per_1m_tokens: 3, cache_read_per_1m_tokens: 3}});

test("a period ending at 24:00 is editable and round-trips unchanged", async () => {
  const {h, pricing} = await fixture();
  await openSchedule(h, pricing, period("18:00", "24:00"));
  const html = h.ids.get("ptPeriods").innerHTML;
  assert.doesNotMatch(html, /type=['"]time['"]/, "native time inputs cannot represent 24:00");
  assert.match(html, /data-pt-end[^>]*value='24:00'/);
  await save(h);
  assert.equal(puts(h).length, 1);
  const body = JSON.parse(puts(h)[0].options.body);
  assert.equal(body.periods[0].end, "24:00");
  assert.equal(body.periods[0].start, "18:00");
});

test("period times are validated: 24:00 only as an end, HH:mm shape required", async () => {
  const {h, pricing} = await fixture();
  for (const [start, end] of [["24:00", "24:00"], ["25:00", "26:00"], ["9", "18:00"], ["09:00", "24:30"], ["09:00", "24:00:00"]]) {
    const cards = await openSchedule(h, pricing, period("09:00", "18:00"));
    cards[0].inputs.start.value = start;
    cards[0].inputs.end.value = end;
    await save(h);
    assert.equal(puts(h).length, 0, `${start}-${end} must be rejected`);
  }
  const cards = await openSchedule(h, pricing, period("09:00", "18:00"));
  cards[0].inputs.start.value = "00:00";
  cards[0].inputs.end.value = "24:00";
  await save(h);
  assert.equal(puts(h).length, 1);
  assert.equal(JSON.parse(puts(h)[0].options.body).periods[0].end, "24:00");
});

test("period rate with unparseable input is rejected", async () => {
  const {h, pricing} = await fixture();
  const cards = await openSchedule(h, pricing, period("09:00", "18:00"));
  cards[0].inputs.write5m.value = "";
  cards[0].inputs.write5m.validity.badInput = true;
  await save(h);
  assert.equal(puts(h).length, 0, "an unparseable optional period price must not become null");
  cards[0].inputs.write5m.validity.badInput = false;
  await save(h);
  assert.equal(puts(h).length, 1);
  assert.equal(JSON.parse(puts(h)[0].options.body).periods[0].rates.cache_write_5m_per_1m_tokens, null);
});

test("saving twice quickly submits once and re-enables the button afterwards", async () => {
  const {h, pricing} = await fixture();
  await openNew(h, pricing);
  fillRates(h);
  const base = h.context.fetch;
  let release;
  h.context.fetch = (url, options) => options?.method === "POST" && url === "/admin/data/pricing-templates"
    ? new Promise(resolve => { release = () => resolve(json({})); h.requests.push({url, options}); }) : base(url, options);
  const first = save(h);
  const second = save(h);
  await tick();
  assert.equal(posts(h).length, 1);
  assert.equal(h.ids.get("saveModelCostBtn").disabled, true);
  release();
  await first; await second;
  assert.equal(h.ids.get("saveModelCostBtn").disabled, false);
});

test("a failed pricing load does not leave the table on 加载中", async () => {
  const {h, pricing} = await fixture();
  h.ids.get("modelCostsTableBody").innerHTML = "<tr class='empty-row'><td>加载中...</td></tr>";
  h.setRouteFailure("/admin/data/pricing-templates", true);
  await pricing.loadPricingTemplates();
  assert.doesNotMatch(h.ids.get("modelCostsTableBody").innerHTML, /加载中/);
  assert.match(h.ids.get("modelCostsTableBody").innerHTML, /加载失败/);
});

test("an older pricing load cannot overwrite a newer one", async () => {
  const {h, app, pricing} = await fixture();
  await app.modules.quota.loadQuotaEditorData(); // targets present: requests are issued in call order
  const base = h.context.fetch;
  const pending = [];
  h.context.fetch = (url, options) => url === "/admin/data/pricing-templates" && !options?.method
    ? new Promise(resolve => pending.push(resolve)) : base(url, options);
  const first = pricing.loadPricingTemplates();
  const second = pricing.loadPricingTemplates();
  for (let i = 0; i < 50 && pending.length < 2; i++) await tick();
  assert.equal(pending.length, 2);
  pending[1](json({templates: [{id: "new", targets: ["t1"], models: ["m1"], mode: "flat", base_rates: {}}]}));
  await second;
  pending[0](json({templates: [{id: "old", targets: ["t1"], models: ["m1"], mode: "flat", base_rates: {}}]}));
  await first;
  assert.equal(app.state.pricingTemplates[0].id, "new");
});

test("a slow earlier open cannot overwrite a later modal open", async () => {
  const {h, app, pricing} = await fixture();
  const base = h.context.fetch;
  const pending = [];
  h.context.fetch = (url, options) => url === "/admin/data/targets"
    ? new Promise(resolve => pending.push(() => resolve(base(url, options)))) : base(url, options);
  const first = pricing.openPricingModal(null, null);
  const second = pricing.openPricingModal({id: "tpl-9", targets: ["t1"], models: ["m1"], mode: "flat", base_rates: {}}, null);
  await tick();
  pending.forEach(release => release());
  await Promise.all([first, second]);
  assert.equal(app.state.editingPricingTemplate.id, "tpl-9");
  assert.equal(h.ids.get("modelCostModalTitle").textContent, "编辑模型计费");
});

test("start and end keep seconds and exact stored instants when unrelated fields change", async () => {
  const {h, pricing} = await fixture();
  const template = {id: "tpl-2", targets: ["t1"], models: ["m1"], mode: "flat", timezone: "+08:00",
    start: "2026-03-01T01:02:03Z", end: "2026-04-01T04:05:06.123456Z", version: 1,
    base_rates: {input_per_1m_tokens: 1, output_per_1m_tokens: 1, cache_write_per_1m_tokens: 1, cache_read_per_1m_tokens: 1}};
  await pricing.openPricingModal(template, null);
  assert.match(h.ids.get("ptStart").value, /:03/, "seconds are shown");
  fillRates(h, {ptInput: "9"});
  await save(h);
  const body = JSON.parse(puts(h)[0].options.body);
  assert.equal(new Date(body.start).getTime(), new Date(template.start).getTime());
  assert.equal(body.end, template.end);
});

test("rates are displayed with every stored digit", async () => {
  const {pricing} = await fixture();
  assert.equal(pricing.pricingRate(0.1234567), "$0.1234567");
  assert.equal(pricing.pricingRate(0.5), "$0.5");
  assert.equal(pricing.pricingRate(0), "$0");
  assert.equal(pricing.pricingRate(3), "$3");
  assert.equal(pricing.pricingRate(1e-7), "$0.0000001");
  assert.equal(pricing.pricingRate(null), "$0");
});

test("period times accept H:mm like the backend; only edited fields are normalized", async () => {
  const {h, pricing} = await fixture();
  let cards = await openSchedule(h, pricing, period("9:00", "24:00"));
  await save(h);
  let body = JSON.parse(puts(h)[0].options.body);
  assert.equal(body.periods[0].start, "9:00", "an unchanged stored value is sent verbatim");
  cards = await openSchedule(h, pricing, period("9:00", "10:00"));
  assert.equal(puts(h).length, 1);
  await save(h);
  assert.equal(puts(h).length, 2, "9:00-10:00 is ordered by minutes, not as strings");
  cards = await openSchedule(h, pricing, period("09:00", "18:00"));
  cards[0].inputs.start.value = "9:30";
  await save(h);
  body = JSON.parse(puts(h)[2].options.body);
  assert.equal(body.periods[0].start, "09:30", "an edited H:mm value is normalized to HH:mm");
  cards = await openSchedule(h, pricing, period("09:00", "18:00"));
  cards[0].inputs.start.value = "9:60";
  await save(h);
  assert.equal(puts(h).length, 3);
});
