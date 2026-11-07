package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"net/http"
	"strings"
	"time"
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

// poolCostRoutingResponse adds the pool identity to the proxy's read-only
// snapshot. Members and pricing fields are embedded from the proxy struct.
type poolCostRoutingResponse struct {
	Pool            string `json:"pool"`
	RoutingStrategy string `json:"routing_strategy"`
	Active          bool   `json:"active"`
	Paused          bool   `json:"paused"`
	proxy.CostRoutingPreview
}

// poolCostRoutingInactiveResponse deliberately omits the preview fields for a
// non-cost pool so a zero-valued snapshot cannot be mistaken for real data.
type poolCostRoutingInactiveResponse struct {
	Pool            string `json:"pool"`
	RoutingStrategy string `json:"routing_strategy"`
	Active          bool   `json:"active"`
	Paused          bool   `json:"paused"`
}

// handlePoolCostRoutingPreview returns the alias-first cost ranking for one
// pool and one client request name. It is read-only: no probes, no affinity,
// no audit, and no model-group coefficient (the proxy resolver is used).
func (h *Handler) handlePoolCostRoutingPreview(w http.ResponseWriter, r *http.Request) {
	name, err := poolNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("model is required"))
		return
	}
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}
	var pool *config.DeploymentPool
	for i := range cfg.Pools {
		if strings.EqualFold(strings.TrimSpace(cfg.Pools[i].Name), name) {
			pool = &cfg.Pools[i]
			break
		}
	}
	if pool == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("pool not found"))
		return
	}
	normalized := config.NormalizeDeploymentPool(*pool)
	strategy := normalized.EffectiveRoutingStrategy()
	if strategy != config.PoolRoutingCost {
		writeJSON(w, http.StatusOK, poolCostRoutingInactiveResponse{
			Pool:            normalized.Name,
			RoutingStrategy: strategy,
			Active:          false,
			Paused:          normalized.Paused,
		})
		return
	}
	writeJSON(w, http.StatusOK, poolCostRoutingResponse{
		Pool:               normalized.Name,
		RoutingStrategy:    strategy,
		Active:             true,
		Paused:             normalized.Paused,
		CostRoutingPreview: h.proxyService.CostRoutingPreview(normalized.Name, model, time.Now()),
	})
}

func (h *Handler) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	limitConfigBody(w, r)
	var body struct {
		Name            string `json:"name"`
		DisplayName     string `json:"display_name"`
		Description     string `json:"description"`
		Paused          bool   `json:"paused"`
		RoutingStrategy string `json:"routing_strategy"`
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
	strategy, ok := parsePoolRoutingStrategy(w, body.RoutingStrategy)
	if !ok {
		return
	}

	txn := h.lockConfig(r)
	defer txn.Unlock()
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
		Name:            name,
		DisplayName:     displayName,
		Description:     strings.TrimSpace(body.Description),
		Paused:          body.Paused,
		RoutingStrategy: strategy,
	}
	cfg.Pools = append(cfg.Pools, pool)

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool", errors.New("pool store unavailable"))
		return
	}
	warning, ok := h.commitConfigChange(w, cfg, "failed to persist pool", func() error {
		return poolStore.Create(pool)
	})
	if !ok {
		return
	}

	txn.audit("pool_create", name, "success", "")
	writeJSON(w, http.StatusCreated, withWarning(map[string]any{"ok": true, "name": name}, warning))
}

func (h *Handler) handleUpdatePool(w http.ResponseWriter, r *http.Request) {
	limitConfigBody(w, r)
	name, err := poolNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	var body struct {
		Name            string  `json:"name"`
		DisplayName     *string `json:"display_name"`
		Description     *string `json:"description"`
		Paused          *bool   `json:"paused"`
		RoutingStrategy *string `json:"routing_strategy"`
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
	var strategy string
	if body.RoutingStrategy != nil {
		var ok bool
		if strategy, ok = parsePoolRoutingStrategy(w, *body.RoutingStrategy); !ok {
			return
		}
	}

	txn := h.lockConfig(r)
	defer txn.Unlock()
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
			if body.RoutingStrategy != nil {
				cfg.Pools[i].RoutingStrategy = strategy
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

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool", errors.New("pool store unavailable"))
		return
	}
	warning, ok := h.commitConfigChange(w, cfg, "failed to persist pool", func() error {
		return poolStore.Update(name, updatedPool)
	})
	if !ok {
		return
	}

	txn.audit("pool_update", name, "success", "")
	writeJSON(w, http.StatusOK, withWarning(map[string]any{"ok": true, "name": updatedPool.Name, "paused": updatedPool.Paused}, warning))
}

func (h *Handler) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	txn := h.lockConfig(r)
	defer txn.Unlock()
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

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool deletion", errors.New("pool store unavailable"))
		return
	}
	warning, ok := h.commitConfigChange(w, cfg, "failed to persist pool deletion", func() error {
		return poolStore.Delete(name)
	})
	if !ok {
		return
	}

	txn.audit("pool_delete", name, "success", "")
	writeJSON(w, http.StatusOK, withWarning(map[string]any{"ok": true}, warning))
}

func (h *Handler) handlePausePool(w http.ResponseWriter, r *http.Request) {
	h.handleSetPoolPaused(w, r, true)
}

func (h *Handler) handleResumePool(w http.ResponseWriter, r *http.Request) {
	h.handleSetPoolPaused(w, r, false)
}

func (h *Handler) handleSetPoolPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	txn := h.lockConfig(r)
	defer txn.Unlock()
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

	poolStore := h.poolStore()
	if poolStore == nil {
		h.writeInternalError(w, "failed to persist pool", errors.New("pool store unavailable"))
		return
	}
	warning, ok := h.commitConfigChange(w, cfg, "failed to persist pool", func() error {
		return poolStore.Update(name, updatedPool)
	})
	if !ok {
		return
	}

	action := "pool_resume"
	if paused {
		action = "pool_pause"
	}
	txn.audit(action, name, "success", "")
	writeJSON(w, http.StatusOK, withWarning(map[string]any{"ok": true, "name": updatedPool.Name, "paused": paused}, warning))
}

// poolStore returns the deployment-pool store view sharing the target store's
// db handle. Derived from targetStore so NewHandler's signature stays unchanged.
// parsePoolRoutingStrategy normalizes and validates a requested strategy,
// writing the 400 response itself when the value is not a public option.
func parsePoolRoutingStrategy(w http.ResponseWriter, raw string) (string, bool) {
	strategy := strings.ToLower(strings.TrimSpace(raw))
	if !config.IsValidPoolRoutingStrategy(strategy) {
		writeJSON(w, http.StatusBadRequest, errorResponse(fmt.Sprintf("unsupported routing_strategy %q", raw)))
		return "", false
	}
	return strategy, true
}

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
