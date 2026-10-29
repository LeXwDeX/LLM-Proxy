"use strict";
// Clients owns client CRUD and target/pool permission pickers.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("clients", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var maskKey = app.bind("core", "maskKey");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var epShortLabel = app.bind("schema", "epShortLabel");
  var poolMemberCounter = app.bind("pools", "poolMemberCounter");
  var poolDisplayName = app.bind("pools", "poolDisplayName");
  var loadTargets = app.bind("targets", "loadTargets");
  var awaitSchedulingRefresh = app.bind("targets", "awaitSchedulingRefresh");
  var targetPoolsAvailable = app.bind("targets", "targetPoolsAvailable");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var loadDictOptions = app.bind("quota", "loadDictOptions");
  var dictEmptyLabel = app.bind("quota", "dictEmptyLabel");
  var dictOptionsHTML = app.bind("quota", "dictOptionsHTML");
  var clientLoadVersion = 0;
  var clientOpenVersion = 0;
  var clientDraftRevision = 0;
  var clientSaveInFlight = false;
  var clientCreatedDraftNeedsReopen = false;
  var clientSavingName = "";
  var clientVersions = Object.create(null);
  var editingClientVersion = 0;
  ["input", "change"].forEach(function (eventName) {
    document.getElementById("clientModal").addEventListener(eventName, function () { clientDraftRevision++; });
  });
  document.getElementById("clientModal").addEventListener("click", function (event) {
    if (event.target.dataset.target || event.target.dataset.pool) clientDraftRevision++;
  });
  /* ===== Clients ===== */
  async function loadClients() {
    var version = ++clientLoadVersion;
    try {
      var jobs = [
        api("/admin/data/clients").then(function (r) {
          return { key: "clients", data: r };
        }),
        api(
          "/admin/data/usage/aggregate?group_by=day&from=" +
            encodeURIComponent(
              new Date(Date.now() - 30 * 24 * 3600 * 1000).toISOString(),
            ) +
            "&to=" +
            encodeURIComponent(new Date().toISOString()),
          { timeoutMs: 60000 }, // 30-day aggregate scans many usage events
        ).then(function (r) {
          return { key: "agg", data: r };
        }),
      ];
      // Targets feed the access pickers only. Prefetch when none are loaded, and
      // never let that side request fail the clients list itself.
      if (!state.targets || !state.targets.length) {
        jobs.push(loadTargets(true).catch(function () { return null; }));
      }
      var results = await Promise.all(jobs);
      if (version !== clientLoadVersion) return;
      var byKey = {};
      results.forEach(function (x) {
        if (x && x.key) byKey[x.key] = x.data;
      });
      state.clients = Array.isArray(byKey.clients.clients)
        ? byKey.clients.clients
        : [];
      // Build per-client usage map from by_client breakdown
      state.clientUsage = {};
      var byClient = (byKey.agg && byKey.agg.by_client) || [];
      byClient.forEach(function (item) {
        state.clientUsage[item.key] = item;
      });
      renderClients();
    } catch (e) {
      if (version !== clientLoadVersion) return;
      toast("加载客户端失败: " + e.message, "error");
    }
  }
  function renderClients() {
    var tb = document.getElementById("clientsTableBody");
    if (!state.clients.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='8'>暂无客户端</td></tr>";
      return;
    }
    tb.innerHTML = state.clients
      .slice()
      .sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      })
      .map(function (c) {
        var hasTargets =
          Array.isArray(c.allowed_targets) && c.allowed_targets.length > 0;
        var hasPools =
          Array.isArray(c.allowed_pools) && c.allowed_pools.length > 0;
        var targets;
        if (hasTargets || hasPools) {
          var parts = [];
          // Direct targets and pools on separate lines, direct first.
          if (hasTargets) {
            parts.push(
              "<div class='client-target-line client-target-direct'>" +
                c.allowed_targets
                  .map(function (target) {
                    return "<span class='client-target-direct-tag'>" + esc(target) + "</span>";
                  })
                  .join("") +
                "</div>",
            );
          }
          if (hasPools) {
            parts.push(
              "<div class='client-target-line client-target-pool'>" +
                c.allowed_pools
                  .map(function (pn) {
                    return "<span class='client-target-pool-tag'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='1.8' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='3' y='4' width='18' height='7' rx='2'></rect><rect x='3' y='13' width='18' height='7' rx='2'></rect><path d='M7 7.5h.01M7 16.5h.01'></path></svg> " + esc(poolDisplayName(pn)) + (targetPoolsAvailable() ? "" : "（部署池信息不可用）") + "</span>";
                  })
                  .join("") +
                "</div>",
            );
          }
          targets = "<div class='client-target-list'>" + parts.join("") + "</div>";
        } else {
          targets = "全部目标";
        }
        var usage = (state.clientUsage && state.clientUsage[c.name]) || {};
        var tok = totalTok(usage);
        var cost = Number(usage.estimated_cost || 0);
        // 额度 v2：客户端额度入口 = 职级绑定（上限由策略基数 × 职级系数
        // × 个人系数推导，模型原价按组系数折算已用，明细在额度管理页）。
        var quotaLabel = c.job_level
          ? "职级: " + esc(c.job_level)
          : "<span class='quota-badge warn'>未配置职级</span>";
        return (
          "<tr><td>" +
          esc(c.name) +
          "</td>" +
          "<td style='max-width:220px;overflow-wrap:anywhere;'>" +
          esc(c.ip_address || "-") +
          "</td>" +
          "<td><span class='key-mask'>" +
          esc(maskKey(c.access_key)) +
          "</span></td>" +
          "<td>" +
          targets +
          "</td>" +
          "<td>" +
          fmtNum(tok) +
          "</td>" +
          "<td>" +
          fmtMoney(cost) +
          "</td>" +
          "<td>" +
          quotaLabel +
          "</td>" +
          "<td><div class='btn-group'>" +
          "<button class='btn btn-sm readonly-hide' data-edit-client='" +
          esc(c.name) +
          "'>编辑</button>" +
          "<button class='btn btn-danger btn-sm readonly-hide' data-del-client='" +
          esc(c.name) +
          "'>删除</button>" +
          "</div></td></tr>"
        );
      })
      .join("");
  }
  document
    .getElementById("clientsTableBody")
    .addEventListener("click", function (e) {
      var t = e.target;
      if (t.dataset.editClient) {
        var c = state.clients.find(function (x) {
          return x.name === t.dataset.editClient;
        });
        if (c) openClientModal(c);
      }
      if (t.dataset.delClient) {
        var name = t.dataset.delClient;
        showConfirm(
          "确认删除客户端「" + name + "」？此操作不可恢复。",
          async function () {
            try {
              await api("/admin/data/clients/" + encodeURIComponent(name), {
                method: "DELETE",
              });
              toast("已删除客户端: " + name, "success");
              await loadClients();
            } catch (e) {
              toast("删除失败: " + e.message, "error");
            }
          },
        );
      }
    });
  // The client editor needs fresh targets and pools for its pickers, nothing
  // else. loadTargets(true) settles the refresh itself and rethrows its error.
  async function loadClientEditorData() {
    await loadTargets(true);
  }
  async function openClientModal(c) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    if (c && clientSaveInFlight && clientSavingName === c.name) {
      toast("该客户端正在保存，请等待完成后重新打开。", "error");
      return;
    }
    var clientVersion = c ? (clientVersions[c.name] || 0) : 0;
    var version = ++clientOpenVersion;
    try {
      await loadClientEditorData();
      await loadDictOptions();
      // A refresh that started while the dictionaries loaded must land before
      // the pickers are filled.
      await awaitSchedulingRefresh();
    } catch (e) {
      if (version !== clientOpenVersion) return;
      toast("加载客户端编辑数据失败: " + e.message, "error");
      return;
    }
    if (version !== clientOpenVersion) return;
    if (c && clientSaveInFlight && clientSavingName === c.name) {
      toast("该客户端正在保存，请等待完成后重新打开。", "error");
      return;
    }
    if (c && (clientVersions[c.name] || 0) !== clientVersion) {
      toast("客户端已更新，请重新打开以确认最新授权。", "error");
      return;
    }
    clientCreatedDraftNeedsReopen = false;
    editingClientVersion = clientVersion;
    state.editingClient = c || null;
    document.getElementById("clientModalTitle").textContent = c
      ? "编辑客户端"
      : "新增客户端";
    document.getElementById("clientName").value = c ? c.name : "";
    document.getElementById("clientName").readOnly = !!c;
    document.getElementById("clientAccessKey").value = c ? c.access_key : "";
    document.getElementById("clientIPAddress").value =
      c && c.ip_address ? c.ip_address : "";
    renderClientTargetTags(c && c.allowed_targets ? c.allowed_targets : []);
    renderClientPoolTags(c && c.allowed_pools ? c.allowed_pools : []);
    document.getElementById("clientJobLevel").innerHTML = dictOptionsHTML(
      state.jobLevels,
      c && c.job_level ? c.job_level : "",
      dictEmptyLabel("job"),
    );
    document.getElementById("clientProjectGroup").innerHTML = dictOptionsHTML(
      state.projectGroups,
      c && c.project_group ? c.project_group : "",
      dictEmptyLabel("project"),
    );
    openModal("clientModal");
  }
  // ensureClientPickerData：对话框打开时目标/池列表数据缺失则后台补拉。
  // 仅在容器尚无可选标签（用户未开始选择）时重渲染，避免覆盖已选项。
  async function ensureClientPickerData() {
    var needTargets = !state.targets || !state.targets.length;
    var needPools = !state.pools || !state.pools.length;
    if (!needTargets && !needPools) {
      return;
    }
    try {
      await loadTargets(true);
      var targetTags = document.getElementById("clientTargetTags");
      if (
        !targetTags.querySelector("span[data-target]") &&
        state.targets &&
        state.targets.length
      ) {
        renderClientTargetTags(
          state.editingClient && state.editingClient.allowed_targets
            ? state.editingClient.allowed_targets
            : [],
        );
      }
    } catch (e) {
      /* 静默失败：容器保持空态提示 */
    }
  }
  function renderClientTargetTags(selected) {
    var container = document.getElementById("clientTargetTags");
    container.innerHTML = "";
    var selectedSet = Object.create(null);
    (selected || []).forEach(function (t) {
      selectedSet[t.toLowerCase()] = true;
    });
    var targets = (state.targets || []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    (selected || []).forEach(function (name) {
      if (!targets.some(function (target) {
        return target.name.toLowerCase() === name.toLowerCase();
      }))
        targets.push({ name: name, missing: true });
    });
    if (!targets.length) {
      container.innerHTML =
        "<span style='color:var(--text-muted);font-size:12px;'>暂无目标，请先添加目标</span>";
      return;
    }
    targets.forEach(function (t) {
      var tag = document.createElement("span");
      var isSelected = !!selectedSet[t.name.toLowerCase()];
      var epLabel = epShortLabel(
        t.endpoint_type || state.schema.defaults.endpoint_type,
      );
      tag.textContent = t.name + (t.missing ? "（目标已删除）" : "");
      tag.title = t.missing
        ? "保留已有授权，点击可移除"
        : epLabel + " — " + (t.endpoint || "");
      tag.dataset.target = t.name;
      applyTagStyle(tag, isSelected);
      tag.addEventListener("click", function () {
        var cur = this.dataset.selected === "1";
        applyTagStyle(this, !cur);
      });
      container.appendChild(tag);
    });
  }
  function applyTagStyle(tag, selected) {
    tag.dataset.selected = selected ? "1" : "0";
    tag.style.cssText =
      "display:inline-block;padding:4px 12px;border-radius:6px;font-size:12px;cursor:pointer;transition:all 0.15s ease;user-select:none;" +
      (selected
        ? "background:var(--primary);color:var(--accent-ink);border:1px solid var(--primary);"
        : "background:var(--bg);color:var(--text-secondary);border:1px solid var(--card-border);");
  }
  function getSelectedClientTargets() {
    var tags = document
      .getElementById("clientTargetTags")
      .querySelectorAll("span[data-target]");
    var result = [];
    tags.forEach(function (tag) {
      if (tag.dataset.selected === "1") result.push(tag.dataset.target);
    });
    return result;
  }
  // renderClientPoolTags 渲染客户端表单的允许池 chips（交互与目标 tags 一致）。
  function renderClientPoolTags(selected) {
    var container = document.getElementById("clientPoolTags");
    container.innerHTML = "";
    var selectedSet = Object.create(null);
    var available = targetPoolsAvailable();
    (selected || []).forEach(function (p) {
      selectedSet[String(p).toLowerCase()] = true;
    });
    var memberCount = poolMemberCounter();
    var pools = (state.pools || []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    (selected || []).forEach(function (name) {
      if (!pools.some(function (pool) {
        return pool.name.toLowerCase() === name.toLowerCase();
      }))
        pools.push({ name: name, missing: available, unknown: !available });
    });
    if (!pools.length) {
      container.innerHTML =
        available ? "<span style='color:var(--text-muted);font-size:12px;'>暂无部署池，请先在「部署池」页创建</span>" :
          "<span role='status'>部署池列表不可用，请刷新后重试。</span>";
      return;
    }
    pools.forEach(function (p) {
      var tag = document.createElement("span");
      var isSelected = !!selectedSet[String(p.name).toLowerCase()];
      tag.textContent = poolDisplayName(p) +
        (!available ? "（部署池信息不可用）" : p.missing ? "（部署池已删除）" : "（" + memberCount(p.name) + "）");
      tag.title = p.missing
        ? "保留已有授权，点击可移除"
        : p.description ? p.description : "部署池 " + poolDisplayName(p);
      tag.dataset.pool = p.name;
      applyTagStyle(tag, isSelected);
      tag.addEventListener("click", function () {
        var cur = this.dataset.selected === "1";
        applyTagStyle(this, !cur);
      });
      container.appendChild(tag);
    });
  }
  function getSelectedClientPools() {
    var tags = document
      .getElementById("clientPoolTags")
      .querySelectorAll("span[data-pool]");
    var result = [];
    tags.forEach(function (tag) {
      if (tag.dataset.selected === "1") result.push(tag.dataset.pool);
    });
    return result;
  }
  function onSchedulingRefresh() {
    if (!document.getElementById("clientModal").classList.contains("open")) return;
    renderClientPoolTags(getSelectedClientPools());
  }

  // ensureClientPickerData：对话框打开时目标/池列表数据缺失则后台补拉。
  // 仅在容器尚无可选标签（用户未开始选择）时重渲染，避免覆盖已选项。
  document
    .getElementById("newClientBtn")
    .addEventListener("click", function () {
      openClientModal(null);
    });
  document
    .getElementById("refreshClientsBtn")
    .addEventListener("click", function () {
      loadClients();
    });
  document
    .getElementById("saveClientBtn")
    .addEventListener("click", async function () {
      if (isReadonlyMode()) return;
      if (clientSaveInFlight) {
        toast("客户端正在保存，请稍候。", "error");
        return;
      }
      if (clientCreatedDraftNeedsReopen) {
        toast("客户端已创建。请先复制未保存的修改，再重新打开客户端编辑。", "error");
        return;
      }
      var name = document.getElementById("clientName").value.trim();
      if (state.editingClient && editingClientVersion !== (clientVersions[state.editingClient.name] || 0)) {
        toast("客户端已更新，请保留草稿并重新打开以确认最新授权。", "error");
        return;
      }
      var key = document.getElementById("clientAccessKey").value.trim();
      var ipAddress = document.getElementById("clientIPAddress").value.trim();
      var targets = getSelectedClientTargets();
      var pools = getSelectedClientPools();
      if (!name || !key) {
        toast("请填写名称和 Access Key", "error");
        return;
      }
      var payload = {
        name: name,
        access_key: key,
        ip_address: ipAddress,
        allowed_targets: targets,
        allowed_pools: pools,
        job_level: document.getElementById("clientJobLevel").value.trim(),
        project_group: document
          .getElementById("clientProjectGroup")
          .value.trim(),
      };
      var saveVersion = clientOpenVersion;
      var saveRevision = clientDraftRevision;
      var wasEditing = !!state.editingClient;
      clientSaveInFlight = true;
      clientSavingName = name;
      // B3 按渠道分层限额：收集非空行（池名必填）
      try {
        if (state.editingClient) {
          await api("/admin/data/clients/" + encodeURIComponent(name), {
            method: "PUT",
            body: payload,
          });
          toast("客户端已更新", "success");
        } else {
          await api("/admin/data/clients", { method: "POST", body: payload });
          toast("客户端已创建", "success");
        }
        ++clientLoadVersion;
        clientVersions[name] = (clientVersions[name] || 0) + 1;
        if (saveVersion === clientOpenVersion && wasEditing) editingClientVersion = clientVersions[name];
        var savedIndex = state.clients.findIndex(function (client) { return client.name === name; });
        if (savedIndex >= 0) state.clients[savedIndex] = Object.assign({}, state.clients[savedIndex], payload);
        else state.clients.push(payload);
        renderClients();
        if (saveVersion === clientOpenVersion && saveRevision === clientDraftRevision) {
          closeModal("clientModal");
        } else if (saveVersion === clientOpenVersion) {
          if (!wasEditing) clientCreatedDraftNeedsReopen = true;
          toast(wasEditing ? "请求时的内容已保存，后续修改尚未保存。" :
            "客户端已创建，后续修改未保存。请复制草稿并重新打开客户端编辑。", "error");
        }
        await loadClients();
      } catch (e) {
        toast("保存失败: " + e.message, "error");
      } finally {
        clientSaveInFlight = false;
        clientSavingName = "";
      }
    });

  return {
    loadClients: loadClients,
    renderClients: renderClients,
    openClientModal: openClientModal,
    ensureClientPickerData: ensureClientPickerData,
    renderClientTargetTags: renderClientTargetTags,
    applyTagStyle: applyTagStyle,
    getSelectedClientTargets: getSelectedClientTargets,
    renderClientPoolTags: renderClientPoolTags,
    getSelectedClientPools: getSelectedClientPools,
    onSchedulingRefresh: onSchedulingRefresh,
  };
});
