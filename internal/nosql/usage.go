package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// UsageStore manages usage events backed by bbolt.
// It implements the usage.Recorder interface.
//
// Raw detail, two hourly rollups, a derived client/time index, and digest
// receipts for pruned identities are involved:
//   - usage_events          : raw event detail (RFC3339Nano + uuid key)  → for List()
//   - usage_agg_hourly      : pre-aggregated hourly rollups               → for Summary/Aggregate/Count
//   - usage_record_receipts : versioned digests retained after raw pruning
//
// Record() writes all affected records in the same write tx so they stay consistent.
type UsageStore struct {
	db *bolt.DB
	// receiptMu serializes the one-time legacy coverage scan. receiptsReady is
	// guarded by receiptMu and stays false when a scan fails so it can retry.
	// A failure is cached in receiptErr until receiptRetryAt, with the delay
	// doubling per failure (receiptBackoff), so writes do not rescan.
	receiptMu      sync.Mutex
	receiptsReady  bool
	receiptErr     error
	receiptRetryAt time.Time
	receiptBackoff time.Duration
	// receiptScans counts migration attempts that read the database.
	receiptScans atomic.Int64
	// now overrides the clock for receipt retry backoff and Count warning
	// rate limiting in tests.
	now func() time.Time

	// countWarnMu guards Count's skipped-cell WARN rate limit: at most one
	// WARN per countWarnInterval, carrying the number of suppressed ones.
	countWarnMu         sync.Mutex
	countWarnAt         time.Time
	countWarnSuppressed int
}

// countWarnInterval bounds how often Count logs skipped malformed cells.
const countWarnInterval = time.Minute

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
	if err := usage.ValidateExplicitRecordIdentity(event); err != nil {
		return false, s.preserveInvalidUsage(event, withUsageKind(ErrUsageBundleInvalid, err))
	}
	rows, keys, err := normalizeUsageBundle(event)
	if err != nil {
		return false, s.preserveInvalidUsage(event, err)
	}
	migrationErr := s.EnsureUsageReceipts()
	if s == nil || s.db == nil {
		return false, migrationErr
	}
	// P8：用 db.Batch 而非 db.Update —— bbolt 写事务全库串行且每次 commit 触发
	// fsync，高并发下每请求一次同步 Update 会在全部成功请求上形成吞吐天花板。
	// Batch 将并发调用合并为单次 commit（≤MaxBatchDelay/≤1000 条，见 OpenDB）。
	// 生产请求路径不再同步等待这里：UsageJournal 异步模式先同步追加恢复日志，
	// 再由后台写入器调用 RecordBatch；本方法服务于回放与同步回退路径。
	// 安全性：合并事务失败时整体回滚后逐个重试，身份判定基于已提交状态，
	// 不会双计。
	added := false
	var rejection error
	err = s.db.Batch(func(tx *bolt.Tx) error {
		// bbolt may retry this callback after a different batch member fails.
		added = false
		rejection = nil
		fresh := freshScopeProven(tx, rows) || continuationScopeProven(tx, rows)
		var err error
		added, err = recordUsageBundleTx(tx, rows, keys, migrationErr)
		if errors.Is(err, ErrUsageIdentityUnverified) || errors.Is(err, ErrUsageBundlePartial) {
			original, encodeErr := json.Marshal(event)
			if encodeErr != nil {
				return encodeErr
			}
			if saveErr := putUnresolvedUsage(tx, rows[0], original); saveErr != nil {
				return saveErr
			}
			rejection = err
			return nil
		}
		if err == nil {
			if !added && tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(keys[0])) != nil && !usageAccountingProven(tx, rows, keys) {
				original, encodeErr := json.Marshal(event)
				if encodeErr != nil {
					return encodeErr
				}
				if saveErr := putUnresolvedUsage(tx, rows[0], original); saveErr != nil {
					return saveErr
				}
				if knownUnverifiedLiveAccounting(tx, rows, keys) {
					rejection = fmt.Errorf("%w: committed identity has unverified quota accounting", ErrUsageHistoryIncomplete)
				}
				return nil
			}
			if added && fresh {
				if err := writeFreshAttestation(tx, rows[0], keys[0]); err != nil {
					return err
				}
			}
			return resolveUnresolvedUsage(tx, rows[0])
		}
		return err
	})
	if err == nil && rejection != nil {
		return false, rejection
	}
	if err != nil {
		original, encodeErr := json.Marshal(event)
		if encodeErr == nil {
			encodeErr = s.RegisterUnresolvedUsage(rows[0], original)
		}
		return false, errors.Join(err, encodeErr)
	}
	return added && err == nil, err
}

