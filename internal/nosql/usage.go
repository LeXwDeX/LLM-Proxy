package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// UsageStore manages usage events backed by bbolt.
// It implements the usage.Recorder interface.
//
// Raw detail, two hourly rollups, and a derived client/time index are involved:
//   - usage_events     : raw event detail (RFC3339Nano + uuid key)  → for List()
//   - usage_agg_hourly : pre-aggregated hourly rollups               → for Summary/Aggregate/Count
//
// Record() writes all of them in the same write tx so they stay consistent.
type UsageStore struct {
	db *bolt.DB
}

// NewUsageStore creates a new bbolt-backed usage store.
func NewUsageStore(db *bolt.DB) *UsageStore {
	return &UsageStore{db: db}
}

// Record appends one usage event AND bumps the hourly aggregation cell atomically.
func (s *UsageStore) Record(event usage.Event) error {
	_, err := s.RecordOnce(event)
	return err
}

// RecordOnce reports the insertion outcome from the same transaction that
// writes the entire bundle, so callers can suppress duplicate live charges.
func (s *UsageStore) RecordOnce(event usage.Event) (bool, error) {
	rows, keys, err := normalizeUsageBundle(event)
	if err != nil {
		return false, err
	}
	// P8：用 db.Batch 而非 db.Update —— bbolt 写事务全库串行且每次 commit 触发
	// fsync，高并发下每请求一次同步 Update 会在全部成功请求上形成吞吐天花板。
	// Batch 将并发调用合并为单次 commit（≤MaxBatchDelay/≤1000 条，见 OpenDB）。
	// 生产请求路径不再同步等待这里：UsageJournal 异步模式先同步追加恢复日志，
	// 再由后台写入器调用 RecordBatch；本方法服务于回放与同步回退路径。
	// 安全性：合并事务失败时整体回滚后逐个重试，bumpAgg 的 RMW 基于已提交
	// 状态，不会双计。
	added := false
	err = s.db.Batch(func(tx *bolt.Tx) error {
		// bbolt may retry this callback after a different batch member fails.
		added = tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(keys[0])) == nil
		for i := range rows {
			if err := recordUsageTx(tx, rows[i], keys[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return added && err == nil, err
}

// RecordBatch commits several usage events in one write transaction. It is
// all-or-nothing: any rejected event rolls the whole batch back so the caller
// can retry the events individually. Identical replays are idempotent by
// RecordID, exactly like Record.
func (s *UsageStore) RecordBatch(events []usage.Event) error {
	if len(events) == 0 {
		return nil
	}
	var normalized []usage.Event
	var keys []string
	for _, event := range events {
		rows, rowKeys, err := normalizeUsageBundle(event)
		if err != nil {
			return err
		}
		normalized = append(normalized, rows...)
		keys = append(keys, rowKeys...)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for i := range normalized {
			if err := recordUsageTx(tx, normalized[i], keys[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// normalizeUsageBundle keeps root metadata intact and expands every additional
// row before opening a write transaction. A rejected component rejects its root.
func normalizeUsageBundle(event usage.Event) ([]usage.Event, []string, error) {
	event = usage.CloneEvent(event)
	key, err := normalizeUsageEvent(&event)
	if err != nil {
		return nil, nil, err
	}
	if event.Supplementary && len(event.Components) > 0 {
		return nil, nil, fmt.Errorf("supplementary usage cannot contain components")
	}
	seen := make(map[int]struct{}, len(event.Components))
	for _, component := range event.Components {
		if component.IterationIndex < 0 {
			return nil, nil, fmt.Errorf("invalid usage iteration index")
		}
		if _, exists := seen[component.IterationIndex]; exists {
			return nil, nil, fmt.Errorf("duplicate usage iteration index %d", component.IterationIndex)
		}
		seen[component.IterationIndex] = struct{}{}
	}
	rows := usage.ExpandEvent(event)
	keys := make([]string, len(rows))
	keys[0] = key
	for i := 1; i < len(rows); i++ {
		keys[i], err = normalizeUsageEvent(&rows[i])
		if err != nil {
			return nil, nil, err
		}
	}
	return rows, keys, nil
}

// RecordExists verifies a supplied stable identity against its complete stored
// payload. It is used only by explicit journal retries, not ordinary requests.
func (s *UsageStore) RecordExists(event usage.Event) (bool, error) {
	rows, keys, err := normalizeUsageBundle(event)
	if err != nil {
		return false, err
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		return false, err
	}
	exists := false
	err = s.db.View(func(tx *bolt.Tx) error {
		if quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine)); quarantine != nil && quarantine.Get([]byte(keys[0])) != nil {
			return fmt.Errorf("%w: usage record is quarantined pending reconciliation", ErrUsageClientIndexIncomplete)
		}
		stored := tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(keys[0]))
		if stored == nil {
			return nil
		}
		if string(stored) != string(data) {
			return fmt.Errorf("usage record ID %q already exists with different event data", event.RecordID)
		}
		exists = true
		return nil
	})
	return exists, err
}

func normalizeUsageEvent(event *usage.Event) (string, error) {
	usage.NormalizeImagePartitions(event)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.Timestamp = event.Timestamp.UTC()
	event.RecordID = strings.TrimSpace(event.RecordID)
	if event.RecordID == "" {
		event.RecordID = uuid.NewString()
	} else {
		recordID, err := uuid.Parse(event.RecordID)
		if err != nil {
			return "", fmt.Errorf("invalid usage record ID: %w", err)
		}
		event.RecordID = recordID.String()
	}
	event.ClientName = strings.TrimSpace(event.ClientName)
	event.EndpointType = strings.ToLower(strings.TrimSpace(event.EndpointType))
	event.Model = strings.ToLower(strings.TrimSpace(event.Model))
	event.RequestID = strings.TrimSpace(event.RequestID)
	event.Target = strings.TrimSpace(event.Target)
	event.Path = strings.TrimSpace(event.Path)
	return usageKey(event.Timestamp, event.RecordID), nil
}

func recordUsageTx(tx *bolt.Tx, event usage.Event, key string) error {
	if quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine)); quarantine != nil && quarantine.Get([]byte(key)) != nil {
		// The original write may already be represented in permanent
		// rollups. A journal replay cannot resurrect and count it twice.
		return fmt.Errorf("%w: usage record is quarantined pending reconciliation", ErrUsageClientIndexIncomplete)
	}
	// 1. Append raw detail.
	b := tx.Bucket([]byte(BucketUsageEvents))
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal usage event: %w", err)
	}
	if existing := b.Get([]byte(key)); existing != nil {
		if string(existing) != string(data) {
			return fmt.Errorf("usage record ID %q already exists with different event data", event.RecordID)
		}
		return addClientEventIndex(tx, event, []byte(key), data)
	}
	if err := b.Put([]byte(key), data); err != nil {
		return err
	}
	// 2. Bump both general and quota-specific hourly aggregation cells.
	if err := bumpAgg(tx, event); err != nil {
		return err
	}
	if err := bumpQuotaAgg(tx, event); err != nil {
		return err
	}
	if err := addClientEventIndex(tx, event, []byte(key), data); err != nil {
		return err
	}
	return bumpRawRevision(tx)
}

// List returns events matching the filter, sorted by timestamp descending.
//
// Scans complete seconds so historical RFC3339Nano keys retain correct ordering.
func (s *UsageStore) List(filter usage.Filter) ([]usage.Event, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}

	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	events := make([]usage.Event, 0, limit)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		events = scanRecentEvents(b, limit, filter.From, filter.To,
			func(evt usage.Event) time.Time { return evt.Timestamp },
			func(evt usage.Event) bool { return usageFilterMatch(evt, filter, clientKey, modelKey) })
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Sorted by actual timestamp, including legacy fractional-second keys.
	return events, nil
}

// Count returns total and success request counts within the given time range.
//
// New implementation: scans the hourly agg bucket — bounded to ~hours-in-range cells
// times unique (et,client,model) combos per hour. For 72h window this is typically
// < 1000 cells vs. previously scanning every event in that window.
func (s *UsageStore) Count(from, to time.Time) (total, success int64, err error) {
	from = from.UTC()
	to = to.UTC()

	err = s.db.View(func(tx *bolt.Tx) error {
		return aggIter(tx, from, to, func(_ time.Time, _, _, _, _ string, cell AggCell) error {
			total += cell.Requests
			success += cell.Success
			return nil
		})
	})
	return
}

// Aggregate aggregates usage in requested time buckets, served from the hourly agg bucket.
//
// For group_by=hour: each agg cell maps 1:1 to a Bucket entry (further accumulated by hour).
// For group_by=day:  hour cells are accumulated into the corresponding day bucket.
// by_client / by_model totals are accumulated in the same pass.
//
// Filter semantics: ClientName / Model filters apply at the agg cell level. From/To
// are honored as inclusive hour-bucket bounds (an event whose hour falls within the
// range counts; partial-hour exclusion is approximate to ±1h, acceptable for the UI).
func (s *UsageStore) Aggregate(filter usage.Filter, groupBy string, costs usage.RateResolver) (usage.AggregateResult, error) {
	if usage.RequiresEventTimestamps(costs) {
		return s.aggregateTargetModelCells(filter, groupBy, costs, nil, false)
	}
	groupBy = usageNormalizeGroupBy(groupBy)

	result := usage.AggregateResult{GroupBy: groupBy}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}

	from := time.Time{}
	if filter.From != nil {
		from = filter.From.UTC()
	}
	to := time.Now().UTC().Add(time.Hour)
	if filter.To != nil {
		to = filter.To.UTC()
	}

	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	bucketMap := map[time.Time]*usage.Bucket{}
	byClient := map[string]*usage.DimensionTotals{}
	byModel := map[string]*usage.DimensionTotals{}

	err := s.db.View(func(tx *bolt.Tx) error {
		return aggIter(tx, from, to, func(hour time.Time, et, client, model, target string, cell AggCell) error {
			// Apply dimension filters.
			if clientKey != "" && client != clientKey {
				return nil
			}
			if modelKey != "" && model != modelKey {
				return nil
			}

			cost := aggCellCost(cell, "", hour, model, costs)
			addCellToTotals(&result.Totals, cell, cost)

			bucketStart := bucketStartFromHour(hour, groupBy)
			bkt, ok := bucketMap[bucketStart]
			if !ok {
				bkt = &usage.Bucket{
					BucketStart: bucketStart,
					BucketEnd:   bucketStart.Add(usageStepDuration(groupBy)),
				}
				bucketMap[bucketStart] = bkt
			}
			addCellToTotals(&bkt.Totals, cell, cost)

			ck := client
			if ck == "" {
				ck = "unknown"
			}
			cd := byClient[ck]
			if cd == nil {
				cd = &usage.DimensionTotals{Key: ck}
				byClient[ck] = cd
			}
			addCellToTotals(&cd.Totals, cell, cost)

			mk := model
			if mk == "" {
				mk = "unknown"
			}
			md := byModel[mk]
			if md == nil {
				md = &usage.DimensionTotals{Key: mk}
				byModel[mk] = md
			}
			addCellToTotals(&md.Totals, cell, cost)
			return nil
		})
	})
	if err != nil {
		return usage.AggregateResult{}, err
	}

	bucketKeys := make([]time.Time, 0, len(bucketMap))
	for key := range bucketMap {
		bucketKeys = append(bucketKeys, key)
	}
	sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i].Before(bucketKeys[j]) })
	for _, key := range bucketKeys {
		result.Buckets = append(result.Buckets, *bucketMap[key])
	}

	result.ByClient = usageDimsByCostDesc(byClient)
	result.ByModel = usageSortedDimensions(byModel)
	return result, nil
}

