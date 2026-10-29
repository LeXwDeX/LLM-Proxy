package admin

import (
	"github.com/go-chi/chi/v5"
)

// routes builds the admin router. The readonly guard binds to the current
// session manager, so tests that replace it rebuild the router.
func (h *Handler) routes() chi.Router {
	// readonlyGuard is applied to write sub-routers under /data and /config
	// to block readonly-role sessions from mutating operations.
	readonlyGuard := h.sessionManager.ReadonlyGuard()

	r := chi.NewRouter()
	r.Get("/", h.handleUI)
	r.Get("/ui", h.handleUI)
	r.Get("/ui/js/*", h.handleUIJS)
	r.Get("/healthz", h.handleHealthz)
	r.Get("/metrics", h.handleMetrics)
	r.Get("/api/me", h.handleMe)
	r.Get("/api/overview", h.cachedReport(h.handleOverview))
	r.Get("/api/activity", h.handleActivity)
	// change-password is allowed for all authenticated users (including readonly)
	r.Post("/api/change-password", h.handleChangePassword)

	r.Route("/data", func(r chi.Router) {
		// === Read-only routes (accessible to all authenticated users, including readonly) ===
		r.Get("/clients", h.handleListClients)
		r.Get("/model-costs", h.handleListModelCosts)
		r.Get("/pricing-templates", h.handleListPricingTemplates)
		r.Get("/usage/events", h.handleListUsageEvents)
		r.Get("/usage/unresolved", h.handleListUsageUnresolved)
		r.Get("/usage/aggregate", h.cachedReport(h.handleAggregateUsage))
		r.Get("/usage/summary", h.handleUsageSummary)
		r.Get("/usage/model-groups", h.handleUsageModelGroups)
		r.Get("/audit", h.handleListAudit)

		// Target management (read)
		r.Get("/targets", h.handleListTargets)

		// Deployment pool management (read)
		r.Get("/pools", h.handleListPools)

		// Model catalog (read)
		r.Get("/catalog", h.handleListCatalog)
		r.Get("/catalog/{endpoint_type}", h.handleListCatalogByType)

		// Quota status
		r.Get("/quota/summary", h.handleQuotaSummary)
		r.Get("/quota/{client}", h.handleQuotaStatus)
		r.Get("/model-groups", h.handleListModelGroups)

		// 职级（结构化：日/周/月系数，额度体系 v2）与项目组字典
		r.Get("/job-levels", h.handleListJobLevels)
		r.Get("/project-groups", h.handleListDict(dictProjectGroups))

		// Endpoint type metadata（单一信息源，UI 下拉/徽章数据）
		r.Get("/endpoint-types", h.handleListEndpointTypes)
		r.Get("/schema", h.handleSchema)

		// Trace store (DEBUG mode)
		r.Get("/trace", h.handleListTrace)
		r.Get("/trace/stats", h.handleTraceStats)
		r.Get("/trace/{request_id}", h.handleGetTrace)

		// === Write routes (blocked for readonly users via ReadonlyGuard) ===
		guarded := r.With(readonlyGuard)
		r.With(h.auditReadonlyUsageRecheck, readonlyGuard).Post("/usage/recheck", h.handleUsageRecheck)

		// Client write
		guarded.Post("/clients", h.handleCreateClient)
		guarded.Put("/clients/{name}", h.handleUpdateClient)
		guarded.Delete("/clients/{name}", h.handleDeleteClient)
		guarded.Post("/quota/{client}/sync", h.handleSyncQuota)
		guarded.Put("/quota/{client}", h.handleUpdateQuota)
		guarded.Delete("/quota/{client}", h.handleDeleteQuotaEntry)
		guarded.Post("/model-groups", h.handleCreateModelGroup)
		guarded.Put("/model-groups/{name}", h.handleUpdateModelGroup)
		guarded.Delete("/model-groups/{name}", h.handleDeleteModelGroup)

		// 职级（结构化 CRUD）与项目组字典
		guarded.Post("/job-levels", h.handleCreateJobLevel)
		guarded.Put("/job-levels/{name}", h.handleUpdateJobLevel)
		guarded.Delete("/job-levels/{name}", h.handleDeleteJobLevel)
		guarded.Post("/project-groups", h.handleDictCreate(dictProjectGroups))
		guarded.Put("/project-groups/{name}", h.handleDictUpdate(dictProjectGroups))
		guarded.Delete("/project-groups/{name}", h.handleDictDelete(dictProjectGroups))

		// Model cost write（compat facade over template-0 pricing templates）
		guarded.Put("/model-costs/{model}", h.handleUpsertModelCost)
		guarded.Delete("/model-costs/{model}", h.handleDeleteModelCost)

		// Pricing template write
		guarded.Post("/pricing-templates", h.handleCreatePricingTemplate)
		guarded.Put("/pricing-templates/{id}", h.handleUpdatePricingTemplate)
		guarded.Delete("/pricing-templates/{id}", h.handleDeletePricingTemplate)

		// Target write
		guarded.Post("/targets", h.handleCreateTarget)
		guarded.Put("/targets/{name}", h.handleUpdateTarget)
		guarded.Delete("/targets/{name}", h.handleDeleteTarget)
		guarded.Put("/targets/{name}/pause", h.handlePauseTarget)
		guarded.Put("/targets/{name}/resume", h.handleResumeTarget)
		guarded.Post("/targets/{name}/test-model", h.handleTestModel)
		guarded.Put("/targets/{name}/keys/{index}/block", h.handleBlockTargetKey)
		guarded.Put("/targets/{name}/keys/{index}/unblock", h.handleUnblockTargetKey)
		guarded.Put("/targets/{name}/keys/id/{key_id}/block", h.handleBlockTargetKeyByID)
		guarded.Put("/targets/{name}/keys/id/{key_id}/unblock", h.handleUnblockTargetKeyByID)
		guarded.Post("/targets/{name}/keys/wakeup", h.handleWakeUpTargetKeys)

		// Pool write
		guarded.Post("/pools", h.handleCreatePool)
		guarded.Put("/pools/{name}", h.handleUpdatePool)
		guarded.Delete("/pools/{name}", h.handleDeletePool)
		guarded.Put("/pools/{name}/pause", h.handlePausePool)
		guarded.Put("/pools/{name}/resume", h.handleResumePool)

		// === User management (admin-only, always blocked for readonly) ===
		guarded.Get("/users", h.handleListUsers)
		guarded.Post("/users", h.handleCreateUser)
		guarded.Put("/users/{username}", h.handleUpdateUser)
		guarded.Delete("/users/{username}", h.handleDeleteUser)
	})

	// Config reload: blocked for readonly users
	r.With(readonlyGuard).Post("/config/reload", h.handleReloadConfig)

	return r
}
