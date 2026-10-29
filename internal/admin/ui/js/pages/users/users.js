"use strict";
// Users owns session role presentation, password changes and admin account CRUD.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("users", function (app) {
  var state = app.state;
  var esc = app.bind("core", "esc");
  var fmtTime = app.bind("core", "fmtTime");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var openModal = app.bind("core", "openModal");
  var closeModal = app.bind("core", "closeModal");
  var showConfirm = app.bind("core", "showConfirm");
  var setSchemaSelectValue = app.bind("schema", "setSchemaSelectValue");
  /* ===== User info ===== */
  function isReadonlyMode() {
    return document.body.classList.contains("readonly-mode");
  }
  function applyReadonlyMode(root) {
    if (!isReadonlyMode()) return;
    (root || document)
      .querySelectorAll(".readonly-disable")
      .forEach(function (control) {
        control.disabled = true;
      });
  }
  async function loadUserInfo() {
    var data = await api("/admin/api/me");
    if (!data || typeof data.username !== "string" || !data.username.trim() ||
        (data.authenticated !== undefined && data.authenticated !== true) ||
        (data.role !== "admin" && data.role !== "readonly")) {
      throw new Error("当前会话角色不可用，请重试");
    }
    state.currentUser = data.username;
    var badgeName = document.getElementById("userBadgeName");
    if (badgeName) badgeName.textContent = data.username;
    if (data.role === "readonly") {
      var badge = document.getElementById("readonlyBadge");
      if (badge) badge.style.display = "";
    }
    // User management remains admin-only; the server enforces this independently.
    if (data.role === "admin") {
      var usersNav = document.getElementById("nav-users");
      if (usersNav) usersNav.style.display = "";
      var systemSection = document.getElementById("nav-section-system");
      if (systemSection) systemSection.style.display = "";
    }
    return data.role;
  }

  /* ===== Change Password ===== */
  document
    .getElementById("changePasswordBtn")
    .addEventListener("click", function () {
      document.getElementById("cpOldPassword").value = "";
      document.getElementById("cpNewPassword").value = "";
      document.getElementById("cpConfirmPassword").value = "";
      openModal("changePasswordModal");
    });
  document
    .getElementById("savePasswordBtn")
    .addEventListener("click", async function () {
      var oldPw = document.getElementById("cpOldPassword").value;
      var newPw = document.getElementById("cpNewPassword").value;
      var confirmPw = document.getElementById("cpConfirmPassword").value;
      if (!oldPw) {
        toast("请输入当前密码", "error");
        return;
      }
      if (!newPw) {
        toast("请输入新密码", "error");
        return;
      }
      if (newPw.length < 6) {
        toast("新密码至少需要 6 个字符", "error");
        return;
      }
      if (newPw !== confirmPw) {
        toast("两次输入的新密码不一致", "error");
        return;
      }
      try {
        await api("/admin/api/change-password", {
          method: "POST",
          body: { old_password: oldPw, new_password: newPw },
        });
        toast("密码修改成功", "success");
        closeModal("changePasswordModal");
      } catch (e) {
        toast("修改失败: " + e.message, "error");
      }
    });

  /* ===== 用户管理（admin-only） ===== */
  // 与后端 admin.DefaultAdminUsername 一致：默认引导账号禁止管理操作
  // （后端 PUT/DELETE 返回 400，UI 行内不渲染操作按钮，双防线）。
  var DEFAULT_ADMIN_USERNAME = "admin";
  var usersCache = [];
  var editingUser = null;
  async function loadUsers() {
    try {
      var data = await api("/admin/data/users");
      var users = data && Array.isArray(data.users) ? data.users : [];
      usersCache = users;
      var tbody = document.getElementById("usersTableBody");
      if (!users.length) {
        tbody.innerHTML =
          "<tr class='empty-row'><td colspan='5'>暂无用户</td></tr>";
        return;
      }
      tbody.innerHTML = users
        .map(function (u) {
          var isAdmin = u.role === "admin";
          var roleBadge =
            "<span class='badge' style='background:" +
            (isAdmin
              ? "var(--primary-light);color:var(--primary);"
              : "var(--bg);color:var(--text-secondary);") +
            "padding:2px 8px;border-radius:4px;font-size:12px;'>" +
            (isAdmin ? "管理" : "只读") +
            "</span>";
          var statusBadge = u.disabled
            ? "<span style='color:var(--danger);font-size:12px;'>已禁用</span>"
            : "<span style='color:var(--success);font-size:12px;'>正常</span>";
          var ops;
          if (u.username === DEFAULT_ADMIN_USERNAME) {
            ops =
              "<span class='badge' style='background:var(--bg);color:var(--text-secondary);padding:2px 8px;border-radius:4px;font-size:12px;'>默认账号</span>";
          } else {
            ops =
              "<div class='btn-group' style='flex-wrap:nowrap;'>" +
              "<button class='btn btn-sm' data-edit-user='" +
              esc(u.username) +
              "'>编辑</button>" +
              // 禁止自禁用：自己的行不提供启用/禁用开关（后端亦拒绝）
              (u.username === state.currentUser
                ? ""
                : "<button class='btn btn-sm' data-toggle-user='" +
                  esc(u.username) +
                  "'>" +
                  (u.disabled ? "启用" : "禁用") +
                  "</button>") +
              "<button class='btn btn-danger btn-sm' data-del-user='" +
              esc(u.username) +
              "'>删除</button>" +
              "</div>";
          }
          return (
            "<tr>" +
            "<td>" +
            esc(u.username) +
            "</td>" +
            "<td>" +
            roleBadge +
            "</td>" +
            "<td>" +
            statusBadge +
            "</td>" +
            "<td>" +
            esc(fmtTime(u.updated_at)) +
            "</td>" +
            "<td>" +
            ops +
            "</td>" +
            "</tr>"
          );
        })
        .join("");
    } catch (e) {
      document.getElementById("usersTableBody").innerHTML =
        "<tr class='empty-row'><td colspan='5'>加载失败: " +
        esc(e.message) +
        "</td></tr>";
    }
  }
  function findCachedUser(username) {
    for (var i = 0; i < usersCache.length; i++) {
      if (usersCache[i].username === username) return usersCache[i];
    }
    return null;
  }
  function openCreateUser() {
    editingUser = null;
    document.getElementById("userModalTitle").textContent = "新增用户";
    document.getElementById("userUsername").value = "";
    document.getElementById("userUsername").readOnly = false;
    document.getElementById("userPassword").value = "";
    document.getElementById("userConfirmPassword").value = "";
    setSchemaSelectValue("userRole", state.schema.defaults.role);
    document.getElementById("userRole").disabled = false;
    document.getElementById("userPasswordHint").textContent = "(至少6位)";
    openModal("userModal");
  }
  function openEditUser(username) {
    var u = findCachedUser(username);
    if (!u) {
      toast("用户不存在", "error");
      return;
    }
    editingUser = u;
    document.getElementById("userModalTitle").textContent = "编辑用户";
    document.getElementById("userUsername").value = u.username;
    document.getElementById("userUsername").readOnly = true;
    document.getElementById("userPassword").value = "";
    document.getElementById("userConfirmPassword").value = "";
    setSchemaSelectValue("userRole", u.role || state.schema.defaults.role);
    // 防自降级：编辑自己时锁定角色（后端亦有 cannot change your own role 防护）
    document.getElementById("userRole").disabled =
      u.username === state.currentUser;
    document.getElementById("userPasswordHint").textContent =
      "(留空则不修改；填写至少6位)";
    openModal("userModal");
  }
  async function toggleUserDisabled(username) {
    var u = findCachedUser(username);
    if (!u) {
      toast("用户不存在", "error");
      return;
    }
    var action = u.disabled ? "启用" : "禁用";
    if (!u.disabled) {
      showConfirm(
        "确认禁用用户「" + username + "」？其全部会话将被立即吊销。",
        function () {
          applyUserDisabled(u, username, action);
        },
      );
      return;
    }
    return applyUserDisabled(u, username, action);
  }
  async function applyUserDisabled(u, username, action) {
    try {
      await api("/admin/data/users/" + encodeURIComponent(username), {
        method: "PUT",
        body: { disabled: !u.disabled },
      });
      toast(
        "已" +
          action +
          "用户「" +
          username +
          "」" +
          (u.disabled ? "" : "（其会话将被吊销）"),
        "success",
      );
      loadUsers();
    } catch (e) {
      toast(action + "失败: " + e.message, "error");
    }
  }
  function deleteUser(username) {
    showConfirm(
      "确认删除用户「" +
        username +
        "」？该用户的全部会话将被吊销，此操作不可恢复。",
      async function () {
        try {
          await api("/admin/data/users/" + encodeURIComponent(username), {
            method: "DELETE",
          });
          toast("用户已删除", "success");
          loadUsers();
        } catch (e) {
          toast("删除失败: " + e.message, "error");
        }
      },
    );
  }
  document
    .getElementById("usersTableBody")
    .addEventListener("click", function (e) {
      var editBtn = e.target.closest("[data-edit-user]");
      if (editBtn) {
        openEditUser(editBtn.getAttribute("data-edit-user"));
        return;
      }
      var toggleBtn = e.target.closest("[data-toggle-user]");
      if (toggleBtn) {
        toggleUserDisabled(toggleBtn.getAttribute("data-toggle-user"));
        return;
      }
      var delBtn = e.target.closest("[data-del-user]");
      if (delBtn) {
        deleteUser(delBtn.getAttribute("data-del-user"));
        return;
      }
    });
  document
    .getElementById("refreshUsersBtn")
    .addEventListener("click", function () {
      loadUsers();
    });
  document
    .getElementById("newUserBtn")
    .addEventListener("click", openCreateUser);
  document
    .getElementById("saveUserBtn")
    .addEventListener("click", async function () {
      var username = document.getElementById("userUsername").value.trim();
      var password = document.getElementById("userPassword").value;
      var confirmPw = document.getElementById("userConfirmPassword").value;
      var role = document.getElementById("userRole").value;
      var isEdit = !!editingUser;
      if (!username) {
        toast("请输入用户名", "error");
        return;
      }
      if (!isEdit && password.length < 6) {
        toast("密码至少需要 6 个字符", "error");
        return;
      }
      if (isEdit && password.length > 0 && password.length < 6) {
        toast("密码至少需要 6 个字符", "error");
        return;
      }
      if (password !== confirmPw) {
        toast("两次输入的密码不一致", "error");
        return;
      }
      var btn = this;
      btn.disabled = true;
      try {
        if (isEdit) {
          await api(
            "/admin/data/users/" + encodeURIComponent(editingUser.username),
            { method: "PUT", body: { password: password, role: role } },
          );
          toast("用户已更新", "success");
        } else {
          await api("/admin/data/users", {
            method: "POST",
            body: { username: username, password: password, role: role },
          });
          toast("用户创建成功", "success");
        }
        closeModal("userModal");
        loadUsers();
      } catch (e) {
        toast("保存失败: " + e.message, "error");
      } finally {
        btn.disabled = false;
      }
    });

  window.isReadonlyMode = isReadonlyMode;
  window.applyReadonlyMode = applyReadonlyMode;
  return {
    isReadonlyMode: isReadonlyMode,
    applyReadonlyMode: applyReadonlyMode,
    loadUserInfo: loadUserInfo,
    loadUsers: loadUsers,
    findCachedUser: findCachedUser,
    openCreateUser: openCreateUser,
    openEditUser: openEditUser,
    toggleUserDisabled: toggleUserDisabled,
    deleteUser: deleteUser,
  };
});