// Summary returns predefined window totals for the UI.
//
// Implementation:
//   - last_hour:  scans raw usage_events for the past 1h (small volume, exact precision).
//   - today/yesterday/last_7d/last_30d: keep hourly membership, but use CostUnits
//     to resolve sub-hour prices at event time with raw/rollup parity checks.
//
// Why split: agg cells are aligned to whole hours, so a window whose start is not on
// an hour boundary (e.g. now-1h) cannot be answered exactly from agg without ±1h drift.
// last_hour is the only such window; resolving it from raw events keeps semantics exact
// Stable prices need only last-hour raw detail. Sub-hour prices share one raw
// scan across all windows; incomplete/pruned exact-price history is an error.
func (s *UsageStore) Summary(now time.Time, costs usage.RateResolver) (usage.SummaryResult, error) {
	return s.summaryRead(now, costs, costReadHooks{})
}

func (s *UsageStore) summaryRead(now time.Time, costs usage.RateResolver, hooks costReadHooks) (usage.SummaryResult, error) {
	now = now.UTC()
	result := usage.SummaryResult{GeneratedAt: now}

	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lastHourStart := now.Add(-1 * time.Hour)
	yesterdayDay := now.AddDate(0, 0, -1)
	yStart := time.Date(yesterdayDay.Year(), yesterdayDay.Month(), yesterdayDay.Day(), 0, 0, 0, 0, time.UTC)
	yEnd := yStart.Add(24 * time.Hour)
	last7Start := now.AddDate(0, 0, -7)
	last30Start := now.AddDate(0, 0, -30)

	// Raw last-hour detail and exact-price history share one scan and snapshot.
	data, err := s.readCostUnits(context.Background(), usage.Filter{From: &last30Start, To: &now}, costs, false, &lastHourStart, hooks)
	if err != nil {
		return usage.SummaryResult{}, err
	}
	for _, event := range data.raw {
		addEventToTotals(&result.LastHour, event, usageEstimateEventCost(event, costs))
	}
	for _, cell := range data.units {
		hour := cell.Hour
		cost := cell.CostUSD
		if cell.EventPriced && !cell.At.Before(lastHourStart) && !cell.At.After(now) {
			addCellToTotals(&result.LastHour, cell.AggCell, cost)
		}
		if !hour.Before(last30Start) && !hour.After(now) {
			addCellToTotals(&result.Last30Days, cell.AggCell, cost)
			if !hour.Before(last7Start) {
				addCellToTotals(&result.Last7Days, cell.AggCell, cost)
			}
			if !hour.Before(todayStart) {
				addCellToTotals(&result.Today, cell.AggCell, cost)
			}
			if !hour.Before(yStart) && hour.Before(yEnd) {
				addCellToTotals(&result.Yesterday, cell.AggCell, cost)
			}
		}
	}

	return result, nil
}

