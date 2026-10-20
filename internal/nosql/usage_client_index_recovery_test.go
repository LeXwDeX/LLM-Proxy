package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestUsageClientIndexMigrationConcurrentRecordPruneCancelRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	var events []usage.Event
	for i := 0; i < 200; i++ {
		events = append(events, usage.Event{Timestamp: at.Add(time.Duration(i) * time.Second), ClientName: fmt.Sprintf("client-%d", i%4), InputTokens: 1})
	}
	putLegacyClientEvents(t, db, events)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstBatch, resume := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	var once sync.Once
	go func() {
		finished <- migrateUsageClientIndex(ctx, db, 16, func() {
			once.Do(func() {
				close(firstBatch)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})
		})
	}()
	<-firstBatch // worker is alive; its first write transaction is committed
	store := NewUsageStore(db)
	written := make(chan error, 1)
	go func() {
		written <- store.Record(usage.Event{Timestamp: at.Add(-time.Second), ClientName: "client-0", InputTokens: 2})
	}()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	pruned := make(chan error, 1)
	go func() {
		n, err := store.PruneEventsBefore(at.Add(31*time.Second), 7)
		if err == nil && n != 31 {
			err = fmt.Errorf("pruned %d, want 31", n)
		}
		pruned <- err
	}()
	if err := <-pruned; err != nil {
		t.Fatal(err)
	}
	if err := store.Record(usage.Event{Timestamp: at.Add(1000 * time.Second), ClientName: "client-1", InputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(resume)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown cancellation: %v", err)
	}
	rawBefore := indexBucketSnapshot(t, db, BucketUsageEvents)
	if len(rawBefore) != 171 {
		t.Fatalf("concurrent raw writes/prune lost events: %d", len(rawBefore))
	}
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if meta.Get([]byte(usageClientIndexCursor)) == nil || meta.Get([]byte(usageClientIndexDone)) != nil {
			t.Fatal("shutdown did not retain incomplete durable progress")
		}
		return nil
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
	migrateClientIndex(t, db)
	if !reflect.DeepEqual(rawBefore, indexBucketSnapshot(t, db, BucketUsageEvents)) {
		t.Fatal("restart changed raw history")
	}
	for c := 0; c < 4; c++ {
		client := fmt.Sprintf("client-%d", c)
		want := 0
		for _, raw := range rawBefore {
			var event usage.Event
			if err := json.Unmarshal([]byte(raw), &event); err != nil {
				t.Fatal(err)
			}
			if event.ClientName == client {
				want++
			}
		}
		got, err := NewUsageStore(db).ReadClientEvents(context.Background(), client, at.Add(-time.Hour), at.Add(time.Hour), 5)
		if err != nil || len(got) != want {
			t.Fatalf("restarted client %s: %d want %d err=%v", client, len(got), want, err)
		}
	}
}

