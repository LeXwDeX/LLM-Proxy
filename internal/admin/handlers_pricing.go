package admin

import (
	"encoding/json"
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/costutil"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func (h *Handler) handleListModelCosts(w http.ResponseWriter, r *http.Request) {
	templates, err := h.pricingTemplateStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}

	type modelCostProjection struct {
		EndpointType                string   `json:"endpoint_type"`
		Model                       string   `json:"model"`
		InputPer1MTokens            float64  `json:"input_per_1m_tokens"`
		OutputPer1MTokens           float64  `json:"output_per_1m_tokens"`
		CacheWritePer1MToken        float64  `json:"cache_write_per_1m_tokens"`
		CacheReadPer1MToken         float64  `json:"cache_read_per_1m_tokens"`
		CacheWrite1hPer1MToken      *float64 `json:"cache_write_1h_per_1m_tokens,omitempty"`
		ImageInputPer1MTokens       *float64 `json:"image_input_per_1m_tokens,omitempty"`
		CachedImageInputPer1MTokens *float64 `json:"cached_image_input_per_1m_tokens,omitempty"`
		ImageOutputPer1MTokens      *float64 `json:"image_output_per_1m_tokens,omitempty"`
		CacheWrite5mPer1MToken      *float64 `json:"cache_write_5m_per_1m_tokens,omitempty"`
		ExplicitCacheReadPer1MToken *float64 `json:"explicit_cache_read_per_1m_tokens,omitempty"`
	}

	projections := make([]modelCostProjection, 0, len(templates))
	for _, t := range templates {
		if !pricing.IsTemplate0(&t) {
			continue
		}
		for _, model := range t.EffectiveModels() {
			projections = append(projections, modelCostProjection{
				EndpointType:                "",
				Model:                       model,
				InputPer1MTokens:            t.BaseRates.InputPer1MTokens,
				OutputPer1MTokens:           t.BaseRates.OutputPer1MTokens,
				CacheWritePer1MToken:        t.BaseRates.CacheWritePer1MToken,
				CacheReadPer1MToken:         t.BaseRates.CacheReadPer1MToken,
				CacheWrite1hPer1MToken:      t.BaseRates.CacheWrite1hPer1MToken,
				ImageInputPer1MTokens:       t.BaseRates.ImageInputPer1MTokens,
				CachedImageInputPer1MTokens: t.BaseRates.CachedImageInputPer1MTokens,
				ImageOutputPer1MTokens:      t.BaseRates.ImageOutputPer1MTokens,
				CacheWrite5mPer1MToken:      t.BaseRates.CacheWrite5mPer1MToken,
				ExplicitCacheReadPer1MToken: t.BaseRates.ExplicitCacheReadPer1MToken,
			})
		}
	}
	sort.Slice(projections, func(i, j int) bool { return projections[i].Model < projections[j].Model })

	writeJSON(w, http.StatusOK, map[string]any{
		"models": projections,
		"count":  len(projections),
	})
}

