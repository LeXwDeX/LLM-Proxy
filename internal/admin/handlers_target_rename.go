package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

// handleRenameTarget 是受支持的目标改名/合并入口（Issue #195）：
//
//	POST /admin/data/targets/{name}/rename  {"new_name": "...", "merge": false}
//
// 历史 usage 事件与用量/额度聚合按写入时的 target 名记账，接口不重写也不删除它们；
// 而是在同一 bbolt 事务内让价表与模型组绑定新旧名并存（改名），或保留源目标的
// 绑定（合并），并把客户端 allowed_targets 中的旧名替换为新名。可带 If-Match（源目标 ETag）。
func (h *Handler) handleRenameTarget(w http.ResponseWriter, r *http.Request) {
	limitConfigBody(w, r)
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	im, ifMatchErr := parseIfMatch(r)
	if ifMatchErr != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(ifMatchErr.Error()))
		return
	}
	var body struct {
		NewName string `json:"new_name"`
		Merge   bool   `json:"merge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}
	newName := strings.TrimSpace(body.NewName)
	if newName == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("new_name must not be empty"))
		return
	}
	if strings.ContainsRune(newName, 0) {
		writeJSON(w, http.StatusBadRequest, errorResponse("new_name contains invalid characters"))
		return
	}
	if body.Merge && strings.EqualFold(newName, name) {
		writeJSON(w, http.StatusBadRequest, errorResponse("cannot merge a target into itself"))
		return
	}

	txn := h.lockConfig(r)
	defer txn.Unlock()
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}
	srcIdx, dstIdx := -1, -1
	for i := range cfg.Targets {
		switch {
		case strings.EqualFold(cfg.Targets[i].Name, name):
			srcIdx = i
		case strings.EqualFold(cfg.Targets[i].Name, newName):
			dstIdx = i
		}
	}
	if srcIdx < 0 {
		if im.present {
			writeTargetRevisionConflict(w)
			return
		}
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}
	if body.Merge && dstIdx < 0 {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("merge destination %q not found", newName)))
		return
	}
	if !body.Merge && dstIdx >= 0 {
		writeJSON(w, http.StatusConflict, errorResponse(fmt.Sprintf("target %q already exists; set merge to combine", newName)))
		return
	}
	var matchedETag *string
	if im.present {
		etag, ok := h.resolveIfMatch(w, name, im)
		if !ok {
			return
		}
		matchedETag = &etag
	}
	oldName := cfg.Targets[srcIdx].Name

	if body.Merge {
		cfg.Targets = append(cfg.Targets[:srcIdx:srcIdx], cfg.Targets[srcIdx+1:]...)
	} else {
		cfg.Targets[srcIdx].Name = newName
	}
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to apply config", fmt.Errorf("load clients: %w", err))
		return
	}
	for i := range clients {
		clients[i], _ = nosql.RenameClientAllowedTarget(clients[i], oldName, newName)
	}

	var result nosql.TargetRenameResult
	warning, ok := h.commitConfigChangeWithClients(w, cfg, clients, "failed to persist target rename", func() error {
		var err error
		result, err = h.targetStore.Rename(nosql.TargetRenameRequest{From: oldName, To: newName, Merge: body.Merge, ExpectedETag: matchedETag})
		return err
	})
	if !ok {
		return
	}

	// 价表与模型组绑定都变了：刷新全部额度客户端（与计价变更同一口径）。
	txn.refresh(quota.RefreshScope{All: true})
	action := "rename_target"
	if body.Merge {
		action = "merge_target"
	}
	txn.audit(action, oldName, "success", renameAuditDetail(result))
	writeJSON(w, http.StatusOK, h.withQuotaRefresh(withWarning(map[string]any{
		"ok": true, "from": result.From, "to": result.To, "merged": result.Merged,
		"pricing_templates": result.PricingTemplates, "model_groups": result.ModelGroups, "clients": result.Clients,
		"etag": h.targetETag(newName),
	}, warning)))
}

func renameAuditDetail(res nosql.TargetRenameResult) string {
	return fmt.Sprintf("to=%s merged=%t pricing_templates=%s model_groups=%s clients=%s",
		res.To, res.Merged, strings.Join(res.PricingTemplates, ","), strings.Join(res.ModelGroups, ","), strings.Join(res.Clients, ","))
}