func TestUsageClientIndexRestoredBackupAndJournalReplay(t *testing.T) {
	dir := t.TempDir()
	source, err := OpenDB(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, source, []usage.Event{{Timestamp: at, ClientName: "alice", InputTokens: 1}})
	migrateClientIndex(t, source)
	journalPath := filepath.Join(dir, "source-journal.jsonl")
	journal, err := NewUsageJournal(&commitThenFailUsageRecorder{next: NewUsageStore(source), err: errors.New("uncertain committed fixture write"), failFirst: true}, journalPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(usage.Event{Timestamp: at.Add(time.Second), ClientName: "alice", InputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "restored.db")
	if err := source.View(func(tx *bolt.Tx) error { return tx.CopyFile(backup, 0600) }); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenDB(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, bucket := range []string{BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours, BucketUsageEventsQuarantine, BucketMeta} {
		if !reflect.DeepEqual(indexBucketSnapshot(t, source, bucket), indexBucketSnapshot(t, restored, bucket)) {
			t.Fatalf("backup lost %s", bucket)
		}
	}
	journalBytes, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	restoredJournalPath := filepath.Join(dir, "restored-journal.jsonl")
	if err := os.WriteFile(restoredJournalPath, journalBytes, 0600); err != nil {
		t.Fatal(err)
	}
	restoredJournal, err := NewUsageJournal(NewUsageStore(restored), restoredJournalPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := indexBucketSnapshot(t, restored, BucketUsageClientHours)
	if _, err := restoredJournal.Replay(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, restored, BucketUsageClientHours)) {
		t.Fatal("restored journal double-counted committed event")
	}
	got, err := NewUsageStore(restored).ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1)
	if err != nil || len(got) != 2 || got[0].InputTokens != 1 || got[1].InputTokens != 2 {
		t.Fatalf("restored reads: %+v %v", got, err)
	}
}

func TestUsageClientIndexRestoredBackupPreservesQuarantine(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: at, ClientName: "alice"}})
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(at, "0")), []byte{0xff, 0, 0xfe})
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := NewUsageStore(db).PruneEventsBefore(at.Add(time.Hour), 1); err != nil || n != 1 {
		t.Fatalf("quarantine before backup: %d %v", n, err)
	}
	backup := filepath.Join(dir, "restore.db")
	if err := db.View(func(tx *bolt.Tx) error { return tx.CopyFile(backup, 0600) }); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenDB(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !reflect.DeepEqual(indexBucketSnapshot(t, db, BucketUsageEventsQuarantine), indexBucketSnapshot(t, restored, BucketUsageEventsQuarantine)) {
		t.Fatal("backup lost quarantine evidence")
	}
	if !reflect.DeepEqual(indexBucketSnapshot(t, db, BucketMeta), indexBucketSnapshot(t, restored, BucketMeta)) {
		t.Fatal("backup lost unresolved quarantine state")
	}
	got, err := NewUsageStore(restored).ReadClientEvents(context.Background(), "alice", at, at.Add(time.Hour), 1)
	if got != nil || !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("restored quarantine returned complete history: %+v %v", got, err)
	}
	if err := MigrateUsageClientIndex(context.Background(), restored, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("restore incorrectly completed unresolved migration: %v", err)
	}
}

// Generate at most one million actual legacy raw events, in bounded writes.
// Fixture generation has a two-minute ceiling and must reach at least 200k.
// Measured migration includes build, all verification passes and completion;
// write batch metrics include bolt.Update lock acquisition and commit/fsync.
func BenchmarkUsageClientIndexMigration(b *testing.B) {
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		db := testDB(b)
		at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
		generated, fixtureStarted := 0, time.Now()
		for generated < 1_000_000 {
			if time.Since(fixtureStarted) > 2*time.Minute {
				if generated < 200_000 {
					b.Fatalf("fixture generation reached only %d events in two minutes", generated)
				}
				break
			}
			end := generated + 4096
			if end > 1_000_000 {
				end = 1_000_000
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				for i := generated; i < end; i++ {
					event := usage.Event{Timestamp: at.Add(time.Duration(i) * time.Millisecond), ClientName: fmt.Sprintf("client-%03d", i%100), Model: "qwen-max", InputTokens: 10}
					data, err := json.Marshal(event)
					if err != nil {
						return err
					}
					if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, fmt.Sprint(i))), data); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			generated = end
		}
		fixtureDuration := time.Since(fixtureStarted)
		var batches []time.Duration
		b.StartTimer()
		started := time.Now()
		err := migrateUsageClientIndexObserved(context.Background(), db, 256, func(d time.Duration) { batches = append(batches, d) })
		elapsed := time.Since(started)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		sort.Slice(batches, func(i, j int) bool { return batches[i] < batches[j] })
		b.ReportMetric(float64(generated), "events/op")
		b.ReportMetric(fixtureDuration.Seconds(), "fixture_s/op")
		b.ReportMetric(elapsed.Seconds(), "migration_s/op")
		b.ReportMetric(float64(batches[len(batches)/2])/float64(time.Millisecond), "write_batch_p50_ms")
		b.ReportMetric(float64(batches[(len(batches)-1)*95/100])/float64(time.Millisecond), "write_batch_p95_ms")
		b.ReportMetric(float64(batches[len(batches)-1])/float64(time.Millisecond), "write_batch_max_ms")
		b.ReportMetric(float64(len(batches)), "write_batches/op")
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
