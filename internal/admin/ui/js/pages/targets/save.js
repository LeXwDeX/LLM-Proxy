"use strict";
// Targets save: validates the form and creates or updates a target.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsSave", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var closeModal = app.bind("core", "closeModal");
  var epMeta = app.bind("schema", "epMeta");
  var collectCustomBody = app.bind("targetsMappings", "collectCustomBody");
  var collectCustomHeaders = app.bind("targetsMappings", "collectCustomHeaders");
  var collectModelMappings = app.bind("targetsMappings", "collectModelMappings");
  var loadTargets = app.bind("targets", "loadTargets");
  var renderKeyList = app.bind("targetsKeyList", "renderKeyList");
  var setUsageAdapterError = app.bind("targetsModal", "setUsageAdapterError");
  document
    .getElementById("saveTargetBtn")
    .addEventListener("click", async function () {
      var name = document.getElementById("tgtName").value.trim();
      var epType = document.getElementById("tgtType").value;
      var endpoint = document.getElementById("tgtEndpoint").value.trim();
      var usageAdapter = document.getElementById("tgtUsageAdapter").value;
      var allowBearer = document.getElementById("tgtAllowBearer").checked;
      var paused = document.getElementById("tgtPaused").checked;
      var authMode = document.getElementById("tgtAuthMode").value;
      var imageOperation = document.getElementById("tgtImageOperation").value;
      // Only types that support it submit the preference; the server also
      // clears it for any other type.
      var preferWS =
        !!(epMeta(epType) || {}).supports_prefer_websocket &&
        document.getElementById("tgtPreferWS").checked;
      var modelMappings = collectModelMappings();
      var customHeaders = collectCustomHeaders();
      var customBody = collectCustomBody();
      // 统一 keypool：从卡片列表读取，第一个为 api_key，其余为 api_keys
      var allKeys = ts.tgtKeyList
        .map(function (item) {
          return item.ref || item.value;
        })
        .filter(Boolean);
      var apiKey = allKeys.length > 0 ? allKeys[0] : "";
      var apiKeys = allKeys.length > 1 ? allKeys.slice(1) : [];
      var keyResetTime = document
        .getElementById("tgtKeyResetTime")
        .value.trim();
      var pool = document.getElementById("tgtPool").value;

      if (!name) {
        toast("请填写名称", "error");
        return;
      }
      if (!endpoint) {
        toast("请填写 Endpoint URL", "error");
        return;
      }
      if (!state.editingTarget && document.getElementById("tgtNewKeyInput").value.trim()) {
        toast("请先添加输入的密钥，再保存目标。", "error");
        return;
      }
      if (
        !usageAdapter ||
        !state.schema.usage_adapters.some(function (option) {
          return option.value === usageAdapter;
        })
      ) {
        setUsageAdapterError("请选择计费类型后再保存。");
        document.getElementById("tgtUsageAdapter").focus();
        return;
      }
      setUsageAdapterError("");

      if (ts.createdDraftNeedsReopen) {
        toast("目标已创建。请先复制未保存的修改，再重新打开目标编辑。", "error");
        return;
      }
      if (ts.editingKeyBusy || (state.editingTarget && (ts.editingKeyNeedsReview || !ts.editingKeySnapshot))) {
        toast("密钥快照不可用或操作正在进行，请重新确认后保存", "error");
        return;
      }

      var saveSession = ts.editingKeySession;
      var saveRevision = ts.editingDraftRevision;
      var wasEditing = !!state.editingTarget;
      ts.editingKeyBusy = true;
      ts.editingKeySaving = true;
      if (!wasEditing) {
        document.getElementById("tgtNewKeyInput").disabled = true;
        document.getElementById("addKeyBtn").disabled = true;
        renderKeyList();
      }
      try {
        if (state.editingTarget) {
          renderKeyList();
          var updateBody = {
            endpoint_type: epType,
            endpoint: endpoint,
            paused: paused,
            allow_bearer_passthrough: allowBearer,
            auth_mode: authMode,
            usage_adapter: usageAdapter,
            image_operation: imageOperation,
            prefer_websocket: preferWS,
            openai_prefix: document.getElementById("tgtOpenAIPrefix").value,
            anthropic_prefix: document.getElementById("tgtAnthropicPrefix").value,
            supports_responses: document.getElementById("tgtSupportsResponses").checked,
            request_stream_usage_if_missing: document.getElementById("tgtRequestStreamUsage").checked,
            model_mappings: modelMappings,
            model_prefix: document.getElementById("tgtModelPrefix").value,
            custom_headers: customHeaders,
            custom_body: customBody,
            api_key: apiKey,
            api_keys: apiKeys,
            key_reset_time: keyResetTime,
            pool: pool,
          };
          await api("/admin/data/targets/" + encodeURIComponent(name), {
            method: "PUT",
            headers: { "X-Target-Key-Snapshot": ts.editingKeySnapshot },
            body: updateBody,
          });
        } else {
          await api("/admin/data/targets", {
            method: "POST",
            body: {
              name: name,
              endpoint_type: epType,
              endpoint: endpoint,
              api_key: apiKey,
              paused: paused,
              allow_bearer_passthrough: allowBearer,
              auth_mode: authMode,
              usage_adapter: usageAdapter,
              image_operation: imageOperation,
              prefer_websocket: preferWS,
              openai_prefix: document.getElementById("tgtOpenAIPrefix").value,
              anthropic_prefix: document.getElementById("tgtAnthropicPrefix").value,
              supports_responses: document.getElementById("tgtSupportsResponses").checked,
              request_stream_usage_if_missing: document.getElementById("tgtRequestStreamUsage").checked,
              model_mappings: modelMappings,
            model_prefix: document.getElementById("tgtModelPrefix").value,
              custom_headers: customHeaders,
              custom_body: customBody,
              api_keys: apiKeys,
              key_reset_time: keyResetTime,
              pool: pool,
            },
          });
        }
        if (saveSession !== ts.editingKeySession) {
          await loadTargets();
          return;
        }
        if (!wasEditing) {
          document.getElementById("tgtNewKeyInput").disabled = false;
          document.getElementById("addKeyBtn").disabled = false;
        }
        if (saveRevision !== ts.editingDraftRevision) {
          ts.editingKeyBusy = false;
          ts.editingKeySaving = false;
          if (wasEditing) ts.editingKeyNeedsReview = true;
          else {
            ts.createdDraftNeedsReopen = true;
            document.getElementById("tgtNewKeyInput").disabled = true;
            document.getElementById("addKeyBtn").disabled = true;
          }
          renderKeyList();
          toast(wasEditing ? "请求时的内容已保存，后续修改未保存；请重新确认密钥" :
            "目标已创建，后续修改未保存。请复制草稿并重新打开目标编辑。", "error");
          await loadTargets();
          return;
        }
        toast(wasEditing ? "目标已更新" : "目标已创建", "success");
        state.editingTarget = null;
        ts.editingKeySession++;
        ts.editingKeyBusy = false;
        ts.editingKeySaving = false;
        closeModal("targetModal");
        await loadTargets();
      } catch (e) {
        if (saveSession !== ts.editingKeySession) return;
        if (!wasEditing) {
          document.getElementById("tgtNewKeyInput").disabled = false;
          document.getElementById("addKeyBtn").disabled = false;
        }
        ts.editingKeyBusy = false;
        ts.editingKeySaving = false;
        if (state.editingTarget) {
          ts.editingKeyNeedsReview = true;
        }
        renderKeyList();
        toast("保存失败: " + e.message, "error");
      }
    });
  return {
  };
});
