"use strict";
// Targets edit modal form: field population, capability visibility and pool options.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsModal", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var epMeta = app.bind("schema", "epMeta");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var setSchemaSelectValue = app.bind("schema", "setSchemaSelectValue");
  var poolMemberCount = app.bind("pools", "poolMemberCount");
  var poolDisplayName = app.bind("pools", "poolDisplayName");
  var addCustomBodyRow = app.bind("targetsMappings", "addCustomBodyRow");
  var addCustomHeaderRow = app.bind("targetsMappings", "addCustomHeaderRow");
  var addModelMappingRow = app.bind("targetsMappings", "addModelMappingRow");
  var keyListFromSnapshot = app.bind("targetsHealth", "keyListFromSnapshot");
  var renderKeyList = app.bind("targetsKeyList", "renderKeyList");
  var renderTgtModelSuggestions = app.bind("targetsMappings", "renderTgtModelSuggestions");
  var updateModelPrefixPreview = app.bind("targetsMappings", "updateModelPrefixPreview");
  var usableKeySnapshot = app.bind("targetsHealth", "usableKeySnapshot");
  function updateTargetFormVisibility() {
    var epType = document.getElementById("tgtType").value;
    var meta = epMeta(epType) || {};
    document.getElementById("tgtCapabilities").textContent =
      "协议：" +
      (meta.protocols || [])
        .map(function (value) {
          return schemaLabel("protocols", value);
        })
        .join(" / ") +
      "；请求格式：" +
      (meta.request_schemas || [])
        .map(function (value) {
          return schemaLabel("request_schemas", value);
        })
        .join(" / ");
    var supportsBearer = !!meta.supports_bearer_passthrough;
    var supportsAuthMode = !!meta.supports_auth_mode;
    var supportsImageOperation = !!meta.supports_image_operation;
    document.getElementById("tgtBearerGroup").style.display = supportsBearer
      ? ""
      : "none";
    document.getElementById("tgtAuthModeGroup").style.display = supportsAuthMode
      ? ""
      : "none";
    document.getElementById("tgtImageOperationGroup").style.display =
      supportsImageOperation ? "" : "none";
    document.getElementById("tgtPreferWSGroup").style.display =
      meta.supports_prefer_websocket ? "" : "none";
    // Visibility is capability-driven; switching type must not clear persisted values.
    document.getElementById("tgtDualProtocolGroup").style.display =
      meta.supports_dual_protocol ? "" : "none";
    document.getElementById("tgtStreamUsageGroup").style.display =
      (meta.request_schemas || []).includes(state.schema.stream_usage_request_schema) ? "" : "none";
    // API Keys 和冷却期对所有 provider 类型都显示（统一 keypool 亲和机制）
    applyEndpointPlaceholder(epType);
    renderTgtModelSuggestions(epType);
  }
  // applyEndpointPlaceholder 按 endpoint_type 给 Endpoint URL 输入框替换 placeholder 与提示文案。
  // 通用模板：每个 provider 给出官方文档推荐的 base_url 示例和必要的接入提示。
  // 不再硬编码 Azure 描述。
  function applyEndpointPlaceholder(epType) {
    var ep = document.getElementById("tgtEndpoint");
    var hint = document.getElementById("tgtEndpointHint");
    var meta = epMeta(epType) || {};
    if (ep) ep.placeholder = meta.endpoint_placeholder || "";
    if (hint) hint.textContent = meta.endpoint_hint || "";
  }
  async function openTargetModal(t) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    var session = ++ts.editingKeySession;
    // 保证模型目录缓存可用，供模型行的 datalist 软建议；失败不阻塞弹窗
    if (!state.catalogAllCache) {
      try {
        var catData = await api("/admin/data/catalog");
        state.catalogAllCache = Array.isArray(catData.models)
          ? catData.models
          : [];
      } catch (e) {
        /* 软建议数据缺失可容忍 */
      }
    }
    if (session !== ts.editingKeySession) return;
    ts.createdDraftNeedsReopen = false;
    document.getElementById("tgtNewKeyInput").disabled = false;
    document.getElementById("addKeyBtn").disabled = false;
    state.editingTarget = t || null;
    ts.editingDraftRevision = 0;
    ts.editingKeyBusy = false;
    ts.editingKeyReloading = false;
    ts.editingKeySaving = false;
    ts.editingKeySnapshot = usableKeySnapshot(t) ? t.key_snapshot : null;
    ts.editingKeyIndices = usableKeySnapshot(t) ? t.key_runtime_indices.slice() : null;
    ts.editingKeyNeedsReview = !!t && (!ts.editingKeySnapshot || ts.targetsLoadFailed);
    ts.editingKeyStatusStale = false;
    document.getElementById("targetModalTitle").textContent = t
      ? "编辑目标"
      : "新增目标";
    document.getElementById("tgtName").value = t ? t.name : "";
    document.getElementById("tgtName").readOnly = !!t;
    setSchemaSelectValue(
      "tgtType",
      t
        ? t.endpoint_type || state.schema.defaults.endpoint_type
        : state.schema.defaults.endpoint_type,
    );
    document.getElementById("tgtEndpoint").value = t ? t.endpoint : "";
    var epType = t
      ? t.endpoint_type || state.schema.defaults.endpoint_type
      : state.schema.defaults.endpoint_type;
    document.getElementById("tgtAllowBearer").checked = t
      ? !!t.allow_bearer_passthrough
      : false;
    document.getElementById("tgtPaused").checked = t ? !!t.paused : false;
    document.getElementById("tgtPreferWS").checked = t ? !!t.prefer_websocket : false;
    document.getElementById("tgtOpenAIPrefix").value =
      t && t.openai_prefix != null ? t.openai_prefix : state.schema.defaults.openai_prefix;
    document.getElementById("tgtAnthropicPrefix").value =
      t && t.anthropic_prefix != null ? t.anthropic_prefix : state.schema.defaults.anthropic_prefix;
    document.getElementById("tgtSupportsResponses").checked =
      t && t.supports_responses != null ? t.supports_responses : state.schema.defaults.supports_responses;
    document.getElementById("tgtRequestStreamUsage").checked =
      t && t.request_stream_usage_if_missing != null ? t.request_stream_usage_if_missing : state.schema.defaults.request_stream_usage_if_missing;
    setSchemaSelectValue(
      "tgtAuthMode",
      t
        ? t.auth_mode || state.schema.defaults.auth_mode
        : state.schema.defaults.auth_mode,
    );
    setSchemaSelectValue(
      "tgtUsageAdapter",
      t ? t.usage_adapter : state.schema.defaults.usage_adapter,
    );
    setUsageAdapterError("");
    setSchemaSelectValue(
      "tgtImageOperation",
      t
        ? t.image_operation || state.schema.defaults.image_operation
        : state.schema.defaults.image_operation,
    );
    document.getElementById("tgtModelPrefix").value = t ? t.model_prefix || "" : "";
    // 模型映射：清空容器后按 model_mappings 填充行，无数据时给一行空行
    var mappingsContainer = document.getElementById("model-mappings-container");
    mappingsContainer.innerHTML = "";
    if (t && Array.isArray(t.model_mappings) && t.model_mappings.length > 0) {
      t.model_mappings.forEach(function (m) {
        addModelMappingRow(m.upstream || "", m.fallback || "", !!t, !!m.paused);
      });
    } else {
      addModelMappingRow("", "", !!t);
    }
    updateModelPrefixPreview();
    // Custom Headers：填充 KV 行
    var headersContainer = document.getElementById("custom-headers-container");
    headersContainer.innerHTML = "";
    if (t && t.custom_headers) {
      Object.keys(t.custom_headers).forEach(function (k) {
        addCustomHeaderRow(k, t.custom_headers[k]);
      });
    }
    if (headersContainer.children.length === 0) addCustomHeaderRow("", "");

    // Custom Body：填充 KV 行
    var bodyContainer = document.getElementById("custom-body-container");
    bodyContainer.innerHTML = "";
    if (t && t.custom_body) {
      Object.keys(t.custom_body).forEach(function (k) {
        addCustomBodyRow(k, JSON.stringify(t.custom_body[k]));
      });
    }
    if (bodyContainer.children.length === 0) addCustomBodyRow("", "");
    // Config slot references must stay paired with the snapshot that issued them.
    ts.tgtKeyList = t ? keyListFromSnapshot(t) : [];
    renderKeyList();
    document.getElementById("tgtNewKeyInput").value = "";
    document.getElementById("tgtKeyResetTime").value =
      t && t.key_reset_time ? t.key_reset_time : "";
    renderTargetPoolOptions(t && t.pool ? t.pool : "");
    renderKeyList();
    updateTargetFormVisibility();
    openModal("targetModal");
  }
  // renderTargetPoolOptions 重建目标表单的池下拉（数据来自 /admin/data/pools）。
  function updateTargetPoolAvailability() {
    var sel = document.getElementById("tgtPool");
    var label = ts.targetPoolsSnapshot && ts.targetPoolOptionsSnapshot === ts.targetPoolsSnapshot
      ? "部署池" : "部署池数据不可用或选项可能过期，请重新打开编辑查看";
    sel.setAttribute("aria-label", label);
    sel.setAttribute("title", label);
  }
  function renderTargetPoolOptions(selected) {
    var sel = document.getElementById("tgtPool");
    ts.targetPoolOptionsSnapshot = ts.targetPoolsSnapshot;
    updateTargetPoolAvailability();
    var val = selected || "";
    sel.innerHTML = "<option value=''>不属于任何池</option>";
    (ts.targetPoolsSnapshot || state.pools || [])
      .slice()
      .sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      })
      .forEach(function (p) {
        var opt = document.createElement("option");
        opt.value = p.name;
        opt.textContent =
          poolDisplayName(p) +
          (poolMemberCount(p.name) > 0
            ? "（" + poolMemberCount(p.name) + " 成员）"
            : "");
        sel.appendChild(opt);
      });
    var known = (ts.targetPoolsSnapshot || state.pools || []).some(function (p) {
      return String(p.name || "") === val;
    });
    if (val && !known) {
      // Roster absence is not proof of deletion: the roster can be stale after
      // a failed refresh or a concurrent change. Keep the saved membership
      // selectable; choosing "不属于任何池" still unbinds it explicitly.
      var current = document.createElement("option");
      current.value = val;
      current.textContent = poolDisplayName(val);
      sel.appendChild(current);
      sel.value = val;
      return;
    }
    sel.value = val;
    if (sel.value !== val) sel.value = "";
  }
  document
    .getElementById("tgtType")
    .addEventListener("change", updateTargetFormVisibility);
  function setUsageAdapterError(message) {
    var error = document.getElementById("tgtUsageAdapterError");
    error.textContent = message;
    error.hidden = !message;
    document.getElementById("tgtUsageAdapter").setAttribute(
      "aria-invalid", message ? "true" : "false",
    );
  }
  document.getElementById("tgtUsageAdapter").addEventListener("change", function () {
    setUsageAdapterError("");
  });
  return {
    updateTargetFormVisibility: updateTargetFormVisibility,
    applyEndpointPlaceholder: applyEndpointPlaceholder,
    openTargetModal: openTargetModal,
    renderTargetPoolOptions: renderTargetPoolOptions,
    setUsageAdapterError: setUsageAdapterError,
    updateTargetPoolAvailability: updateTargetPoolAvailability,
  };
});
