package admin

import (
	"encoding/json"
	"errors"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strings"
)

// ===== Consumer group API =====

// groupNameParam extracts and validates the {name} path param for group
// routes (mirrors poolNameParam).
func groupNameParam(r *http.Request) (string, error) {
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if name == "" {
		return "", errors.New("group name must not be empty")
	}
	return name, nil
}

func (h *Handler) handleListModelGroups(w http.ResponseWriter, r *http.Request) {
	items, err := h.modelGroupStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model groups", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model_groups": items, "count": len(items)})
}

func (h *Handler) handleCreateModelGroup(w http.ResponseWriter, r *http.Request) {
	var req config.ModelGroup
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	if err := h.modelGroupStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}
	h.recordAudit(r, "model_group_create", req.Name, "success", "")
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (h *Handler) handleUpdateModelGroup(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req config.ModelGroup
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	renamedClients, err := h.modelGroupStore.Update(name, req)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}
	detail := ""
	if len(renamedClients) > 0 {
		detail = "renamed_clients: " + strings.Join(renamedClients, ",")
	}
	h.recordAudit(r, "model_group_update", name, "success", detail)
	if renamedClients == nil {
		renamedClients = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "renamed_clients": renamedClients})
}

func (h *Handler) handleDeleteModelGroup(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	removedClients, err := h.modelGroupStore.Delete(name)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}
	detail := ""
	if len(removedClients) > 0 {
		detail = "removed_from_clients: " + strings.Join(removedClients, ",")
	}
	h.recordAudit(r, "model_group_delete", name, "success", detail)
	if removedClients == nil {
		removedClients = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "removed_from_clients": removedClients})
}
