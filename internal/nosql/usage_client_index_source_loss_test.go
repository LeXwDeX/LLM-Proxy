package nosql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const testClientIndexLossMarker = "usage_client_index_source_missing_v1"

func TestClientIndexSourceLossSurvivesReopenAndRebuild(t *testing.T) {
	for _, missing := range []string{"index", "hours", "both"} {
		t.Run(missing, func(t *testing.T) {
			db, _, r, day := cursorCostFixture(t)
			migrateClientIndex(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				if missing != "hours" {
					if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
						return err
					}
				}
				if missing != "index" {
					return tx.DeleteBucket([]byte(BucketUsageClientHours))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				reopened, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := reopened.View(func(tx *bolt.Tx) error {
					if string(tx.Bucket([]byte(BucketMeta)).Get([]byte(testClientIndexLossMarker))) != "1" {
						t.Error("source loss marker missing")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				store := NewUsageStore(reopened)
				from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) || len(units) != 0 {
					t.Errorf("exact after source loss: units=%+v err=%v", units, err)
				}
				if _, err := store.summaryRead(day.Add(10*time.Hour+30*time.Minute), usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}, costReadHooks{}); !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) {
					t.Errorf("last-hour after source loss: %v", err)
				}
				stable, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}, true)
				if err != nil || costUnitTotal(stable) != 3 {
					t.Errorf("stable cost=%v err=%v", costUnitTotal(stable), err)
				}
				if attempt == 0 {
					// A surviving half can prevent rebuilding, but either outcome must retain the loss marker.
					if err := MigrateUsageClientIndex(context.Background(), reopened, 1); err != nil && missing == "both" {
						t.Fatal(err)
					}
				}
				if err := reopened.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestClientIndexSourceLossMarkerWriteRollsBackOpen(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageClientHours)); err != nil {
			return err
		}
		_, err := tx.Bucket([]byte(BucketMeta)).CreateBucket([]byte(testClientIndexLossMarker))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenDB(path)
	if err == nil {
		opened.Close()
		t.Fatal("expected marker write failure")
	}
	raw, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageClientHours)) != nil || tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) == nil {
			t.Error("failed open did not roll back schema and original completion marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUninitializedLegacyIndexIsNotSourceLoss(t *testing.T) {
	db, _, r, day := cursorCostFixture(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageClientHours))
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(testClientIndexLossMarker)) != nil {
			t.Error("legacy incorrectly marked as lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	units, err := NewUsageStore(reopened).ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(units) != 21 {
		t.Fatalf("legacy cost=%v err=%v", costUnitTotal(units), err)
	}
}

func TestClientIndexSourceLossRecognizesInterruptedOrAtomicIndex(t *testing.T) {
	for _, kind := range []string{"cursor", "revision", "surviving-data"} {
		t.Run(kind, func(t *testing.T) {
			db, store, _, day := cursorCostFixture(t)
			switch kind {
			case "cursor":
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "revision":
				if err := store.Record(usage.Event{Timestamp: day.Add(time.Hour), ClientName: "bob", Model: "other", InputTokens: 1}); err != nil {
					t.Fatal(err)
				}
			case "surviving-data":
				migrateClientIndex(t, db)
				if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageClientIndexDone)) }); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketUsageEventsByClient)) }); err != nil {
				t.Fatal(err)
			}
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.View(func(tx *bolt.Tx) error {
				if string(tx.Bucket([]byte(BucketMeta)).Get([]byte(testClientIndexLossMarker))) != "1" {
					t.Errorf("%s did not retain source loss", kind)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourceLossDoesNotBlockUnrelatedStablePriceScope(t *testing.T) {
	db, _, _, day := cursorCostFixture(t)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: day.Add(8*time.Hour + 20*time.Minute), ClientName: "alice", Target: "upstream", Model: "stable", InputTokens: 3_000_000}})
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageClientHours))
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := pricing.NewResolver([]pricing.PricingTemplate{
		{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
		{Model: "stable", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
	}, nil)
	from, to := day.Add(8*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	units, err := NewUsageStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "stable", From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(units) != 3 {
		t.Fatalf("stable scoped cost=%v err=%v", costUnitTotal(units), err)
	}
}

func TestEmptyFreshDatabaseWithoutIndexBucketsIsNotSourceLoss(t *testing.T) {
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageClientHours))
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(testClientIndexLossMarker)) != nil {
			t.Error("empty fresh database marked as lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceLossBlocksEmptyExactScope(t *testing.T) {
	db, _, r, day := cursorCostFixture(t)
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageClientHours))
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	got, err := NewUsageStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "missing-client", Model: "qwen-max", From: &from, To: &to}, r, true)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) || len(got) != 0 {
		t.Fatalf("unknown source loss accepted empty exact scope: units=%+v err=%v", got, err)
	}
}
