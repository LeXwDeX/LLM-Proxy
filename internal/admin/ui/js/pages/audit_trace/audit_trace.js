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
  async function loadAudit() {
    try {
      var data = await api("/admin/data/audit?limit=50");
      renderAuditTable("auditTableBody", data.events || [], 6);
    } catch (e) {
      toast("加载审计日志失败: " + e.message, "error");
      document.getElementById("auditTableBody").innerHTML =
        "<tr class='empty-row'><td colspan='6'>加载失败</td></tr>";
    }
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
          row +=
            "<td style='max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;'>" +
            esc(ev.detail || ev.details || "-") +
            "</td>";
        row += "</tr>";
        return row;
      })
      .join("");
  }
  document
    .getElementById("refreshAuditBtn")
    .addEventListener("click", function () {
      loadAudit();
    });

  /* ===== Trace (DEBUG) ===== */
  var traceEnabled = false;
  var traceOffset = 0;

  // Braille spinner animation
  var brailleFrames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
  var brailleIndex = 0;
  var brailleTimer = null;

  function startBrailleSpinner(text) {
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
      var data = await api("/admin/data/trace?limit=1");
      if (data && data.stats) {
        traceEnabled = true;
        document.getElementById("nav-trace").style.display = "";
      }
    } catch (e) {
      traceEnabled = false;
      if (isTransientError(e)) throw e; // bootstrap retries, then treats trace as off
    }
  }

  async function loadTrace() {
    if (!traceEnabled) {
      document.getElementById("traceTableBody").innerHTML =
        "<tr class='empty-row'><td colspan='9'>Trace Store 未启用</td></tr>";
      return;
    }

    traceOffset = 0; // 重置偏移量
    var limit = parseInt(document.getElementById("traceLimit").value) || 50;
    var searchId = document.getElementById("traceSearchId").value.trim();
    var clientFilter = document.getElementById("traceClientFilter").value;
    var targetFilter = document.getElementById("traceTargetFilter").value;
    var statusFilter = document.getElementById("traceStatusFilter").value;

    startBrailleSpinner("加载请求记录...");
    try {
      // If searching by ID, use the single-record endpoint
      if (searchId) {
        var record = await api(
          "/admin/data/trace/" + encodeURIComponent(searchId),
        );
        stopBrailleSpinner();
        if (record && record.trace_id) {
          renderTraceTable([record], {});
        } else {
          renderTraceTable([], {});
        }
        return;
      }

      var data = await api(
        "/admin/data/trace?limit=" + limit + "&offset=" + traceOffset,
        { timeoutMs: 60000 }, // trace pages scan the trace store
      );
      var records = data.records || [];

      // Client-side filtering
      if (clientFilter) {
        records = records.filter(function (r) {
          return r.client_name === clientFilter;
        });
      }
      if (targetFilter) {
        records = records.filter(function (r) {
          return r.target === targetFilter;
        });
      }
      if (statusFilter) {
        records = records.filter(function (r) {
          var code = r.status_code || 0;
          if (statusFilter === "2xx") return code >= 200 && code < 300;
          if (statusFilter === "4xx") return code >= 400 && code < 500;
          if (statusFilter === "5xx") return code >= 500;
          return true;
        });
      }

      renderTraceTable(records, data.stats || {});
      populateTraceFilters(data.records || []);

      // Check if there are more records to load
      var loadMoreBtn = document.getElementById("traceLoadMore");
      if (data.records && data.records.length >= limit) {
        loadMoreBtn.style.display = "";
      } else {
        loadMoreBtn.style.display = "none";
      }
    } catch (e) {
      toast("加载请求记录失败: " + e.message, "error");
      document.getElementById("traceTableBody").innerHTML =
        "<tr class='empty-row'><td colspan='9'>加载失败</td></tr>";
    }
    stopBrailleSpinner();
  }

  async function loadTraceMore() {
    traceOffset += parseInt(document.getElementById("traceLimit").value) || 50;
    var limit = parseInt(document.getElementById("traceLimit").value) || 50;
    startBrailleSpinner("扫描历史记录...");
    try {
      var data = await api(
        "/admin/data/trace?limit=" + limit + "&offset=" + traceOffset,
        { timeoutMs: 60000 }, // trace pages scan the trace store
      );
      stopBrailleSpinner();
      if (data && data.records && data.records.length > 0) {
        appendTraceRecords(data.records);
        var loadMoreBtn = document.getElementById("traceLoadMore");
        if (data.records.length >= limit) {
          loadMoreBtn.style.display = "";
        } else {
          loadMoreBtn.style.display = "none";
        }
      } else {
        document.getElementById("traceLoadMore").style.display = "none";
      }
    } catch (e) {
      stopBrailleSpinner();
      toast("加载更多记录失败: " + e.message, "error");
    }
  }

  function appendTraceRecords(records) {
    var tb = document.getElementById("traceTableBody");
    for (var i = 0; i < records.length; i++) {
      var r = records[i];
      var statusClass = "";
      var code = r.status_code || 0;
      if (code >= 200 && code < 300) statusClass = "color:var(--success)";
      else if (code >= 400 && code < 500) statusClass = "color:var(--warning)";
      else if (code >= 500) statusClass = "color:var(--danger)";

      var tokens = "";
      if (r.input_tokens || r.output_tokens) {
        tokens = (r.input_tokens || 0) + " / " + (r.output_tokens || 0);
      }

      var row =
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
        "</tr>";
      tb.innerHTML += row;
    }
  }

  function populateTraceFilters(records) {
    var clients = {};
    var targets = {};
    records.forEach(function (r) {
      if (r.client_name) clients[r.client_name] = true;
      if (r.target) targets[r.target] = true;
    });

    var clientSelect = document.getElementById("traceClientFilter");
    var currentClient = clientSelect.value;
    clientSelect.innerHTML = "<option value=''>全部客户端</option>";
    Object.keys(clients)
      .sort()
      .forEach(function (c) {
        clientSelect.innerHTML +=
          "<option value='" +
          esc(c) +
          "'" +
          (c === currentClient ? " selected" : "") +
          ">" +
          esc(c) +
          "</option>";
      });

    var targetSelect = document.getElementById("traceTargetFilter");
    var currentTarget = targetSelect.value;
    targetSelect.innerHTML = "<option value=''>全部目标</option>";
    Object.keys(targets)
      .sort()
      .forEach(function (t) {
        targetSelect.innerHTML +=
          "<option value='" +
          esc(t) +
          "'" +
          (t === currentTarget ? " selected" : "") +
          ">" +
          esc(t) +
          "</option>";
      });
  }

  function renderTraceTable(records, stats) {
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
        "<tr class='empty-row'><td colspan='9'>暂无请求记录</td></tr>";
      return;
    }

    tb.innerHTML = records
      .map(function (r) {
        var statusClass = "";
        var code = r.status_code || 0;
        if (code >= 200 && code < 300) statusClass = "color:var(--success)";
        else if (code >= 400 && code < 500)
          statusClass = "color:var(--warning)";
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
      })
      .join("");
  }

  async function showTraceDetail(traceId) {
    var modal = document.getElementById("traceDetailModal");
    var body = document.getElementById("traceDetailBody");
    modal.style.display = "flex";
    body.innerHTML =
      "<div class='loading-overlay'><span class='loading-spinner'></span> 加载中...</div>";

    try {
      var r = await api("/admin/data/trace/" + encodeURIComponent(traceId));
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
        (r.request_body ? r.request_body.length : 0) +
        " bytes)</h4>";
      html +=
        "<div class='trace-body-preview'>" +
        esc(formatJson(r.request_body)) +
        "</div></div>";

      // Response Body
      html +=
        "<div class='trace-detail-section'><h4>Response Body (" +
        (r.response_body ? r.response_body.length : 0) +
        " bytes)</h4>";
      html +=
        "<div class='trace-body-preview'>" +
        esc(formatJson(r.response_body)) +
        "</div></div>";

      body.innerHTML = html;
    } catch (e) {
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
    renderTraceTable: renderTraceTable,
    showTraceDetail: showTraceDetail,
    closeTraceDetail: closeTraceDetail,
    detailItem: detailItem,
    formatJson: formatJson,
  };
});
