package admin

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/version"
	"net/http"
	"time"
)

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	status := h.targetStatusesForSession(r, now)
	response := struct {
		Status    string               `json:"status"`
		CheckedAt time.Time            `json:"checked_at"`
		Targets   []proxy.TargetStatus `json:"targets"`
		Count     int                  `json:"target_count"`
		Muted     int                  `json:"muted_targets"`
	}{
		Status:    "ok",
		CheckedAt: now,
		Targets:   status,
		Count:     len(status),
	}

	for _, target := range status {
		if target.Muted {
			response.Muted++
		}
		if target.Muted && response.Status == "ok" {
			response.Status = "degraded"
		}
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) targetStatusesForSession(r *http.Request, now time.Time) []proxy.TargetStatus {
	statuses := h.proxyService.TargetStatuses(now)
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil || session.Role != RoleAdmin {
		for i := range statuses {
			statuses[i].Endpoint = readonlyTargetEndpoint(statuses[i].Endpoint)
		}
	}
	return statuses
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	metrics := h.proxyService.MetricsSnapshot()
	statuses := h.proxyService.TargetStatuses(now)
	uptime := now.Sub(metrics.StartTime)
	if uptime < 0 {
		uptime = 0
	}

	response := struct {
		GeneratedAt    time.Time `json:"generated_at"`
		UptimeSeconds  float64   `json:"uptime_seconds"`
		ActiveRequests int64     `json:"active_requests"`
		Requests       struct {
			Total                           int64 `json:"total"`
			Success                         int64 `json:"success"`
			Failures                        int64 `json:"failures"`
			UsageRecordFailures             int64 `json:"usage_record_failures"`
			UsageClientGoneNoUsage          int64 `json:"usage_client_gone_no_usage"`
			UsageUpstreamInterruptedNoUsage int64 `json:"usage_upstream_interrupted_no_usage"`
			RequestMembersNormalized        int64 `json:"request_members_normalized"`
			Retries                         int64 `json:"retries"`
			KeyRetries                      int64 `json:"key_retries"`
			TargetRetries                   int64 `json:"target_retries"`
			ClientCanceled                  int64 `json:"client_canceled"`
		} `json:"requests"`
		Targets        int                    `json:"targets"`
		TargetsRuntime []metricsTargetRuntime `json:"targets_runtime"`
		// Upstream Responses WebSocket pool counters and handshake phases.
		ResponsesWS proxy.ResponsesWSMetrics `json:"responses_ws"`
		// Asynchronous usage/statistics persistence backlog.
		Persistence proxy.PersistenceMetrics `json:"persistence"`
		// Shared decode budget of compressed-response usage observation.
		ObservationBudget proxy.ObservationBudgetMetrics `json:"observation_budget"`
		// 额度后台刷新状态：pending 为 true 表示写操作之后的刷新尚未完成。
		QuotaRefresh *quota.RefreshStatus `json:"quota_refresh,omitempty"`
	}{
		GeneratedAt:       now,
		UptimeSeconds:     uptime.Seconds(),
		ActiveRequests:    metrics.ActiveRequests,
		Targets:           len(statuses),
		ResponsesWS:       metrics.ResponsesWS,
		Persistence:       h.proxyService.PersistenceMetrics(),
		ObservationBudget: h.proxyService.ObservationBudgetMetrics(),
	}
	if h.quotaManager != nil {
		st := h.quotaManager.RefreshStatus()
		response.QuotaRefresh = &st
	}
	response.Requests.Total = metrics.TotalRequests
	response.Requests.Success = metrics.TotalSuccess
	response.Requests.Failures = metrics.TotalFailures
	response.Requests.UsageRecordFailures = metrics.TotalUsageRecordFailures
	response.Requests.UsageClientGoneNoUsage = metrics.TotalUsageClientGoneNoUsage
	response.Requests.UsageUpstreamInterruptedNoUsage = metrics.TotalUsageUpstreamInterruptedNoUsage
	response.Requests.RequestMembersNormalized = metrics.TotalRequestMembersNormalized
	response.Requests.Retries = metrics.TotalRetries
	response.Requests.KeyRetries = metrics.TotalKeyRetries
	response.Requests.TargetRetries = metrics.TotalTargetRetries
	response.Requests.ClientCanceled = metrics.TotalClientCanceled

	// S5 调度可观测（议题 #18）：每节点运行态含派生调度相位与亲和/熔断计数。
	for _, ts := range statuses {
		response.TargetsRuntime = append(response.TargetsRuntime, metricsTargetRuntime{
			Name:           ts.Name,
			InFlight:       ts.InFlight,
			MaxConcurrency: ts.Capacity,
			Utilization:    ts.Utilization,
			Paused:         ts.Paused,
			Phase:          ts.Phase,
			AffinityHits:   ts.AffinityHits,
			BreakerTrips:   ts.BreakerTrips,
			Window15m:      ts.Window15m,
			MutedUntil:     ts.MutedUntil,
		})
	}

	writeJSON(w, http.StatusOK, response)
}

