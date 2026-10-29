"use strict";
// Pricing list: grouped table, detail cards, loading and deletion.
AdminApp.register("pricingList", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var showConfirm = app.bind("core", "showConfirm");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var loadQuotaEditorData = app.bind("quota", "loadQuotaEditorData");
  var pricingTargets = app.bind("pricingShared", "pricingTargets");
  var pricingModels = app.bind("pricingShared", "pricingModels");
  var pricingGroupKey = app.bind("pricingShared", "pricingGroupKey");
  var pricingAttrKey = app.bind("pricingShared", "pricingAttrKey");
  var pricingDetailId = app.bind("pricingShared", "pricingDetailId");
  var pricingParseAttrKey = app.bind("pricingShared", "pricingParseAttrKey");
  var pricingIsTemplate0 = app.bind("pricingShared", "pricingIsTemplate0");
  var pricingWeekdays = app.bind("pricingShared", "pricingWeekdays");
  var pricingRateGridHtml = app.bind("pricingShared", "pricingRateGridHtml");
  var pricingRangeHtml = app.bind("pricingShared", "pricingRangeHtml");
  var pricingTargetMissing = app.bind("pricingShared", "pricingTargetMissing");
  var openPricingModal = app.bind("pricingModal", "openPricingModal");
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
  document
    .getElementById("refreshModelCostsBtn")
    .addEventListener("click", function () {
      loadPricingTemplates();
    });
  return {
    conditionsDetail: conditionsDetail,
    renderPricingDetail: renderPricingDetail,
    renderPricingTemplates: renderPricingTemplates,
    pricingTargetMeta: pricingTargetMeta,
    loadPricingTemplates: loadPricingTemplates,
    deletePricingTemplate: deletePricingTemplate,
  };
});
