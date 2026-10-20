package admin

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"time"
)

func (h *Handler) requestStatsSnapshot(now time.Time) requeststats.Snapshot {
	stats := requeststats.EmptySnapshot(now)
	if h.requestStatsStore == nil || !h.proxyService.HasRequestStatsRecorder() {
		stats.Error = "request statistics unavailable"
	} else {
		value, err := h.requestStatsStore.Snapshot(now)
		if err != nil {
			stats.Error = "request statistics query failed"
			if h.logger != nil {
				h.logger.Error("request stats query failed", "error", err)
			}
		} else {
			stats = value
		}
	}
	stats.RecordFailures = h.proxyService.RequestStatsRecordFailures()
	return stats
}
