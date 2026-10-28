package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestStableCostRejectsInflatedCheaperPartitions(t *testing.T) {
	zero := float64(0)
	for _, tc := range []struct {
		name    string
		event   func(*usage.Event)
		inflate func(*AggCell)
		rates   usage.CostRates
	}{
		{"explicit-cache", func(e *usage.Event) { e.CachedTokens = 1_000_000 }, func(c *AggCell) { c.ExplicitCacheReadTokens = 1_000_000 }, usage.CostRates{CacheReadPer1MToken: 2, ExplicitCacheReadPer1MToken: &zero}},
		{"cached-image", func(e *usage.Event) { e.CachedTokens = 1_000_000 }, func(c *AggCell) { c.CachedImageInputTokens = 1_000_000 }, usage.CostRates{CacheReadPer1MToken: 2, CachedImageInputPer1MTokens: &zero}},
		{"write-5m", func(e *usage.Event) { e.CacheCreationTokens = 1_000_000 }, func(c *AggCell) { c.CacheCreation5mTokens = 1_000_000 }, usage.CostRates{CacheWritePer1MToken: 2, CacheWrite5mPer1MToken: &zero}},
		{"write-1h", func(e *usage.Event) { e.CacheCreationTokens = 1_000_000 }, func(c *AggCell) { c.CacheCreation1hTokens = 1_000_000 }, usage.CostRates{CacheWritePer1MToken: 2, CacheWrite1hPer1MToken: &zero}},
		{"image-input", func(e *usage.Event) { e.InputTokens = 1_000_000 }, func(c *AggCell) { c.ImageInputTokens = 1_000_000 }, usage.CostRates{InputPer1MTokens: 2, ImageInputPer1MTokens: &zero}},
		{"image-output", func(e *usage.Event) { e.OutputTokens = 1_000_000 }, func(c *AggCell) { c.ImageOutputTokens = 1_000_000 }, usage.CostRates{OutputPer1MTokens: 2, ImageOutputPer1MTokens: &zero}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			e := usage.Event{Timestamp: time.Date(2026, 9, 20, 9, 10, 0, 0, time.UTC), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "model"}
			tc.event(&e)
			if err := store.Record(e); err != nil {
				t.Fatal(err)
			}
			from, to := e.Timestamp.Truncate(time.Hour), e.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
			rates := usage.CostTable{"model": tc.rates}
			want := tc.rates.CostFor(e.InputTokens, e.OutputTokens, e.CachedTokens, e.CacheCreationTokens, e.ExplicitCacheReadTokens, e.CacheCreation5mTokens, e.CacheCreation1hTokens, e.ImageInputTokens, e.CachedImageInputTokens, e.ImageOutputTokens)
			before, err := store.ReadCostUnits(context.Background(), filter, rates, true)
			if err != nil || costUnitTotal(before) != want || want != 2 {
				t.Fatalf("baseline cost=%v want=%v err=%v", costUnitTotal(before), want, err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)
				var cell AggCell
				if err := json.Unmarshal(b.Get(key), &cell); err != nil {
					return err
				}
				tc.inflate(&cell)
				data, err := json.Marshal(cell)
				if err != nil {
					return err
				}
				return b.Put(key, data)
			}); err != nil {
				t.Fatal(err)
			}
			units, err := store.ReadCostUnits(context.Background(), filter, rates, true)
			if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
				t.Errorf("partition inflation accepted: cost=%v units=%+v err=%v", costUnitTotal(units), units, err)
			}
		})
	}
}

func TestStableCostValidFreeExplicitPartition(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	e := usage.Event{Timestamp: time.Date(2026, 9, 20, 9, 10, 0, 0, time.UTC), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "model", CachedTokens: 1_000_000, ExplicitCacheReadTokens: 1_000_000}
	if err := store.Record(e); err != nil {
		t.Fatal(err)
	}
	zero := float64(0)
	rate := usage.CostRates{CacheReadPer1MToken: 2, ExplicitCacheReadPer1MToken: &zero}
	from, to := e.Timestamp.Truncate(time.Hour), e.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, usage.CostTable{"model": rate}, true)
	want := rate.CostFor(0, 0, e.CachedTokens, 0, e.ExplicitCacheReadTokens)
	if err != nil || len(units) != 1 || costUnitTotal(units) != want || want != 0 {
		t.Fatalf("valid free cost=%v want=%v err=%v", costUnitTotal(units), want, err)
	}
}

