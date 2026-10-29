"use strict";
// Pools owns deployment pool CRUD and member scheduling summaries.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("pools", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var schemaLabel = app.bind("schema", "schemaLabel");
  var fetchSchedulingRuntime = app.bind("targets", "fetchSchedulingRuntime");
  var beginSchedulingRefresh = app.bind("targets", "beginSchedulingRefresh");
  var schedulingRefreshCurrent = app.bind("targets", "schedulingRefreshCurrent");
  var publishSchedulingRefresh = app.bind("targets", "publishSchedulingRefresh");
  var failSchedulingRefresh = app.bind("targets", "failSchedulingRefresh");
  var validPoolRoster = app.bind("targets", "validPoolRoster");
  var targetPoolsAvailable = app.bind("targets", "targetPoolsAvailable");
  /* ===== Pools ===== */
  function poolDisplayName(pool) {
    if (typeof pool === "string") {
      var key = pool.toLowerCase();
      return poolDisplayName((state.pools || []).find(function (p) {
        return String(p.name || "").toLowerCase() === key;
      }) || {name: pool});
    }
    return pool ? pool.display_name || pool.name || "" : "";
  }
  // poolMemberCounter reads state.targets once and answers per-pool counts in
  // O(1), so callers rendering many pools stay linear in pools + targets.
  function poolMemberCounter() {
    var counts = Object.create(null);
    (state.targets || []).forEach(function (t) {
      if (!t.pool) return;
      var key = String(t.pool).toLowerCase();
      counts[key] = (counts[key] || 0) + 1;
    });
    return function (poolName) {
      return counts[String(poolName || "").toLowerCase()] || 0;
    };
  }
  function poolMemberCount(poolName) {
    return poolMemberCounter()(poolName);
  }
  // poolMemberChips：池成员节点调度状态 chips（S5）。每 chip = 相位色点 + 名称，
  // hover 展示在途/利用率/亲和命中/熔断次数；运行时快照缺失时仅显示名称。
  function poolMemberChips(poolName) {
    var key = String(poolName || "").toLowerCase();
    var members = (state.targets || []).filter(function (t) {
      return t.pool && String(t.pool).toLowerCase() === key;
    });
    if (!members.length)
      return "<span style='color:var(--text-muted)'>无成员</span>";
    return members
      .map(function (t) {
        var rt = state.runtime
          ? state.runtime[String(t.name || "").toLowerCase()]
          : null;
        var dotColors = {
          ready: "var(--ok)",
          muted: "var(--warn)",
          "half-open": "var(--out)",
          offline: "var(--ink-3)",
        };
        var dot = "var(--ink-3)";
        var tip = esc(t.name);
        if (rt) {
          dot = dotColors[rt.phase] || dot;
          tip +=
            "｜状态 " +
            esc(schemaLabel("target_phases", rt.phase)) +
            "｜在途 " +
            rt.in_flight +
            (rt.max_concurrency > 0
              ? "/" +
                rt.max_concurrency +
                "（" +
                Math.round((rt.utilization || 0) * 100) +
                "%）"
              : "") +
            "｜亲和命中 " +
            rt.affinity_hits +
            "｜熔断 " +
            rt.breaker_trips;
        } else {
          tip += "｜运行态未知";
        }
        var inFlightTxt = rt
          ? " <span class='pool-member-load'>" +
            rt.in_flight +
            (rt.max_concurrency > 0 ? "/" + rt.max_concurrency : "") +
            "</span>"
          : "";
        return (
          "<span class='pool-member-chip' title='" +
          tip +
          "'>" +
          "<span class='pool-member-dot' style='background:" +
          dot +
          ";'></span>" +
          "<span class='pool-member-name'>" +
          esc(t.name) +
          "</span>" +
          inFlightTxt +
          "</span>"
        );
      })
      .join(" ");
  }
  async function loadPools() {
    var sequence = beginSchedulingRefresh();
    try {
      // 成员 chips 按 targets 汇总；每次刷新都重新拉取，失败时整体报错，
      // 不能把未知成员关系渲染成"无成员"。
      var results = await Promise.all([
        api("/admin/data/pools"),
        fetchSchedulingRuntime(true),
        api("/admin/data/targets"),
      ]);
      if (!schedulingRefreshCurrent(sequence)) return;
      if (!validPoolRoster(results[0])) {
        throw new Error("部署池响应格式无效");
      }
      publishSchedulingRefresh(sequence, results[2], results[1], results[0]);
    } catch (e) {
      if (!failSchedulingRefresh(sequence, e)) return;
      // Stale rows must not read as the current roster.
      renderPoolsLoadError(e.message);
      toast("加载部署池失败: " + e.message, "error");
    }
  }
  function renderPoolsLoadError(message) {
    document.getElementById("poolsTableBody").innerHTML =
      "<tr class='empty-row'><td colspan='5'>加载失败: " +
      esc(message) +
      "。请点击「刷新」重试。</td></tr>";
  }
  function onSchedulingRefresh() {
    if (targetPoolsAvailable()) renderPools();
    else renderPoolsLoadError("部署池数据不可用");
  }
  function renderPools() {
    var tb = document.getElementById("poolsTableBody");
    if (!state.pools || !state.pools.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='5'>暂无部署池</td></tr>";
      return;
    }
    tb.innerHTML = state.pools
      .slice()
      .sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      })
      .map(function (p) {
        var members = poolMemberChips(p.name);
        var pauseBadge = p.paused
          ? "<span class='quota-badge over'>已暂停（成员等效暂停）</span>"
          : "<span class='quota-badge ok'>运行中</span>";
        var pauseButton = p.paused
          ? "<button class='btn btn-sm readonly-hide' data-resume-pool='" +
            esc(p.name) +
            "'>恢复</button>"
          : "<button class='btn btn-sm readonly-hide' data-pause-pool='" +
            esc(p.name) +
            "'>暂停</button>";
        return (
          "<tr><td class='pool-name'>" +
          esc(poolDisplayName(p)) +
          "</td>" +
          "<td class='pool-description'><div class='pool-description-text'>" +
          (p.description
            ? esc(p.description)
            : "<span style='color:var(--text-muted)'>—</span>") +
          "</div></td>" +
          "<td class='pool-status'>" +
          pauseBadge +
          "</td>" +
          "<td class='pool-members'><div class='pool-member-list'>" +
          members +
          "</div></td>" +
          "<td class='pool-actions'><div class='btn-group'>" +
          pauseButton +
          "<button class='btn btn-sm readonly-hide' data-edit-pool='" +
          esc(p.name) +
          "'>编辑</button>" +
          "<button class='btn btn-danger btn-sm readonly-hide' data-del-pool='" +
          esc(p.name) +
          "'>删除</button>" +
          "</div></td></tr>"
        );
      })
      .join("");
  }
  // A repeated click must not send the same write twice while one is pending.
  var rowActionsInFlight = Object.create(null);
  var poolSaveInFlight = false;
  document
    .getElementById("poolsTableBody")
    .addEventListener("click", function (e) {
      var t = e.target;
      if (t.dataset.editPool) {
        var p = state.pools.find(function (x) {
          return x.name === t.dataset.editPool;
        });
        if (p) openPoolModal(p);
      }
      if (t.dataset.pausePool) {
        var pauseName = t.dataset.pausePool;
        showConfirm(
          "确认暂停部署池「" +
            poolDisplayName(pauseName) +
            "」？池内全部成员节点将等效暂停，不参与转发。",
          async function () {
            try {
              await api(
                "/admin/data/pools/" + encodeURIComponent(pauseName) + "/pause",
                { method: "PUT" },
              );
              toast("已暂停池: " + poolDisplayName(pauseName), "success");
              await loadPools();
            } catch (e) {
              toast("暂停失败: " + e.message, "error");
            }
          },
        );
      }
      if (t.dataset.resumePool) {
        var resumeName = t.dataset.resumePool;
        if (rowActionsInFlight["resume:" + resumeName]) return;
        rowActionsInFlight["resume:" + resumeName] = true;
        (async function () {
          try {
            await api(
              "/admin/data/pools/" + encodeURIComponent(resumeName) + "/resume",
              { method: "PUT" },
            );
            toast("已恢复池: " + poolDisplayName(resumeName), "success");
            await loadPools();
          } catch (e) {
            toast("恢复失败: " + e.message, "error");
          } finally {
            delete rowActionsInFlight["resume:" + resumeName];
          }
        })();
      }
      if (t.dataset.delPool) {
        var delName = t.dataset.delPool;
        showConfirm(
          "确认删除部署池「" + poolDisplayName(delName) + "」？池内需无成员目标。",
          async function () {
            try {
              await api("/admin/data/pools/" + encodeURIComponent(delName), {
                method: "DELETE",
              });
              toast("已删除池: " + poolDisplayName(delName), "success");
              await loadPools();
            } catch (e) {
              toast("删除失败: " + e.message, "error");
            }
          },
        );
      }
    });
  function openPoolModal(p) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return;
    }
    state.editingPool = p || null;
    document.getElementById("poolModalTitle").textContent = p
      ? "编辑部署池"
      : "新增部署池";
    document.getElementById("poolName").value = poolDisplayName(p);
    document.getElementById("poolName").readOnly = false;
    document.getElementById("poolDescription").value =
      p && p.description ? p.description : "";
    document.getElementById("poolPaused").checked = p ? !!p.paused : false;
    openModal("poolModal");
  }
  document.getElementById("newPoolBtn").addEventListener("click", function () {
    openPoolModal(null);
  });
  document
    .getElementById("refreshPoolsBtn")
    .addEventListener("click", function () {
      loadPools();
    });
  document
    .getElementById("savePoolBtn")
    .addEventListener("click", async function () {
      var name = document.getElementById("poolName").value.trim();
      var description = document.getElementById("poolDescription").value.trim();
      var paused = document.getElementById("poolPaused").checked;
      if (!name) {
        toast("请填写池名称", "error");
        return;
      }
      if (poolSaveInFlight) return;
      poolSaveInFlight = true;
      try {
        if (state.editingPool) {
          await api("/admin/data/pools/" + encodeURIComponent(state.editingPool.name), {
            method: "PUT",
            body: {
              name: state.editingPool.name,
              display_name: name,
              description: description,
              paused: paused,
            },
          });
          toast("部署池已更新", "success");
        } else {
          await api("/admin/data/pools", {
            method: "POST",
            body: {
              name: name,
              description: description,
              paused: paused,
            },
          });
          toast("部署池已创建", "success");
        }
        closeModal("poolModal");
        await loadPools();
      } catch (e) {
        toast("保存失败: " + e.message, "error");
      } finally {
        poolSaveInFlight = false;
      }
    });

  /* ===== Consumer Groups ===== */

  return {
    poolDisplayName: poolDisplayName,
    poolMemberCount: poolMemberCount,
    poolMemberCounter: poolMemberCounter,
    poolMemberChips: poolMemberChips,
    loadPools: loadPools,
    renderPools: renderPools,
    onSchedulingRefresh: onSchedulingRefresh,
    openPoolModal: openPoolModal,
  };
});
