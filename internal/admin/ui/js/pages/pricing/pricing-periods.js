"use strict";
// Pricing periods editor: schedule mode, period cards and their inputs.
AdminApp.register("pricingPeriods", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var conditionsForm = app.bind("pricingRates", "conditionsForm");
  var PT_WEEKDAYS = app.modules.pricingShared.PT_WEEKDAYS;
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
  return {
    updatePricingModeUI: updatePricingModeUI,
    renderPricingPeriods: renderPricingPeriods,
    syncPricingPeriod: syncPricingPeriod,
    collectPricingPeriods: collectPricingPeriods,
    imagePeriodInputs: imagePeriodInputs,
  };
});
