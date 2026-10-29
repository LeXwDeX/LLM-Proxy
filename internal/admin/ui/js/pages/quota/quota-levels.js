"use strict";
// Job levels and project-group dictionaries: tables and modals.
AdminApp.register("quotaLevels", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var jobLevelEntryName = app.bind("quotaModel", "jobLevelEntryName");
  var jobLevelByName = app.bind("quotaModel", "jobLevelByName");
  var jobLevelCoefText = app.bind("quotaModel", "jobLevelCoefText");
  var loadQuotaManagement = app.bind("quotaData", "loadQuotaManagement");
  var runGuarded = app.bind("core", "runGuarded");
  var quotaInputNumber = app.bind("quotaModel", "quotaInputNumber");
  var loadDictOptions = app.bind("quotaData", "loadDictOptions");
  var quotaDimensions = app.modules.quotaModel.quotaDimensions;
  var dictKinds = {
    job: { label: "职级", path: "job-levels", empty: "（未设置）" },
    project: { label: "项目组", path: "project-groups", empty: "（未设置）" },
  };
  function dictEmptyLabel(kind) {
    return dictKinds[kind].empty;
  }
  function dictOptionsHTML(entries, current, emptyLabel) {
    var values = {};
    (entries || []).forEach(function (v) {
      var name = jobLevelEntryName(v);
      if (name) values[name] = true;
    });
    // Names are matched case-insensitively (the backend resolves them that way): a stored
    // "lead" selects the existing "Lead" option instead of adding a second entry.
    var selected = current;
    if (current) {
      var folded = String(current).toLowerCase();
      var existing = Object.keys(values).filter(function (name) {
        return name.toLowerCase() === folded;
      })[0];
      if (existing) selected = existing;
      else values[current] = true;
    }
    return (
      "<option value=''>" +
      esc(emptyLabel) +
      "</option>" +
      Object.keys(values)
        .sort()
        .map(function (v) {
          return (
            "<option value='" +
            esc(v) +
            "'" +
            (v === selected ? " selected" : "") +
            ">" +
            esc(v) +
            "</option>"
          );
        })
        .join("")
    );
  }
  function renderDictTables() {
    var levels = state.jobLevels || [];
    document.getElementById("jobLevelsBody").innerHTML = levels.length
      ? levels
          .map(function (level) {
            var name = jobLevelEntryName(level);
            return (
              "<tr><td><strong>" +
              esc(name) +
              "</strong></td>" +
              quotaDimensions
                .map(function (dim) {
                  return (
                    "<td>× " +
                    esc(
                      jobLevelCoefText(
                        typeof level === "string"
                          ? null
                          : level[dim.job_coefficient_field],
                      ),
                    ) +
                    "</td>"
                  );
                })
                .join("") +
              "<td><button class='btn btn-sm readonly-hide' data-edit-job-level='" +
              esc(name) +
              "'>编辑</button> <button class='btn btn-sm btn-danger readonly-hide' data-delete-job-level='" +
              esc(name) +
              "'>删除</button></td></tr>"
            );
          })
          .join("")
      : "<tr class='empty-row'><td colspan='" +
        (quotaDimensions.length + 2) +
        "'>暂无职级</td></tr>";
    var entries = state.projectGroups || [];
    document.getElementById("projectGroupsBody").innerHTML = entries.length
      ? entries
          .map(function (entry) {
            return (
              "<tr><td><strong>" +
              esc(entry) +
              "</strong></td><td><button class='btn btn-sm readonly-hide' data-edit-dict='" +
              esc(entry) +
              "' data-dict-kind='project'>编辑</button> <button class='btn btn-sm btn-danger readonly-hide' data-delete-dict='" +
              esc(entry) +
              "' data-dict-kind='project'>删除</button></td></tr>"
            );
          })
          .join("")
      : "<tr class='empty-row'><td colspan='2'>暂无项目组</td></tr>";
  }

  /* ===== 职级（结构化：三系数） ===== */
  function openJobLevelModal(level) {
    if (isReadonlyMode()) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    state.editingJobLevel = level || null;
    var isObj = level && typeof level !== "string";
    document.getElementById("jobLevelModalTitle").textContent = level
      ? "编辑职级"
      : "新增职级";
    document.getElementById("jobLevelName").value = level
      ? jobLevelEntryName(level)
      : "";
    quotaDimensions.forEach(function (dim) {
      document.getElementById("jobLevel-" + dim.key).value =
        isObj && level[dim.job_coefficient_field] != null
          ? level[dim.job_coefficient_field]
          : "";
    });
    openModal("jobLevelModal");
  }
  async function saveJobLevel() {
    if (isReadonlyMode()) return;
    var name = document.getElementById("jobLevelName").value.trim();
    if (!name) {
      toast("职级名称不能为空", "error");
      return;
    }
    var payload = { name: name };
    var bad = false;
    quotaDimensions.forEach(function (dim) {
      // Blank keeps the field unset (×1); text the browser cannot parse must not collapse to blank.
      var v = quotaInputNumber(document.getElementById("jobLevel-" + dim.key));
      if (v === null) return;
      if (!Number.isFinite(v) || v < 0) {
        bad = true;
        return;
      }
      payload[dim.job_coefficient_field] = v;
    });
    if (bad) {
      toast("职级可用额度倍率必须是 ≥0 的数字（0 = 不限）", "error");
      return;
    }
    var current = state.editingJobLevel;
    var currentName = current ? jobLevelEntryName(current) : "";
    try {
      var res = await api(
        current
          ? "/admin/data/job-levels/" + encodeURIComponent(currentName)
          : "/admin/data/job-levels",
        { method: current ? "PUT" : "POST", body: payload },
      );
      closeModal("jobLevelModal");
      var renamed = (res && res.renamed_clients) || [];
      toast(
        renamed.length
          ? "职级已保存，" + renamed.length + " 个客户端已跟随改名"
          : "职级已保存",
        "success",
      );
      await loadQuotaManagement();
    } catch (e) {
      toast("保存失败: " + e.message, "error");
    }
  }
  document
    .getElementById("saveJobLevelBtn")
    .addEventListener("click", function () {
      return runGuarded("saveJobLevel", document.getElementById("saveJobLevelBtn"), saveJobLevel);
    });
  document
    .getElementById("jobLevelsBody")
    .addEventListener("click", function (e) {
      if (isReadonlyMode()) return;
      var editName = e.target.dataset.editJobLevel;
      if (editName !== undefined) {
        openJobLevelModal(jobLevelByName(editName) || editName);
        return;
      }
      var delName = e.target.dataset.deleteJobLevel;
      if (delName) {
        showConfirm(
          "确认删除职级「" +
            delName +
            "」？仍被客户端绑定时将拒绝删除并列出客户端。",
          async function () {
            try {
              await api(
                "/admin/data/job-levels/" + encodeURIComponent(delName),
                { method: "DELETE" },
              );
              toast("职级已删除", "success");
              await loadQuotaManagement();
            } catch (err) {
              toast("删除失败: " + err.message, "error");
            }
          },
        );
      }
    });
  async function openDictModal(kind, entry) {
    if (isReadonlyMode()) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    await loadDictOptions();
    state.editingDict = { kind: kind, entry: entry || null };
    var meta = dictKinds[kind];
    document.getElementById("dictModalTitle").textContent = entry
      ? "编辑" + meta.label
      : "新增" + meta.label;
    document.getElementById("dictModalLabel").textContent = meta.label + "名称";
    document.getElementById("dictEntryName").value = entry || "";
    openModal("dictModal");
  }
  document
    .getElementById("newJobLevelBtn")
    .addEventListener("click", function () {
      openJobLevelModal(null);
    });
  document
    .getElementById("newProjectGroupBtn")
    .addEventListener("click", function () {
      openDictModal("project", null);
    });
  async function saveDictEntry() {
    if (isReadonlyMode()) return;
    var ed = state.editingDict;
    if (!ed) return;
    var meta = dictKinds[ed.kind];
    var name = document.getElementById("dictEntryName").value.trim();
    if (!name) {
      toast("名称不能为空", "error");
      return;
    }
    try {
      await api(
        ed.entry
          ? "/admin/data/" + meta.path + "/" + encodeURIComponent(ed.entry)
          : "/admin/data/" + meta.path,
        { method: ed.entry ? "PUT" : "POST", body: { name: name } },
      );
      closeModal("dictModal");
      toast(meta.label + "已保存", "success");
      await loadQuotaManagement();
    } catch (e) {
      toast("保存失败: " + e.message, "error");
    }
  }
  document
    .getElementById("saveDictEntryBtn")
    .addEventListener("click", function () {
      return runGuarded("saveDictEntry", document.getElementById("saveDictEntryBtn"), saveDictEntry);
    });
  document
    .getElementById("projectGroupsBody")
    .addEventListener("click", function (e) {
      if (isReadonlyMode()) return;
      var name = e.target.dataset.deleteDict;
      if (name) {
        var meta = dictKinds[e.target.dataset.dictKind];
        showConfirm(
          "确认删除" +
            meta.label +
            "「" +
            name +
            "」？已有客户端仍会保留该值。",
          async function () {
            try {
              await api(
                "/admin/data/" + meta.path + "/" + encodeURIComponent(name),
                { method: "DELETE" },
              );
              toast(meta.label + "已删除", "success");
              await loadQuotaManagement();
            } catch (err) {
              toast("删除失败: " + err.message, "error");
            }
          },
        );
      }
      if (e.target.dataset.editDict !== undefined)
        openDictModal(e.target.dataset.dictKind, e.target.dataset.editDict);
    });
  return {
    dictEmptyLabel: dictEmptyLabel,
    dictOptionsHTML: dictOptionsHTML,
    renderDictTables: renderDictTables,
    openJobLevelModal: openJobLevelModal,
    openDictModal: openDictModal,
  };
});
