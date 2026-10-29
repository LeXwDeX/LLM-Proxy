"use strict";
// Model groups: table, binding editor and create/edit/delete modal.
AdminApp.register("quotaGroups", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtMoney = app.bind("core", "fmtMoney");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var modelGroupModels = app.bind("quotaModel", "modelGroupModels");
  var modelGroupLabel = app.bind("quotaModel", "modelGroupLabel");
  var quotaTargetOptions = app.bind("quotaModel", "quotaTargetOptions");
  var quotaModelOptions = app.bind("quotaModel", "quotaModelOptions");
  var loadQuotaEditorData = app.bind("quotaData", "loadQuotaEditorData");
  var loadQuotaManagement = app.bind("quotaData", "loadQuotaManagement");
  var quotaInputNumber = app.bind("quotaModel", "quotaInputNumber");
  var quotaDimensions = app.modules.quotaModel.quotaDimensions;
  var shared = app.modules.quotaModel.shared;
  function renderModelGroups() {
    document.getElementById("modelGroupsBody").innerHTML = state.modelGroups
      .length
      ? state.modelGroups
          .map(function (group, index) {
            return (
              "<tr><td><strong>" +
              esc(modelGroupLabel(group.name)) +
              (shared.modelGroupNeedsRefresh[group.name] ? "（已过期，请刷新）" : "") +
              "</strong></td><td>" +
              modelGroupModels(group) +
              "</td><td>× " +
              esc(group.cost_coefficient) +
              "</td><td>" +
              quotaDimensions
                .map(function (dim) {
                  return (
                    esc(dim.label) + " " + fmtMoney(group[dim.base_field] || 0) +
                    (Number(group[dim.base_field] || 0) === 0 ? "（已绑定职级时该周期上限不限）" : "")
                  );
                })
                .join("<br>") +
              "</td><td><button class='btn btn-sm readonly-hide' data-edit-model-group='" +
              index +
              "'>编辑</button> <button class='btn btn-sm btn-danger readonly-hide' data-delete-model-group='" +
              esc(group.name) +
              "'>删除</button></td></tr>"
            );
          })
          .join("")
      : "<tr class='empty-row'><td colspan='5'>暂无模型组</td></tr>";
  }
  function renderModelGroupBindings(bindings) {
    var existingTargets = (state.targets || []).map(function (t) {
      return String(t.name || "").toLowerCase();
    });
    document.getElementById("modelGroupBindingEditor").innerHTML = (
      bindings || []
    )
      .map(function (binding, index) {
        var stale =
          !!binding.target &&
          existingTargets.indexOf(String(binding.target).toLowerCase()) < 0;
        var targetSelect = stale
          ? "<select class='form-control' data-model-binding-target disabled><option value='" +
            esc(binding.target) +
            "'>" +
            esc(binding.target) +
            "（目标已删除）</option></select>"
          : "<select class='form-control readonly-disable' data-model-binding-target>" +
            quotaTargetOptions(binding.target || "") +
            "</select>";
        var modelSelect = stale
          ? "<select class='form-control' data-model-binding-model disabled><option value='" +
            esc(binding.model || "") +
            "'>" +
            esc(binding.model || "整个 Target") +
            "</option></select>"
          : "<select class='form-control readonly-disable' data-model-binding-model>" +
            quotaModelOptions(binding.target || "", binding.model || "") +
            "</select>";
        var removeBtn = stale
          ? ""
          : "<button class='btn btn-sm btn-danger readonly-hide' type='button' data-remove-model-binding>移除</button>";
        return (
          "<div class='quota-binding-row' data-model-binding-index='" +
          index +
          "'>" +
          targetSelect +
          modelSelect +
          removeBtn +
          "</div>"
        );
      })
      .join("");
  }
  function collectModelGroupBindings() {
    var result = [];
    document
      .querySelectorAll("#modelGroupBindingEditor [data-model-binding-index]")
      .forEach(function (row) {
        var target = row.querySelector("[data-model-binding-target]").value,
          model = row.querySelector("[data-model-binding-model]").value;
        if (target)
          result.push(
            model ? { target: target, model: model } : { target: target },
          );
      });
    return result;
  }
  var modelGroupOpenVersion = 0;
  var modelGroupDraftRevision = 0;
  var modelGroupSaving = false;
  var modelGroupCreatedDraft = false;
  var modelGroupSavingName = "";
  var modelGroupSavingOriginalName = "";
  var modelGroupVersions = Object.create(null);
  var editingModelGroupVersion = 0;
  ["input", "change"].forEach(function (eventName) {
    document.getElementById("modelGroupModal").addEventListener(eventName, function () { modelGroupDraftRevision++; });
  });
  document.getElementById("modelGroupModal").addEventListener("click", function (event) {
    var button = event.target.closest("button");
    if (modelGroupSaving && button && button.id !== "saveModelGroupBtn") modelGroupDraftRevision++;
  });
  async function openModelGroupModal(group) {
    if (isReadonlyMode()) return;
    if (group && shared.modelGroupNeedsRefresh[group.name]) {
      toast("模型组已保存但列表尚未刷新，请刷新后重新编辑。", "error");
      return;
    }
    if (group && modelGroupSaving && (modelGroupSavingName === group.name || modelGroupSavingOriginalName === group.name)) {
      toast("该模型组正在保存，请等待完成后重新打开。", "error");
      return;
    }
    var groupVersion = group ? (modelGroupVersions[group.name] || 0) : 0;
    var version = ++modelGroupOpenVersion;
    try {
      await loadQuotaEditorData();
    } catch (e) {
      if (version !== modelGroupOpenVersion) return;
      toast("加载模型组编辑数据失败: " + e.message, "error");
      return;
    }
    if (version !== modelGroupOpenVersion) return;
    if (group && (shared.modelGroupNeedsRefresh[group.name] || (modelGroupSaving &&
        (modelGroupSavingName === group.name || modelGroupSavingOriginalName === group.name)) ||
        (modelGroupVersions[group.name] || 0) !== groupVersion)) {
      toast("模型组正在保存或已更新，请重新打开确认。", "error");
      return;
    }
    modelGroupCreatedDraft = false;
    editingModelGroupVersion = groupVersion;
    state.editingModelGroup = group || null;
    document.getElementById("modelGroupModalTitle").textContent = group
      ? "编辑模型组"
      : "新建模型组";
    document.getElementById("modelGroupName").value = group ? group.name : "";
    document.getElementById("modelGroupCoefficient").value = group
      ? group.cost_coefficient
      : 1;
    quotaDimensions.forEach(function (dim) {
      document.getElementById("modelGroup-" + dim.key).value =
        group && group[dim.base_field] != null ? group[dim.base_field] : "";
    });
    renderModelGroupBindings(group ? group.bindings : []);
    openModal("modelGroupModal");
  }
  document
    .getElementById("newModelGroupBtn")
    .addEventListener("click", function () {
      openModelGroupModal(null);
    });
  document
    .getElementById("modelGroupsBody")
    .addEventListener("click", function (e) {
      if (isReadonlyMode()) return;
      if (e.target.dataset.editModelGroup !== undefined)
        openModelGroupModal(
          state.modelGroups[Number(e.target.dataset.editModelGroup)],
        );
      if (e.target.dataset.deleteModelGroup)
        showConfirm(
          "确认删除模型组「" +
            modelGroupLabel(e.target.dataset.deleteModelGroup) +
            "」？引用该组的客户端方案将一并移除该组。",
          async function () {
            try {
              var res = await api(
                "/admin/data/model-groups/" +
                  encodeURIComponent(e.target.dataset.deleteModelGroup),
                { method: "DELETE" },
              );
              var affected = (res && res.removed_from_clients) || [];
              toast(
                affected.length
                  ? "模型组已删除，已从 " +
                      affected.length +
                      " 个客户端方案中移除"
                  : "模型组已删除",
                "success",
              );
              await loadQuotaManagement();
            } catch (err) {
              toast("删除失败: " + err.message, "error");
            }
          },
        );
    });
  document
    .getElementById("modelGroupBindingEditor")
    .addEventListener("change", function (e) {
      if (!e.target.matches("[data-model-binding-target]")) return;
      var row = e.target.closest("[data-model-binding-index]");
      row.querySelector("[data-model-binding-model]").innerHTML =
        quotaModelOptions(e.target.value, "");
    });
  document
    .getElementById("modelGroupBindingEditor")
    .addEventListener("click", function (e) {
      if (isReadonlyMode() || !e.target.matches("[data-remove-model-binding]"))
        return;
      var bindings = collectModelGroupBindings();
      bindings.splice(
        Number(
          e.target.closest("[data-model-binding-index]").dataset
            .modelBindingIndex,
        ),
        1,
      );
      renderModelGroupBindings(bindings);
    });
  document
    .getElementById("addModelGroupBinding")
    .addEventListener("click", function () {
      if (isReadonlyMode()) return;
      var bindings = collectModelGroupBindings();
      bindings.push({ target: "", model: "" });
      renderModelGroupBindings(bindings);
    });
  document
    .getElementById("saveModelGroupBtn")
    .addEventListener("click", async function () {
      if (isReadonlyMode()) return;
      if (modelGroupSaving) {
        toast("模型组正在保存，请稍候。", "error");
        return;
      }
      if (modelGroupCreatedDraft) {
        toast("模型组已创建，请复制未保存的修改并重新打开编辑。", "error");
        return;
      }
      if (state.editingModelGroup && shared.modelGroupNeedsRefresh[state.editingModelGroup.name] &&
          !document.getElementById("modelGroupModal").classList.contains("open")) {
        toast("模型组已保存但列表尚未刷新，请刷新后重新编辑。", "error");
        return;
      }
      if (state.editingModelGroup && editingModelGroupVersion !== (modelGroupVersions[state.editingModelGroup.name] || 0)) {
        toast("模型组已更新，请保留草稿并重新打开确认。", "error");
        return;
      }
      var name = document.getElementById("modelGroupName").value.trim(),
        coefficient = quotaInputNumber(
          document.getElementById("modelGroupCoefficient"),
        );
      if (!name || coefficient === null || !Number.isFinite(coefficient) || coefficient < 0) {
        toast("请填写有效的模型组标识和扣费折算系数（系数须 ≥0）", "error");
        return;
      }
      var payload = {
        name: name,
        cost_coefficient: coefficient,
        bindings: collectModelGroupBindings(),
      };
      // Base 0 means "unlimited once a job level is bound", so a blank field must never be
      // read as 0: every base has to be typed explicitly (0 = unlimited), and text the browser
      // cannot parse is rejected rather than collapsing to blank.
      var bad = false, blank = [];
      quotaDimensions.forEach(function (dim) {
        var value = quotaInputNumber(document.getElementById("modelGroup-" + dim.key));
        if (value === null) {
          blank.push(dim.label);
          return;
        }
        if (!Number.isFinite(value) || value < 0) {
          bad = true;
          return;
        }
        payload[dim.base_field] = value;
      });
      if (blank.length) {
        toast("请填写额度基数：" + blank.join("、") + "（0 = 已绑定职级时该周期不限）", "error");
        return;
      }
      if (bad) {
        toast("模型组额度基数必须是 ≥0 的数字", "error");
        return;
      }
      var saveVersion = modelGroupOpenVersion;
      var saveRevision = modelGroupDraftRevision;
      var current = state.editingModelGroup;
      modelGroupSaving = true;
      modelGroupSavingName = name;
      modelGroupSavingOriginalName = current ? current.name : "";
      try {
        await api(
          current
            ? "/admin/data/model-groups/" + encodeURIComponent(current.name)
            : "/admin/data/model-groups",
          { method: current ? "PUT" : "POST", body: payload },
        );
        ++shared.quotaLoadVersion;
        if (current) shared.modelGroupNeedsRefresh[current.name] = true;
        shared.modelGroupNeedsRefresh[name] = true;
        if (current && current.name !== name) modelGroupVersions[current.name] = (modelGroupVersions[current.name] || 0) + 1;
        modelGroupVersions[name] = (modelGroupVersions[name] || 0) + 1;
        if (current && saveVersion === modelGroupOpenVersion) {
          state.editingModelGroup = Object.assign({}, current, {name: name});
          editingModelGroupVersion = modelGroupVersions[name];
        }
        renderModelGroups();
        toast("模型组已保存", "success");
        if (saveVersion === modelGroupOpenVersion && saveRevision === modelGroupDraftRevision) {
          closeModal("modelGroupModal");
        } else if (saveVersion === modelGroupOpenVersion) {
          if (!current) modelGroupCreatedDraft = true;
          toast(current ? "请求时的内容已保存，后续修改尚未保存。" :
            "模型组已创建，请复制未保存的修改并重新打开编辑。", "error");
        }
        await loadQuotaManagement();
      } catch (e) {
        toast("保存失败: " + e.message, "error");
      } finally {
        modelGroupSaving = false;
        modelGroupSavingName = "";
        modelGroupSavingOriginalName = "";
      }
    });
  return {
    renderModelGroups: renderModelGroups,
    renderModelGroupBindings: renderModelGroupBindings,
    collectModelGroupBindings: collectModelGroupBindings,
    openModelGroupModal: openModelGroupModal,
  };
});
