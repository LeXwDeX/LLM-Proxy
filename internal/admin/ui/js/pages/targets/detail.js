"use strict";
// Targets detail drawer: read-only overview, keys, models and advanced tabs.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsDetail", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var esc = app.bind("core", "esc");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var poolDisplayName = app.bind("pools", "poolDisplayName");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var keyCooldownEnd = app.bind("targetsHealth", "keyCooldownEnd");
  var keyStatusColor = app.bind("targetsHealth", "keyStatusColor");
  var keyStatusNeedsAttention = app.bind("targetsHealth", "keyStatusNeedsAttention");
  var openTargetModal = app.bind("targetsModal", "openTargetModal");
  var targetHealth = app.bind("targetsHealth", "targetHealth");
  var targetKeyCounts = app.bind("targetsHealth", "targetKeyCounts");
  var targetKeyStatusComplete = app.bind("targetsHealth", "targetKeyStatusComplete");
  var targetPhaseKnown = app.bind("targetsHealth", "targetPhaseKnown");
  var targetRuntimeFor = app.bind("targetsHealth", "targetRuntimeFor");
  function detailRow(label, value) {
    return (
      "<div class='target-detail-row'><dt>" +
      esc(label) +
      "</dt><dd>" +
      value +
      "</dd></div>"
    );
  }
  function detailGrid(rows) {
    return "<dl class='target-detail-grid'>" + rows.join("") + "</dl>";
  }
  function targetKeyDetailSummary(t) {
    var counts = targetKeyCounts(t);
    if (!counts.total) {
      return t.allow_bearer_passthrough
        ? "未配置独立密钥；已允许 Bearer 透传，客户端凭据可能直接发往上游"
        : "未配置独立密钥";
    }
    if (!Array.isArray(t.key_statuses) || !t.key_statuses.length) {
      return counts.total + " 个密钥；运行态明细尚未上报";
    }
    var parts = [counts.total + " 个密钥"];
    if (counts.blocked) parts.push("屏蔽 " + counts.blocked);
    if (counts.exhausted) parts.push("不可用 " + counts.exhausted);
    if (counts.recoverable) parts.push("可用(待激活) " + counts.recoverable);
    if (!targetKeyStatusComplete(t)) {
      parts.push("运行态明细不完整");
    } else if (!counts.blocked && !counts.exhausted) {
      parts.push(counts.recoverable ? "无阻塞异常" : "未报告异常");
    }
    return parts.join("；");
  }
  function renderTargetOverview(t) {
    var health = targetHealth(t);
    var rt = targetRuntimeFor(t);
    var models = Array.isArray(t.model_mappings) ? t.model_mappings : [];
    var pausedModels = models.filter(function (m) {
      return !!m.paused;
    }).length;
    var runtimeText;
    if (!rt) {
      runtimeText = "运行态不可用（/admin/metrics 未返回该目标）";
    } else if (!targetPhaseKnown(rt.phase)) {
      runtimeText = "运行态不可用（调度相位缺失或无法识别）";
    } else {
      runtimeText =
        schemaLabel("target_phases", rt.phase) +
        " · 在途 " +
        Number(rt.in_flight || 0) +
        (rt.max_concurrency > 0 ? "/" + rt.max_concurrency : "") +
        " · 亲和 " +
        Number(rt.affinity_hits || 0) +
        " · 熔断 " +
        Number(rt.breaker_trips || 0);
      if (rt.window_15m) {
        runtimeText += " · 15 分钟峰值 " + Number(rt.window_15m.in_flight_peak || 0);
      }
    }
    return (
      detailGrid([
        detailRow(
          "健康状态",
          "<span class='target-health-badge tone-" +
            esc(health.tone) +
            "'>" +
            esc(health.label) +
            "</span> " +
            esc(health.reason),
        ),
        detailRow("类型", esc(epDisplayName(t.endpoint_type) || t.endpoint_type || "—")),
        detailRow(
          "Endpoint",
          t.endpoint
            ? "<span class='target-detail-mono'>" + esc(t.endpoint) + "</span>" +
              (isReadonlyMode() ? "（仅显示协议和主机）" : "")
            : isReadonlyMode() ? "不可查看" : "未配置",
        ),
        detailRow(
          "部署池",
          t.pool ? esc(poolDisplayName(t.pool)) : "未加入部署池",
        ),
        detailRow("调度观测", esc(runtimeText)),
        detailRow("密钥", esc(targetKeyDetailSummary(t))),
        detailRow(
          "模型",
          models.length
            ? esc(
                models.length +
                  " 个映射" +
                  (pausedModels ? "，" + pausedModels + " 个已暂停" : ""),
              )
            : "未限定模型",
        ),
      ]) +
      "<p class='target-detail-note'>详情只读取 /admin/data/targets 与 /admin/metrics 的现有字段，不展示明文密钥或逐 Key 请求数。</p>"
    );
  }
  function renderTargetKeys(t) {
    var statuses = Array.isArray(t.key_statuses)
      ? t.key_statuses.filter(function (s) { return !!s && typeof s === "object"; })
      : [];
    var counts = targetKeyCounts(t);
    if (!statuses.length) {
      return (
        "<div class='target-detail-empty'>" +
        (counts.total
          ? esc(counts.total + " 个密钥已保存，运行状态明细尚未上报。")
          : "未配置独立密钥。") +
        "</div><p class='target-detail-note'>明文密钥不会回读。密钥状态由运行中的 Key 池上报；未上报时不推断每个 Key 的请求数。</p>" +
        (t.allow_bearer_passthrough
          ? "<p class='target-detail-note'>已允许 Bearer 透传：未配置独立密钥时，客户端凭据可能直接发往上游。</p>"
          : "")
      );
    }
    return (
      "<div class='target-detail-keys'>" +
      statuses
        .map(function (status) {
          var meta = [];
          if (status.blocked) meta.push("已屏蔽");
          if (keyStatusNeedsAttention(status) && !status.blocked)
            meta.push("额度或限流不可用");
          var cooldownEnd = keyCooldownEnd(status);
          if (cooldownEnd) meta.push("冷却结束 " + cooldownEnd);
          return (
            "<div class='target-detail-key'><span class='target-detail-key-mask'>" +
            esc(status.key_mask || "掩码不可用") +
            "</span>" +
            (status.status
              ? "<span style='" +
                keyStatusColor(status.status) +
                ";padding:2px 8px;border-radius:10px;font-size:11px;font-weight:600;white-space:nowrap;'>" +
                esc(status.status) +
                "</span>"
              : "") +
            (meta.length
              ? "<span class='target-detail-key-meta'>" + esc(meta.join(" · ")) + "</span>"
              : "") +
            "</div>"
          );
        })
        .join("") +
      "</div>" +
      (!targetKeyStatusComplete(t) ? "<p class='target-detail-note'>密钥运行态明细不完整。</p>" : "") +
      "<p class='target-detail-note'>仅显示密钥掩码和运行状态；不显示明文密钥，也不推导每个 Key 的请求数。</p>"
    );
  }
  function renderTargetModels(t) {
    var models = Array.isArray(t.model_mappings) ? t.model_mappings : [];
    if (!models.length) {
      return (
        "<div class='target-detail-empty'>未限定模型；上游接受配置中的任意模型。</div>" +
        "<p class='target-detail-note'>模型行只反映已保存的上游映射，不提供默认模型或测试模型选择。</p>"
      );
    }
    return (
      "<div class='target-detail-models'><div class='target-model-list'>" +
      models
        .map(function (m) {
          var label = m.fallback
            ? m.upstream + " (" + m.fallback + ")"
            : m.upstream;
          if (m.paused) label = "已暂停 · " + label;
          return (
            "<span class='target-model-badge' title='" +
            esc(label) +
            "'>" +
            esc(label) +
            "</span>"
          );
        })
        .join("") +
      "</div></div><p class='target-detail-note'>括号内为下游兜底别名；已暂停的模型不接收请求。</p>"
    );
  }
  function renderTargetAdvanced(t) {
    var headerNames =
      t.custom_headers && typeof t.custom_headers === "object"
        ? Object.keys(t.custom_headers)
        : [];
    var bodyNames =
      t.custom_body && typeof t.custom_body === "object"
        ? Object.keys(t.custom_body)
        : [];
    function onOff(value) {
      return value ? "开启" : "关闭";
    }
    function raw(value) {
      return value ? "<span class='target-detail-mono'>" + esc(value) + "</span>" : "未配置";
    }
    return (
      detailGrid([
        detailRow(
          "用量适配",
          t.usage_adapter
            ? esc(schemaLabel("usage_adapters", t.usage_adapter))
            : "未配置",
        ),
        detailRow(
          "认证方式",
          t.auth_mode ? esc(schemaLabel("auth_modes", t.auth_mode)) : "默认",
        ),
        detailRow("允许 Bearer 透传", onOff(t.allow_bearer_passthrough)),
        detailRow("Key 额度重置时间", raw(t.key_reset_time)),
        detailRow("模型前缀", raw(t.model_prefix)),
        detailRow(
          "图片操作",
          t.image_operation
            ? esc(schemaLabel("image_operations", t.image_operation) || t.image_operation)
            : "未配置",
        ),
        detailRow("请求流式用量", onOff(t.request_stream_usage_if_missing)),
        detailRow("Responses 优先 WebSocket", onOff(t.prefer_websocket)),
        detailRow("上游支持 Responses", onOff(t.supports_responses)),
        detailRow("OpenAI 路径前缀", raw(t.openai_prefix)),
        detailRow("Anthropic 路径前缀", raw(t.anthropic_prefix)),
        detailRow(
          "自定义请求头",
          isReadonlyMode() ? "不可查看" : headerNames.length
            ? esc(headerNames.join("、")) + "<span class='target-detail-key-meta'>（仅字段名）</span>"
            : "未配置",
        ),
        detailRow(
          "自定义请求体",
          isReadonlyMode() ? "不可查看" : bodyNames.length
            ? esc(bodyNames.join("、")) + "<span class='target-detail-key-meta'>（仅字段名）</span>"
            : "未配置",
        ),
      ]) +
      (isReadonlyMode()
        ? "<p class='target-detail-note'>只读用户不可查看自定义请求头与请求体。</p>"
        : "<p class='target-detail-note'>高级设置是当前保存值的只读视图；修改请使用「编辑目标」。自定义请求头与请求体只显示字段名，不显示值。</p>")
    );
  }
  function setTargetDetailTab(tab) {
    ts.detailTab = tab;
    document
      .querySelectorAll("[data-target-detail-tab]")
      .forEach(function (button) {
        var active = button.dataset.targetDetailTab === tab;
        button.classList.toggle("active", active);
        button.setAttribute("aria-selected", active ? "true" : "false");
        button.tabIndex = active ? 0 : -1;
      });
    [
      ["targetDetailOverview", "overview"],
      ["targetDetailKeys", "keys"],
      ["targetDetailModels", "models"],
      ["targetDetailAdvanced", "advanced"],
    ].forEach(function (entry) {
      var panel = document.getElementById(entry[0]);
      panel.hidden = entry[1] !== tab;
    });
  }
  function switchTargetDetailTab(tab) {
    var valid = ["overview", "keys", "models", "advanced"];
    if (valid.indexOf(tab) < 0) return;
    setTargetDetailTab(tab);
  }
  function renderTargetDetail(t) {
    var health = targetHealth(t);
    document.getElementById("targetDetailTitle").textContent = t.name;
    document.getElementById("targetDetailSubtitle").textContent =
      health.label +
      " · " +
      (epDisplayName(t.endpoint_type) || t.endpoint_type || "类型未知");
    document.getElementById("targetDetailOverview").innerHTML = renderTargetOverview(t);
    document.getElementById("targetDetailKeys").innerHTML = renderTargetKeys(t);
    document.getElementById("targetDetailModels").innerHTML = renderTargetModels(t);
    document.getElementById("targetDetailAdvanced").innerHTML = renderTargetAdvanced(t);
    setTargetDetailTab(ts.detailTab);
  }
  function openTargetDetail(name) {
    var t = (state.targets || []).find(function (target) {
      return target.name === name;
    });
    if (!t) return false;
    ts.detailTargetName = name;
    ts.detailTab = "overview";
    renderTargetDetail(t);
    openModal("targetDetailDrawer");
    return true;
  }
  function closeTargetDetail() {
    closeModal("targetDetailDrawer");
  }
  function refreshOpenTargetDetail() {
    if (!ts.detailTargetName) return;
    var drawer = document.getElementById("targetDetailDrawer");
    if (!drawer || !drawer.classList.contains("open")) return;
    var t = (state.targets || []).find(function (target) {
      return target.name === ts.detailTargetName;
    });
    if (!t) {
      ts.detailTargetName = "";
      closeTargetDetail();
      return;
    }
    renderTargetDetail(t);
  }
  document
    .getElementById("targetDetailTabs")
    .addEventListener("click", function (event) {
      var tab = event.target.closest("[data-target-detail-tab]");
      if (tab) switchTargetDetailTab(tab.dataset.targetDetailTab);
    });
  document
    .getElementById("targetDetailTabs")
    .addEventListener("keydown", function (event) {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      var tabs = Array.prototype.slice.call(
        document.querySelectorAll("[data-target-detail-tab]"),
      );
      var current = tabs.indexOf(document.activeElement);
      if (current < 0) return;
      event.preventDefault();
      var next =
        (current + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) %
        tabs.length;
      tabs[next].focus();
      switchTargetDetailTab(tabs[next].dataset.targetDetailTab);
    });
  document
    .getElementById("targetDetailEditBtn")
    .addEventListener("click", function () {
      var target = (state.targets || []).find(function (candidate) {
        return candidate.name === ts.detailTargetName;
      });
      if (!target) return;
      closeTargetDetail();
      openTargetModal(target);
    });
  return {
    openTargetDetail: openTargetDetail,
    closeTargetDetail: closeTargetDetail,
    switchTargetDetailTab: switchTargetDetailTab,
    refreshOpenTargetDetail: refreshOpenTargetDetail,
  };
});