// metricsTargetRuntime 是 /admin/metrics 中单节点的容量与调度运行态快照。
type metricsTargetRuntime struct {
	Window15m      proxy.RuntimeWindow `json:"window_15m"`
	Name           string              `json:"name"`
	InFlight       int64               `json:"in_flight"`
	MaxConcurrency int                 `json:"max_concurrency"`
	Utilization    float64             `json:"utilization"`
	Paused         bool                `json:"paused"`
	Phase          string              `json:"phase"` // ready / muted / half-open / offline（S5）
	AffinityHits   int64               `json:"affinity_hits"`
	BreakerTrips   int64               `json:"breaker_trips"`
	MutedUntil     time.Time           `json:"muted_until,omitempty"`
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	if session, ok := SessionFromContext(r.Context()); ok && session != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"username":      session.Username,
			"role":          session.Role,
			"expires_at":    session.ExpiresAt,
			"version":       version.Version,
		})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	health := h.targetStatusesForSession(r, now)
	metrics := h.proxyService.MetricsSnapshot()

	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}
	costs, err := h.modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}
	summary, err := h.usageStore.SummaryContext(r.Context(), now, h.getResolver())
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		h.writeInternalError(w, "failed to summarize usage", err)
		return
	}

	activeTargets := 0
	for _, t := range health {
		if !t.Muted && !t.Paused {
			activeTargets++
		}
	}

	// 72-hour request/success stats from usage events.
	from72h := now.Add(-72 * time.Hour)
	reqs72h, success72h, err := h.usageStore.Count(from72h, now)
	if err != nil {
		h.writeInternalError(w, "failed to count usage events", err)
		return
	}
	var successRate72h float64
	if reqs72h > 0 {
		successRate72h = float64(success72h) / float64(reqs72h) * 100
	}

	requestStats := h.requestStatsSnapshot(now)
	response := map[string]any{
		"legacy_sample_basis": "usage_events",
		"request_stats_72h":   requestStats,
		"generated_at":        now,
		"targets":             health,
		"target_count":        len(health),
		"active_targets":      activeTargets,
		"active_requests":     metrics.ActiveRequests,
		"total_requests":      metrics.TotalRequests,
		"success_rate":        successRate72h,
		"requests_72h":        reqs72h,
		"success_72h":         success72h,
		"client_count":        len(clients),
		"model_cost_count":    len(costs),
		"requests": map[string]int64{
			"total":                               metrics.TotalRequests,
			"success":                             metrics.TotalSuccess,
			"failures":                            metrics.TotalFailures,
			"usage_record_failures":               metrics.TotalUsageRecordFailures,
			"usage_client_gone_no_usage":          metrics.TotalUsageClientGoneNoUsage,
			"usage_upstream_interrupted_no_usage": metrics.TotalUsageUpstreamInterruptedNoUsage,
			"request_members_normalized":          metrics.TotalRequestMembersNormalized,
			"request_stats_record_failures":       h.proxyService.RequestStatsRecordFailures(),
			"retries":                             metrics.TotalRetries,
			"key_retries":                         metrics.TotalKeyRetries,
			"target_retries":                      metrics.TotalTargetRetries,
			"client_canceled":                     metrics.TotalClientCanceled,
		},
		"usage_summary": summary,
	}
	if events, err := h.listAuditEvents(10); err == nil {
		response["recent_audit"] = events
	}
	writeJSON(w, http.StatusOK, response)
}
