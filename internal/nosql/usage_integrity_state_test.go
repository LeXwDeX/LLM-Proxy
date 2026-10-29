package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// Issue #125 phase 4: the production classification, its epoch, and the
// weakened detection semantics it deliberately accepts.

func productionIntegrityMode(t *testing.T) {
	t.Helper()
	saved := integrityForceFast.Load()
	integrityForceFast.Store(false)
	t.Cleanup(func() { integrityForceFast.Store(saved) })
	// Earlier tests (TestIndexV2AtomicProofAndRollback) roll back a
	// transaction that called the exported ReindexUsageEvent; its mark is
	// never finished, which keeps the fast path off (the fail-safe direction).
	// Clear such marks so this test starts clean.
	integrityTxMarks.Range(func(tx, _ any) bool {
		integrityFinishTx(tx.(*bolt.Tx))
		return true
	})
	if n := integrityInflight.Load(); n != 0 {
		t.Fatalf("integrity writes in flight before the test: %d", n)
	}
}

// integrityFixture records history in two hours and returns a store whose
// receipt migration already ran, so later reads do not write.
func integrityFixture(t *testing.T) (*bolt.DB, *UsageStore, usage.Event) {
	t.Helper()
	db := testDB(t)
	store := clockedStore(db)
	base := fixedPlainEvent()
	for i, offset := range []time.Duration{-3 * time.Hour, 0} {
		e := base
		e.RecordID = []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab1", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab2"}[i]
		e.Timestamp = base.Timestamp.Add(offset)
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	t.Cleanup(func() { ForgetUsageIntegrityClassification(db) })
	return db, store, base
}

func integrityWindowRead(t *testing.T, store *UsageStore, client string, at time.Time) error {
	t.Helper()
	from := at.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	_, err := store.ReadQuotaCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, exactCostTable{replayCosts}, true)
	return err
}

func classifyNow(t *testing.T, db *bolt.DB) {
	t.Helper()
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !UsageIntegrityStats(db).Complete {
		t.Fatalf("classification not installed: %+v", UsageIntegrityStats(db))
	}
}

func TestIntegrityClassificationEnablesFastReads(t *testing.T) {
	productionIntegrityMode(t)
	db, store, base := integrityFixture(t)
	before := UsageIntegrityStats(db)
	if err := integrityWindowRead(t, store, base.ClientName, base.Timestamp); err != nil {
		t.Fatal(err)
	}
	if got := UsageIntegrityStats(db); got.FastReads != before.FastReads || got.FallbackReads[integrityFallbackIncomplete] != before.FallbackReads[integrityFallbackIncomplete]+1 {
		t.Fatalf("read before classification: before=%+v after=%+v", before, got)
	}
	classifyNow(t, db)
	before = UsageIntegrityStats(db)
	if err := integrityWindowRead(t, store, base.ClientName, base.Timestamp); err != nil {
		t.Fatal(err)
	}
	if got := UsageIntegrityStats(db); got.FastReads != before.FastReads+1 {
		t.Fatalf("read after classification did not take the fast path: before=%+v after=%+v", before, got)
	}
	// A realtime write of a well-formed row keeps the classification.
	next := base
	next.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab3"
	next.Timestamp = base.Timestamp.Add(time.Minute)
	if err := store.Record(next); err != nil {
		t.Fatal(err)
	}
	if !UsageIntegrityStats(db).Complete {
		t.Fatal("a canonical realtime write invalidated the classification")
	}
	// A reader whose snapshot predates the classification must not use it.
	tx, err := db.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	classifyNow(t, db)
	if err := store.Record(func() usage.Event {
		e := base
		e.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab4"
		e.Timestamp = base.Timestamp.Add(2 * time.Minute)
		return e
	}()); err != nil {
		t.Fatal(err)
	}
	classifyNow(t, db)
	if integrityClassFor(tx) != nil {
		t.Fatal("a snapshot older than the classification used it")
	}
}

