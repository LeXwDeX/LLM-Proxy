package admin

import (
	"errors"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"net/http"
	"time"
)

func (h *Handler) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	path := h.configManager.Path()
	prev, _ := h.configManager.Current()

	cfg, err := config.Load(path)
	if err != nil {
		h.logger.Error("admin reload failed: load config",
			"path", path,
			"error", err,
		)
		writeJSON(w, http.StatusInternalServerError, errorResponse("failed to load config"))
		return
	}
	if err := h.overlayStoredTargets(cfg); err != nil {
		h.writeInternalError(w, "failed to load targets", err)
		return
	}
	if err := h.overlayStoredPools(cfg); err != nil {
		h.writeInternalError(w, "failed to load pools", err)
		return
	}

	// Use the existing clientStore (backed by bbolt, not file path).
	tempStore := auth.NewStore()
	clients, err := h.clientStore.List()
	if err != nil {
		h.logger.Warn("admin reload rejected: invalid clients",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid clients"))
		return
	}

	if err := tempStore.LoadFromConfigWithPools(clients, cfg.Targets); err != nil {
		h.logger.Warn("admin reload rejected: invalid clients",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid client configuration"))
		return
	}

	if err := h.proxyService.ApplyConfig(cfg); err != nil {
		h.logger.Warn("admin reload rejected: invalid proxy configuration",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid proxy configuration"))
		return
	}

	if err := h.authStore.LoadFromConfigWithPools(clients, cfg.Targets); err != nil {
		h.logger.Error("admin reload failed: auth apply",
			"error", err,
		)
		if prev != nil {
			if revertErr := h.proxyService.ApplyConfig(prev); revertErr != nil {
				h.logger.Error("admin reload revert failed", "error", revertErr)
			}
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse("failed to update auth configuration"))
		return
	}

	if poolStore := h.poolStore(); poolStore != nil && len(cfg.Pools) > 0 {
		if _, err := poolStore.MigrateFromConfig(cfg.Pools); err != nil {
			h.writeInternalError(w, "failed to persist pools", err)
			return
		}
	}

	h.configManager.Replace(cfg)
	if h.costCache != nil {
		h.costCache.Invalidate()
	}
	h.noteQuotaConfigChange()
	h.warmCostCache()
	// m11：同步 admin_session 配置（secret/ttl），否则 reload 返回 ok 但 sessionManager
	// 仍持旧 secret，运维轮换 secret 后状态不一致。
	h.sessionManager.UpdateConfig(cfg.AdminSession)
	h.recordAudit(r, "config_reload", path, "success", fmt.Sprintf("targets=%d clients=%d", len(cfg.Targets), len(clients)))
	h.logger.Info("configuration reloaded via admin endpoint",
		"path", path,
		"targets", len(cfg.Targets),
		"clients", len(clients),
	)

	response := struct {
		Status     string    `json:"status"`
		ReloadedAt time.Time `json:"reloaded_at"`
		Targets    int       `json:"targets"`
		Clients    int       `json:"clients"`
	}{
		Status:     "ok",
		ReloadedAt: time.Now(),
		Targets:    len(cfg.Targets),
		Clients:    len(clients),
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) currentConfig() (*config.Config, error) {
	cfg, err := h.configManager.Current()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("config unavailable")
	}
	if err := h.applyStoredTargets(cfg); err != nil {
		return nil, err
	}
	if err := h.applyStoredPools(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (h *Handler) applyStoredTargets(cfg *config.Config) error {
	if h.targetStore == nil {
		return nil
	}
	if _, err := h.targetStore.MigrateFromConfig(cfg.Targets, h.logger); err != nil {
		return err
	}
	if err := h.targetStore.MigrateUsageAdapterExplicit(h.logger); err != nil {
		return err
	}
	targets, err := h.targetStore.List()
	if err != nil {
		return err
	}
	// The database owns targets even if List skipped every residual invalid
	// record. Never publish legacy file targets as an empty-store fallback.
	cfg.Targets = targets
	return nil
}

func (h *Handler) overlayStoredTargets(cfg *config.Config) error {
	return h.applyStoredTargets(cfg)
}

// applyStoredPools mirrors applyStoredTargets: overlay bbolt-stored pools onto
// the config snapshot, lazily migrating file-config pools when the bucket is
// still empty (first touch after upgrade).
func (h *Handler) applyStoredPools(cfg *config.Config) error {
	poolStore := h.poolStore()
	if poolStore == nil {
		return nil
	}
	pools, err := poolStore.List()
	if err != nil {
		return err
	}
	if len(pools) == 0 && len(cfg.Pools) > 0 {
		if _, err := poolStore.MigrateFromConfig(cfg.Pools); err != nil {
			return err
		}
		pools, err = poolStore.List()
		if err != nil {
			return err
		}
	}
	if len(pools) > 0 || len(cfg.Pools) == 0 {
		cfg.Pools = pools
	}
	return nil
}

// overlayStoredPools mirrors overlayStoredTargets for the reload path: the
// store wins when non-empty; file pools only apply when the bucket is empty.
func (h *Handler) overlayStoredPools(cfg *config.Config) error {
	poolStore := h.poolStore()
	if poolStore == nil {
		return nil
	}
	pools, err := poolStore.List()
	if err != nil {
		return err
	}
	if len(pools) > 0 || len(cfg.Pools) == 0 {
		cfg.Pools = pools
	}
	return nil
}

func (h *Handler) reloadAuthFromClientStore() error {
	clients, err := h.clientStore.List()
	if err != nil {
		return err
	}
	var targets []config.Target
	if h.targetStore != nil {
		targets, err = h.targetStore.List()
		if err != nil {
			return err
		}
	}
	return h.authStore.LoadFromConfigWithPools(clients, targets)
}
