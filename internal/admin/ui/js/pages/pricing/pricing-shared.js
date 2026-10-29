"use strict";
// Pricing shared helpers: scope keys, selects, rate/range formatting and target lookups.
AdminApp.register("pricingShared", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtTime = app.bind("core", "fmtTime");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var quotaModelLabel = app.bind("quota", "quotaModelLabel");
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
  function clonePricingValue(value) { return JSON.parse(JSON.stringify(value)); }
  return {
    pricingTargets: pricingTargets,
    pricingModels: pricingModels,
    pricingArray: pricingArray,
    pricingScopePart: pricingScopePart,
    pricingGroupKey: pricingGroupKey,
    pricingAttrKey: pricingAttrKey,
    pricingDetailId: pricingDetailId,
    pricingParseAttrKey: pricingParseAttrKey,
    pricingSelected: pricingSelected,
    pricingSelectValues: pricingSelectValues,
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
    clonePricingValue: clonePricingValue,
    PT_WEEKDAYS: PT_WEEKDAYS,
  };
});
