"use strict";
// Pricing owns model price templates, periods and template copy workflows.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("pricing", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtTime = app.bind("core", "fmtTime");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var setSchemaSelectValue = app.bind("schema", "setSchemaSelectValue");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var quotaModelLabel = app.bind("quota", "quotaModelLabel");
  var loadQuotaEditorData = app.bind("quota", "loadQuotaEditorData");
  /* ===== Pricing Templates (Model Costs) ===== */
  var PT_WEEKDAYS = ["周一", "周二", "周三", "周四", "周五", "周六", "周日"];
  function pricingTargets(t) {
    return t && Array.isArray(t.targets) && t.targets.length ? t.targets.slice() : [t && t.target || ""];
  }
  function pricingModels(t) {
    return t && Array.isArray(t.models) && t.models.length ? t.models.slice() : [t && t.model || ""];
  }
  function pricingArray(value) { return Array.isArray(value) ? value : [value || ""]; }
  function pricingScopePart(value) {
    return pricingArray(value).map(function (v) { return String(v).toLowerCase(); }).sort();
  }
  function pricingGroupKey(target, model) {
    return JSON.stringify([pricingScopePart(target), pricingScopePart(model)]);
  }
  function pricingAttrKey(target, model) {
    return encodeURIComponent(JSON.stringify([pricingArray(target), pricingArray(model)]));
  }
  function pricingDetailId(target, model) {
    var value = pricingAttrKey(target, model);
    var hash = 0;
    for (var i = 0; i < value.length; i++) hash = ((hash << 5) - hash + value.charCodeAt(i)) | 0;
    return "pricing-detail-" + Math.abs(hash) + "-" + value.replace(/[^a-zA-Z0-9_-]/g, "_").slice(0, 48);
  }
  function pricingParseAttrKey(attr) {
    try {
      var scope = JSON.parse(decodeURIComponent(attr));
      if (!Array.isArray(scope) || scope.length !== 2 || !scope.every(Array.isArray)) return null;
      return {targets: scope[0], models: scope[1], target: scope[0][0] || "", model: scope[1][0] || ""};
    } catch (e) { return null; }
  }
  function pricingSelected(id) {
    return Array.from(document.getElementById(id).options).filter(function (option) {
      return option.selected && !option.disabled;
    }).map(function (option) { return option.value; });
  }
  function pricingSelectValues(id, values) {
    var normalized = pricingArray(values).map(function (value) { return String(value).toLowerCase(); });
    Array.from(document.getElementById(id).options).forEach(function (option) {
      option.selected = !option.disabled && normalized.indexOf(String(option.value).toLowerCase()) >= 0;
    });
  }
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
  function pricingStartIsZero(start) {
    if (!start) return true;
    var d = new Date(start);
    if (isNaN(d.getTime())) return true;
    return d.getUTCFullYear() <= 1;
  }
  function pricingIsTemplate0(t) {
    return !!t && t.mode === "flat" && !t.end && pricingStartIsZero(t.start);
  }
  function pricingRate(v) {
    var n = Number(v || 0);
    if (Number.isInteger(n)) return "$" + n;
    return "$" + n.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
  }
  function pricingWeekdays(wds) {
    if (!wds || !wds.length)
      return "<span style='color:var(--text-muted);'>未选</span>";
    return wds
      .slice()
      .sort(function (a, b) {
        return a - b;
      })
      .map(function (w) {
        return PT_WEEKDAYS[w - 1] || "周" + w;
      })
      .join("、");
  }
  function pricingRatesHtml(r) {
    r = r || {};
    return (
      "输入 " +
      pricingRate(r.input_per_1m_tokens) +
      " · 输出 " +
      pricingRate(r.output_per_1m_tokens) +
      " · 缓存写 " +
      pricingRate(r.cache_write_per_1m_tokens) +
      " · 缓存读 " +
      pricingRate(r.cache_read_per_1m_tokens)
    );
  }
  function pricingRateGridHtml(r) {
    r = r || {};
    var rates = [
      ["输入", r.input_per_1m_tokens],
      ["输出", r.output_per_1m_tokens],
      ["缓存写入", r.cache_write_per_1m_tokens],
      [
        "显式缓存读取",
        r.explicit_cache_read_per_1m_tokens,
        r.cache_read_per_1m_tokens,
      ],
      [
        "5 分钟缓存写入",
        r.cache_write_5m_per_1m_tokens,
        r.cache_write_per_1m_tokens,
      ],
      [
        "1 小时缓存写入",
        r.cache_write_1h_per_1m_tokens,
        r.cache_write_per_1m_tokens,
      ],
      ["缓存读取", r.cache_read_per_1m_tokens],
      ["图像输入", r.image_input_per_1m_tokens, null],
      ["缓存图像输入", r.cached_image_input_per_1m_tokens, r.image_input_per_1m_tokens],
      ["图像输出", r.image_output_per_1m_tokens, null],
    ];
    return (
      "<div class='pricing-rate-grid'>" +
      rates
        .map(function (entry) {
          var value =
            entry.length > 2 && entry[1] == null
              ? (entry[2] == null ? "未配置" : "继承 " + pricingRate(entry[2]))
              : pricingRate(entry[1]);
          return (
            "<div class='pricing-rate-item'><span class='pricing-rate-label'>" +
            entry[0] +
            " / 1M</span><span class='pricing-rate-value'>" +
            value +
            "</span></div>"
          );
        })
        .join("") +
      "</div>"
    );
  }
  function pricingRangeHtml(t) {
    if (pricingIsTemplate0(t)) return "全天兜底";
    return (
      (t.start ? fmtTime(t.start) : "—") +
      " ~ " +
      (t.end ? fmtTime(t.end) : "至今")
    );
  }
  function toLocalInputValue(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    var pad = function (n) {
      return (n < 10 ? "0" : "") + n;
    };
    return (
      d.getFullYear() +
      "-" +
      pad(d.getMonth() + 1) +
      "-" +
      pad(d.getDate()) +
      "T" +
      pad(d.getHours()) +
      ":" +
      pad(d.getMinutes())
    );
  }
  function pricingTargetExists(name) {
    return (state.targets || []).some(function (target) {
      return (
        String(target.name || "").toLowerCase() ===
        String(name || "").toLowerCase()
      );
    });
  }
  function pricingTargetMissing(name) {
    return (
      !!name && (state.targets || []).length > 0 && !pricingTargetExists(name)
    );
  }
  function pricingTargetOptions(selected, requireExisting) {
    var selectedNames = pricingArray(selected);
    function isSelected(value) { return selectedNames.some(function (name) { return String(name).toLowerCase() === String(value).toLowerCase(); }); }
    var html = requireExisting ? "<option value='' disabled>选择现存 Target...</option>"
      : "<option value=''" + (isSelected("") ? " selected" : "") + ">默认（全局兜底）</option>";
    (state.targets || []).slice().sort(function (a, b) { return String(a.name).localeCompare(String(b.name)); }).forEach(function (target) {
      var meta = epDisplayName(target.endpoint_type || "") || target.endpoint_type || "";
      html += "<option value='" + esc(target.name) + "'" + (isSelected(target.name) ? " selected" : "") + ">" + esc(target.name + " · " + meta) + "</option>";
    });
    if (!requireExisting) selectedNames.forEach(function (name) {
      if (name && !pricingTargetExists(name)) html += "<option value='" + esc(name) + "' selected>" + esc(name) + "（目标已删除，模板不再生效）</option>";
    });
    return html;
  }
  function pricingModelLabel(target, model) {
    if (!target) return model;
    return quotaModelLabel(target, model);
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
  function clonePricingValue(value) { return JSON.parse(JSON.stringify(value)); }
  var contextRateFields = [
    ["input_per_1m_tokens", "输入", false], ["output_per_1m_tokens", "输出", false],
    ["cache_write_per_1m_tokens", "缓存写入", false], ["cache_read_per_1m_tokens", "缓存读取", false],
    ["explicit_cache_read_per_1m_tokens", "显式缓存读取", true],
    ["cache_write_5m_per_1m_tokens", "5 分钟缓存写入", true], ["cache_write_1h_per_1m_tokens", "1 小时缓存写入", true],
    ["image_input_per_1m_tokens", "图像输入", true], ["cached_image_input_per_1m_tokens", "缓存图像输入", true],
    ["image_output_per_1m_tokens", "图像输出", true],
  ];
  function contextRatesForm(rates, attributes, prefix) {
    return "<div class='pricing-rate-editor'>" + contextRateFields.map(function (field) {
      var id = prefix + "-" + field[0];
      return "<div class='form-group'><label for='" + esc(id) + "'>" + field[1] + (field[2] ? "（可选）" : "") + "</label>" +
        "<input id='" + esc(id) + "' class='form-control' type='number' min='0' step='any' " + attributes + " data-pt-context-rate='" + field[0] + "' value='" + esc(rates[field[0]] != null ? rates[field[0]] : "") + "'" +
        (field[2] ? " placeholder='留空继承本规则同类价格；0 为免费'" : " required") + "></div>";
    }).join("") + "</div>";
  }
  function conditionList(scope) {
    if (scope === "base") return state.ptConditions || [];
    var parts = scope.split(":"), item = parts[0] === "period" ? (state.ptPeriods || [])[Number(parts[1])] : (state.ptDateExceptions || [])[Number(parts[1])];
    if (!item) return [];
    item.conditions = item.conditions || [];
    return item.conditions;
  }
  function conditionOptions(key, selected) {
    var values = state.schema[key] || [];
    var html = "<option value=''>不限</option>";
    values.forEach(function (option) { html += "<option value='" + esc(option.value) + "'" + (option.value === selected ? " selected" : "") + ">" + esc(option.label) + "</option>"; });
    if (selected && !values.some(function (option) { return option.value === selected; })) html += "<option selected value='" + esc(selected) + "'>未识别配置：" + esc(selected) + "</option>";
    return html;
  }
  function conditionsForm(scope) {
    var conditions = conditionList(scope);
    return "<details class='pricing-context-section'" + (scope === "base" || conditions.length ? " open" : "") + "><summary>条件价格 · " + conditions.length + " 条</summary>" +
      "<p class='pricing-help'>所有条件同时满足才使用这一整套价格，不叠加倍率。输入上下限可留空；服务档位和速度来自上游实际响应。</p>" +
      conditions.map(function (c, index) {
        var attrs = "data-pt-context-scope='" + esc(scope) + "' data-pt-condition-index='" + index + "'";
        var prefix = "pt-rule-" + scope.replace(":", "-") + "-" + index;
        function input(field, label, type, value, hint) {
          var id = prefix + "-" + field;
          return "<div class='form-group'><label for='" + id + "'>" + label + "</label><input id='" + id + "' class='form-control' type='" + type + "' " + attrs + " data-pt-condition-field='" + field + "' value='" + esc(value != null ? value : "") + "'" + (type === "number" ? " min='0' step='1'" : "") + ">" + (hint ? "<small class='pricing-help'>" + hint + "</small>" : "") + "</div>";
        }
        return "<div class='pricing-context-row'><div class='pricing-context-heading'><strong>条件 " + (index + 1) + "</strong><button type='button' class='btn btn-sm btn-danger readonly-hide' data-pt-delete-condition='" + index + "' data-pt-context-scope='" + esc(scope) + "'>删除条件</button></div><div class='pricing-form-grid'>" +
          input("id", "规则 ID", "text", c.id, "用于识别计价规则") + input("min_input_tokens", "总输入下限（包含）", "number", c.min_input_tokens, "包含普通、缓存读取与写入；不重复计数") + input("max_input_tokens", "总输入上限（不包含）", "number", c.max_input_tokens, "例如大于 272000：下限填 272001") +
          "<div class='form-group'><label for='" + prefix + "-tier'>实际服务档位</label><select id='" + prefix + "-tier' class='form-control' " + attrs + " data-pt-condition-field='service_tier'>" + conditionOptions("pricing_service_tiers", c.service_tier) + "</select></div>" +
          "<div class='form-group'><label for='" + prefix + "-speed'>实际执行速度</label><select id='" + prefix + "-speed' class='form-control' " + attrs + " data-pt-condition-field='speed'>" + conditionOptions("pricing_speeds", c.speed) + "</select></div></div>" +
          "<details class='pricing-context-rates'><summary>独立价格（每 1M tokens）</summary>" + contextRatesForm(c.rates || {}, attrs, prefix) + "</details></div>";
      }).join("") + "<button type='button' class='btn btn-sm readonly-hide' data-pt-add-condition='" + esc(scope) + "'>添加条件价格</button></details>";
  }
  function renderPricingContext() {
    var container = document.getElementById("ptContextRules");
    container.innerHTML = conditionsForm("base");
    var exceptions = state.ptDateExceptions || [];
    document.getElementById("ptDateExceptions").innerHTML = exceptions.map(function (e, index) {
      var attrs = "data-pt-date-index='" + index + "'", prefix = "pt-date-" + index;
      return "<div class='pricing-context-row'><div class='pricing-context-heading'><strong>日期例外 " + (index + 1) + "</strong><button type='button' class='btn btn-sm btn-danger readonly-hide' data-pt-delete-date='" + index + "'>删除日期例外</button></div>" +
        "<div class='pricing-form-grid'><div class='form-group'><label for='" + prefix + "-start'>开始日期（包含）</label><input id='" + prefix + "-start' type='date' class='form-control' " + attrs + " data-pt-date-field='start_date' value='" + esc(e.start_date || "") + "'></div>" +
        "<div class='form-group'><label for='" + prefix + "-end'>结束日期（不包含）</label><input id='" + prefix + "-end' type='date' class='form-control' " + attrs + " data-pt-date-field='end_date' value='" + esc(e.end_date || "") + "'></div></div>" +
        "<details class='pricing-context-rates'><summary>日期例外价格（每 1M tokens）</summary>" + contextRatesForm(e.rates || {}, attrs, prefix) + "</details>" + conditionsForm("date:" + index) + "</div>";
    }).join("") || "<p class='pricing-help'>尚无日期例外。节假日需按官方日历明确填写，不会自动添加或沿用到下一年。</p>";
  }
  function syncPricingContextInput(input) {
    var data = input.dataset || {}, raw = String(input.value == null ? "" : input.value).trim(), item;
    if (data.ptConditionIndex !== undefined) item = conditionList(data.ptContextScope)[Number(data.ptConditionIndex)];
    else if (data.ptDateIndex !== undefined) item = (state.ptDateExceptions || [])[Number(data.ptDateIndex)];
    if (!item) return;
    if (data.ptContextRate) { item.rates = item.rates || {}; item.rates[data.ptContextRate] = raw === "" ? null : Number(raw); }
    else if (data.ptConditionField) {
      var field = data.ptConditionField;
      if (field === "min_input_tokens" || field === "max_input_tokens") { if (raw === "") delete item[field]; else item[field] = Number(raw); }
      else item[field] = raw;
    } else if (data.ptDateField) item[data.ptDateField] = raw;
  }
  function defaultContextRates() { return {input_per_1m_tokens: 0, output_per_1m_tokens: 0, cache_write_per_1m_tokens: 0, cache_read_per_1m_tokens: 0}; }
  function pricingContextClick(event) {
    if (isReadonlyMode()) return;
    var button = event.target.closest("button"); if (!button) return;
    var data = button.dataset || {};
    if (data.ptAddCondition !== undefined) {
      var conditions = conditionList(data.ptAddCondition), n = conditions.length + 1;
      while (conditions.some(function (c) { return c.id === "rule-" + n; })) n++;
      conditions.push({id: "rule-" + n, rates: defaultContextRates()});
    } else if (data.ptDeleteCondition !== undefined) conditionList(data.ptContextScope).splice(Number(data.ptDeleteCondition), 1);
    else if (data.ptDeleteDate !== undefined) state.ptDateExceptions.splice(Number(data.ptDeleteDate), 1);
    else return;
    renderPricingContext(); renderPricingPeriods();
  }
  function validatePricingContext(mode) {
    var version = Number(document.getElementById("ptVersion").value);
    if (!Number.isSafeInteger(version) || version < 0) throw new Error("规则版本必须是非负整数");
    function ratesValid(rates) {
      contextRateFields.forEach(function (field) {
        var value = (rates || {})[field[0]];
        if ((!field[2] && value == null) || (value != null && (!Number.isFinite(value) || value < 0))) throw new Error("独立价格的输入、输出、缓存写入和缓存读取必须填写非负数字；可选价格可留空");
      });
    }
    function validateConditions(conditions) {
      var ids = new Set();
      (conditions || []).forEach(function (c, index) {
        if (!String(c.id || "").trim() || ids.has(c.id)) throw new Error("条件规则 ID 不能为空或重复"); ids.add(c.id);
        if (c.min_input_tokens == null && c.max_input_tokens == null && !c.service_tier && !c.speed) throw new Error("请为条件价格填写输入范围、实际服务档位或速度");
        ["min_input_tokens", "max_input_tokens"].forEach(function (key) { if (c[key] != null && (!Number.isSafeInteger(c[key]) || c[key] < 0)) throw new Error("总输入上下限必须是非负整数"); });
        if (c.max_input_tokens != null && (c.max_input_tokens === 0 || c.min_input_tokens != null && c.min_input_tokens >= c.max_input_tokens)) throw new Error("总输入上限必须大于下限（上限不包含）");
        [["service_tier", "pricing_service_tiers"], ["speed", "pricing_speeds"]].forEach(function (field) { if (c[field[0]] && !state.schema[field[1]].some(function (option) { return option.value === c[field[0]]; })) throw new Error("存在未识别的实际档位或速度，请重新选择"); });
        ratesValid(c.rates);
        (conditions || []).slice(0, index).forEach(function (other) {
          var separate = c.service_tier && other.service_tier && c.service_tier !== other.service_tier || c.speed && other.speed && c.speed !== other.speed || c.max_input_tokens != null && other.min_input_tokens != null && c.max_input_tokens <= other.min_input_tokens || other.max_input_tokens != null && c.min_input_tokens != null && other.max_input_tokens <= c.min_input_tokens;
          if (!separate) throw new Error("条件价格存在重叠；请调整输入范围或档位，确保只命中一条");
        });
      });
    }
    validateConditions(state.ptConditions);
    (state.ptPeriods || []).forEach(function (period) { validateConditions(period.conditions); });
    var exceptions = state.ptDateExceptions || [];
    if (exceptions.length && mode !== "schedule") throw new Error("日期例外仅适用于分时段模板；请切换模式或删除例外");
    exceptions.forEach(function (e, index) {
      function validDate(raw) { return /^\d{4}-\d{2}-\d{2}$/.test(raw || "") && !isNaN(new Date(raw + "T00:00:00Z").getTime()) && new Date(raw + "T00:00:00Z").toISOString().slice(0, 10) === raw; }
      if (!validDate(e.start_date) || !validDate(e.end_date) || e.start_date >= e.end_date) throw new Error("日期例外必须填写有效范围，结束日期晚于开始日期且不包含结束日");
      exceptions.slice(0, index).forEach(function (other) { if (e.start_date < other.end_date && other.start_date < e.end_date) throw new Error("日期例外不能重叠"); });
      ratesValid(e.rates); validateConditions(e.conditions);
    });
    return {version: version, conditions: clonePricingValue(state.ptConditions || []), date_exceptions: clonePricingValue(exceptions)};
  }
  function conditionsDetail(conditions) {
    if (!(conditions || []).length) return "";
    return "<ul class='pricing-condition-summary'>" + conditions.map(function (c) {
      var terms = [];
      if (c.min_input_tokens != null) terms.push("总输入 ≥ " + c.min_input_tokens);
      if (c.max_input_tokens != null) terms.push("总输入 < " + c.max_input_tokens);
      if (c.service_tier) terms.push(schemaLabel("pricing_service_tiers", c.service_tier));
      if (c.speed) terms.push(schemaLabel("pricing_speeds", c.speed));
      return "<li><strong>" + esc(c.id) + "</strong> · " + esc(terms.join(" · ")) + pricingRateGridHtml(c.rates) + "</li>";
    }).join("") + "</ul>";
  }

  function updatePricingModeUI() {
    var schedule = document.getElementById("ptMode").value === "schedule";
    document.getElementById("ptDateExceptionsGroup").style.display = schedule || (state.ptDateExceptions || []).length ? "" : "none";
    document.getElementById("ptPeriodsGroup").style.display = schedule
      ? ""
      : "none";
  }
  function renderPricingPeriods() {
    var container = document.getElementById("ptPeriods");
    var periods = state.ptPeriods || [];
    if (!periods.length) {
      container.innerHTML =
        "<div style='padding:8px 0;color:var(--text-muted);font-size:12px;'>暂无时段，点击“添加时段”创建</div>";
      return;
    }
    container.innerHTML = periods
      .map(function (p, index) {
        var wd = "";
        for (var i = 1; i <= 7; i++) {
          var checked = (p.weekdays || []).indexOf(i) >= 0 ? " checked" : "";
          wd +=
            "<label style='margin-right:6px;font-size:12px;white-space:nowrap;'><input type='checkbox' data-pt-wd='" +
            i +
            "'" +
            checked +
            "> " +
            PT_WEEKDAYS[i - 1] +
            "</label>";
        }
        var rate = p.rates || {};
        return (
          "<div class='quota-group-card' data-pt-period='" +
          index +
          "'>" +
          "<div style='display:flex;justify-content:space-between;align-items:center;margin-bottom:6px;'>" +
          "<strong>时段 " +
          (index + 1) +
          "</strong>" +
          "<button type='button' class='btn btn-sm btn-danger readonly-hide' data-pt-del>删除</button></div>" +
          "<div class='pricing-weekday-list'>" +
          wd +
          "</div>" +
          "<div style='display:grid;grid-template-columns:1fr 1fr;gap:8px;'>" +
          "<div class='form-group'><label>开始</label><input type='time' class='form-control' data-pt-start value='" +
          esc(p.start || "") +
          "'></div>" +
          "<div class='form-group'><label>结束</label><input type='time' class='form-control' data-pt-end value='" +
          esc(p.end || "") +
          "'></div>" +
          "</div>" +
          "<div style='display:grid;grid-template-columns:1fr 1fr;gap:8px;'>" +
          "<div class='form-group'><label>输入</label><input type='number' step='any' min='0' class='form-control' data-pt-in value='" +
          esc(
            rate.input_per_1m_tokens != null ? rate.input_per_1m_tokens : "",
          ) +
          "'></div>" +
          "<div class='form-group'><label>输出</label><input type='number' step='any' min='0' class='form-control' data-pt-out value='" +
          esc(
            rate.output_per_1m_tokens != null ? rate.output_per_1m_tokens : "",
          ) +
          "'></div>" +
          "<div class='form-group'><label>缓存写</label><input type='number' step='any' min='0' class='form-control' data-pt-cw value='" +
          esc(
            rate.cache_write_per_1m_tokens != null
              ? rate.cache_write_per_1m_tokens
              : "",
          ) +
          "'></div>" +
          "<div class='form-group'><label>缓存读</label><input type='number' step='any' min='0' class='form-control' data-pt-cr value='" +
          esc(
            rate.cache_read_per_1m_tokens != null
              ? rate.cache_read_per_1m_tokens
              : "",
          ) +
          "'></div>" +
          "<div class='form-group'><label>显式缓存读取（可选）</label><input type='number' step='any' min='0' class='form-control' data-pt-ExplicitRead placeholder='留空继承' value='" +
          esc(
            rate.explicit_cache_read_per_1m_tokens != null
              ? rate.explicit_cache_read_per_1m_tokens
              : "",
          ) +
          "'></div>" +
          "<div class='form-group'><label>5 分钟缓存写入（可选）</label><input type='number' step='any' min='0' class='form-control' data-pt-Write5m placeholder='留空继承' value='" +
          esc(
            rate.cache_write_5m_per_1m_tokens != null
              ? rate.cache_write_5m_per_1m_tokens
              : "",
          ) +
          "'></div>" +
          "<div class='form-group'><label>1 小时缓存写入（可选）</label><input type='number' step='any' min='0' class='form-control' data-pt-Write1h placeholder='留空继承' value='" +
          esc(
            rate.cache_write_1h_per_1m_tokens != null
              ? rate.cache_write_1h_per_1m_tokens
              : "",
          ) +
          "'></div>" +
          imagePeriodInputs(rate) +
          "</div>" + conditionsForm("period:" + index) + "</div>"
        );
      })
      .join("");
  }
  function syncPricingPeriod(idx, card) {
    var p = (state.ptPeriods || [])[idx];
    if (!p) return;
    var wds = [];
    card.querySelectorAll("[data-pt-wd]:checked").forEach(function (cb) {
      wds.push(Number(cb.dataset.ptWd));
    });
    p.weekdays = wds;
    p.start = (card.querySelector("[data-pt-start]") || {}).value || "";
    p.end = (card.querySelector("[data-pt-end]") || {}).value || "";
    var rateValue = function (selector) {
      var input = card.querySelector(selector);
      var raw = input ? input.value.trim() : "";
      return raw === "" ? null : Number(raw);
    };
    p.rates = {
      input_per_1m_tokens: rateValue("[data-pt-in]"),
      output_per_1m_tokens: rateValue("[data-pt-out]"),
      cache_write_per_1m_tokens: rateValue("[data-pt-cw]"),
      explicit_cache_read_per_1m_tokens: rateValue("[data-pt-ExplicitRead]"),
      cache_write_5m_per_1m_tokens: rateValue("[data-pt-Write5m]"),
      cache_write_1h_per_1m_tokens: rateValue("[data-pt-Write1h]"),
      cache_read_per_1m_tokens: rateValue("[data-pt-cr]"),
      image_input_per_1m_tokens: rateValue("[data-pt-ImageInput]"),
      cached_image_input_per_1m_tokens: rateValue("[data-pt-CachedImageInput]"),
      image_output_per_1m_tokens: rateValue("[data-pt-ImageOutput]"),
    };
  }
  function collectPricingPeriods() {
    var result = [];
    document
      .querySelectorAll("#ptPeriods [data-pt-period]")
      .forEach(function (card) {
        var idx = Number(card.dataset.ptPeriod);
        syncPricingPeriod(idx, card);
        result.push((state.ptPeriods || [])[idx]);
      });
    state.ptPeriods = result;
    return result;
  }
  function imagePeriodInputs(rate) {
    return [
      ["ImageInput", "image_input_per_1m_tokens", "图像输入", "留空表示缺价"],
      ["CachedImageInput", "cached_image_input_per_1m_tokens", "缓存图像输入", "留空继承图像输入价"],
      ["ImageOutput", "image_output_per_1m_tokens", "图像输出", "留空表示缺价"],
    ].map(function (field) {
      return "<div class='form-group'><label>" + field[2] + "（可选）</label><input type='number' step='any' min='0' class='form-control' aria-label='" + field[2] + "' data-pt-" + field[0] + " placeholder='" + field[3] + "' value='" + esc(rate[field[1]] != null ? rate[field[1]] : "") + "'></div>";
    }).join("");
  }
  function renderPricingDetail(list) {
    var blocks = list
      .slice()
      .sort(function (a, b) {
        var a0 = pricingIsTemplate0(a) ? 0 : 1;
        var b0 = pricingIsTemplate0(b) ? 0 : 1;
        if (a0 !== b0) return a0 - b0;
        var as = a.start ? new Date(a.start).getTime() : 0;
        var bs = b.start ? new Date(b.start).getTime() : 0;
        return as - bs;
      })
      .map(function (t) {
        var is0 = pricingIsTemplate0(t);
        var modeBadge = t.mode === "schedule" ? "warn" : "ok";
        var modeLabel = schemaLabel("pricing_modes", t.mode);
        var rows =
          "<div class='pricing-template-header'>" +
          "<div><div class='pricing-template-title'><span class='quota-badge " +
          modeBadge +
          "'>" +
          esc(modeLabel) +
          "</span>" +
          "<span>" +
          (is0 ? "全天兜底" : pricingRangeHtml(t)) +
          "</span></div>" +
          "<div class='pricing-template-subtitle'>UTC 偏移 " +
          esc(t.timezone || state.schema.defaults.pricing_timezone) +
          (is0 ? " · 作为该 Target / 模型的默认回退" : "") +
          "</div></div>" +
          "<div class='btn-group pricing-template-actions'>" +
          "<button type='button' class='btn btn-sm readonly-hide' data-edit-pricing='" +
          esc(t.id) +
          "'>编辑</button>" +
          (pricingTargets(t).some(pricingTargetMissing)
            ? "<button type='button' class='btn btn-sm btn-primary readonly-hide' data-copy-pricing='" +
              esc(t.id) +
              "'>复制到现存目标</button>"
            : "") +
          "<button type='button' class='btn btn-sm btn-danger readonly-hide' data-del-pricing='" +
          esc(t.id) +
          "'>删除</button>" +
          "</div></div>";
        rows +=
          "<div class='pricing-section-label'>基础价格与缓存细分（每 1M tokens）</div>";
        rows += "<p class='pricing-help'>规则 ID " + esc(t.id || "未保存") + " · 版本 " + esc(t.version || 0) + " · " + (t.date_exceptions || []).length + " 个日期例外</p>";
        rows += pricingRateGridHtml(t.base_rates) + conditionsDetail(t.conditions);
        (t.date_exceptions || []).forEach(function (exception) {
          rows += "<div class='pricing-section-label'>日期例外：" + esc(exception.start_date) + " 至 " + esc(exception.end_date) + "（不含结束日；沿模板时区）</div>" + pricingRateGridHtml(exception.rates) + conditionsDetail(exception.conditions);
        });
        if (t.mode === "schedule" && t.periods && t.periods.length) {
          rows +=
            "<div class='pricing-section-label'>分时段价格</div><div class='pricing-period-list'>";
          t.periods.forEach(function (p) {
            rows +=
              "<div class='pricing-period-item'><div class='pricing-period-top'><span class='pricing-period-days'>" +
              pricingWeekdays(p.weekdays) +
              "</span><span class='pricing-period-time'>" +
              esc(p.start || "") +
              " ~ " +
              esc(p.end || "") +
              "</span></div>" +
              pricingRateGridHtml(p.rates) + conditionsDetail(p.conditions) +
              "</div>";
          });
          rows += "</div>";
        } else if (t.mode === "schedule") {
          rows +=
            "<div class='pricing-section-label'>分时段价格</div><div class='pricing-empty-periods'>未配置分时段规则，将使用基础价格。</div>";
        }
        return "<div class='pricing-template-card'>" + rows + "</div>";
      });
    return (
      "<div class='pricing-detail'><div class='pricing-detail-list'>" +
      blocks.join("") +
      "</div></div>"
    );
  }
  function renderPricingTemplates() {
    var tb = document.getElementById("modelCostsTableBody");
    var templates = state.pricingTemplates || [];
    if (!templates.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='6'>暂无模型计费</td></tr>";
      return;
    }
    var groups = Object.create(null);
    templates.forEach(function (t) {
      var targets = pricingTargets(t), models = pricingModels(t);
      var key = pricingGroupKey(targets, models);
      if (!groups[key]) groups[key] = {targets: targets, models: models, list: []};
      groups[key].list.push(t);
    });
    var keys = Object.keys(groups).sort();
    var html = "";
    keys.forEach(function (key) {
      var group = groups[key], targets = group.targets, models = group.models;
      var list = group.list;
      var hasSchedule = list.some(function (t) {
        return t.mode === "schedule";
      });
      var expanded = !!(state.pricingExpanded || {})[key];
      var attr = pricingAttrKey(targets, models);
      var detailId = pricingDetailId(targets, models);
      html +=
        "<tr data-pricing-summary>" +
        "<td><button class='quota-expand-btn" +
        (expanded ? " open" : "") +
        "' data-expand-pricing='" +
        esc(attr) +
        "' aria-label='展开/收起明细' aria-controls='" +
        esc(detailId) +
        "' aria-expanded='" +
        (expanded ? "true" : "false") +
        "' title='展开/收起明细'>" +
        (expanded ? "▼" : "▶") +
        "</button></td>" +
        "<td class='pricing-target-cell'>" +
        "<span class='pricing-target-name'>" + esc(targets.map(function (name) { return name || "默认"; }).join("、")) +
        "</span><span class='pricing-target-meta'>" + esc(targets.length === 1 ? (targets[0] ? pricingTargetMeta(targets[0]) : "全局兜底") : targets.length + " 个 Target 共用价格") + "</span></td>" +
        "<td class='pricing-model-cell'><span class='pricing-model-name'>" + esc(models.join("、")) +
        "</span><span class='pricing-model-meta'>" + models.length + " 个模型 · 共用价格</span></td>" +
        "<td><span class='pricing-count'>" +
        list.length +
        "</span></td>" +
        "<td><div class='pricing-mode-summary'>" +
        (hasSchedule
          ? "<span class='quota-badge warn'>分时段</span>"
          : "<span class='quota-badge none'>全天</span>") +
        "<span class='pricing-mode-note'>" +
        (hasSchedule ? "含时段规则" : "固定价格") + " · " + list.reduce(function (count, t) { return count + (t.conditions || []).length + (t.periods || []).reduce(function (n, p) { return n + (p.conditions || []).length; }, 0) + (t.date_exceptions || []).reduce(function (n, e) { return n + (e.conditions || []).length; }, 0); }, 0) + " 条条件价" +
        "</span></div></td>" +
        "<td><div class='btn-group'><button type='button' class='btn btn-sm readonly-hide' " +
        (targets.some(pricingTargetMissing)
          ? "data-expand-pricing='" + esc(attr) + "'>展开复制"
          : "data-add-pricing='" + esc(attr) + "'>新增") +
        "</button></div></td></tr>";
      if (expanded) {
        html +=
          "<tr class='quota-detail-row pricing-detail-row' id='" +
          esc(detailId) +
          "' data-pricing-detail='" +
          esc(attr) +
          "'><td colspan='6'>" +
          renderPricingDetail(list) +
          "</td></tr>";
      }
    });
    tb.innerHTML = html;
  }
  // pricingTargetMeta 返回模型计费列表 Target 列的说明小字：
  // 匹配到现存 target 时显示「类型 · 端点」，匹配不到时提示已删除。
  function pricingTargetMeta(name) {
    var list = state.targets || [];
    if (!list.length) return "目标信息未加载";
    for (var i = 0; i < list.length; i++) {
      if (
        String(list[i].name || "").toLowerCase() === String(name).toLowerCase()
      ) {
        var type = epDisplayName(
          list[i].endpoint_type || state.schema.defaults.endpoint_type,
        );
        var ep = String(list[i].endpoint || "");
        return type + (ep ? " · " + ep : "");
      }
    }
    return "目标不存在或已删除 · 旧模板不再生效";
  }
  async function loadPricingTemplates() {
    try {
      if (!state.targets || !state.targets.length) {
        try {
          await loadQuotaEditorData();
        } catch (e) {
          /* target 元信息缺失不阻塞模板渲染 */
        }
      }
      var data = await api("/admin/data/pricing-templates");
      state.pricingTemplates = Array.isArray(data.templates)
        ? data.templates
        : [];
      renderPricingTemplates();
    } catch (e) {
      toast("加载模型计费失败: " + e.message, "error");
    }
  }
  async function openPricingModal(template, prefillGroup, copyMode) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    try {
      await loadQuotaEditorData();
    } catch (e) {}
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
    document.getElementById("ptTimezone").value =
      (template && template.timezone) || state.schema.defaults.pricing_timezone;
    state.ptPeriods = template && Array.isArray(template.periods) ? clonePricingValue(template.periods) : [];
    state.ptPeriods.forEach(function (period) {
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
  function deletePricingTemplate(id) {
    var t = (state.pricingTemplates || []).filter(function (x) {
      return x.id === id;
    })[0];
    var label = t ? pricingTargets(t).map(function (name) { return name || "默认"; }).join("、") + " / " + pricingModels(t).join("、") : id;
    showConfirm("确认删除模型计费「" + label + "」？该规则的全部绑定将一同删除。", async function () {
      try {
        await api("/admin/data/pricing-templates/" + encodeURIComponent(id), {
          method: "DELETE",
        });
        toast("模型计费已删除", "success");
        await loadPricingTemplates();
      } catch (e) {
        toast("删除失败: " + e.message, "error");
      }
    });
  }
  document
    .getElementById("modelCostsTableBody")
    .addEventListener("click", function (e) {
      var expandBtn = e.target.closest("[data-expand-pricing]");
      if (expandBtn) {
        var parsed = pricingParseAttrKey(expandBtn.dataset.expandPricing);
        if (parsed) {
          var gk = pricingGroupKey(parsed.targets, parsed.models);
          state.pricingExpanded = state.pricingExpanded || {};
          state.pricingExpanded[gk] = !state.pricingExpanded[gk];
          renderPricingTemplates();
        }
        return;
      }
      var addBtn = e.target.closest("[data-add-pricing]");
      if (addBtn) {
        openPricingModal(null, pricingParseAttrKey(addBtn.dataset.addPricing));
        return;
      }
      var copyBtn = e.target.closest("[data-copy-pricing]");
      if (copyBtn) {
        var source = (state.pricingTemplates || []).filter(function (x) {
          return x.id === copyBtn.dataset.copyPricing;
        })[0];
        if (source) openPricingModal(source, null, true);
        return;
      }
      var editBtn = e.target.closest("[data-edit-pricing]");
      if (editBtn) {
        var t = (state.pricingTemplates || []).filter(function (x) {
          return x.id === editBtn.dataset.editPricing;
        })[0];
        if (t) openPricingModal(t, null);
        return;
      }
      var delBtn = e.target.closest("[data-del-pricing]");
      if (delBtn) {
        deletePricingTemplate(delBtn.dataset.delPricing);
        return;
      }
    });
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
    .getElementById("ptMode")
    .addEventListener("change", updatePricingModeUI);
  document.getElementById("ptPeriods").addEventListener("change", function (e) {
    var card = e.target.closest("[data-pt-period]");
    if (card) syncPricingPeriod(Number(card.dataset.ptPeriod), card);
  });
  document.getElementById("ptPeriods").addEventListener("input", function (e) {
    var card = e.target.closest("[data-pt-period]");
    if (card) syncPricingPeriod(Number(card.dataset.ptPeriod), card);
  });
  document.getElementById("ptPeriods").addEventListener("click", function (e) {
    var del = e.target.closest("[data-pt-del]");
    if (!del) return;
    if (isReadonlyMode()) return;
    var card = e.target.closest("[data-pt-period]");
    state.ptPeriods = state.ptPeriods || [];
    state.ptPeriods.splice(Number(card.dataset.ptPeriod), 1);
    renderPricingPeriods();
  });
  document.getElementById("addPtPeriod").addEventListener("click", function () {
    if (isReadonlyMode()) return;
    state.ptPeriods = state.ptPeriods || [];
    state.ptPeriods.push({
      weekdays: [1, 2, 3, 4, 5],
      start: "09:00",
      end: "18:00",
      rates: {
        input_per_1m_tokens: 0,
        output_per_1m_tokens: 0,
        cache_write_per_1m_tokens: 0,
        cache_read_per_1m_tokens: 0,
      },
    });
    renderPricingPeriods();
  });
  document.getElementById("modelCostModal").addEventListener("input", function (event) { syncPricingContextInput(event.target); });
  document.getElementById("modelCostModal").addEventListener("change", function (event) { syncPricingContextInput(event.target); });
  document.getElementById("modelCostModal").addEventListener("click", pricingContextClick);
  document.getElementById("addPtDateException").addEventListener("click", function () {
    if (isReadonlyMode()) return;
    state.ptDateExceptions = state.ptDateExceptions || [];
    state.ptDateExceptions.push({start_date: "", end_date: "", rates: defaultContextRates(), conditions: []});
    renderPricingContext(); updatePricingModeUI();
  });
  document
    .getElementById("newModelCostBtn")
    .addEventListener("click", function () {
      openPricingModal(null, null);
    });
  document
    .getElementById("refreshModelCostsBtn")
    .addEventListener("click", function () {
      loadPricingTemplates();
    });
  document
    .getElementById("saveModelCostBtn")
    .addEventListener("click", async function () {
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
        var raw = document.getElementById(id).value.trim();
        if (raw === "") {
          toast("请填写" + label + "价格", "error");
          return null;
        }
        var value = Number(raw);
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
        var raw = document.getElementById(optional[0]).value.trim();
        var value = raw === "" ? null : Number(raw);
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
        var sd = new Date(startInput);
        if (isNaN(sd.getTime())) {
          toast("请填写有效的生效开始时间", "error");
          return;
        }
        payload.start = sd.toISOString();
        if (endInput) {
          var ed = new Date(endInput);
          if (isNaN(ed.getTime())) {
            toast("请填写有效的生效结束时间", "error");
            return;
          }
          payload.end = ed.toISOString();
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
          if (!p.start || !p.end) {
            toast("时段 " + (i + 1) + " 缺少开始或结束时间", "error");
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
          if (p.start >= p.end) {
            toast("时段 " + (i + 1) + " 开始时间必须早于结束时间", "error");
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
            start: p.start,
            end: p.end,
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
    });

  return {
    renderPricingContext: renderPricingContext,
    validatePricingContext: validatePricingContext,
    syncPricingContextInput: syncPricingContextInput,
    pricingContextClick: pricingContextClick,
    conditionList: conditionList,
    pricingTargets: pricingTargets,
    pricingModels: pricingModels,
    pricingSelected: pricingSelected,
    updatePricingScopeNotice: updatePricingScopeNotice,
    pricingGroupKey: pricingGroupKey,
    pricingAttrKey: pricingAttrKey,
    pricingDetailId: pricingDetailId,
    pricingParseAttrKey: pricingParseAttrKey,
    pricingStartIsZero: pricingStartIsZero,
    pricingIsTemplate0: pricingIsTemplate0,
    pricingRate: pricingRate,
    pricingWeekdays: pricingWeekdays,
    pricingRatesHtml: pricingRatesHtml,
    pricingRateGridHtml: pricingRateGridHtml,
    pricingRangeHtml: pricingRangeHtml,
    toLocalInputValue: toLocalInputValue,
    pricingTargetExists: pricingTargetExists,
    pricingTargetMissing: pricingTargetMissing,
    pricingTargetOptions: pricingTargetOptions,
    pricingModelLabel: pricingModelLabel,
    renderPricingModelSelect: renderPricingModelSelect,
    updatePricingModeUI: updatePricingModeUI,
    renderPricingPeriods: renderPricingPeriods,
    syncPricingPeriod: syncPricingPeriod,
    collectPricingPeriods: collectPricingPeriods,
    renderPricingDetail: renderPricingDetail,
    renderPricingTemplates: renderPricingTemplates,
    pricingTargetMeta: pricingTargetMeta,
    loadPricingTemplates: loadPricingTemplates,
    openPricingModal: openPricingModal,
    deletePricingTemplate: deletePricingTemplate,
  };
});
