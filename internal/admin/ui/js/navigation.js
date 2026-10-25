"use strict";
// Navigation owns page selection, mobile sidebar and page data dispatch.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("navigation", function (app) {
  var state = app.state;
  var loadOverview = app.bind("overview", "loadOverview");
  var startOverviewAutoRefresh = app.bind(
    "overview",
    "startOverviewAutoRefresh",
  );
  var stopOverviewAutoRefresh = app.bind("overview", "stopOverviewAutoRefresh");
  var loadTargets = app.bind("targets", "loadTargets");
  var loadClients = app.bind("clients", "loadClients");
  var loadPricingTemplates = app.bind("pricing", "loadPricingTemplates");
  var loadUsage = app.bind("usage", "loadUsage");
  var loadAudit = app.bind("audit_trace", "loadAudit");
  var loadTrace = app.bind("audit_trace", "loadTrace");
  var loadUsers = app.bind("users", "loadUsers");
  var loadModelCatalog = app.bind("catalog", "loadModelCatalog");
  var loadQuotaManagement = app.bind("quota", "loadQuotaManagement");
  /* ===== Navigation ===== */
  state.currentPage = "overview";
  var pageTitles = {
    overview: "总览",
    targets: "目标与部署池",
    clients: "客户端",
    "model-costs": "模型计费",
    usage: "消费统计",
    quota: "额度管理",
    audit: "审计日志",
    trace: "请求记录",
    users: "后台用户",
    "model-catalog": "模型目录",
  };
  var mobileNavQuery =
    typeof window.matchMedia === "function"
      ? window.matchMedia("(max-width: 768px)")
      : null;
  function isMobileNav() {
    if (mobileNavQuery) return mobileNavQuery.matches;
    return window.innerWidth <= 768;
  }
  function setSidebarOpen(open) {
    var sidebar = document.getElementById("sidebar");
    sidebar.classList.toggle("open", open);
    document.getElementById("sidebarOverlay").classList.toggle("open", open);
    var toggle = document.getElementById("menuToggle");
    if (toggle) {
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      toggle.setAttribute("aria-label", open ? "关闭主导航" : "打开主导航");
    }
    /* A closed mobile drawer stays off-canvas; strip it from the tab order too. */
    var isolated = isMobileNav() && !open;
    var hadFocus =
      isolated &&
      typeof sidebar.contains === "function" &&
      sidebar.contains(document.activeElement);
    sidebar.inert = isolated;
    sidebar.setAttribute("aria-hidden", isolated ? "true" : "false");
    if (hadFocus && toggle) toggle.focus();
    if (open && isMobileNav()) {
      var firstNav = sidebar.querySelector(".sidebar-nav-item");
      if (firstNav) firstNav.focus();
    }
  }
  function switchPage(page) {
    state.currentPage = page;
    document.querySelectorAll(".sidebar-nav-item").forEach(function (n) {
      var active = n.dataset.page === page;
      n.classList.toggle("active", active);
      n.setAttribute("aria-current", active ? "page" : "false");
    });
    document.querySelectorAll(".page").forEach(function (p) {
      p.classList.toggle("active", p.id === "page-" + page);
    });
    document.getElementById("topbarTitle").textContent =
      pageTitles[page] || page;
    /* Close sidebar on mobile */
    setSidebarOpen(false);
    /* Manage overview auto-refresh */
    if (page === "overview") {
      startOverviewAutoRefresh();
    } else {
      stopOverviewAutoRefresh();
    }
    /* Load data */
    loadPageData(page);
  }
  document.querySelectorAll(".sidebar-nav-item").forEach(function (item) {
    function activate() {
      switchPage(item.dataset.page);
    }
    item.addEventListener("click", activate);
    item.addEventListener("keydown", function (event) {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        activate();
      }
    });
  });

  /* Mobile menu */
  document.getElementById("menuToggle").addEventListener("click", function () {
    setSidebarOpen(
      !document.getElementById("sidebar").classList.contains("open"),
    );
  });
  document
    .getElementById("sidebarOverlay")
    .addEventListener("click", function () {
      setSidebarOpen(false);
    });
  document.addEventListener("keydown", function (event) {
    if (
      event.key === "Escape" &&
      isMobileNav() &&
      document.getElementById("sidebar").classList.contains("open")
    ) {
      setSidebarOpen(false);
    }
  });
  if (mobileNavQuery && typeof mobileNavQuery.addEventListener === "function") {
    mobileNavQuery.addEventListener("change", function () {
      setSidebarOpen(document.getElementById("sidebar").classList.contains("open"));
    });
  }
  setSidebarOpen(false);

  /* ===== Page Data Loader ===== */
  function loadPageData(page) {
    switch (page) {
      case "overview":
        loadOverview();
        break;
      case "targets":
        loadTargets();
        break;
      case "clients":
        loadClients();
        break;
      case "model-costs":
        loadPricingTemplates();
        break;
      case "usage":
        loadUsage();
        break;
      case "quota":
        loadQuotaManagement();
        break;
      case "audit":
        loadAudit();
        break;
      case "trace":
        loadTrace();
        break;
      case "users":
        loadUsers();
        break;
      case "model-catalog":
        loadModelCatalog();
        break;
    }
  }

  return {
    switchPage: switchPage,
    loadPageData: loadPageData,
  };
});
