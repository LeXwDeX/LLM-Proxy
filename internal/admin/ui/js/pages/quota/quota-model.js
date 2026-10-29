"use strict";
// Quota model: dimensions, labels, job-level lookups and the shared load state.
AdminApp.register("quotaModel", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtTime = app.bind("core", "fmtTime");
  var epDisplayName = app.bind("schema", "epDisplayName");
  function modelGroupModels(group) {
    var targets = (state.targets || []).map(function (t) {
      return String(t.name || "").toLowerCase();
    });
    return (
      (group.bindings || [])
        .map(function (binding) {
          var label = esc(
            binding.target +
              (binding.model ? " · " + binding.model : " · 全部模型"),
          );
          if (targets.indexOf(String(binding.target || "").toLowerCase()) < 0)
            label += "<span class='quota-target-deleted'>（目标已删除）</span>";
          return label;
        })
        .join(", ") || "未配置模型"
    );
  }
  // Runs fn once at a time per key and keeps the button disabled while it is in flight.
  var inflight = Object.create(null);
  async function runGuarded(key, button, fn) {
    if (inflight[key]) return;
    inflight[key] = true;
    var wasDisabled = button ? button.disabled : false;
    if (button) button.disabled = true;
    try {
      return await fn();
    } finally {
      inflight[key] = false;
      if (button) button.disabled = wasDisabled;
    }
  }
  // Reads a numeric <input>: null when blank, NaN when the browser holds text it cannot parse
  // (type=number reports value "" with validity.badInput), otherwise the number.
  function quotaInputNumber(input) {
    if (!input) return null;
    if (input.validity && input.validity.badInput) return NaN;
    var raw = String(input.value == null ? "" : input.value).trim();
    return raw === "" ? null : Number(raw);
  }
  function modelGroupLabel(name) {
    return name === "company_general" ? "公司通用" : name;
  }

  /* ===== Quota Management ===== */
  function quotaTargetByName(name) {
    return (state.targets || []).find(function (target) {
      return String(target.name).toLowerCase() === String(name).toLowerCase();
    });
  }
  // Display names are looked up per rendered option, so index the catalog once per cache array
  // (rebuilt when the array object or its length changes) instead of scanning it for every call.
  var catalogIndex = { source: null, size: -1, entries: null };
  function catalogEntryFor(endpointType, model) {
    var source = state.catalogAllCache || null;
    var size = source ? source.length : 0;
    if (catalogIndex.source !== source || catalogIndex.size !== size) {
      var entries = Object.create(null);
      (source || []).forEach(function (item) {
        var key = item.endpoint_type + "\u0000" + String(item.model || "").toLowerCase();
        if (!entries[key]) entries[key] = item;
      });
      catalogIndex = { source: source, size: size, entries: entries };
    }
    return catalogIndex.entries[endpointType + "\u0000" + String(model || "").toLowerCase()];
  }
  function quotaModelLabel(target, model) {
    var entry = catalogEntryFor(target.endpoint_type, model);
    return (
      model +
      (entry && entry.display_name && entry.display_name !== model
        ? " · " + entry.display_name
        : "")
    );
  }
  function quotaTargetOptions(selected) {
    return (
      "<option value=''>选择 Target</option>" +
      (state.targets || [])
        .slice()
        .sort(function (a, b) {
          return String(a.name || "").localeCompare(String(b.name || ""));
        })
        .map(function (target) {
          var meta =
            epDisplayName(target.endpoint_type || "") ||
            target.endpoint_type ||
            "";
          return (
            "<option value='" +
            esc(target.name) +
            "'" +
            (String(target.name).toLowerCase() === String(selected).toLowerCase()
              ? " selected" : "") +
            ">" +
            esc(target.name + " · " + meta) +
            "</option>"
          );
        })
        .join("")
    );
  }
  function quotaModelOptions(targetName, selected) {
    var target = quotaTargetByName(targetName),
      mappings =
        target && Array.isArray(target.model_mappings)
          ? target.model_mappings
          : [];
    var html = "<option value=''>整个 Target</option>";
    mappings.forEach(function (mapping) {
      var model = mapping.upstream || "";
      if (model)
        html +=
          "<option value='" +
          esc(model) +
          "'" +
          (model === selected ? " selected" : "") +
          ">" +
          esc(quotaModelLabel(target, model)) +
          "</option>";
    });
    if (
      selected &&
      !mappings.some(function (mapping) {
        return mapping.upstream === selected;
      })
    ) {
      html +=
        "<option value='" + esc(selected) + "' selected>" +
        esc(selected) + "（模型已移除）</option>";
    }
    return html;
  }
  function globalQuotaGroupByName(name) {
    return (state.modelGroups || []).find(function (group) {
      return String(group.name).toLowerCase() === String(name).toLowerCase();
    });
  }
  // 额度 v2：职级为结构化记录 {name, coefficient_daily/weekly/monthly}；
  // 兼容旧字符串字典形态。
  function jobLevelEntryName(entry) {
    return typeof entry === "string"
      ? entry
      : String((entry && entry.name) || "");
  }
  function jobLevelByName(name) {
    var key = String(name || "")
      .trim()
      .toLowerCase();
    if (!key) return null;
    return (
      (state.jobLevels || []).find(function (l) {
        return jobLevelEntryName(l).toLowerCase() === key;
      }) || null
    );
  }
  function jobLevelCoef(level, dimKey) {
    if (!level || typeof level === "string") return 1;
    var dim = quotaDimensions.find(function (item) {
      return item.key === dimKey;
    });
    var v = dim ? level[dim.job_coefficient_field] : null;
    return v == null ? 1 : Number(v);
  }
  function jobLevelCoefText(v) {
    if (v == null) return "1";
    return Number(v) === 0 ? "0（不限）" : quotaNumber(v);
  }

  var quotaDimensions = state.schema.quota_dimensions.map(function (dim) {
    return Object.assign({ key: dim.value }, dim);
  });
  function quotaNumber(v) {
    var n = Number(v || 0);
    return Number.isInteger(n)
      ? String(n)
      : n.toFixed(4).replace(/0+$/, "").replace(/\.$/, "");
  }
  function quotaResetText(value) {
    if (!value) return "";
    var d = new Date(value);
    if (!isFinite(d.getTime()) || d.getUTCFullYear() <= 1) return "";
    return fmtTime(value) + "（本地时间）";
  }
  // Cross-file mutable state is owned here and shared by reference; never reassign the object.
  var shared = {
    quotaLoadVersion: 0,
    dictLoadVersion: 0,
    modelGroupNeedsRefresh: Object.create(null),
  };
  return {
    modelGroupModels: modelGroupModels,
    modelGroupLabel: modelGroupLabel,
    quotaTargetByName: quotaTargetByName,
    quotaModelLabel: quotaModelLabel,
    quotaTargetOptions: quotaTargetOptions,
    quotaModelOptions: quotaModelOptions,
    globalQuotaGroupByName: globalQuotaGroupByName,
    jobLevelEntryName: jobLevelEntryName,
    jobLevelByName: jobLevelByName,
    jobLevelCoef: jobLevelCoef,
    jobLevelCoefText: jobLevelCoefText,
    quotaNumber: quotaNumber,
    quotaResetText: quotaResetText,
    runGuarded: runGuarded,
    quotaInputNumber: quotaInputNumber,
    quotaDimensions: quotaDimensions,
    shared: shared,
  };
});
