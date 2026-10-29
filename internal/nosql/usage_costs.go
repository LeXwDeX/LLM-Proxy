package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

var ErrUsageHistoryIncomplete = errors.New("usage history is incomplete for exact pricing")

// ErrUsageAggregateSourceMissing is an activation blocker with unknown scope.
var ErrUsageAggregateSourceMissing = errors.New("initialized usage aggregate source is missing; version activation blocked")

type TargetModelCost struct {
	Target  string
	Model   string
	CostUSD float64
}

// CostUnit is either one stable-price hourly rollup or one precisely priced
// event. Hour retains reporting's inclusive hour-window semantics; At is the
// price/window timestamp (the actual event time for sub-hour pricing).
type CostUnit struct {
	TargetModelCell
	At          time.Time
	CostUSD     float64
	EventPriced bool
	Event       *usage.Event
}

type costReadHooks struct {
	decoded     func()
	scan        func(bool) // true: client index; false: legacy raw scan
	evidenceRaw func()     // additional raw rows visited for index completeness
	direct      *directIntegrityHooks
	forceLegacy bool // call-local test/benchmark comparison; never mutable store state
	// windowKeys 标记额度校准路径：直接完整性检查里对窗口外事件/键的累积与遍历
	// 被裁剪。键格式的全量校验（validateQuotaKeys）在两条路径上都执行。
	windowKeys bool
}

type costReadResult struct {
	units []CostUnit
	raw   []usage.Event // optional precise last-hour summary, from the same scan
}

// costPairFilter limits both sides of history validation to the same reporting
// scope. Nil includes every pair; it must not be derived from existing rollups
// alone, because an orphan raw event must still fail reverse validation.
type costPairFilter func(target, model string) bool

func (allow costPairFilter) includes(target, model string) bool {
	return allow == nil || allow(target, model)
}

func observeCostRawHour(ctx context.Context, tx *bolt.Tx, refs *indexRawReferences, progress *indexProgress, evidence map[string]*costRawHourEvidence, key, value []byte, e usage.Event, filter usage.Filter, resolver usage.RateResolver, rawFrom *time.Time, to time.Time, allow costPairFilter) error {
	client := strings.TrimSpace(e.ClientName)
	proofValid, proofInvalid := false, false
	if progress != nil {
		proof, attributable, invalid, owned, err := indexProofForRaw(ctx, tx, refs, progress, key, value, e)
		if err != nil {
			return err
		}
		proofValid = attributable && !invalid
		if invalid {
			filterClient := strings.TrimSpace(filter.ClientName)
			if filterClient != "" && client != filterClient {
				if attributable && bytes.HasPrefix(proof.IndexKey, []byte(clientIndexPrefix(filterClient))) || !attributable && !owned {
					return fmt.Errorf("%w: invalid client index proof", ErrUsageHistoryIncomplete)
				}
			} else {
				model := strings.ToLower(strings.TrimSpace(filter.Model))
				disjoint := (model != "" && model != strings.ToLower(e.Model)) || !allow.includes(e.Target, e.Model)
				proofInvalid = !owned || !disjoint
			}
		}
	}
	if filterClient := strings.TrimSpace(filter.ClientName); filterClient != "" && client != filterClient {
		return nil
	}
	hour := e.Timestamp.UTC().Truncate(time.Hour)
	hourKey := string(clientHourKey(client, hour))
	observed := evidence[hourKey]
	if observed == nil {
		observed = &costRawHourEvidence{client: client, hour: hour, owned: true}
		evidence[hourKey] = observed
	}
	observed.count++
	xorDigest(&observed.digest, rawDigest(key, value))
	observed.proofInvalid = observed.proofInvalid || proofInvalid
	if proofInvalid {
		observed.relevant = true
	}
	model := strings.ToLower(strings.TrimSpace(filter.Model))
	exact := (model == "" || model == strings.ToLower(e.Model)) && allow.includes(e.Target, e.Model) && usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model)
	detail := rawFrom != nil && !hour.Before(rawFrom.UTC().Truncate(time.Hour)) && !hour.After(to.UTC().Truncate(time.Hour)) && !usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model)
	if exact || detail {
		observed.relevant = true
		owner, known, err := readRawOwner(tx, key)
		if err != nil {
			return err
		}
		digest, err := usage.RetryEquivalentDigest(e)
		observed.owned = observed.owned && known && err == nil && owner.Client == receiptClientHash(e.ClientName) && owner.Digest == digest && usageKey(e.Timestamp, e.RecordID) == string(key)
	}
	meta := tx.Bucket([]byte(BucketMeta))
	if meta != nil && meta.Get([]byte(usageClientIndexDone)) == nil {
		cursor := meta.Get([]byte(usageClientIndexCursor))
		if cursor != nil {
			index := tx.Bucket([]byte(BucketUsageEventsByClient))
			var entry []byte
			if index != nil {
				entry = index.Get(clientEventIndexKey(e, key))
			}
			covered := bytes.Compare(key, cursor) <= 0
			if covered || entry != nil {
				observed.coveredCount++
				xorDigest(&observed.coveredDigest, rawDigest(key, value))
				if !bytes.Equal(entry, key) {
					observed.coverageInvalid = true
				}
				if !covered {
					owner, known, err := readRawOwner(tx, key)
					if err != nil {
						return err
					}
					digest, err := usage.RetryEquivalentDigest(e)
					if !proofValid && (!known || err != nil || owner.Client != receiptClientHash(e.ClientName) || owner.Digest != digest || usageKey(e.Timestamp, e.RecordID) != string(key)) {
						observed.coverageInvalid = true
					}
				}
			} else {
				_, known, err := readRawOwner(tx, key)
				if err != nil {
					return err
				}
				if known {
					observed.coverageInvalid = true
				}
			}
		}
	}
	return nil
}