// PruneEventsBefore deletes usage_events strictly older than cutoff, in bounded
// per-transaction batches so the global bbolt write lock is never held long.
// Returns the number of deleted events. The hourly agg bucket is intentionally
// kept (tiny cells, long-term stats). Stable-price reads remain available;
// exact sub-hour pricing fails explicitly if its required raw history was pruned.
// Undecodable records are moved losslessly to quarantine instead of blocking
// subsequent batches. Their unknown ownership invalidates indexed reads globally.
func (s *UsageStore) PruneEventsBefore(cutoff time.Time, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 5000
	}
	// 截止前缀回退 1s，与 List 同理由（RFC3339Nano 同秒内序不稳）：宁可少删
	// 一秒，绝不误删 cutoff 之后的事件。
	cutoffPrefix := cutoff.UTC().Add(-time.Second).Format(time.RFC3339Nano)
	total := 0
	for {
		deleted := 0
		quarantined := 0
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketUsageEvents))
			if b == nil {
				return nil
			}
			// 两段式：先收集再删除，回避 cursor.Delete+Next 迭代的历史边界问题。
			keys := make([][]byte, 0, batchSize)
			c := b.Cursor()
			for k, _ := c.First(); k != nil && len(keys) < batchSize; k, _ = c.Next() {
				if string(k) >= cutoffPrefix {
					break
				}
				keys = append(keys, append([]byte(nil), k...))
			}
			for _, k := range keys {
				data := b.Get(k)
				var event usage.Event
				if json.Unmarshal(data, &event) != nil {
					if err := quarantineUsageEvent(tx, k, data); err != nil {
						return err
					}
					quarantined++
				} else {
					if err := removeClientEventIndex(tx, k, data); err != nil {
						return err
					}
				}
				if err := b.Delete(k); err != nil {
					return err
				}
				if err := bumpRawRevision(tx); err != nil {
					return err
				}
			}
			deleted = len(keys)
			return nil
		})
		if err != nil {
			return total, err
		}
		if quarantined > 0 {
			// Only log after the transaction commits, and never log raw bytes,
			// record keys or a decoder error that could include event content.
			slog.Warn("usage retention quarantined corrupt events; indexed history unavailable until evidence is reconciled", "quarantined", quarantined)
		}
		total += deleted
		if deleted < batchSize {
			return total, nil
		}
	}
}