func TestStableCostRejectsInflationWithPrunedHistory(t *testing.T) {
	zero := float64(0)
	for _, tc := range []struct {
		name     string
		retained usage.Event
		inflate  func(*AggCell)
		rates    usage.CostRates
	}{
		{"explicit", usage.Event{CachedTokens: 1_000_000}, func(c *AggCell) { c.ExplicitCacheReadTokens += 1_000_000 }, usage.CostRates{CacheReadPer1MToken: 2, ExplicitCacheReadPer1MToken: &zero}},
		{"cached-image", usage.Event{CachedTokens: 1_000_000}, func(c *AggCell) { c.CachedImageInputTokens += 1_000_000 }, usage.CostRates{CacheReadPer1MToken: 2, CachedImageInputPer1MTokens: &zero}},
		{"write-5m", usage.Event{CacheCreationTokens: 1_000_000}, func(c *AggCell) { c.CacheCreation5mTokens += 1_000_000 }, usage.CostRates{CacheWritePer1MToken: 2, CacheWrite5mPer1MToken: &zero}},
		{"write-1h", usage.Event{CacheCreationTokens: 1_000_000}, func(c *AggCell) { c.CacheCreation1hTokens += 1_000_000 }, usage.CostRates{CacheWritePer1MToken: 2, CacheWrite1hPer1MToken: &zero}},
		{"image-input", usage.Event{InputTokens: 1_000_000}, func(c *AggCell) { c.ImageInputTokens += 1_000_000 }, usage.CostRates{InputPer1MTokens: 2, ImageInputPer1MTokens: &zero}},
		{"image-output", usage.Event{OutputTokens: 1_000_000}, func(c *AggCell) { c.ImageOutputTokens += 1_000_000 }, usage.CostRates{OutputPer1MTokens: 2, ImageOutputPer1MTokens: &zero}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			at := time.Date(2026, 9, 20, 9, 10, 0, 0, time.UTC)
			old := usage.Event{Timestamp: at, RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "model"}
			current := tc.retained
			current.Timestamp = at.Add(2 * time.Minute)
			current.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"
			current.ClientName, current.Target, current.Model = old.ClientName, old.Target, old.Model
			for _, e := range []usage.Event{old, current} {
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := store.PruneEventsBefore(at.Add(time.Minute), 100); n != 1 || err != nil {
				t.Fatalf("prune=%d err=%v", n, err)
			}
			from, to := at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
			rates := usage.CostTable{"model": tc.rates}
			units, err := store.ReadCostUnits(context.Background(), filter, rates, true)
			want := tc.rates.CostFor(current.InputTokens, current.OutputTokens, current.CachedTokens, current.CacheCreationTokens, current.ExplicitCacheReadTokens, current.CacheCreation5mTokens, current.CacheCreation1hTokens, current.ImageInputTokens, current.CachedImageInputTokens, current.ImageOutputTokens)
			if err != nil || costUnitTotal(units) != want || want != 2 {
				t.Fatalf("baseline cost=%v want=%v err=%v", costUnitTotal(units), want, err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(at, "alice", "upstream", "model")
				var cell AggCell
				if err := json.Unmarshal(b.Get(key), &cell); err != nil {
					return err
				}
				tc.inflate(&cell)
				data, err := json.Marshal(cell)
				if err != nil {
					return err
				}
				return b.Put(key, data)
			}); err != nil {
				t.Fatal(err)
			}
			units, err = store.ReadCostUnits(context.Background(), filter, rates, true)
			if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
				t.Errorf("historical inflation accepted: units=%+v err=%v", units, err)
			}
		})
	}
}

func TestStableCostHistoricalPartitionAdditivity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second usage.Event
	}{
		{"overlap-cache", usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100}, usage.Event{CachedTokens: 100, CachedImageInputTokens: 100}},
		{"overlap-write", usage.Event{CacheCreationTokens: 100, CacheCreation5mTokens: 100}, usage.Event{CacheCreationTokens: 100, CacheCreation1hTokens: 100}},
		{"nonbillable", usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100, NonBillable: true}, usage.Event{CachedTokens: 100}},
		{"clamped-image-overlap", usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100, CachedImageInputTokens: 100}, usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100}},
		{"pruned-explicit-overlap", usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100}, usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100, CachedImageInputTokens: 100}},
		{"pruned-explicit-overlap-2", usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 100}, usage.Event{CachedTokens: 100, ExplicitCacheReadTokens: 200, CachedImageInputTokens: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			at := time.Date(2026, 9, 20, 9, 10, 0, 0, time.UTC)
			tc.first.Timestamp = at
			tc.first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
			tc.first.ClientName = "alice"
			tc.first.Target = "upstream"
			tc.first.Model = "model"
			tc.second.Timestamp = at.Add(2 * time.Minute)
			tc.second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"
			tc.second.ClientName = "alice"
			tc.second.Target = "upstream"
			tc.second.Model = "model"
			for _, e := range []usage.Event{tc.first, tc.second} {
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := store.PruneEventsBefore(at.Add(time.Minute), 100); n != 1 || err != nil {
				t.Fatalf("prune=%d err=%v", n, err)
			}
			rate := usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1, CacheReadPer1MToken: 1, CacheWritePer1MToken: 1}
			from, to := at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, usage.CostTable{"model": rate}, true)
			if err != nil || len(units) != 1 {
				t.Fatalf("healthy pruned aggregate: units=%+v err=%v", units, err)
			}
		})
	}
}
