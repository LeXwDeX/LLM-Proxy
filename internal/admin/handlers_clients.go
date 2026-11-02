package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func (h *Handler) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}

	// Plaintext access keys are admin-only; other roles (and sessionless
	// requests) get the same suffix-only mask used by audit logs.
	if !isAdminSession(r) {
		for i := range clients {
			clients[i].AccessKey = auth.MaskClientKey(clients[i].AccessKey)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": clients,
		"count":   len(clients),
	})
}

// 客户端写入与目标/部署池写入共用 configMu：commitConfigChange 在锁内读取 clients 并应用 auth，
// 客户端写入若不持锁，会被其落库后的 auth 应用以旧 clients 列表覆盖。锁内只做 clientStore 写入与
// reloadAuthFromClientStore；配额评估（可能较慢）在释放 configMu 之后执行，不阻塞目标保存。

// evaluateClientQuota 触发该客户端的配额评估（配置可能已变化）。调用方不得持有 configMu。
func (h *Handler) evaluateClientQuota(name string) {
	if h.clientQuotaEvaluateHook != nil {
		h.clientQuotaEvaluateHook(name)
	}
	if h.quotaManager != nil {
		h.quotaManager.NoteConfigChange()
		h.quotaManager.Evaluate(name)
	}
}

func (h *Handler) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	req, ok := h.createClientLocked(w, r)
	if !ok {
		return
	}
	h.evaluateClientQuota(req.Name)
	h.recordAudit(r, "client_create", req.Name, "success", auth.MaskClientKey(req.AccessKey))

	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

// createClientLocked 在 configMu 内完成校验、落库与 auth 重载；失败时已写好响应并返回 false。
func (h *Handler) createClientLocked(w http.ResponseWriter, r *http.Request) (config.Client, bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return req, false
	}

	if err := h.validateClientPoolQuotas(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return req, false
	}

	if err := h.clientStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return req, false
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return req, false
	}
	return req, true
}

func (h *Handler) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	name, req, ok := h.updateClientLocked(w, r)
	if !ok {
		return
	}
	h.evaluateClientQuota(name)
	h.recordAudit(r, "client_update", name, "success", auth.MaskClientKey(req.AccessKey))

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) updateClientLocked(w http.ResponseWriter, r *http.Request) (string, config.Client, bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	var req config.Client
	name := pathParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return name, req, false
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return name, req, false
	}

	if err := h.validateClientPoolQuotas(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return name, req, false
	}

	if err := h.clientStore.Update(name, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return name, req, false
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return name, req, false
	}
	return name, req, true
}

func (h *Handler) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	name, ok := h.deleteClientLocked(w, r)
	if !ok {
		return
	}
	h.evaluateClientQuota(name)
	h.recordAudit(r, "client_delete", name, "success", "")

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteClientLocked(w http.ResponseWriter, r *http.Request) (string, bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name := pathParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return name, false
	}

	existing, err := h.clientStore.Get(name)
	if err != nil {
		h.writeInternalError(w, "failed to load client", err)
		return name, false
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("client not found"))
		return name, false
	}

	if h.clientQuotaStore != nil {
		if err := h.clientQuotaStore.DeleteClientAndArchive(*existing); err != nil {
			h.writeInternalError(w, "failed to delete client", err)
			return name, false
		}
	} else if err := h.clientStore.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return name, false
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return name, false
	}
	return name, true
}

// validateClientPoolQuotas 校验 client.PoolQuotas：池引用必须存在（同
// allowed_pools 语义）、归一化池名；重复池名视为无效。
func (h *Handler) validateClientPoolQuotas(client *config.Client) error {
	if len(client.PoolQuotas) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(client.PoolQuotas))
	for i := range client.PoolQuotas {
		pq := &client.PoolQuotas[i]
		pq.Pool = strings.ToLower(strings.TrimSpace(pq.Pool))
		if pq.Pool == "" {
			return fmt.Errorf("pool_quotas[%d]: pool must not be empty", i)
		}
		if _, dup := seen[pq.Pool]; dup {
			return fmt.Errorf("pool_quotas: duplicate pool %q", pq.Pool)
		}
		seen[pq.Pool] = struct{}{}
		if err := h.validatePoolExists(pq.Pool); err != nil {
			return fmt.Errorf("pool_quotas[%d]: %w", i, err)
		}
	}
	return nil
}
