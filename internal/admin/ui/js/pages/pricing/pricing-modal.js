"use strict";
// Pricing template modal: scope choices, open/copy flows and save.
AdminApp.register("pricingModal", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var setSchemaSelectValue = app.bind("schema", "setSchemaSelectValue");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var loadQuotaEditorData = app.bind("quota", "loadQuotaEditorData");
  var pricingTargets = app.bind("pricingShared", "pricingTargets");
  var pricingModels = app.bind("pricingShared", "pricingModels");
  var pricingArray = app.bind("pricingShared", "pricingArray");
  var pricingSelected = app.bind("pricingShared", "pricingSelected");
  var pricingSelectValues = app.bind("pricingShared", "pricingSelectValues");
  var pricingStartIsZero = app.bind("pricingShared", "pricingStartIsZero");
  var pricingIsTemplate0 = app.bind("pricingShared", "pricingIsTemplate0");
  var toLocalInputValue = app.bind("pricingShared", "toLocalInputValue");
  var pricingTargetExists = app.bind("pricingShared", "pricingTargetExists");
  var pricingTargetMissing = app.bind("pricingShared", "pricingTargetMissing");
  var pricingTargetOptions = app.bind("pricingShared", "pricingTargetOptions");
  var pricingModelLabel = app.bind("pricingShared", "pricingModelLabel");
  var clonePricingValue = app.bind("pricingShared", "clonePricingValue");
  var pricingInputNumber = app.bind("pricingShared", "pricingInputNumber");
  var runGuarded = app.bind("quotaModel", "runGuarded");
  var pricingPeriodTimeError = app.bind("pricingPeriods", "pricingPeriodTimeError");
  var pricingPeriodTimeValue = app.bind("pricingPeriods", "pricingPeriodTimeValue");
  var renderPricingContext = app.bind("pricingRates", "renderPricingContext");
  var syncPricingContextInput = app.bind("pricingRates", "syncPricingContextInput");
  var validatePricingContext = app.bind("pricingRates", "validatePricingContext");
  var updatePricingModeUI = app.bind("pricingPeriods", "updatePricingModeUI");
  var renderPricingPeriods = app.bind("pricingPeriods", "renderPricingPeriods");
  var collectPricingPeriods = app.bind("pricingPeriods", "collectPricingPeriods");
  var loadPricingTemplates = app.bind("pricingList", "loadPricingTemplates");
  var contextRateFields = app.modules.pricingRates.contextRateFields;
  // Hidden selects retain one source of truth for editing, validation and submission.
  // Search only hides checkbox rows; it never changes the selected option values.
  function filterPricingChoices(id) {
    var query = document.getElementById(id + "Search").value.trim().toLocaleLowerCase();
    var list = document.getElementById(id + "Choices");
    var visible = 0;
    list.querySelectorAll("label").forEach(function (row) {
      row.hidden = query !== "" && row.textContent.toLocaleLowerCase().indexOf(query) < 0;
      if (!row.hidden) visible++;
    });
    var empty = document.getElementById(id + "Empty");
    empty.hidden = visible > 0;
    list.classList.toggle("is-empty", visible === 0);
    empty.textContent = query ? "没有匹配项，已勾选内容保持不变。"
      : id === "ptModel" ? (pricingSelected("ptTarget").length ? "所选 Target 暂无可选模型。" : "先勾选 Target，再选择模型。")
      : "暂无可选 Target。";
  }
  function renderPricingChoices(id) {
    var select = document.getElementById(id);
    document.getElementById(id + "Search").disabled = select.disabled;
    document.getElementById(id + "Count").textContent = "已选 " + pricingSelected(id).length;
    document.getElementById(id + "Choices").innerHTML = Array.from(select.options).filter(function (option) {
      return !option.disabled;
    }).map(function (option) {
      return "<label class='pricing-choice-option'><input type='checkbox' value='" + esc(option.value) + "'" +
        (option.selected ? " checked" : "") + (select.disabled ? " disabled" : "") + "><span>" + esc(option.textContent) + "</span></label>";
    }).join("");
    filterPricingChoices(id);
  }
  function renderPricingSelectionPreview(targets, models) {
    var pairs = [];
    targets.forEach(function (target) {
      models.forEach(function (model) { pairs.push("<li>" + esc((target || "全局兜底") + " / " + model) + "</li>"); });
    });
    document.getElementById("ptSelectionCount").textContent = pairs.length + " 个组合";
    document.getElementById("ptSelectionPreview").innerHTML = pairs.length ? pairs.join("")
      : "<li class='pricing-choice-empty'>" + (!targets.length ? "尚未选择 Target。" : "尚未选择模型。") + "</li>";
  }
  function pricingScopeConflicts(targets, models) {
    if (!targets.length) return ["请选择至少一个 Target"];
    if (targets.length > 1 && targets.indexOf("") >= 0) return ["全局兜底不能与具体 Target 混选"];
    if (!models.length) return [];
    var conflicts = [];
    targets.forEach(function (name) {
      if (!name) return;
      var target = (state.targets || []).find(function (t) { return String(t.name).toLowerCase() === name.toLowerCase(); });
      if (!target) { conflicts.push(name + "：目标已删除或不存在"); return; }
      models.forEach(function (model) {
        if (!(target.model_mappings || []).some(function (m) { return String(m.upstream).toLowerCase() === model.toLowerCase(); }))
          conflicts.push(name + " / " + model + "：未配置此模型");
      });
    });
    return conflicts;
  }
  function updatePricingScopeNotice() {
    var targets = pricingSelected("ptTarget"), models = pricingSelected("ptModel");
    renderPricingChoices("ptTarget");
    renderPricingChoices("ptModel");
    renderPricingSelectionPreview(targets, models);
    var conflicts = pricingScopeConflicts(targets, models);
    var notice = document.getElementById("ptScopeNotice");
    notice.textContent = conflicts.length
      ? conflicts.join("；")
      : models.length ? (targets[0] === "" ? "全局兜底" : targets.length + " 个 Target") + " × " + models.length + " 个模型，共用一套价格。修改价格对全部所选组合生效。"
      : "选择模型后，将显示共用价格的范围。";
    notice.classList.toggle("pricing-scope-error", conflicts.length > 0);
    return conflicts;
  }
  function pricingSubmissionConflict(candidate, editingID) {
    var targets = pricingTargets(candidate), models = pricingModels(candidate);
    var templates = state.pricingTemplates || [];
    for (var i = 0; i < templates.length; i++) {
      var existing = templates[i];
      if (editingID && existing.id === editingID) continue;
      var sharedTarget = targets.find(function (name) { return pricingTargets(existing).some(function (other) { return name.toLowerCase() === other.toLowerCase(); }); });
      var sharedModel = models.find(function (name) { return pricingModels(existing).some(function (other) { return name.toLowerCase() === other.toLowerCase(); }); });
      if (sharedTarget === undefined || sharedModel === undefined) continue;
      var candidateDefault = pricingStartIsZero(candidate.start), existingDefault = pricingStartIsZero(existing.start);
      var label = (sharedTarget || "默认") + " / " + sharedModel;
      if (candidateDefault && existingDefault) return label + "：已有全天兜底价，请编辑原规则";
      if (candidateDefault || existingDefault) continue;
      var start = new Date(candidate.start).getTime(), end = candidate.end ? new Date(candidate.end).getTime() : Infinity;
      var oldStart = new Date(existing.start).getTime(), oldEnd = existing.end ? new Date(existing.end).getTime() : Infinity;
      if (start < oldEnd && oldStart < end) return label + "：生效时间与已有规则重叠";
    }
    return "";
  }
  function renderPricingModelSelect(targetNames, selected, preserveMissing) {
    var sel = document.getElementById("ptModel");
    targetNames = pricingArray(targetNames);
    var selectedModels = pricingArray(selected).filter(Boolean);
    var targets = (state.targets || []).filter(function (target) {
      return targetNames.indexOf("") >= 0 || targetNames.some(function (name) { return String(name).toLowerCase() === String(target.name).toLowerCase(); });
    });
    var models = [], seen = Object.create(null);
    function addModel(model, target) {
      var key = String(model || "").toLowerCase();
      if (!key || seen[key]) return;
      seen[key] = true; models.push({model: model, target: target});
    }
    targets.forEach(function (target) { (target.model_mappings || []).forEach(function (mapping) { addModel(mapping.upstream, target); }); });
    if (preserveMissing) selectedModels.forEach(function (model) { addModel(model, null); });
    models.sort(function (a, b) { return String(a.model).localeCompare(String(b.model)); });
    sel.innerHTML = models.map(function (entry) {
      var selected = selectedModels.some(function (model) { return model.toLowerCase() === entry.model.toLowerCase(); });
      return "<option value='" + esc(entry.model) + "'" + (selected ? " selected" : "") + ">" + esc(pricingModelLabel(entry.target, entry.model)) + "</option>";
    }).join("");
    pricingSelectValues("ptModel", selectedModels);
    sel.disabled = !targetNames.length;
  }
  var pricingOpenVersion = 0;
  async function openPricingModal(template, prefillGroup, copyMode) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    var openVersion = ++pricingOpenVersion;
    try {
      await loadQuotaEditorData();
    } catch (e) {}
    // A newer open (or the same slow load finishing late) must not overwrite the latest form.
    if (openVersion !== pricingOpenVersion) return;
    state.editingPricingTemplate = copyMode ? null : template || null;
    state.copyingPricingTemplate = copyMode ? template : null;
    document.getElementById("modelCostModalTitle").textContent = copyMode
      ? "复制模型计费到现存目标"
      : template
        ? "编辑模型计费"
        : "新增模型计费";
    var targets = copyMode ? [] : pricingTargets(template || prefillGroup);
    var models = copyMode ? [] : pricingModels(template || prefillGroup).filter(Boolean);
    document.getElementById("ptTargetSearch").value = "";
    document.getElementById("ptModelSearch").value = "";
    document.getElementById("ptTarget").innerHTML = pricingTargetOptions(targets, !!copyMode);
    pricingSelectValues("ptTarget", targets);
    renderPricingModelSelect(targets, models, !!template && !copyMode);
    document.getElementById("ptTarget").disabled = !!template && !copyMode;
    document.getElementById("ptModel").disabled = (!!template && !copyMode) || !targets.length;
    var notice = document.getElementById("ptTargetNotice");
    notice.style.display = copyMode || template ? "block" : "none";
    notice.textContent = copyMode
      ? "旧模板保持原样；选择现存 Target 和模型后，可检查并保存复制的价格。"
      : template && pricingTargets(template).some(pricingTargetMissing)
        ? "此模板绑定的目标已删除。Target 和模型范围在编辑时固定；改价对全部绑定生效。更换范围请复制价格，旧模板保留。"
        : template
          ? "Target 和模型范围在编辑时固定；改价对全部绑定生效。更换范围请复制价格，旧模板保留。"
          : "";
    document.getElementById("ptCopyFromEditBtn").style.display =
      template && !copyMode ? "" : "none";
    setSchemaSelectValue(
      "ptMode",
      template ? template.mode : state.schema.defaults.pricing_mode,
    );
    var rates = (template && template.base_rates) || {};
    [["ptImageInput", "image_input_per_1m_tokens"], ["ptCachedImageInput", "cached_image_input_per_1m_tokens"], ["ptImageOutput", "image_output_per_1m_tokens"]].forEach(function (field) {
      document.getElementById(field[0]).value = rates[field[1]] != null ? rates[field[1]] : "";
    });
    document.getElementById("ptInput").value =
      rates.input_per_1m_tokens != null ? rates.input_per_1m_tokens : "";
    document.getElementById("ptOutput").value =
      rates.output_per_1m_tokens != null ? rates.output_per_1m_tokens : "";
    document.getElementById("ptCacheWrite").value =
      rates.cache_write_per_1m_tokens != null
        ? rates.cache_write_per_1m_tokens
        : "";
    document.getElementById("ptCacheRead").value =
      rates.cache_read_per_1m_tokens != null
        ? rates.cache_read_per_1m_tokens
        : "";
    document.getElementById("ptWrite1h").value =
      rates.cache_write_1h_per_1m_tokens != null
        ? rates.cache_write_1h_per_1m_tokens
        : "";
    document.getElementById("ptWrite5m").value =
      rates.cache_write_5m_per_1m_tokens != null
        ? rates.cache_write_5m_per_1m_tokens
        : "";
    document.getElementById("ptExplicitRead").value =
      rates.explicit_cache_read_per_1m_tokens != null
        ? rates.explicit_cache_read_per_1m_tokens
        : "";
    var is0 = pricingIsTemplate0(template);
    document.getElementById("ptStart").value =
      template && !is0 && template.start
        ? toLocalInputValue(template.start)
        : "";
    document.getElementById("ptEnd").value =
      template && template.end ? toLocalInputValue(template.end) : "";
    // Remember the stored instants: an unchanged field is submitted verbatim, so an unrelated
    // edit never truncates seconds or sub-second digits through the local input round-trip.
    state.ptTimeOrigin = {
      start: template && !is0 && template.start ? { local: document.getElementById("ptStart").value, iso: template.start } : null,
      end: template && template.end ? { local: document.getElementById("ptEnd").value, iso: template.end } : null,
    };
    document.getElementById("ptTimezone").value =
      (template && template.timezone) || state.schema.defaults.pricing_timezone;
    state.ptPeriods = template && Array.isArray(template.periods) ? clonePricingValue(template.periods) : [];
    state.ptPeriods.forEach(function (period) {
      period.original = { start: period.start, end: period.end };
      period.rates = period.rates || {};
      contextRateFields.forEach(function (field) { if (period.rates[field[0]] == null) period.rates[field[0]] = field[2] ? null : 0; });
    });
    state.ptConditions = template && Array.isArray(template.conditions) ? clonePricingValue(template.conditions) : [];
    state.ptDateExceptions = template && Array.isArray(template.date_exceptions) ? clonePricingValue(template.date_exceptions) : [];
    document.getElementById("ptAdvancedPricing").open = false;
    document.getElementById("ptVersion").value = String(template && template.version != null ? template.version : 0);
    document.getElementById("ptRuleID").textContent = copyMode ? "保存后生成新的 ID" : template && template.id ? template.id : "保存后生成";
    renderPricingContext();
    renderPricingPeriods();
    updatePricingModeUI();
    updatePricingScopeNotice();
    openModal("modelCostModal");
  }
  function pricingTargetChanged() {
    if (state.editingPricingTemplate) return;
    var selectedModels = pricingSelected("ptModel");
    if (!selectedModels.length && state.copyingPricingTemplate) selectedModels = pricingModels(state.copyingPricingTemplate);
    // Keep unsupported selections visible so the full combination is validated, never silently reduced.
    renderPricingModelSelect(pricingSelected("ptTarget"), selectedModels, true);
    updatePricingScopeNotice();
  }
  document.getElementById("ptTarget").addEventListener("change", pricingTargetChanged);
  document.getElementById("ptModel").addEventListener("change", updatePricingScopeNotice);
  ["ptTarget", "ptModel"].forEach(function (id) {
    document.getElementById(id + "Search").addEventListener("input", function () { filterPricingChoices(id); });
    document.getElementById(id + "Choices").addEventListener("change", function (event) {
      var input = event.target;
      var select = document.getElementById(id);
      if (!input || input.type !== "checkbox" || select.disabled) return;
      var hadFocus = document.activeElement === input;
      Array.from(select.options).forEach(function (option) {
        if (option.disabled) return;
        if (option.value === input.value) option.selected = input.checked;
        else if (id === "ptTarget" && input.checked && (!input.value || !option.value)) option.selected = false;
      });
      if (id === "ptTarget") pricingTargetChanged();
      else updatePricingScopeNotice();
      if (hadFocus) Array.from(document.getElementById(id + "Choices").querySelectorAll("input")).some(function (checkbox) {
        if (checkbox.value !== input.value) return false;
        checkbox.focus();
        return true;
      });
    });
  });
  document
    .getElementById("ptCopyFromEditBtn")
    .addEventListener("click", function () {
      var source = state.editingPricingTemplate;
      if (!source) return;
      // Only the template identity changes; keep the prices and schedule already entered in this form.
      state.editingPricingTemplate = null;
      state.copyingPricingTemplate = source;
      document.getElementById("modelCostModalTitle").textContent =
        "复制模型计费到现存目标";
      var targetSelect = document.getElementById("ptTarget");
      targetSelect.innerHTML = pricingTargetOptions([], true);
      pricingSelectValues("ptTarget", []);
      targetSelect.disabled = false;
      document.getElementById("ptTargetSearch").value = "";
      document.getElementById("ptModelSearch").value = "";
      renderPricingModelSelect([], []);
      updatePricingScopeNotice();
      document.getElementById("ptTargetNotice").textContent =
        "当前填写的价格和生效设置已保留。请选择现存 Target 和模型；旧模板保持原样。";
      this.style.display = "none";
      document.getElementById("ptTargetSearch").focus();
    });
  document
    .getElementById("newModelCostBtn")
    .addEventListener("click", function () {
      openPricingModal(null, null);
    });
  document
    .getElementById("saveModelCostBtn")
    .addEventListener("click", function () {
      return runGuarded("saveModelCost", document.getElementById("saveModelCostBtn"), savePricingTemplate);
    });
  async function savePricingTemplate() {
      if (isReadonlyMode()) return;
      var targets = pricingSelected("ptTarget"), models = pricingSelected("ptModel");
      var mode = document.getElementById("ptMode").value;
      var conflicts = updatePricingScopeNotice();
      // Existing orphan templates may still be edited without changing their historical scope.
      if (!state.editingPricingTemplate && conflicts.length) { toast(conflicts.join("；"), "error"); return; }
      if (state.copyingPricingTemplate && targets.some(function (name) { return !name || !pricingTargetExists(name); })) {
        toast("复制时请选择现存 Target", "error"); return;
      }
      if (!targets.length || !models.length) { toast("请选择至少一个 Target 和一个模型", "error"); return; }
      if (mode !== "flat" && mode !== "schedule") {
        toast("无效的计费模式", "error");
        return;
      }
      var readRate = function (id, label) {
        var value = pricingInputNumber(document.getElementById(id));
        if (value === null) {
          toast("请填写" + label + "价格", "error");
          return null;
        }
        if (!Number.isFinite(value) || value < 0) {
          toast(label + "价格必须是非负数字", "error");
          return null;
        }
        return value;
      };
      var baseRates = {
        input_per_1m_tokens: readRate("ptInput", "输入"),
        output_per_1m_tokens: readRate("ptOutput", "输出"),
        cache_write_per_1m_tokens: readRate("ptCacheWrite", "缓存写入"),
        cache_read_per_1m_tokens: readRate("ptCacheRead", "缓存读取"),
      };
      if (
        Object.keys(baseRates).some(function (key) {
          return baseRates[key] === null;
        })
      )
        return;
      var optionalRates = [
        ["ptExplicitRead", "explicit_cache_read_per_1m_tokens"],
        ["ptWrite5m", "cache_write_5m_per_1m_tokens"],
        ["ptWrite1h", "cache_write_1h_per_1m_tokens"],
        ["ptImageInput", "image_input_per_1m_tokens"],
        ["ptCachedImageInput", "cached_image_input_per_1m_tokens"],
        ["ptImageOutput", "image_output_per_1m_tokens"],
      ];
      for (var optional of optionalRates) {
        // Blank means "not set" (null → fall back); text the browser cannot parse is NaN and rejected.
        var value = pricingInputNumber(document.getElementById(optional[0]));
        if (value !== null && (!Number.isFinite(value) || value < 0)) {
          toast("可选价格必须是非负数字", "error");
          return;
        }
        baseRates[optional[1]] = value;
      }

      var startInput = document.getElementById("ptStart").value;
      var endInput = document.getElementById("ptEnd").value;
      var isTemplate0 = mode === "flat" && !startInput;
      if (isTemplate0 && endInput) {
        toast("全天兜底模板不能填写结束时间", "error");
        return;
      }
      if (mode === "schedule" && !startInput) {
        toast("分时段模板必须填写生效开始时间", "error");
        return;
      }
      var payload = {
        targets: targets,
        models: models,
        mode: mode,
        timezone:
          document.getElementById("ptTimezone").value.trim() ||
          state.schema.defaults.pricing_timezone,
        base_rates: baseRates,
      };
      if (!isTemplate0) {
        var origin = state.ptTimeOrigin || {};
        var sd = new Date(startInput);
        if (isNaN(sd.getTime())) {
          toast("请填写有效的生效开始时间", "error");
          return;
        }
        payload.start = origin.start && origin.start.local === startInput ? origin.start.iso : sd.toISOString();
        if (endInput) {
          var ed = new Date(endInput);
          if (isNaN(ed.getTime())) {
            toast("请填写有效的生效结束时间", "error");
            return;
          }
          payload.end = origin.end && origin.end.local === endInput ? origin.end.iso : ed.toISOString();
        }
      }
      try {
        document.getElementById("modelCostModal").querySelectorAll("[data-pt-condition-field], [data-pt-context-rate], [data-pt-date-field]").forEach(syncPricingContextInput);
        var context = validatePricingContext(mode);
        payload.version = context.version;
        payload.conditions = context.conditions;
        payload.date_exceptions = context.date_exceptions;
      } catch (error) { toast(error.message, "error"); return; }
      if (mode === "schedule") {
        var periods = collectPricingPeriods();
        if (!periods.length && !payload.date_exceptions.length) {
          toast("分时段模式至少需要一个时段", "error");
          return;
        }
        for (var i = 0; i < periods.length; i++) {
          var p = periods[i];
          if (!p.weekdays || !p.weekdays.length) {
            toast("时段 " + (i + 1) + " 未选择星期", "error");
            return;
          }
          var timeError = pricingPeriodTimeError(p, "时段 " + (i + 1));
          if (timeError) {
            toast(timeError, "error");
            return;
          }
          if (
            [
              "explicit_cache_read_per_1m_tokens",
              "cache_write_5m_per_1m_tokens",
              "cache_write_1h_per_1m_tokens",
              "image_input_per_1m_tokens",
              "cached_image_input_per_1m_tokens",
              "image_output_per_1m_tokens",
            ].some(function (key) {
              var v = p.rates[key];
              return v != null && (!Number.isFinite(v) || v < 0);
            })
          ) {
            toast("可选价格必须是非负数字", "error");
            return;
          }
          if (
            [
              p.rates.input_per_1m_tokens,
              p.rates.output_per_1m_tokens,
              p.rates.cache_write_per_1m_tokens,
              p.rates.cache_read_per_1m_tokens,
            ].some(function (rate) {
              return rate === null || !Number.isFinite(rate) || rate < 0;
            })
          ) {
            toast("时段 " + (i + 1) + " 的四类价格必须填写非负数字", "error");
            return;
          }
        }
        payload.periods = periods.map(function (p) {
          return {
            weekdays: p.weekdays,
            start: pricingPeriodTimeValue(p, "start"),
            end: pricingPeriodTimeValue(p, "end"),
            rates: p.rates,
            conditions: p.conditions || [],
          };
        });
      }
      var editing = state.editingPricingTemplate;
      var conflict = pricingSubmissionConflict(payload, editing && editing.id);
      if (conflict) {
        var notice = document.getElementById("ptScopeNotice");
        notice.textContent = conflict;
        notice.classList.add("pricing-scope-error");
        toast(conflict, "error");
        return;
      }
      try {
        if (editing && editing.id) {
          await api(
            "/admin/data/pricing-templates/" + encodeURIComponent(editing.id),
            { method: "PUT", body: payload },
          );
          toast("模型计费已更新", "success");
        } else {
          await api("/admin/data/pricing-templates", {
            method: "POST",
            body: payload,
          });
          toast("模型计费已创建", "success");
        }
        closeModal("modelCostModal");
        await loadPricingTemplates();
      } catch (e) {
        toast("保存失败: " + e.message, "error");
      }
  }
  return {
    filterPricingChoices: filterPricingChoices,
    renderPricingChoices: renderPricingChoices,
    renderPricingSelectionPreview: renderPricingSelectionPreview,
    pricingScopeConflicts: pricingScopeConflicts,
    updatePricingScopeNotice: updatePricingScopeNotice,
    pricingSubmissionConflict: pricingSubmissionConflict,
    renderPricingModelSelect: renderPricingModelSelect,
    openPricingModal: openPricingModal,
    pricingTargetChanged: pricingTargetChanged,
  };
});
