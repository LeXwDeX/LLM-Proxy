"use strict";
// Quota sync: copy one client's plan to clients, a project group or a job level.
AdminApp.register("quotaSync", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var showConfirm = app.bind("core", "showConfirm");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var jobLevelEntryName = app.bind("quotaModel", "jobLevelEntryName");
  var loadQuotaManagement = app.bind("quotaData", "loadQuotaManagement");
  var runGuarded = app.bind("core", "runGuarded");
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
        function () {
          return runGuarded("syncQuota", document.getElementById("syncQuotaBtn"), async function () {
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
          });
        },
      );
    });
  return {
    renderQuotaSyncClients: renderQuotaSyncClients,
    applyQuotaSyncMode: applyQuotaSyncMode,
    updateQuotaSyncTargetsLabel: updateQuotaSyncTargetsLabel,
    renderQuotaSyncTargets: renderQuotaSyncTargets,
  };
});
