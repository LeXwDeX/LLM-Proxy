package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func cursorCostFixture(t *testing.T) (*bolt.DB, *UsageStore, *pricing.Resolver, time.Time) {
	t.Helper()
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{
		{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000},
		{Timestamp: day.Add(9*time.Hour + 45*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000},
	})
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}}, nil)
	return db, NewUsageStore(db), r, day
}

func TestPartialClientIndexMigrationStillAllowsExactCost(t *testing.T) {
	db, store, r, day := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("migration was not stopped after a committed batch: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if meta.Get([]byte(usageClientIndexCursor)) == nil || meta.Get([]byte(usageClientIndexDone)) != nil {
			t.Fatal("fixture lacks partial migration cursor")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	for _, client := range []string{"alice", ""} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true)
		if err != nil || costUnitTotal(units) != 21 {
			t.Errorf("client=%q cost=%v err=%v", client, costUnitTotal(units), err)
		}
	}
}

func TestFixedPriceSummaryRejectsChangedIndexedBoundaryHour(t *testing.T) {
	db, store, _, day := cursorCostFixture(t)
	migrateClientIndex(t, db)
	r := usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}
	now := day.Add(10*time.Hour + 30*time.Minute)
	before, err := store.summaryRead(now, r, costReadHooks{})
	if err != nil || before.LastHour.EstimatedCost != 2 {
		t.Fatalf("before=%+v err=%v", before.LastHour, err)
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
			value, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := b.Put(append([]byte(nil), k...), value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.summaryRead(now, r, costReadHooks{})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || got.LastHour.EstimatedCost != 0 {
		t.Fatalf("changed raw accepted: lastHour=%v err=%v", got.LastHour.EstimatedCost, err)
	}
}

func TestPartialMigrationIncludesConcurrentAtomicWrites(t *testing.T) {
	db, store, r, day := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := store.Record(usage.Event{Timestamp: day.Add(9*time.Hour + 50*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	for _, client := range []string{"alice", ""} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true)
		if err != nil || costUnitTotal(units) != 31 {
			t.Errorf("client=%q cost=%v err=%v", client, costUnitTotal(units), err)
		}
	}
}

func TestPartialMigrationIncludesAtomicLegacyReindexAfterCursor(t *testing.T) {
	db, store, r, day := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		c.First()
		key, value := c.Next()
		return ReindexUsageEvent(tx, key, value, value)
	}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(units) != 21 {
		t.Fatalf("atomic legacy reindex cost=%v err=%v", costUnitTotal(units), err)
	}
}

func TestMigrationCursorRewindIndistinguishableFromAtomicReindex(t *testing.T) {
	db1, _, _, _ := cursorCostFixture(t)
	db2, _, _, _ := cursorCostFixture(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	if err := migrateUsageClientIndex(ctx1, db1, 1, cancel1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := db1.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		c.First()
		key, value := c.Next()
		return ReindexUsageEvent(tx, key, value, value)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db2.Update(bumpRawRevision); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := migrateUsageClientIndex(ctx2, db2, 2, cancel2); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := db2.Update(func(tx *bolt.Tx) error {
		first, _ := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexCursor), first)
	}); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours, BucketUsageAggHourly, BucketQuotaUsageAggHourly, BucketMeta} {
		a, b := indexBucketSnapshot(t, db1, bucket), indexBucketSnapshot(t, db2, bucket)
		if bucket == BucketMeta {
			delete(a, "request_stats_since")
			delete(b, "request_stats_since")
			// Per-database random key material is not migration state.
			delete(a, string(keyIDSecretKey))
			delete(b, string(keyIDSecretKey))
			// Compare all v1-visible metadata; v2 intentionally distinguishes these states.
			delete(a, indexProgressV2)
			delete(b, indexProgressV2)
			delete(a, indexRequiredV2)
			delete(b, indexRequiredV2)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("fixture snapshots differ in %s: %v vs %v", bucket, a, b)
		}
	}
	for i, db := range []*bolt.DB{db1, db2} {
		if err := db.View(func(tx *bolt.Tx) error {
			var p indexProgress
			if err := json.Unmarshal(tx.Bucket([]byte(BucketMeta)).Get([]byte(indexProgressV2)), &p); err != nil {
				return err
			}
			c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			first, _ := c.First()
			second, _ := c.Next()
			var proof indexProof
			if err := json.Unmarshal(tx.Bucket([]byte(indexProofsV2)).Get(second), &proof); err != nil {
				return err
			}
			if i == 0 && (!bytes.Equal(p.Through, first) || proof.Origin != "atomic") || i == 1 && (!bytes.Equal(p.Through, second) || proof.Origin != "scan") {
				t.Fatalf("v2 evidence does not distinguish case %d: through=%q origin=%s", i, p.Through, proof.Origin)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPartialMigrationRejectsDamagedCursorAndEvidence(t *testing.T) {
	for _, problem := range []string{"forward", "malformed", "missing-cursor-record", "bad-hour", "removed-index", "lost-completion"} {
		t.Run(problem, func(t *testing.T) {
			db, store, r, day := cursorCostFixture(t)
			if problem == "lost-completion" {
				migrateClientIndex(t, db)
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
				first, _ := c.First()
				first = append([]byte(nil), first...)
				second, _ := c.Next()
				switch problem {
				case "forward":
					return meta.Put([]byte(usageClientIndexCursor), second)
				case "malformed":
					return meta.Put([]byte(usageClientIndexCursor), []byte("bad"))
				case "missing-cursor-record":
					return meta.Put([]byte(usageClientIndexCursor), []byte(usageKey(day.Add(9*time.Hour+20*time.Minute), "missing")))
				case "bad-hour":
					return tx.Bucket([]byte(BucketUsageClientHours)).Put(clientHourKey("alice", day.Add(9*time.Hour)), []byte("{bad"))
				case "removed-index":
					index := tx.Bucket([]byte(BucketUsageEventsByClient))
					return index.Delete(clientEventIndexKey(usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice"}, first))
				default:
					return meta.Delete([]byte(usageClientIndexDone))
				}
			}); err != nil {
				t.Fatal(err)
			}
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", ""} {
				got, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got) != 0 {
					t.Errorf("client=%q accepted %s: units=%+v err=%v", client, problem, got, err)
				}
			}
		})
	}
}
