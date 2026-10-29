package admin

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"net/http"
	"strings"
)

// dictKindSpec parameterizes the shared dictionary handlers.
type dictKindSpec struct {
	metaKey  string
	respKey  string
	auditTag string
}

var (
	dictProjectGroups = dictKindSpec{metaKey: nosql.MetaKeyProjectGroups, respKey: "project_groups", auditTag: "project_group"}
)

func (h *Handler) handleListDict(kind dictKindSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := h.dictStore.List(kind.metaKey)
		if err != nil {
			h.writeInternalError(w, "failed to list "+kind.respKey, err)
			return
		}
		if items == nil {
			items = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{kind.respKey: items, "count": len(items)})
	}
}

func (h *Handler) handleDictCreate(kind dictKindSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
			return
		}
		if err := h.dictStore.Create(kind.metaKey, req.Name); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "already exists") {
				status = http.StatusConflict
			}
			writeJSON(w, status, errorResponse(err.Error()))
			return
		}
		h.recordAudit(r, kind.auditTag+"_create", req.Name, "success", "")
		writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
	}
}

func (h *Handler) handleDictUpdate(kind dictKindSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := pathParam(r, "name")
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
			return
		}
		if err := h.dictStore.Update(kind.metaKey, name, req.Name); err != nil {
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
		h.recordAudit(r, kind.auditTag+"_update", name, "success", req.Name)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (h *Handler) handleDictDelete(kind dictKindSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := pathParam(r, "name")
		if err := h.dictStore.Delete(kind.metaKey, name); err != nil {
			status := http.StatusNotFound
			writeJSON(w, status, errorResponse(err.Error()))
			return
		}
		h.recordAudit(r, kind.auditTag+"_delete", name, "success", "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// ===== 职级（结构化：日/周/月系数，额度体系 v2） =====

// handleListJobLevels 返回结构化职级目录（对象数组）。
func (h *Handler) handleListJobLevels(w http.ResponseWriter, r *http.Request) {
	items, err := h.jobLevelStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list job levels", err)
		return
	}
	if items == nil {
		items = []config.JobLevel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_levels": items, "count": len(items)})
}

func (h *Handler) handleCreateJobLevel(w http.ResponseWriter, r *http.Request) {
	var req config.JobLevel
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	if err := h.jobLevelStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	h.recordAudit(r, "job_level_create", req.Name, "success", "")
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

// handleUpdateJobLevel 更新职级系数；改名时级联更新所有绑定客户端的
// JobLevel（同一事务），避免客户端落入「未绑职级 = 拒绝」状态。
func (h *Handler) handleUpdateJobLevel(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	var req config.JobLevel
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	renamedClients, err := h.jobLevelStore.Update(name, req)
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
	h.scheduleQuotaRefresh(quota.RefreshScope{JobLevels: []string{name, req.Name}})
	detail := ""
	if len(renamedClients) > 0 {
		detail = "renamed_clients: " + strings.Join(renamedClients, ",")
	}
	h.recordAudit(r, "job_level_update", name, "success", detail)
	if renamedClients == nil {
		renamedClients = []string{}
	}
	writeJSON(w, http.StatusOK, h.withQuotaRefresh(map[string]any{"status": "ok", "renamed_clients": renamedClients}))
}

// handleDeleteJobLevel 删除职级。仍被活跃客户端绑定时拒绝并列出客户端名
// （解绑会把客户端打入「未绑职级 = 拒绝请求」状态，必须先人工改绑）。
func (h *Handler) handleDeleteJobLevel(w http.ResponseWriter, r *http.Request) {
	name := pathParam(r, "name")
	if err := h.jobLevelStore.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		if strings.Contains(err.Error(), "is referenced by clients") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	h.scheduleQuotaRefresh(quota.RefreshScope{JobLevels: []string{name}})
	h.recordAudit(r, "job_level_delete", name, "success", "")
	w.WriteHeader(http.StatusNoContent)
}
