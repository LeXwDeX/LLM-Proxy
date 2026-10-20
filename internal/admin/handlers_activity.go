package admin

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type activityTotals struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	IncompleteUsageCount int64 `json:"incomplete_usage_count"`
}

type activitySeries struct {
	From               time.Time `json:"from"`
	To                 time.Time `json:"to"`
	Partial            bool      `json:"partial"`
	ActiveRequestsPeak *int      `json:"active_requests_peak"`
	activityTotals
}

type activityClient struct {
	Name              string           `json:"name"`
	Configured        bool             `json:"configured"`
	ActiveRequests    int              `json:"active_requests"`
	WebsocketSessions int              `json:"websocket_sessions"`
	Totals            activityTotals   `json:"totals"`
	Series            []activitySeries `json:"series"`
}

type activityResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	Window      struct {
		From        time.Time `json:"from"`
		To          time.Time `json:"to"`
		StepSeconds int       `json:"step_seconds"`
	} `json:"window"`
	Runtime struct {
		Since             time.Time `json:"since"`
		Complete          bool      `json:"complete"`
		ActiveRequests    int       `json:"active_requests"`
		ActiveClients     int       `json:"active_clients"`
		WebsocketSessions int       `json:"websocket_sessions"`
	} `json:"runtime"`
	Totals  activityTotals   `json:"totals"`
	Series  []activitySeries `json:"series"`
	Clients []activityClient `json:"clients"`
}

func activityTokenTotals(t usage.Totals) activityTotals {
	input := t.InputTokens + t.CachedTokens + t.CacheCreationTokens + t.ToolUsePromptTokens
	return activityTotals{InputTokens: input, OutputTokens: t.OutputTokens, TotalTokens: input + t.OutputTokens, IncompleteUsageCount: t.IncompleteUsageCount}
}

func buildActivitySeries(runtime proxy.ActivitySnapshot, peaks []*int, totals []usage.Totals) []activitySeries {
	series := make([]activitySeries, len(runtime.Intervals))
	for i, interval := range runtime.Intervals {
		series[i] = activitySeries{From: interval.From, To: interval.To, Partial: interval.Partial}
		if i < len(peaks) {
			series[i].ActiveRequestsPeak = peaks[i]
		}
		if i < len(totals) {
			series[i].activityTotals = activityTokenTotals(totals[i])
		}
	}
	return series
}

func (h *Handler) handleActivity(w http.ResponseWriter, r *http.Request) {
	runtime := h.proxyService.ActivitySnapshot()
	now := runtime.To
	ws := h.proxyService.WebsocketActivitySnapshot()
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list activity clients", err)
		return
	}
	recent, err := h.usageStore.RecentActivity(r.Context(), runtime.From, runtime.To, proxy.ActivityStep)
	if err != nil {
		h.writeInternalError(w, "failed to read recent activity usage", err)
		return
	}
	response := activityResponse{GeneratedAt: now, Totals: activityTokenTotals(recent.Totals), Series: buildActivitySeries(runtime, runtime.Peaks, recent.Series), Clients: make([]activityClient, 0)}
	response.Window.From, response.Window.To, response.Window.StepSeconds = runtime.From, runtime.To, int(proxy.ActivityStep/time.Second)
	response.Runtime.Since, response.Runtime.Complete = runtime.Since, runtime.Complete
	response.Runtime.ActiveRequests, response.Runtime.ActiveClients = runtime.ActiveRequests, runtime.ActiveClients
	names := make(map[string]string)
	configured := make(map[string]bool)
	for key, c := range runtime.Clients {
		names[key] = c.Name
	}
	for key, c := range recent.Clients {
		names[key] = c.Name
	}
	for key, c := range ws {
		names[key] = c.Name
		response.Runtime.WebsocketSessions += c.Sessions
	}
	for _, c := range clients {
		key := strings.ToLower(strings.TrimSpace(c.Name))
		names[key] = c.Name
		configured[key] = true
	}
	keys := make([]string, 0, len(names))
	for key := range names {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		live := runtime.Clients[key]
		peaks := live.Peaks
		if peaks == nil {
			// Known idle clients have zero peaks only where runtime history is known.
			peaks = make([]*int, len(runtime.Peaks))
			for i, p := range runtime.Peaks {
				if p != nil {
					v := 0
					peaks[i] = &v
				}
			}
		}
		var tokens nosql.ClientActivityUsage
		if u := recent.Clients[key]; u != nil {
			tokens = *u
		}
		response.Clients = append(response.Clients, activityClient{Name: names[key], Configured: configured[key], ActiveRequests: live.ActiveRequests, WebsocketSessions: ws[key].Sessions, Totals: activityTokenTotals(tokens.Totals), Series: buildActivitySeries(runtime, peaks, tokens.Series)})
	}
	writeJSON(w, http.StatusOK, response)
}
