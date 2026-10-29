package admin

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse("not authenticated"))
		return
	}

	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	if strings.TrimSpace(req.OldPassword) == "" || strings.TrimSpace(req.NewPassword) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("旧密码和新密码均不能为空"))
		return
	}
	if len(req.NewPassword) < 6 {
		writeJSON(w, http.StatusBadRequest, errorResponse("新密码至少需要 6 个字符"))
		return
	}

	// Verify old password.
	if _, err := AuthenticateUser(h.userStore, session.Username, req.OldPassword); err != nil {
		writeJSON(w, http.StatusForbidden, errorResponse("原密码验证失败"))
		return
	}

	// Generate new hash with random salt.
	newHash, err := HashPasswordWithRandomSalt(req.NewPassword)
	if err != nil {
		h.writeInternalError(w, "failed to hash password", err)
		return
	}

	// Update the user's password hash via nosql store.
	user, err := h.userStore.Get(session.Username)
	if err != nil {
		h.writeInternalError(w, "failed to get user", err)
		return
	}
	user.PasswordHash = newHash
	if err := h.userStore.Update(session.Username, user); err != nil {
		h.writeInternalError(w, "failed to update password", err)
		return
	}
	h.sessionManager.revokeByUsernameExcept(session.Username, session.ID)

	h.recordAudit(r, "change_password", session.Username, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "密码已修改"})
}

// ========== User management (admin-only) ==========

func (h *Handler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil || session.Role != RoleAdmin {
		writeJSON(w, http.StatusForbidden, errorResponse("admin role required"))
		return
	}
	users, err := h.userStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list users", err)
		return
	}
	// Strip password hashes from response.
	for i := range users {
		users[i].PasswordHash = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "count": len(users)})
}

func (h *Handler) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil || session.Role != RoleAdmin {
		writeJSON(w, http.StatusForbidden, errorResponse("admin role required"))
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("username required"))
		return
	}
	if req.Password == "" || len(req.Password) < 6 {
		writeJSON(w, http.StatusBadRequest, errorResponse("password must be at least 6 characters"))
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = RoleAdmin
	}
	if role != RoleAdmin && role != RoleReadonly {
		writeJSON(w, http.StatusBadRequest, errorResponse("role must be 'admin' or 'readonly'"))
		return
	}
	hash, err := HashPasswordWithRandomSalt(req.Password)
	if err != nil {
		h.writeInternalError(w, "failed to hash password", err)
		return
	}
	user := config.AdminUser{
		Username:     req.Username,
		PasswordHash: hash,
		Role:         role,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := h.userStore.Create(user); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	h.recordAudit(r, "user_create", req.Username, "success", "role="+role)
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (h *Handler) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil || session.Role != RoleAdmin {
		writeJSON(w, http.StatusForbidden, errorResponse("admin role required"))
		return
	}
	username := strings.TrimSpace(pathParam(r, "username"))
	if username == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("username required"))
		return
	}
	// 默认 admin 引导账号禁止经用户管理修改（角色/禁用/重置密码）——
	// 删除或降权会破坏 seed 语义；自助改密走 /api/change-password。
	if strings.EqualFold(username, DefaultAdminUsername) {
		writeJSON(w, http.StatusBadRequest, errorResponse("the default admin account cannot be modified via user management"))
		return
	}
	var req struct {
		Password string `json:"password,omitempty"`
		Role     string `json:"role,omitempty"`
		Disabled *bool  `json:"disabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	existing, err := h.userStore.Get(username)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}
	username = existing.Username
	previousRole := existing.Role
	if req.Password != "" {
		if len(req.Password) < 6 {
			writeJSON(w, http.StatusBadRequest, errorResponse("password must be at least 6 characters"))
			return
		}
		hash, hashErr := HashPasswordWithRandomSalt(req.Password)
		if hashErr != nil {
			h.writeInternalError(w, "failed to hash password", hashErr)
			return
		}
		existing.PasswordHash = hash
	}
	if strings.TrimSpace(req.Role) != "" {
		role := strings.TrimSpace(req.Role)
		if role != RoleAdmin && role != RoleReadonly {
			writeJSON(w, http.StatusBadRequest, errorResponse("role must be 'admin' or 'readonly'"))
			return
		}
		// Prevent admin from downgrading their own role to readonly —
		// this would lock the admin out of all management functions,
		// and there is no recovery path without another admin account.
		if strings.EqualFold(session.Username, username) && role != session.Role {
			writeJSON(w, http.StatusBadRequest, errorResponse("cannot change your own role"))
			return
		}
		existing.Role = role
	}
	if req.Disabled != nil && *req.Disabled && strings.EqualFold(session.Username, username) {
		writeJSON(w, http.StatusBadRequest, errorResponse("cannot disable your own account"))
		return
	}
	if req.Disabled != nil && *req.Disabled && strings.EqualFold(session.Username, username) {
		writeJSON(w, http.StatusBadRequest, errorResponse("cannot disable your own account"))
		return
	}
	if req.Disabled != nil {
		existing.Disabled = *req.Disabled
	}
	existing.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := h.userStore.Update(username, existing); err != nil {
		h.writeInternalError(w, "failed to update user", err)
		return
	}
	// P14：改密/禁用/降权后吊销该用户的存量会话，否则旧登录在滑动过期下
	// 可无限期保留权限。自改自己（非禁用）保留当前会话体验。
	if existing.Role != previousRole || req.Password != "" || (req.Disabled != nil && *req.Disabled) {
		keepID := ""
		if strings.EqualFold(session.Username, username) && req.Password != "" && (req.Disabled == nil || !*req.Disabled) {
			keepID = session.ID
		}
		if n := h.sessionManager.revokeByUsernameExcept(username, keepID); n > 0 {
			h.logger.Info("revoked sessions after user update", "username", username, "count", n)
		}
	}
	h.recordAudit(r, "user_update", username, "success", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil || session.Role != RoleAdmin {
		writeJSON(w, http.StatusForbidden, errorResponse("admin role required"))
		return
	}
	username := strings.TrimSpace(pathParam(r, "username"))
	if username == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("username required"))
		return
	}
	// 默认 admin 引导账号禁止删除（删除后重启会触发 seed 重置，AGENTS.md 红线）。
	if strings.EqualFold(username, DefaultAdminUsername) {
		writeJSON(w, http.StatusBadRequest, errorResponse("the default admin account cannot be deleted"))
		return
	}
	// Prevent self-deletion.
	if strings.EqualFold(session.Username, username) {
		writeJSON(w, http.StatusBadRequest, errorResponse("cannot delete your own account"))
		return
	}
	existing, err := h.userStore.Get(username)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}
	username = existing.Username
	if err := h.userStore.Delete(username); err != nil {
		h.writeInternalError(w, "failed to delete user", err)
		return
	}
	// P14：删除用户后吊销其全部存量会话。
	if n := h.sessionManager.RevokeByUsername(username); n > 0 {
		h.logger.Info("revoked sessions after user delete", "username", username, "count", n)
	}
	h.recordAudit(r, "user_delete", username, "success", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