// addEventToTotals adds one raw event into a Totals accumulator (used by last_hour scan).
func addEventToTotals(target *usage.Totals, evt usage.Event, cost float64) {
	target.Requests += usage.RequestWeight(evt)
	if evt.UsageStatus == usage.UsageStatusIncomplete || evt.UsageStatus == usage.UsageStatusConflict {
		target.IncompleteUsageCount++
	}
	if evt.CostIncomplete {
		target.IncompleteCostCount++
	}
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.CacheCreationTokens += evt.CacheCreationTokens
	target.ExplicitCacheReadTokens += evt.ExplicitCacheReadTokens
	target.ReasoningTokens += evt.ReasoningTokens
	target.CacheCreation5mTokens += evt.CacheCreation5mTokens
	target.CacheCreation1hTokens += evt.CacheCreation1hTokens
	target.ToolUsePromptTokens += evt.ToolUsePromptTokens
	target.ImageInputTokens += evt.ImageInputTokens
	target.CachedImageInputTokens += evt.CachedImageInputTokens
	target.ImageOutputTokens += evt.ImageOutputTokens
	target.EstimatedCost += cost
}

// usageEstimateEventCost computes cost for a single raw event.
func usageEstimateEventCost(evt usage.Event, resolver usage.RateResolver) float64 {
	return usage.EstimateEventCost(evt, resolver)
}

