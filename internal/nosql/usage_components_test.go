package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

func iterationBundle() usage.Event {
	return usage.Event{RecordID: uuid.NewString(), Timestamp: time.Date(2026, 10, 21, 9, 45, 0, 0, time.UTC),
		ClientName: "alice", Target: "anthropic", Model: "executor", EndpointType: "claude", RequestID: "client-chosen-id", Path: "/v1/messages", StatusCode: 200,
		InputTokens: 1_000_000, Components: []usage.Component{
			{Model: "advisor", IterationType: "advisor", IterationIndex: 2, InputTokens: 2_000_000},
			{Model: "advisor", IterationType: "advisor", IterationIndex: 4, InputTokens: 3_000_000, NonBillable: true},
		}}
}

type exactBundleRates struct{ usage.CostTable }

func (exactBundleRates) RequiresEventTimestamps() bool { return true }

func TestUsageBundleExactAndHourlyReportsAgree(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := iterationBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	rates := usage.CostTable{"executor": {InputPer1MTokens: 2}, "advisor": {InputPer1MTokens: 5}}
	for _, resolver := range []usage.RateResolver{rates, exactBundleRates{rates}, usage.WithCoefficient(rates, func(string, string) float64 { return .5 })} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, resolver, true)
		if err != nil {
			t.Fatal(err)
		}
		var cost float64
		var requests, tokens int64
		for _, unit := range units {
			cost += unit.CostUSD
			requests += unit.Requests
			tokens += unit.InputTokens
		}
		want := 12.0
		if _, ok := resolver.(exactBundleRates); !ok {
			if _, ok := resolver.(usage.CostTable); !ok {
				want = 6
			}
		}
		if cost != want || requests != 1 || tokens != 6_000_000 {
			t.Fatalf("units=%+v, cost=%v requests=%v tokens=%v", units, cost, requests, tokens)
		}
	}
}

func TestUsageBundleConflictRollsBackWholeBatch(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "Record", true: "RecordBatch"}[batch], func(t *testing.T) {
			store := NewUsageStore(testDB(t))
			event := iterationBundle()
			conflict := usage.ExpandEvent(event)[1]
			conflict.InputTokens++
			if err := store.Record(conflict); err != nil {
				t.Fatal(err)
			}
			var err error
			if batch {
				other := event
				other.RecordID = uuid.NewString()
				other.Components = nil
				err = store.RecordBatch([]usage.Event{other, event})
			} else {
				err = store.Record(event)
			}
			if err == nil {
				t.Fatal("conflicting child accepted")
			}
			rows, err := store.List(usage.Filter{})
			if err != nil || len(rows) != 1 || rows[0].RecordID != conflict.RecordID {
				t.Fatalf("partial commit: rows=%+v err=%v", rows, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			total, success, err := store.Count(from, to)
			if err != nil || total != 0 || success != 0 {
				t.Fatalf("rolled-back root changed counts: %d/%d %v", total, success, err)
			}
		})
	}
}

func TestUsageBundleRejectsChangedPayloadAndDuplicateIndexes(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := iterationBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	changed := usage.CloneEvent(event)
	changed.Components[0].OutputTokens++
	if err := store.Record(changed); err == nil {
		t.Fatal("changed child accepted under existing root identity")
	}
	invalid := usage.CloneEvent(event)
	invalid.RecordID = uuid.NewString()
	invalid.Components[1].IterationIndex = invalid.Components[0].IterationIndex
	if err := store.Record(invalid); err == nil {
		t.Fatal("duplicate component indexes accepted")
	}
	rows, err := store.List(usage.Filter{})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

func TestUsageBundleMixedLegacyRollupUsesRecordParityAndHistoricalPrice(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := iterationBundle()
	legacy := usage.Event{RecordID: uuid.NewString(), Timestamp: event.Timestamp.Add(-35 * time.Minute), ClientName: "alice", Target: "anthropic", Model: "advisor", InputTokens: 1_000_000, StatusCode: 200}
	if err := store.Record(legacy); err != nil {
		t.Fatal(err)
	}
	// Simulate an existing database whose cells predate record_count.
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
			bucket := tx.Bucket([]byte(name))
			c := bucket.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var cell map[string]any
				if err := json.Unmarshal(v, &cell); err != nil {
					return err
				}
				delete(cell, "record_count")
				data, err := json.Marshal(cell)
				if err != nil {
					return err
				}
				if err := bucket.Put(k, data); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour)
	resolver := pricing.NewResolver([]pricing.PricingTemplate{
		{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
		{Target: "anthropic", Model: "advisor", Start: from.Add(-time.Hour), Mode: pricing.ModeSchedule, Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 5}, Periods: []pricing.PricingPeriod{{Weekdays: []int{3}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}},
	}, nil)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to, ClientName: "alice"}, resolver, true)
	if err != nil {
		t.Fatal(err)
	}
	var cost float64
	var requests int64
	for _, u := range units {
		cost += u.CostUSD
		requests += u.Requests
	}
	if cost != 27 || requests != 2 {
		t.Fatalf("cost=%v requests=%d units=%+v", cost, requests, units)
	}
	child := usage.ExpandEvent(event)[1]
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Delete([]byte(usageKey(child.Timestamp, child.RecordID)))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to, ClientName: "alice"}, resolver, true); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing zero-request child history silently accepted: %v", err)
	}
}

