"use strict";
// Run the embedded scripts in their actual HTML order using only Node built-ins.
// The DOM fixture focuses on boot, schema-driven controls and cross-module behavior.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const root = path.resolve(__dirname, "../..");
const { assembleAdminHTML } = require("./assemble_admin_html.cjs");
const { manifest, pageSource } = require("./admin_ui_scripts.cjs");
const html = assembleAdminHTML();
const scripts = manifest(html).map(({ rel, source }) => ({ name: rel, source }));
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
    // Inline display (e.g. the role-gated nav entries) is part of the shell contract.
    const inlineDisplay = /(?:^|;)\s*display\s*:\s*([\w-]+)/.exec(attrs.style || "");
    if (inlineDisplay) this.style.display = inlineDisplay[1];
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
  removeAttribute(name) {
    delete this.attrs[name];
  }
  querySelectorAll(selector) {
    const dataSelector = selector.match(/^([a-z][\w-]*)?\[data-([\w-]+)\]$/i);
    return this.children.flatMap((child) => [child, ...child.querySelectorAll("*")])
      .filter((child) => selector === "*" ||
        (dataSelector && (!dataSelector[1] || child.tagName === dataSelector[1].toUpperCase()) &&
          Object.prototype.hasOwnProperty.call(child.dataset, dataSelector[2].replace(/-([a-z])/g, (_, c) => c.toUpperCase()))) ||
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
function withKeySnapshot(target, snapshot) {
  assert.ok(typeof snapshot === "string" && snapshot, "success fixtures need an explicit snapshot");
  return {
    ...target,
    has_api_key: true,
    api_key: "fixt...0001",
    api_keys: [],
    key_snapshot: snapshot,
    key_runtime_indices: [0],
    key_statuses: [{ index: 0, key_mask: "fixt...0001", status: "等待中", blocked: false, exhausted: false }],
  };
}
function simulateTargetUpdate(harness, current, successor) {
  const fetch = harness.context.fetch;
  harness.context.fetch = async (url, options) => {
    if (url !== "/admin/data/targets/" + encodeURIComponent(current.name) || options?.method !== "PUT")
      return fetch(url, options);
    assert.equal(options.headers["X-Target-Key-Snapshot"], current.key_snapshot);
    harness.context.fetch = fetch;
    const response = await fetch(url, options);
    if (response.ok) harness.setRoute("/admin/data/targets", { targets: [successor] });
    return response;
  };
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
    target_phases: [
      option("ready", "Schema正常"),
      option("muted", "冷却"),
      option("half-open", "恢复"),
      option("offline", "暂停"),
    ],
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
  metrics = { targets_runtime: [] },
  metricsFailure = false,
  routes = {},
  // Optional overrides for transport and clock tests (bootstrap.test.cjs):
  // fetch(url, options, defaultFetch) may delegate to the fixture transport.
  fetch: fetchOverride = null,
  timers = null,
  location = {},
  // Script names (e.g. "pages/users/users.js") that fail to load, to model a 404/SyntaxError.
  omitScripts = [],
  history = undefined,
} = {}) {
  const windowListeners = {};
  const harnessScripts = scripts.filter((script) => !omitScripts.includes(script.name));
  const elements = [],
    ids = new Map(),
    requests = [],
    routeFailures = new Set();
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
    setTimeout: timers ? timers.setTimeout : () => 1,
    clearTimeout: timers ? timers.clearTimeout : () => {},
    setInterval: () => 1,
    clearInterval() {},
    navigator: { clipboard: { writeText: async () => {} } },
    addEventListener(type, fn) {
      (windowListeners[type] = windowListeners[type] || []).push(fn);
    },
    history,
    location,
    requestAnimationFrame: (fn) => fn(),
    fetch: (url, options) => {
      requests.push({ url, options });
      return fetchOverride
        ? fetchOverride(url, options, fixtureFetch)
        : fixtureFetch(url, options);
    },
  };
  async function fixtureFetch(url, options) {
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
    if (url === "/admin/metrics") {
      if (metricsFailure)
        return {
          ok: false,
          status: 503,
          text: async () => JSON.stringify({ error: "metrics offline" }),
        };
      return {
        ok: true,
        status: 200,
        text: async () => JSON.stringify(metrics),
      };
    }
    if (routeFailures.has(url)) {
      return {
        ok: false,
        status: 503,
        text: async () => JSON.stringify({ error: "route failed" }),
      };
    }
    if (Object.prototype.hasOwnProperty.call(routes, url)) {
      return {
        ok: true,
        status: 200,
        text: async () => JSON.stringify(routes[url]),
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
  }
  sandbox.window = sandbox;
  const context = vm.createContext(sandbox);
  return {
    context,
    document,
    windowListeners,
    ids,
    requests,
    setFailure: (value) => {
      failure = value;
    },
    setMetrics: (value) => {
      metrics = value;
    },
    setMetricsFailure: (value) => {
      metricsFailure = value;
    },
    setRoute: (url, value) => {
      routes[url] = value;
    },
    setRouteFailure: (url, value) => {
      value ? routeFailures.add(url) : routeFailures.delete(url);
    },
    // start() runs the scripts without waiting for boot, for hung-transport tests.
    start: () => {
      for (const script of harnessScripts)
        vm.runInContext(script.source, context, { filename: script.name });
      return context.AdminApp;
    },
    run: async () => {
      for (const script of harnessScripts)
        vm.runInContext(script.source, context, { filename: script.name });
      if (context.AdminApp?.bootPromise) await context.AdminApp.bootPromise;
      return context.AdminApp;
    },
  };
}
async function main() {
  assert.ok(scripts.length >= 14, "all feature scripts are loaded explicitly");
  assert.equal(scripts[0].name, "core/core.js", "core.js loads first");
  assert.equal(scripts[scripts.length - 1].name, "core/bootstrap.js", "bootstrap.js loads last");
  const harness = createHarness({ routes: { "/admin/data/targets": { targets: [] } } });
  const app = await harness.run();
  assert.equal(app.state.ready, true, "schema success enables startup");
  assert.ok(harness.requests.some(item => item.url === "/admin/api/activity"), "homepage loads live activity");
  assert.ok(!harness.requests.some(item => item.url.startsWith("/admin/data/audit")), "homepage does not query audit logs");
  assert.ok(!html.includes("overviewAuditBody"), "homepage removes the obsolete audit table");
  assert.ok(!html.includes('id="poolsModal"'), "pools live in an inline page tab, not a modal");
  assert.ok(!html.includes('id="openPoolsModalBtn"'), "the obsolete pool modal trigger is removed");
  for (const id of [
    "targetsMainTab",
    "targetsPoolsTab",
    "targetsMainPanel",
    "targetsPoolsPanel",
    "targetStatusFilters",
    "targetDetailDrawer",
    "targetDetailTitle",
    "targetDetailSubtitle",
    "targetDetailTabs",
    "targetDetailOverview",
    "targetDetailKeys",
    "targetDetailModels",
    "targetDetailAdvanced",
    "targetDetailEditBtn",
  ]) {
    assert.ok(harness.ids.get(id), `${id} is rendered`);
  }
  assert.equal(
    harness.ids.get("targetsPoolsPanel").getAttribute("role"),
    "tabpanel",
    "pools pane is a real tab panel",
  );
  assert.deepEqual(
    [...html.matchAll(/data-target-detail-tab="([^"]+)"/g)].map((match) => match[1]),
    ["overview", "keys", "models", "advanced"],
    "target detail offers the four read-only tabs",
  );
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
    "targetsHealth",
    "targetsDetail",
    "targetsKeyList",
    "targetsMappings",
    "targetsModal",
    "targetsSave",
    "targetsList",
    "pools",
    "clients",
    "pricingShared",
    "pricingRates",
    "pricingPeriods",
    "pricingList",
    "pricingModal",
    "pricing",
    "usage",
    "audit_trace",
    "users",
    "catalog",
    "quotaModel",
    "quotaData",
    "quotaGroups",
    "quotaLevels",
    "quotaSummary",
    "quotaDrawer",
    "quotaSync",
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
    const target = { name: "edit-target", endpoint: "https://fixture.example",
      endpoint_type: "provider-new", usage_adapter: value };
    const snapshot = `adapter-${value}-snapshot`;
    const saved = withKeySnapshot(target, snapshot);
    await app.modules.targets.openTargetModal(saved);
    assert.equal(harness.ids.get("tgtUsageAdapter").value, value, "editing preserves adapter");
    harness.requests.length = 0;
    simulateTargetUpdate(harness, saved, withKeySnapshot(target, `${snapshot}-next`));
    await harness.ids.get("saveTargetBtn").listeners.click[0]();
    const updated = harness.requests.find((r) => r.options?.method === "PUT");
    assert.equal(JSON.parse(updated.options.body).usage_adapter, value);
    assert.equal(updated.options.headers["X-Target-Key-Snapshot"], snapshot);
    assert.equal(JSON.parse(updated.options.body).api_key, "__existing_key_index__:0");
    assert.equal(app.state.targets[0].key_snapshot, `${snapshot}-next`);
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
  const targetsSource = pageSource("targets");
  assert.match(
    targetsSource,
    /getElementById\("tgtPreferWS"\)\.checked = t \? !!t\.prefer_websocket : false/,
    "editing a target shows its stored preference",
  );
  for (const [type, want] of [["provider-new", true], ["provider-other", false]]) {
    const target = { name: "ws-target", endpoint: "https://ws.example",
      endpoint_type: "provider-new", usage_adapter: "standard" };
    const snapshot = `ws-${type}-snapshot`;
    const saved = withKeySnapshot(target, snapshot);
    await app.modules.targets.openTargetModal(saved);
    harness.ids.get("tgtPreferWS").checked = true;
    harness.ids.get("tgtType").value = type;
    harness.requests.length = 0;
    simulateTargetUpdate(harness, saved, withKeySnapshot({ ...target, endpoint_type: type }, `${snapshot}-next`));
    await harness.ids.get("saveTargetBtn").listeners.click[0]();
    const write = harness.requests.find(
      (request) =>
        request.options?.method === "PUT" &&
        request.url === "/admin/data/targets/ws-target",
    );
    assert.ok(write, "target update was sent");
    assert.equal(write.options.headers["X-Target-Key-Snapshot"], snapshot);
    assert.equal(JSON.parse(write.options.body).prefer_websocket, want, `prefer_websocket for ${type}`);
    assert.equal(app.state.targets[0].key_snapshot, `${snapshot}-next`);
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

  // Targets page: health summary, abnormal-first filtering and the read-only
  // detail drawer are driven by /admin/data/targets plus /admin/metrics only.
  app.state.schema.target_phases[0].label = "正常";
  const targetsModule = app.modules.targets;
  app.state.targets = [
    { name: "healthy-target", has_api_key: true, key_statuses: [] },
    {
      name: "attention-target",
      has_api_key: true,
      key_statuses: [
        { index: 0, key_mask: "sk-****bad", status: "已屏蔽", blocked: true },
      ],
    },
    { name: "phase-target", has_api_key: true, key_statuses: [] },
    { name: "paused-target", paused: true },
  ];
  app.state.runtime = {
    "healthy-target": { phase: "ready", in_flight: 1, max_concurrency: 2 },
    "attention-target": { phase: "ready", in_flight: 0 },
    "phase-target": { phase: "muted", in_flight: 0 },
    "paused-target": { phase: "offline", paused: true },
  };
  const healthCounts = targetsModule.targetHealthCounts();
  assert.equal(healthCounts.all, 4);
  assert.equal(healthCounts.attention, 2);
  assert.equal(healthCounts.healthy, 1);
  assert.equal(healthCounts.paused, 1);
  targetsModule.renderTargets();
  assert.deepEqual(
    [
      ...harness.ids
        .get("targetStatusFilters")
        .innerHTML.matchAll(/data-target-filter="([^"]+)"/g),
    ].map((match) => match[1]),
    ["all", "attention", "healthy", "paused"],
    "target health exposes exactly four filters",
  );
  const sortedBody = harness.ids.get("targetsTableBody").innerHTML;
  assert.ok(
    sortedBody.indexOf("attention-target") < sortedBody.indexOf("healthy-target"),
    "targets needing attention sort before healthy targets",
  );
  assert.ok(
    sortedBody.indexOf("healthy-target") < sortedBody.indexOf("paused-target"),
    "paused targets sort after healthy targets, at the end of the list",
  );
  const pausedTargetRow = sortedBody
    .split("</tr>")
    .find((row) => row.includes("paused-target"));
  assert.ok(pausedTargetRow, "saved paused target row is rendered");
  assert.match(pausedTargetRow, /aria-checked='false'/);
  assert.match(pausedTargetRow, /data-resume-tgt='paused-target'/);
  assert.doesNotMatch(pausedTargetRow, /data-pause-tgt='paused-target'/);
  assert.match(pausedTargetRow, /目标已暂停/);
  assert.match(
    pausedTargetRow,
    /aria-label='[^']*自身暂停状态[^']*已暂停/,
    "target switch labels its own saved pause state",
  );
  assert.match(
    harness.ids.get("targetStatusFilters").innerHTML,
    /data-target-filter="attention"[^>]*aria-pressed="false"[\s\S]*?target-status-count[^>]*>2</,
    "status filter exposes the abnormal count",
  );
  targetsModule.setTargetFilter("attention");
  assert.match(
    harness.ids.get("targetsTableBody").innerHTML,
    /attention-target/,
  );
  assert.match(
    harness.ids.get("targetsTableBody").innerHTML,
    /phase-target/,
  );
  assert.doesNotMatch(
    harness.ids.get("targetsTableBody").innerHTML,
    /healthy-target|paused-target/,
  );
  assert.match(
    harness.ids.get("targetStatusFilters").innerHTML,
    /data-target-filter="attention"[^>]*aria-pressed="true"/,
    "active filter is exposed to assistive technology",
  );
  targetsModule.setTargetFilter("all");
  assert.match(
    harness.ids.get("targetsTableBody").innerHTML,
    /healthy-target/,
  );
  assert.match(
    harness.ids.get("targetStatusFilters").innerHTML,
    /data-target-filter="all"[^>]*aria-pressed="true"/,
  );

  const detailTarget = {
    name: "detail-target",
    endpoint_type: "provider-new",
    endpoint: "https://upstream.example/v1",
    usage_adapter: "standard",
    key_reset_time: "23",
    has_api_key: true,
    api_key: "sk-secret-plaintext",
    api_keys: ["sk-second-plaintext"],
    key_statuses: [
      { index: 0, key_mask: "sk-****mask", status: "已屏蔽", blocked: true },
    ],
    model_mappings: [
      { upstream: "Upstream-A", fallback: "alias-a" },
      { upstream: "Upstream-B", paused: true },
    ],
    custom_headers: { "X-Safe-Header": "secret-header-value" },
    custom_body: { temperature: 0.2 },
  };
  app.state.targets = [detailTarget];
  app.state.runtime = {
    "detail-target": {
      phase: "ready",
      in_flight: 1,
      max_concurrency: 4,
      utilization: 0.25,
      affinity_hits: 2,
      breaker_trips: 0,
      window_15m: { in_flight_peak: 3, affinity_hits: 2, breaker_trips: 0 },
    },
  };
  targetsModule.renderTargets();
  await targetsModule.openTargetDetail("detail-target");
  assert.equal(
    harness.ids.get("targetDetailDrawer").classList.contains("open"),
    true,
    "target detail opens through openModal",
  );
  assert.match(
    harness.ids.get("targetDetailKeys").innerHTML,
    /sk-\*\*\*\*mask/,
    "key status shows the masked key only",
  );
  assert.doesNotMatch(
    harness.ids.get("targetDetailKeys").innerHTML,
    /sk-secret-plaintext|sk-second-plaintext/,
    "detail never renders a plaintext key",
  );
  assert.match(
    harness.ids.get("targetDetailModels").innerHTML,
    /target-model-badge/,
  );
  assert.match(
    harness.ids.get("targetDetailAdvanced").innerHTML,
    /X-Safe-Header/,
  );
  assert.match(
    harness.ids.get("targetDetailAdvanced").innerHTML,
    /用量适配/,
    "usage_adapter is described as usage adaptation, not billing",
  );
  assert.doesNotMatch(
    harness.ids.get("targetDetailAdvanced").innerHTML,
    /计费类型/,
    "usage_adapter is not labelled as a billing type",
  );
  assert.doesNotMatch(
    harness.ids.get("targetDetailAdvanced").innerHTML,
    /secret-header-value/,
    "advanced settings list custom header names without their values",
  );
  assert.match(
    harness.ids.get("targetDetailEditBtn").getAttribute("class"),
    /readonly-hide/,
  );
  targetsModule.switchTargetDetailTab("keys");
  assert.equal(harness.ids.get("targetDetailKeys").hidden, false);
  assert.equal(harness.ids.get("targetDetailOverview").hidden, true);
  assert.equal(
    harness.ids.get("targetDetailKeys").getAttribute("role"),
    "tabpanel",
  );
  assert.equal(
    harness.ids.get("targetDetailKeysTab").getAttribute("aria-selected"),
    "true",
  );
  assert.equal(
    harness.ids.get("targetDetailOverviewTab").getAttribute("aria-selected"),
    "false",
  );
  targetsModule.closeTargetDetail();
  assert.equal(
    harness.ids.get("targetDetailDrawer").classList.contains("open"),
    false,
  );

  app.state.targets = [
    {
      name: "masked-only",
      has_api_key: true,
      api_key: "sk-secret-only",
      key_statuses: [],
    },
  ];
  targetsModule.renderTargets();
  targetsModule.openTargetDetail("masked-only");
  assert.doesNotMatch(
    harness.ids.get("targetDetailKeys").innerHTML,
    /sk-secret-only/,
    "missing key runtime status never falls back to the stored key",
  );
  targetsModule.closeTargetDetail();

  await targetsModule.switchTargetPageTab("pools");
  assert.equal(harness.ids.get("targetsPoolsPanel").hidden, false);
  assert.equal(harness.ids.get("targetsMainPanel").hidden, true);
  assert.equal(
    harness.ids.get("targetsPoolsTab").getAttribute("aria-selected"),
    "true",
  );
  await targetsModule.switchTargetPageTab("targets");
  assert.equal(harness.ids.get("targetsMainPanel").hidden, false);
  assert.equal(harness.ids.get("targetsPoolsPanel").hidden, true);
  assert.equal(
    harness.ids.get("targetsMainTab").getAttribute("aria-selected"),
    "true",
  );

  // A paused deployment pool pauses members at runtime only. The saved target
  // keeps paused=false, health and filters explain the pool, and returning from
  // the Pools tab reloads targets plus scheduling runtime.
  const poolHarness = createHarness({
    metrics: {
      targets_runtime: [
        { name: "pool-paused", phase: "ready", paused: false, in_flight: 0 },
        { name: "pool-active", phase: "ready", in_flight: 0 },
      ],
    },
    routes: {
      "/admin/data/targets": {
        targets: [
          { name: "pool-paused", pool: "pool-a", paused: false, key_statuses: [] },
          { name: "pool-active", pool: "pool-b", paused: false, key_statuses: [] },
        ],
      },
      "/admin/data/pools": {
        pools: [
          { name: "pool-a", paused: false },
          { name: "pool-b", paused: false },
        ],
      },
    },
  });
  const poolApp = await poolHarness.run();
  await poolApp.modules.targets.loadTargets();
  assert.equal(
    poolApp.modules.targets.targetHealth(poolApp.state.targets[0]).key,
    "healthy",
  );
  poolHarness.setMetrics({
    targets_runtime: [
      { name: "pool-paused", phase: "offline", paused: true, in_flight: 0 },
      { name: "pool-active", phase: "ready", in_flight: 0 },
    ],
  });
  poolHarness.setRoute("/admin/data/pools", {
    pools: [
      { name: "pool-a", paused: true },
      { name: "pool-b", paused: false },
    ],
  });
  await poolApp.modules.targets.switchTargetPageTab("pools");
  const beforeReturn = poolHarness.requests.length;
  await poolApp.modules.targets.switchTargetPageTab("targets");
  const returnRequests = poolHarness.requests.slice(beforeReturn);
  assert.ok(
    returnRequests.some((request) => request.url === "/admin/data/targets"),
    "returning from Pools reloads targets",
  );
  assert.ok(
    returnRequests.some((request) => request.url === "/admin/metrics"),
    "returning from Pools reloads scheduling runtime",
  );
  const pausedHealth = poolApp.modules.targets.targetHealth(
    poolApp.state.targets[0],
  );
  assert.equal(pausedHealth.key, "paused");
  assert.match(pausedHealth.reason, /部署池/);
  assert.match(pausedHealth.reason, /暂停/);
  assert.equal(
    poolApp.state.targets[0].paused,
    false,
    "pool pause never rewrites the saved target setting",
  );
  const poolCounts = poolApp.modules.targets.targetHealthCounts();
  assert.equal(poolCounts.all, 2);
  assert.equal(poolCounts.attention, 0);
  assert.equal(poolCounts.healthy, 1);
  assert.equal(poolCounts.paused, 1);
  poolApp.modules.targets.setTargetFilter("paused");
  const poolPausedBody = poolHarness.ids.get("targetsTableBody").innerHTML;
  assert.match(poolPausedBody, /pool-paused/);
  assert.doesNotMatch(poolPausedBody, /pool-active/);
  const poolPausedRow = poolPausedBody
    .split("</tr>")
    .find((row) => row.includes("pool-paused"));
  assert.match(
    poolPausedRow,
    /data-pause-tgt='pool-paused'/,
    "a pool-paused member keeps its own enabled target switch",
  );
  assert.doesNotMatch(poolPausedRow, /data-resume-tgt='pool-paused'/);
  assert.match(poolPausedRow, /aria-checked='true'/);
  assert.match(poolPausedRow, /目标启用/);
  assert.match(
    poolPausedRow,
    /aria-label='[^']*自身暂停状态[^']*未暂停/,
    "pool pause does not change the target switch label",
  );
  assert.match(
    poolPausedRow,
    /target-health-badge[^>]*title='[^']*部署池[^']*暂停[^']*'/,
    "effective pause keeps its pool reason in the health badge",
  );
  poolApp.state.targets[0].paused = true;
  poolApp.modules.targets.renderTargets();
  const bothPausedRow = poolHarness.ids
    .get("targetsTableBody")
    .innerHTML.split("</tr>")
    .find((row) => row.includes("pool-paused"));
  assert.match(
    bothPausedRow,
    /data-resume-tgt='pool-paused'/,
    "a saved target pause still offers target resume while the pool is paused",
  );
  assert.match(bothPausedRow, /aria-checked='false'/);
  poolApp.state.targets[0].paused = false;
  poolApp.modules.targets.renderTargets();
  poolApp.modules.targets.setTargetFilter("healthy");
  assert.match(poolHarness.ids.get("targetsTableBody").innerHTML, /pool-active/);
  assert.doesNotMatch(
    poolHarness.ids.get("targetsTableBody").innerHTML,
    /pool-paused/,
  );
  poolApp.modules.targets.setTargetFilter("all");
  poolHarness.setMetrics({
    targets_runtime: [
      { name: "pool-paused", phase: "ready", paused: false, in_flight: 0 },
      { name: "pool-active", phase: "ready", in_flight: 0 },
    ],
  });
  poolHarness.setRoute("/admin/data/pools", {
    pools: [
      { name: "pool-a", paused: false },
      { name: "pool-b", paused: false },
    ],
  });
  await poolApp.modules.targets.switchTargetPageTab("pools");
  await poolApp.modules.targets.switchTargetPageTab("targets");
  const resumedCounts = poolApp.modules.targets.targetHealthCounts();
  assert.equal(resumedCounts.attention, 0);
  assert.equal(resumedCounts.healthy, 2);
  assert.equal(resumedCounts.paused, 0);

  // A fresh runtime snapshot outranks a cached pool roster. Target refresh must
  // reload pools every time, and a failed pool refresh must keep the last known
  // roster instead of overriding authoritative runtime state.
  const runtimePriorityHarness = createHarness({
    metrics: {
      targets_runtime: [{ name: "runtime-priority", phase: "ready", in_flight: 0 }],
    },
    routes: {
      "/admin/data/targets": {
        targets: [
          { name: "runtime-priority", pool: "pool-a", paused: false, key_statuses: [] },
        ],
      },
      "/admin/data/pools": { pools: [{ name: "pool-a", paused: true }] },
    },
  });
  const runtimePriorityApp = await runtimePriorityHarness.run();
  await runtimePriorityApp.modules.targets.loadTargets();
  assert.equal(runtimePriorityApp.state.pools[0].paused, true);
  assert.equal(
    runtimePriorityApp.modules.targets.targetHealth(
      runtimePriorityApp.state.targets[0],
    ).key,
    "healthy",
    "fresh ready runtime outranks a cached paused pool",
  );
  runtimePriorityHarness.setRoute("/admin/data/pools", {
    pools: [{ name: "pool-a", paused: false }],
  });
  await runtimePriorityApp.modules.targets.loadTargets();
  assert.equal(
    runtimePriorityApp.state.pools[0].paused,
    false,
    "target refresh always reloads the pool roster",
  );
  runtimePriorityHarness.setRoute("/admin/data/pools", {
    pools: [{ name: "pool-a", paused: true }],
  });
  await runtimePriorityApp.modules.targets.loadTargets();
  runtimePriorityHarness.setRouteFailure("/admin/data/pools", true);
  runtimePriorityHarness.setMetrics({
    targets_runtime: [{ name: "runtime-priority", phase: "ready", in_flight: 0 }],
  });
  await runtimePriorityApp.modules.targets.loadTargets();
  assert.equal(
    runtimePriorityApp.state.pools[0].paused,
    true,
    "failed pool refresh keeps the last known roster",
  );
  assert.equal(
    runtimePriorityApp.modules.targets.targetHealth(
      runtimePriorityApp.state.targets[0],
    ).key,
    "healthy",
    "failed pool fetch cannot override fresh ready runtime",
  );
  runtimePriorityHarness.setRouteFailure("/admin/data/pools", false);
  runtimePriorityHarness.setRoute("/admin/data/pools", {
    pools: [{ name: "pool-a", paused: false }],
  });
  await runtimePriorityApp.modules.targets.loadTargets();
  runtimePriorityHarness.setMetrics({
    targets_runtime: [
      { name: "runtime-priority", phase: "offline", paused: true, in_flight: 0 },
    ],
  });
  await runtimePriorityApp.modules.targets.loadTargets();
  assert.equal(
    runtimePriorityApp.modules.targets.targetHealth(
      runtimePriorityApp.state.targets[0],
    ).key,
    "paused",
    "fresh offline runtime pauses a target while the cached pool is active",
  );
  runtimePriorityHarness.setRoute("/admin/data/targets", {
    targets: [
      { name: "runtime-priority", pool: "pool-a", paused: true, key_statuses: [] },
    ],
  });
  runtimePriorityHarness.setRouteFailure("/admin/data/pools", true);
  await runtimePriorityApp.modules.targets.loadTargets();
  assert.equal(
    runtimePriorityApp.state.targets[0].paused,
    true,
    "a failed pool refresh never erases the stored target pause",
  );
  const ownPauseHealth = runtimePriorityApp.modules.targets.targetHealth(
    runtimePriorityApp.state.targets[0],
  );
  assert.equal(ownPauseHealth.key, "paused");
  assert.match(ownPauseHealth.reason, /目标已暂停/);

  // Runtime fetch failures clear stale metrics. Missing, unknown or malformed
  // phases stay attention with explicit unavailable text, never healthy.
  const runtimeHarness = createHarness({
    metrics: {
      targets_runtime: [{ name: "runtime-stale", phase: "ready", in_flight: 1 }],
    },
  });
  const runtimeApp = await runtimeHarness.run();
  await runtimeApp.modules.targets.fetchSchedulingRuntime();
  assert.equal(runtimeApp.state.runtime["runtime-stale"].phase, "ready");
  runtimeHarness.setMetricsFailure(true);
  await runtimeApp.modules.targets.fetchSchedulingRuntime();
  assert.equal(
    runtimeApp.state.runtime,
    null,
    "failed metrics clears stale scheduling runtime",
  );
  runtimeHarness.setRoute("/admin/data/targets", {
    targets: [{ name: "runtime-stale", has_api_key: true, key_statuses: [] }],
  });
  await runtimeApp.modules.targets.loadTargets();
  const unavailableHealth = runtimeApp.modules.targets.targetHealth(
    runtimeApp.state.targets[0],
  );
  assert.equal(unavailableHealth.key, "attention");
  assert.match(unavailableHealth.reason, /运行态不可用/);
  runtimeApp.modules.targets.renderTargets();
  const unavailableBody = runtimeHarness.ids.get("targetsTableBody").innerHTML;
  assert.match(unavailableBody, /运行态不可用/);
  assert.match(unavailableBody, /data-target-health='attention'/);
  assert.doesNotMatch(unavailableBody, /data-target-health='healthy'/);
  runtimeHarness.setMetricsFailure(false);
  runtimeHarness.setMetrics({
    targets_runtime: [{ name: "runtime-stale", phase: "ready", in_flight: 1 }],
  });
  await runtimeApp.modules.targets.fetchSchedulingRuntime();
  assert.equal(
    runtimeApp.modules.targets.targetHealth(runtimeApp.state.targets[0]).key,
    "healthy",
    "metrics recovery restores a healthy classification",
  );
  runtimeHarness.setMetrics({
    targets_runtime: [{ name: "runtime-stale", phase: "unexpected", in_flight: 0 }],
  });
  await runtimeApp.modules.targets.fetchSchedulingRuntime();
  const unknownPhase = runtimeApp.modules.targets.targetHealth(
    runtimeApp.state.targets[0],
  );
  assert.equal(unknownPhase.key, "attention");
  assert.match(unknownPhase.reason, /运行态不可用/);
  runtimeApp.modules.targets.renderTargets();
  runtimeApp.modules.targets.openTargetDetail("runtime-stale");
  assert.match(
    runtimeHarness.ids.get("targetDetailOverview").innerHTML,
    /运行态不可用/,
  );
  runtimeApp.modules.targets.closeTargetDetail();
  runtimeHarness.setMetrics({
    targets_runtime: [{ name: "runtime-stale", in_flight: 0 }],
  });
  await runtimeApp.modules.targets.fetchSchedulingRuntime();
  const missingPhase = runtimeApp.modules.targets.targetHealth(
    runtimeApp.state.targets[0],
  );
  assert.equal(missingPhase.key, "attention");
  assert.match(missingPhase.reason, /运行态不可用/);

  // keypool.status keeps exhausted=true after an expired cooldown. A
  // recoverable key reports 可用(待激活) and is not an issue; only the
  // non-recoverable 失效(待请求验证) state raises attention.
  const keyHarness = createHarness({
    metrics: {
      targets_runtime: [
        { name: "cooldown-recoverable", phase: "ready", in_flight: 0 },
        { name: "cooldown-invalid", phase: "ready", in_flight: 0 },
      ],
    },
  });
  const keyApp = await keyHarness.run();
  keyApp.state.targets = [
    {
      name: "cooldown-recoverable",
      has_api_key: true,
      key_statuses: [
        {
          index: 0,
          key_mask: "sk-***good",
          status: "可用(待激活)",
          exhausted: true,
          exhaust_reason: "rate_limited",
          cooldown_end: "2020-01-01T00:00:00Z",
        },
      ],
    },
    {
      name: "cooldown-invalid",
      has_api_key: true,
      key_statuses: [
        {
          index: 0,
          key_mask: "sk-***bad",
          status: "失效(待请求验证)",
          exhausted: true,
          exhaust_reason: "invalid_token",
          cooldown_end: "2020-01-01T00:00:00Z",
        },
      ],
    },
  ];
  keyApp.state.runtime = {
    "cooldown-recoverable": { phase: "ready", in_flight: 0 },
    "cooldown-invalid": { phase: "ready", in_flight: 0 },
  };
  assert.equal(
    keyApp.modules.targets.targetHealth(keyApp.state.targets[0]).key,
    "healthy",
    "a cooldown-expired recoverable key is not an issue",
  );
  assert.equal(
    keyApp.modules.targets.targetHealth(keyApp.state.targets[1]).key,
    "attention",
  );
  const keyCounts = keyApp.modules.targets.targetHealthCounts();
  assert.equal(keyCounts.all, 2);
  assert.equal(keyCounts.attention, 1);
  assert.equal(keyCounts.healthy, 1);
  assert.equal(keyCounts.paused, 0);
  keyApp.modules.targets.renderTargets();
  const keyBody = keyHarness.ids.get("targetsTableBody").innerHTML;
  const recoverableRow = keyBody
    .split("</tr>")
    .find((row) => row.includes("cooldown-recoverable"));
  const invalidRow = keyBody
    .split("</tr>")
    .find((row) => row.includes("cooldown-invalid"));
  assert.ok(recoverableRow);
  assert.ok(invalidRow);
  assert.doesNotMatch(
    recoverableRow,
    /限流|不可用|唤醒/,
    "recoverable cooldown shows no key issue or wakeup action",
  );
  assert.match(invalidRow, /唤醒/);
  keyApp.modules.targets.openTargetDetail("cooldown-recoverable");
  const recoverableOverview =
    keyHarness.ids.get("targetDetailOverview").innerHTML;
  const recoverableKeys = keyHarness.ids.get("targetDetailKeys").innerHTML;
  assert.match(recoverableOverview, /可用\(待激活\)/);
  assert.doesNotMatch(recoverableOverview, /不可用/);
  assert.match(recoverableKeys, /可用\(待激活\)/);
  assert.doesNotMatch(recoverableKeys, /不可用/);
  keyApp.modules.targets.openTargetDetail("cooldown-invalid");
  assert.match(
    keyHarness.ids.get("targetDetailOverview").innerHTML,
    /不可用 1/,
  );
  assert.match(
    keyHarness.ids.get("targetDetailKeys").innerHTML,
    /失效\(待请求验证\)/,
  );
  keyApp.modules.targets.closeTargetDetail();

  // Go's zero time must read as "no cooldown", not a real 0001 deadline.
  keyApp.state.targets.push({
    name: "cooldown-zero",
    has_api_key: true,
    key_statuses: [
      {
        index: 0,
        key_mask: "sk-***normal",
        status: "可用",
        cooldown_end: "0001-01-01T00:00:00Z",
      },
      {
        index: 1,
        key_mask: "sk-***no-deadline",
        status: "失效(待请求验证)",
        blocked: false,
        exhausted: true,
        cooldown_end: "0001-01-01T00:00:00Z",
      },
      {
        index: 2,
        key_mask: "sk-***deadline",
        status: "限流",
        blocked: false,
        exhausted: true,
        cooldown_end: "2030-01-01T00:00:00Z",
      },
    ],
  });
  keyApp.state.runtime["cooldown-zero"] = { phase: "ready", in_flight: 0 };
  keyApp.modules.targets.renderTargets();
  keyApp.modules.targets.openTargetDetail("cooldown-zero");
  const zeroCooldownKeys = keyHarness.ids.get("targetDetailKeys").innerHTML;
  assert.doesNotMatch(
    zeroCooldownKeys,
    /冷却结束 0001-01-01T00:00:00Z/,
    "Go zero time is not a real cooldown deadline",
  );
  assert.match(
    zeroCooldownKeys,
    /冷却结束 2030-01-01T00:00:00Z/,
    "a real cooldown deadline is preserved",
  );
  keyApp.modules.targets.closeTargetDetail();

  // Zero keys explain passthrough; single or multiple keys only expose masks,
  // and no per-key health is invented without runtime status.
  app.state.targets = [
    {
      name: "passthrough-only",
      allow_bearer_passthrough: true,
      has_api_key: false,
      key_statuses: [],
    },
  ];
  app.state.runtime = {
    "passthrough-only": { phase: "ready", in_flight: 0 },
  };
  targetsModule.renderTargets();
  targetsModule.openTargetDetail("passthrough-only");
  assert.match(
    harness.ids.get("targetDetailOverview").innerHTML,
    /未配置独立密钥/,
  );
  assert.match(
    harness.ids.get("targetDetailOverview").innerHTML,
    /Bearer 透传/,
  );
  assert.doesNotMatch(
    harness.ids.get("targetDetailKeys").innerHTML,
    /target-detail-key/,
    "zero keys never invent per-key health",
  );
  targetsModule.closeTargetDetail();
  app.state.targets = [
    {
      name: "single-mask",
      has_api_key: true,
      api_key: "sk-secret-single",
      key_statuses: [],
    },
  ];
  app.state.runtime = { "single-mask": { phase: "ready", in_flight: 0 } };
  targetsModule.renderTargets();
  targetsModule.openTargetDetail("single-mask");
  assert.doesNotMatch(
    harness.ids.get("targetDetailKeys").innerHTML,
    /sk-secret-single/,
  );
  assert.match(
    harness.ids.get("targetDetailKeys").innerHTML,
    /运行状态明细尚未上报/,
  );
  assert.doesNotMatch(
    harness.ids.get("targetDetailKeys").innerHTML,
    /target-detail-key/,
    "single key without runtime status has no fabricated per-key state",
  );
  targetsModule.closeTargetDetail();

  const extendedSchema = schemaFixture();
  extendedSchema.quota_dimensions.push({
    value: "yearly",
    label: "年度",
    base_field: "yearly_usd",
    job_coefficient_field: "coefficient_yearly",
  });
  const extended = createHarness({ schema: extendedSchema, routes: { "/admin/data/targets": { targets: [] } } });
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
  for (const dim of ["daily", "weekly", "monthly"]) extended.ids.get("modelGroup-" + dim).value = "0"; // bases must be explicit
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
  failedApp.modules.core.openModal("targetDetailDrawer");
  assert.equal(
    failed.ids.get("targetDetailDrawer").classList.contains("open"),
    false,
  );
  failed.setFailure("");
  await failedApp.boot();
  assert.equal(failedApp.state.ready, true, "retry initializes features once");
  assert.equal(failed.ids.get("newTargetBtn").listeners.click.length, 1);
  const readonly = createHarness({ role: "readonly" });
  const readonlyApp = await readonly.run();
  assert.equal(readonlyApp.modules.users.isReadonlyMode(), true);
  assert.equal(
    readonly.ids.get("nav-users").style.display,
    "none",
    "admin menu remains hidden",
  );
  await readonlyApp.modules.targets.openTargetModal(null);
  assert.equal(
    readonly.ids.get("targetModal").classList.contains("open"),
    false,
  );
  readonlyApp.state.targets = [
    { name: "readonly-detail", key_statuses: [] },
  ];
  readonlyApp.modules.targets.renderTargets();
  assert.equal(
    readonlyApp.modules.targets.openTargetDetail("readonly-detail"),
    true,
    "readonly users can inspect the target detail",
  );
  assert.equal(
    readonly.ids.get("targetDetailDrawer").classList.contains("open"),
    true,
  );
  assert.match(
    readonly.ids.get("targetDetailEditBtn").getAttribute("class"),
    /readonly-hide/,
    "edit action stays hidden in readonly mode",
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
module.exports = { createHarness, schemaFixture, withKeySnapshot, simulateTargetUpdate };
