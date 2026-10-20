"use strict";
const assert = require("node:assert/strict");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

async function main() {
  const harness = createHarness();
  const app = await harness.run();
  const trace = app.modules.audit_trace;
  const table = harness.ids.get("traceTableBody");
  // The request ID is supplied by a proxy client and persisted unchanged.
  const id = 'x");globalThis.traceExecuted=true;//\'&<>';
  for (const render of [
    () => trace.renderTraceTable([{ trace_id: id }], {}),
    () => { table.innerHTML = ""; trace.appendTraceRecords([{ trace_id: id }]); },
  ]) {
    render();
    assert.doesNotMatch(table.innerHTML, /\bonclick\s*=/i,
      "client trace IDs must never enter executable inline handlers");
    const encoded = table.innerHTML.match(/data-trace-id='([^']*)'/)?.[1];
    assert.ok(encoded, "rendered trace details carry a data attribute");
    const decoded = encoded.replace(/&lt;/g, "<").replace(/&gt;/g, ">")
      .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, "&");
    assert.equal(decoded, id, "HTML decoding preserves the identifier as data");
    for (const listener of table.listeners.click || [])
      await listener({ target: { closest: () => ({ dataset: { traceId: decoded } }) } });
    assert.ok(harness.requests.some(request => request.url ===
      "/admin/data/trace/" + encodeURIComponent(id)), "delegated click uses the exact ID");
    assert.equal(harness.context.traceExecuted, undefined);
  }
  console.log("PASS trace table and pagination IDs stay data during rendering and clicks");
}
main().catch(error => { console.error(error); process.exitCode = 1; });