func TestNonBillableRollupKeepsObservedPartitionsAndBillsOnlyPaidRows(t *testing.T) {
	store := NewUsageStore(testDB(t))
	at := iterationBundle().Timestamp
	paid := usage.Event{Timestamp: at, Model: "m", ClientName: "alice", InputTokens: 1000, OutputTokens: 500, CachedTokens: 400, CacheCreationTokens: 300,
		ExplicitCacheReadTokens: 50, CacheCreation5mTokens: 100, CacheCreation1hTokens: 100, ImageInputTokens: 100, CachedImageInputTokens: 50, ImageOutputTokens: 100}
	free := paid
	free.NonBillable = true
	free.InputTokens = 10000
	free.ImageInputTokens = 10000
	if err := store.RecordBatch([]usage.Event{paid, free}); err != nil {
		t.Fatal(err)
	}
	ptr := func(n float64) *float64 { return &n }
	rates := usage.CostTable{"m": {InputPer1MTokens: 1, OutputPer1MTokens: 2, CacheReadPer1MToken: 3, CacheWritePer1MToken: 4,
		ExplicitCacheReadPer1MToken: ptr(5), CacheWrite5mPer1MToken: ptr(6), CacheWrite1hPer1MToken: ptr(7), ImageInputPer1MTokens: ptr(8), CachedImageInputPer1MTokens: ptr(9), ImageOutputPer1MTokens: ptr(10)}}
	from, to := at.Truncate(time.Hour), at.Add(time.Hour)
	for _, resolver := range []usage.RateResolver{rates, exactBundleRates{rates}} {
		agg, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", resolver)
		if err != nil {
			t.Fatal(err)
		}
		if agg.Totals.EstimatedCost != usage.EstimateEventCost(paid, rates) || agg.Totals.InputTokens != 11000 || agg.Totals.ImageInputTokens != 10100 || agg.Totals.CachedTokens != 800 || agg.Totals.CacheCreationTokens != 600 {
			t.Fatalf("nonbillable partition aggregation=%+v", agg.Totals)
		}
	}
}

func TestUsageBundleConditionalPricingUsesEachModelsContext(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := iterationBundle()
	event.PricingContext = &usage.PricingContext{Version: 1, TotalInputTokens: 200, TotalInputKnown: true, TotalInputValid: true, ActualSpeed: "fast"}
	event.Components[0].PricingContext = &usage.PricingContext{Version: 1, TotalInputTokens: 20, TotalInputKnown: true, TotalInputValid: true}
	threshold := int64(100)
	resolver := pricing.NewResolver([]pricing.PricingTemplate{
		{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}, Conditions: []pricing.PriceCondition{{ID: "executor-long", MinInputTokens: &threshold, Rates: usage.CostRates{InputPer1MTokens: 2}}}},
		{Target: "anthropic", Model: "advisor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}, Conditions: []pricing.PriceCondition{{ID: "advisor-long", MinInputTokens: &threshold, Rates: usage.CostRates{InputPer1MTokens: 50}}}},
	}, nil)
	usage.CheckBundlePrices(&event, resolver)
	if !strings.HasSuffix(event.PricingRuleID, "/executor-long") || strings.HasSuffix(event.Components[0].PricingRuleID, "/advisor-long") || event.Components[0].CostIncomplete || event.Components[1].CostIncomplete {
		t.Fatalf("independent pricing evidence=%+v", event)
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, resolver, true)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, unit := range units {
		total += unit.CostUSD
	}
	if total != 12 {
		t.Fatalf("executor context leaked into advisor price: %v", total)
	}
}

func TestUsageBundleStoredAndPricedOnce(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := iterationBundle()
	for i := 0; i < 2; i++ {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.List(usage.Filter{})
	if err != nil || len(rows) != 3 {
		t.Fatalf("bundle rows = %d, err = %v, want root and two iterations", len(rows), err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	total, success, err := store.Count(from, to)
	if err != nil || total != 1 || success != 1 {
		t.Fatalf("request counts = %d/%d, err = %v", total, success, err)
	}
	agg, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", usage.CostTable{"executor": {InputPer1MTokens: 2}, "advisor": {InputPer1MTokens: 5}})
	if err != nil || agg.Totals.Requests != 1 || agg.Totals.InputTokens != 6_000_000 || agg.Totals.EstimatedCost != 12 || len(agg.ByModel) != 2 {
		t.Fatalf("aggregate = %+v, err = %v", agg, err)
	}
}

func TestUsageBundleClientRequestIDDoesNotDeduplicateSeparateRequests(t *testing.T) {
	store := NewUsageStore(testDB(t))
	event := iterationBundle()
	event.RecordID = ""
	if err := store.RecordBatch([]usage.Event{event, event}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(usage.Filter{})
	if err != nil || len(rows) != 6 {
		t.Fatalf("separate requests sharing RequestID: rows=%d err=%v", len(rows), err)
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		if seen[row.RecordID] {
			t.Fatal("separate requests collided in a component identity")
		}
		seen[row.RecordID] = true
	}
}

func TestUsageBundleQuarantinedRootCannotBeAcceptedAsNewByJournal(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := iterationBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(event.Timestamp, event.RecordID))
		bucket := tx.Bucket([]byte(BucketUsageEvents))
		quarantine, err := tx.CreateBucketIfNotExists([]byte(BucketUsageEventsQuarantine))
		if err != nil {
			return err
		}
		if err := quarantine.Put(key, bucket.Get(key)); err != nil {
			return err
		}
		return bucket.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	added, err := journal.RecordOnce(event)
	if added || !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("quarantined replay newly accepted=%v err=%v", added, err)
	}
}