// --- Internal helpers ---

func usageKey(t time.Time, id string) string {
	return t.UTC().Format(time.RFC3339Nano) + "_" + id
}

func usageFilterMatch(evt usage.Event, filter usage.Filter, clientKey, modelKey string) bool {
	t := evt.Timestamp.UTC()
	if filter.From != nil && t.Before(filter.From.UTC()) {
		return false
	}
	if filter.To != nil && t.After(filter.To.UTC()) {
		return false
	}
	if clientKey != "" && evt.ClientName != clientKey {
		return false
	}
	if modelKey != "" && strings.ToLower(evt.Model) != modelKey {
		return false
	}
	return true
}

func usageNormalizeGroupBy(groupBy string) string {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy != "hour" {
		return "day"
	}
	return groupBy
}

func usageStepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

// bucketStartFromHour rounds an hour-bucket timestamp down to the requested granularity.
func bucketStartFromHour(hour time.Time, groupBy string) time.Time {
	hour = hour.UTC()
	if groupBy == "hour" {
		return hour
	}
	return time.Date(hour.Year(), hour.Month(), hour.Day(), 0, 0, 0, 0, time.UTC)
}

// addCellToTotals adds one agg cell into a Totals accumulator.
func addCellToTotals(target *usage.Totals, cell AggCell, cost float64) {
	target.Requests += cell.Requests
	target.InputTokens += cell.InputTokens
	target.OutputTokens += cell.OutputTokens
	target.CachedTokens += cell.CachedTokens
	target.IncompleteUsageCount += cell.IncompleteUsageCount
	target.IncompleteCostCount += cell.IncompleteCostCount
	target.CacheCreationTokens += cell.CacheCreationTokens
	target.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
	target.ReasoningTokens += cell.ReasoningTokens
	target.CacheCreation5mTokens += cell.CacheCreation5mTokens
	target.CacheCreation1hTokens += cell.CacheCreation1hTokens
	target.ToolUsePromptTokens += cell.ToolUsePromptTokens
	target.ImageInputTokens += cell.ImageInputTokens
	target.CachedImageInputTokens += cell.CachedImageInputTokens
	target.ImageOutputTokens += cell.ImageOutputTokens
	target.EstimatedCost += cost
}

