"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness, schemaFixture } = require("../../../scripts/testing/admin_ui_test.cjs");
test("stream usage switch uses schema capability and round-trips true/false", async () => {
  const schema = schemaFixture();
  schema.defaults.request_stream_usage_if_missing = false;
  schema.stream_usage_request_schema = "fixture-chat";
  schema.request_schemas.push({value:"fixture-chat",label:"Fixture Chat"});
  schema.endpoint_types[0].request_schemas.push("fixture-chat");
  const harness = createHarness({ schema }); const app = await harness.run(); const el = id => harness.ids.get(id);
  await app.modules.targets.openTargetModal(null);
  assert.equal(el("tgtStreamUsageGroup").style.display, "");
  assert.equal(el("tgtRequestStreamUsage").checked, false);
  for (const editing of [false,true]) {
    await app.modules.targets.openTargetModal(editing ? {name:"saved",endpoint_type:schema.endpoint_types[0].code,endpoint:"https://fixture.invalid",usage_adapter:"standard",request_stream_usage_if_missing:true} : null);
    if(editing) assert.equal(el("tgtRequestStreamUsage").checked,true);
    el("tgtName").value="saved";el("tgtEndpoint").value="https://fixture.invalid";el("tgtUsageAdapter").value="standard";
    el("tgtRequestStreamUsage").checked=!editing;
    el("tgtType").value=schema.endpoint_types[1].code;app.modules.targets.updateTargetFormVisibility();
    assert.equal(el("tgtStreamUsageGroup").style.display,"none");
    await el("saveTargetBtn").listeners.click[0]();
    assert.equal(JSON.parse(harness.requests.findLast(r=>r.options?.method===(editing?"PUT":"POST")).options.body).request_stream_usage_if_missing,!editing);
  }
});
test("missing stream usage schema disables editing", async()=>{
  for(const field of ["request_stream_usage_if_missing","stream_usage_request_schema"]) {
    const schema=schemaFixture(); if(field==="stream_usage_request_schema") delete schema[field]; else delete schema.defaults[field];
    const app=await createHarness({schema}).run(); assert.equal(app.state.ready,false);
  }
});