// RecordBatch commits several usage events in one write transaction. It is
// all-or-nothing: any rejected event rolls the whole batch back so the caller
// can retry the events individually. Identical replays are idempotent by
// the stable Timestamp and RecordID pair, exactly like Record.
func (s *UsageStore) RecordBatch(events []usage.Event) error {
	return s.recordBatchContext(context.Background(), events)
}

func (s *UsageStore) recordBatchContext(ctx context.Context, events []usage.Event) error {
	if len(events) == 0 {
		return nil
	}
	type normalizedBundle struct {
		rows     []usage.Event
		keys     []string
		original usage.Event
	}
	var bundles []normalizedBundle
	var invalid error
	for _, event := range events {
		if err := usage.ValidateExplicitRecordIdentity(event); err != nil {
			if invalid == nil {
				invalid = withUsageKind(ErrUsageBundleInvalid, err)
			}
			bundles = append(bundles, normalizedBundle{original: event})
			continue
		}
		rows, keys, err := normalizeUsageBundle(event)
		if err != nil {
			if invalid == nil {
				invalid = err
			}
			bundles = append(bundles, normalizedBundle{original: event})
			continue
		}
		bundles = append(bundles, normalizedBundle{rows: rows, keys: keys, original: event})
	}
	migrationErr := s.EnsureUsageReceipts()
	if s == nil || s.db == nil {
		return migrationErr
	}
	var rejection error
	err := s.db.Update(func(tx *bolt.Tx) error {
		committedRows := make([]bool, len(bundles))
		inspectionErrors := make([]error, len(bundles))
		type seenIdentity struct{ digest, bundle string }
		seen := make(map[string]seenIdentity)
		rejection = invalid
		for i, bundle := range bundles {
			if len(bundle.rows) == 0 {
				continue
			}
			committed, inspectErr := inspectUsageBundleTx(tx, bundle.rows, bundle.keys, migrationErr)
			committedRows[i], inspectionErrors[i] = committed, inspectErr
			if committed && knownUnverifiedLiveAccounting(tx, bundle.rows, bundle.keys) && rejection == nil {
				rejection = fmt.Errorf("%w: committed identity has unverified quota accounting", ErrUsageHistoryIncomplete)
			}
			if inspectErr != nil && rejection == nil {
				rejection = inspectErr
			}
			bundleDigest, err := usage.RetryEquivalentDigest(bundle.rows[0])
			if err != nil {
				return err
			}
			for n, key := range bundle.keys {
				digest, err := usage.RetryEquivalentDigest(bundle.rows[n])
				if err != nil {
					return err
				}
				if prior, ok := seen[key]; ok && (prior.digest != digest || prior.bundle != bundleDigest) && rejection == nil {
					rejection = withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
				}
				seen[key] = seenIdentity{digest: digest, bundle: bundleDigest}
			}
		}
		if rejection != nil {
			for i, bundle := range bundles {
				proven := false
				if len(bundle.rows) > 0 && (committedRows[i] || errors.Is(inspectionErrors[i], ErrUsageRecordConflict)) {
					proven = usageAccountingProven(tx, bundle.rows, bundle.keys)
					if committedRows[i] && proven {
						continue
					}
				}
				original, err := json.Marshal(bundle.original)
				if err != nil {
					return err
				}
				candidate := bundle.original
				if len(bundle.rows) > 0 {
					candidate = bundle.rows[0]
				}
				if err := putUnresolvedEvidence(tx, unresolvedUsage{Event: candidate, Original: original, Resolved: proven}); err != nil {
					return err
				}
			}
			return nil
		}
		fresh := make([]bool, len(bundles))
		memo := freshScopeMemo{cells: make(map[string]bool), scopes: make(map[string]continuationProof)}
		for i, bundle := range bundles {
			fresh[i] = memo.proven(ctx, tx, bundle.rows)
		}
		for i, bundle := range bundles {
			added, err := recordUsageBundleTx(tx, bundle.rows, bundle.keys, migrationErr)
			if err != nil {
				return err
			}
			if added && fresh[i] {
				if err := writeFreshAttestation(tx, bundle.rows[0], bundle.keys[0]); err != nil {
					return err
				}
			}
			if !added && tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(bundle.keys[0])) != nil && !usageAccountingProven(tx, bundle.rows, bundle.keys) {
				original, err := json.Marshal(bundle.original)
				if err != nil {
					return err
				}
				if err := putUnresolvedUsage(tx, bundle.rows[0], original); err != nil {
					return err
				}
			}
			if err := resolveUnresolvedUsage(tx, bundle.rows[0]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		saveErr := s.db.Update(func(tx *bolt.Tx) error {
			for _, bundle := range bundles {
				candidate := bundle.original
				if len(bundle.rows) > 0 {
					committed, inspectErr := inspectUsageBundleTx(tx, bundle.rows, bundle.keys, migrationErr)
					if (committed || errors.Is(inspectErr, ErrUsageRecordConflict)) && usageAccountingProven(tx, bundle.rows, bundle.keys) {
						continue
					}
					candidate = bundle.rows[0]
				}
				original, err := json.Marshal(bundle.original)
				if err != nil {
					return err
				}
				if err := putUnresolvedUsage(tx, candidate, original); err != nil {
					return err
				}
			}
			return nil
		})
		return errors.Join(err, saveErr)
	}
	return rejection
}

// normalizeUsageBundle keeps root metadata intact and expands every additional
// row before opening a write transaction. A rejected component rejects its root.
// Every failure is payload-deterministic and tagged ErrUsageBundleInvalid.
func normalizeUsageBundle(event usage.Event) ([]usage.Event, []string, error) {
	rows, keys, err := expandUsageBundle(event)
	return rows, keys, withUsageKind(ErrUsageBundleInvalid, err)
}

func expandUsageBundle(event usage.Event) ([]usage.Event, []string, error) {
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

// RecordExists verifies every row of a supplied stable bundle identity against
// retained raw detail or its pruned receipt. It is used only by explicit
// journal retries, not ordinary requests.
func (s *UsageStore) RecordExists(event usage.Event) (bool, error) {
	if err := usage.ValidateExplicitRecordIdentity(event); err != nil {
		return false, withUsageKind(ErrUsageBundleInvalid, err)
	}
	rows, keys, err := normalizeUsageBundle(event)
	if err != nil {
		return false, err
	}
	if s == nil || s.db == nil {
		return false, fmt.Errorf("usage receipts: store is not initialized")
	}
	exists := false
	err = s.db.View(func(tx *bolt.Tx) error {
		quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
		for _, key := range keys {
			if quarantine != nil && quarantine.Get([]byte(key)) != nil {
				return fmt.Errorf("%w: usage record is quarantined pending reconciliation", ErrUsageClientIndexIncomplete)
			}
		}
		committed := 0
		for i := range rows {
			digest, err := usage.RetryEquivalentDigest(rows[i])
			if err != nil {
				return err
			}
			state, _, _, err := lookupUsageIdentity(tx, []byte(keys[i]), []byte(digest))
			if err != nil {
				return classifyUsageConflict(tx, rows, keys, err)
			}
			if state != identityNew {
				committed++
			}
		}
		if committed == len(rows) {
			exists = true
			return nil
		}
		if committed > 0 {
			return withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
		}
		now := time.Now()
		if s.now != nil {
			now = s.now()
		}
		return checkUsageIdentityAdmissionAt(tx, rows, &now)
	})
	return exists, err
}

func normalizeUsageEvent(event *usage.Event) (string, error) {
	usage.NormalizeImagePartitions(event)
	if err := usage.ValidateExplicitRecordIdentity(*event); err != nil {
		return "", err
	}
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
// It reads both physical aggregate key formats and only needs hour and totals,
// so structurally valid legacy keys stay counted even when their dimensions are
// ambiguous. Like v1.0.1, this display-only total skips malformed keys and
// undecodable cells. It logs at most one WARN per minute per store with the
// skipped count, the first skipped key (never a cell value) and how many
// warnings were suppressed since the last one. Billing and quota readers stay
// fail-closed.
func (s *UsageStore) Count(from, to time.Time) (total, success int64, err error) {
	var skipped int
	var firstKey string
	err = s.db.View(func(tx *bolt.Tx) error {
		total, success, skipped, firstKey = 0, 0, 0, ""
		return aggCellIterSkipping(tx, from, to, func(entry aggCellEntry) error {
			total += entry.cell.Requests
			success += entry.cell.Success
			return nil
		}, func(key []byte, _ error) {
			if skipped == 0 {
				firstKey = string(key)
			}
			skipped++
		})
	})
	if err != nil {
		return 0, 0, err
	}
	if skipped > 0 {
		s.warnCountSkipped(skipped, firstKey)
	}
	return total, success, nil
}

func (s *UsageStore) warnCountSkipped(skipped int, firstKey string) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	at := now()
	s.countWarnMu.Lock()
	if !s.countWarnAt.IsZero() && at.Sub(s.countWarnAt) < countWarnInterval {
		s.countWarnSuppressed++
		s.countWarnMu.Unlock()
		return
	}
	suppressed := s.countWarnSuppressed
	s.countWarnAt, s.countWarnSuppressed = at, 0
	s.countWarnMu.Unlock()
	slog.Warn("usage count skipped malformed hourly aggregate cells; totals exclude them",
		"bucket", BucketUsageAggHourly, "skipped", skipped, "first_key", firstKey,
		"suppressed_warnings", suppressed)
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
		if err := checkUnresolvedUsageInScope(tx, clientKey, from, to, modelKey, nil); err != nil {
			return err
		}
		if err := checkDirectQuotaEvidence(tx, filter, from, to, nil, nil); err != nil {
			return err
		}
		if err := checkMissingFreshRoots(tx, filter, from, to); err != nil {
			return err
		}
		return aggIter(tx, from, to, func(hour time.Time, et, client, model, target string, cell AggCell) error {
			// Apply dimension filters.
			if clientKey != "" && client != clientKey {
				return nil
			}
			if modelKey != "" && model != modelKey {
				return nil
			}

			cost, missingPrice := aggCellCost(cell, "", hour, model, costs)
			cell.IncompleteCostCount += missingPrice
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
	return s.SummaryContext(context.Background(), now, costs)
}

// SummaryContext is Summary bounded by ctx: a canceled request context stops the scan.
func (s *UsageStore) SummaryContext(ctx context.Context, now time.Time, costs usage.RateResolver) (usage.SummaryResult, error) {
	return s.summaryReadContext(ctx, now, costs, costReadHooks{})
}

func (s *UsageStore) summaryRead(now time.Time, costs usage.RateResolver, hooks costReadHooks) (usage.SummaryResult, error) {
	return s.summaryReadContext(context.Background(), now, costs, hooks)
}

func (s *UsageStore) summaryReadContext(ctx context.Context, now time.Time, costs usage.RateResolver, hooks costReadHooks) (usage.SummaryResult, error) {
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
	data, err := s.readCostUnits(ctx, usage.Filter{From: &last30Start, To: &now}, costs, false, &lastHourStart, hooks)
	if err != nil {
		return usage.SummaryResult{}, err
	}
	for _, event := range data.raw {
		usage.CheckEventPrices(&event, costs)
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
// Each decoded event writes a normalized digest receipt in the same transaction
// before its raw row is removed, so a later replay cannot resurrect pruned
// detail or double-count its permanent rollups. Undecodable records are moved
// losslessly to quarantine instead of blocking subsequent batches. Their unknown
// ownership invalidates indexed reads globally.
func (s *UsageStore) PruneEventsBefore(cutoff time.Time, batchSize int) (int, error) {
	if err := s.EnsureUsageReceipts(); err != nil {
		return 0, err
	}
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
			if _, err := rollupProofSource(tx); err != nil {
				return err
			}
			progress, err := loadIndexProgressState(tx, false)
			if err != nil {
				return err
			}
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
			proofCache := make(map[string]bool)
			refs := &indexRawReferences{tx: tx}
			for _, k := range keys {
				data := b.Get(k)
				var event usage.Event
				decodeErr := json.Unmarshal(data, &event)
				if decodeErr == nil && progress != nil {
					_, _, invalid, _, err := indexProofForRaw(context.Background(), tx, refs, progress, k, data, event)
					if err != nil {
						return err
					}
					if invalid {
						return fmt.Errorf("%w: invalid client index proof during retention", ErrUsageHistoryIncomplete)
					}
				}
				if decodeErr != nil {
					if err := quarantineUsageEvent(tx, k, data); err != nil {
						return err
					}
					if err := preserveUnprovenPrune(tx, event, k); err != nil {
						return err
					}
					quarantined++
				} else {
					if err := capturePruneRollupProof(tx, event, k, proofCache); err != nil {
						return err
					}
					if err := removeClientEventIndex(tx, k, data); err != nil {
						return err
					}
					digest, err := usage.RetryEquivalentDigest(event)
					if err != nil {
						return err
					}
					if err := writeUsageReceipt(tx, k, digest); err != nil {
						return err
					}
					if err := writeReceiptOwner(tx, k, digest, event, data); err != nil {
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

// aggCellCost computes estimated cost for one agg cell given its model. The
// second result is the number of rows whose iteration price is now missing; it
// is query-derived and must never be written back to stored rollups.
func aggCellCost(cell AggCell, target string, hour time.Time, model string, resolver usage.RateResolver) (float64, int64) {
	if resolver == nil {
		return 0, 0
	}
	rate, ok := resolver.RateFor(target, model, hour)
	if !ok {
		return 0, cell.missingIterationPriceCount()
	}
	cell = cell.billable()
	return rate.CostFor(cell.InputTokens, cell.OutputTokens, cell.CachedTokens, cell.CacheCreationTokens, cell.ExplicitCacheReadTokens, cell.CacheCreation5mTokens, cell.CacheCreation1hTokens, cell.ImageInputTokens, cell.CachedImageInputTokens, cell.ImageOutputTokens), 0
}

// billable removes observed non-billable tokens from the cell without changing
// the cost formula or price selection.
func (cell AggCell) billable() AggCell {
	if cell.NonBillable == nil {
		return cell
	}
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
	return cell
}

func (cell AggCell) hasBillableTokens() bool {
	cell = cell.billable()
	return cell.InputTokens > 0 || cell.OutputTokens > 0 || cell.CachedTokens > 0 ||
		cell.CacheCreationTokens > 0 || cell.ExplicitCacheReadTokens > 0 ||
		cell.CacheCreation5mTokens > 0 || cell.CacheCreation1hTokens > 0 ||
		cell.ImageInputTokens > 0 || cell.CachedImageInputTokens > 0 || cell.ImageOutputTokens > 0
}

// missingIterationPriceCount reports rows that would be incomplete if their
// price disappeared. Legacy cells lack per-row coverage, so they contribute at
// most one conservative marker. Rows already counted as incomplete are never
// counted again.
func (cell AggCell) missingIterationPriceCount() int64 {
	total := cell.recordCount()
	legacyUnknown := cell.PriceCoverageRecordCount == 0 || cell.PriceCoverageRecordCount < total
	missing := cell.IterationPriceCompleteCount
	if legacyUnknown && cell.hasBillableTokens() {
		missing++
	}
	limit := total
	if legacyUnknown && limit == 0 {
		limit = 1
	}
	return max(0, min(missing, limit-cell.IncompleteCostCount))
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
