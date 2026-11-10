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
  // Cost-preview state: at most one pool detail row is open. The sequence
  // guard drops responses that arrive after a refresh or another pool opened.
  var expandedCostPool = "";
  var costPreviewSequence = 0;
  /* ===== Pools ===== */
  function poolRoutingStrategy(pool) {
    return (pool && pool.routing_strategy) || state.schema.defaults.pool_routing_strategy;
  }
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
  // splitAliases mirrors config.SplitFallbackAliases: comma-separated,
  // trimmed, empty entries ignored, configured spelling preserved.
  function splitAliases(fallback) {
    return String(fallback || "")
      .split(",")
      .map(function (alias) {
        return alias.trim();
      })
      .filter(Boolean);
  }
  // poolMemberModelTip 以成本选路的比较单位——客户端别名——为主展示成员的
  // 模型映射：alias（上游 upstream）。别名按逗号拆分；没有 model_mappings
  // 的成员接受任意模型，显式标为「不限」；已暂停映射仍展示但标注「已暂停」。
  function poolMemberModelTip(t) {
    var mappings = Array.isArray(t.model_mappings) ? t.model_mappings : [];
    var labels = [];
    mappings.forEach(function (m) {
      var upstream = String((m && m.upstream) || "").trim();
      var aliases = splitAliases(m && m.fallback);
      if (!aliases.length && upstream) aliases = [""];
      aliases.forEach(function (alias) {
        var label = alias
          ? esc(alias) + "（上游 " + esc(upstream || alias) + "）"
          : esc(upstream);
        if (label) labels.push(m && m.paused ? "已暂停 " + label : label);
      });
    });
    return labels.length ? labels.join("、") : "不限";
  }
  // poolAliasGroups 只聚合启用中的 cost 池成员，按客户端可接受的请求名分组：
