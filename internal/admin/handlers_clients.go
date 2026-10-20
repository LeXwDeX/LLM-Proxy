package admin

import (
	"encoding/json"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strings"
)

func (h *Handler) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": clients,
		"count":   len(clients),
	})
}

func (h *Handler) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	if err := h.validateClientPoolQuotas(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	if err := h.clientStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	// Trigger quota evaluation for this client (config may have changed).
	if h.quotaManager != nil {
		h.quotaManager.Evaluate(req.Name)
	}
	h.recordAudit(r, "client_create", req.Name, "success", maskKey(req.AccessKey))

	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (h *Handler) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return
	}

	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	if err := h.validateClientPoolQuotas(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	if err := h.clientStore.Update(name, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	// Trigger quota evaluation (config may have changed).
	if h.quotaManager != nil {
		h.quotaManager.Evaluate(name)
	}
	h.recordAudit(r, "client_update", name, "success", maskKey(req.AccessKey))

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return
	}

	existing, err := h.clientStore.Get(name)
	if err != nil {
		h.writeInternalError(w, "failed to load client", err)
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("client not found"))
		return
	}

	if h.clientQuotaStore != nil {
		if err := h.clientQuotaStore.DeleteClientAndArchive(*existing); err != nil {
			h.writeInternalError(w, "failed to delete client", err)
			return
		}
	} else if err := h.clientStore.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	if h.quotaManager != nil {
		h.quotaManager.Evaluate(name)
	}
	h.recordAudit(r, "client_delete", name, "success", "")

	w.WriteHeader(http.StatusNoContent)
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
