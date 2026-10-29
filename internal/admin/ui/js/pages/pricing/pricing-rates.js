"use strict";
// Pricing rate inputs: context rates, conditions, date exceptions and their validation.
AdminApp.register("pricingRates", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var clonePricingValue = app.bind("pricingShared", "clonePricingValue");
  var pricingInputNumber = app.bind("pricingShared", "pricingInputNumber");
  var pricingRateInputValue = app.bind("pricingShared", "pricingRateInputValue");
  var updatePricingModeUI = app.bind("pricingPeriods", "updatePricingModeUI");
  var renderPricingPeriods = app.bind("pricingPeriods", "renderPricingPeriods");
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
        "<input id='" + esc(id) + "' class='form-control' type='number' min='0' step='any' " + attributes + " data-pt-context-rate='" + field[0] + "' value='" + esc(pricingRateInputValue(rates[field[0]])) + "'" +
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
    // pricingInputNumber keeps unparseable text as NaN so validation rejects it instead of
    // silently treating it as blank (null price / no bound).
    if (data.ptContextRate) { item.rates = item.rates || {}; item.rates[data.ptContextRate] = pricingInputNumber(input); }
    else if (data.ptConditionField) {
      var field = data.ptConditionField;
      if (field === "min_input_tokens" || field === "max_input_tokens") { var bound = pricingInputNumber(input); if (bound === null) delete item[field]; else item[field] = bound; }
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
  document.getElementById("modelCostModal").addEventListener("input", function (event) { syncPricingContextInput(event.target); });
  document.getElementById("modelCostModal").addEventListener("change", function (event) { syncPricingContextInput(event.target); });
  document.getElementById("modelCostModal").addEventListener("click", pricingContextClick);
  document.getElementById("addPtDateException").addEventListener("click", function () {
    if (isReadonlyMode()) return;
    state.ptDateExceptions = state.ptDateExceptions || [];
    state.ptDateExceptions.push({start_date: "", end_date: "", rates: defaultContextRates(), conditions: []});
    renderPricingContext(); updatePricingModeUI();
  });
  return {
    contextRatesForm: contextRatesForm,
    conditionList: conditionList,
    conditionOptions: conditionOptions,
    conditionsForm: conditionsForm,
    renderPricingContext: renderPricingContext,
    syncPricingContextInput: syncPricingContextInput,
    defaultContextRates: defaultContextRates,
    pricingContextClick: pricingContextClick,
    validatePricingContext: validatePricingContext,
    contextRateFields: contextRateFields,
  };
});
