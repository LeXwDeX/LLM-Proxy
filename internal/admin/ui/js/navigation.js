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
    targets: "目标管理",
    clients: "客户端管理",
    "model-costs": "模型计费",
    usage: "消费统计",
    quota: "额度管理",
    audit: "审计日志",
    users: "用户管理",
    "model-catalog": "模型目录",
  };
  function switchPage(page) {
    state.currentPage = page;
    document.querySelectorAll(".sidebar-nav-item").forEach(function (n) {
      n.classList.toggle("active", n.dataset.page === page);
    });
    document.querySelectorAll(".page").forEach(function (p) {
      p.classList.toggle("active", p.id === "page-" + page);
    });
    document.getElementById("topbarTitle").textContent =
      pageTitles[page] || page;
    /* Close sidebar on mobile */
    document.getElementById("sidebar").classList.remove("open");
    document.getElementById("sidebarOverlay").classList.remove("open");
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
    item.addEventListener("click", function () {
      switchPage(item.dataset.page);
    });
  });

  /* Mobile menu */
  document.getElementById("menuToggle").addEventListener("click", function () {
    document.getElementById("sidebar").classList.toggle("open");
    document.getElementById("sidebarOverlay").classList.toggle("open");
  });
  document
    .getElementById("sidebarOverlay")
    .addEventListener("click", function () {
      document.getElementById("sidebar").classList.remove("open");
      document.getElementById("sidebarOverlay").classList.remove("open");
    });

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