func (h *Handler) handleUpsertModelCost(w http.ResponseWriter, r *http.Request) {
	// URL-decode path param: Go net/http does not decode %2F in path segments,
	// so model names like "MiniMax/MiniMax-M3" arrive as "MiniMax%2FMiniMax-M3".
	rawModel := chi.URLParam(r, "model")
	if decoded, err := url.PathUnescape(rawModel); err == nil {
		rawModel = decoded
	}
	model := strings.ToLower(strings.TrimSpace(rawModel))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing model"))
		return
	}

	var req struct {
		EndpointType                string   `json:"endpoint_type"`
		InputPer1MTokens            float64  `json:"input_per_1m_tokens"`
		OutputPer1MTokens           float64  `json:"output_per_1m_tokens"`
		CacheWritePer1MToken        float64  `json:"cache_write_per_1m_tokens"`
		CacheReadPer1MToken         float64  `json:"cache_read_per_1m_tokens"`
		CacheWrite1hPer1MToken      *float64 `json:"cache_write_1h_per_1m_tokens,omitempty"`
		ImageInputPer1MTokens       *float64 `json:"image_input_per_1m_tokens,omitempty"`
		CachedImageInputPer1MTokens *float64 `json:"cached_image_input_per_1m_tokens,omitempty"`
		ImageOutputPer1MTokens      *float64 `json:"image_output_per_1m_tokens,omitempty"`
		CacheWrite5mPer1MToken      *float64 `json:"cache_write_5m_per_1m_tokens,omitempty"`
		ExplicitCacheReadPer1MToken *float64 `json:"explicit_cache_read_per_1m_tokens,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	tmpl := pricing.PricingTemplate{
		Target:   "",
		Model:    model,
		Mode:     pricing.ModeFlat,
		Timezone: pricing.DefaultTimezone,
		BaseRates: usage.CostRates{
			InputPer1MTokens:            req.InputPer1MTokens,
			OutputPer1MTokens:           req.OutputPer1MTokens,
			CacheWritePer1MToken:        req.CacheWritePer1MToken,
			CacheReadPer1MToken:         req.CacheReadPer1MToken,
			CacheWrite1hPer1MToken:      req.CacheWrite1hPer1MToken,
			ImageInputPer1MTokens:       req.ImageInputPer1MTokens,
			CachedImageInputPer1MTokens: req.CachedImageInputPer1MTokens,
			ImageOutputPer1MTokens:      req.ImageOutputPer1MTokens,
			CacheWrite5mPer1MToken:      req.CacheWrite5mPer1MToken,
			ExplicitCacheReadPer1MToken: req.ExplicitCacheReadPer1MToken,
		},
	}
	// Reuse an existing template-0 ID for ("", model) so this is an upsert, not a
	// duplicate template 0.
	if existing, err := h.pricingTemplateStore.ListByTargetModel("", model); err == nil {
		for _, e := range existing {
			if pricing.IsTemplate0(&e) {
				if e.Shared() {
					writeJSON(w, http.StatusBadRequest, errorResponse("shared pricing rule must be changed through pricing-templates by ID"))
					return
				}
				tmpl.ID = e.ID
				break
			}
		}
	}
	if _, err := h.pricingTemplateStore.Upsert(tmpl); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	if h.costCache != nil {
		h.costCache.Invalidate()
	}
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}
	h.recordAudit(r, "model_cost_upsert", model, "success", "")

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"model": model,
	})
}

func (h *Handler) handleDeleteModelCost(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(pathParam(r, "model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing model"))
		return
	}

	id := ""
	if existing, err := h.pricingTemplateStore.ListByTargetModel("", model); err == nil {
		for _, e := range existing {
			if pricing.IsTemplate0(&e) {
				if e.Shared() {
					writeJSON(w, http.StatusBadRequest, errorResponse("shared pricing rule must be changed through pricing-templates by ID"))
					return
				}
				id = e.ID
				break
			}
		}
	}
	if id == "" {
		writeJSON(w, http.StatusNotFound, errorResponse("model not found"))
		return
	}
	if err := h.pricingTemplateStore.Delete(id); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	if h.costCache != nil {
		h.costCache.Invalidate()
	}
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}
	h.recordAudit(r, "model_cost_delete", model, "success", "")

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListPricingTemplates(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	var templates []pricing.PricingTemplate
	var err error
	if target != "" || model != "" {
		templates, err = h.pricingTemplateStore.ListByTargetModel(target, model)
	} else {
		templates, err = h.pricingTemplateStore.List()
	}
	if err != nil {
		h.writeInternalError(w, "failed to list pricing templates", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": templates, "count": len(templates)})
}

func (h *Handler) handleCreatePricingTemplate(w http.ResponseWriter, r *http.Request) {
	var t pricing.PricingTemplate
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	legacy := t.Targets == nil && t.Models == nil
	if err := pricing.ValidateBindings(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	pricing.NormalizeTemplate(&t)
	if !legacy {
		if err := h.validatePricingBindings(t); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
			return
		}
	}
	t.ID = ""
	// Template 0 is a singleton per (target, model): reuse the existing ID if one exists.
	if legacy && pricing.IsTemplate0(&t) {
		if existing, err := h.pricingTemplateStore.ListByTargetModel(t.Target, t.Model); err == nil {
			for _, e := range existing {
				if pricing.IsTemplate0(&e) && !e.Shared() && e.SameBindings(t) {
					t.ID = e.ID
					break
				}
			}
		}
	}
	stored, err := h.pricingTemplateStore.Upsert(t)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	h.afterPricingTemplateWrite(r, "pricing_template_create", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "template": stored})
}

func (h *Handler) handleUpdatePricingTemplate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing template id"))
		return
	}
	var t pricing.PricingTemplate
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	t.ID = id
	// Binding sets are immutable: reinterpreting a shared rule for different
	// targets/models would alter historical event pricing.
	existing, err := h.pricingTemplateStore.Get(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse(err.Error()))
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("pricing template not found"))
		return
	}
	if err := pricing.ValidateBindings(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	pricing.NormalizeTemplate(&t)
	if !t.SameBindings(*existing) {
		writeJSON(w, http.StatusBadRequest, errorResponse("target/model bindings cannot be changed on update; create a new rule with a separate effective interval instead"))
		return
	}
	stored, err := h.pricingTemplateStore.Upsert(t)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	h.afterPricingTemplateWrite(r, "pricing_template_update", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "template": stored})
}

func (h *Handler) handleDeletePricingTemplate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing template id"))
		return
	}
	if err := h.pricingTemplateStore.Delete(id); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}
	h.afterPricingTemplateWrite(r, "pricing_template_delete", id)
	w.WriteHeader(http.StatusNoContent)
}

// afterPricingTemplateWrite invalidates the resolver cache, refreshes quota
// in-memory snapshots, and records an audit event after a successful write.
func (h *Handler) afterPricingTemplateWrite(r *http.Request, action, id string) {
	if h.costCache != nil {
		h.costCache.Invalidate()
	}
	if h.quotaManager != nil {
		h.quotaManager.RefreshAll()
	}
	h.recordAudit(r, action, id, "success", "")
}

// getResolver returns the rate resolver used by all statistics views. The
// base resolver (pricing cache or model-cost fallback) is wrapped with the
// global model-group cost coefficient so report costs and quota deductions
// share the same discounted basis (quota applies the same coefficient values
// on its own counters).
func (h *Handler) getResolver() usage.RateResolver {
	var base usage.RateResolver
	if h.costCache != nil {
		base = h.costCache.Get() // *pricing.Resolver
	} else {
		var costs []nosql.ModelCost
		if h.modelCostStore != nil {
			costs, _ = h.modelCostStore.List()
		}
		base = costutil.ToCostTable(costs, h.modelCatalog)
	}
	if h.modelGroupStore != nil {
		if lookup, err := h.modelGroupStore.CoefficientLookup(); err == nil && lookup != nil {
			return usage.WithCoefficient(base, lookup)
		}
	}
	return base
}

// validatePricingBindings validates the complete Cartesian product before a new
// shared rule is written. Billing names are configured, unprefixed upstream names.
// Existing rule edits preserve historical bindings even if targets were removed.
func (h *Handler) validatePricingBindings(t pricing.PricingTemplate) error {
	if err := pricing.ValidateBindings(&t); err != nil {
		return err
	}
	targets := t.EffectiveTargets()
	if len(targets) == 1 && targets[0] == "" {
		return nil
	}
	if h.targetStore == nil {
		return fmt.Errorf("pricing: target store unavailable")
	}
	configured, err := h.targetStore.List()
	if err != nil {
		return fmt.Errorf("pricing: cannot list targets: %w", err)
	}
	for _, name := range targets {
		found := false
		for _, target := range configured {
			if !strings.EqualFold(strings.TrimSpace(target.Name), name) {
				continue
			}
			found = true
			for _, model := range t.EffectiveModels() {
				matched := false
				for _, mapping := range target.ModelMappings {
					if strings.EqualFold(strings.TrimSpace(mapping.Upstream), model) {
						matched = true
						break
					}
				}
				if !matched {
					return fmt.Errorf("pricing: target %q does not configure model %q", name, model)
				}
			}
			break
		}
		if !found {
			return fmt.Errorf("pricing: target %q does not exist", name)
		}
	}
	return nil
}
