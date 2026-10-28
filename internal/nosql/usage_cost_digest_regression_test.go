package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIndexedCostRejectsChangedLegacyRawWithEqualHourlyTotals(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000},
		{Timestamp: day.Add(9*time.Hour + 45*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000},
	}
	putLegacyClientEvents(t, db, events)
	migrateClientIndex(t, db)
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00",
		BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	before, err := store.readCostUnits(context.Background(), filter, r, true, nil, costReadHooks{})
	if err != nil || costUnitTotal(before.units) != 21 {
		t.Fatalf("original cost=%v err=%v", costUnitTotal(before.units), err)
	}
	globalFilter := filter
	globalFilter.ClientName = ""
	global, err := store.ReadCostUnits(context.Background(), globalFilter, r, true)
	if err != nil || costUnitTotal(global) != 21 {
		t.Fatalf("original global cost=%v err=%v", costUnitTotal(global), err)
	}
	summary, err := store.summaryRead(day.Add(10*time.Hour), r, costReadHooks{})
	if err != nil || summary.LastHour.EstimatedCost != 21 {
		t.Fatalf("original summary=%+v err=%v", summary.LastHour, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var e usage.Event
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			e.InputTokens = 3_000_000 - e.InputTokens
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := b.Put(append([]byte(nil), k...), data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.readCostUnits(context.Background(), filter, r, true, nil, costReadHooks{})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
		t.Fatalf("changed legacy raw accepted: cost=%v units=%d err=%v", costUnitTotal(got.units), len(got.units), err)
	}
	global, err = store.ReadCostUnits(context.Background(), globalFilter, r, true)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(global) != 0 {
		t.Errorf("changed legacy raw accepted globally: cost=%v units=%d err=%v", costUnitTotal(global), len(global), err)
	}
	summary, err = store.summaryRead(day.Add(10*time.Hour), r, costReadHooks{})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || summary.LastHour.EstimatedCost != 0 {
		t.Errorf("changed legacy raw accepted in summary: cost=%v err=%v", summary.LastHour.EstimatedCost, err)
	}
}

func TestCostCancellationStopsQuotaKeyLoop(t *testing.T) {
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 100)
	for i := range events {
		events[i] = usage.Event{Timestamp: day.Add(10 * time.Minute), ClientName: "alice", Target: "upstream", Model: fmt.Sprintf("model-%03d", i), InputTokens: 1}
	}
	putLegacyClientEvents(t, db, events)
	to := day.Add(time.Hour - time.Nanosecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visits := 0
	allow := costPairFilter(func(_, _ string) bool {
		visits++
		cancel()
		return true
	})
	got, err := NewUsageStore(db).readCostUnitsInScope(ctx, usage.Filter{ClientName: "alice", From: &day, To: &to}, replayCosts, false, nil, costReadHooks{}, allow)
	if !errors.Is(err, context.Canceled) || len(got.units) != 0 || visits > 1 {
		t.Fatalf("err=%v units=%d visits after cancellation=%d", err, len(got.units), visits)
	}
}

func TestCostCancellationStopsDirectEvidenceScan(t *testing.T) {
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 100)
	for i := range events {
		events[i] = usage.Event{Timestamp: day.Add(time.Duration(i) * time.Second), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1}
	}
	putLegacyClientEvents(t, db, events)
	to := day.Add(time.Hour - time.Nanosecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visits := 0
	hooks := costReadHooks{direct: &directIntegrityHooks{raw: func() {
		visits++
		cancel()
	}}}
	got, err := NewUsageStore(db).readCostUnits(ctx, usage.Filter{ClientName: "alice", From: &day, To: &to}, replayCosts, false, nil, hooks)
	if !errors.Is(err, context.Canceled) || len(got.units) != 0 || visits > 1 {
		t.Fatalf("err=%v units=%d raw visits after cancellation=%d", err, len(got.units), visits)
	}
}

func TestIndexedCostFallbackRequiresEveryRelevantRawOwner(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Second)
	for _, event := range []usage.Event{first, second} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(second.Timestamp, second.RecordID))
		if err := tx.Bucket([]byte(rawOwnerBucket)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageEventsByClient)).Put(clientEventIndexKey(first, []byte(usageKey(first.Timestamp, first.RecordID))), []byte("missing"))
	}); err != nil {
		t.Fatal(err)
	}
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, costReadHooks{})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
		t.Fatalf("accepted incomplete owner proof: units=%+v err=%v", got.units, err)
	}
}

func TestIndexedCostOwnedRawMutationFailsBeforeFallback(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Second)
	second.InputTokens = first.InputTokens * 2
	for _, event := range []usage.Event{first, second} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		for _, event := range []usage.Event{first, second} {
			key := []byte(usageKey(event.Timestamp, event.RecordID))
			var current usage.Event
			if err := json.Unmarshal(b.Get(key), &current); err != nil {
				return err
			}
			current.InputTokens = 3*first.InputTokens - current.InputTokens
			value, err := json.Marshal(current)
			if err != nil {
				return err
			}
			if err := b.Put(key, value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	scans := 0
	got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, costReadHooks{scan: func(bool) { scans++ }})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 || scans != 0 {
		t.Fatalf("owned mutation accepted: units=%+v scans=%d err=%v", got.units, scans, err)
	}
}
