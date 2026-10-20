package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

var ErrUsageHistoryIncomplete = errors.New("usage history is incomplete for exact pricing")

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
	forceLegacy bool       // call-local test/benchmark comparison; never mutable store state
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

// ReadCostUnits reads a window once. precise clips event-priced units to
// [From, To]; false preserves reports' inclusive boundary-hour membership.
// Stable-price pairs keep the existing hourly semantics, even after retention.
// Exact pairs must match permanent rollup counts. No partial bill is returned.
func (s *UsageStore) ReadCostUnits(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool) ([]CostUnit, error) {
	result, err := s.readCostUnits(ctx, filter, resolver, precise, nil, costReadHooks{})
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

func readCostRollups(tx *bolt.Tx, filter usage.Filter, from, to time.Time, strict bool, allow costPairFilter) ([]TargetModelCell, error) {
	var result []TargetModelCell
	b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if b == nil {
		return result, nil
	}
	client := strings.TrimSpace(filter.ClientName)
	model := strings.ToLower(strings.TrimSpace(filter.Model))
	end := to.Truncate(time.Hour).Format(hourLayout) + "}"
	c := b.Cursor()
	for k, v := c.Seek(hourPrefix(from)); k != nil && string(k) <= end; k, v = c.Next() {
		parts := strings.SplitN(string(k), "|", 4)
		if len(parts) != 4 {
			if strict {
				return nil, fmt.Errorf("%w: invalid quota aggregation key", ErrUsageHistoryIncomplete)
			}
			continue
		}
		owner, currentModel := quotaKeyDecode(parts[1]), quotaKeyDecode(parts[3])
		target := quotaKeyDecode(parts[2])
		if (client != "" && owner != client) || (model != "" && currentModel != model) || !allow.includes(target, currentModel) {
			continue
		}
		var cell AggCell
		hour, err := time.Parse(hourLayout, parts[0])
		if err != nil || json.Unmarshal(v, &cell) != nil {
			if strict {
				return nil, fmt.Errorf("%w: invalid quota aggregation", ErrUsageHistoryIncomplete)
			}
			continue
		}
		result = append(result, TargetModelCell{Hour: hour, Client: owner, Target: target, Model: currentModel, AggCell: cell})
	}
	return result, nil
}

// assembleCostUnits preserves the old bidirectional request-count parity
// contract. Even events outside precise bounds count towards boundary-hour
// parity, so missing/pruned history cannot silently reduce the bill.
func assembleCostUnits(cells []TargetModelCell, events []usage.Event, filter usage.Filter, resolver usage.RateResolver, precise bool, allow costPairFilter) ([]CostUnit, error) {
	from, to := costBounds(filter)
	var units []CostUnit
	expected, observed := make(map[string]int64), make(map[string]int64)
	for _, cell := range cells {
		if !allow.includes(cell.Target, cell.Model) {
			continue
		}
		if usage.RequiresEventTimestampsFor(resolver, cell.Target, cell.Model) {
			expected[string(quotaAggKey(cell.Hour, cell.Client, cell.Target, cell.Model))] = cell.Requests
		} else {
			units = append(units, CostUnit{TargetModelCell: cell, At: cell.Hour})
		}
	}
	client, model := strings.TrimSpace(filter.ClientName), strings.ToLower(strings.TrimSpace(filter.Model))
	for _, event := range events {
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
		if observed[key] != count {
			return nil, fmt.Errorf("%w: raw request count %d differs from rollup count %d", ErrUsageHistoryIncomplete, observed[key], count)
		}
	}
	for key, count := range observed {
		if expected[key] != count {
			return nil, fmt.Errorf("%w: raw event lacks matching quota rollup", ErrUsageHistoryIncomplete)
		}
	}
	return units, nil
}

func eventAggCell(e usage.Event) AggCell {
	c := AggCell{Requests: 1, InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CachedTokens: e.CachedTokens, CacheCreationTokens: e.CacheCreationTokens,
		ExplicitCacheReadTokens: e.ExplicitCacheReadTokens, ReasoningTokens: e.ReasoningTokens, CacheCreation5mTokens: e.CacheCreation5mTokens, CacheCreation1hTokens: e.CacheCreation1hTokens, ToolUsePromptTokens: e.ToolUsePromptTokens,
		ImageInputTokens: e.ImageInputTokens, CachedImageInputTokens: e.CachedImageInputTokens, ImageOutputTokens: e.ImageOutputTokens}
	if isSuccessStatus(e.StatusCode) {
		c.Success = 1
	}
	if e.UsageStatus == usage.UsageStatusIncomplete || e.UsageStatus == usage.UsageStatusConflict {
		c.IncompleteUsageCount = 1
	}
	if e.CostIncomplete {
		c.IncompleteCostCount = 1
	}
	return c
}

func (s *UsageStore) readCostUnits(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool, rawFrom *time.Time, hooks costReadHooks) (costReadResult, error) {
	return s.readCostUnitsInScope(ctx, filter, resolver, precise, rawFrom, hooks, nil)
}

func (s *UsageStore) readCostUnitsInScope(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, precise bool, rawFrom *time.Time, hooks costReadHooks, allow costPairFilter) (costReadResult, error) {
	if err := ctx.Err(); err != nil {
		return costReadResult{}, err
	}
	from, to := costBounds(filter)
	needsExact := usage.RequiresEventTimestamps(resolver)
	// Preserve the legacy exact-cost default (now), while reports and stable
	// rollups retain their default inclusive hour bound (now + one hour).
	if precise && needsExact && filter.To == nil {
		to = time.Now().UTC()
	}
	filter.From, filter.To = &from, &to
	if (precise && needsExact && to.Before(from)) || to.Truncate(time.Hour).Before(from.Truncate(time.Hour)) {
		return costReadResult{}, nil
	}
	start, end := from.Truncate(time.Hour), to.Truncate(time.Hour).Add(time.Hour)
	result := costReadResult{}
	useIndex := needsExact && strings.TrimSpace(filter.ClientName) != "" && !from.IsZero() && !hooks.forceLegacy
	var err error
	if useIndex {
		if hooks.scan != nil {
			hooks.scan(true)
		}
		_, err = s.readClientEvents(ctx, filter.ClientName, start, end, 0, clientIndexReadHooks{decoded: hooks.decoded, discardEvents: true, completeHour: func(tx *bolt.Tx, hour time.Time, events []usage.Event) error {
			cells, err := readCostRollups(tx, filter, hour, hour, true, allow)
			if err != nil {
				return err
			}
			units, err := assembleCostUnits(cells, events, filter, resolver, precise, allow)
			if err != nil {
				return err
			}
			result.units = append(result.units, units...)
			if rawFrom != nil {
				for _, e := range events {
					if !e.Timestamp.Before(*rawFrom) && !e.Timestamp.After(to) && !usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
						result.raw = append(result.raw, e)
					}
				}
			}
			return nil
		}})
		if err != nil {
			if ctx.Err() != nil {
				return costReadResult{}, ctx.Err()
			}
			// Do not disclose event bytes/keys. Legacy parity still fails for the
			// affected owner, while unrelated quarantine does not block everyone.
			slog.Warn("exact usage pricing: client index unavailable; falling back to legacy scan", "error", err)
			result = costReadResult{}
		}
	}
	if !useIndex || err != nil {
		err = s.db.View(func(tx *bolt.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			cells, err := readCostRollups(tx, filter, from, to, needsExact, allow)
			if err != nil {
				return err
			}
			// Scan even if no exact rollup exists: an orphan precise-price raw
			// event must fail the reverse parity check, as on the legacy path.
			wantEvents := needsExact
			var events []usage.Event
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
						if wantEvents && allow.includes(e.Target, e.Model) && usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
							events = append(events, e)
						}
						if rawFrom != nil && !e.Timestamp.Before(*rawFrom) && !e.Timestamp.After(to) && !usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
							result.raw = append(result.raw, e)
						}
					}
				}
			}
			result.units, err = assembleCostUnits(cells, events, filter, resolver, precise, allow)
			return err
		})
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
			u.CostUSD = aggCellCost(u.AggCell, u.Target, u.At, u.Model, resolver)
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
