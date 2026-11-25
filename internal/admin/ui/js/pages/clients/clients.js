"use strict";
// Clients owns client CRUD and target/pool permission pickers.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("clients", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var usageWindowQuery = app.bind("core", "usageWindowQuery");
  var fmtNum = app.bind("core", "fmtNum");
  var fmtMoney = app.bind("core", "fmtMoney");
  var totalTok = app.bind("core", "totalTok");
  var maskKey = app.bind("core", "maskKey");
  var createTableSort = app.bind("core", "createTableSort");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var isUnconfirmedOutcome = app.bind("core", "isUnconfirmedOutcome");
  var epShortLabel = app.bind("schema", "epShortLabel");
  var poolMemberCounter = app.bind("pools", "poolMemberCounter");
  var poolDisplayName = app.bind("pools", "poolDisplayName");
  var loadTargets = app.bind("targets", "loadTargets");
  var targetPoolsAvailable = app.bind("targets", "targetPoolsAvailable");
  var isReadonlyMode = app.bind("users", "isReadonlyMode");
  var loadDictOptions = app.bind("quota", "loadDictOptions");
  var dictEmptyLabel = app.bind("quota", "dictEmptyLabel");
  var dictOptionsHTML = app.bind("quota", "dictOptionsHTML");
  var clientLoadVersion = 0;
  var clientOpenVersion = 0;
  var clientDraftRevision = 0;
  var clientSaveInFlight = false;

  // Issue #240: a slow save must not look like a frozen dialog. The button is disabled and
  // relabelled for the duration of the request and always restored (success, failure, unconfirmed).
  function setClientSaveBusy(busy) {
    var btn = document.getElementById("saveClientBtn");
    if (!btn) return;
    btn.disabled = busy;
    btn.textContent = busy ? "保存中…" : "保存";
  }
  var clientCreatedDraftNeedsReopen = false;
  var clientSavingName = "";
  var clientVersions = Object.create(null);
  var editingClientVersion = 0;
  // Issue 220: each picker keeps its own "the user already chose here" marker, so late data for
  // one roster never rewrites a click while the other picker may still take a newer list.
  var clientTargetsTouched = false;
  var clientPoolsTouched = false;
  var clientUsageAbort = null;
  var clientUsageSeq = 0;
  var clientRenderCache = { html: null };
  // In-page search and paging (Issue 210). The server keeps returning the full list; these only decide
  // what is rendered. The search covers the fields the table already shows, and the page size is one
  // of a fixed set with a fixed default.
  var CLIENT_PAGE_SIZES = [20, 50, 100, 200];
  var CLIENT_PAGE_SIZE_DEFAULT = 50;
  var clientSearchTerm = "";
  var clientSearchTimer = null;
  var clientPage = 1;
  var clientPageSize = CLIENT_PAGE_SIZE_DEFAULT;
  // "pending" | "ready" | "error": the 30-day usage summary is fetched separately from the list, so
  // the usage columns (and their sort values) have a state of their own.
  if (!state.clientUsageStatus) state.clientUsageStatus = "pending";
  // "pending" | "ready" | "error": the client list itself. A failed or still-loading list must
  // never read as an empty list.
  if (!state.clientListStatus) state.clientListStatus = "pending";
  // Issue 220: editor readiness. A create may only be saved once the target/pool roster and the
  // job-level/project-group dictionaries have actually loaded; edits fall back to the saved record.
  var clientEditorData = { version: 0, status: "idle" };
  var AUTH_RELOAD_PENDING_MESSAGE = "数据已保存，但鉴权尚未生效；请稍后刷新或重试确认";
  function clientUsageReady() {
    return state.clientUsageStatus === "ready";
  }
  // Column sorting (shared createTableSort). Getters return the underlying value, not the display text:
  // usage columns are numbers while the summary is ready and null (empty, always last) otherwise,
  // the Access Key sorts by its masked text (the raw key never drives order),
  // and a client without any target/pool restriction counts as having the most access.
  function clientUsageOf(c) {
    return (state.clientUsage && state.clientUsage[c.name]) || {};
  }
  var clientSort = createTableSort({
    columns: {
      name: function (c) { return String(c.name || ""); },
      ip: function (c) { return c.ip_address || ""; },
      key: function (c) { return c.access_key ? maskKey(c.access_key) : ""; },
      targets: function (c) {
        var granted = (Array.isArray(c.allowed_targets) ? c.allowed_targets.length : 0) +
          (Array.isArray(c.allowed_pools) ? c.allowed_pools.length : 0);
        return granted > 0 ? granted : Number.MAX_SAFE_INTEGER;
      },
      tokens: function (c) { return clientUsageReady() ? totalTok(clientUsageOf(c)) : null; },
      cost: function (c) { return clientUsageReady() ? Number(clientUsageOf(c).estimated_cost || 0) : null; },
      quota: function (c) { return c.job_level || ""; },
    },
    // A new sort order always starts from the first page of its result.
    onChange: function () {
      clientPage = 1;
      renderClients();
    },
  });
  clientSort.bind(document.getElementById("clientsTableHead"));
  ["input", "change"].forEach(function (eventName) {
    document.getElementById("clientModal").addEventListener(eventName, function () { clientDraftRevision++; });
  });
  document.getElementById("clientModal").addEventListener("click", function (event) {
    if (event.target.dataset.target) {
      clientDraftRevision++;
      clientTargetsTouched = true;
    }
    if (event.target.dataset.pool) {
      clientDraftRevision++;
      clientPoolsTouched = true;
    }
  });
  /* ===== Clients ===== */
  // The client list and the 30-day usage summary are independent: GET /admin/data/clients assigns
  // state.clients and renders at once, while the summary fills only the usage columns. A hung
  // or failed summary never blocks the list or its row actions (Issue #208).
  async function loadClients() {
    var version = ++clientLoadVersion;
    var prefetch = null;
    state.clientListStatus = "pending";
    // Targets feed the access pickers only. Prefetch when none are loaded, and never let that
    // side request fail the clients list itself.
    // Also retry when the pools roster is unavailable (an earlier fetch failed).
    if (!state.targets || !state.targets.length || !targetPoolsAvailable()) {
      prefetch = loadTargets(true).catch(function () { return null; });
    }
    loadClientUsage();
    try {
      var r = await api("/admin/data/clients");
      if (version !== clientLoadVersion) return;
      state.clients = Array.isArray(r.clients) ? r.clients : [];
      state.clientListStatus = "ready";
      renderClients();
    } catch (e) {
      if (version !== clientLoadVersion) return;
      state.clientListStatus = "error";
      renderClients();
      toast("加载客户端失败: " + e.message, "error");
    }
    if (prefetch) {
      await prefetch;
      if (version === clientLoadVersion) renderClients();
    }
  }
  // The usage summary is cancelled by the next usage load and when the page goes away; an AbortError
  // is a cancellation, never a failure.
  function cancelClientUsageLoad() {
    if (!clientUsageAbort) return;
    var controller = clientUsageAbort;
    clientUsageAbort = null;
    controller.abort();
  }
  function loadClientUsage() {
    var seq = ++clientUsageSeq;
    cancelClientUsageLoad();
    var controller = typeof AbortController === "function" ? new AbortController() : null;
    clientUsageAbort = controller;
    state.clientUsageStatus = "pending";
    renderClientUsageStatus();
    renderClients();
    return api(
      "/admin/data/usage/by-client?" + usageWindowQuery(30),
      { timeoutMs: 60000, signal: controller ? controller.signal : undefined },
    )
      .then(
        function (r) {
          if (seq !== clientUsageSeq) return;
          state.clientUsage = {};
          ((r && r.clients) || []).forEach(function (item) {
            state.clientUsage[item.key] = item;
          });
          state.clientUsageStatus = "ready";
        },
        function (e) {
          if (seq !== clientUsageSeq) return;
          if (e && e.name === "AbortError") return;
          state.clientUsageStatus = "error";
        },
      )
      .then(function () {
        if (clientUsageAbort === controller) clientUsageAbort = null;
        if (seq !== clientUsageSeq) return;
        renderClientUsageStatus();
        renderClients();
      });
  }
  function renderClientUsageStatus() {
    var bar = document.getElementById("clientsUsageStatus");
    if (!bar) return;
    var status = state.clientUsageStatus || "pending";
    bar.style.display = status === "ready" ? "none" : "";
    var text = document.getElementById("clientsUsageStatusText");
    if (text) {
      text.textContent = status === "error"
        ? "近30天用量加载失败，Token 与费用暂不可用。"
        : "近30天用量加载中…";
    }
    var retry = document.getElementById("retryClientsUsageBtn");
    if (retry) retry.style.display = status === "error" ? "" : "none";
  }
  if (typeof window !== "undefined" && typeof window.addEventListener === "function") {
    window.addEventListener("pagehide", cancelClientUsageLoad);
  }
  // After a write only the server-normalized client records are needed; the slow 30-day usage summary
  // is never re-requested by a save or delete, and an in-flight one simply finishes on its own.
  async function refreshClientsLight() {
    var version = ++clientLoadVersion;
    try {
      var r = await api("/admin/data/clients");
      if (version !== clientLoadVersion) return;
      state.clients = Array.isArray(r.clients) ? r.clients : [];
      state.clientListStatus = "ready";
    } catch (e) {
      if (version !== clientLoadVersion) return;
      state.clientListStatus = "error";
      toast("加载客户端失败: " + e.message, "error");
    }
    renderClients();
  }
  // Case-insensitive contains match over the displayed fields: name, IP, masked Access Key, job level
  // and the granted target/pool names. The raw Access Key is never a search source, because readonly
  // users only receive the masked value.
  function clientSearchText(c) {
    var fields = [c.name, c.ip_address, c.access_key ? maskKey(c.access_key) : "", c.job_level];
    if (Array.isArray(c.allowed_targets)) fields = fields.concat(c.allowed_targets);
    if (Array.isArray(c.allowed_pools)) fields = fields.concat(c.allowed_pools);
    return fields
      .map(function (value) {
        return String(value == null ? "" : value);
      })
      .join(" ")
      .toLowerCase();
  }
  // Fixed render order (Issue 210): filter every client, sort the whole match set, then page. The sort
  // therefore never applies to only the visible page.
  function clientMatchedRows() {
    var clients = Array.isArray(state.clients) ? state.clients : [];
    var matched = clientSearchTerm
      ? clients.filter(function (c) {
        return clientSearchText(c).indexOf(clientSearchTerm) >= 0;
      })
      : clients;
    return clientSort.apply(matched, function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
  }
  // Issue 211: after a write whose outcome is unknown, read the client list back and judge
  // by content. This is a plain list GET, never the 30-day aggregate; null means the
  // readback itself failed or a newer load superseded it, and the caller must keep whatever
  // it already shows (never write an older list over a newer one).
  async function readbackClients() {
    var version = ++clientLoadVersion;
    try {
      var r = await api("/admin/data/clients");
      if (version !== clientLoadVersion) return null;
      return Array.isArray(r.clients) ? r.clients : [];
    } catch (e) {
      return null;
    }
  }
  function trimmedClientField(value) {
    return typeof value === "string" ? value.trim() : "";
  }
  // Mirrors normalizeClient for allowed_targets/allowed_pools: trim, lowercase, drop empty
  // entries and duplicates, then sort so order never decides the comparison.
  function normalizedClientGrants(list) {
    var seen = Object.create(null);
    var out = [];
    (Array.isArray(list) ? list : []).forEach(function (item) {
      var value = String(item == null ? "" : item).trim().toLowerCase();
      if (!value || seen[value]) return;
      seen[value] = true;
      out.push(value);
    });
    return out.sort();
  }
  function clientIPv4(text) {
    var match = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(text);
    if (!match) return null;
    var octets = [];
    for (var i = 1; i <= 4; i++) {
      var part = match[i];
      // netip rejects leading zeros, so "01.2.3.4" is not an address the server masks.
      if (part.length > 1 && part.charAt(0) === "0") return null;
      var value = Number(part);
      if (value > 255) return null;
      octets.push(value);
    }
    return octets;
  }
  function maskedIPv4(octets, bits) {
    var value =
      ((((octets[0] << 24) >>> 0) + (octets[1] << 16) + (octets[2] << 8) + octets[3]) >>> 0);
    var mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0;
    value = (value & mask) >>> 0;
    return [(value >>> 24) & 255, (value >>> 16) & 255, (value >>> 8) & 255, value & 255].join(".");
  }
  // Mirrors clientip.Parse (internal/clientip): comma/whitespace separated IPv4 hosts and
  // CIDR ranges, each masked to its network, first occurrence per prefix wins, joined with
  // commas. null means the value is not canonicalizable; callers compare it trimmed.
  function canonicalClientIP(raw) {
    var text = String(raw == null ? "" : raw).trim();
    if (!text) return "";
    var seen = Object.create(null);
    var out = [];
    var groups = text.split(",");
    for (var i = 0; i < groups.length; i++) {
      var group = groups[i].trim();
      if (!group) return null;
      var entries = group.split(/\s+/);
      for (var j = 0; j < entries.length; j++) {
        var entry = entries[j];
        var prefix = false;
        var bits = 32;
        var address = entry;
        var slash = entry.indexOf("/");
        if (slash >= 0) {
          prefix = true;
          address = entry.slice(0, slash);
          var rawBits = entry.slice(slash + 1);
          if (!/^\d{1,2}$/.test(rawBits)) return null;
          if (rawBits.length > 1 && rawBits.charAt(0) === "0") return null;
          bits = Number(rawBits);
          if (bits > 32) return null;
        }
        var octets = clientIPv4(address);
        if (!octets) return null;
        // A bare host is compared as its /32 network, so it dedupes against the same CIDR.
        var key = maskedIPv4(octets, bits) + "/" + bits;
        if (seen[key]) continue;
        seen[key] = true;
        out.push(prefix ? key : octets.join("."));
      }
    }
    return out.join(",");
  }
  function clientIPEqual(submitted, stored) {
    var a = canonicalClientIP(submitted);
    var b = canonicalClientIP(stored);
    if (a !== null && b !== null) return a === b;
    return trimmedClientField(submitted) === trimmedClientField(stored);
  }
  function clientGrantsEqual(submitted, stored) {
    var a = normalizedClientGrants(submitted);
    var b = normalizedClientGrants(stored);
    return a.length === b.length && a.every(function (value, i) { return value === b[i]; });
  }
  // Does the server's copy hold the fields this request actually sent? A field absent from
  // the payload never participates in the decision.
  function clientHoldsRequest(fresh, sent) {
    if (!fresh || !sent) return false;
    var sameText = function (a, b) {
      return trimmedClientField(a) === trimmedClientField(b);
    };
    var checks = {
      name: sameText,
      access_key: sameText,
      ip_address: clientIPEqual,
      allowed_targets: clientGrantsEqual,
      allowed_pools: clientGrantsEqual,
      job_level: sameText,
      project_group: sameText,
    };
    return Object.keys(checks).every(function (field) {
      if (!Object.prototype.hasOwnProperty.call(sent, field)) return true;
      return checks[field](sent[field], fresh[field]);
    });
  }
  function findClientByName(clients, name) {
    return clients.find(function (client) {
      return trimmedClientField(client.name) === name;
    });
  }
  // The follow-ups of a committed save: version bump and the draft/close decision. The caller
  // repaints once from the server's list, whether it came from the write itself or a readback.
  function adoptSavedClient(payload, saveVersion, saveRevision, wasEditing) {
    var name = payload.name;
    ++clientLoadVersion;
    clientVersions[name] = (clientVersions[name] || 0) + 1;
    if (saveVersion === clientOpenVersion && wasEditing) editingClientVersion = clientVersions[name];
    if (saveVersion === clientOpenVersion && saveRevision === clientDraftRevision) {
      closeModal("clientModal");
    } else if (saveVersion === clientOpenVersion) {
      if (!wasEditing) clientCreatedDraftNeedsReopen = true;
      toast(wasEditing ? "请求时的内容已保存，后续修改尚未保存。" :
        "客户端已创建，后续修改未保存。请复制草稿并重新打开客户端编辑。", "error");
    }
  }
  // Issue 211: after a timeout, network error, 5xx or non-JSON error body the save may have
  // been applied. Read the list back and decide by content: the server holds the request ->
  // saved, with the normal success follow-ups; a different or missing record -> keep the
  // draft and report the unconfirmed outcome; a failed readback -> keep the list and retry.
  // Issue 220: a server answer that says the auth reload is still pending means the data is
  // there but new credentials are not enforced yet, so the confirmation stays a warning.
  async function reconcileUnconfirmedSave(payload, saveVersion, saveRevision, wasEditing, error) {
    var clients = await readbackClients();
    if (!clients) {
      toast("保存结果未确认，可重试", "warn");
      return;
    }
    state.clients = clients;
    state.clientListStatus = "ready";
    renderClients();
    var fresh = findClientByName(clients, payload.name);
    if (clientHoldsRequest(fresh, payload)) {
      toast(
        error && error.code === "auth_reload_pending"
          ? AUTH_RELOAD_PENDING_MESSAGE
          : "已保存（已回读确认）",
        error && error.code === "auth_reload_pending" ? "warn" : "success",
      );
      adoptSavedClient(payload, saveVersion, saveRevision, wasEditing);
      return;
    }
    toast(
      fresh
        ? "保存结果未确认，服务器内容与提交不一致，请刷新后重试"
        : "保存结果未确认，可重试",
      "warn",
    );
  }
  // Issue 211: a DELETE whose outcome is unknown. An absent record means it was deleted
  // (the success path, applied locally); a record still there only refreshes the table.
  async function reconcileUnconfirmedDelete(name, error) {
    var clients = await readbackClients();
    if (!clients) {
      toast("删除结果未确认，可重试", "warn");
      return;
    }
    state.clients = clients;
    state.clientListStatus = "ready";
    if (findClientByName(clients, name)) {
      toast("删除结果未确认，列表已刷新", "warn");
    } else {
      toast(
        error && error.code === "auth_reload_pending"
          ? AUTH_RELOAD_PENDING_MESSAGE
          : "已删除客户端: " + name,
        error && error.code === "auth_reload_pending" ? "warn" : "success",
      );
      if (state.clientUsage) delete state.clientUsage[name];
    }
    renderClients();
  }
  function renderClients() {
    var tb = document.getElementById("clientsTableBody");
    var usageStatus = state.clientUsageStatus || "pending";
    var usagePlaceholder =
      usageStatus === "ready" ? null : usageStatus === "error" ? "加载失败" : "加载中…";
    var rows = clientMatchedRows();
    var total = rows.length;
    var pages = Math.max(1, Math.ceil(total / clientPageSize));
    if (clientPage < 1) clientPage = 1;
    if (clientPage > pages) clientPage = pages;
    var start = (clientPage - 1) * clientPageSize;
    var pageRows = rows.slice(start, start + clientPageSize);
    var html;
    if (!total) {
      var hasClients = Array.isArray(state.clients) && state.clients.length > 0;
      var listStatus = state.clientListStatus || "pending";
      // Issue 220: an empty table only means "no clients" after a list load actually succeeded;
      // a pending or failed load says so instead of pretending the list is empty.
      if (clientSearchTerm && hasClients) {
        html = "<tr class='empty-row'><td colspan='8'>无匹配客户端</td></tr>";
      } else if (listStatus === "error") {
        html = "<tr class='empty-row'><td colspan='8'>加载客户端失败，请重试</td></tr>";
      } else if (listStatus !== "ready") {
        html = "<tr class='empty-row'><td colspan='8'>加载中...</td></tr>";
      } else {
        html = "<tr class='empty-row'><td colspan='8'>暂无客户端</td></tr>";
      }
    } else {
      html = pageRows
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
            (usagePlaceholder === null ? fmtNum(tok) : usagePlaceholder) +
            "</td>" +
            "<td>" +
            (usagePlaceholder === null ? fmtMoney(cost) : usagePlaceholder) +
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
    var info = document.getElementById("clientsPageInfo");
    // An unknown list reports unknown totals and no page number instead of a made-up 0.
    var listKnown = (state.clientListStatus || "pending") === "ready" || total > 0 ||
      (Array.isArray(state.clients) && state.clients.length > 0);
    if (info) {
      info.textContent = !listKnown
        ? "客户端数量未知"
        : total
          ? "共 " + total + " 个客户端，第 " + (start + 1) + "~" + (start + pageRows.length) + " 条"
          : "共 0 个客户端";
    }
    var num = document.getElementById("clientsPageNum");
    if (num) num.textContent = listKnown ? clientPage + " / " + pages : "0 / 0";
    var prev = document.getElementById("clientsPrevBtn");
    if (prev) prev.disabled = !listKnown || clientPage <= 1;
    var next = document.getElementById("clientsNextBtn");
    if (next) next.disabled = !listKnown || clientPage >= pages;
    // A refresh that returns the same markup (tab switch, manual refresh, usage arriving before
    // the list changes) must not swap the DOM under the pointer and keyboard focus.
    if (clientRenderCache.html === html) return;
    clientRenderCache.html = html;
    tb.innerHTML = html;
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
              // The deleted record is known locally: drop it and its usage entry, repaint once,
              // and do not reload the list or the usage summary (Issue #208).
              ++clientLoadVersion;
              state.clients = state.clients.filter(function (client) {
                return client.name !== name;
              });
              if (state.clientUsage) delete state.clientUsage[name];
              renderClients();
            } catch (e) {
              if (isUnconfirmedOutcome(e)) await reconcileUnconfirmedDelete(name, e);
              else toast("删除失败: " + e.message, "error");
            }
          },
        );
      }
    });
  // The editor opens immediately; targets/pools and dictionaries load in the background and
  // unknown pickers show a loading state until they land.
  function renderClientPickerLoading(id, label) {
    var container = document.getElementById(id);
    if (!container) return;
    container.innerHTML =
      "<span role='status' style='color:var(--text-muted);font-size:12px;'>" +
      label +
      "加载中…</span>";
  }
  function renderClientDictOptions(c) {
    // While the dictionaries are (re)loading, cached entries are stale by definition: show the
    // loading state, never options a fresh load may already have removed. The saved value stays
    // selectable so an edit never loses it.
    var levels = state.dictsLoaded ? state.jobLevels : [];
    var groups = state.dictsLoaded ? state.projectGroups : [];
    document.getElementById("clientJobLevel").innerHTML = dictOptionsHTML(
      levels,
      c && c.job_level ? c.job_level : "",
      state.dictsLoaded ? dictEmptyLabel("job") : "加载中…",
    );
    document.getElementById("clientProjectGroup").innerHTML = dictOptionsHTML(
      groups,
      c && c.project_group ? c.project_group : "",
      state.dictsLoaded ? dictEmptyLabel("project") : "加载中…",
    );
  }
  // Re-fill the dictionaries without discarding what the user selected meanwhile. A failed load
  // keeps the saved value selectable and says why the list is incomplete, so the placeholder never
  // reads as "the server has no job levels".
  function fillClientDictOptions(c, dictsFailed) {
    var levelEl = document.getElementById("clientJobLevel");
    var groupEl = document.getElementById("clientProjectGroup");
    var level = levelEl.value || (c && c.job_level ? c.job_level : "");
    var group = groupEl.value || (c && c.project_group ? c.project_group : "");
    var levelEmpty = state.dictsLoaded
      ? dictEmptyLabel("job")
      : dictsFailed ? "职级加载失败，请关闭后重新打开重试" : "加载中…";
    var groupEmpty = state.dictsLoaded
      ? dictEmptyLabel("project")
      : dictsFailed ? "项目组加载失败，请关闭后重新打开重试" : "加载中…";
    // Cached dictionaries are stale while a fresh load is pending/failed; only the
    // successfully loaded lists may back the options. The saved value stays selectable.
    var levels = state.dictsLoaded ? state.jobLevels : [];
    var groups = state.dictsLoaded ? state.projectGroups : [];
    levelEl.innerHTML = dictOptionsHTML(levels, level, levelEmpty);
    groupEl.innerHTML = dictOptionsHTML(groups, group, groupEmpty);
  }
  // Fills the pickers with the data the background refresh landed. Each picker has its own
  // marker (Issue 220): late data never rewrites a choice the user already clicked there, while
  // the other picker still takes the roster that arrived. A failed refresh explains itself
  // instead of leaving the loading placeholder forever.
  function fillClientPickerTags(c, targetsFailed, fillTargets, fillPools) {
    var targetBox = document.getElementById("clientTargetTags");
    if (targetBox && fillTargets !== false) {
      if (targetsFailed && !(state.targets && state.targets.length)) {
        targetBox.innerHTML =
          "<span role='status' style='color:var(--text-muted);font-size:12px;'>目标加载失败，请关闭后重新打开重试。</span>";
      } else {
        renderClientTargetTags(c && c.allowed_targets ? c.allowed_targets : []);
      }
    }
    var poolBox = document.getElementById("clientPoolTags");
    if (poolBox && fillPools !== false) {
      if (targetsFailed && !targetPoolsAvailable() && !(state.pools && state.pools.length)) {
        poolBox.innerHTML =
          "<span role='status' style='color:var(--text-muted);font-size:12px;'>部署池加载失败，请关闭后重新打开重试。</span>";
      } else {
        renderClientPoolTags(c && c.allowed_pools ? c.allowed_pools : []);
      }
    }
  }
  async function loadClientEditorDataInBackground(c, clientVersion, version) {
    var targetsFailed = false;
    var dictsFailed = false;
    var failure = null;
    var targetsRefresh = loadTargets(true).catch(function (e) {
      targetsFailed = true;
      failure = failure || e;
      return null;
    });
    var dicts = state.dictsLoaded
      ? Promise.resolve()
      : loadDictOptions().catch(function (e) {
        dictsFailed = true;
        failure = failure || e;
        return null;
      });
    var results = await Promise.all([targetsRefresh, dicts]);
    if (version !== clientOpenVersion) return;
    if (clientEditorData.version === version) {
      // Targets and pools come from one refresh: without a valid pool roster the pool picker
      // cannot describe or preserve a grant, so the editor is only ready with both.
      var targetsOk = !targetsFailed && Array.isArray(results[0]) && targetPoolsAvailable();
      var dictsOk = !dictsFailed && !!state.dictsLoaded;
      clientEditorData.status = targetsOk && dictsOk ? "ready" : "error";
      if (!targetsOk && !failure) failure = new Error("目标或部署池数据不可用");
    }
    if (!document.getElementById("clientModal").classList.contains("open")) return;
    if (c) {
      var currentVersion = clientVersions[c.name] || 0;
      // A version advanced by this editor's own save is already what the draft shows; only an
      // update the editor did not observe forces a reopen.
      var ownSave = state.editingClient === c && editingClientVersion === currentVersion;
      if (currentVersion !== clientVersion && !ownSave) {
        toast("客户端已更新，请重新打开以确认最新授权。", "error");
        closeModal("clientModal");
        return;
      }
    }
    fillClientDictOptions(c, dictsFailed);
    fillClientPickerTags(c, targetsFailed, !clientTargetsTouched, !clientPoolsTouched);
    if (failure) toast("加载客户端编辑数据失败: " + failure.message + "。请关闭后重新打开重试。", "error");
  }
  function openClientModal(c) {
    if (document.body.classList.contains("readonly-mode")) {
      toast("只读用户无法执行此操作", "error");
      return Promise.resolve();
    }
    if (c && clientSaveInFlight && clientSavingName === c.name) {
      toast("该客户端正在保存，请等待完成后重新打开。", "error");
      return Promise.resolve();
    }
    var clientVersion = c ? (clientVersions[c.name] || 0) : 0;
    var version = ++clientOpenVersion;
    clientTargetsTouched = false;
    clientPoolsTouched = false;
    clientEditorData = { version: version, status: "pending" };
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
    // The modal opens at once. Known picker data is shown immediately and refreshed in the
    // background; unknown data shows a loading state and is filled when it arrives.
    if (state.targets && state.targets.length) {
      renderClientTargetTags(c && c.allowed_targets ? c.allowed_targets : []);
    } else {
      renderClientPickerLoading("clientTargetTags", "目标");
    }
    if ((state.pools && state.pools.length) || targetPoolsAvailable()) {
      renderClientPoolTags(c && c.allowed_pools ? c.allowed_pools : []);
    } else {
      renderClientPickerLoading("clientPoolTags", "部署池");
    }
    renderClientDictOptions(c);
    openModal("clientModal");
    return loadClientEditorDataInBackground(c, clientVersion, version);
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
        !clientTargetsTouched &&
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
    // Nothing rendered yet (still loading or unavailable): keep the saved grant rather than
    // submitting an empty list.
    if (!tags.length && state.editingClient &&
        Array.isArray(state.editingClient.allowed_targets)) {
      return state.editingClient.allowed_targets.slice();
    }
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
    if (!tags.length && state.editingClient &&
        Array.isArray(state.editingClient.allowed_pools)) {
      return state.editingClient.allowed_pools.slice();
    }
    return result;
  }
  function onSchedulingRefresh() {
    if (!document.getElementById("clientModal").classList.contains("open")) return;
    var pools = getSelectedClientPools();
    // Before the pool picker has any tags (still loading, or a failed roster), the editing
    // client's saved grants are the only source; use them so a refresh cannot drop them.
    var hasTags = document
      .getElementById("clientPoolTags")
      .querySelectorAll("span[data-pool]").length > 0;
    if (!hasTags && state.editingClient &&
        Array.isArray(state.editingClient.allowed_pools)) {
      pools = state.editingClient.allowed_pools.slice();
    }
    renderClientPoolTags(pools);
  }
  // While the dictionary request is still pending the selects say so; a stored value stays
  // selectable so a save during the load cannot silently clear it.
  function clientDictValue(id, fallback) {
    var value = document.getElementById(id).value.trim();
    if (value || state.dictsLoaded) return value;
    return fallback || "";
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
    .getElementById("retryClientsUsageBtn")
    .addEventListener("click", function () {
      loadClientUsage();
    });
  // Typing re-renders the whole table, so wait for a short pause instead of every keystroke; a
  // changed search always starts from the first page of the new result.
  var clientsSearchInput = document.getElementById("clientsSearch");
  clientsSearchInput.addEventListener("input", function () {
    if (clientSearchTimer !== null) clearTimeout(clientSearchTimer);
    var value = clientsSearchInput.value;
    clientSearchTimer = setTimeout(function () {
      clientSearchTimer = null;
      clientSearchTerm = String(value || "").trim().toLowerCase();
      clientPage = 1;
      renderClients();
    }, 200);
  });
  document
    .getElementById("clientsPageSize")
    .addEventListener("change", function () {
      var size = Number(document.getElementById("clientsPageSize").value);
      clientPageSize = CLIENT_PAGE_SIZES.indexOf(size) >= 0 ? size : CLIENT_PAGE_SIZE_DEFAULT;
      clientPage = 1;
      renderClients();
    });
  document
    .getElementById("clientsPrevBtn")
    .addEventListener("click", function () {
      if (clientPage <= 1) return;
      clientPage--;
      renderClients();
    });
  document
    .getElementById("clientsNextBtn")
    .addEventListener("click", function () {
      var pages = Math.max(1, Math.ceil(clientMatchedRows().length / clientPageSize));
      if (clientPage >= pages) return;
      clientPage++;
      renderClients();
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
      // Issue 220: an unloaded picker would be submitted as an empty ACL, which the server
      // reads as "allow every target". A create therefore waits for the editor's data; edits
      // keep using the saved record as their fallback.
      if (!state.editingClient && clientEditorData.status !== "ready") {
        toast("目标/部署池/职级数据加载中，请稍后再保存", "error");
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
        job_level: clientDictValue(
          "clientJobLevel",
          state.editingClient && state.editingClient.job_level,
        ),
        project_group: clientDictValue(
          "clientProjectGroup",
          state.editingClient && state.editingClient.project_group,
        ),
      };
      var saveVersion = clientOpenVersion;
      var saveRevision = clientDraftRevision;
      var wasEditing = !!state.editingClient;
      clientSaveInFlight = true;
      clientSavingName = name;
      setClientSaveBusy(true);
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
        // The optimistic record keeps the save visible if the list GET below fails; the single
        // repaint happens after that GET, and no usage summary is requested (Issue #208).
        adoptSavedClient(payload, saveVersion, saveRevision, wasEditing);
        await refreshClientsLight();
      } catch (e) {
        if (isUnconfirmedOutcome(e)) {
          await reconcileUnconfirmedSave(payload, saveVersion, saveRevision, wasEditing, e);
        } else {
          toast("保存失败: " + e.message, "error");
        }
      } finally {
        clientSaveInFlight = false;
        clientSavingName = "";
        setClientSaveBusy(false);
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
