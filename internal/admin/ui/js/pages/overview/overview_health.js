"use strict";
// Overview live status: operations chain, target health, recent changes and historical stats.
AdminApp.register("overviewHealth", function (app) {
  var esc = app.bind("core", "esc");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var fmtPct = app.bind("core", "fmtPct");
  var totalTok = app.bind("core", "totalTok");
  var S = app.bind("overviewState", "store")();
  var request = app.bind("overview", "request");
  var showTooltip = app.bind("overview", "showTooltip");
  var counter = app.bind("overviewUtil", "counter");
  var duration = app.bind("overviewUtil", "duration");
  var el = app.bind("overviewUtil", "el");
  var isObject = app.bind("overviewUtil", "isObject");
  var keyIssue = app.bind("overviewUtil", "keyIssue");
  var keySummary = app.bind("overviewUtil", "keySummary");
  var localDate = app.bind("overviewUtil", "localDate");
  var number = app.bind("overviewUtil", "number");
  var phaseLabel = app.bind("overviewUtil", "phaseLabel");
  var targetPhase = app.bind("overviewUtil", "targetPhase");
  var targetRank = app.bind("overviewUtil", "targetRank");
  var text = app.bind("overviewUtil", "text");
  function persistenceComplete(value) {
    return isObject(value) && ["usage_async_pending", "usage_async_deferred", "request_stats_dropped"].every(function (key) { return counter(value[key]); });
  }
  function metricsComplete() {
    if (!isObject(S.metricsSnapshot) || !counter(S.metricsSnapshot.uptime_seconds)) return false;
    if (!isObject(S.metricsSnapshot.requests) || !counter(S.metricsSnapshot.requests.total)) return false;
    if (!persistenceComplete(S.metricsSnapshot.persistence)) return false;
    var budget = S.metricsSnapshot.observation_budget;
    if (!isObject(budget) || !isObject(budget.observation_budget_exhausted)) return false;
    return counter(budget.observation_budget_exhausted.br) && counter(budget.observation_budget_exhausted.zstd);
  }
  function historyStaleText() {
    return "更新" + (S.historyError === "加载超时" ? "超时" : "失败") + "，显示上次数据";
  }
  function targetRow(target) {
    var phase = targetPhase(target);
    var issue = keyIssue(target);
    var status = '<span class="overview-phase phase-' + esc(phase) + '">' + esc(phaseLabel(target)) + '</span>';
    if (phase === "muted" && localDate(target.MutedUntil) !== "—") status += ' <span class="overview-status">冷却至 ' + esc(localDate(target.MutedUntil)) + '</span>';
    if (phase === "offline" && target.Paused === true) status += ' <span class="overview-status">已暂停</span>';
    var capacity = counter(target.Capacity) ? (target.Capacity === 0 ? "不限" : fmtNum(target.Capacity)) : "不可用";
    var inflight = counter(target.InFlight) ? fmtNum(target.InFlight) : "不可用";
    var window = isObject(target.Window15m) ? "峰值 " + (counter(target.Window15m.in_flight_peak) ? fmtNum(target.Window15m.in_flight_peak) : "不可用") + " · 熔断 " + (counter(target.Window15m.breaker_trips) ? fmtNum(target.Window15m.breaker_trips) : "不可用") : "不可用";
    return '<tr data-target-name="' + esc(String(target.Name || "")) + '" data-target-phase="' + esc(phase) + '" data-target-key-issue="' + issue + '"><td>' + esc(String(target.Name || "未知目标")) + '</td><td>' + status + '</td><td>' + esc(keySummary(target)) + '</td><td>' + esc(inflight + " / " + capacity) + '</td><td>' + esc(window) + '</td></tr>';
  }
  function renderTargetHealth() {
    var body = el("targetHealthBody");
    var count = el("targetHealthCount");
    var status = el("targetHealthStatus");
    var targets = S.historySnapshot && Array.isArray(S.historySnapshot.targets) ? S.historySnapshot.targets.slice() : null;
    if (!targets) {
      body.innerHTML = '<tr><td colspan="5">目标数据不可用</td></tr>';
      count.textContent = "不可用";
      status.textContent = S.historyError ? "目标数据" + historyStaleText() + "，目标数据不可用" : "目标数据不可用";
      status.dataset.state = "error";
      return;
    }
    targets = targets.map(function (target, index) { return {target: target || {}, index: index}; }).sort(function (a, b) {
      var difference = targetRank(a.target) - targetRank(b.target);
      return difference || a.index - b.index;
    });
    body.innerHTML = targets.length ? targets.map(function (entry) { return targetRow(entry.target); }).join("") : '<tr><td colspan="5">暂无已配置目标</td></tr>';
    count.textContent = fmtNum(S.historySnapshot.targets.length) + " 个目标";
    var attention = targets.filter(function (entry) { return targetRank(entry.target) <= 4; }).length;
    var unknown = targets.filter(function (entry) { return targetPhase(entry.target) === "unknown"; }).length;
    if (!targets.length) status.textContent = "暂无目标";
    else if (attention) status.textContent = fmtNum(attention) + " 个目标需要关注" + (unknown ? "，另有 " + fmtNum(unknown) + " 个目标状态未知" : "");
    else if (unknown) status.textContent = fmtNum(unknown) + " 个目标状态未知";
    else status.textContent = "目标均在正常状态";
    if (S.historyError) {
      status.textContent += "；目标数据" + historyStaleText();
      status.dataset.state = "partial";
    } else {
      status.dataset.state = attention || unknown ? "partial" : "ready";
    }
  }
  function metricsUsable() {
    return isObject(S.metricsSnapshot);
  }
  // Ops signals that used to live in the removed attention panel. Only non-zero,
  // well-formed counters are listed; missing data yields no item (never a fake 0).
  function opsNoticeItems(metrics, ov) {
    var items = [];
    var persistence = metrics && isObject(metrics.persistence) ? metrics.persistence : null;
    if (persistence) {
      [
        ["usage_async_pending", "用量写入积压 ", " 条"],
        ["usage_async_deferred", "自启动累计 ", " 条用量因写入队列满转入后台重放"],
        ["request_stats_dropped", "自启动累计 ", " 条请求完成统计因队列满丢弃"],
      ].forEach(function (entry) {
        var count = persistence[entry[0]];
        if (counter(count) && count > 0) items.push(entry[1] + fmtNum(count) + entry[2]);
      });
    }
    var budget = metrics && isObject(metrics.observation_budget) ? metrics.observation_budget.observation_budget_exhausted : null;
    if (isObject(budget)) {
      var br = counter(budget.br) ? budget.br : null;
      var zstd = counter(budget.zstd) ? budget.zstd : null;
      if ((br || 0) + (zstd || 0) > 0) {
        if (br !== null && zstd !== null) items.push("压缩响应观察预算拒绝（自启动累计 " + fmtNum(br + zstd) + " 次；br " + fmtNum(br) + " / zstd " + fmtNum(zstd) + "）");
        else items.push("压缩响应观察预算拒绝（自启动累计；br " + (br === null ? "不可用" : fmtNum(br)) + " / zstd " + (zstd === null ? "不可用" : fmtNum(zstd)) + "；总数不完整）");
      }
    }
    // Process-lifetime request persistence counters: never present them as a
    // success rate or as a selected-range figure.
    var requests = ov && isObject(ov.requests) ? ov.requests : null;
    if (requests) {
      [
        ["usage_record_failures", "用量写入失败"],
        ["usage_client_gone_no_usage", "客户端断开且未取得用量"],
        ["usage_upstream_interrupted_no_usage", "上游中断且未取得用量"],
        ["request_stats_record_failures", "请求完成统计写入失败"],
      ].forEach(function (entry) {
        var count = requests[entry[0]];
        if (counter(count) && count > 0) items.push("自启动累计 " + fmtNum(count) + " 次" + entry[1]);
      });
    }
    return items;
  }
  function renderOpsNotice(metrics, ov) {
    var notice = el("chainOpsNotice");
    var items = opsNoticeItems(metrics, ov);
    var message = items.join(" · ");
    if (notice.textContent !== message) notice.textContent = message;
    if (notice.hidden !== !items.length) notice.hidden = !items.length;
  }
  function renderChain() {
    var ov = S.historySnapshot;
    var targets = ov && Array.isArray(ov.targets) ? ov.targets : null;
    var clientCount = ov && counter(ov.client_count) ? ov.client_count : null;
    var clientText = clientCount === null ? "配置客户端不可用" : fmtNum(clientCount) + " 个配置客户端";
    if (S.snapshot && counter(S.snapshot.runtime.active_clients)) clientText += " · 活跃 " + fmtNum(S.snapshot.runtime.active_clients);
    text("chainClients", clientText);
    var proxyParts = [];
    var metrics = metricsUsable() ? S.metricsSnapshot : null;
    if (metrics) {
      proxyParts.push(isObject(metrics.requests) && counter(metrics.requests.total) ? "累计 " + fmtNum(metrics.requests.total) : "累计请求不可用");
      proxyParts.push(counter(metrics.uptime_seconds) ? "运行 " + duration(metrics.uptime_seconds) : "运行时长不可用");
    } else {
      proxyParts.push("运行指标不可用");
    }
    text("chainProxy", proxyParts.join(" · "));
    renderOpsNotice(metrics, ov);
    if (targets) {
      if (!targets.length) {
        text("chainTargets", "暂无目标");
      } else {
        var healthy = targets.filter(function (target) { return targetPhase(target) === "ready" && keyIssue(target) === "false"; }).length;
        text("chainTargets", fmtNum(healthy) + " / " + fmtNum(targets.length));
      }
    } else {
      text("chainTargets", "不可用");
    }
    var keys = el("chainKeys");
    var keysAvailable = !!targets;
    var keysDetail = el("chainKeysDetail");
    keysDetail.textContent = "可用 / 总数";
    if (targets && !targets.length) {
      keys.textContent = "暂无目标";
      keys.setAttribute("data-active", "0");
      keys.setAttribute("data-total", "0");
      keys.setAttribute("data-rate-limited", "0");
    } else if (targets) {
      var total = 0, active = 0, rate = 0, complete = true;
      targets.forEach(function (target) {
        target = target || {};
        if (!["TotalKeys", "ActiveKeys", "RateLimitedKeys"].every(function (key) { return counter(target[key]); })) { complete = false; return; }
        total += target.TotalKeys;
        active += target.ActiveKeys;
        rate += target.RateLimitedKeys;
      });
      if (complete) {
        keys.textContent = total === 0 ? "未配置独立密钥" : fmtNum(active) + " / " + fmtNum(total);
        keysDetail.textContent = total === 0 ? "可能使用 Bearer/透传" : "可用 / 总数" + (rate > 0 ? " · 限流 " + fmtNum(rate) : "");
        keys.setAttribute("data-active", String(active));
        keys.setAttribute("data-total", String(total));
        keys.setAttribute("data-rate-limited", String(rate));
      } else {
        keysAvailable = false;
        keys.textContent = "不可用";
        keysDetail.textContent = "部分目标缺少密钥计数";
        keys.setAttribute("data-active", "unavailable");
        keys.setAttribute("data-total", "unavailable");
        keys.setAttribute("data-rate-limited", "unavailable");
      }
    } else {
      keys.textContent = "不可用";
      keysDetail.textContent = "目标数据不可用";
      keys.setAttribute("data-active", "unavailable");
      keys.setAttribute("data-total", "unavailable");
      keys.setAttribute("data-rate-limited", "unavailable");
    }
    var stats = ov && isObject(ov.request_stats_72h) ? ov.request_stats_72h : null;
    if (stats && stats.available === true) {
      text("ovLiveSuccessRate", stats.success_rate == null ? "--" : fmtPct(stats.success_rate));
      text("ovLiveSuccessDetail", counter(stats.total) ? (stats.total === 0 ? "暂无完成样本" : fmtNum(stats.total) + " 个完成样本") : "样本总数不可用");
    } else {
      text("ovLiveSuccessRate", "不可用");
      text("ovLiveSuccessDetail", "请求完成统计不可用");
    }
    var available = [!!targets, !!S.snapshot, metricsComplete(), clientCount !== null, keysAvailable].filter(Boolean).length;
    var chain = el("chainSummary");
    var status = el("chainStatus");
    var statusText;
    if (S.historyError) {
      chain.dataset.state = S.historySnapshot ? "partial" : "error";
      statusText = S.historySnapshot ? "实时状态" + historyStaleText() : "实时状态" + historyStaleText() + "，目标与客户端数据不可用";
    } else if (available === 5) {
      chain.dataset.state = "ready";
      statusText = "实时状态已加载";
    } else if (available === 0) {
      chain.dataset.state = "error";
      statusText = "实时状态暂不可用";
    } else {
      chain.dataset.state = "partial";
      statusText = "部分实时状态不可用";
    }
    var activityStale = S.activityError || (S.snapshot && Date.now() - Date.parse(S.snapshot.generated_at) > 15000 ? "实时活动数据更新延迟，显示上次采样。" : "");
    if (activityStale) {
      if (chain.dataset.state === "ready") statusText = "部分实时状态已过期";
      if (chain.dataset.state !== "error") chain.dataset.state = "partial";
      statusText += "；" + activityStale;
    }
    if (status.textContent !== statusText) status.textContent = statusText;
  }
  function renderRecentChanges() {
    var body = el("recentChangesBody");
    var status = el("recentChangesStatus");
    var events = S.historySnapshot && Array.isArray(S.historySnapshot.recent_audit) ? S.historySnapshot.recent_audit.slice() : null;
    if (!events) {
      body.innerHTML = '<tr><td colspan="5">最近变更不可用</td></tr>';
      status.textContent = S.historyError ? "最近变更" + historyStaleText() + "，最近变更不可用" : "最近变更不可用";
      status.dataset.state = "error";
      return;
    }
    if (!events.length) {
      body.innerHTML = '<tr><td colspan="5">暂无可显示的最近变更</td></tr>';
      status.textContent = "暂无变更" + (S.historyError ? "；最近变更" + historyStaleText() : "");
      status.dataset.state = S.historyError ? "partial" : "ready";
      return;
    }
    events.sort(function (a, b) {
      var at = Date.parse(a && a.timestamp), bt = Date.parse(b && b.timestamp);
      at = isFinite(at) ? at : -Infinity;
      bt = isFinite(bt) ? bt : -Infinity;
      // Equal keys (two unparseable stamps give -Infinity - -Infinity = NaN) stay in input order.
      return bt === at ? 0 : bt > at ? 1 : -1;
    });
    events = events.slice(0, 5);
    body.innerHTML = events.map(function (event, index) {
      var id = event && event.id ? String(event.id) : String(event && event.timestamp || index);
      return '<tr data-audit-id="' + esc(id) + '"><td><time datetime="' + esc(event && event.timestamp || "") + '">' + esc(localDate(event && event.timestamp)) + '</time></td><td>' + esc(event && event.actor || "—") + '</td><td>' + esc(event && event.action || "—") + '</td><td>' + esc(event && event.object || "—") + '</td><td>' + esc(event && event.result || "—") + '</td></tr>';
    }).join("");
    status.textContent = "最近 " + fmtNum(events.length) + " 条变更" + (S.historyError ? "；最近变更" + historyStaleText() : "");
    status.dataset.state = S.historyError ? "partial" : "ready";
  }
  // Historical completion samples and cost keep their existing API and semantics.
  function renderHistory(ov) {
    var stats = isObject(ov.request_stats_72h) ? ov.request_stats_72h : null;
    var available = !!stats && stats.available === true;
    var totalAvailable = available && counter(stats.total);
    var failuresAvailable = available && counter(stats.record_failures);
    var statsStatus;
    if (!available) statsStatus = "请求完成统计暂不可用，请稍后刷新。";
    else {
      statsStatus = "";
      if (!totalAvailable) statsStatus += "完成样本总数不可用。";
      else if (stats.total === 0) statsStatus += "暂无完成样本。";
      if (!failuresAvailable) statsStatus += "统计写入失败计数不可用，无法判断样本完整性。";
      else if (stats.record_failures > 0) statsStatus += "警告：本进程有 " + number(stats.record_failures) + " 次统计写入失败，样本可能不完整。";
    }
    text("activityHistoryState", available ? (!failuresAvailable ? "样本完整性不可用" : stats.record_failures > 0 ? "样本可能不完整" : "") : "统计暂不可用");
    text("ovTotalReqs", totalAvailable ? fmtNum(stats.total) : "不可用");
    text("ovSuccessRate", available ? (stats.success_rate == null ? "--" : fmtPct(stats.success_rate)) : "不可用");
    text("ovRequestStatsStatus", statsStatus);
    text("activityHistoryTooltip", "按代理请求完成及有效 WebSocket create 轮次计数；成功率 = 成功 / 总量（含失败、取消），不是服务可用率。不含代理前鉴权/IP 拒绝、健康/管理/ping、会话控制及无效或鉴权复核拒绝消息。" + (available ? "采集开始：" + (stats.since || "不可用") + "；统计区间：[" + (stats.from || "不可用") + "，" + (stats.to || "不可用") + ")。当前未结束分钟不计入。" : "请求完成统计暂不可用。"));
    text("ovTargets", counter(ov.active_targets) ? fmtNum(ov.active_targets) : "不可用");
    text("ovClients", counter(ov.client_count) ? fmtNum(ov.client_count) : "不可用");
    text("ovModelCosts", counter(ov.model_cost_count) ? fmtNum(ov.model_cost_count) : "不可用");
    var summary = isObject(ov.usage_summary) ? ov.usage_summary : null;
    var periods = [{k: "last_hour", l: "近 1 小时（滚动）"}, {k: "today", l: "今日（UTC 日）"}, {k: "yesterday", l: "昨日（UTC 日）"}, {k: "last_7_days", l: "近 7 天（滚动 7×24 小时）"}, {k: "last_30_days", l: "近 30 天（滚动 30×24 小时）"}];
    el("overviewUsageSummary").innerHTML = '<table class="activity-history-table"><thead><tr><th>区间（服务端口径）</th><th>请求权重</th><th>总 Token</th><th>历史费用</th></tr></thead><tbody>' + periods.map(function (period) {
      var data = summary && isObject(summary[period.k]) ? summary[period.k] : null;
      var requests = data && counter(data.requests) ? fmtNum(data.requests) : "不可用";
      var total = data && counter(data.input_tokens) && counter(data.output_tokens) ? fmtNum(totalTok(data)) : "不可用";
      var cost = data && counter(data.estimated_cost) ? fmtMoney(data.estimated_cost) : "不可用";
      return '<tr><th scope="row">' + esc(period.l) + '</th><td>' + esc(requests) + '</td><td>' + esc(total) + '</td><td>' + esc(cost) + '</td></tr>';
    }).join("") + '</tbody></table>';
    renderTargetHealth();
    renderRecentChanges();
    renderChain();
    if (S.tooltipTrigger && S.tooltipTrigger.dataset.activityInfoSource === "activityHistoryTooltip") showTooltip(S.tooltipTrigger);
  }
  return {renderChain: renderChain, renderHistory: renderHistory, renderRecentChanges: renderRecentChanges, renderTargetHealth: renderTargetHealth};
});