// ReadCostUnits reads a window once. precise clips event-priced units to
// [From, To]; false preserves reports' inclusive boundary-hour membership.
// Stable-price pairs keep the existing hourly semantics, even after retention.
// Exact pairs must match permanent rollup counts. No partial bill is returned.
func (s *UsageStore) ReadCostUnits(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool) ([]CostUnit, error) {
	result, err := s.readCostUnits(ctx, filter, resolver, precise, nil, costReadHooks{})
	return result.units, err
}

// ReadQuotaCostUnits 与 ReadCostUnits 语义一致，供额度校准使用：额度聚合键只
// 校验请求窗口内的部分（Seek 到窗口起点，该步骤耗时随窗口而非历史总量增长）。
// 窗口外的键按小时可解析即与本次读取无关；唯一差异是窗口外「小时部分无法解析」
// 的畸形键不再使本次校准失败（它们无法归属到任何窗口，报表读取路径仍会报告）。
func (s *UsageStore) ReadQuotaCostUnits(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool) ([]CostUnit, error) {
	result, err := s.readCostUnits(ctx, filter, resolver, precise, nil, costReadHooks{windowKeys: true})
	return result.units, err
}

func costBounds(filter usage.Filter) (from, to time.Time) {
	to = time.Now().UTC().Add(time.Hour)
	if filter.From != nil {
		from = filter.From.UTC()
	}
	if filter.To != nil {
		to = filter.To.UTC()
	}
	return
}

// Malformed hours may sort outside any requested window, so inspect keys once
// before the bounded rollup scan. Trust a dimension only after validating it.
func validateQuotaKeys(ctx context.Context, tx *bolt.Tx, filter usage.Filter, from, to time.Time, allow costPairFilter) error {
	b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if b == nil {
		return nil
	}
	return b.ForEach(func(k, _ []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := quotaKeyInScope(k, filter, from, to, allow)
		return err
	})
}