// 每个映射的 fallback 别名优先，其 upstream 名也可被客户端直接请求（与
// /v1/models 的展示一致），因此作为次要分组一并列出；同一请求名的成员合并。
// 成员的上游名同时放在分组 title 中。已暂停的 target/映射不参与路由，不进入
// 分组；没有 model_mappings 的成员接受任意请求名，无法归入具体分组。
  function poolAliasGroups(poolName) {
    var key = String(poolName || "").toLowerCase();
    var groups = Object.create(null);
    var order = [];
    (state.targets || []).forEach(function (t) {
      if (!t || t.paused) return;
      if (!t.pool || String(t.pool).toLowerCase() !== key) return;
      var name = String(t.name || "");
      (Array.isArray(t.model_mappings) ? t.model_mappings : []).forEach(function (m) {
        if (!m || m.paused) return;
        var upstream = String(m.upstream || "").trim();
        var names = splitAliases(m.fallback).map(function (alias) {
          return { name: alias, aliasLike: true };
        });
        if (upstream) names.push({ name: upstream, aliasLike: false });
        names.forEach(function (item) {
          var groupKey = item.name.toLowerCase();
          var group = groups[groupKey];
          if (!group) {
            group = groups[groupKey] = {
              alias: item.name,
              aliasLike: false,
              members: [],
              upstreams: Object.create(null),
            };
            order.push(groupKey);
          }
          if (item.aliasLike) group.aliasLike = true;
          if (group.members.indexOf(name) === -1) group.members.push(name);
          if (upstream) {
            var ups = group.upstreams[name] || (group.upstreams[name] = []);
            if (ups.indexOf(upstream) === -1) ups.push(upstream);
          }
        });
      });
    });
    // fallback 别名组在前，可直请的上游名组作为次要分组在后。
    return order
      .sort(function (a, b) {
        if (groups[a].aliasLike !== groups[b].aliasLike) {
          return groups[a].aliasLike ? -1 : 1;
        }
        return a.localeCompare(b);
      })
      .map(function (groupKey) {
        return groups[groupKey];
      });
  }
  function poolAliasGroupBadges(poolName) {
    var groups = poolAliasGroups(poolName);
    if (!groups.length) return "";
    return (
      "<div class='pool-alias-groups'>" +
      groups
        .map(function (g) {
          var tip =
            "上游：" +
            g.members
              .map(function (name) {
                var ups = g.upstreams[name];
                return name + " → " + (ups && ups.length ? ups.join("、") : "不限");
              })
              .join("；");
          return (
            "<span class='pool-alias-group' title='" +
            esc(tip) +
            "'>" +
            esc(g.alias) +
            " · " +
            esc(g.members.join("、")) +
            "</span>"
          );
        })
        .join("") +
      "</div>"
    );
  }
  // poolMemberChips：池成员节点调度状态 chips（S5）。每 chip = 相位色点 + 名称，
  // hover 展示在途/利用率/亲和命中/熔断次数；showModels 为 true 时（cost 池）
  // 追加别名优先的模型映射。运行时快照缺失时显示名称与模型映射。
  function poolMemberChips(poolName, showModels) {
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
                "（并发上限，" +
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
        if (showModels)
          tip += "｜模型 " + (t.paused ? "目标已暂停 " : "") + poolMemberModelTip(t);
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
  // poolCostModelNames：预览下拉/提示的客户端请求名。与可见分组不同，这里
  // 同时包含已暂停 target/映射声明的名字，否则操作者无法通过预览看到它们
  // 为何不参与。fallback 别名优先、可直请上游名次之，各自按名称排序；
  // 无映射成员接受任意名，不在列表中，操作者可直接输入。
  function poolCostModelNames(poolName) {
    var key = String(poolName || "").toLowerCase();
    var aliases = [];
    var upstreams = [];
    var seen = Object.create(null);
    var byName = function (a, b) {
      return a.toLowerCase().localeCompare(b.toLowerCase());
    };
    (state.targets || []).forEach(function (t) {
      if (!t.pool || String(t.pool).toLowerCase() !== key) return;
      (Array.isArray(t.model_mappings) ? t.model_mappings : []).forEach(function (m) {
        if (!m) return;
        splitAliases(m.fallback).forEach(function (alias) {
          var nameKey = alias.toLowerCase();
          if (seen[nameKey]) return;
          seen[nameKey] = true;
          aliases.push(alias);
        });
        var upstream = String(m.upstream || "").trim();
        if (upstream) {
          var upstreamKey = upstream.toLowerCase();
          if (!seen[upstreamKey]) {
            seen[upstreamKey] = true;
            upstreams.push(upstream);
          }
        }
      });
    });
    return aliases.sort(byName).concat(upstreams.sort(byName));
  }
  function poolCostReasonLabel(reason) {
    var labels = {
      target_paused: "目标暂停",
      mapping_paused: "映射暂停",
      alias_not_declared: "未声明该名",
      unpriced: "无价格",
      pricing_unavailable: "价格源不可用",
    };
    return labels[reason] || "";
  }
  function poolCostPreviewHtml(poolName) {
    var names = poolCostModelNames(poolName);
    var options = names
      .map(function (name) {
        return "<option value='" + esc(name) + "'></option>";
      })
      .join("");
    var value = names.length ? names[0] : "";
    return (
      "<div class='pool-cost-panel'>" +
      "<div class='pool-cost-head'>" +
      "<label for='poolCostModel'>客户端请求名</label>" +
      "<input class='form-control' id='poolCostModel' list='poolCostModelOptions' value='" +
      esc(value) +
      "' placeholder='例如 " +
      esc(names[0] || "model-alias") +
      "'>" +
      "<datalist id='poolCostModelOptions'>" +
      options +
      "</datalist>" +
      "<button class='btn btn-sm' id='poolCostPreviewRun' data-cost-preview-run='" +
      esc(poolName) +
      "'>预览</button>" +
      "<span id='poolCostStatus' class='pool-cost-status'></span>" +
      "</div>" +
      "<div id='poolCostResult' class='pool-cost-result'></div>" +
      "</div>"
    );
  }
  function renderPoolCostPreview(data) {
    if (!data || !data.active) {
      return "<div class='pool-cost-note'>该池不是「成本优先」策略，不参与成本选路。</div>";
    }
    var ref = data.reference_tokens || {};
    var fmtTokens = function (value) {
      var n = Number(value || 0);
      return n % 10000 === 0 ? n / 10000 + " 万" : String(n);
    };
    var meta =
      "<div class='pool-cost-meta'>请求名 " +
      esc(String(data.model || "")) +
      " · 生成时间 " +
      esc(String(data.generated_at || "")) +
      " · 参考组合 " +
      fmtTokens(ref.input) +
      "未缓存输入 + " +
      fmtTokens(ref.cached_input) +
      "缓存读 + " +
      fmtTokens(ref.output) +
      "输出 · 同层容差 " +
      Math.round(Number(data.tolerance || 0) * 100) +
      "%</div>";
    var warning = data.pricing_available
      ? ""
      : "<div class='pool-cost-error'>定价源不可用" +
        (data.pricing_error ? "：" + esc(data.pricing_error) : "") +
        "；以下仅展示成员与可参与性。</div>";
    var members = Array.isArray(data.members) ? data.members : [];
    if (!members.length) {
      return meta + warning + "<div class='pool-cost-note'>该池暂无可展示的成员。</div>";
    }
    var rows = members
      .map(function (m) {
        var cost =
          m.estimated_cost_usd == null
            ? "—"
            : "$" + Number(m.estimated_cost_usd).toFixed(6);
        var tier =
          m.tier == null ? "—" : m.tier === 0 ? "最便宜层" : "较贵层";
        var phase = m.phase
          ? esc(schemaLabel("target_phases", m.phase) || m.phase)
          : "—";
        var reason = poolCostReasonLabel(m.reason);
        var load =
          m.max_concurrency > 0
            ? " · 在途 " + m.in_flight + "/" + m.max_concurrency + "（上限）"
            : "";
        var cap = m.affinity_soft_cap_reached ? " · 软上限" : "";
        var state = phase + load + cap + (reason ? " · " + reason : "");
        var via =
          m.resolved_via === "alias"
            ? "别名"
            : m.resolved_via === "upstream"
              ? "上游名"
              : m.resolved_via === "unrestricted"
                ? "不限"
                : "未声明";
        var upstream = m.upstream
          ? esc(m.upstream) + " <span class='pool-cost-secondary'>（" + via + "）</span>"
          : "—";
        return (
          "<tr class='" +
          (m.cheapest ? "pool-cost-cheapest" : "") +
          "'><td>" +
          esc(m.target) +
          "</td><td>" +
          upstream +
          "</td><td class='mono'>" +
          cost +
          "</td><td>" +
          tier +
          "</td><td>" +
          state +
          "</td></tr>"
        );
      })
      .join("");
    return (
      meta +
      warning +
      "<div class='pool-cost-table-wrap'><table class='pool-cost-table'><thead><tr>" +
      "<th>成员</th><th>上游（次要）</th><th>估算成本（USD）</th><th>层级</th><th>运行态/原因</th>" +
      "</tr></thead><tbody>" +
      rows +
      "</tbody></table></div>" +
      "<p class='pool-cost-note'>估算按选路口径（固定参考 token 组合），不含条件价、模型组系数与实际 token；实际选择还受可用性、容量、探针与粘连影响；计费以事件回读为准。</p>"
    );
  }
  async function loadPoolCostPreview(poolName) {
    var input = document.getElementById("poolCostModel");
    var result = document.getElementById("poolCostResult");
    var status = document.getElementById("poolCostStatus");
    if (!input || !result) return;
    var model = String(input.value || "").trim();
    if (!model) {
      if (status) status.textContent = "请输入客户端请求名";
      return;
    }
    var sequence = ++costPreviewSequence;
    if (status) status.textContent = "加载中…";
    try {
      var data = await api(
        "/admin/data/pools/" +
          encodeURIComponent(poolName) +
          "/cost-routing?model=" +
          encodeURIComponent(model),
      );
      if (sequence !== costPreviewSequence || expandedCostPool !== poolName) return;
      if (status) status.textContent = "";
      result.innerHTML = renderPoolCostPreview(data);
    } catch (e) {
      if (sequence !== costPreviewSequence || expandedCostPool !== poolName) return;
      if (status) status.textContent = "";
      result.innerHTML =
        "<div class='pool-cost-error'>预览失败: " + esc(e.message) + "</div>";
    }
  }
  function focusCostPreviewControl(poolName, opening) {
    var el = null;
    if (opening) {
      el = document.getElementById("poolCostModel");
    } else {
      var buttons = document.querySelectorAll("[data-cost-preview-pool]");
      for (var i = 0; i < buttons.length; i++) {
        if (buttons[i].dataset && buttons[i].dataset.costPreviewPool === poolName) {
          el = buttons[i];
          break;
        }
      }
    }
    if (el && typeof el.focus === "function") el.focus();
  }
  async function loadPools() {
    // A roster refresh invalidates any open detail row and in-flight preview.
    expandedCostPool = "";
    costPreviewSequence++;
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
    // Any roster refresh invalidates the open detail row and in-flight preview.
    expandedCostPool = "";
    costPreviewSequence++;
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
        // Alias-first model info is a cost-routing concept; load pools keep
        // their previous chips-only runtime view.
        var isCost = poolRoutingStrategy(p) === "cost";
        var members = poolMemberChips(p.name, isCost);
        var aliasGroups = isCost && !p.paused ? poolAliasGroupBadges(p.name) : "";
        var pauseBadge = p.paused
          ? "<span class='quota-badge over'>已暂停（成员等效暂停）</span>"
          : "<span class='quota-badge ok'>运行中</span>";
        if (poolRoutingStrategy(p) !== state.schema.defaults.pool_routing_strategy)
          pauseBadge +=
            " <span class='quota-badge strategy'>" +
            esc(schemaLabel("pool_routing_strategies", poolRoutingStrategy(p))) +
            "</span>";
        var pauseButton = p.paused
          ? "<button class='btn btn-sm readonly-hide' data-resume-pool='" +
            esc(p.name) +
            "'>恢复</button>"
          : "<button class='btn btn-sm readonly-hide' data-pause-pool='" +
            esc(p.name) +
            "'>暂停</button>";
        var previewButton = isCost
          ? "<button class='btn btn-sm' data-cost-preview-pool='" +
            esc(p.name) +
            "' aria-expanded='" +
            (expandedCostPool === p.name ? "true" : "false") +
            "'" +
            (expandedCostPool === p.name
              ? " aria-controls='poolCostDetail'"
              : "") +
            ">成本预览</button>"
          : "";
        var detailRow =
          isCost && expandedCostPool === p.name
            ? "<tr class='pool-cost-row' id='poolCostDetail'><td colspan='5'>" +
              poolCostPreviewHtml(p.name) +
              "</td></tr>"
            : "";
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
          "</div>" +
          aliasGroups +
          "</td>" +
          "<td class='pool-actions'><div class='btn-group'>" +
          pauseButton +
          previewButton +
          "<button class='btn btn-sm readonly-hide' data-edit-pool='" +
          esc(p.name) +
          "'>编辑</button>" +
          "<button class='btn btn-danger btn-sm readonly-hide' data-del-pool='" +
          esc(p.name) +
          "'>删除</button>" +
          "</div></td></tr>" +
          detailRow
        );
      })
      .join("");
  }
  document
    .getElementById("poolsTableBody")
    .addEventListener("keydown", function (e) {
      if (e.key === "Enter" && e.target && e.target.id === "poolCostModel") {
        e.preventDefault();
        loadPoolCostPreview(expandedCostPool);
      }
    });
  // A repeated click must not send the same write twice while one is pending.
  var rowActionsInFlight = Object.create(null);
  var poolSaveInFlight = false;
  document
    .getElementById("poolsTableBody")
    .addEventListener("click", function (e) {
      var t = e.target;
      if (t.dataset.costPreviewPool) {
        var previewName = t.dataset.costPreviewPool;
        if (expandedCostPool === previewName) {
          expandedCostPool = "";
          costPreviewSequence++;
          renderPools();
          focusCostPreviewControl(previewName, false);
        } else {
          expandedCostPool = previewName;
          costPreviewSequence++;
          renderPools();
          loadPoolCostPreview(previewName);
          focusCostPreviewControl(previewName, true);
        }
        return;
      }
      if (t.dataset.costPreviewRun) {
        loadPoolCostPreview(t.dataset.costPreviewRun);
        return;
      }
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
    document.getElementById("poolRoutingStrategy").value = poolRoutingStrategy(p);
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
      var routingStrategy = document.getElementById("poolRoutingStrategy").value;
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
              routing_strategy: routingStrategy,
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
              routing_strategy: routingStrategy,
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
