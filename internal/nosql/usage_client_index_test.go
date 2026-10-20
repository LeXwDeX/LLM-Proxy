package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func migrateClientIndex(t testing.TB, db *bolt.DB) {
	t.Helper()
	if err := MigrateUsageClientIndex(context.Background(), db, 2); err != nil {
		t.Fatal(err)
	}
}

func TestUsageClientIndexMigrationCompletesUnderContinuousWrites(t *testing.T) {
	db := testDB(t)
	db.MaxBatchDelay = time.Microsecond
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 12000)
	for i := range events {
		events[i] = usage.Event{Timestamp: base.Add(time.Duration(i) * time.Second), ClientName: fmt.Sprintf("client-%d", i%80), Model: "qwen-max", InputTokens: 1}
	}
	putLegacyClientEvents(t, db, events)
	// The deadline only guards against starvation. The race detector slows
	// both the writer and the migration several times over (about 3s alone
	// becomes about 7s alone and more under package-parallel load), so the
	// guard scales with it instead of measuring instrumentation overhead.
	deadline := 10 * time.Second
	if raceEnabled {
		deadline *= 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	done := make(chan error, 1)
	store := NewUsageStore(db)
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			if err := store.Record(usage.Event{Timestamp: base.Add(time.Duration(i%12000) * time.Second), ClientName: "live", Model: "qwen-max", InputTokens: 1}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	err := MigrateUsageClientIndex(ctx, db, 64)
	cancel()
	if writerErr := <-done; writerErr != nil {
		t.Fatal(writerErr)
	}
	if err != nil {
		t.Fatalf("continuous atomic writes prevented migration completion: %v", err)
	}
	if err := db.View(requireClientIndex); err != nil {
		t.Fatal(err)
	}
}

func putLegacyClientEvents(t testing.TB, db *bolt.DB, events []usage.Event) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		for i, event := range events {
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			// Keep variable-width legacy fractions and non-UUID suffixes.
			if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, fmt.Sprint(i))), data); err != nil {
				return err
			}
			if err := bumpAgg(tx, event); err != nil {
				return err
			}
			if err := bumpQuotaAgg(tx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func indexBucketSnapshot(t testing.TB, db *bolt.DB, name string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error { result[string(k)] = string(v); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestUsageClientIndexRecordAtomic(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := usage.Event{Timestamp: time.Date(2026, 10, 12, 23, 59, 59, 120000000, time.UTC), ClientName: " Alice|张三 ", Model: "qwen-max", InputTokens: 7, RecordID: "a5d23e1e-3223-4bd3-85e4-69783465ca21"}
	for i := 0; i < 2; i++ {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.View(func(tx *bolt.Tx) error {
		index := tx.Bucket([]byte("usage_events_by_client_v1"))
		if index == nil {
			t.Fatal("Record did not create/maintain the client index")
		}
		if index.Stats().KeyN != 1 || tx.Bucket([]byte(BucketUsageEvents)).Stats().KeyN != 1 {
			t.Fatal("replay must retain exactly one raw event and index entry")
		}
		_, rawKey := index.Cursor().First()
		if tx.Bucket([]byte(BucketUsageEvents)).Get(rawKey) == nil {
			t.Fatal("index does not reference its original raw event")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		for _, raw := range indexBucketSnapshot(t, db, bucket) {
			var cell AggCell
			if err := json.Unmarshal([]byte(raw), &cell); err != nil || cell.Requests != 1 || cell.InputTokens != 7 {
				t.Fatalf("replay changed %s: %s", bucket, raw)
			}
		}
	}
}

func TestUsageClientIndexRecordRollback(t *testing.T) {
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketUsageClientHours)) }); err != nil {
		t.Fatal(err)
	}
	if err := NewUsageStore(db).Record(usage.Event{ClientName: "alice", Model: "qwen-max", InputTokens: 10}); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("expected index maintenance failure, got %v", err)
	}
	for _, name := range []string{BucketUsageEvents, BucketUsageAggHourly, BucketQuotaUsageAggHourly, BucketUsageEventsByClient} {
		if len(indexBucketSnapshot(t, db, name)) != 0 {
			t.Fatalf("%s did not roll back", name)
		}
	}
}

func TestUsageClientIndexMigrationResumeReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	base := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: base, ClientName: " Alice ", Model: "qwen-max", InputTokens: 1},
		{Timestamp: base.Add(120 * time.Millisecond), ClientName: "Alice", Model: "glm-flash", InputTokens: 2},
		{Timestamp: base.Add(123 * time.Millisecond), ClientName: "Alice", Model: "qwen-max", InputTokens: 3},
		{Timestamp: base.Add(time.Second), ClientName: "bob", Model: "qwen-max", InputTokens: 4},
		{Timestamp: base.Add(time.Second), ClientName: "Alice", Model: "glm-flash", InputTokens: 5},
	}
	putLegacyClientEvents(t, db, events)
	rawBefore := indexBucketSnapshot(t, db, BucketUsageEvents)
	aggBefore := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	quotaBefore := indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)
	ctx, cancel := context.WithCancel(context.Background())
	err = migrateUsageClientIndex(ctx, db, 2, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption: %v", err)
	}
	if len(indexBucketSnapshot(t, db, BucketUsageEventsByClient)) != 2 {
		t.Fatal("first bounded batch was not committed")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) != nil {
			t.Fatal("premature completion marker")
		}
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexCursor)) == nil {
			t.Fatal("missing durable resume cursor")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := NewUsageStore(db).ReadClientEvents(context.Background(), "Alice", base, base.Add(time.Hour), 1); !errors.Is(err, ErrUsageClientIndexIncomplete) || got != nil {
		t.Fatalf("partial migration returned a successful partial history: %v %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	indexBefore := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
	stateBefore := indexBucketSnapshot(t, db, BucketUsageClientHours)
	migrateClientIndex(t, db)
	for name, before := range map[string]map[string]string{BucketUsageEvents: rawBefore, BucketUsageAggHourly: aggBefore, BucketQuotaUsageAggHourly: quotaBefore, BucketUsageEventsByClient: indexBefore, BucketUsageClientHours: stateBefore} {
		if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, name)) {
			t.Fatalf("migration changed %s", name)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewUsageStore(db).ReadClientEvents(context.Background(), " Alice ", base, base.Add(time.Second+time.Nanosecond), 1)
	if err != nil || len(got) != 4 {
		t.Fatalf("reopened read: %d %v", len(got), err)
	}
	for i, want := range []int64{1, 2, 3, 5} {
		if got[i].InputTokens != want {
			t.Fatalf("legacy fractional ordering: %+v", got)
		}
	}
}

func TestUsageClientIndexBoundariesAndDecodeIsolation(t *testing.T) {
	db := testDB(t)
	base := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC) // month/day transition
	var events []usage.Event
	for i := 0; i < 30; i++ {
		events = append(events, usage.Event{Timestamp: base.Add(time.Duration(i) * 100 * time.Millisecond), ClientName: "bob", InputTokens: 1000})
	}
	for _, offset := range []time.Duration{0, 120 * time.Millisecond, 123 * time.Millisecond, time.Second, 2 * time.Second} {
		events = append(events, usage.Event{Timestamp: base.Add(offset), ClientName: "A|张三:%", InputTokens: int64(offset)})
	}
	putLegacyClientEvents(t, db, events)
	migrateClientIndex(t, db)
	// Corruption in another client must not be decoded or break A's read.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(events[0].Timestamp, "0")), []byte("bad JSON"))
	}); err != nil {
		t.Fatal(err)
	}
	decoded := 0
	got, err := NewUsageStore(db).readClientEvents(context.Background(), " A|张三:% ", base.Add(120*time.Millisecond), base.Add(2*time.Second), 1, clientIndexReadHooks{decoded: func() { decoded++ }})
	if err != nil || len(got) != 3 {
		t.Fatalf("[from,to) read: %+v %v", got, err)
	}
	if decoded != 5 {
		t.Fatalf("decoded %d events, want exactly the 5 events of A's intersecting hours", decoded)
	}
	got, err = NewUsageStore(db).ReadClientEvents(context.Background(), "a|张三:%", base, base.Add(time.Hour), 1)
	if err != nil || len(got) != 0 {
		t.Fatalf("client names must stay case-sensitive: %+v %v", got, err)
	}
}