func quotaKeyInScope(key []byte, filter usage.Filter, from, to time.Time, allow costPairFilter) (bool, error) {
	client, model := strings.TrimSpace(filter.ClientName), strings.ToLower(strings.TrimSpace(filter.Model))
	parts := strings.Split(string(key), "|")
	hour, hourErr := parseAggHour(parts[0])
	owner, ownerKnown := "", false
	if len(parts) >= 3 {
		owner = quotaKeyDecode(parts[1])
		ownerKnown = owner != "" && utf8.ValidString(owner) && owner == strings.TrimSpace(owner) && quotaKeyEncode(owner) == parts[1]
	}
	if client != "" && ownerKnown && owner != client {
		return false, nil
	}
	if hourErr == nil && (hour.Before(from.UTC().Truncate(time.Hour)) || hour.After(to.UTC().Truncate(time.Hour))) {
		return false, nil
	}
	if len(parts) == 4 {
		currentModel, target := quotaKeyDecode(parts[3]), quotaKeyDecode(parts[2])
		modelKnown := currentModel != "" && utf8.ValidString(currentModel) && currentModel == strings.ToLower(strings.TrimSpace(currentModel)) && quotaKeyEncode(currentModel) == parts[3]
		targetKnown := utf8.ValidString(target) && target == strings.ToLower(strings.TrimSpace(target)) && quotaKeyEncode(target) == parts[2]
		if model != "" && modelKnown && model != currentModel {
			return false, nil
		}
		if allow != nil && modelKnown && targetKnown && !allow.includes(target, currentModel) {
			return false, nil
		}
		if !utf8.ValidString(currentModel) || !utf8.ValidString(target) || quotaKeyEncode(currentModel) != parts[3] || !targetKnown || (currentModel != "" && !modelKnown) || (model != "" && !modelKnown) || (allow != nil && !modelKnown) {
			return false, fmt.Errorf("%w: invalid quota aggregation key", ErrUsageHistoryIncomplete)
		}
	}
	if hourErr != nil || (client != "" && !ownerKnown) {
		return false, fmt.Errorf("%w: %w: quota aggregation owner or time unknown", ErrUsageHistoryIncomplete, ErrUsageAggregateSourceMissing)
	}
	if len(parts) != 4 || (parts[1] != "" && !ownerKnown) {
		return false, fmt.Errorf("%w: invalid quota aggregation key", ErrUsageHistoryIncomplete)
	}
	return true, nil
}

func readCostRollups(ctx context.Context, tx *bolt.Tx, filter usage.Filter, from, to time.Time, allow costPairFilter) ([]TargetModelCell, error) {
	var result []TargetModelCell
	b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if b == nil {
		return result, nil
	}
	end := to.Truncate(time.Hour).Format(hourLayout) + "}"
	c := b.Cursor()
	for k, v := c.Seek(hourPrefix(from)); k != nil && string(k) <= end; k, v = c.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inScope, err := quotaKeyInScope(k, filter, from, to, allow)
		if err != nil {
			return nil, err
		}
		if !inScope {
			continue
		}
		parts := strings.Split(string(k), "|")
		hour, _ := parseAggHour(parts[0])
		owner, currentModel := quotaKeyDecode(parts[1]), quotaKeyDecode(parts[3])
		target := quotaKeyDecode(parts[2])
		var cell AggCell
		if json.Unmarshal(v, &cell) != nil {
			return nil, fmt.Errorf("%w: invalid quota aggregation", ErrUsageHistoryIncomplete)
		}
		result = append(result, TargetModelCell{Hour: hour, Client: owner, Target: target, Model: currentModel, AggCell: cell})
	}
	return result, nil
}

// assembleCostUnits preserves bidirectional record-count parity (legacy cells
// use Requests). Supplementary rows contribute records but no requests to this
// contract. Even events outside precise bounds count towards boundary-hour
// parity, so missing/pruned history cannot silently reduce the bill.
func assembleCostUnits(ctx context.Context, cells []TargetModelCell, events []usage.Event, filter usage.Filter, resolver usage.RateResolver, precise bool, allow costPairFilter) ([]CostUnit, error) {
	from, to := costBounds(filter)
	var units []CostUnit
	expected, observed := make(map[string]int64), make(map[string]int64)
	for _, cell := range cells {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !allow.includes(cell.Target, cell.Model) {
			continue
		}
		if usage.RequiresEventTimestampsFor(resolver, cell.Target, cell.Model) {
			expected[string(quotaAggKey(cell.Hour, cell.Client, cell.Target, cell.Model))] = cell.recordCount()
		} else {
			units = append(units, CostUnit{TargetModelCell: cell, At: cell.Hour})
		}
	}
	client, model := strings.TrimSpace(filter.ClientName), strings.ToLower(strings.TrimSpace(filter.Model))
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if (client != "" && strings.TrimSpace(event.ClientName) != client) || (model != "" && strings.ToLower(event.Model) != model) || !allow.includes(event.Target, event.Model) || !usage.RequiresEventTimestampsFor(resolver, event.Target, event.Model) {
			continue
		}
		key := string(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
		observed[key]++
		if precise && (event.Timestamp.Before(from) || event.Timestamp.After(to)) {
			continue
		}
		units = append(units, CostUnit{TargetModelCell: TargetModelCell{
			Hour: event.Timestamp.UTC().Truncate(time.Hour), Client: strings.TrimSpace(event.ClientName), Target: strings.ToLower(strings.TrimSpace(event.Target)), Model: strings.ToLower(event.Model), AggCell: eventAggCell(event),
		}, At: event.Timestamp, EventPriced: true, Event: &event})
	}
	for key, count := range expected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if observed[key] != count {
			return nil, fmt.Errorf("%w: raw record count %d differs from rollup count %d", ErrUsageHistoryIncomplete, observed[key], count)
		}
	}
	for key, count := range observed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if expected[key] != count {
			return nil, fmt.Errorf("%w: raw event lacks matching quota rollup", ErrUsageHistoryIncomplete)
		}
	}
	return units, nil
}

