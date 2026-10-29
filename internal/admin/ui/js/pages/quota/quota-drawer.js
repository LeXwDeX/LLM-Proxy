"use strict";
// Quota edit drawer: job level, model-group selection and personal coefficients.
AdminApp.register("quotaDrawer", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var applyReadonlyMode = app.bind("users", "applyReadonlyMode");
  var modelGroupModels = app.bind("quotaModel", "modelGroupModels");
  var modelGroupLabel = app.bind("quotaModel", "modelGroupLabel");
  var globalQuotaGroupByName = app.bind("quotaModel", "globalQuotaGroupByName");
  var jobLevelByName = app.bind("quotaModel", "jobLevelByName");
  var jobLevelCoef = app.bind("quotaModel", "jobLevelCoef");
  var quotaNumber = app.bind("quotaModel", "quotaNumber");
  var loadQuotaManagement = app.bind("quotaData", "loadQuotaManagement");
  var runGuarded = app.bind("quotaModel", "runGuarded");
  var dictOptionsHTML = app.bind("quotaLevels", "dictOptionsHTML");
  var quotaDimensions = app.modules.quotaModel.quotaDimensions;
  /* ===== Quota Edit Drawer ===== */
  function openQuotaEditDrawer(item) {
    if (isReadonlyMode()) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    var c = item.client || {};
    state.editingQuotaClient = c.name;
    document.getElementById("quotaEditDrawerTitle").textContent =
      "编辑可用额度 — " + c.name;
    document.getElementById("quotaEditDrawerInfo").textContent =
      (c.ip_address || "-") + (c.project_group ? " · " + c.project_group : "");
    document.getElementById("quotaEditJobLevel").innerHTML = dictOptionsHTML(
      state.jobLevels,
      c.job_level || "",
      "（未绑职级 — 请求将被拒绝）",
    );
    state.quotaEditGroups = (c.quota_groups || []).map(function (group) {
      return {
        name: group.name,
        coefficient:
          group.coefficient == null ? null : Number(group.coefficient),
      };
    });
    renderQuotaEditGroups();
    openModal("quotaEditDrawer");
  }
  function renderQuotaEditGroups() {
    var selected = state.quotaEditGroups || [],
      selectedNames = {};
    selected.forEach(function (group) {
      selectedNames[String(group.name).toLowerCase()] = true;
    });
    var picker = document.getElementById("quotaEditGroupPicker");
    picker.innerHTML =
      "<option value=''>选择模型组...</option>" +
      state.modelGroups
        .map(function (group) {
          var disabled = selectedNames[String(group.name).toLowerCase()]
            ? " disabled"
            : "";
          return (
            "<option value='" +
            esc(group.name) +
            "'" +
            disabled +
            ">" +
            esc(modelGroupLabel(group.name)) +
            "</option>"
          );
        })
        .join("");
    var levelName =
      (document.getElementById("quotaEditJobLevel") || {}).value || "";
    var level = jobLevelByName(levelName);
    var editor = document.getElementById("quotaEditGroupEditor");
    editor.innerHTML = selected.length
      ? selected
          .map(function (group, index) {
            var global = globalQuotaGroupByName(group.name) || {};
            var gc = Number(
              global.cost_coefficient == null ? 1 : global.cost_coefficient,
            );
            var preview = quotaEffectivePreview(group, level);
            return (
              "<div class='quota-group-card' data-edit-group-index='" +
              index +
              "'>" +
              "<div class='quota-edit-group-header'><div><label>组名</label><strong>" +
              esc(modelGroupLabel(group.name)) +
              "</strong></div>" +
              "<div><label>扣费折算系数</label><strong>× " +
              quotaNumber(gc) +
              "</strong></div>" +
              "<button class='btn btn-sm btn-danger readonly-hide' type='button' data-remove-edit-group>移除</button></div>" +
              "<div style='margin-top:8px;font-size:12px;color:var(--text-muted);'>所属模型：" +
              modelGroupModels(global) +
              "</div>" +
              "<div style='margin-top:4px;font-size:12px;color:var(--text-muted);'>额度基数：" +
              quotaDimensions
                .map(function (dim) {
                  return (
                    esc(dim.label) +
                    " $" +
                    quotaNumber(global[dim.base_field] || 0)
                  );
                })
                .join(" / ") +
              "（只读，在模型组页维护）</div>" +
              "<div class='quota-edit-limit-list'><div class='quota-edit-limit-row'><label>个人可用额度倍率</label><div class='form-group'><input class='form-control readonly-disable' data-edit-coefficient type='number' min='0' step='0.01' placeholder='留空 = 1；0 = 不限' value='" +
              (group.coefficient == null ? "" : esc(group.coefficient)) +
              "'></div></div></div>" +
              "<div data-effective-preview style='margin-top:6px;font-size:12px;color:var(--text-muted);'>可用额度上限预览：" +
              preview +
              "</div></div>"
            );
          })
          .join("")
      : "<div style='padding:12px 0;color:var(--text-muted);font-size:12px;'>未选择模型组</div>";
    applyReadonlyMode(editor);
  }
  function quotaEffectivePreview(group, level) {
    // Without a bound (existing) job level the backend rejects the request, so no limit applies.
    if (!level) return "未绑定职级或职级不存在，请求将被拒绝";
    var global = globalQuotaGroupByName(group.name) || {};
    var personal = group.coefficient == null ? 1 : Number(group.coefficient);
    var groupCoefficient = Number(
      global.cost_coefficient == null ? 1 : global.cost_coefficient,
    );
    return quotaDimensions
      .map(function (dim) {
        var base = Number(global[dim.base_field] || 0);
        var rank = jobLevelCoef(level, dim.key);
        var limit =
          base > 0 && rank > 0 && personal > 0 && groupCoefficient > 0
            ? base * rank * personal
            : 0;
        return (
          esc(dim.label) + " " + (limit > 0 ? "$" + quotaNumber(limit) : "不限")
        );
      })
      .join(" / ");
  }
  document
    .getElementById("quotaEditJobLevel")
    .addEventListener("change", function () {
      if (isReadonlyMode()) return;
      syncQuotaEditGroups();
      renderQuotaEditGroups();
    });
  function syncQuotaEditGroups() {
    var groups = [];
    var invalid = false;
    document
      .querySelectorAll("#quotaEditGroupEditor [data-edit-group-index]")
      .forEach(function (card) {
        var index = Number(card.dataset.editGroupIndex),
          group = (state.quotaEditGroups || [])[index];
        if (!group) return;
        var input = card.querySelector("[data-edit-coefficient]");
        var raw = input.value.trim();
        if (input.validity.badInput) {
          invalid = true;
          return;
        }
        if (raw === "") {
          groups.push({ name: group.name });
          return;
        }
        var coefficient = Number(raw);
        if (!Number.isFinite(coefficient) || coefficient < 0) {
          invalid = true;
          return;
        }
        groups.push({ name: group.name, coefficient: coefficient });
      });
    if (invalid) return null;
    state.quotaEditGroups = groups;
    return groups;
  }
  document
    .getElementById("quotaEditGroupPicker")
    .addEventListener("change", function (e) {
      if (isReadonlyMode() || !e.target.value) return;
      var global = globalQuotaGroupByName(e.target.value);
      if (!global) return;
      state.quotaEditGroups = state.quotaEditGroups || [];
      if (
        state.quotaEditGroups.some(function (group) {
          return (
            String(group.name).toLowerCase() ===
            String(global.name).toLowerCase()
          );
        })
      ) {
        toast("该模型组已添加", "error");
        e.target.value = "";
        return;
      }
      state.quotaEditGroups.push({ name: global.name });
      e.target.value = "";
      renderQuotaEditGroups();
    });
  document
    .getElementById("quotaEditGroupEditor")
    .addEventListener("input", function () {
      if (isReadonlyMode()) return;
      var groups = syncQuotaEditGroups();
      var level = jobLevelByName(
        document.getElementById("quotaEditJobLevel").value,
      );
      document
        .querySelectorAll("#quotaEditGroupEditor [data-edit-group-index]")
        .forEach(function (card) {
          var preview = card.querySelector("[data-effective-preview]");
          if (!preview) return;
          var group = groups && groups[Number(card.dataset.editGroupIndex)];
          preview.textContent = group
            ? "可用额度上限预览：" + quotaEffectivePreview(group, level)
            : "可用额度上限预览：请输入有效的个人倍率";
        });
    });
  document
    .getElementById("quotaEditGroupEditor")
    .addEventListener("click", function (e) {
      if (isReadonlyMode() || !e.target.matches("[data-remove-edit-group]"))
        return;
      var card = e.target.closest("[data-edit-group-index]");
      if (!card) return;
      state.quotaEditGroups.splice(Number(card.dataset.editGroupIndex), 1);
      renderQuotaEditGroups();
    });
  async function saveQuotaEdit() {
    if (isReadonlyMode() || !state.editingQuotaClient) return;
    var groups = syncQuotaEditGroups();
    if (groups === null) {
      toast(
        "个人可用额度倍率必须是 ≥0 的数字（留空 = 1；0 = 不限）",
        "error",
      );
      return;
    }
    var payload = {
      job_level: document.getElementById("quotaEditJobLevel").value,
      quota_groups: groups,
    };
    try {
      await api(
        "/admin/data/quota/" + encodeURIComponent(state.editingQuotaClient),
        { method: "PUT", body: payload },
      );
      closeModal("quotaEditDrawer");
      toast("额度已更新", "success");
      await loadQuotaManagement();
    } catch (e) {
      toast("保存失败: " + e.message, "error");
    }
  }
  document
    .getElementById("saveQuotaEditBtn")
    .addEventListener("click", function () {
      return runGuarded("saveQuotaEdit", document.getElementById("saveQuotaEditBtn"), saveQuotaEdit);
    });
  return {
    openQuotaEditDrawer: openQuotaEditDrawer,
    renderQuotaEditGroups: renderQuotaEditGroups,
    quotaEffectivePreview: quotaEffectivePreview,
    syncQuotaEditGroups: syncQuotaEditGroups,
  };
});
