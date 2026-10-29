"use strict";
// Targets health: key status predicates, key identity helpers, per-target health, counts, sort and filter.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsHealth", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var schemaLabel = app.bind("schema", "schemaLabel");
  var targetHealthRanks = { attention: 0, healthy: 1, paused: 2 };
  // keypool.status keeps exhausted=true after an expired cooldown; only the
  // 可用(待激活) display label means the key can be retried without an issue.
  // 失效(待请求验证), blocked keys and active cooldowns still need attention.
  function keyStatusNeedsAttention(status) {
    return (
      !!status &&
      (!!status.blocked ||
        (!!status.exhausted && status.status !== "可用(待激活)"))
    );
  }
  function keyStatusRecoverablePending(status) {
    return (
      !!status &&
      !!status.exhausted &&
      !status.blocked &&
      status.status === "可用(待激活)"
    );
  }
  function targetKeyCounts(t) {
    var statuses = t && Array.isArray(t.key_statuses)
      ? t.key_statuses.filter(function (s) { return !!s && typeof s === "object"; })
      : [];
    var extraKeys = t && Array.isArray(t.api_keys) ? t.api_keys.length : 0;
    return {
      total: (t && t.has_api_key ? 1 : 0) + extraKeys,
      blocked: statuses.filter(function (s) {
        return !!s.blocked;
      }).length,
      exhausted: statuses.filter(function (s) {
        return keyStatusNeedsAttention(s) && !s.blocked;
      }).length,
      recoverable: statuses.filter(keyStatusRecoverablePending).length,
    };
  }
  function targetKeyStatusComplete(t) {
    var counts = targetKeyCounts(t);
    if (counts.total <= 1) return true;
    if (keyRuntimeMapUsable(t) && t.key_runtime_indices.every(function (i) { return i === null; })) return true;
    if (hasUnmappedNonemptyKey(t)) return false;
    var statuses = Array.isArray(t.key_statuses) ? t.key_statuses : [];
    var indices = keyRuntimeMapUsable(t) ? t.key_runtime_indices : null;
    var expected = indices
      ? indices.filter(function (i) { return Number.isInteger(i) && i >= 0; })
      : [];
    expected = Array.from(new Set(expected));
    if (!expected.length) {
      expected = Array.from({ length: counts.total }, function (_, i) { return i; });
    }
    var complete = completeStatusIndexes(statuses);
    return expected.every(function (index) { return complete.has(index); });
  }
  // Indexes of statuses that report every field the health logic relies on.
  function completeStatusIndexes(statuses) {
    var complete = new Set();
    statuses.forEach(function (s) {
      if (s && typeof s.blocked === "boolean" && typeof s.exhausted === "boolean" &&
          typeof s.status === "string" && !!s.status) complete.add(s.index);
    });
    return complete;
  }
  function targetRuntimeFor(t) {
    return state.runtime
      ? state.runtime[String((t && t.name) || "").toLowerCase()] || null
      : null;
  }
  // Known phases come from the schema; a missing or malformed snapshot stays
  // attention instead of being treated as healthy.
  function targetPhaseKnown(phase) {
    if (typeof phase !== "string" || !phase) return false;
    var phases = (state.schema && state.schema.target_phases) || [];
    return phases.some(function (option) {
      return option.value === phase;
    });
  }
  // Pool pause is applied to runtime targets (proxy.applyPoolPause); the saved
  // target keeps its own paused flag. Read the roster only, never write it.
  // This roster can lag behind /admin/metrics, so health uses it only when the
  // runtime snapshot is missing or also reports a pause.
  function targetPoolPaused(t) {
    var poolName = String((t && t.pool) || "").toLowerCase();
    if (!poolName || !ts.targetPoolsSnapshot) return false;
    return ts.targetPoolsSnapshot.some(function (p) {
      return (
        String((p && p.name) || "").toLowerCase() === poolName && !!p.paused
      );
    });
  }
  // Health is a pure function of the loaded state, so one render pass computes
  // it once per target (sort, filter, counts and rows all share the result).
  function withHealthMemo(fn) {
    if (ts.healthMemo) return fn();
    ts.healthMemo = new Map();
    try {
      return fn();
    } finally {
      ts.healthMemo = null;
    }
  }
  function targetHealth(t) {
    var memo = ts.healthMemo;
    if (memo && t && typeof t === "object") {
      if (!memo.has(t)) memo.set(t, computeTargetHealth(t));
      return memo.get(t);
    }
    return computeTargetHealth(t);
  }
  function computeTargetHealth(t) {
    var keys = targetKeyCounts(t);
    var rt = targetRuntimeFor(t);
    var phase = rt && rt.phase ? String(rt.phase) : "";
    if (ts.targetsLoadFailed) {
      return {
        key: "attention", label: "需要关注", tone: "warn",
        reason: "目标或部署池数据更新失败；显示上次数据",
      };
    }
    if (t && t.paused) {
      return {
        key: "paused",
        label: "已暂停",
        tone: "muted",
        reason: "目标已暂停，不参与转发",
      };
    }
    // A fresh runtime snapshot is authoritative. Only fall back to the roster
    // when runtime is absent, or use its reason when runtime also shows pause.
    if (!rt) {
      if (targetPoolPaused(t)) {
        return {
          key: "paused",
          label: "已暂停",
          tone: "muted",
          reason:
            "所属部署池已暂停，目标作为池成员停止转发；目标自身暂停设置未修改",
        };
      }
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "运行态不可用：/admin/metrics 未返回该目标",
      };
    }
    if (rt.paused || phase === "offline") {
      if (targetPoolPaused(t)) {
        return {
          key: "paused",
          label: "已暂停",
          tone: "muted",
          reason:
            "所属部署池已暂停，目标作为池成员停止转发；目标自身暂停设置未修改",
        };
      }
      return {
        key: "paused",
        label: "已暂停",
        tone: "muted",
        reason: rt.paused
          ? "运行态显示目标已暂停，不参与转发；目标自身暂停设置未修改"
          : "运行态相位为暂停（offline），不参与转发；目标自身暂停设置未修改",
      };
    }
    if (!targetPhaseKnown(phase)) {
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "运行态不可用：调度相位缺失或无法识别",
      };
    }
    if (keys.blocked || keys.exhausted) {
      var issues = [];
      if (keys.blocked) issues.push(keys.blocked + " 个 Key 已屏蔽");
      if (keys.exhausted) issues.push(keys.exhausted + " 个 Key 额度或限流不可用");
      return {
        key: "attention",
        label: "需要关注",
        tone: "bad",
        reason: issues.join("，"),
      };
    }
    if (!targetKeyStatusComplete(t)) {
      return {
        key: "attention", label: "需要关注", tone: "warn",
        reason: "多密钥运行状态明细不完整",
      };
    }
    if (phase && phase !== "ready") {
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "运行状态：" + (schemaLabel("target_phases", phase) || phase),
      };
    }
    var models = t && Array.isArray(t.model_mappings) ? t.model_mappings : [];
    if (models.length && models.every(function (m) { return !!m.paused; })) {
      return {
        key: "attention",
        label: "需要关注",
        tone: "warn",
        reason: "全部模型已暂停",
      };
    }
    return {
      key: "healthy",
      label: "正常",
      tone: "ok",
      reason: "目标已启用，调度运行正常",
    };
  }
  function targetHealthCounts() {
    var targets = Array.isArray(state.targets) ? state.targets : [];
    var counts = { all: targets.length, attention: 0, healthy: 0, paused: 0 };
    targets.forEach(function (t) {
      var key = targetHealth(t).key;
      if (counts[key] !== undefined) counts[key]++;
    });
    return counts;
  }
  function targetSort(a, b) {
    var ar = targetHealthRanks[targetHealth(a).key];
    var br = targetHealthRanks[targetHealth(b).key];
    if (ar === undefined) ar = 3;
    if (br === undefined) br = 3;
    return ar - br || String(a.name || "").localeCompare(String(b.name || ""));
  }
  function filteredTargets() {
    var targets = Array.isArray(state.targets) ? state.targets : [];
    return targets
      .slice()
      .sort(targetSort)
      .filter(function (t) {
        return ts.targetFilter === "all" || targetHealth(t).key === ts.targetFilter;
      });
  }
  // Go serializes an unset time.Time as 0001-01-01T00:00:00Z. A cooldown that
  // never existed must not read as a real deadline.
  function keyCooldownEnd(status) {
    var value = status && status.cooldown_end;
    if (!value) return "";
    var date = new Date(value);
    if (isNaN(date.getTime()) || date.getUTCFullYear() <= 1) return "";
    return String(value);
  }
  function hasUnmappedNonemptyKey(t) {
    if (!t || !Array.isArray(t.api_keys) || !Array.isArray(t.key_runtime_indices)) return false;
    return t.key_runtime_indices.some(function (index, slot) {
      var mask = slot === 0 ? t.api_key : t.api_keys[slot - 1];
      // Empty slots and short keys both mask as ****; whitespace-only keys are absent at runtime.
      return index === null && typeof mask === "string" && mask !== "****" && !!mask.replace("...", "").trim();
    });
  }
  // The runtime mapping (slot -> keypool index) is health-display data only; it
  // never decides whether a target can be edited or saved.
  function keyRuntimeMapUsable(t) {
    if (
      !t || typeof t.has_api_key !== "boolean" || typeof t.api_key !== "string" ||
      t.has_api_key !== !!t.api_key || !Array.isArray(t.api_keys) ||
      !t.api_keys.every(function (k) { return typeof k === "string"; }) ||
      !Array.isArray(t.key_runtime_indices) ||
      t.key_runtime_indices.length !== t.api_keys.length + 1 ||
      (!t.has_api_key && t.key_runtime_indices[0] !== null) ||
      (hasUnmappedNonemptyKey(t) &&
        !(t.key_runtime_indices.every(function (i) { return i === null; }) &&
          (t.key_statuses == null || (Array.isArray(t.key_statuses) && !t.key_statuses.length))))
    ) return false;
    var complete = completeStatusIndexes(Array.isArray(t.key_statuses) ? t.key_statuses : []);
    return t.key_runtime_indices.every(function (i) {
      return i === null || (Number.isInteger(i) && i >= 0 && complete.has(i));
    });
  }
  // key_ids has the shape of key_runtime_indices: slot 0 is the primary key, an
  // empty slot is null. The server sends key_ids: null when it cannot derive IDs.
  // Without usable IDs the key list is display-only and a save must carry no key
  // fields at all (the server then keeps the stored keys). A null ID in a slot
  // whose mask shows a real key means the payload is not trustworthy either.
  function keyIdsUsable(t) {
    if (!t || !Array.isArray(t.key_ids) || !Array.isArray(t.api_keys) ||
        t.key_ids.length !== t.api_keys.length + 1) return false;
    return t.key_ids.every(function (id, slot) {
      if (typeof id === "string" && !!id) return true;
      if (id !== null) return false;
      // Short keys and empty slots both mask as "****"; only a longer mask proves a key.
      var present = slot === 0 ? t.has_api_key === true : t.api_keys[slot - 1] !== "****";
      return !present;
    });
  }
  // Runtime index and status of one saved key, found by its stable ID.
  function keyRuntimeByID(t, keyId) {
    var slot = t && Array.isArray(t.key_ids) ? t.key_ids.indexOf(keyId) : -1;
    var indices = t && Array.isArray(t.key_runtime_indices) ? t.key_runtime_indices : [];
    var statuses = t && Array.isArray(t.key_statuses) ? t.key_statuses : [];
    var runtimeIndex = slot >= 0 ? indices[slot] : null;
    var status = Number.isInteger(runtimeIndex)
      ? statuses.find(function (s) { return s && s.index === runtimeIndex; }) || null : null;
    return { runtimeIndex: Number.isInteger(runtimeIndex) ? runtimeIndex : null, status: status };
  }
  // Editor key cards for the saved keys. Saved keys are referenced by stable ID,
  // so a card stays valid however the other keys or targets change meanwhile.
  function keyListFromTarget(t) {
    var keys = Array.isArray(t.api_keys) ? t.api_keys : [];
    var list = [];
    if (!keyIdsUsable(t)) {
      // Display only: the save request omits key fields while IDs are unusable.
      if (t.has_api_key || keys.length) list.push({ value: t.api_key || "", existing: true, status: null });
      keys.forEach(function (key) { list.push({ value: key, existing: true, status: null }); });
      return list;
    }
    function add(value, slot) {
      var keyId = t.key_ids[slot];
      if (keyId === null) {
        // An empty primary slot keeps its position; empty extra slots carry nothing.
        if (slot === 0 && keys.length) list.push({ value: "", existing: true, emptyPrimary: true, status: null });
        return;
      }
      var runtime = keyRuntimeByID(t, keyId);
      list.push({
        value: value, existing: true, keyId: keyId, ref: "__existing_key_id__:" + keyId,
        runtimeIndex: runtime.runtimeIndex, status: runtime.status,
      });
    }
    add(t.api_key || "", 0);
    keys.forEach(function (key, i) { add(key, i + 1); });
    return list;
  }
  // Content ETag of a target (hash of the stored bytes). Every stored target has
  // one; "" only for a malformed payload, which must never be saved over.
  function targetETag(t) {
    return t && typeof t.etag === "string" && /^t1-\S+$/.test(t.etag) ? t.etag : "";
  }
  function etagHeaders(etag) {
    return etag ? { "If-Match": '"' + etag + '"' } : undefined;
  }
  function keyStatusColor(s) {
    switch (s) {
      case "已屏蔽":
        return "background:var(--bad-soft);color:var(--bad)";
      case "额度超限":
        return "background:var(--warn-soft);color:var(--warn)";
      case "额度超限(自动恢复)":
        return "background:var(--warn-soft);color:var(--warn)";
      case "额度超限(人工恢复)":
        return "background:var(--warn-soft);color:var(--warn)";
      case "限流中":
        return "background:var(--out-soft);color:var(--out)";
      case "使用中":
        return "background:var(--ok-soft);color:var(--ok)";
      case "等待中":
        return "background:var(--muted-soft);color:var(--ink-3)";
      case "使用过":
        return "background:var(--accent-soft);color:var(--accent)";
      // 新增文案（status() GLM 分类 / 冷却过期区分）
      case "可用(待激活)":
        return "background:var(--accent-soft);color:var(--accent)"; // 浅蓝（sky-50/sky-700），区别于"使用中"绿色但仍属"接近可用"的冷色调
      case "失效(待请求验证)":
        return "background:var(--bad-soft);color:var(--bad)"; // 红色系警示
      case "失效(冷却中)":
        return "background:var(--out-soft);color:var(--out)"; // 橙色系
      case "GLM额度限流":
        return "background:var(--warn-soft);color:var(--warn)"; // 黄色系额度超限色
      case "GLM速率限流":
        return "background:var(--out-soft);color:var(--out)"; // 橙色系限流色
      case "失效":
        return "background:var(--bad-soft);color:var(--bad)"; // 兜底失效
      default:
        return "background:var(--muted-soft);color:var(--ink-3)";
    }
  }
  return {
    targetHealth: targetHealth,
    targetHealthCounts: targetHealthCounts,
    withHealthMemo: withHealthMemo,
    filteredTargets: filteredTargets,
    keyStatusColor: keyStatusColor,
    keyCooldownEnd: keyCooldownEnd,
    keyListFromTarget: keyListFromTarget,
    keyIdsUsable: keyIdsUsable,
    keyRuntimeByID: keyRuntimeByID,
    targetETag: targetETag,
    etagHeaders: etagHeaders,
    keyStatusNeedsAttention: keyStatusNeedsAttention,
    targetKeyCounts: targetKeyCounts,
    targetKeyStatusComplete: targetKeyStatusComplete,
    targetPhaseKnown: targetPhaseKnown,
    targetRuntimeFor: targetRuntimeFor,
  };
});
