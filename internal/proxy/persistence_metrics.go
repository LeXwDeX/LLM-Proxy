package proxy

// PersistenceMetrics describes the asynchronous persistence backlog behind
// the configured usage and request statistics recorders. Zero values mean
// the recorder is synchronous or does not report the counter.
type PersistenceMetrics struct {
	// UsageAsyncPending: usage events synced to the recovery journal that
	// the background writer has not committed to the database yet.
	UsageAsyncPending int64 `json:"usage_async_pending"`
	// UsageAsyncDeferred: acknowledged usage events that found the commit
	// queue full and wait for the periodic journal replay (cumulative).
	UsageAsyncDeferred int64 `json:"usage_async_deferred"`
	// RequestStatsDropped: completions dropped by a full statistics queue
	// (cumulative; also included in request_stats_record_failures).
	RequestStatsDropped int64 `json:"request_stats_dropped"`
}

// PersistenceMetrics reports the current persistence backlog counters.
func (s *Service) PersistenceMetrics() PersistenceMetrics {
	var metrics PersistenceMetrics
	if journal, ok := s.currentUsageRecorder().(interface {
		AsyncPending() int64
		AsyncDeferred() int64
	}); ok {
		metrics.UsageAsyncPending = journal.AsyncPending()
		metrics.UsageAsyncDeferred = journal.AsyncDeferred()
	}
	if stats, ok := s.currentRequestStatsRecorder().(interface{ Dropped() int64 }); ok {
		metrics.RequestStatsDropped = stats.Dropped()
	}
	return metrics
}