func TestUsageClientIndexMissingAndCorruptFailClosed(t *testing.T) {
	for _, damage := range []string{"unmarked", "missing-index", "redirected-index", "missing-raw", "modified-raw", "corrupt-state"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			at := time.Date(2026, 10, 1, 1, 1, 1, 0, time.UTC)
			putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice", InputTokens: 100}})
			migrateClientIndex(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				idx := tx.Bucket([]byte(BucketUsageEventsByClient))
				k, v := idx.Cursor().First()
				switch damage {
				case "unmarked":
					return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageClientIndexDone))
				case "missing-index":
					return idx.Delete(k)
				case "redirected-index":
					return idx.Put(k, []byte("nonexistent"))
				case "missing-raw":
					return tx.Bucket([]byte(BucketUsageEvents)).Delete(v)
				case "modified-raw":
					data, _ := json.Marshal(usage.Event{Timestamp: at, ClientName: "alice", InputTokens: 1})
					return tx.Bucket([]byte(BucketUsageEvents)).Put(v, data)
				case "corrupt-state":
					return tx.Bucket([]byte(BucketUsageClientHours)).Put(clientHourKey("alice", at), []byte("bad JSON"))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := NewUsageStore(db).ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1)
			if got != nil || !errors.Is(err, ErrUsageClientIndexIncomplete) {
				t.Fatalf("damage returned successful low history: %+v %v", got, err)
			}
		})
	}
}