// addSuccessfulCellToTotals uses successful usage-bearing events for the
// Target-aware reporting request count while retaining the full token/cost
// accounting of the hourly cell.
func addSuccessfulCellToTotals(target *usage.Totals, cell AggCell, cost float64) {
	target.Requests += cell.Success
	target.InputTokens += cell.InputTokens
	target.OutputTokens += cell.OutputTokens
	target.CachedTokens += cell.CachedTokens
	target.IncompleteUsageCount += cell.IncompleteUsageCount
	target.IncompleteCostCount += cell.IncompleteCostCount
	target.CacheCreationTokens += cell.CacheCreationTokens
	target.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
	target.ReasoningTokens += cell.ReasoningTokens
	target.CacheCreation5mTokens += cell.CacheCreation5mTokens
	target.CacheCreation1hTokens += cell.CacheCreation1hTokens
	target.ToolUsePromptTokens += cell.ToolUsePromptTokens
	target.ImageInputTokens += cell.ImageInputTokens
	target.CachedImageInputTokens += cell.CachedImageInputTokens
	target.ImageOutputTokens += cell.ImageOutputTokens
	target.EstimatedCost += cost
}

// aggCellCost computes estimated cost for one agg cell given its model.
func aggCellCost(cell AggCell, target string, hour time.Time, model string, resolver usage.RateResolver) float64 {
	if resolver == nil {
		return 0
	}
	rate, ok := resolver.RateFor(target, model, hour)
	if !ok {
		return 0
	}
	if cell.NonBillable != nil {
		cell.InputTokens = max(0, cell.InputTokens-cell.NonBillable.InputTokens)
		cell.OutputTokens = max(0, cell.OutputTokens-cell.NonBillable.OutputTokens)
		cell.CachedTokens = max(0, cell.CachedTokens-cell.NonBillable.CachedTokens)
		cell.CacheCreationTokens = max(0, cell.CacheCreationTokens-cell.NonBillable.CacheCreationTokens)
		cell.ExplicitCacheReadTokens = max(0, cell.ExplicitCacheReadTokens-cell.NonBillable.ExplicitCacheReadTokens)
		cell.CacheCreation5mTokens = max(0, cell.CacheCreation5mTokens-cell.NonBillable.CacheCreation5mTokens)
		cell.CacheCreation1hTokens = max(0, cell.CacheCreation1hTokens-cell.NonBillable.CacheCreation1hTokens)
		cell.ImageInputTokens = max(0, cell.ImageInputTokens-cell.NonBillable.ImageInputTokens)
		cell.CachedImageInputTokens = max(0, cell.CachedImageInputTokens-cell.NonBillable.CachedImageInputTokens)
		cell.ImageOutputTokens = max(0, cell.ImageOutputTokens-cell.NonBillable.ImageOutputTokens)
	}
	return rate.CostFor(cell.InputTokens, cell.OutputTokens, cell.CachedTokens, cell.CacheCreationTokens, cell.ExplicitCacheReadTokens, cell.CacheCreation5mTokens, cell.CacheCreation1hTokens, cell.ImageInputTokens, cell.CachedImageInputTokens, cell.ImageOutputTokens)
}

func usageSortedDimensions(input map[string]*usage.DimensionTotals) []usage.DimensionTotals {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]usage.DimensionTotals, 0, len(keys))
	for _, key := range keys {
		result = append(result, *input[key])
	}
	return result
}

// usageDimsByCostDesc 按估算费用降序输出维度聚合，费用并列时按 key 升序保证确定性。
func usageDimsByCostDesc(input map[string]*usage.DimensionTotals) []usage.DimensionTotals {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ci, cj := input[keys[i]].EstimatedCost, input[keys[j]].EstimatedCost
		if ci != cj {
			return ci > cj
		}
		return keys[i] < keys[j]
	})

	result := make([]usage.DimensionTotals, 0, len(keys))
	for _, key := range keys {
		result = append(result, *input[key])
	}
	return result
}
