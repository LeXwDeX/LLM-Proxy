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
  var collectCustomBodyRaw = app.bind("targetsMappings", "collectCustomBodyRaw");
  var collectCustomHeaders = app.bind("targetsMappings", "collectCustomHeaders");
  var collectModelMappings = app.bind("targetsMappings", "collectModelMappings");
  var loadTargets = app.bind("targets", "loadTargets");
  var renderKeyList = app.bind("targetsKeyList", "renderKeyList");
  var setUsageAdapterError = app.bind("targetsModal", "setUsageAdapterError");
  var setTargetFormLocked = app.bind("targetsModal", "setTargetFormLocked");
  var etagHeaders = app.bind("targetsHealth", "etagHeaders");
  var targetETag = app.bind("targetsHealth", "targetETag");
  var isUnconfirmedOutcome = app.bind("core", "isUnconfirmedOutcome");
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
      // Raw JSON text, never a parsed value: JSON.parse would round large integers.
      var customBodyRaw = collectCustomBodyRaw();
      // 统一 keypool：从卡片列表读取，第一个为 api_key，其余为 api_keys。
      // 已保存的空主密钥槽位保持为空，不让第一个附加密钥顶替主密钥。
      var emptyPrimary = !!(ts.tgtKeyList[0] && ts.tgtKeyList[0].emptyPrimary);
      var allKeys = ts.tgtKeyList
        .filter(function (item) { return !item.emptyPrimary; })
        .map(function (item) {
          return item.ref || item.value;
        })
        .filter(Boolean);
      var apiKey = emptyPrimary ? "" : allKeys.length > 0 ? allKeys[0] : "";
      var apiKeys = emptyPrimary ? allKeys : allKeys.length > 1 ? allKeys.slice(1) : [];
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

      if (ts.editingKeySaving) return;
      if (ts.editingKeyBusy) {
        toast("密钥操作进行中，请稍后再保存", "error");
        return;
      }
      var wasEditing = !!state.editingTarget;
      if (wasEditing && !ts.editingETag) {
        toast("目标版本信息缺失，请重新加载目标后再保存", "error");
        return;
      }
      // Key IDs unusable: the key list is display-only, so the request carries no
      // key fields and the server keeps the stored keys.
      var sendKeys = !wasEditing || ts.editingKeyIdsOk;
      var sentETag = ts.editingETag;

      var saveSession = ts.editingKeySession;
      var request;
      if (wasEditing) {
        request = {
          path: "/admin/data/targets/" + encodeURIComponent(name),
          method: "PUT",
          // The server recomputes the stored target's ETag and compares it with this one.
          headers: etagHeaders(sentETag),
          body: {
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
            custom_body_raw: customBodyRaw,
            key_reset_time: keyResetTime,
            pool: pool,
          },
        };
        if (sendKeys) {
          request.body.api_key = apiKey;
          request.body.api_keys = apiKeys;
        }
      } else {
        request = {
          path: "/admin/data/targets",
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
            custom_body_raw: customBodyRaw,
            api_keys: apiKeys,
            key_reset_time: keyResetTime,
            pool: pool,
          },
        };
      }
      // Read-only while in flight: the request body above is exactly what is saved.
      ts.editingKeySaving = true;
      ts.editingConflict = false;
      renderKeyList();
      setTargetFormLocked(true);
      var failure = null;
      var result = null;
      try {
        result = await api(request.path, { method: request.method, headers: request.headers, body: request.body });
      } catch (e) {
        failure = e;
      }
      var warning = result && typeof result.warning === "string" ? result.warning : "";
      if (saveSession !== ts.editingKeySession) {
        // Another editor owns the modal now; the outcome is still reported, and
        // only the list needs to catch up.
        if (failure) {
          if (isUnconfirmedOutcome(failure)) {
            toast("目标「" + name + "」保存结果未确认，请在列表中核对", "warn");
            await loadTargets();
            return;
          }
          toast(failure.status === 409 && failure.code === "target_revision_conflict"
            ? "目标「" + name + "」已被修改，保存未生效，请重新打开后确认" : "保存目标「" + name + "」失败: " + failure.message, "error");
        } else if (warning) {
          toast("目标「" + name + "」已保存，但认证配置刷新待处理，请稍后核对", "warn");
        }
        if (!failure) await loadTargets();
        return;
      }
      ts.editingKeySaving = false;
      setTargetFormLocked(false);
      if (failure) {
        if (wasEditing && failure.status === 409 && failure.code === "target_revision_conflict") {
          // Nothing was written. Keep the modal and the draft; the user decides when to reload.
          ts.editingConflict = true;
          renderKeyList();
          toast("该目标已被修改，请重新加载后再保存", "error");
          return;
        }
        renderKeyList();
        if (isUnconfirmedOutcome(failure)) {
          // The request may or may not have reached the server: look before reporting.
          await reconcileUnconfirmedSave(saveSession, name, wasEditing, sentETag, request.body, sendKeys, failure);
          return;
        }
        toast("保存失败: " + failure.message, "error");
        return;
      }
      // Saved. Closing and refreshing are separate from saving, so trouble in them
      // is reported as what it is, never as a failed save.
      if (warning) {
        toast("目标已保存，但认证配置刷新待处理（auth reload pending），请稍后核对", "warn");
      } else {
        toast((wasEditing ? "目标已更新" : "目标已创建") + (result && result.quota_refresh === "pending" ? "，额度将在后台刷新" : ""), "success");
      }
      state.editingTarget = null;
      ts.editingKeySession++;
      closeModal("targetModal");
      await loadTargets();
    });
  // The server's key mask (admin handlers_targets.go maskKey), for matching a key typed
  // into the draft against the masked list the server returns.
  function serverMask(key) {
    return key.length <= 8 ? "****" : key.slice(0, 4) + "..." + key.slice(-4);
  }
  var REF_PREFIX = "__existing_key_id__:";
  // Fields of a save request that are compared with the server's copy. Key fields are
  // compared separately (by key ID, in order); fields the request omitted are not compared.
  var COMPARED_FIELDS = [
    "endpoint_type", "endpoint", "paused", "allow_bearer_passthrough", "auth_mode", "usage_adapter",
    "image_operation", "prefer_websocket", "openai_prefix", "anthropic_prefix", "supports_responses",
    "request_stream_usage_if_missing", "model_mappings", "model_prefix", "custom_headers", "custom_body",
    "custom_body_raw",
    "key_reset_time", "pool",
  ];
  // Stable form with the server's own normalisation applied and "empty" values dropped,
  // so null / "" / false / [] / {} and key order never cause a false difference.
  function canonical(value) {
    if (Array.isArray(value)) {
      var items = value.map(canonical).filter(function (v) { return v !== undefined; });
      return items.length ? items : undefined;
    }
    if (value && typeof value === "object") {
      var out = {};
      Object.keys(value).sort().forEach(function (k) {
        var c = canonical(value[k]);
        if (c !== undefined) out[k] = c;
      });
      return Object.keys(out).length ? out : undefined;
    }
    if (value === null || value === undefined || value === "" || value === false) return undefined;
    return value;
  }
  function comparableFields(source, sent) {
    var picked = {};
    COMPARED_FIELDS.forEach(function (field) {
      if (sent && !Object.prototype.hasOwnProperty.call(sent, field)) return;
      var v = source[field];
      if (typeof v === "string") {
        v = v.trim();
        if (field === "pool" || field === "auth_mode" || field === "image_operation") v = v.toLowerCase();
      }
      picked[field] = v;
    });
    // The server fills legacy-alias prefixes itself: an empty submitted prefix claims nothing.
    ["openai_prefix", "anthropic_prefix"].forEach(function (field) {
      if (sent && !sent[field]) delete picked[field];
    });
    return JSON.stringify(canonical(picked) || {});
  }
  // The submitted key list (references by ID, new keys by value), against the server's.
  function keysMatch(body, fresh) {
    var wanted = [];
    if (typeof body.api_key === "string" && body.api_key) wanted.push(body.api_key);
    (body.api_keys || []).forEach(function (k) { if (k) wanted.push(k); });
    if (!Array.isArray(fresh.key_ids)) return wanted.length === 0;
    var stored = [];
    fresh.key_ids.forEach(function (id, slot) {
      if (id === null) return;
      stored.push({ id: id, mask: slot === 0 ? fresh.api_key : (fresh.api_keys || [])[slot - 1] });
    });
    if (stored.length !== wanted.length) return false;
    return wanted.every(function (item, i) {
      if (item.indexOf(REF_PREFIX) === 0) return item.slice(REF_PREFIX.length) === stored[i].id;
      return serverMask(item.trim()) === stored[i].mask;
    });
  }
  // Does the server's copy of the target hold what this request asked for? Judged by
  // content, never by ETag alone: the request's own fields, key IDs in order.
  function serverHoldsRequest(body, fresh, sendKeys) {
    if (!fresh) return false;
    if (comparableFields(body, body) !== comparableFields(fresh, body)) return false;
    return sendKeys ? keysMatch(body, fresh) : true;
  }
  // After a timeout, network error, 5xx or non-JSON error body the save may have been
  // applied. Read the target back and decide by content:
  //  - it holds what was submitted: the save happened (a green "已保存"), whatever the ETag;
  //  - it differs and the ETag moved: someone else changed it - the same conflict flow as 409,
  //    the draft is kept and the form is never replaced behind the user's back;
  //  - it differs and the ETag is unchanged: nothing was written, retry is safe (If-Match still guards).
  // A new target is created or not: found with the same content, found with other content
  // (a same-named target exists), or absent.
  async function reconcileUnconfirmedSave(session, name, wasEditing, sentETag, body, sendKeys, failure) {
    var targets = null;
    try {
      targets = await loadTargets(true);
    } catch (e) {
      targets = null;
    }
    if (session !== ts.editingKeySession) return;
    var fresh = targets && targets.find(function (t) { return t.name === name; });
    if (!targets) {
      toast("保存结果未确认（" + failure.message + "），且无法获取服务器状态，请刷新后核对", "warn");
      return;
    }
    if (serverHoldsRequest(body, fresh, sendKeys)) {
      toast("已保存", "success");
      state.editingTarget = null;
      ts.editingKeySession++;
      closeModal("targetModal");
      return;
    }
    if (wasEditing) {
      if (!fresh) {
        toast("保存结果未确认，该目标已不在列表中，请核对", "warn");
        return;
      }
      if (targetETag(fresh) !== sentETag) {
        ts.editingConflict = true;
        renderKeyList();
        toast("该目标已被修改，请重新加载后再保存", "error");
        return;
      }
    } else if (fresh) {
      toast("同名目标「" + name + "」已存在且内容不同，请更换名称或在列表中核对", "warn");
      return;
    }
    toast("保存未完成，可重试", "warn");
  }
  return {
  };
});