func TestUsageClientIndexPrune(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice"}, {Timestamp: at.Add(2 * time.Second), ClientName: "alice"}, {Timestamp: at.Add(time.Hour), ClientName: "alice"}})
	migrateClientIndex(t, db)
	aggBefore := indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)
	store := NewUsageStore(db)
	n, err := store.PruneEventsBefore(at.Add(3*time.Second), 1)
	if err != nil || n != 1 {
		t.Fatalf("existing conservative pruning boundary: %d %v", n, err)
	}
	if len(indexBucketSnapshot(t, db, BucketUsageEventsByClient)) != 2 {
		t.Fatal("prune left stale index")
	}
	got, err := store.ReadClientEvents(context.Background(), "alice", at, at.Add(2*time.Hour), 1)
	if err != nil || len(got) != 2 {
		t.Fatalf("retained event query: %+v %v", got, err)
	}
	if !reflect.DeepEqual(aggBefore, indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)) {
		t.Fatal("prune changed permanent rollups")
	}
	if err := verifyUsageClientIndex(context.Background(), db, 1); err != nil {
		t.Fatal(err)
	}
}

func TestUsageClientIndexPruneQuarantinesCorruptionAndContinues(t *testing.T) {
	for _, batchSize := range []int{1, 2} {
		t.Run(fmt.Sprint(batchSize), func(t *testing.T) {
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			db := testDB(t)
			at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
			putLegacyClientEvents(t, db, []usage.Event{
				{Timestamp: at, ClientName: "alice", InputTokens: 1},
				{Timestamp: at.Add(time.Second), ClientName: "alice", InputTokens: 2},
				{Timestamp: at.Add(2 * time.Second), ClientName: "bob", InputTokens: 3},
			})
			migrateClientIndex(t, db)
			before := map[string]map[string]string{
				BucketUsageAggHourly:      indexBucketSnapshot(t, db, BucketUsageAggHourly),
				BucketQuotaUsageAggHourly: indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly),
			}
			badKey := []byte(usageKey(at.Add(time.Second), "1"))
			badBytes := []byte(`{"broken":"sensitive fixture body`)
			if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(badKey, badBytes) }); err != nil {
				t.Fatal(err)
			}
			store := NewUsageStore(db)
			n, err := store.PruneEventsBefore(at.Add(time.Hour), batchSize)
			if err != nil || n != 3 {
				t.Fatalf("corruption stalled retention: removed=%d err=%v", n, err)
			}
			if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "sensitive fixture body") {
				t.Fatalf("quarantine warning is absent or contains raw content: %s", logs.String())
			}
			if len(indexBucketSnapshot(t, db, BucketUsageEvents)) != 0 {
				t.Fatal("other client's expired event was not pruned")
			}
			var saved struct {
				Raw    []byte    `json:"raw_value"`
				At     time.Time `json:"quarantined_at"`
				Reason string    `json:"reason"`
			}
			if err := db.View(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte("usage_events_quarantine_v1"))
				if bucket == nil {
					t.Fatal("missing quarantine bucket")
				}
				if err := json.Unmarshal(bucket.Get(badKey), &saved); err != nil {
					return err
				}
				if !reflect.DeepEqual(saved.Raw, badBytes) || saved.At.IsZero() || saved.Reason == "" {
					t.Fatalf("quarantine lost evidence: %+v", saved)
				}
				if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) != nil {
					t.Fatal("quarantine left index marked complete")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			quarantineBefore := indexBucketSnapshot(t, db, "usage_events_quarantine_v1")
			n, err = store.PruneEventsBefore(at.Add(time.Hour), batchSize)
			if err != nil || n != 0 || !reflect.DeepEqual(quarantineBefore, indexBucketSnapshot(t, db, "usage_events_quarantine_v1")) {
				t.Fatalf("repeat prune changed quarantine: %d %v", n, err)
			}
			for bucket, snapshot := range before {
				if !reflect.DeepEqual(snapshot, indexBucketSnapshot(t, db, bucket)) {
					t.Fatalf("quarantine changed permanent %s", bucket)
				}
			}
			// Even a stale/restored completion marker must not hide quarantine.
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexDone), []byte("stale"))
			}); err != nil {
				t.Fatal(err)
			}
			for _, client := range []string{"alice", "bob"} {
				got, err := store.ReadClientEvents(context.Background(), client, at, at.Add(time.Hour), 1)
				if got != nil || !errors.Is(err, ErrUsageClientIndexIncomplete) {
					t.Fatalf("quarantine returned partial history for %s: %+v %v", client, got, err)
				}
			}
			if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
				t.Fatalf("migration ignored unresolved quarantine: %v", err)
			}
		})
	}
}