// Each maintenance write invalidates the classification (v3 item 4): the
// next read falls back to the exhaustive checks until a new build.
func TestIntegrityEpochAdvancesOnMaintenanceWrites(t *testing.T) {
	productionIntegrityMode(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name  string
		write func(t *testing.T, db *bolt.DB, store *UsageStore, base usage.Event) error
	}{
		{"prune", func(_ *testing.T, _ *bolt.DB, store *UsageStore, base usage.Event) error {
			_, err := store.PruneEventsBefore(base.Timestamp.Add(-2*time.Hour), 100)
			return err
		}},
		{"recheck", func(_ *testing.T, _ *bolt.DB, store *UsageStore, _ usage.Event) error {
			_, err := store.RecheckUsage(UsageRecheckRequest{EvidenceID: "missing", Version: "x", OperationID: "op-125", Actor: "test"})
			if err != nil {
				return nil
			}
			return nil
		}},
		{"unresolved", func(_ *testing.T, _ *bolt.DB, store *UsageStore, base usage.Event) error {
			e := base
			e.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaac1"
			original, _ := json.Marshal(e)
			return store.RegisterUnresolvedUsage(e, original)
		}},
		{"receipts", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return clockedStore(db).EnsureUsageReceipts()
		}},
		{"backfill", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			if err := BackfillUsageAgg(db); err != nil {
				return err
			}
			return BackfillQuotaUsageAgg(db)
		}},
		{"index-migration", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return MigrateUsageClientIndex(context.Background(), db, 2)
		}},
		{"index-exceptions", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return rebuildClientIndexExceptions(db)
		}},
		{"reindex", func(_ *testing.T, db *bolt.DB, _ *UsageStore, base usage.Event) error {
			return db.Update(func(tx *bolt.Tx) error {
				key := []byte(usageKey(base.Timestamp, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaab2"))
				raw := tx.Bucket([]byte(BucketUsageEvents))
				before := append([]byte(nil), raw.Get(key)...)
				return ReindexUsageEvent(tx, key, before, before)
			})
		}},
		{"json-migration", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return MigrateFromJSON(db, config.DataFiles{UsageEventsFile: filepath.Join(t.TempDir(), "absent.json")})
		}},
		{"billing", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return MigrateBillingData(db, logger)
		}},
		{"quota-v2", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return MigrateQuotaV2(db, t.TempDir(), logger)
		}},
		{"adapter", func(_ *testing.T, db *bolt.DB, _ *UsageStore, _ usage.Event) error {
			return MigrateUsageAdapterExplicit(db, logger)
		}},
		{"realtime-anomaly", func(_ *testing.T, _ *bolt.DB, store *UsageStore, base usage.Event) error {
			// json.Marshal rewrites an invalid UTF-8 client name, so the stored
			// row no longer matches the owner bound to the original name.
			e := base
			e.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaac2"
			e.Timestamp = base.Timestamp.Add(-5 * time.Hour)
			e.ClientName = "alice\xff"
			return store.Record(e)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, store, base := integrityFixture(t)
			classifyNow(t, db)
			if err := tc.write(t, db, store, base); err != nil {
				t.Logf("write: %v", err)
			}
			if integrityInflight.Load() != 0 {
				t.Fatalf("write left %d integrity marks in flight", integrityInflight.Load())
			}
			if UsageIntegrityStats(db).Complete {
				t.Fatal("classification still complete after a maintenance write")
			}
			before := UsageIntegrityStats(db)
			_ = integrityWindowRead(t, store, base.ClientName, base.Timestamp)
			after := UsageIntegrityStats(db)
			if after.FastReads != before.FastReads || after.FallbackReads[integrityFallbackEpochChanged] != before.FallbackReads[integrityFallbackEpochChanged]+1 {
				t.Fatalf("read after the write: before=%+v after=%+v", before, after)
			}
			classifyNow(t, db)
		})
	}
}

// The one weakened guarantee (design v3 item 7): an anomalous object written
// outside the window by a writer that bypasses this package (no epoch bump)
// is missed by the fast path until the next classification, while the
// exhaustive path reports it. Pinned so the trade-off stays explicit.
func TestIntegrityOutOfBandAnomalyMissedUntilReclassified(t *testing.T) {
	productionIntegrityMode(t)
	db, store, base := integrityFixture(t)
	classifyNow(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		old := base
		old.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaad1"
		old.Timestamp = base.Timestamp.Add(-3 * time.Hour)
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(old.Timestamp, old.RecordID)), []byte{0xff, '{'})
	}); err != nil {
		t.Fatal(err)
	}
	if !UsageIntegrityStats(db).Complete {
		t.Fatal("out-of-band write unexpectedly invalidated the classification")
	}
	if err := integrityWindowRead(t, store, base.ClientName, base.Timestamp); err != nil {
		t.Fatalf("fast path should miss the out-of-window anomaly until reclassified: %v", err)
	}
	ForgetUsageIntegrityClassification(db)
	if err := integrityWindowRead(t, store, base.ClientName, base.Timestamp); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("exhaustive path must report the anomaly: %v", err)
	}
	classifyNow(t, db)
	if err := integrityWindowRead(t, store, base.ClientName, base.Timestamp); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("after reclassification the fast path must report it: %v", err)
	}
}

func TestIntegrityClassificationOverflowFallsBack(t *testing.T) {
	productionIntegrityMode(t)
	db, store, base := integrityFixture(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		for i := 0; i <= integrityAnomalyLimit; i++ {
			if err := quota.Put([]byte("odd-"+time.Duration(i).String()), []byte(`{}`)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := UsageIntegrityStats(db)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	after := UsageIntegrityStats(db)
	if after.Complete || after.FallbackReads[integrityFallbackOverflow] != before.FallbackReads[integrityFallbackOverflow]+1 {
		t.Fatalf("overflow: before=%+v after=%+v", before, after)
	}
	if err := integrityWindowRead(t, store, base.ClientName, base.Timestamp); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("exhaustive read must still reject malformed keys: %v", err)
	}
}

func TestIntegrityClassificationLoopStops(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	stop := StartUsageIntegrityClassification(context.Background(), db, time.Hour, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !UsageIntegrityStats(db).Complete && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !UsageIntegrityStats(db).Complete {
		t.Fatal("background classification did not complete")
	}
	stop()
	if UsageIntegrityStats(db).Complete {
		t.Fatal("stopped classifier left its classification installed")
	}
}
