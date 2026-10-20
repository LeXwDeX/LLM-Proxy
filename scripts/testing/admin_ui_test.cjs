"use strict";
// Run the embedded scripts in their actual HTML order using only Node built-ins.
// The DOM fixture focuses on boot, schema-driven controls and cross-module behavior.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const root = path.resolve(__dirname, "../..");
const html = fs.readFileSync(
  path.join(root, "internal/admin/ui/index.html"),
  "utf8",
);
const scripts = [
  ...html.matchAll(
    /<script\s+src="\/admin\/ui\/js\/([^"/]+)"[^>]*><\/script>/g,
  ),
].map((match) => ({
  name: match[1],
  source: fs.readFileSync(
    path.join(root, "internal/admin/ui/js", match[1]),
    "utf8",
  ),
}));
function decode(value) {
  return String(value)
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, "&");
}
class Element {
  constructor(tagName, attrs = {}) {
    this.tagName = tagName.toUpperCase();
    this.attrs = attrs;
    this.id = attrs.id || "";
    this.children = [];
    this.value = attrs.value || "";
    this._html = "";
    this.textContent = "";
    this.style = {};
    this.listeners = {};
    this.disabled = false;
    this.hidden = false;
    this.dataset = {};
    for (const [key, value] of Object.entries(attrs))
      if (key.startsWith("data-"))
        this.dataset[
          key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())
        ] = value;
    const classes = new Set((attrs.class || "").split(/\s+/).filter(Boolean));
    this.classList = {
      contains: (name) => classes.has(name),
      add: (name) => classes.add(name),
      remove: (name) => classes.delete(name),
      toggle: (name, force) => {
        const on = force === undefined ? !classes.has(name) : force;
        on ? classes.add(name) : classes.delete(name);
        return on;
      },
    };
  }
  get innerHTML() {
    return this._html;
  }
  set innerHTML(value) {
    this._html = value;
    this.children = [];
    if (this.registerChildren) this.registerChildren(value);
    if (this.tagName === "SELECT") {
      for (const match of value.matchAll(
        /<option\b[^>]*value=["']([^"']*)["'][^>]*>(.*?)<\/option>/g,
      )) {
        const option = new Element("option", { value: decode(match[1]) });
        option.textContent = decode(match[2]);
        this.children.push(option);
      }
      this.value = this.children[0]?.value || "";
    } else {
      for (const match of value.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
        const attrs = Object.fromEntries(
          [...match[2].matchAll(/([\w-]+)=["']([^"']*)["']/g)]
            .map((attr) => [attr[1], decode(attr[2])]),
        );
        this.children.push(new Element(match[1], attrs));
      }
    }
  }
  get options() {
    return this.children.filter((child) => child.tagName === "OPTION");
  }
  appendChild(child) {
    if (child.parentNode) child.parentNode.removeChild(child);
    child.parentNode = this;
    this.children.push(child);
    return child;
  }
  removeChild(child) { this.children.splice(this.children.indexOf(child), 1); child.parentNode = null; }
  append(...children) {
    this.children.push(...children);
  }
  addEventListener(name, fn) {
    (this.listeners[name] ||= []).push(fn);
  }
  setAttribute(name, value) {
    this.attrs[name] = value;
  }
  getAttribute(name) {
    return this.attrs[name] ?? null;
  }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [child, ...child.querySelectorAll("*")])
      .filter((child) => selector === "*" ||
        (selector.startsWith(".") ? (child.className || child.attrs.class || "").split(/\s+/).includes(selector.slice(1))
          : child.tagName === selector.toUpperCase()));
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
  closest() {
    return null;
  }
  focus() { this.focused = true; }
  remove() {}
}
function option(value, label = value) {
  return { value, label };
}
function schemaFixture() {
  return {
    version: 1,
    stream_usage_request_schema: "shape-new",
    endpoint_types: [
      {
        code: "provider-new",
        display_name: "未来 Provider",
        short_label: "未来",
        protocols: ["proto-new"],
        request_schemas: ["shape-new"],
        supports_bearer_passthrough: true,
        supports_auth_mode: true,
        supports_image_operation: true,
        supports_dual_protocol: true,
        supports_prefer_websocket: true,
        endpoint_placeholder: "https://future.example",
        endpoint_hint: "未来上游提示",
        badge_background: "#fff",
        badge_foreground: "#000",
      },
      {
        code: "provider-other",
        display_name: "其他 Provider",
        protocols: [],
        request_schemas: [],
        supports_bearer_passthrough: false,
        supports_auth_mode: false,
        supports_image_operation: false,
        supports_dual_protocol: false,
      },
    ],
    protocols: [option("proto-new", "新协议")],
    request_schemas: [option("shape-new", "新请求")],
    usage_adapters: [
      option("standard", "原厂规则"), option("glm", "GLM"),
      option("deepseek", "DeepSeek"), option("qwen", "Qwen"),
      option("kimi", "Kimi"),
    ],
    image_operations: [option("image-new", "新图片操作")],
    auth_modes: [option("", "默认认证"), option("auth-new", "新认证")],
    pricing_modes: [option("flat", "全天价格"), option("schedule", "分时价格")],
    pricing_service_tiers: [option("standard", "标准服务"), option("fast", "快速服务"), option("future-tier", "未来档位")],
    pricing_speeds: [option("standard", "标准速度"), option("fast", "快速模式")],
    roles: [option("admin", "管理员"), option("readonly", "只读")],
    quota_scopes: [option("model_group", "模型组")],
    quota_sync_modes: [
      option("clients", "指定客户端"),
      option("job_level", "整个职级"),
    ],
    quota_dimensions: [
      {
        value: "daily",
        label: "Schema日",
        base_field: "daily_usd",
        job_coefficient_field: "coefficient_daily",
      },
      {
        value: "weekly",
        label: "Schema周",
        base_field: "weekly_usd",
        job_coefficient_field: "coefficient_weekly",
      },
      {
        value: "monthly",
        label: "Schema月",
        base_field: "monthly_usd",
        job_coefficient_field: "coefficient_monthly",
      },
    ],
    target_phases: [option("ready", "Schema正常")],
    defaults: {
      endpoint_type: "provider-new",
      usage_adapter: "",
      image_operation: "image-new",
      auth_mode: "",
      pricing_mode: "flat",
      pricing_timezone: "+06:00",
      role: "admin",
      openai_prefix: "",
      anthropic_prefix: "",
      supports_responses: false,
      request_stream_usage_if_missing: false,
    },
  };
}
function createHarness({
  failure = "",
  role = "admin",
  schema = schemaFixture(),
} = {}) {
  const elements = [],
    ids = new Map(),
    requests = [];
  for (const match of html.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
    const attrs = Object.fromEntries(
      [...match[2].matchAll(/([\w-]+)="([^"]*)"/g)].map((attr) => [
        attr[1],
        decode(attr[2]),
      ]),
    );
    const element = new Element(match[1], attrs);
    elements.push(element);
    if (element.id) ids.set(element.id, element);
  }
  const body = elements.find((element) => element.tagName === "BODY");
  const querySelectorAll = (selector) =>
    elements.filter((element) => {
      if (selector === "select.endpoint-type-select")
        return (
          element.tagName === "SELECT" &&
          element.classList.contains("endpoint-type-select")
        );
      if (selector === "select[data-schema-options]")
        return (
          element.tagName === "SELECT" &&
          element.getAttribute("data-schema-options") !== null
        );
      if (/^\.[\w-]+$/.test(selector))
        return element.classList.contains(selector.slice(1));
      if (/^\[[\w-]+\]$/.test(selector))
        return element.getAttribute(selector.slice(1, -1)) !== null;
      return false;
    });
  const registerChildren = (source) => {
    for (const match of source.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
      const attrs = Object.fromEntries(
        [...match[2].matchAll(/([\w-]+)=["']([^"']*)["']/g)].map((attr) => [
          attr[1],
          decode(attr[2]),
        ]),
      );
      if (attrs.id) {
        const element = new Element(match[1], attrs);
        element.registerChildren = registerChildren;
        elements.push(element);
        ids.set(element.id, element);
      }
    }
  };
  for (const element of elements) element.registerChildren = registerChildren;
  const document = {
    body,
    getElementById: (id) => ids.get(id) || null,
    querySelectorAll,
    querySelector: (selector) => querySelectorAll(selector)[0] || null,
    createElement: (tag) => {
      const element = new Element(tag);
      element.registerChildren = registerChildren;
      return element;
    },
    addEventListener() {},
  };
  const sandbox = {
    document,
    console,
    URLSearchParams,
    AbortController,
    setTimeout: () => 1,
    clearTimeout() {},
    setInterval: () => 1,
    clearInterval() {},
    navigator: { clipboard: { writeText: async () => {} } },
    addEventListener() {},
    location: {},
    requestAnimationFrame: (fn) => fn(),
    fetch: async (url, options) => {
      requests.push({ url, options });
      if (url === "/admin/data/schema") {
        if (failure)
          return {
            ok: false,
            status: 503,
            text: async () => JSON.stringify({ error: failure }),
          };
        return {
          ok: true,
          status: 200,
          text: async () => JSON.stringify(schema),
        };
      }
      const data =
        url === "/admin/api/me" ? { username: "fixture-user", role } :
        url === "/admin/api/activity" ? {
          generated_at: new Date().toISOString(),
          window: {from: new Date(Date.now() - 300000).toISOString(), to: new Date().toISOString(), step_seconds: 10},
          runtime: {since: new Date().toISOString(), complete: false, active_requests: 0, active_clients: 0, websocket_sessions: 0},
          totals: {input_tokens: 0, output_tokens: 0, total_tokens: 0, incomplete_usage_count: 0}, series: [], clients: [],
        } : {};
      return { ok: true, status: 200, text: async () => JSON.stringify(data) };
    },
  };
  sandbox.window = sandbox;
  const context = vm.createContext(sandbox);
  return {
    context,
    document,
    ids,
    requests,
    setFailure: (value) => {
      failure = value;
    },
    run: async () => {
      for (const script of scripts)
        vm.runInContext(script.source, context, { filename: script.name });
      if (context.AdminApp?.bootPromise) await context.AdminApp.bootPromise;
      return context.AdminApp;
    },
  };
}
async function main() {
  assert.equal(scripts.length, 14, "all feature scripts are loaded explicitly");
  const harness = createHarness();
  const app = await harness.run();
  assert.equal(app.state.ready, true, "schema success enables startup");
  assert.ok(harness.requests.some(item => item.url === "/admin/api/activity"), "homepage loads live activity");
  assert.ok(!harness.requests.some(item => item.url.startsWith("/admin/data/audit")), "homepage does not query audit logs");
  assert.ok(!html.includes("overviewAuditBody"), "homepage removes the obsolete audit table");
  for (const [id, keywords] of [
    ["usageBillingBasis", ["官方", "不估算", "连接断开，未获取到计费字段", "少于厂商官方账单", "目录默认价"]],
    ["overviewBillingBasis", ["官方", "不估算", "连接断开，未获取到计费字段", "少于厂商官方账单", "目录默认价"]],
    ["pricingBillingBasis", ["官方", "目录默认价", "不估算 token"]],
    ["clientsBillingBasis", ["官方", "目录默认价", "不估算 token"]],
    ["quotaBillingBasis", ["官方", "目录默认价", "不估算 token"]],
  ]) {
    const note = harness.ids.get(id);
    assert.ok(note, `${id} billing basis note is rendered`);
    assert.equal(note.hidden, false, `${id} stays visible after startup`);
    assert.notEqual(note.style.display, "none", `${id} stays visible after startup`);
    assert.notEqual(note.getAttribute("role"), "tooltip", `${id} is a visible note, not a tooltip`);
    const text = html.match(new RegExp(`id="${id}"[^>]*>([\\s\\S]*?)</(?:p|div)>`))[1].replace(/<\/?strong>/g, "");
    for (const keyword of keywords) assert.ok(text.includes(keyword), `${id} states ${keyword}`);
  }
  const expected = [
    "core",
    "schema",
    "navigation",
    "overview",
    "targets",
    "pools",
    "clients",
    "pricing",
    "usage",
    "audit_trace",
    "users",
    "catalog",
    "quota",
  ];
  assert.deepEqual(
    Object.keys(app.modules),
    expected,
    "every registered factory initializes",
  );
  for (const script of scripts) {
    for (const match of script.source.matchAll(
      /app\.bind\("([^"\n]+)", "([^"\n]+)"\)/g,
    )) {
      assert.equal(
        typeof app.modules[match[1]]?.[match[2]],
        "function",
        `${script.name} imports ${match[1]}.${match[2]}`,
      );
    }
  }
  assert.equal(harness.ids.get("adminBootStatus").hidden, true);
  assert.ok(
    harness.ids.get("jobLevel-daily"),
    "quota dimension fields are generated from Schema",
  );
  for (const [id, key] of [
    ["tgtType", "endpoint_types"],
    ["tgtUsageAdapter", "usage_adapters"],
    ["tgtAuthMode", "auth_modes"],
    ["tgtImageOperation", "image_operations"],
    ["userRole", "roles"],
    ["ptMode", "pricing_modes"],
    ["quotaSyncMode", "quota_sync_modes"],
  ]) {
    const actual = harness.ids.get(id).options.map((item) => item.value);
    assert.deepEqual(
      actual,
      (key === "usage_adapters" ? [""] : []).concat(
        Array.from(app.state.schema[key], (item) => item.code ?? item.value),
      ),
      `${id} uses schema options`,
    );
  }
  assert.equal(harness.ids.get("tgtUsageAdapter").options[0].textContent, "请选择计费类型");
  assert.equal(app.state.schema.usage_adapters.some((o) => o.value === ""), false,
    "placeholder is not a business enum value");
  await app.modules.targets.openTargetModal(null);
  assert.equal(harness.ids.get("tgtUsageAdapter").value, "", "new target has no selection");
  harness.ids.get("tgtName").value = "new-target";
  harness.ids.get("tgtEndpoint").value = "https://fixture.example";
  for (const value of ["", "auto", "unknown"]) {
    harness.ids.get("tgtUsageAdapter").value = value;
    harness.requests.length = 0;
    await harness.ids.get("saveTargetBtn").listeners.click[0]();
    assert.equal(harness.requests.some((r) => r.options?.method === "POST"), false,
      `invalid adapter ${value} blocks target creation`);
    assert.equal(harness.ids.get("tgtUsageAdapterError").hidden, false);
    assert.match(harness.ids.get("tgtUsageAdapterError").textContent, /请选择计费类型/);
    assert.equal(harness.ids.get("tgtUsageAdapter").getAttribute("aria-invalid"), "true");
  }
  harness.ids.get("tgtUsageAdapter").value = "standard";
  harness.ids.get("tgtUsageAdapter").listeners.change[0]();
  assert.equal(harness.ids.get("tgtUsageAdapterError").hidden, true);
  await harness.ids.get("saveTargetBtn").listeners.click[0]();
  const created = harness.requests.find((r) => r.url === "/admin/data/targets" && r.options?.method === "POST");
  assert.equal(JSON.parse(created.options.body).usage_adapter, "standard");
  for (const value of ["standard", "glm", "deepseek", "qwen", "kimi"]) {
    await app.modules.targets.openTargetModal({ name: "edit-target", endpoint: "https://fixture.example",
      endpoint_type: "provider-new", usage_adapter: value });
    assert.equal(harness.ids.get("tgtUsageAdapter").value, value, "editing preserves adapter");
    harness.requests.length = 0;
    await harness.ids.get("saveTargetBtn").listeners.click[0]();
    const updated = harness.requests.find((r) => r.options?.method === "PUT");
    assert.equal(JSON.parse(updated.options.body).usage_adapter, value);
  }
  app.modules.targets.updateTargetFormVisibility();
  for (const id of [
    "tgtBearerGroup",
    "tgtAuthModeGroup",
    "tgtImageOperationGroup",
  ])
    assert.equal(
      harness.ids.get(id).style.display,
      "",
      "unknown provider controls use flags",
    );
  assert.equal(
    harness.ids.get("tgtEndpoint").placeholder,
    "https://future.example",
  );
  assert.equal(harness.ids.get("tgtEndpointHint").textContent, "未来上游提示");
  harness.ids.get("tgtType").value = "provider-other";
  app.modules.targets.updateTargetFormVisibility();
  for (const id of [
    "tgtBearerGroup",
    "tgtAuthModeGroup",
    "tgtImageOperationGroup",
  ])
    assert.equal(harness.ids.get(id).style.display, "none");
  app.modules.schema.setSchemaSelectValue(
    "tgtUsageAdapter",
    "stored-future-value",
  );
  assert.equal(
    harness.ids.get("tgtUsageAdapter").value,
    "stored-future-value",
    "unknown saved enum remains preserved",
  );
  // Responses WebSocket preference follows the endpoint metadata flag and is
  // submitted only for types that support it.
  harness.ids.get("tgtType").value = "provider-new";
  app.modules.targets.updateTargetFormVisibility();
  assert.equal(harness.ids.get("tgtPreferWSGroup").style.display, "");
  harness.ids.get("tgtType").value = "provider-other";
  app.modules.targets.updateTargetFormVisibility();
  assert.equal(harness.ids.get("tgtPreferWSGroup").style.display, "none");
  const targetsSource = scripts.find((script) => script.name === "targets.js").source;
  assert.match(
    targetsSource,
    /getElementById\("tgtPreferWS"\)\.checked = t \? !!t\.prefer_websocket : false/,
    "editing a target shows its stored preference",
  );
  app.state.editingTarget = { name: "ws-target" };
  harness.ids.get("tgtName").value = "ws-target";
  harness.ids.get("tgtEndpoint").value = "https://ws.example";
  harness.ids.get("tgtPreferWS").checked = true;
  harness.ids.get("tgtUsageAdapter").value = "standard";
  for (const [type, want] of [["provider-new", true], ["provider-other", false]]) {
    harness.ids.get("tgtType").value = type;
    harness.requests.length = 0;
    await harness.ids.get("saveTargetBtn").listeners.click[0]();
    const write = harness.requests.find(
      (request) =>
        request.options?.method === "PUT" &&
        request.url === "/admin/data/targets/ws-target",
    );
    assert.ok(write, "target update was sent");
    assert.equal(JSON.parse(write.options.body).prefer_websocket, want, `prefer_websocket for ${type}`);
  }
  app.state.targets = [];
  app.state.modelGroups = [
    {
      name: "danger",
      cost_coefficient: 1,
      bindings: [
        { target: "<img src=x onerror=bad()>", model: "Model<MixedCase>" },
      ],
    },
  ];
  await app.modules.clients.openClientModal(null);
  assert.equal(
    harness.ids.get("clientModal").classList.contains("open"),
    true,
    "client editor cross-module dependencies work",
  );
  app.modules.core.closeModal("clientModal");
  const groupHTML = app.modules.quota.modelGroupModels(
    app.state.modelGroups[0],
  );
  assert.match(groupHTML, /&lt;img/);
  assert.match(groupHTML, /class='quota-target-deleted'/);
  assert.doesNotMatch(groupHTML, /<img/);
  app.modules.quota.renderModelGroups();
  const rendered = harness.ids.get("modelGroupsBody").innerHTML;
  assert.match(rendered, /<span class='quota-target-deleted'>/);
  assert.doesNotMatch(rendered, /&lt;span|&amp;lt;img/);
  assert.match(
    app.modules.quota.quotaConvertedDetail({
      groups: { danger: { quotas: {} } },
    }),
    /Schema日/,
  );
  app.state.schema.target_phases[0].label = "<img src=x onerror=bad()>";
  app.state.schema.pricing_modes[0].label = "<img src=x onerror=bad()>";
  app.state.targets = [
    {
      name: "label-target",
      endpoint_type: "provider-new",
      pool: "label-pool",
      model_mappings: [],
    },
  ];
  app.state.runtime = { "label-target": { phase: "ready", in_flight: 0 } };
  app.modules.targets.renderTargets();
  assert.doesNotMatch(harness.ids.get("targetsTableBody").innerHTML, /<img/);
  assert.match(harness.ids.get("targetsTableBody").innerHTML, /&lt;img/);
  assert.doesNotMatch(app.modules.pools.poolMemberChips("label-pool"), /<img/);
  assert.doesNotMatch(
    app.modules.pricing.renderPricingDetail([
      {
        id: "label-pricing",
        target: "label-target",
        model: "Model",
        mode: "flat",
        start: "",
        base_rates: {},
      },
    ]),
    /<img/,
  );
  const extendedSchema = schemaFixture();
  extendedSchema.quota_dimensions.push({
    value: "yearly",
    label: "年度",
    base_field: "yearly_usd",
    job_coefficient_field: "coefficient_yearly",
  });
  const extended = createHarness({ schema: extendedSchema });
  const extendedApp = await extended.run();
  assert.ok(extended.ids.get("jobLevel-yearly"));
  assert.ok(extended.ids.get("modelGroup-yearly"));
  extendedApp.state.modelGroups = [{ name: "extended", yearly_usd: 100 }];
  extendedApp.modules.quota.openJobLevelModal({
    name: "future",
    coefficient_yearly: 7,
  });
  assert.equal(
    extended.ids.get("jobLevel-yearly").value,
    7,
    "new dimension reads its configured coefficient field",
  );
  extended.ids.get("jobLevel-yearly").value = "8";
  await extended.ids.get("saveJobLevelBtn").listeners.click[0]();
  const jobWrite = extended.requests.find(
    (request) =>
      request.options?.method === "PUT" &&
      request.url.startsWith("/admin/data/job-levels/"),
  );
  assert.equal(
    JSON.parse(jobWrite.options.body).coefficient_yearly,
    8,
    "new dimension writes its declared coefficient field",
  );
  extendedApp.state.modelGroups = [{ name: "extended", yearly_usd: 100 }];
  assert.match(
    extendedApp.modules.quota.quotaEffectivePreview(
      { name: "extended" },
      { coefficient_yearly: 7 },
    ),
    /年度 \$700/,
  );
  assert.match(
    extended.ids.get("adminSchemaSummary").innerHTML,
    /coefficient_yearly/,
  );
  await extendedApp.modules.quota.openModelGroupModal(null);
  extended.ids.get("modelGroupName").value = "saved-extended";
  extended.ids.get("modelGroup-yearly").value = "321";
  await extended.ids.get("saveModelGroupBtn").listeners.click[0]();
  const groupWrite = extended.requests.find(
    (request) =>
      request.options?.method === "POST" &&
      request.url === "/admin/data/model-groups",
  );
  assert.equal(
    JSON.parse(groupWrite.options.body).yearly_usd,
    321,
    "new dimension writes its declared base field",
  );

  const failed = createHarness({ failure: "schema offline" });
  const failedApp = await failed.run();
  assert.equal(failedApp.state.ready, false);
  assert.deepEqual(Object.keys(failedApp.modules), ["core", "schema"]);
  assert.equal(failed.ids.get("adminBootRetry").hidden, false);
  assert.match(
    failed.ids.get("adminBootMessage").textContent,
    /schema offline/,
  );
  assert.equal(
    failed.ids.get("newTargetBtn").listeners.click,
    undefined,
    "schema failure binds no editing events",
  );
  failedApp.modules.core.openModal("targetModal");
  assert.equal(failed.ids.get("targetModal").classList.contains("open"), false);
  failed.setFailure("");
  await failedApp.boot();
  assert.equal(failedApp.state.ready, true, "retry initializes features once");
  assert.equal(failed.ids.get("newTargetBtn").listeners.click.length, 1);
  const readonly = createHarness({ role: "readonly" });
  const readonlyApp = await readonly.run();
  assert.equal(readonlyApp.modules.users.isReadonlyMode(), true);
  assert.equal(
    readonly.ids.get("nav-users").style.display,
    undefined,
    "admin menu remains hidden",
  );
  await readonlyApp.modules.targets.openTargetModal(null);
  assert.equal(
    readonly.ids.get("targetModal").classList.contains("open"),
    false,
  );
  const invalid = schemaFixture();
  delete invalid.usage_adapters;
  const malformed = createHarness({ schema: invalid });
  const invalidApp = await malformed.run();
  assert.equal(invalidApp.state.ready, false);
  assert.match(
    malformed.ids.get("adminBootMessage").textContent,
    /usage_adapters/,
  );
  for (const key of ["endpoint_type", "image_operation", "auth_mode", "pricing_mode", "role"]) {
    const invalidDefault = schemaFixture();
    invalidDefault.defaults[key] = "not-in-enum";
    assert.throws(() => app.modules.schema.validateSchema(invalidDefault), /默认值无效/,
      `${key} still requires enum membership`);
    invalidDefault.defaults[key] = "";
    if (key !== "auth_mode")
      assert.throws(() => app.modules.schema.validateSchema(invalidDefault), /默认值无效/,
        `${key} does not gain the empty-default exception`);
  }
  const missingDefault = schemaFixture();
  missingDefault.defaults.usage_adapter = "auto";
  assert.throws(() => app.modules.schema.validateSchema(missingDefault), /默认值无效/,
    "nonempty adapter default still requires enum membership");
  delete missingDefault.defaults.usage_adapter;
  assert.throws(() => app.modules.schema.validateSchema(missingDefault), /默认值无效/);
  console.log(
    "PASS admin UI modules, schema controls, failure/retry, escaping and readonly behavior",
  );
}
if (require.main === module)
  main().catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
module.exports = { createHarness, schemaFixture };
