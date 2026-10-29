"use strict";
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { createHarness, schemaFixture, withETag, simulateTargetUpdate } = require("../../../scripts/testing/admin_ui_test.cjs");

function fixture() {
  const schema = schemaFixture();
  Object.assign(schema.defaults, { openai_prefix: "", anthropic_prefix: "", supports_responses: false });
  return schema;
}
async function setup(schema = fixture()) {
  const harness = createHarness({ schema, routes: { "/admin/data/targets": { targets: [] } } });
  const app = await harness.run();
  return { harness, app, el: id => harness.ids.get(id) };
}
test("dual controls use capabilities, schema defaults, and preserve edits across type switches", async () => {
  const schema = fixture();
  // Deliberately use future provider names and nonzero defaults: UI must consume schema.
  Object.assign(schema.defaults, { openai_prefix: "/schema-openai", anthropic_prefix: "/schema-anthropic", supports_responses: true });
  const { app, el } = await setup(schema);
  await app.modules.targets.openTargetModal(null);
  assert.equal(el("tgtDualProtocolGroup").style.display, "");
  assert.equal(el("tgtOpenAIPrefix").value, "/schema-openai");
  assert.equal(el("tgtAnthropicPrefix").value, "/schema-anthropic");
  assert.equal(el("tgtSupportsResponses").checked, true);
  await app.modules.targets.openTargetModal({ name: "saved", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", usage_adapter: "qwen", openai_prefix: "", anthropic_prefix: "/saved", supports_responses: false });
  assert.equal(el("tgtOpenAIPrefix").value, "", "empty persisted string wins over default");
  assert.equal(el("tgtAnthropicPrefix").value, "/saved");
  for (const adapter of ["standard", "qwen", "glm", "deepseek", "kimi"]) {
    el("tgtUsageAdapter").value = adapter;
    el("tgtEndpoint").value = "https://dashscope.aliyuncs.com";
    app.modules.targets.updateTargetFormVisibility();
    assert.equal(el("tgtOpenAIPrefix").value, "", "usage adapter and host do not infer prefixes");
  }
  assert.equal(el("tgtSupportsResponses").checked, false, "persisted false wins over default");
  el("tgtOpenAIPrefix").value = "/typed";
  el("tgtSupportsResponses").checked = true;
  el("tgtType").value = "provider-other";
  app.modules.targets.updateTargetFormVisibility();
  assert.equal(el("tgtDualProtocolGroup").style.display, "none");
  assert.equal(el("tgtOpenAIPrefix").value, "/typed");
  assert.equal(el("tgtSupportsResponses").checked, true);
  el("tgtType").value = "provider-new";
  app.modules.targets.updateTargetFormVisibility();
  assert.equal(el("tgtDualProtocolGroup").style.display, "");
  assert.equal(el("tgtAnthropicPrefix").value, "/saved");
});
test("saving a non-dual type keeps nonempty saved dual values", async () => {
  const { app, harness, el } = await setup();
  const saved = { name: "saved", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", usage_adapter: "standard", openai_prefix: "/openai", anthropic_prefix: "/anthropic", supports_responses: true };
  await app.modules.targets.openTargetModal(withETag(saved, 1));
  el("tgtType").value = "provider-other";
  app.modules.targets.updateTargetFormVisibility();
  simulateTargetUpdate(harness, withETag(saved, 1), withETag({ ...saved, endpoint_type: "provider-other" }, 2));
  await el("saveTargetBtn").listeners.click[0]();
  const write = harness.requests.findLast(r => r.options?.method === "PUT");
  const body = JSON.parse(write.options.body);
  assert.equal(write.options.headers["If-Match"], '"t1-1"');
  assert.equal(body.api_key, "__existing_key_id__:kid-fixture-0");
  assert.equal(app.state.targets[0].etag, "t1-2");
  assert.equal(body.openai_prefix, "/openai");
  assert.equal(body.anthropic_prefix, "/anthropic");
  assert.equal(body.supports_responses, true);
});
test("create and update submit all dual fields, including clear values and hidden retained values", async () => {
  const { app, harness, el } = await setup();
  for (const editing of [false, true]) {
    const saved = { name: "saved", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", usage_adapter: "standard", openai_prefix: "/before", anthropic_prefix: "/before", supports_responses: true };
    await app.modules.targets.openTargetModal(editing ? withETag(saved, 1) : null);
    el("tgtName").value = "saved";
    el("tgtEndpoint").value = "https://fixture.invalid";
    el("tgtUsageAdapter").value = "standard";
    el("tgtOpenAIPrefix").value = editing ? "" : "/openai";
    el("tgtAnthropicPrefix").value = editing ? "" : "/anthropic";
    el("tgtSupportsResponses").checked = !editing;
    if (editing) {
      el("tgtType").value = "provider-other";
      app.modules.targets.updateTargetFormVisibility();
      simulateTargetUpdate(harness, withETag(saved, 1), withETag({ ...saved, endpoint_type: "provider-other", openai_prefix: "", anthropic_prefix: "", supports_responses: false }, 2));
    }
    await el("saveTargetBtn").listeners.click[0]();
    const request = harness.requests.findLast(r => r.options?.method === (editing ? "PUT" : "POST"));
    const body = JSON.parse(request.options.body);
    if (editing) {
      assert.equal(request.options.headers["If-Match"], '"t1-1"');
      assert.equal(body.api_key, "__existing_key_id__:kid-fixture-0");
      assert.equal(app.state.targets[0].etag, "t1-2");
    }
    assert.equal(body.openai_prefix, editing ? "" : "/openai");
    assert.equal(body.anthropic_prefix, editing ? "" : "/anthropic");
    assert.equal(body.supports_responses, !editing);
  }
});
test("HTTP model test explains its scope and limits its success claim", async () => {
  const { app, harness, el } = await setup();
  await app.modules.targets.openTargetModal({ name: "saved", endpoint_type: "provider-new", endpoint: "https://fixture.invalid", model_mappings: [{ upstream: "qwen-max" }] });
  const row = el("model-mappings-container").children[0];
  const button = row.children.find(child => child.className?.includes("test-model-btn"));
  assert.match(button.title, /此检测发送最小 HTTP 请求检查上游响应；不验证 WebSocket 通道/);
  harness.context.fetch = async () => ({ ok: true, status: 200, text: async () => JSON.stringify({ success: true, status_code: 200, latency_ms: 1 }) });
  app.modules.targets.testModelForRow(button, "qwen-max");
  await new Promise(resolve => setImmediate(resolve));
  assert.match(el("toastContainer").children.at(-1).textContent, /HTTP 检测通过/);
  assert.doesNotMatch(el("toastContainer").children.at(-1).textContent, /测试成功/);
});
test("missing dual defaults prevents initialization and editing", async () => {
  for (const field of ["openai_prefix", "anthropic_prefix", "supports_responses"]) {
    const schema = fixture();
    delete schema.defaults[field];
    const { app } = await setup(schema);
    assert.equal(app.state.ready, false);
    assert.equal(app.modules.targets, undefined);
  }
});
