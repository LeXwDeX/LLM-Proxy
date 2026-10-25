"use strict";
// Catalog owns model catalog loading/cache, filtering and pagination.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("catalog", function (app) {
  var state = app.state;
  var catalogSearchTimer = null;
  var esc = app.bind("core", "esc");
  var api = app.bind("core", "api");
  var epDisplayName = app.bind("schema", "epDisplayName");
  var epBadgeStyle = app.bind("schema", "epBadgeStyle");
  /* ===== Shared catalog helpers ===== */
  async function loadCatalog(epType, opts) {
    // opts.force=true 跳过缓存（用于打开 modal 等需要新鲜数据的场景，
    // 避免后台 catalog 更新后前端仍显示旧列表）
    var force = opts && opts.force;
    if (!force && state.catalogCache[epType]) return state.catalogCache[epType];
    try {
      var data = await api("/admin/data/catalog/" + encodeURIComponent(epType));
      var models = Array.isArray(data.models) ? data.models : [];
      state.catalogCache[epType] = models;
      return models;
    } catch (e) {
      return [];
    }
  }
  function catalogFilterModels(models, keyword) {
    if (!keyword) return models;
    var kw = keyword.toLowerCase().trim();
    if (!kw) return models;
    return models.filter(function (m) {
      if ((m.model || "").toLowerCase().indexOf(kw) >= 0) return true;
      if ((m.display_name || "").toLowerCase().indexOf(kw) >= 0) return true;
      var aliases = m.aliases || [];
      for (var i = 0; i < aliases.length; i++) {
        if (aliases[i].toLowerCase().indexOf(kw) >= 0) return true;
      }
      return false;
    });
  }

  /* ===== 模型目录 ===== */
  var catalogAllModels = []; // 全量数据（已排序）
  var catalogFiltered = []; // 过滤后
  var catalogPage = 1;
  var catalogPageSize = 30;

  async function loadModelCatalog() {
    // 优先用缓存；无缓存则全量拉取（使用 /admin/data/catalog 不带 endpoint_type 参数）
    if (!state.catalogAllCache) {
      try {
        var data = await api("/admin/data/catalog");
        state.catalogAllCache = Array.isArray(data.models) ? data.models : [];
      } catch (e) {
        document.getElementById("catalogTableBody").innerHTML =
          "<tr class='empty-row'><td colspan='9'>加载失败: " +
          esc(e.message) +
          "</td></tr>";
        return;
      }
    }
    catalogAllModels = state.catalogAllCache;
    catalogPage = 1;
    applyCatalogFilter();
  }

  function applyCatalogFilter() {
    var epFilter = document.getElementById("catalogEpFilter").value;
    var kw = (document.getElementById("catalogSearch").value || "")
      .toLowerCase()
      .trim();
    catalogFiltered = catalogAllModels.filter(function (m) {
      if (epFilter && m.endpoint_type !== epFilter) return false;
      if (kw) {
        var haystack = (
          (m.model || "") +
          " " +
          (m.display_name || "") +
          " " +
          (m.model_family || "")
        ).toLowerCase();
        if (haystack.indexOf(kw) === -1) return false;
      }
      return true;
    });
    catalogPage = 1;
    renderCatalogPage();
  }

  function renderCatalogPage() {
    var total = catalogFiltered.length;
    var pages = Math.max(1, Math.ceil(total / catalogPageSize));
    if (catalogPage < 1) catalogPage = 1;
    if (catalogPage > pages) catalogPage = pages;

    var start = (catalogPage - 1) * catalogPageSize;
    var slice = catalogFiltered.slice(start, start + catalogPageSize);

    // 渠道徽章：颜色与标签均从 endpoint_type 元数据派生（单一信息源）。
    function epBadge(ep) {
      return (
        "<span class='ep-badge' style='" +
        epBadgeStyle(ep) +
        "'>" +
        esc(epDisplayName(ep) || ep) +
        "</span>"
      );
    }

    function fmtCtx(n) {
      if (!n) return "<span style='color:var(--text-muted)'>—</span>";
      if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
      if (n >= 1000) return Math.round(n / 1000) + "K";
      return String(n);
    }

    function capBadges(caps) {
      if (!caps || !caps.length)
        return "<span style='color:var(--text-muted)'>—</span>";
      function icon(paths) {
        return (
          "<svg width='13' height='13' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='1.8' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'>" +
          paths +
          "</svg>"
        );
      }
      var capIcons = {
        vision: icon(
          "<path d='M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12z'></path><circle cx='12' cy='12' r='3'></circle>",
        ),
        function_calling: icon(
          "<path d='M14 4a4 4 0 0 0-4 6.3L4 16.3V20h3.7l6-6A4 4 0 1 0 14 4z'></path>",
        ),
        structured_output: icon(
          "<path d='M6 3h9l3 3v15H6z'></path><path d='M14 3v4h4M9 11h6M9 15h6'></path>",
        ),
        reasoning: icon(
          "<path d='M9 18h6M10 21h4M12 3a6 6 0 0 0-3.6 10.8c.7.6 1.1 1.3 1.3 2.2h4.6c.2-.9.6-1.6 1.3-2.2A6 6 0 0 0 12 3z'></path>",
        ),
        image_generation: icon(
          "<rect x='3' y='4' width='18' height='16' rx='2'></rect><circle cx='8.5' cy='9' r='1.5'></circle><path d='m21 15-5-5L5 20'></path>",
        ),
        audio_input: icon(
          "<rect x='9' y='3' width='6' height='11' rx='3'></rect><path d='M5 11a7 7 0 0 0 14 0M12 18v3M8 21h8'></path>",
        ),
        audio_output: icon(
          "<path d='M11 5 6 9H3v6h3l5 4z'></path><path d='M15.5 8.5a5 5 0 0 1 0 7M18 6a8 8 0 0 1 0 12'></path>",
        ),
      };
      return caps
        .map(function (c) {
          var icon = capIcons[c] || "•";
          var label = c.replace(/_/g, " ");
          return (
            "<span title='" +
            label +
            "' style='display:inline-block;padding:1px 6px;border-radius:3px;font-size:11px;" +
            "background:var(--accent-soft);color:var(--accent);margin:1px;'>" +
            icon +
            " " +
            label +
            "</span>"
          );
        })
        .join("");
    }

    document.getElementById("catalogTableBody").innerHTML = slice.length
      ? slice
          .map(function (m) {
            var cost = m.default_cost || {};
            return (
              "<tr>" +
              "<td>" +
              epBadge(m.endpoint_type) +
              "</td>" +
              "<td style='font-family:ui-monospace, SF Mono, Menlo, Consolas, monospace;font-size:12px;'>" +
              esc(m.model) +
              "</td>" +
              "<td style='color:var(--text-secondary)'>" +
              esc(m.display_name || "") +
              "</td>" +
              "<td style='font-size:12px;'>" +
              esc(m.model_family || "") +
              "</td>" +
              "<td style='text-align:right;font-variant-numeric:tabular-nums;'>" +
              fmtCtx(m.context_window) +
              "</td>" +
              "<td style='text-align:right;font-variant-numeric:tabular-nums;'>" +
              fmtCtx(m.max_output_tokens) +
              "</td>" +
              "<td>" +
              capBadges(m.capabilities) +
              "</td>" +
              "<td style='text-align:right;color:var(--text-secondary);font-size:12px;'>" +
              (cost.input_per_1m_tokens != null
                ? "$" + cost.input_per_1m_tokens
                : "<span style='color:var(--text-muted)'>—</span>") +
              "</td>" +
              "<td style='text-align:right;color:var(--text-secondary);font-size:12px;'>" +
              (cost.output_per_1m_tokens != null
                ? "$" + cost.output_per_1m_tokens
                : "<span style='color:var(--text-muted)'>—</span>") +
              "</td>" +
              "</tr>"
            );
          })
          .join("")
      : "<tr class='empty-row'><td colspan='9'>无匹配模型</td></tr>";

    document.getElementById("catalogPageInfo").textContent =
      "共 " +
      total +
      " 个模型，第 " +
      (start + 1) +
      "~" +
      Math.min(start + catalogPageSize, total) +
      " 条";
    document.getElementById("catalogPageNum").textContent =
      catalogPage + " / " + pages;
    document.getElementById("catalogPrevBtn").disabled = catalogPage <= 1;
    document.getElementById("catalogNextBtn").disabled = catalogPage >= pages;
  }

  document
    .getElementById("catalogEpFilter")
    .addEventListener("change", applyCatalogFilter);
  document
    .getElementById("catalogSearch")
    .addEventListener("input", function () {
      clearTimeout(catalogSearchTimer);
      catalogSearchTimer = setTimeout(applyCatalogFilter, 250);
    });
  document
    .getElementById("refreshCatalogBtn")
    .addEventListener("click", function () {
      state.catalogAllCache = null;
      loadModelCatalog();
    });
  document
    .getElementById("catalogPrevBtn")
    .addEventListener("click", function () {
      if (catalogPage > 1) {
        catalogPage--;
        renderCatalogPage();
      }
    });
  document
    .getElementById("catalogNextBtn")
    .addEventListener("click", function () {
      var pages = Math.ceil(catalogFiltered.length / catalogPageSize);
      if (catalogPage < pages) {
        catalogPage++;
        renderCatalogPage();
      }
    });

  return {
    loadCatalog: loadCatalog,
    catalogFilterModels: catalogFilterModels,
    loadModelCatalog: loadModelCatalog,
    applyCatalogFilter: applyCatalogFilter,
    renderCatalogPage: renderCatalogPage,
  };
});
