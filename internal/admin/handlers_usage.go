package admin

import (
	"errors"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) handleListUsageEvents(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	events, err := h.usageStore.List(filter)
	if err != nil {
		h.writeInternalError(w, "failed to list usage events", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

func (h *Handler) handleAggregateUsage(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	var result usage.AggregateResult
	if len(filter.ModelGroups) > 0 {
		result, err = h.usageStore.AggregateByModelGroups(filter, r.URL.Query().Get("group_by"), h.getResolver(), h.targetStore, h.modelGroupStore)
	} else {
		result, err = h.usageStore.AggregateByModelGroupsAllRequests(filter, r.URL.Query().Get("group_by"), h.getResolver(), h.targetStore, h.modelGroupStore)
	}
	if err != nil {
		h.writeInternalError(w, "failed to aggregate usage", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleUsageModelGroups(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	result, err := h.usageStore.AggregateModelGroups(filter, h.getResolver(), h.targetStore, h.modelGroupStore)
	if err != nil {
		h.writeInternalError(w, "failed to aggregate usage model groups", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	result, err := h.usageStore.Summary(time.Now().UTC(), h.getResolver())
	if err != nil {
		h.writeInternalError(w, "failed to summarize usage", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func parseUsageFilter(r *http.Request, withLimit bool) (usage.Filter, error) {
	query := r.URL.Query()

	from, err := parseTimeValue(query.Get("from"))
	if err != nil {
		return usage.Filter{}, errors.New("invalid from, expect RFC3339")
	}
	to, err := parseTimeValue(query.Get("to"))
	if err != nil {
		return usage.Filter{}, errors.New("invalid to, expect RFC3339")
	}

	filter := usage.Filter{
		From:       from,
		To:         to,
		ClientName: strings.TrimSpace(query.Get("client_name")),
		Model:      strings.TrimSpace(query.Get("model")),
	}
	for _, raw := range query["model_group"] {
		if name := strings.TrimSpace(raw); name != "" {
			filter.ModelGroups = append(filter.ModelGroups, name)
		}
	}

	if withLimit {
		limit := 200
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				return usage.Filter{}, errors.New("invalid limit")
			}
			limit = parsed
		}
		filter.Limit = limit
	}

	return filter, nil
}

func parseTimeValue(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	t = t.UTC()
	return &t, nil
}
