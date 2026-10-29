package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"net/http"
	"strings"
)

// ===== Pool CRUD =====

func poolNameParam(r *http.Request) (string, error) {
	name, err := decodedPathParam(r, "name")
	if err != nil {
		return "", errors.New("invalid pool name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	return name, nil
}

func (h *Handler) handleListPools(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	pools := cfg.Pools
	if pools == nil {
		pools = []config.DeploymentPool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pools": pools,
		"count": len(pools),
	})
}

func (h *Handler) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	var body struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Paused      bool   `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	name := strings.ToLower(strings.TrimSpace(body.Name))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("name must not be empty"))
		return
	}

	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(body.Name)
	}
	if strings.ToLower(displayName) != name {
		writeJSON(w, http.StatusBadRequest, errorResponse("pool display name may only change letter casing"))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	for _, p := range cfg.Pools {
		if strings.EqualFold(p.Name, name) {
			writeJSON(w, http.StatusConflict, errorResponse(fmt.Sprintf("pool %q already exists", name)))
			return
		}
	}

	pool := config.DeploymentPool{
		Name:        name,
		DisplayName: displayName,
		Description: strings.TrimSpace(body.Description),
		Paused:      body.Paused,
	}
	cfg.Pools = append(cfg.Pools, pool)

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool", errors.New("pool store unavailable"))
		return
	}
	if err := poolStore.Create(pool); err != nil {
		h.writeInternalError(w, "failed to persist pool", err)
		return
	}
	h.configManager.Replace(cfg)

	h.recordAudit(r, "pool_create", name, "success", "")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name})
}

func (h *Handler) handleUpdatePool(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := poolNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	var body struct {
		Name        string  `json:"name"`
		DisplayName *string `json:"display_name"`
		Description *string `json:"description"`
		Paused      *bool   `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	// 池名不可变：改名会让成员 target 的 pool 引用悬空。
	if strings.TrimSpace(body.Name) != "" && !strings.EqualFold(strings.TrimSpace(body.Name), name) {
		writeJSON(w, http.StatusBadRequest, errorResponse("pool name cannot be changed"))
		return
	}

	if body.DisplayName != nil && strings.ToLower(strings.TrimSpace(*body.DisplayName)) != strings.ToLower(name) {
		writeJSON(w, http.StatusBadRequest, errorResponse("pool display name may only change letter casing"))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	var updatedPool config.DeploymentPool
	for i := range cfg.Pools {
		if strings.EqualFold(cfg.Pools[i].Name, name) {
			if body.DisplayName != nil {
				cfg.Pools[i].DisplayName = strings.TrimSpace(*body.DisplayName)
			}
			if body.Description != nil {
				cfg.Pools[i].Description = strings.TrimSpace(*body.Description)
			}
			if body.Paused != nil {
				cfg.Pools[i].Paused = *body.Paused
			}
			updatedPool = cfg.Pools[i]
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("pool %q not found", name)))
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool", errors.New("pool store unavailable"))
		return
	}
	if err := poolStore.Update(name, updatedPool); err != nil {
		h.writeInternalError(w, "failed to persist pool", err)
		return
	}
	h.configManager.Replace(cfg)

	h.recordAudit(r, "pool_update", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": updatedPool.Name, "paused": updatedPool.Paused})
}

func (h *Handler) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := poolNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	next := make([]config.DeploymentPool, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		if strings.EqualFold(p.Name, name) {
			found = true
			continue
		}
		next = append(next, p)
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("pool %q not found", name)))
		return
	}

	// 有成员的池拒绝删除，否则成员 target 的 pool 引用会悬空。
	var members []string
	for _, t := range cfg.Targets {
		if strings.EqualFold(strings.TrimSpace(t.Pool), name) {
			members = append(members, t.Name)
		}
	}
	if len(members) > 0 {
		writeJSON(w, http.StatusConflict, errorResponse(fmt.Sprintf("pool %q has %d members: %s", name, len(members), strings.Join(members, ","))))
		return
	}

	cfg.Pools = next

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool deletion", errors.New("pool store unavailable"))
		return
	}
	if err := poolStore.Delete(name); err != nil {
		h.writeInternalError(w, "failed to persist pool deletion", err)
		return
	}
	h.configManager.Replace(cfg)

	h.recordAudit(r, "pool_delete", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handlePausePool(w http.ResponseWriter, r *http.Request) {
	h.handleSetPoolPaused(w, r, true)
}

func (h *Handler) handleResumePool(w http.ResponseWriter, r *http.Request) {
	h.handleSetPoolPaused(w, r, false)
}

func (h *Handler) handleSetPoolPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	name, err := poolNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	var updatedPool config.DeploymentPool
	for i := range cfg.Pools {
		if strings.EqualFold(cfg.Pools[i].Name, name) {
			cfg.Pools[i].Paused = paused
			updatedPool = cfg.Pools[i]
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("pool %q not found", name)))
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}
	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool", errors.New("pool store unavailable"))
		return
	}
	if err := poolStore.Update(name, updatedPool); err != nil {
		h.writeInternalError(w, "failed to persist pool", err)
		return
	}
	h.configManager.Replace(cfg)

	action := "pool_resume"
	if paused {
		action = "pool_pause"
	}
	h.recordAudit(r, action, name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": updatedPool.Name, "paused": paused})
}

// poolStore returns the deployment-pool store view sharing the target store's
// db handle. Derived from targetStore so NewHandler's signature stays unchanged.
func (h *Handler) poolStore() *nosql.PoolStore {
	if h.targetStore == nil {
		return nil
	}
	return h.targetStore.Pools()
}

// validatePoolExists reports an error when the referenced pool is unknown to
// the pool store (lowercase comparison, mirroring target name matching).
func (h *Handler) validatePoolExists(pool string) error {
	poolStore := h.poolStore()
	if poolStore == nil {
		return nil
	}
	pools, err := poolStore.List()
	if err != nil {
		return err
	}
	for _, p := range pools {
		if strings.EqualFold(strings.TrimSpace(p.Name), pool) {
			return nil
		}
	}
	return fmt.Errorf("unknown pool %q", pool)
}
