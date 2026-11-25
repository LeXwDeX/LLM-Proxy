package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// readDisplayCostUnits is the display-only counterpart of readCostUnitsInScope
// for pricing that is aligned to whole UTC hours. It reads the hourly quota
// rollups and, when rawFrom is set, only the raw rows from that hour onward
// (Summary's exact last hour). It deliberately skips the history-integrity
// walks (unresolved usage, raw/rollup parity, receipts) that make the strict
// reader O(raw events in the window): a damaged raw bucket must not turn the
// admin overview into an error, and the strict readers behind billing, quota
// admission, export and audit are unchanged.
//
// A damaged history block (a rollup cell that cannot be decoded, a malformed
// key) is discarded: skipped with a rate-limited warning, never a failed page,
// the same way Count treats them. A missing aggregate source still fails because
// then it is unknown how much is absent. ok=false means some (target,
// model) in the window needs event timestamps; the caller then runs the strict
// reader and nothing from this attempt is used.
func (s *UsageStore) readDisplayCostUnits(ctx context.Context, filter usage.Filter, resolver usage.RateResolver, from, to time.Time, rawFrom *time.Time, allow costPairFilter, hooks costReadHooks) (costReadResult, bool, error) {
	result := costReadResult{}
	fallback := false
	var skipped int
	var firstSkipped string
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
		cells, err := readDisplayRollups(ctx, tx, filter, from, to, allow, &skipped, &firstSkipped)
		if err != nil {
			return err
		}
		units := make([]CostUnit, 0, len(cells))
		for _, cell := range cells {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !allow.includes(cell.Target, cell.Model) {
				continue
			}
			if usage.RequiresEventTimestampsFor(resolver, cell.Target, cell.Model) {
				fallback = true
				return nil
			}
			units = append(units, CostUnit{TargetModelCell: cell, At: cell.Hour})
		}
		result.units = units
		if rawFrom == nil {
			return nil
		}
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		endKey := to.Truncate(time.Hour).Add(time.Hour).Format(hourLayout)
		c := b.Cursor()
		for k, v := c.Seek([]byte(rawFrom.UTC().Truncate(time.Hour).Format(hourLayout))); k != nil && string(k) < endKey; k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if hooks.decoded != nil {
				hooks.decoded()
			}
			var e usage.Event
			if json.Unmarshal(v, &e) != nil {
				continue // same legacy bad-record semantics as the strict scan
			}
			if !e.Timestamp.Before(*rawFrom) && !e.Timestamp.After(to) && !usage.RequiresEventTimestampsFor(resolver, e.Target, e.Model) {
				result.raw = append(result.raw, e)
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return costReadResult{}, false, ctx.Err()
	}
	if err != nil {
		return costReadResult{}, false, err
	}
	if skipped > 0 {
		s.warnCountSkipped(BucketQuotaUsageAggHourly, skipped, firstSkipped)
	}
	if fallback {
		return costReadResult{}, false, nil
	}
	if err := priceCostUnits(ctx, &result, resolver); err != nil {
		return costReadResult{}, false, err
	}
	return result, true, nil
}

// readDisplayRollups is readCostRollups for display reads: rollup keys that
// cannot be attributed and cells that cannot be decoded are skipped and counted
// instead of failing the whole report.
func readDisplayRollups(ctx context.Context, tx *bolt.Tx, filter usage.Filter, from, to time.Time, allow costPairFilter, skipped *int, firstSkipped *string) ([]TargetModelCell, error) {
	var result []TargetModelCell
	b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if b == nil {
		return result, nil
	}
	skip := func(key []byte) {
		if *skipped == 0 {
			*firstSkipped = string(key)
		}
		*skipped++
	}
	end := to.Truncate(time.Hour).Format(hourLayout) + "}"
	c := b.Cursor()
	for k, v := c.Seek(hourPrefix(from)); k != nil && string(k) <= end; k, v = c.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inScope, err := quotaKeyInScope(k, filter, from, to, allow)
		if err != nil {
			skip(k)
			continue
		}
		if !inScope {
			continue
		}
		parts := strings.Split(string(k), "|")
		hour, _ := parseAggHour(parts[0])
		var cell AggCell
		if json.Unmarshal(v, &cell) != nil {
			skip(k)
			continue
		}
		result = append(result, TargetModelCell{Hour: hour, Client: quotaKeyDecode(parts[1]), Target: quotaKeyDecode(parts[2]), Model: quotaKeyDecode(parts[3]), AggCell: cell})
	}
	return result, nil
}