func eventAggCell(e usage.Event) AggCell {
	c := AggCell{Requests: usage.RequestWeight(e), RecordCount: 1, InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CachedTokens: e.CachedTokens, CacheCreationTokens: e.CacheCreationTokens,
		ExplicitCacheReadTokens: e.ExplicitCacheReadTokens, ReasoningTokens: e.ReasoningTokens, CacheCreation5mTokens: e.CacheCreation5mTokens, CacheCreation1hTokens: e.CacheCreation1hTokens, ToolUsePromptTokens: e.ToolUsePromptTokens,
		ImageInputTokens: e.ImageInputTokens, CachedImageInputTokens: e.CachedImageInputTokens, ImageOutputTokens: e.ImageOutputTokens}
	if isSuccessStatus(e.StatusCode) {
		c.Success = usage.RequestWeight(e)
	}
	if e.UsageStatus == usage.UsageStatusIncomplete || e.UsageStatus == usage.UsageStatusConflict {
		c.IncompleteUsageCount = 1
	}
	if e.CostIncomplete {
		c.IncompleteCostCount = 1
	}
	c.addNonBillable(e)
	return c
}

func (s *UsageStore) readCostUnits(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool, rawFrom *time.Time, hooks costReadHooks) (costReadResult, error) {
	return s.readCostUnitsInScope(ctx, filter, resolver, precise, rawFrom, hooks, nil)
}