func TestUsageClientIndexQuarantinedRecordReplayDoesNotDoubleCount(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	event := usage.Event{Timestamp: at, ClientName: "alice", Model: "qwen-max", InputTokens: 10, RecordID: "14f8a512-057a-4d39-8606-e43c091b2e7b"}
	store := NewUsageStore(db)
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	aggBefore := indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(at, event.RecordID)), []byte("corrupt bytes"))
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(at.Add(time.Hour), 1); err != nil || n != 1 {
		t.Fatalf("quarantine: %d %v", n, err)
	}
	if err := store.Record(event); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("replay resurrected quarantined event: %v", err)
	}
	if !reflect.DeepEqual(aggBefore, indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)) || len(indexBucketSnapshot(t, db, BucketUsageEvents)) != 0 {
		t.Fatal("quarantined replay double-counted history")
	}
}

func TestUsageClientIndexMigrationRejectsSwappedHourStates(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice", InputTokens: 1}, {Timestamp: at.Add(time.Hour), ClientName: "bob", InputTokens: 2}})
	err := migrateUsageClientIndex(context.Background(), db, 16, func() {
		if err := db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte(BucketUsageClientHours))
			a, b := clientHourKey("alice", at), clientHourKey("bob", at.Add(time.Hour))
			av, bv := append([]byte(nil), bucket.Get(a)...), append([]byte(nil), bucket.Get(b)...)
			if err := bucket.Put(a, bv); err != nil {
				return err
			}
			return bucket.Put(b, av)
		}); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("swapped hour states passed migration: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) != nil {
			t.Fatal("swapped states published completion marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageClientIndexMigrationVerificationRejectsCorruption(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice"}})
	err := migrateUsageClientIndex(context.Background(), db, 1, func() {
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketUsageEventsByClient)).Put([]byte("orphan"), []byte("missing"))
		}); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("verification: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) != nil {
			t.Fatal("marked corrupt migration complete")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageClientIndexMigrationWithBackdatedWriteAndPrune(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	var events []usage.Event
	for i := 0; i < 6; i++ {
		events = append(events, usage.Event{Timestamp: at.Add(time.Duration(i) * time.Second), ClientName: "alice", InputTokens: 1})
	}
	putLegacyClientEvents(t, db, events)
	store := NewUsageStore(db)
	changed := false
	err := migrateUsageClientIndex(context.Background(), db, 2, func() {
		if changed {
			return
		}
		changed = true
		// Delete both indexed and not-yet-indexed history in one migration.
		n, err := store.PruneEventsBefore(at.Add(4*time.Second), 1)
		if err != nil || n != 3 {
			t.Fatalf("migration prune: %d %v", n, err)
		}
		// This key precedes the durable build cursor; Record must index it.
		if err := store.Record(usage.Event{Timestamp: at.Add(-time.Second), ClientName: "alice", InputTokens: 2}); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadClientEvents(context.Background(), "alice", at.Add(-time.Hour), at.Add(time.Hour), 1)
	if err != nil || len(got) != 4 || got[0].InputTokens != 2 {
		t.Fatalf("backdated migration history: %+v %v", got, err)
	}
}

func TestUsageClientIndexMigrationRetryAfterBadRawRecord(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice"}, {Timestamp: at.Add(time.Second), ClientName: "alice"}})
	badKey := []byte(usageKey(at.Add(2*time.Second), "bad"))
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(badKey, []byte("bad JSON")) }); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("corrupt raw should fail migration: %v", err)
	}
	// Simulate an explicit repair of the isolated fixture. The original valid
	// records and their already committed indexes must survive a retry.
	before := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Delete(badKey) }); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) {
		t.Fatal("retry duplicated previously committed records")
	}
}

