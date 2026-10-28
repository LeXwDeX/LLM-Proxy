"use strict";
// Quota owns model groups, quota dimensions, job/project dictionaries and quota editing/sync.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("quota", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var fmtTime = app.bind("core", "fmtTime");
  var totalTok = app.bind("core", "totalTok");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var refreshUsageModelGroupFilter = app.bind(
    "usage",
    "refreshUsageModelGroupFilter",
  );
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var applyReadonlyMode = app.bind("users", "applyReadonlyMode");
  var loadTargets = app.bind("targets", "loadTargets");
  var awaitSchedulingRefresh = app.bind("targets", "awaitSchedulingRefresh");
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
  function modelGroupLabel(name) {
    return name === "company_general" ? "公司通用" : name;
  }

  /* ===== Quota Management ===== */
  function quotaTargetByName(name) {
    return (state.targets || []).find(function (target) {
      return String(target.name).toLowerCase() === String(name).toLowerCase();
    });
  }
  function quotaModelLabel(target, model) {
    var entry = (state.catalogAllCache || []).find(function (item) {
      return (
        item.endpoint_type === target.endpoint_type &&
        String(item.model || "").toLowerCase() ===
          String(model || "").toLowerCase()
      );
    });
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
  function quotaConvertedDetail(status) {
    var groups = status.groups || {};
    var names = Object.keys(groups).sort();
    var html =
      "<div class='quota-detail-note'>人员行的各周期进度是各独立限额组的合计，不是共享额度池；每个模型组的上限分别生效，任一组不限时合计显示不限。基数是配置值，已绑定职级时基数为 0 表示该周期上限不限；未绑定职级的请求被拒绝，可用上限以额度状态为准。扣费系数只用于消耗折算，不乘入可用上限；但扣费系数为 0 时该组不限。下次重置时间按模型组和周期分别列出。</div>";
    if (status.calibration_error) {
      html += "<div class='quota-detail-group'><h4>计费校准不可用</h4><div>" +
        esc(status.calibration_error) +
        "</div><div>用量暂未完成校准，请求暂时拒绝。修复计费数据后重新校准恢复。</div></div>";
    }
    if (status.job_level_missing) {
      html +=
        "<div class='quota-detail-group'><h4>职级</h4><div style='padding:6px 0;color:var(--bad);font-size:12px;'>未绑定职级：未配置额度，请求将被 429 拒绝。请在「编辑额度」中绑定职级。</div></div>";
    } else if (status.job_level) {
      var level = jobLevelByName(status.job_level);
      html +=
        "<div class='quota-detail-group'><h4>职级可用额度倍率 · " +
        esc(status.job_level) +
        "</h4><div style='font-size:12px;color:var(--text-muted);padding:2px 0;'>" +
        quotaDimensions
          .map(function (dim) {
            return (
              esc(dim.label) +
              " × " +
              esc(jobLevelCoefText(level && level[dim.job_coefficient_field]))
            );
          })
          .join(" · ") +
        "</div></div>";
    }
    if (names.length) {
      var groupRows = names
        .map(function (name) {
          var g = groups[name] || {},
            quotas = g.quotas || {};
          var gc = Number(g.cost_coefficient == null ? 1 : g.cost_coefficient);
          var pc = Number(g.plan_coefficient == null ? 1 : g.plan_coefficient);
          var rank = g.rank_coefficients || {};
          var base = g.base_limits || {};
          return (
            "<tr><th>" +
            esc(modelGroupLabel(name)) +
            (g.in_plan ? "" : " <span class='quota-badge none'>兜底</span>") +
            "</th>" +
            quotaDimensions
              .map(function (dim) {
                var q = quotas[dim.key] || {};
                var formula =
                  "可用上限：基数 " +
                  quotaNumber(base[dim.key] || 0) +
                  " × 职级倍率 " +
                  quotaNumber(rank[dim.key] == null ? 1 : rank[dim.key]) +
                  " × 个人倍率 " +
                  quotaNumber(pc) +
                  "；基数、职级倍率、个人倍率及扣费系数均 >0 时生成正上限，扣费系数为 0 时该组不限；折算消耗：原价 × 扣费系数 " +
                  quotaNumber(gc);
                var reset = quotaResetText(q.resets_at);
                return (
                  "<td>" +
                  quotaNumber(q.used || 0) +
                  " / " +
                  (q.limit > 0 ? quotaNumber(q.limit) : "不限") +
                  "<small style='display:block;color:var(--text-muted);font-weight:400;'>" +
                  formula +
                  "</small>" +
                  (reset
                    ? "<small class='quota-reset'>下次重置：" +
                      esc(reset) +
                      "</small>"
                    : "") +
                  "</td>"
                );
              })
              .join("") +
            "</tr>"
          );
        })
        .join("");
      html +=
        "<div class='quota-detail-group'><h4>模型组折算消耗与可用额度上限</h4><div class='quota-detail-scroll'><table class='quota-detail-table quota-group-table'><thead><tr><th>模型组</th>" +
        quotaDimensions
          .map(function (dim) {
            return "<th>" + esc(dim.label) + "（已折算消耗 / 可用上限）</th>";
          })
          .join("") +
        "</tr></thead><tbody>" +
        groupRows +
        "</tbody></table></div></div>";
    } else if (!status.calibration_error && !status.job_level_missing) {
      html +=
        "<span style='color:var(--text-muted)'>" +
        (status.job_level ? "未加入模型组" : "暂无用量") +
        "</span>";
    }
    return "<div class='quota-detail'>" + html + "</div>";
  }
  async function loadQuotaEditorData() {
    var requests = [];
    requests.push(loadTargets(true));
    if (!state.catalogAllCache)
      requests.push(
        api("/admin/data/catalog").then(function (data) {
          state.catalogAllCache = Array.isArray(data.models) ? data.models : [];
        }),
      );
    await Promise.all(requests);
    await awaitSchedulingRefresh();
  }
  var quotaLoadVersion = 0;
  var dictLoadVersion = 0;
  var modelGroupNeedsRefresh = Object.create(null);
  async function loadQuotaManagement() {
    var version = ++quotaLoadVersion;
    ++dictLoadVersion;
    try {
      var usagePromise = api(
        "/admin/data/usage/aggregate?group_by=day&from=" +
          encodeURIComponent(
            new Date(Date.now() - 30 * 24 * 3600 * 1000).toISOString(),
          ) +
          "&to=" +
          encodeURIComponent(new Date().toISOString()),
        { timeoutMs: 60000 }, // 30-day aggregate scans many usage events
      ).then(
        function (data) {
          return data;
        },
        function () {
          return { by_client: [], unavailable: true };
        },
      );
      var results = await Promise.all([
        api("/admin/data/quota/summary"),
        api("/admin/data/model-groups"),
        api("/admin/data/job-levels"),
        api("/admin/data/project-groups"),
        loadQuotaEditorData(),
        usagePromise,
      ]);
      if (version !== quotaLoadVersion) return;
      if (Object.keys(modelGroupNeedsRefresh).length &&
          (!results[1] || !Object.prototype.hasOwnProperty.call(results[1], "model_groups") ||
            (results[1].model_groups != null &&
              (!Array.isArray(results[1].model_groups) || !results[1].model_groups.every(function (group) {
                return group && typeof group.name === "string" && !!group.name;
              }))))) throw new Error("模型组列表格式无效");
      ++dictLoadVersion;
      state.quotaSummary = results[0].clients || [];
      state.modelGroups = results[1].model_groups || [];
      modelGroupNeedsRefresh = Object.create(null);
      state.modelGroupsLoaded = true;
      refreshUsageModelGroupFilter();
      state.jobLevels = Array.isArray(results[2].job_levels)
        ? results[2].job_levels
        : [];
      state.projectGroups = Array.isArray(results[3].project_groups)
        ? results[3].project_groups
        : [];
      state.clientUsage30d = {};
      state.clientUsage30dUnavailable = !!(results[5] && results[5].unavailable);
      ((results[5] && results[5].by_client) || []).forEach(function (item) {
        state.clientUsage30d[item.key] = item;
      });
      state.dictsLoaded = true;
    } catch (e) {
      if (version !== quotaLoadVersion) return;
      document.getElementById("quotaSummaryBody").innerHTML =
        "<tr class='empty-row'><td colspan='" +
        (quotaDimensions.length + 7) +
        "'>加载失败: " +
        esc(e.message) +
        "</td></tr>";
      return;
    }
    try {
      renderQuotaSummaryFilters();
      renderQuotaSummary();
    } catch (e) {
      document.getElementById("quotaSummaryBody").innerHTML =
        "<tr class='empty-row'><td colspan='" +
        (quotaDimensions.length + 7) +
        "'>加载失败: " +
        esc(e.message) +
        "</td></tr>";
    }
    try {
      renderQuotaSyncClients();
    } catch (e) {
      document.getElementById("quotaSyncTargetsPanel").innerHTML =
        "<span style='color:var(--text-muted);padding:4px 6px;display:block;'>加载失败: " +
        esc(e.message) +
        "</span>";
    }
    try {
      renderModelGroups();
    } catch (e) {
      document.getElementById("modelGroupsBody").innerHTML =
        "<tr class='empty-row'><td colspan='5'>加载失败: " +
        esc(e.message) +
        "</td></tr>";
    }
    try {
      renderDictTables();
    } catch (e) {
      document.getElementById("jobLevelsBody").innerHTML =
        "<tr class='empty-row'><td colspan='" +
        (quotaDimensions.length + 2) +
        "'>加载失败: " +
        esc(e.message) +
        "</td></tr>";
      document.getElementById("projectGroupsBody").innerHTML =
        "<tr class='empty-row'><td colspan='2'>加载失败: " +
        esc(e.message) +
        "</td></tr>";
    }
  }
  function renderModelGroups() {
    document.getElementById("modelGroupsBody").innerHTML = state.modelGroups
      .length
      ? state.modelGroups
          .map(function (group, index) {
            return (
              "<tr><td><strong>" +
              esc(modelGroupLabel(group.name)) +
              (modelGroupNeedsRefresh[group.name] ? "（已过期，请刷新）" : "") +
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
  var dictKinds = {
    job: { label: "职级", path: "job-levels", empty: "（未设置）" },
    project: { label: "项目组", path: "project-groups", empty: "（未设置）" },
  };
  function dictEmptyLabel(kind) {
    return dictKinds[kind].empty;
  }
  async function loadDictOptions() {
    if (state.dictsLoaded) return;
    var version = dictLoadVersion;
    var results = await Promise.all([
      api("/admin/data/job-levels"),
      api("/admin/data/project-groups"),
    ]);
    if (version !== dictLoadVersion) {
      if (!state.dictsLoaded) return loadDictOptions();
      return;
    }
    state.jobLevels = Array.isArray(results[0].job_levels)
      ? results[0].job_levels
      : [];
    state.projectGroups = Array.isArray(results[1].project_groups)
      ? results[1].project_groups
      : [];
    state.dictsLoaded = true;
  }
  function dictOptionsHTML(entries, current, emptyLabel) {
    var values = {};
    (entries || []).forEach(function (v) {
      var name = jobLevelEntryName(v);
      if (name) values[name] = true;
    });
    if (current && !values[current]) values[current] = true;
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
            (v === current ? " selected" : "") +
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
  document
    .getElementById("saveJobLevelBtn")
    .addEventListener("click", async function () {
      if (isReadonlyMode()) return;
      var name = document.getElementById("jobLevelName").value.trim();
      if (!name) {
        toast("职级名称不能为空", "error");
        return;
      }
      var payload = { name: name };
      var bad = false;
      quotaDimensions.forEach(function (dim) {
        var raw = document.getElementById("jobLevel-" + dim.key).value;
        if (raw === "" || raw == null) return;
        var v = parseFloat(raw);
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
  document
    .getElementById("saveDictEntryBtn")
    .addEventListener("click", async function () {
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
    if (group && modelGroupNeedsRefresh[group.name]) {
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
    if (group && (modelGroupNeedsRefresh[group.name] || (modelGroupSaving &&
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
      if (state.editingModelGroup && modelGroupNeedsRefresh[state.editingModelGroup.name] &&
          !document.getElementById("modelGroupModal").classList.contains("open")) {
        toast("模型组已保存但列表尚未刷新，请刷新后重新编辑。", "error");
        return;
      }
      if (state.editingModelGroup && editingModelGroupVersion !== (modelGroupVersions[state.editingModelGroup.name] || 0)) {
        toast("模型组已更新，请保留草稿并重新打开确认。", "error");
        return;
      }
      var name = document.getElementById("modelGroupName").value.trim(),
        coefficient = parseFloat(
          document.getElementById("modelGroupCoefficient").value,
        );
      if (!name || !Number.isFinite(coefficient) || coefficient < 0) {
        toast("请填写有效的模型组标识和扣费折算系数（系数须 ≥0）", "error");
        return;
      }
      var payload = {
        name: name,
        cost_coefficient: coefficient,
        bindings: collectModelGroupBindings(),
      };
      var bad = false;
      quotaDimensions.forEach(function (dim) {
        var raw = document.getElementById("modelGroup-" + dim.key).value.trim();
        var value = raw === "" ? 0 : Number(raw);
        if (!Number.isFinite(value) || value < 0) {
          bad = true;
          return;
        }
        payload[dim.base_field] = value;
      });
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
        ++quotaLoadVersion;
        if (current) modelGroupNeedsRefresh[current.name] = true;
        modelGroupNeedsRefresh[name] = true;
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
  function quotaRowStatus(item) {
    var s = item.status || {},
      groups = s.groups || {};
    var c = item.client || {};
    if (s.calibration_error || (s.exceeded && s.exceeded.scope === "accounting"))
      return { key: "accounting", label: "计费校准不可用", badge: "warn" };
    if (
      !c.deleted &&
      (s.job_level_missing || (s.exceeded && s.exceeded.scope === "job_level"))
    )
      return { key: "joblevel", label: "职级未配置", badge: "over" };
    if (
      Object.keys(groups).some(function (name) {
        return groups[name].exceeded;
      })
    )
      return { key: "group", label: "模型组超限", badge: "over" };
    var configured = false,
      near = false;
    function scan(q) {
      if (q.limit > 0) {
        configured = true;
        if ((q.used || 0) / q.limit >= 0.9) near = true;
      }
    }
    Object.keys(groups).forEach(function (name) {
      quotaDimensions.forEach(function (dim) {
        scan((groups[name].quotas || {})[dim.key] || {});
      });
    });
    if (near) return { key: "warn", label: "接近限额", badge: "warn" };
    if (configured) return { key: "ok", label: "正常", badge: "ok" };
    if (
      Object.keys(groups).some(function (name) {
        return groups[name].in_plan;
      })
    )
      return { key: "unlimited", label: "不限", badge: "none" };
    return { key: "none", label: "未配置", badge: "none" };
  }
  function quotaBarCell(q, unavailable, dimLabel) {
    var prefix = dimLabel ? dimLabel + " " : "";
    if (unavailable) {
      var unavailableText = prefix + unavailable;
      if (unavailable === "-")
        return (
          "<div class='quota-cell'><span class='quota-pct'>-</span></div>"
        );
      return (
        "<div class='quota-cell' role='group' aria-label='" +
        esc(unavailableText) +
        "'><div class='quota-bar-wrap'><div class='quota-bar' aria-hidden='true'><div class='quota-bar-inner q-none' style='width:0%'></div></div></div><div class='quota-pct'>" +
        esc(unavailable) +
        "</div></div>"
      );
    }
    q = q || {};
    var limit = Number(q.limit || 0),
      used = Number(q.used || 0);
    if (!isFinite(limit) || limit <= 0) {
      return (
        "<div class='quota-cell' role='group' aria-label='" +
        esc(prefix + "不限（无正数上限）") +
        "'><div class='quota-bar-wrap'><div class='quota-bar' aria-hidden='true'><div class='quota-bar-inner q-none' style='width:100%'></div></div></div><div class='quota-pct'>不限</div></div>"
      );
    }
    if (!isFinite(used) || used < 0) used = 0;
    var pct = Math.min(100, Math.round((used / limit) * 100));
    var cls = pct >= 90 ? "q-over" : pct >= 70 ? "q-warn" : "q-ok";
    var stateText =
      prefix +
      "已用 " +
      fmtMoney(used) +
      "，上限 " +
      fmtMoney(limit) +
      "，" +
      pct +
      "%" +
      (used > limit ? "，超出上限" : "");
    return (
      "<div class='quota-cell'><div class='quota-bar-wrap'><div class='quota-bar' role='progressbar' aria-valuemin='0' aria-valuemax='100' aria-valuenow='" +
      pct +
      "' aria-label='" +
      esc(stateText) +
      "'><div class='quota-bar-inner " +
      cls +
      "' style='width:" +
      pct +
      "%'></div></div></div><div class='quota-pct'>" +
      fmtMoney(used) +
      " / " +
      fmtMoney(limit) +
      "</div></div>"
    );
  }
  function quotaGroupSummary(groups) {
    var names = Object.keys(groups || {});
    if (!names.length)
      return "<span style='color:var(--text-muted);'>未配置</span>";
    var exceeded = names.filter(function (name) {
      return groups[name].exceeded;
    }).length;
    return (
      "<span>" +
      names.length +
      " 个模型组" +
      (exceeded
        ? " · <span class='quota-badge over'>" + exceeded + " 超限</span>"
        : "") +
      "</span>"
    );
  }
  function renderQuotaSummaryFilters() {
    [
      [
        "quotaSummaryProjectFilter",
        "project_group",
        "projectGroups",
        "全部项目组",
      ],
      ["quotaSummaryJobFilter", "job_level", "jobLevels", "全部职级"],
    ].forEach(function (spec) {
      var select = document.getElementById(spec[0]),
        current = select.value,
        values = {};
      (state[spec[2]] || []).forEach(function (v) {
        var name = jobLevelEntryName(v);
        if (name) values[name] = true;
      });
      (state.quotaSummary || []).forEach(function (item) {
        var v = (item.client || {})[spec[1]];
        if (v) values[v] = true;
      });
      select.innerHTML =
        "<option value=''>" +
        spec[3] +
        "</option>" +
        Object.keys(values)
          .sort()
          .map(function (v) {
            return "<option value='" + esc(v) + "'>" + esc(v) + "</option>";
          })
          .join("");
      if (values[current]) select.value = current;
    });
  }
  function quotaTotalTok(r) {
    return totalTok(r);
  }
  var quotaTokenFields = [
    "input_tokens",
    "output_tokens",
    "cached_tokens",
    "cache_creation_tokens",
    "tool_use_prompt_tokens",
  ];
  function quotaFiniteNumber(value) {
    return typeof value === "number" && isFinite(value) ? value : null;
  }
  function quotaUsageStats(name) {
    if (state.clientUsage30dUnavailable)
      return {
        tokens: null,
        cost: null,
        unavailable: true,
        usageIncomplete: 0,
        costIncomplete: 0,
      };
    var r = (state.clientUsage30d || {})[name];
    if (!r)
      return {
        tokens: 0,
        cost: 0,
        unavailable: false,
        usageIncomplete: 0,
        costIncomplete: 0,
      };
    var hasTokens = quotaTokenFields.some(function (field) {
      return quotaFiniteNumber(r[field]) !== null;
    });
    return {
      tokens: hasTokens ? quotaTotalTok(r) : null,
      cost: quotaFiniteNumber(r.estimated_cost),
      unavailable: false,
      usageIncomplete: Number(r.incomplete_usage_count || 0),
      costIncomplete: Number(r.incomplete_cost_count || 0),
    };
  }
  function quotaTokenCell(stats) {
    if (stats.tokens === null)
      return (
        "<span class='quota-usage-unavailable'>不可用</span>" +
        (stats.usageIncomplete
          ? "<small class='quota-usage-note'>用量不完整 " +
            stats.usageIncomplete +
            " 条记录</small>"
          : "")
      );
    var html = fmtNum(stats.tokens);
    if (stats.usageIncomplete)
      html +=
        "<small class='quota-usage-note'>用量不完整 " +
        stats.usageIncomplete +
        " 条记录</small>";
    return html;
  }
  function quotaCostCell(stats) {
    if (stats.cost === null) {
      return (
        "<span class='quota-usage-unavailable'>不可用</span><small class='quota-cost-note'>" +
        (stats.unavailable
          ? "30 天用量加载失败"
          : "聚合接口未返回 estimated_cost") +
        "</small>" +
        (stats.costIncomplete
          ? "<small class='quota-cost-note'>费用不完整 " +
            stats.costIncomplete +
            " 条记录</small>"
          : "")
      );
    }
    var html = fmtMoney(stats.cost);
    if (stats.costIncomplete)
      html +=
        "<small class='quota-cost-note'>费用不完整 " +
        stats.costIncomplete +
        " 条记录</small>";
    return html;
  }
  function quotaSummaryScopedRows() {
    var search = (document.getElementById("quotaSearch").value || "")
      .trim()
      .toLowerCase();
    var project = document.getElementById("quotaSummaryProjectFilter").value;
    var job = document.getElementById("quotaSummaryJobFilter").value;
    return (state.quotaSummary || [])
      .map(function (item) {
        return { item: item, status: quotaRowStatus(item) };
      })
      .filter(function (entry) {
        var c = entry.item.client || {};
        if (
          search &&
          !(
            (c.name || "").toLowerCase().indexOf(search) >= 0 ||
            (c.ip_address || "").toLowerCase().indexOf(search) >= 0
          )
        )
          return false;
        if (project && (c.project_group || "未分组") !== project) return false;
        if (job && c.job_level !== job) return false;
        return true;
      });
  }
  function renderQuotaStatusCounts(rows) {
    var select = document.getElementById("quotaSummaryStatusFilter");
    var options = Array.prototype.slice.call(select.options || []);
    if (!options.length) return;
    var counts = {};
    rows.forEach(function (entry) {
      counts[entry.status.key] = (counts[entry.status.key] || 0) + 1;
    });
    options.forEach(function (option) {
      var base = String(option.textContent || "").replace(/\s*\(\d+\)$/, "");
      var count = option.value ? counts[option.value] || 0 : rows.length;
      option.textContent = base + " (" + count + ")";
    });
  }
  function renderQuotaSummary() {
    var statusFilter = document.getElementById(
      "quotaSummaryStatusFilter",
    ).value;
    var scoped = quotaSummaryScopedRows();
    renderQuotaStatusCounts(scoped);
    var rows = scoped
      .filter(function (entry) {
        return !statusFilter || entry.status.key === statusFilter;
      })
      .sort(function (a, b) {
        return String((a.item.client || {}).name || "").localeCompare(
          String((b.item.client || {}).name || ""),
        );
      });
    document.getElementById("quotaSummaryBody").innerHTML = rows.length
      ? rows
          .map(function (entry) {
            var item = entry.item,
              c = item.client || {},
              s = item.status || {},
              groups = s.groups || {},
              st = entry.status;
            var expanded = !!state.quotaExpanded[c.name];
            var detailId = "quota-detail-" + encodeURIComponent(c.name);
            var usage30 = quotaUsageStats(c.name);
            var deletedBadge = c.deleted
              ? " <span class='quota-badge del'>已删除</span>"
              : "";
            var jobLabel = c.job_level
              ? esc(c.job_level)
              : c.deleted
                ? "-"
                : "<span class='quota-badge warn'>未绑职级</span>";
            var personCell =
              "<strong>" +
              esc(c.name) +
              "</strong>" +
              deletedBadge +
              "<br><span style='font-size:12px;color:var(--text-muted);'>" +
              esc(c.ip_address || "-") +
              " · " +
              jobLabel +
              " · " +
              esc(c.project_group || "未分组") +
              "</span>";
            var quotaUnavailable = c.deleted
              ? "-"
              : st.key === "accounting"
                ? "校准不可用"
                : st.key === "joblevel"
                ? "职级未配置"
                : st.key === "none"
                  ? "未配置"
                  : "";
            var actions = c.deleted
              ? "<div class='btn-group'><button class='btn btn-sm btn-danger readonly-hide' data-purge-quota='" +
                esc(c.name) +
                "'>清除记录</button></div>"
              : "<div class='btn-group'><button class='btn btn-sm readonly-hide' data-edit-quota='" +
                esc(c.name) +
                "'>编辑额度</button></div>";
            var row =
              "<tr data-quota-person='" +
              esc(c.name) +
              "'>" +
              "<td><button class='quota-expand-btn" +
              (expanded ? " open" : "") +
              "' data-expand-quota='" +
              esc(c.name) +
              "' aria-label='展开/收起明细' aria-expanded='" +
              (expanded ? "true" : "false") +
              "' aria-controls='" +
              esc(detailId) +
              "' title='展开/收起明细'>" +
              (expanded ? "▼" : "▶") +
              "</button></td>" +
              "<td class='quota-person-cell'>" +
              personCell +
              "</td>" +
              quotaDimensions
                .map(function (dim) {
                  return (
                    "<td>" +
                    quotaBarCell(
                      (s.quotas || {})[dim.key],
                      quotaUnavailable,
                      dim.label,
                    ) +
                    "</td>"
                  );
                })
                .join("") +
              "<td class='quota-number quota-token-cell'>" +
              quotaTokenCell(usage30) +
              "</td>" +
              "<td class='quota-number quota-cost-cell'>" +
              quotaCostCell(usage30) +
              "</td>" +
              "<td>" +
              quotaGroupSummary(groups) +
              "</td>" +
              "<td><span class='quota-badge " +
              st.badge +
              "'>" +
              st.label +
              "</span></td>" +
              "<td>" +
              actions +
              "</td></tr>";
            if (expanded)
              row +=
                "<tr class='quota-detail-row' id='" +
                esc(detailId) +
                "' data-quota-detail='" +
                esc(c.name) +
                "'><td colspan='" +
                (quotaDimensions.length + 7) +
                "'>" +
                quotaConvertedDetail(s) +
                "</td></tr>";
            return row;
          })
          .join("")
      : "<tr class='empty-row'><td colspan='" +
        (quotaDimensions.length + 7) +
        "'>暂无客户端</td></tr>";
  }
  document
    .getElementById("quotaSearch")
    .addEventListener("input", renderQuotaSummary);
  document
    .getElementById("quotaSummaryProjectFilter")
    .addEventListener("change", renderQuotaSummary);
  document
    .getElementById("quotaSummaryJobFilter")
    .addEventListener("change", renderQuotaSummary);
  document
    .getElementById("quotaSummaryStatusFilter")
    .addEventListener("change", renderQuotaSummary);
  document
    .getElementById("quotaSummaryBody")
    .addEventListener("click", function (e) {
      var expandBtn = e.target.closest("[data-expand-quota]");
      if (expandBtn) {
        var name = expandBtn.dataset.expandQuota;
        state.quotaExpanded = state.quotaExpanded || {};
        state.quotaExpanded[name] = !state.quotaExpanded[name];
        renderQuotaSummary();
        return;
      }
      var editBtn = e.target.closest("[data-edit-quota]");
      if (editBtn) {
        var item = (state.quotaSummary || []).find(function (x) {
          return (x.client || {}).name === editBtn.dataset.editQuota;
        });
        if (item) openQuotaEditDrawer(item);
      }
      var purgeBtn = e.target.closest("[data-purge-quota]");
      if (purgeBtn) {
        if (isReadonlyMode()) return;
        var name = purgeBtn.dataset.purgeQuota;
        showConfirm(
          "确认清除已删除客户端「" +
            name +
            "」的额度记录？清除后月度统计中不再显示该行。",
          async function () {
            try {
              await api("/admin/data/quota/" + encodeURIComponent(name), {
                method: "DELETE",
              });
              toast("记录已清除", "success");
              await loadQuotaManagement();
            } catch (err) {
              toast("清除失败: " + err.message, "error");
            }
          },
        );
      }
    });
  document
    .getElementById("refreshQuotaBtn")
    .addEventListener("click", loadQuotaManagement);

  /* ===== Quota Tab Switching ===== */
  document.querySelectorAll(".quota-tab").forEach(function (tab) {
    tab.addEventListener("click", function () {
      document.querySelectorAll(".quota-tab").forEach(function (t) {
        t.classList.remove("active");
      });
      document.querySelectorAll(".quota-tab-panel").forEach(function (p) {
        p.classList.remove("active");
      });
      tab.classList.add("active");
      var panel = document.getElementById("quota-tab-" + tab.dataset.quotaTab);
      if (panel) panel.classList.add("active");
    });
  });

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
  document
    .getElementById("saveQuotaEditBtn")
    .addEventListener("click", async function () {
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
    });

  function renderQuotaSyncClients() {
    var source = document.getElementById("quotaSyncSource"),
      current = source.value;
    var clients = (state.quotaSummary || [])
      .map(function (item) {
        return item.client || {};
      })
      .filter(function (c) {
        return !c.deleted;
      })
      .sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      });
    source.innerHTML = clients
      .map(function (c) {
        return (
          "<option value='" +
          esc(c.name) +
          "'>" +
          esc(c.name + (c.ip_address ? " (" + c.ip_address + ")" : "")) +
          "</option>"
        );
      })
      .join("");
    if (
      clients.some(function (c) {
        return c.name === current;
      })
    )
      source.value = current;
    var groupNames = [];
    var seenGroups = {};
    clients.forEach(function (c) {
      var g = c.project_group || "未分组";
      if (!seenGroups[g]) {
        seenGroups[g] = true;
        groupNames.push(g);
      }
    });
    groupNames.sort();
    var filter = document.getElementById("quotaSyncProjectFilter"),
      filterCurrent = filter.value;
    filter.innerHTML =
      "<option value=''>全部项目组</option>" +
      groupNames
        .map(function (g) {
          return "<option value='" + esc(g) + "'>" + esc(g) + "</option>";
        })
        .join("");
    if (groupNames.indexOf(filterCurrent) >= 0) filter.value = filterCurrent;
    // 「到整个项目组」目标下拉（含未分组）。
    var projectTarget = document.getElementById("quotaSyncProjectTarget"),
      projectCurrent = projectTarget.value;
    projectTarget.innerHTML = groupNames
      .map(function (g) {
        return "<option value='" + esc(g) + "'>" + esc(g) + "</option>";
      })
      .join("");
    if (groupNames.indexOf(projectCurrent) >= 0)
      projectTarget.value = projectCurrent;
    // 「到整个职级」目标下拉。
    var jobTarget = document.getElementById("quotaSyncJobTarget"),
      jobCurrent = jobTarget.value;
    var levelNames = (state.jobLevels || [])
      .map(jobLevelEntryName)
      .filter(Boolean)
      .sort();
    jobTarget.innerHTML = levelNames.length
      ? levelNames
          .map(function (g) {
            return "<option value='" + esc(g) + "'>" + esc(g) + "</option>";
          })
          .join("")
      : "<option value=''>（暂无职级）</option>";
    if (levelNames.indexOf(jobCurrent) >= 0) jobTarget.value = jobCurrent;
    renderQuotaSyncTargets();
    applyQuotaSyncMode();
  }
  // applyQuotaSyncMode 按同步范围切换目标选择控件的可见性。
  function applyQuotaSyncMode() {
    var mode = document.getElementById("quotaSyncMode").value;
    document.getElementById("quotaSyncClientsPicker").style.display =
      mode === "clients" ? "" : "none";
    document.getElementById("quotaSyncProjectFilterWrap").style.display =
      mode === "clients" ? "" : "none";
    document.getElementById("quotaSyncProjectTargetWrap").style.display =
      mode === "project_group" ? "" : "none";
    document.getElementById("quotaSyncJobTargetWrap").style.display =
      mode === "job_level" ? "" : "none";
  }
  document
    .getElementById("quotaSyncMode")
    .addEventListener("change", applyQuotaSyncMode);
  function updateQuotaSyncTargetsLabel() {
    var count = document.querySelectorAll(
      "#quotaSyncTargetsPanel input:checked",
    ).length;
    document.getElementById("quotaSyncTargetsLabel").textContent = count
      ? "已选 " + count + " 个客户端"
      : "未选择";
  }
  function renderQuotaSyncTargets() {
    var source = document.getElementById("quotaSyncSource").value;
    var groupFilter = document.getElementById("quotaSyncProjectFilter").value;
    var panel = document.getElementById("quotaSyncTargetsPanel");
    var selected = Array.from(panel.querySelectorAll("input:checked")).map(
      function (input) {
        return input.value;
      },
    );
    var clients = (state.quotaSummary || [])
      .map(function (item) {
        return item.client || {};
      })
      .filter(function (c) {
        if (c.deleted) return false;
        if (c.name === source) return false;
        if (groupFilter && (c.project_group || "未分组") !== groupFilter)
          return false;
        return true;
      })
      .sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      });
    panel.innerHTML = clients.length
      ? clients
          .map(function (c) {
            return (
              "<label class='quota-sync-option'><input type='checkbox' value='" +
              esc(c.name) +
              "'> <span>" +
              esc(c.name) +
              "</span><span class='quota-sync-sub'>" +
              (c.ip_address ? esc(c.ip_address) + " · " : "") +
              esc(c.project_group || "未分组") +
              "</span></label>"
            );
          })
          .join("")
      : "<span style='color:var(--text-muted);padding:4px 6px;display:block;'>没有其他客户端</span>";
    selected.forEach(function (name) {
      var inputs = panel.querySelectorAll("input");
      for (var i = 0; i < inputs.length; i++) {
        if (inputs[i].value === name) {
          inputs[i].checked = true;
          break;
        }
      }
    });
    updateQuotaSyncTargetsLabel();
  }
  document
    .getElementById("quotaSyncSource")
    .addEventListener("change", renderQuotaSyncTargets);
  document
    .getElementById("quotaSyncProjectFilter")
    .addEventListener("change", renderQuotaSyncTargets);
  document
    .getElementById("quotaSyncTargetsBtn")
    .addEventListener("click", function (e) {
      e.stopPropagation();
      document.getElementById("quotaSyncTargetsPanel").classList.toggle("open");
    });
  document
    .getElementById("quotaSyncTargetsPanel")
    .addEventListener("change", updateQuotaSyncTargetsLabel);
  document.addEventListener("click", function (e) {
    var picker = e.target.closest(".quota-target-picker");
    if (!picker)
      document.getElementById("quotaSyncTargetsPanel").classList.remove("open");
  });
  document
    .getElementById("syncQuotaBtn")
    .addEventListener("click", async function () {
      if (isReadonlyMode()) return;
      var source = document.getElementById("quotaSyncSource").value;
      if (!source) {
        toast("请选择源客户端", "error");
        return;
      }
      var mode = document.getElementById("quotaSyncMode").value;
      var body = {},
        describe = "";
      if (mode === "clients") {
        var clients = Array.from(
          document.querySelectorAll("#quotaSyncTargetsPanel input:checked"),
        ).map(function (input) {
          return input.value;
        });
        if (!clients.length) {
          toast("请勾选至少一个目标客户端", "error");
          return;
        }
        body = { clients: clients };
        describe = clients.length + " 个所选客户端";
      } else if (mode === "project_group") {
        var projectGroup = document.getElementById(
          "quotaSyncProjectTarget",
        ).value;
        if (!projectGroup) {
          toast("请选择目标项目组", "error");
          return;
        }
        body = { project_group: projectGroup };
        describe = "项目组「" + projectGroup + "」的全部客户端";
      } else {
        var jobLevel = document.getElementById("quotaSyncJobTarget").value;
        if (!jobLevel) {
          toast("请选择目标职级", "error");
          return;
        }
        body = { job_level: jobLevel };
        describe = "职级「" + jobLevel + "」的全部客户端";
      }
      showConfirm(
        "将「" +
          source +
          "」的模型组选择与个人可用额度倍率同步到 " +
          describe +
          "，覆盖其现有配置（不修改职级绑定，不影响身份、密钥与授权）。确认继续？",
        async function () {
          try {
            var res = await api(
              "/admin/data/quota/" + encodeURIComponent(source) + "/sync",
              // One write transaction per synced client; large job levels take longer.
              { method: "POST", body: body, timeoutMs: 60000 },
            );
            var synced = (res && res.synced_clients) || [];
            toast(
              synced.length
                ? "方案已同步到 " + synced.length + " 个客户端"
                : "没有匹配的同步目标",
              synced.length ? "success" : "error",
            );
            await loadQuotaManagement();
          } catch (e) {
            toast("同步失败: " + e.message, "error");
          }
        },
      );
    });

  function renderQuotaDimensionControls() {
    [
      ["jobLevelDimensions", "jobLevel", "可用额度倍率"],
      ["modelGroupDimensions", "modelGroup", "额度基数 ($)"],
    ].forEach(function (spec) {
      var container = document.getElementById(spec[0]);
      container.innerHTML = quotaDimensions
        .map(function (dim) {
          return (
            "<div class='form-group' style='margin:0;'><label style='font-size:12px;'>" +
            esc(dim.label + spec[2]) +
            "</label><input class='form-control readonly-disable' id='" +
            esc(spec[1] + "-" + dim.key) +
            "' type='number' min='0' step='0.01' placeholder='" +
            (spec[1] === "jobLevel" ? "留空 = 1" : "留空 = 0（已绑定职级时该周期上限不限）") +
            "'></div>"
          );
        })
        .join("");
    });
    document.getElementById("quotaSummaryTableHead").innerHTML =
      "<tr><th></th><th>客户端</th>" +
      quotaDimensions
        .map(function (dim) {
          return "<th>" + esc(dim.label) + "已用 / 可用</th>";
        })
        .join("") +
      "<th title='普通输入 + 输出 + 缓存读取 + 缓存写入 + 工具输入；缺失显示不可用'>近30天 Token</th>" +
      "<th title='聚合接口 estimated_cost 已含模型组扣费系数，前端不再次折算；返回 0 显示 $0.00，缺失显示不可用'>估算费用（含模型组系数）</th>" +
      "<th title='各模型组额度独立生效，不是共享额度池'>模型组</th><th>状态</th><th>操作</th></tr>";
    document.getElementById("jobLevelTableHead").innerHTML =
      "<tr><th>职级</th>" +
      quotaDimensions
        .map(function (dim) {
          return "<th>" + esc(dim.label) + "额度倍率</th>";
        })
        .join("") +
      "<th>操作</th></tr>";
  }
  renderQuotaDimensionControls();
  window.modelGroupModels = modelGroupModels;
  window.modelGroupLabel = modelGroupLabel;
  window.loadQuotaManagement = loadQuotaManagement;
  return {
    modelGroupModels: modelGroupModels,
    modelGroupLabel: modelGroupLabel,
    renderQuotaDimensionControls: renderQuotaDimensionControls,
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
    quotaConvertedDetail: quotaConvertedDetail,
    loadQuotaEditorData: loadQuotaEditorData,
    loadQuotaManagement: loadQuotaManagement,
    renderModelGroups: renderModelGroups,
    dictEmptyLabel: dictEmptyLabel,
    loadDictOptions: loadDictOptions,
    dictOptionsHTML: dictOptionsHTML,
    renderDictTables: renderDictTables,
    openJobLevelModal: openJobLevelModal,
    openDictModal: openDictModal,
    renderModelGroupBindings: renderModelGroupBindings,
    collectModelGroupBindings: collectModelGroupBindings,
    openModelGroupModal: openModelGroupModal,
    quotaRowStatus: quotaRowStatus,
    quotaResetText: quotaResetText,
    quotaBarCell: quotaBarCell,
    quotaGroupSummary: quotaGroupSummary,
    renderQuotaSummaryFilters: renderQuotaSummaryFilters,
    quotaTotalTok: quotaTotalTok,
    renderQuotaSummary: renderQuotaSummary,
    openQuotaEditDrawer: openQuotaEditDrawer,
    renderQuotaEditGroups: renderQuotaEditGroups,
    quotaEffectivePreview: quotaEffectivePreview,
    syncQuotaEditGroups: syncQuotaEditGroups,
    renderQuotaSyncClients: renderQuotaSyncClients,
    applyQuotaSyncMode: applyQuotaSyncMode,
    updateQuotaSyncTargetsLabel: updateQuotaSyncTargetsLabel,
    renderQuotaSyncTargets: renderQuotaSyncTargets,
  };
});
