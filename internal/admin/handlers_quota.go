package admin

import (
	"encoding/json"
	"errors"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"net/http"
	"sort"
	"strings"
)

// handleQuotaStatus 返回指定 client 的配额使用状态。
//   - quotaManager 为 nil 时返回 503 Service Unavailable。
//   - client 缺失返回 400 Bad Request。
//   - 正常返回 200 + quota.QuotaStatus JSON（无论是否超限）。
func (h *Handler) handleQuotaStatus(w http.ResponseWriter, r *http.Request) {
	clientName := pathParam(r, "client")
	if clientName == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("client name required"))
		return
	}
	if h.quotaManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("quota manager not configured"))
		return
	}
	status := h.quotaManager.Status(clientName)
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) handleQuotaSummary(w http.ResponseWriter, r *http.Request) {
	if h.quotaManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("quota manager not configured"))
		return
	}
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}
	groupsByClient, err := h.clientQuotaStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list client quota groups", err)
		return
	}
	type clientQuotaSummary struct {
		Name         string              `json:"name"`
		IPAddress    string              `json:"ip_address,omitempty"`
		JobLevel     string              `json:"job_level,omitempty"`
		ProjectGroup string              `json:"project_group,omitempty"`
		Deleted      bool                `json:"deleted,omitempty"`
		QuotaGroups  []config.QuotaGroup `json:"quota_groups,omitempty"`
	}
	type item struct {
		Client clientQuotaSummary `json:"client"`
		Status quota.QuotaStatus  `json:"status"`
	}
	activeNames := make(map[string]struct{}, len(clients))
	items := make([]item, 0, len(clients))
	for _, client := range clients {
		activeNames[client.Name] = struct{}{}
		items = append(items, item{
			Client: clientQuotaSummary{
				Name:         client.Name,
				IPAddress:    client.IPAddress,
				JobLevel:     client.JobLevel,
				ProjectGroup: client.ProjectGroup,
				QuotaGroups:  groupsByClient[client.Name],
			},
			Status: h.quotaManager.Status(client.Name),
		})
	}
	// Archived entries: clients deleted via the admin UI keep a snapshot row
	// (name/职级/项目组) so monthly statistics remain attributable. Their
	// quota status is empty — usage columns come from the aggregate endpoint.
	entries, err := h.clientQuotaStore.ListEntries()
	if err != nil {
		h.writeInternalError(w, "failed to list archived client quota entries", err)
		return
	}
	archivedNames := make([]string, 0, len(entries))
	for name := range entries {
		archivedNames = append(archivedNames, name)
	}
	sort.Strings(archivedNames)
	for _, name := range archivedNames {
		entry := entries[name]
		if entry.DeletedAt == "" {
			continue
		}
		if _, active := activeNames[entry.Name]; active {
			continue
		}
		items = append(items, item{
			Client: clientQuotaSummary{
				Name:         entry.Name,
				IPAddress:    entry.IPAddress,
				JobLevel:     entry.JobLevel,
				ProjectGroup: entry.ProjectGroup,
				Deleted:      true,
				QuotaGroups:  entry.Groups,
			},
			Status: quota.QuotaStatus{Client: entry.Name, Quotas: map[string]quota.QuotaUsage{}, Groups: map[string]quota.GroupStatus{}},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": items, "count": len(items)})
}

// handleSyncQuota 批量同步（额度体系 v2）：把源客户端的 plan（模型组选择 +
// 个人系数）同步到目标客户端。目标三种模式可组合（并集去重）：
//   - clients:        显式客户端名单
//   - project_group:  整个项目组（空串/「未分组」匹配无项目组的客户端）
//   - job_level:      整个职级（大小写不敏感）
//
// 职级绑定是客户端身份属性，不随同步覆盖；源自身从目标中排除。
func (h *Handler) handleSyncQuota(w http.ResponseWriter, r *http.Request) {
	var req nosql.QuotaSyncSelectors
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	source := pathParam(r, "client")
	names, err := h.clientQuotaStore.SyncQuota(source, req)
	if err != nil {
		var invalid *nosql.QuotaSyncValidationError
		switch {
		case errors.Is(err, nosql.ErrQuotaSyncSourceNotFound):
			writeJSON(w, http.StatusNotFound, errorResponse("client not found"))
		case errors.As(err, &invalid):
			writeJSON(w, http.StatusBadRequest, errorResponse(invalid.Error()))
		default:
			h.writeInternalError(w, "failed to sync quota groups", err)
		}
		return
	}
	if h.quotaManager != nil {
		h.quotaManager.NoteConfigChange()
		for _, name := range names {
			h.quotaManager.Evaluate(name)
		}
	}
	h.recordAudit(r, "quota_sync", source, "success", strings.Join(names, ","))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "synced_clients": names})
}

// handleUpdateQuota 仅更新指定 client 的额度配置（额度体系 v2）：职级绑定 +
// plan（模型组选择 + 个人系数）。个人日/周/月折算额度已退役（字段忽略）；
// 身份、密钥、授权等字段一律不动，避免编辑额度时往返敏感配置。
func (h *Handler) handleUpdateQuota(w http.ResponseWriter, r *http.Request) {
	clientName := pathParam(r, "client")
	if strings.TrimSpace(clientName) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("client name required"))
		return
	}
	var req struct {
		JobLevel    string              `json:"job_level"`
		QuotaGroups []config.QuotaGroup `json:"quota_groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	// Plan groups are validated before any write so a bad request leaves both
	// stores untouched.
	if err := h.clientQuotaStore.Validate(req.QuotaGroups); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	if err := h.clientQuotaStore.UpdateQuota(clientName, req.JobLevel, req.QuotaGroups); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "client") && strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	if h.quotaManager != nil {
		h.quotaManager.NoteConfigChange()
		h.quotaManager.Evaluate(clientName)
	}
	h.recordAudit(r, "quota_update", clientName, "success", "job_level="+strings.TrimSpace(req.JobLevel))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeleteQuotaEntry permanently removes an archived client quota entry
// (the "清除记录" action on the personnel quota page). Only archived entries —
// clients deleted via the admin UI — can be purged; active clients keep their
// entry managed through PUT /quota/{client}.
func (h *Handler) handleDeleteQuotaEntry(w http.ResponseWriter, r *http.Request) {
	clientName := pathParam(r, "client")
	if strings.TrimSpace(clientName) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("client name required"))
		return
	}
	existing, err := h.clientStore.Get(clientName)
	if err != nil {
		h.writeInternalError(w, "failed to load client", err)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("client is active; quota entry must be edited via PUT"))
		return
	}
	if err := h.clientQuotaStore.Delete(clientName); err != nil {
		h.writeInternalError(w, "failed to delete quota entry", err)
		return
	}
	h.recordAudit(r, "quota_entry_delete", clientName, "success", "")
	w.WriteHeader(http.StatusNoContent)
}