func TestUsageClientIndexVerificationAllowsAtomicRevisionChange(t *testing.T) {
	db := testDB(t)
	if err := NewUsageStore(db).Record(usage.Event{ClientName: "alice"}); err != nil {
		t.Fatal(err)
	}
	visited := 0
	err := scanIndexVerification(context.Background(), db, BucketUsageEvents, 1, func(*bolt.Tx, []byte, []byte) error {
		visited++
		return nil
	})
	if err != nil || visited != 1 {
		t.Fatalf("segment scan rejected atomic revision change: visited=%d err=%v", visited, err)
	}
}

func TestUsageClientIndexInterBatchChanges(t *testing.T) {
	for _, change := range []string{"insert", "prune", "other-client", "unstable"} {
		t.Run(change, func(t *testing.T) {
			db := testDB(t)
			at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
			putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice"}, {Timestamp: at.Add(time.Second), ClientName: "alice"}})
			migrateClientIndex(t, db)
			store := NewUsageStore(db)
			calls, changes := 0, 0
			got, err := store.readClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1, clientIndexReadHooks{afterBatch: func() {
				calls++
				if changes > 0 && change != "unstable" {
					return
				}
				changes++
				if change == "prune" {
					if _, err := store.PruneEventsBefore(at.Add(2*time.Second), 1); err != nil {
						t.Fatal(err)
					}
					return
				}
				client := "alice"
				if change == "other-client" {
					client = "bob"
				}
				if err := store.Record(usage.Event{Timestamp: at.Add(-time.Nanosecond).Add(time.Duration(changes) * time.Nanosecond), ClientName: client}); err != nil {
					t.Fatal(err)
				}
			}})
			want := 3
			if change == "prune" {
				want = 1
			}
			if change == "other-client" {
				want = 2
			}
			if change == "unstable" {
				if !errors.Is(err, ErrUsageClientIndexChanged) || got != nil || changes != clientIndexRetries {
					t.Fatalf("bounded retry: %d %+v %v", changes, got, err)
				}
				return
			}
			if err != nil || len(got) != want {
				t.Fatalf("retry result: %+v %v", got, err)
			}
			if change == "other-client" && calls != 2 {
				t.Fatalf("unrelated client caused retry: %d batches", calls)
			}
		})
	}
}

func TestUsageClientIndexConcurrentReadWrite(t *testing.T) {
	db := testDB(t)
	migrateClientIndex(t, db)
	store := NewUsageStore(db)
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := store.Record(usage.Event{Timestamp: at.Add(time.Duration(i) * time.Millisecond), ClientName: "alice", InputTokens: 1}); err != nil {
				errCh <- err
			}
		}(i)
		go func() {
			defer wg.Done()
			_, err := store.ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 2)
			if err != nil && !errors.Is(err, ErrUsageClientIndexChanged) {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	got, err := store.ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 2)
	if err != nil || len(got) != 32 {
		t.Fatalf("final concurrent result: %d %v", len(got), err)
	}
}

