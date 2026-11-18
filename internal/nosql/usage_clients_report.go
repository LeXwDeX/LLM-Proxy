package nosql

import (
	"context"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// AggregateClientTotalsContext returns the per-client totals the clients page
// needs. When every configured price is hour-aligned it reads only the hourly
// quota rollups (bucket quota_usage_agg_hourly) and never opens the raw usage
// events bucket; the request count uses the full Requests field, matching the
// no-filter usage aggregate's by_client.
//
// The lightweight path is deliberately narrower than the general aggregate: it
// validates the aggregate-source markers and every scanned rollup cell, but it
// does not run the history-integrity walks (unresolved usage, receipts, raw
// event parity). A damaged event bucket therefore cannot turn this display-only
// report into an error; billing, quota admission, export and audit must keep
// using the strict readers. Any pricing shape that needs event timestamps
// (conditions, sub-hour boundaries, non-hour timezones, half-hour periods) or a
// model-group filter falls back to the full read and reports exact=false,
// source="events" instead of silently rounding to the hour.
func (s *UsageStore) AggregateClientTotalsContext(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, targets *TargetStore, globalGroups ...*ModelGroupStore) (usage.ClientUsageSummary, error) {
	return s.aggregateClientTotalsContext(ctx, filter, resolver, targets, costReadHooks{}, globalGroups...)
}

// aggregateClientTotalsContext is the test/benchmark variant with cost-read
// hooks; the light path never invokes them, which is asserted by tests.
func (s *UsageStore) aggregateClientTotalsContext(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, targets *TargetStore, hooks costReadHooks, globalGroups ...*ModelGroupStore) (usage.ClientUsageSummary, error) {
	result := usage.ClientUsageSummary{Source: usage.ClientUsageSourceAggregate}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}
	fallback := func(successRequests bool) (usage.ClientUsageSummary, error) {
		data, err := s.aggregateTargetModelCellsReadContext(ctx, filter, "day", resolver, targets, successRequests, hooks, globalGroups...)
		if err != nil {
			return usage.ClientUsageSummary{}, err
		}
		result.Clients = data.ByClient
		result.Exact = false
		result.Source = usage.ClientUsageSourceEvents
		return result, nil
	}
	if usage.RequiresEventTimestamps(resolver) {
		// Event timestamps and conditions are needed for an exact price; keep
		// the aggregate endpoint's request-count contract for group filters.
		return fallback(len(filter.ModelGroups) > 0)
	}
	if len(filter.ModelGroups) > 0 {
		// Group ownership is resolved by the full path; excluding it here
		// would silently report unfiltered traffic.
		return fallback(true)
	}
	if s == nil || s.db == nil {
		return usage.ClientUsageSummary{}, fmt.Errorf("usage source status: store is not initialized")
	}
	if err := ctx.Err(); err != nil {
		// Match the strict readers: a canceled request produces no result, so
		// the handler can skip writing (Issue #97).
		return usage.ClientUsageSummary{}, err
	}
	from, to := costBounds(filter)
	needsEvents := false
	err := s.db.View(func(tx *bolt.Tx) error {
		missing, err := missingUsageAggregateSources(tx)
		if err != nil {
			return err
		}
		for _, source := range missing {
			if source == BucketQuotaUsageAggHourly {
				return fmt.Errorf("%w: %w: %s", ErrUsageHistoryIncomplete, ErrUsageAggregateSourceMissing, source)
			}
		}
		if to.Truncate(time.Hour).Before(from.Truncate(time.Hour)) {
			return ctx.Err()
		}
		// Same window semantics as the reports: from is floored to its hour and
		// the hour containing to is included whole.
		cells, err := readCostRollups(ctx, tx, filter, from, to, nil)
		if err != nil {
			return err
		}
		byClient := make(map[string]*usage.DimensionTotals)
		for _, cell := range cells {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Defensive: a resolver may require event timestamps only for some
			// (target, model) pairs; never round those to the hour.
			if usage.RequiresEventTimestampsFor(resolver, cell.Target, cell.Model) {
				needsEvents = true
				return nil
			}
			cost, missingPrice := aggCellCost(cell.AggCell, cell.Target, cell.Hour, cell.Model, resolver)
			cell.AggCell.IncompleteCostCount += missingPrice
			client := cell.Client
			if client == "" {
				client = "unknown"
			}
			if byClient[client] == nil {
				byClient[client] = &usage.DimensionTotals{Key: client}
			}
			addCellToTotals(&byClient[client].Totals, cell.AggCell, cost)
		}
		result.Clients = usage.SortedDimensions(byClient)
		return nil
	})
	if err != nil {
		return usage.ClientUsageSummary{}, err
	}
	if needsEvents {
		return fallback(false)
	}
	result.Exact = true
	return result, nil
}
