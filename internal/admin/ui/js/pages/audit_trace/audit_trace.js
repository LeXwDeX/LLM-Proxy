"use strict";
// Audit and trace own event queries, trace pagination and request detail rendering.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("audit_trace", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtTime = app.bind("core", "fmtTime");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var isTransientError = app.bind("core", "isTransientError");
  /* ===== Audit ===== */
  var auditFlight = null;
  // A refresh while one is already running reuses it instead of stacking requests.
  function loadAudit() {
    if (auditFlight) return auditFlight;
    auditFlight = (async function () {
      try {
        var data = await api("/admin/data/audit?limit=50");
        renderAuditTable("auditTableBody", data.events || [], 6);
      } catch (e) {
        toast("加载审计日志失败: " + e.message, "error");
        document.getElementById("auditTableBody").innerHTML =
          "<tr class='empty-row'><td colspan='6'>加载失败</td></tr>";
      } finally {
        auditFlight = null;
      }
    })();
    return auditFlight;
  }
  function renderAuditTable(tbId, events, cols) {
    var tb = document.getElementById(tbId);
    if (!events.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='" +
        cols +
        "'>暂无审计记录</td></tr>";
      return;
    }
    tb.innerHTML = events
      .map(function (ev) {
        var row =
          "<tr><td>" +
          esc(fmtTime(ev.timestamp)) +
          "</td>" +
          "<td>" +
          esc(ev.actor || ev.operator || "-") +
          "</td>" +
          "<td>" +
          esc(ev.action || "-") +
          "</td>" +
          "<td>" +
          esc(ev.object || ev.target || "-") +
          "</td>" +
          "<td>" +
          esc(ev.result || ev.status || "-") +
          "</td>";
        if (cols >= 6)
          // The full detail stays in the title; clicking the cell expands it in place.
          row +=
            "<td data-audit-detail='1' title='" +
            esc(ev.detail || ev.details || "-") +
            "' style='max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;cursor:pointer;'>" +
            esc(ev.detail || ev.details || "-") +
            "</td>";
        row += "</tr>";
        return row;
      })
      .join("");
  }
  document
    .getElementById("auditTableBody")
    .addEventListener("click", function (event) {
      var cell = event.target && event.target.closest && event.target.closest("[data-audit-detail]");
      if (!cell) return;
      var expand = cell.dataset.expanded !== "true";
      cell.dataset.expanded = String(expand);
      cell.style.whiteSpace = expand ? "normal" : "nowrap";
      cell.style.maxWidth = expand ? "none" : "200px";
      cell.style.overflow = expand ? "visible" : "hidden";
      cell.style.overflowWrap = expand ? "anywhere" : "normal";
    });
  document
    .getElementById("refreshAuditBtn")
    .addEventListener("click", function () {
      loadAudit();
    });

  /* ===== Trace (DEBUG) ===== */
  var traceEnabled = false;
  var traceOffset = 0;
  // The page size the server actually applied; paging follows it, not the typed value.
  var traceEffectiveLimit = 50;
  // Bumped by every list load and by detail views so a late response is dropped.
  var traceLoadVersion = 0;
  var traceDetailVersion = 0;
  var traceClients = {};
  var traceTargets = {};

  // Braille spinner animation
  var brailleFrames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
  var brailleIndex = 0;
  var brailleTimer = null;

  function startBrailleSpinner(text) {
    // A second start must not orphan the first interval.
    if (brailleTimer) {
      clearInterval(brailleTimer);
      brailleTimer = null;
    }
    brailleIndex = 0;
    document.getElementById("traceLoadingText").textContent =
      text || "扫描历史记录...";
    document.getElementById("traceLoadingOverlay").style.display = "flex";
    brailleTimer = setInterval(function () {
      brailleIndex = (brailleIndex + 1) % brailleFrames.length;
      document.getElementById("brailleSpinnerEl").textContent =
        brailleFrames[brailleIndex];
    }, 120);
  }

  function stopBrailleSpinner() {
    if (brailleTimer) {
      clearInterval(brailleTimer);
      brailleTimer = null;
    }
    document.getElementById("traceLoadingOverlay").style.display = "none";
  }

  async function checkTraceEnabled() {
    try {
      var data = await api("/admin/data/trace/stats");
      // Lightweight probe: the server reports enabled explicitly.
      if (data && data.enabled === true) {
        traceEnabled = true;
        document.getElementById("nav-trace").style.display = "";
      } else {
        traceEnabled = false;
        document.getElementById("nav-trace").style.display = "none";
      }
    } catch (e) {
      traceEnabled = false;
      if (isTransientError(e)) throw e; // bootstrap retries, then treats trace as off
    }
  }

  function tracePageSize() {
    return parseInt(document.getElementById("traceLimit").value) || 50;
  }

  function tracePath(limit, offset) {
    // view=summary: the list never ships request/response bodies or headers.
    return "/admin/data/trace?limit=" + limit + "&offset=" + offset + "&view=summary";
  }

  // Filters apply to every page, including appended ones.
  function filterTraceRecords(records) {
    var clientFilter = document.getElementById("traceClientFilter").value;
    var targetFilter = document.getElementById("traceTargetFilter").value;
    var statusFilter = document.getElementById("traceStatusFilter").value;
    return (records || []).filter(function (r) {
      if (clientFilter && r.client_name !== clientFilter) return false;
      if (targetFilter && r.target !== targetFilter) return false;
      if (statusFilter) {
        var code = r.status_code || 0;
        if (statusFilter === "2xx") return code >= 200 && code < 300;
        if (statusFilter === "4xx") return code >= 400 && code < 500;
        if (statusFilter === "5xx") return code >= 500;
      }
      return true;
    });
  }

  function traceFilterActive() {
    return !!(
      document.getElementById("traceClientFilter").value ||
      document.getElementById("traceTargetFilter").value ||
      document.getElementById("traceStatusFilter").value
    );
  }

  function updateTraceLoadMore(data, limit) {
    var loadMoreBtn = document.getElementById("traceLoadMore");
    var count = data && data.records ? data.records.length : 0;
    loadMoreBtn.style.display = count > 0 && count >= limit ? "" : "none";
  }

  async function loadTrace() {
    if (!traceEnabled) {
      document.getElementById("traceTableBody").innerHTML =
        "<tr class='empty-row'><td colspan='9'>Trace Store 未启用</td></tr>";
      return;
    }

    var version = ++traceLoadVersion;
    traceOffset = 0; // 重置偏移量
    var limit = tracePageSize();
    var searchId = document.getElementById("traceSearchId").value.trim();

    startBrailleSpinner("加载请求记录...");
    try {
      // If searching by ID, use the single-record endpoint
      if (searchId) {
        var record = await api(
          "/admin/data/trace/" + encodeURIComponent(searchId),
        );
        if (version !== traceLoadVersion) return;
        if (record && record.trace_id) {
          renderTraceTable([record], {});
        } else {
          renderTraceTable([], {});
        }
        document.getElementById("traceLoadMore").style.display = "none";
        return;
      }

      var data = await api(
        tracePath(limit, traceOffset),
        { timeoutMs: 60000 }, // trace pages scan the trace store
      );
      if (version !== traceLoadVersion) return;
      var raw = data.records || [];
      traceEffectiveLimit = data.limit > 0 ? data.limit : limit;
      traceClients = {};
      traceTargets = {};
      populateTraceFilters(raw);
      renderTraceTable(filterTraceRecords(raw), data.stats || {}, {
        filtered: traceFilterActive() && raw.length > 0,
      });
      updateTraceLoadMore(data, traceEffectiveLimit);
    } catch (e) {
      if (version !== traceLoadVersion) return;
      toast("加载请求记录失败: " + e.message, "error");
      document.getElementById("traceTableBody").innerHTML =
        "<tr class='empty-row'><td colspan='9'>加载失败</td></tr>";
    } finally {
      if (version === traceLoadVersion) stopBrailleSpinner();
    }
  }

  async function loadTraceMore() {
    var version = ++traceLoadVersion;
    var offset = traceOffset + traceEffectiveLimit;
    startBrailleSpinner("扫描历史记录...");
    try {
      var data = await api(
        tracePath(traceEffectiveLimit, offset),
        { timeoutMs: 60000 }, // trace pages scan the trace store
      );
      if (version !== traceLoadVersion) return;
      traceOffset = offset;
      if (data && data.limit > 0) traceEffectiveLimit = data.limit;
      if (data && data.records && data.records.length > 0) {
        populateTraceFilters(data.records);
        appendTraceRecords(filterTraceRecords(data.records));
        updateTraceLoadMore(data, traceEffectiveLimit);
      } else {
        document.getElementById("traceLoadMore").style.display = "none";
      }
    } catch (e) {
      if (version !== traceLoadVersion) return;
      toast("加载更多记录失败: " + e.message, "error");
    } finally {
      if (version === traceLoadVersion) stopBrailleSpinner();
    }
  }

  function traceRowHTML(r) {
    var statusClass = "";
    var code = r.status_code || 0;
    if (code >= 200 && code < 300) statusClass = "color:var(--success)";
    else if (code >= 400 && code < 500) statusClass = "color:var(--warning)";
    else if (code >= 500) statusClass = "color:var(--danger)";

    var tokens = "";
    if (r.input_tokens || r.output_tokens) {
      tokens = (r.input_tokens || 0) + " / " + (r.output_tokens || 0);
    }

    return (
      "<tr>" +
      "<td>" +
      esc(fmtTime(r.timestamp)) +
      "</td>" +
      "<td style='font-family:var(--font-mono);font-size:12px;'>" +
      esc((r.trace_id || "").substring(0, 8)) +
      "...</td>" +
      "<td>" +
      esc(r.client_name || "-") +
      "</td>" +
      "<td>" +
      esc(r.target || "-") +
      "</td>" +
      "<td>" +
      esc(r.model || "-") +
      "</td>" +
      "<td style='" +
      statusClass +
      ";font-weight:600;'>" +
      code +
      "</td>" +
      "<td>" +
      (r.duration_ms || 0) +
      "ms</td>" +
      "<td style='font-size:12px;'>" +
      tokens +
      "</td>" +
      "<td><button class='btn btn-sm' data-trace-id='" +
      esc(r.trace_id) +
      "'>详情</button></td>" +
      "</tr>"
    );
  }

  // One string, one assignment per page: no per-row innerHTML += reparsing.
  function appendTraceRecords(records) {
    if (!records || !records.length) return;
    var tb = document.getElementById("traceTableBody");
    var existing = tb.innerHTML;
    // An empty/placeholder row must not stay mixed with real rows.
    if (existing.indexOf("empty-row") >= 0) existing = "";
    tb.innerHTML = existing + records.map(traceRowHTML).join("");
  }

  // Filter choices accumulate over every loaded page, so appended records can be
  // filtered by clients and targets that only appear on later pages.
  function populateTraceFilters(records) {
    (records || []).forEach(function (r) {
      if (r.client_name) traceClients[r.client_name] = true;
      if (r.target) traceTargets[r.target] = true;
    });
    function options(select, all, seen) {
      var current = select.value;
      // Keep the selection even when the reloaded first page lacks it, so the
      // filter stays applied instead of silently reverting to "all".
      if (current) seen[current] = true;
      select.innerHTML =
        "<option value=''>" + all + "</option>" +
        Object.keys(seen)
          .sort()
          .map(function (name) {
            return (
              "<option value='" + esc(name) + "'" +
              (name === current ? " selected" : "") + ">" + esc(name) + "</option>"
            );
          })
          .join("");
      select.value = current;
    }
    options(document.getElementById("traceClientFilter"), "全部客户端", traceClients);
    options(document.getElementById("traceTargetFilter"), "全部目标", traceTargets);
  }

  function renderTraceTable(records, stats, opts) {
    var tb = document.getElementById("traceTableBody");
    var statsDiv = document.getElementById("traceStats");

    // Render stats
    statsDiv.innerHTML =
      "<div class='stat-item'>总记录: <span class='stat-value'>" +
      (stats.total_records || 0) +
      "</span></div>" +
      "<div class='stat-item'>磁盘写入: <span class='stat-value'>" +
      (stats.disk_writes || 0) +
      "</span></div>" +
      "<div class='stat-item'>丢弃: <span class='stat-value'>" +
      (stats.dropped_records || 0) +
      "</span></div>" +
      "<div class='stat-item'>缓冲区: <span class='stat-value'>" +
      (stats.ring_buffer_size || 0) +
      "</span></div>";

    if (!records.length) {
      tb.innerHTML =
        "<tr class='empty-row'><td colspan='9'>" +
        (opts && opts.filtered
          ? "已加载页中没有符合当前筛选的记录，可加载更早的记录"
          : "暂无请求记录") +
        "</td></tr>";
      return;
    }

    tb.innerHTML = records.map(traceRowHTML).join("");
  }

  function utf8Bytes(str) {
    var bytes = 0;
    str = String(str || "");
    for (var i = 0; i < str.length; i++) {
      var code = str.charCodeAt(i);
      if (code < 0x80) bytes += 1;
      else if (code < 0x800) bytes += 2;
      else if (code >= 0xd800 && code <= 0xdbff && i + 1 < str.length) {
        bytes += 4; // surrogate pair
        i++;
      } else bytes += 3;
    }
    return bytes;
  }

  async function showTraceDetail(traceId) {
    var version = ++traceDetailVersion;
    var modal = document.getElementById("traceDetailModal");
    var body = document.getElementById("traceDetailBody");
    modal.style.display = "flex";
    body.innerHTML =
      "<div class='loading-overlay'><span class='loading-spinner'></span> 加载中...</div>";

    try {
      var r = await api("/admin/data/trace/" + encodeURIComponent(traceId));
      if (version !== traceDetailVersion) return;
      if (!r || !r.trace_id) {
        body.innerHTML = "<p>未找到记录</p>";
        return;
      }

      var html = "";

      // META: 请求侧
      html +=
        "<div class='trace-detail-section'><h4>请求信息</h4><div class='trace-detail-grid'>";
      html += detailItem("Trace ID", r.trace_id);
      html += detailItem("时间", fmtTime(r.timestamp));
      html += detailItem("客户端", r.client_name);
      html += detailItem("客户端 IP", r.client_ip);
      html += detailItem("Access Key", r.client_access_key);
      html += detailItem("Method", r.method);
      html += detailItem("Path", r.path);
      html += detailItem("Query", r.query_params || "-");
      html += "</div></div>";

      // META: 路由决策
      html +=
        "<div class='trace-detail-section'><h4>路由决策</h4><div class='trace-detail-grid'>";
      html += detailItem("目标", r.target);
      html += detailItem("Endpoint Type", r.endpoint_type);
      html += detailItem("模型", r.model);
      html += detailItem("选择方式", r.selection_kind);
      html += detailItem("Key Index", r.key_index);
      html += detailItem("Key Mask", r.key_mask);
      html += "</div></div>";

      // META: 上游
      html +=
        "<div class='trace-detail-section'><h4>上游响应</h4><div class='trace-detail-grid'>";
      html += detailItem("Upstream URL", r.upstream_url);
      html += detailItem("Upstream Request ID", r.upstream_request_id);
      html += detailItem("Status Code", r.status_code);
      html += detailItem("耗时", (r.duration_ms || 0) + "ms");
      html += detailItem("Input Tokens", r.input_tokens);
      html += detailItem("Output Tokens", r.output_tokens);
      html += detailItem("Cached Tokens (Read)", r.cached_tokens);
      if (r.cache_creation_tokens)
        html += detailItem("缓存写入 Token", r.cache_creation_tokens);
      html += "</div></div>";

      // Request Body
      html +=
        "<div class='trace-detail-section'><h4>Request Body (" +
        utf8Bytes(r.request_body) +
        " bytes)</h4>";
      html +=
        "<div class='trace-body-preview'>" +
        esc(formatJson(r.request_body)) +
        "</div></div>";

      // Response Body
      html +=
        "<div class='trace-detail-section'><h4>Response Body (" +
        utf8Bytes(r.response_body) +
        " bytes)</h4>";
      html +=
        "<div class='trace-body-preview'>" +
        esc(formatJson(r.response_body)) +
        "</div></div>";

      body.innerHTML = html;
    } catch (e) {
      if (version !== traceDetailVersion) return;
      body.innerHTML = "<p>加载失败: " + esc(e.message) + "</p>";
    }
  }

  function closeTraceDetail() {
    document.getElementById("traceDetailModal").style.display = "none";
  }

  function detailItem(label, value) {
    return (
      "<div class='trace-detail-item'><span class='label'>" +
      esc(label) +
      "</span><span class='value'>" +
      esc(String(value || "-")) +
      "</span></div>"
    );
  }

  function formatJson(str) {
    if (!str) return "-";
    try {
      return JSON.stringify(JSON.parse(str), null, 2);
    } catch (e) {
      return str;
    }
  }

  document
    .getElementById("traceTableBody")
    .addEventListener("click", async function (event) {
      var button = event.target.closest("[data-trace-id]");
      if (button) await showTraceDetail(button.dataset.traceId);
    });
  document
    .getElementById("traceLoadMore")
    .addEventListener("click", loadTraceMore);
  document
    .getElementById("refreshTraceBtn")
    .addEventListener("click", function () {
      loadTrace();
    });
  document
    .getElementById("searchTraceBtn")
    .addEventListener("click", function () {
      loadTrace();
    });
  document
    .getElementById("traceSearchId")
    .addEventListener("keypress", function (e) {
      if (e.key === "Enter") loadTrace();
    });

  // 静态关闭按钮仍通过 HTML 调用；请求标识只作为 data 属性传入。
  window.closeTraceDetail = closeTraceDetail;

  return {
    loadAudit: loadAudit,
    renderAuditTable: renderAuditTable,
    startBrailleSpinner: startBrailleSpinner,
    stopBrailleSpinner: stopBrailleSpinner,
    checkTraceEnabled: checkTraceEnabled,
    loadTrace: loadTrace,
    loadTraceMore: loadTraceMore,
    appendTraceRecords: appendTraceRecords,
    populateTraceFilters: populateTraceFilters,
    filterTraceRecords: filterTraceRecords,
    renderTraceTable: renderTraceTable,
    showTraceDetail: showTraceDetail,
    closeTraceDetail: closeTraceDetail,
    detailItem: detailItem,
    formatJson: formatJson,
  };
});