func (s *UsageStore) readCostUnitsInScope(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool, rawFrom *time.Time, hooks costReadHooks, allow costPairFilter) (costReadResult, error) {
	if err := ctx.Err(); err != nil {
		return costReadResult{}, err
	}
	if s == nil || s.db == nil {
		return costReadResult{}, fmt.Errorf("usage source status: store is not initialized")
	}
	from, to := costBounds(filter)
	needsExact := usage.RequiresEventTimestamps(resolver)
	// Preserve the legacy exact-cost default (now), while reports and stable
	// rollups retain their default inclusive hour bound (now + one hour).
	if precise && needsExact && filter.To == nil {
		to = time.Now().UTC()
	}
	filter.From, filter.To = &from, &to
	start, end := from.Truncate(time.Hour), to.Truncate(time.Hour).Add(time.Hour)
	result := costReadResult{}
	useIndex := needsExact && strings.TrimSpace(filter.ClientName) != "" && !from.IsZero() && !hooks.forceLegacy && (rawFrom != nil || strings.TrimSpace(filter.Model) == "" || usage.RequiresEventTimestampsForModel(resolver, filter.Model))
	err := s.db.View(func(tx *bolt.Tx) error {
		refs := &indexRawReferences{tx: tx}
		missing, err := missingUsageAggregateSources(tx)
		if err != nil {
			return err
		}
		for _, source := range missing {
			if source == BucketQuotaUsageAggHourly {
				return fmt.Errorf("%w: %w: %s", ErrUsageHistoryIncomplete, ErrUsageAggregateSourceMissing, source)
			}
		}
		if (precise && needsExact && to.Before(from)) || to.Truncate(time.Hour).Before(from.Truncate(time.Hour)) {
			return ctx.Err()
		}
		meta := tx.Bucket([]byte(BucketMeta))
		marker := meta.Get([]byte(usageClientIndexSourceMissing))
		if marker != nil || meta.Bucket([]byte(usageClientIndexSourceMissing)) != nil {
			scopeNeedsExact := needsExact
			if rawFrom == nil && strings.TrimSpace(filter.Model) != "" {
				scopeNeedsExact = usage.RequiresEventTimestampsForModel(resolver, filter.Model)
			}
			if scopeNeedsExact || rawFrom != nil {
				return fmt.Errorf("%w: %w: client index source missing", ErrUsageHistoryIncomplete, ErrUsageAggregateSourceMissing)
			}
		}
		var progress *indexProgress
		scopeNeedsEvidence := rawFrom != nil || (needsExact && (strings.TrimSpace(filter.Model) == "" || usage.RequiresEventTimestampsForModel(resolver, filter.Model)))
		if scopeNeedsEvidence {
			progress, err = loadIndexProgress(tx)
			if err != nil {
				// A lost v1 completion marker can fall back only to independent
				// raw-owner evidence; validation below enforces that per hour.
				p, stateErr := loadIndexProgressState(tx, false)
				if stateErr != nil || p == nil || p.Phase != "complete" || meta.Get([]byte(usageClientIndexDone)) != nil || meta.Get([]byte(usageClientIndexCursor)) != nil {
					return fmt.Errorf("%w: invalid client index progress: %v", ErrUsageHistoryIncomplete, err)
				}
				progress = p
			}
			if err := refs.checkOrphans(ctx, progress, filter, from, to, allow); err != nil {
				return err
			}
		}
		// 全量校验：窗口外「小时部分无法解析」的畸形键同样必须使额度读取 fail-closed。
		if err := validateQuotaKeys(ctx, tx, filter, from, to, allow); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkUnresolvedUsageInScopeContext(ctx, tx, filter.ClientName, from, to, filter.Model, allow); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		direct := hooks.direct
		if hooks.windowKeys && direct == nil {
			direct = &directIntegrityHooks{windowKeys: true}
		}
		if err := checkDirectQuotaEvidenceContext(ctx, tx, filter, from, to, allow, direct); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkMissingFreshRootsContext(ctx, tx, filter, from, to); err != nil {
			return err
		}
		if useIndex {
			if hooks.scan != nil {
				hooks.scan(true)
			}
			indexErr := requireClientIndex(tx)
			for hour := start; indexErr == nil && hour.Before(end); hour = hour.Add(time.Hour) {
				if err := ctx.Err(); err != nil {
					return err
				}
				events, err := readClientHourInTx(ctx, tx, strings.TrimSpace(filter.ClientName), hour, hooks.decoded, func(key, data []byte, event usage.Event) error {
					model := strings.ToLower(strings.TrimSpace(filter.Model))
					relevant := (model == "" || model == strings.ToLower(event.Model)) && allow.includes(event.Target, event.Model) && usage.RequiresEventTimestampsFor(resolver, event.Target, event.Model)
					if rawFrom != nil && !usage.RequiresEventTimestampsFor(resolver, event.Target, event.Model) {
						relevant = true
					}
					if relevant {
						_, _, invalid, _, err := indexProofForRaw(ctx, tx, refs, progress, key, data, event)
						if err != nil {
							return err
						}
						if invalid {
							return fmt.Errorf("%w: invalid client index proof", ErrUsageHistoryIncomplete)
						}
					}
					return nil
				})
				if err != nil {
					indexErr = err
					break
				}
				cells, err := readCostRollups(ctx, tx, filter, hour, hour, allow)
				if err != nil {
					return err
				}
				units, err := assembleCostUnits(ctx, cells, events, filter, resolver, precise, allow)
				if err != nil {
					indexErr = err
					break
				}
				result.units = append(result.units, units...)
				if rawFrom != nil {
					for _, e := range events {
						if err := ctx.Err(); err != nil {
							return err
						}
						if !e.Timestamp.Before(*rawFrom) && !e.Timestamp.After(to) && !usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
							result.raw = append(result.raw, e)
						}
					}
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if indexErr == nil {
				// The index and its digest can agree while an extra raw row is
				// absent from both. Compare against raw independently as well.
				evidence := make(map[string]*costRawHourEvidence)
				c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
				for k, v := c.Seek([]byte(start.Format(hourLayout))); k != nil && string(k) < end.Format(hourLayout); k, v = c.Next() {
					if err := ctx.Err(); err != nil {
						return err
					}
					if hooks.evidenceRaw != nil {
						hooks.evidenceRaw()
					}
					var e usage.Event
					if json.Unmarshal(v, &e) != nil {
						continue
					}
					if err := observeCostRawHour(ctx, tx, refs, progress, evidence, k, v, e, filter, resolver, rawFrom, to, allow); err != nil {
						return err
					}
				}
				valid, err := validateCostRawHourEvidence(ctx, tx, progress, evidence)
				if err != nil {
					return err
				}
				if valid {
					return nil
				}
				indexErr = ErrUsageClientIndexIncomplete
			}
			slog.Warn("exact usage pricing: client index unavailable; falling back to legacy scan", "error", indexErr)
			result = costReadResult{}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cells, err := readCostRollups(ctx, tx, filter, from, to, allow)
		if err != nil {
			return err
		}
		// Scan even if no exact rollup exists: an orphan precise-price raw
		// event must fail the reverse parity check, as on the legacy path.
		wantEvents := needsExact
		var events []usage.Event
		evidence := make(map[string]*costRawHourEvidence)
		if wantEvents || rawFrom != nil {
			if hooks.scan != nil {
				hooks.scan(false)
			}
			seek := start.Format(hourLayout)
			if !wantEvents {
				seek = rawFrom.UTC().Truncate(time.Hour).Format(hourLayout)
			}
			if b := tx.Bucket([]byte(BucketUsageEvents)); b != nil {
				c := b.Cursor()
				for k, v := c.Seek([]byte(seek)); k != nil && string(k) < end.Format(hourLayout); k, v = c.Next() {
					if err := ctx.Err(); err != nil {
						return err
					}
					if hooks.decoded != nil {
						hooks.decoded()
					}
					var e usage.Event
					if json.Unmarshal(v, &e) != nil {
						continue
					} // same legacy bad-record semantics
					if needsExact || rawFrom != nil {
						if err := observeCostRawHour(ctx, tx, refs, progress, evidence, k, v, e, filter, resolver, rawFrom, to, allow); err != nil {
							return err
						}
					}
					if wantEvents && allow.includes(e.Target, e.Model) && usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
						events = append(events, e)
					}
					if rawFrom != nil && !e.Timestamp.Before(*rawFrom) && !e.Timestamp.After(to) && !usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
						result.raw = append(result.raw, e)
					}
				}
			}
		}
		if scopeNeedsEvidence {
			if _, err := validateCostRawHourEvidence(ctx, tx, progress, evidence); err != nil {
				return err
			}
		}
		result.units, err = assembleCostUnits(ctx, cells, events, filter, resolver, precise, allow)
		return err
	})
	if ctx.Err() != nil {
		return costReadResult{}, ctx.Err()
	}
	if err != nil {
		return costReadResult{}, err
	}
	// Pricing is deliberately outside every database transaction.
	for i := range result.units {
		if err := ctx.Err(); err != nil {
			return costReadResult{}, err
		}
		u := &result.units[i]
		if u.Event != nil {
			usage.CheckEventPrices(u.Event, resolver)
			if u.Event.CostIncomplete {
				u.IncompleteCostCount = 1
			}
			u.CostUSD = usage.EstimateEventCost(*u.Event, resolver)
		} else {
			var missingPrice int64
			u.CostUSD, missingPrice = aggCellCost(u.AggCell, u.Target, u.At, u.Model, resolver)
			u.IncompleteCostCount += missingPrice
		}
	}
	if err := ctx.Err(); err != nil {
		return costReadResult{}, err
	}
	return result, nil
}

func (s *UsageStore) SumTargetModelCostsRange(filter usage.Filter, resolver usage.RateResolver) ([]TargetModelCost, error) {
	units, err := s.ReadCostUnits(context.Background(), filter, resolver, true)
	if err != nil {
		return nil, err
	}
	totals := make(map[string]TargetModelCost)
	for _, u := range units {
		key := string(quotaAggKey(time.Time{}, "", u.Target, u.Model))
		v := totals[key]
		v.Target = u.Target
		v.Model = u.Model
		v.CostUSD += u.CostUSD
		totals[key] = v
	}
	keys := make([]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]TargetModelCost, 0, len(keys))
	for _, key := range keys {
		result = append(result, totals[key])
	}
	return result, nil
}