func TestUsageClientIndexJournalReplay(t *testing.T) {
	db := testDB(t)
	migrateClientIndex(t, db)
	store := NewUsageStore(db)
	journal, err := NewUsageJournal(&commitThenFailUsageRecorder{next: store, err: errors.New("uncertain commit"), failFirst: true}, filepath.Join(t.TempDir(), "journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	if err := journal.Record(usage.Event{Timestamp: at, ClientName: "alice", InputTokens: 9}); err != nil {
		t.Fatal(err)
	}
	before := indexBucketSnapshot(t, db, BucketUsageClientHours)
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, BucketUsageClientHours)) {
		t.Fatal("journal replay duplicated client index state")
	}
	got, err := store.ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1)
	if err != nil || len(got) != 1 || got[0].InputTokens != 9 {
		t.Fatalf("replay history: %+v %v", got, err)
	}
}

func TestUsageClientIndexCancellation(t *testing.T) {
	db := testDB(t)
	migrateClientIndex(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := NewUsageStore(db).ReadClientEvents(ctx, "alice", time.Time{}, time.Now(), 1)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %+v %v", got, err)
	}
}

func TestUsageClientIndexCalendarBoundaries(t *testing.T) {
	for _, boundary := range []time.Time{
		time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), // Sunday -> Monday
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),  // month boundary
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), // day boundary
	} {
		t.Run(boundary.Format(time.RFC3339), func(t *testing.T) {
			db := testDB(t)
			putLegacyClientEvents(t, db, []usage.Event{
				{Timestamp: boundary.Add(-time.Nanosecond), ClientName: "alice", InputTokens: 1},
				{Timestamp: boundary, ClientName: "alice", InputTokens: 2},
				{Timestamp: boundary.Add(time.Nanosecond), ClientName: "alice", InputTokens: 3},
			})
			migrateClientIndex(t, db)
			store := NewUsageStore(db)
			before, err := store.ReadClientEvents(context.Background(), "alice", boundary.Add(-time.Hour), boundary, 1)
			if err != nil || len(before) != 1 || before[0].InputTokens != 1 {
				t.Fatalf("before boundary: %+v %v", before, err)
			}
			after, err := store.ReadClientEvents(context.Background(), "alice", boundary, boundary.Add(time.Hour), 1)
			if err != nil || len(after) != 2 || after[0].InputTokens != 2 || after[1].InputTokens != 3 {
				t.Fatalf("after boundary: %+v %v", after, err)
			}
		})
	}
}

func TestUsageClientIndexMissingSchemaOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice", InputTokens: 10}})
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageClientHours))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := NewUsageStore(db).ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1)
	if got != nil || !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("missing schema reused stale completion marker: %+v %v", got, err)
	}
	migrateClientIndex(t, db)
	got, err = NewUsageStore(db).ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("recovered missing derived schema: %+v %v", got, err)
	}
}

func BenchmarkUsageClientEvents(b *testing.B) {
	db := testDB(b)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const clients, perClient = 100, 1000
	events := make([]usage.Event, 0, clients*perClient)
	for c := 0; c < clients; c++ {
		for i := 0; i < perClient; i++ {
			events = append(events, usage.Event{Timestamp: at.Add(time.Duration(i) * time.Second), ClientName: fmt.Sprintf("client-%03d", c), Model: "qwen-max", InputTokens: 10})
		}
	}
	putLegacyClientEvents(b, db, events)
	if err := MigrateUsageClientIndex(context.Background(), db, 4096); err != nil {
		b.Fatal(err)
	}
	store := NewUsageStore(db)
	b.Run("legacy_full_scan", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(clients*perClient, "decoded/op")
		for i := 0; i < b.N; i++ {
			count := 0
			err := db.View(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEvents)).ForEach(func(_, data []byte) error {
					var event usage.Event
					if err := json.Unmarshal(data, &event); err != nil {
						return err
					}
					if event.ClientName == "client-000" {
						count++
					}
					return nil
				})
			})
			if err != nil || count != perClient {
				b.Fatalf("legacy: %d %v", count, err)
			}
		}
	})
	b.Run("client_index", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(perClient, "decoded/op")
		for i := 0; i < b.N; i++ {
			got, err := store.ReadClientEvents(context.Background(), "client-000", at, at.Add(time.Hour), 256)
			if err != nil || len(got) != perClient {
				b.Fatalf("index: %d %v", len(got), err)
			}
		}
	})
}
