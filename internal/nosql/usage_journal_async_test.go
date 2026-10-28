package nosql

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// gatedBatchRecorder blocks the asynchronous batch commit until released while
// leaving the synchronous Record path (replay, DrainClient) usable.
type gatedBatchRecorder struct {
	store   *UsageStore
	gate    chan struct{}
	batches atomic.Int64
	records atomic.Int64
	fail    atomic.Bool
}

func (r *gatedBatchRecorder) Record(event usage.Event) error {
	r.records.Add(1)
	if r.fail.Load() {
		return errors.New("primary unavailable")
	}
	return r.store.Record(event)
}

func (r *gatedBatchRecorder) RecordBatch(events []usage.Event) error {
	<-r.gate
	r.batches.Add(1)
	if r.fail.Load() {
		return errors.New("primary unavailable")
	}
	return r.store.RecordBatch(events)
}

func (r *gatedBatchRecorder) RegisterUnresolvedUsage(event usage.Event, original []byte) error {
	return r.store.RegisterUnresolvedUsage(event, original)
}

func asyncTestEvent(i int, client string) usage.Event {
	return usage.Event{
		Timestamp:    time.Date(2026, 10, 20, 1, 2, 3, i, time.UTC),
		ClientName:   client,
		EndpointType: "openai",
		Model:        "qwen-max",
		InputTokens:  int64(10 + i),
		OutputTokens: 5,
		StatusCode:   200,
		RequestID:    fmt.Sprintf("req-%d", i),
	}
}

func countStoredUsage(t *testing.T, store *UsageStore, client string) (int, int64) {
	t.Helper()
	items, err := store.List(usage.Filter{ClientName: client, Limit: 2000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.RecordID] {
			t.Fatalf("record %s stored twice", item.RecordID)
		}
		seen[item.RecordID] = true
	}
	total, _, err := store.Count(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return len(items), total
}

func journalPendingIDs(t *testing.T, path string) map[string]bool {
	t.Helper()
	probe, err := NewUsageJournal(noopUsageRecorder{}, path, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	probe.mu.Lock()
	events, corrupt, err := probe.readLocked()
	probe.mu.Unlock()
	if err != nil || len(corrupt) > 0 {
		t.Fatalf("read journal: err=%v corrupt=%d", err, len(corrupt))
	}
	ids := map[string]bool{}
	for _, event := range events {
		ids[event.RecordID] = true
	}
	return ids
}

func TestAsyncUsageJournalRecordReturnsBeforePrimaryCommit(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	event := asyncTestEvent(1, "alice")
	event.RecordID = uuid.NewString()
	returned := make(chan error, 1)
	go func() { returned <- journal.Record(event) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Record waited for the blocked primary commit")
	}
	// Durability before return: the acknowledged event is in the journal.
	if ids := journalPendingIDs(t, path); !ids[event.RecordID] {
		t.Fatalf("acknowledged event %s is not durable in the journal: %v", event.RecordID, ids)
	}
	if n, _ := countStoredUsage(t, store, "alice"); n != 0 {
		t.Fatalf("primary commit should still be pending, stored=%d", n)
	}
	close(primary.gate)
	journal.Flush()
	if n, total := countStoredUsage(t, store, "alice"); n != 1 || total != 1 {
		t.Fatalf("after flush stored=%d agg=%d, want 1/1", n, total)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ids := journalPendingIDs(t, path); len(ids) != 0 {
		t.Fatalf("committed events must be compacted out of the journal: %v", ids)
	}
}

func TestAsyncUsageJournalCloseCommitsAllQueuedEvents(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{QueueSize: 1024, MaxBatch: 16}); err != nil {
		t.Fatal(err)
	}
	const n = 300
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := journal.Record(asyncTestEvent(i, "alice")); err != nil {
				t.Errorf("Record %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	// Release commits only once Close has started draining.
	closed := make(chan error, 1)
	go func() { closed <- journal.Close() }()
	time.Sleep(20 * time.Millisecond)
	close(primary.gate)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not drain the queue")
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != n || total != n {
		t.Fatalf("after Close stored=%d agg=%d, want %d", stored, total, n)
	}
	if primary.batches.Load() >= n {
		t.Fatalf("queued events were not batched: %d commits for %d events", primary.batches.Load(), n)
	}
	if ids := journalPendingIDs(t, path); len(ids) != 0 {
		t.Fatalf("journal still holds %d committed events after Close", len(ids))
	}
	// After Close the journal keeps the synchronous durable path.
	late := asyncTestEvent(n+1, "alice")
	if err := journal.Record(late); err != nil {
		t.Fatalf("Record after Close: %v", err)
	}
	if stored, _ := countStoredUsage(t, store, "alice"); stored != n+1 {
		t.Fatalf("Record after Close stored=%d, want %d", stored, n+1)
	}
}

// A process crash between the journal append and the bbolt commit must be
// recovered by the startup replay, exactly once, including events whose
// commit already landed before the crash but were not yet compacted.
func TestAsyncUsageJournalCrashBeforeCommitReplaysExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "usage.db")
	path := filepath.Join(dir, "usage-journal.jsonl")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store := NewUsageStore(db)
	// The first committed batch lands, every later commit "crashes" (never returns).
	var commits atomic.Int64
	hang := make(chan struct{})
	primary := &crashingBatchRecorder{store: store, commitsBeforeCrash: 1, commits: &commits, hang: hang}
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{MaxBatch: 1}); err != nil {
		t.Fatal(err)
	}
	const n = 20
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		event := asyncTestEvent(i, "alice")
		event.RecordID = uuid.NewString()
		ids[event.RecordID] = true
		if err := journal.Record(event); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for commits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stored, _ := countStoredUsage(t, store, "alice"); stored != 1 {
		t.Fatalf("before crash stored=%d, want exactly the first committed event", stored)
	}
	// Crash: the writer is stuck, nothing is closed or compacted. Every
	// acknowledged event must still be in the journal file.
	pending := journalPendingIDs(t, path)
	committed, err := store.List(usage.Filter{ClientName: "alice", Limit: 2000})
	if err != nil {
		t.Fatal(err)
	}
	inStore := map[string]bool{}
	for _, item := range committed {
		inStore[item.RecordID] = true
	}
	for id := range ids {
		if !pending[id] && !inStore[id] {
			t.Fatalf("acknowledged event %s is neither committed nor in the journal at crash time", id)
		}
	}
	// Restart against the same database file (bbolt holds an exclusive lock, so
	// release it the way process exit would). The stuck writer is abandoned
	// like a dead process: it never resumes.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restartedDB, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedDB.Close()
	restartedStore := NewUsageStore(restartedDB)
	restarted, err := NewUsageJournal(restartedStore, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Replay(); err != nil {
		t.Fatalf("startup Replay: %v", err)
	}
	if stored, total := countStoredUsage(t, restartedStore, "alice"); stored != n || total != n {
		t.Fatalf("after replay stored=%d agg=%d, want %d (exactly once)", stored, total, n)
	}
	if _, err := restarted.Replay(); err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if stored, total := countStoredUsage(t, restartedStore, "alice"); stored != n || total != n {
		t.Fatalf("second replay double counted: stored=%d agg=%d", stored, total)
	}
}

type crashingBatchRecorder struct {
	store              *UsageStore
	commitsBeforeCrash int64
	commits            *atomic.Int64
	hang               chan struct{}
}

func (r *crashingBatchRecorder) Record(event usage.Event) error { return r.store.Record(event) }

func (r *crashingBatchRecorder) RecordBatch(events []usage.Event) error {
	if r.commits.Load() >= r.commitsBeforeCrash {
		<-r.hang
		return errors.New("process exited before commit")
	}
	if err := r.store.RecordBatch(events); err != nil {
		return err
	}
	r.commits.Add(1)
	return nil
}

func TestAsyncUsageJournalQueueFullDefersToReplayWithoutBlocking(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{QueueSize: 1, MaxBatch: 1}); err != nil {
		t.Fatal(err)
	}
	const n = 6
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			if err := journal.Record(asyncTestEvent(i, "alice")); err != nil {
				t.Errorf("Record %d: %v", i, err)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full queue blocked Record")
	}
	if journal.AsyncDeferred() == 0 {
		t.Fatal("expected events deferred to replay when the queue is full")
	}
	close(primary.gate)
	journal.Flush()
	if _, err := journal.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != n || total != n {
		t.Fatalf("stored=%d agg=%d, want %d", stored, total, n)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if ids := journalPendingIDs(t, path); len(ids) != 0 {
		t.Fatalf("journal not compacted: %v", ids)
	}
}

func TestAsyncUsageJournalCommitFailureStaysDurableForReplay(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	close(primary.gate)
	primary.fail.Store(true)
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	event := asyncTestEvent(1, "alice")
	event.RecordID = uuid.NewString()
	if err := journal.Record(event); err != nil {
		t.Fatalf("Record: %v", err)
	}
	journal.Flush()
	if ids := journalPendingIDs(t, path); !ids[event.RecordID] {
		t.Fatal("failed async commit dropped the event from the journal")
	}
	if _, err := journal.Replay(); err == nil {
		t.Fatal("Replay must report the still failing primary")
	}
	primary.fail.Store(false)
	recovered, err := journal.Replay()
	if err != nil || len(recovered) != 1 || recovered[0] != "alice" {
		t.Fatalf("Replay recovered=%v err=%v", recovered, err)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 1 || total != 1 {
		t.Fatalf("stored=%d agg=%d", stored, total)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if ids := journalPendingIDs(t, path); len(ids) != 0 {
		t.Fatalf("journal not empty: %v", ids)
	}
}

// Quota calibration drains the client's queued events synchronously; the
// later asynchronous commit of the same events must not double count.
func TestAsyncUsageJournalDrainClientCommitsQueuedEventsOnce(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{MaxBatch: 64}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := journal.Record(asyncTestEvent(i, "alice")); err != nil {
			t.Fatal(err)
		}
		if err := journal.Record(asyncTestEvent(100+i, "bob")); err != nil {
			t.Fatal(err)
		}
	}
	// DrainClient waits for the writer's batches instead of replaying the
	// client's owned events one by one.
	drained := make(chan error, 1)
	go func() { drained <- journal.DrainClient("alice") }()
	select {
	case err := <-drained:
		t.Fatalf("DrainClient returned before the writer committed the owned events: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(primary.gate)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("DrainClient: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrainClient did not return after the writer caught up")
	}
	if stored, _ := countStoredUsage(t, store, "alice"); stored != 5 {
		t.Fatalf("DrainClient stored=%d, want 5", stored)
	}
	if got := primary.records.Load(); got != 0 {
		t.Fatalf("DrainClient replayed %d owned events one by one", got)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 5 {
		t.Fatalf("alice stored=%d after async commit", stored)
	} else if total != 10 {
		t.Fatalf("aggregate total=%d, want 10 (5 alice + 5 bob, no double count)", total)
	}
	if stored, _ := countStoredUsage(t, store, "bob"); stored != 5 {
		t.Fatalf("bob stored=%d", stored)
	}
	if ids := journalPendingIDs(t, path); len(ids) != 0 {
		t.Fatalf("journal not empty: %v", ids)
	}
}

// Owned events the writer fails to commit are released as foreign; DrainClient
// then replays them so calibration still sees every queued event.
func TestAsyncUsageJournalDrainClientReplaysWriterFailures(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	close(primary.gate)
	primary.fail.Store(true)
	journal, err := NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage-journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := journal.Record(asyncTestEvent(i, "alice")); err != nil {
			t.Fatal(err)
		}
	}
	journal.Flush()
	if err := journal.DrainClient("alice"); err == nil {
		t.Fatal("DrainClient must fail while the primary rejects the replay")
	}
	primary.fail.Store(false)
	if err := journal.DrainClient("alice"); err != nil {
		t.Fatalf("DrainClient: %v", err)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 3 || total != 3 {
		t.Fatalf("stored=%d agg=%d, want 3", stored, total)
	}
}

// Events left by a previous process (or by the synchronous failure path) are
// not owned by the writer: compaction must keep them until a replay commits them.
func TestAsyncUsageJournalCompactionKeepsForeignPendingEvents(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	leftover := asyncTestEvent(1, "carol")
	leftover.RecordID = uuid.NewString()
	seed, err := NewUsageJournal(alwaysFailUsageRecorder{}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Record(leftover); err != nil {
		t.Fatalf("seed: %v", err)
	}
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(asyncTestEvent(2, "alice")); err != nil {
		t.Fatal(err)
	}
	journal.Flush()
	if ids := journalPendingIDs(t, path); !ids[leftover.RecordID] {
		t.Fatalf("compaction dropped the foreign pending event: %v", ids)
	}
	// Close forces a rewrite that drops the committed writer event and keeps
	// the foreign one.
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if ids := journalPendingIDs(t, path); !ids[leftover.RecordID] || len(ids) != 1 {
		t.Fatalf("forced compaction must keep exactly the foreign event: %v", ids)
	}
	// The replay recovers only the foreign event; the already committed
	// writer event is not replayed or reported again.
	recovered, err := journal.Replay()
	if err != nil || len(recovered) != 1 || recovered[0] != "carol" {
		t.Fatalf("Replay recovered=%v err=%v, want [carol]", recovered, err)
	}
	if stored, _ := countStoredUsage(t, store, "carol"); stored != 1 {
		t.Fatalf("carol stored=%d", stored)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 1 || total != 2 {
		t.Fatalf("alice stored=%d agg total=%d, want 1/2", stored, total)
	}
	if _, err := os.Stat(path); err == nil {
		if ids := journalPendingIDs(t, path); len(ids) != 0 {
			t.Fatalf("journal not empty: %v", ids)
		}
	}
}

type alwaysFailUsageRecorder struct{}

func (alwaysFailUsageRecorder) Record(usage.Event) error { return errors.New("primary unavailable") }

func TestUsageStoreRecordBatchIsAtomicAndIdempotent(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	events := []usage.Event{asyncTestEvent(1, "alice"), asyncTestEvent(2, "alice")}
	for i := range events {
		events[i].RecordID = uuid.NewString()
	}
	if err := store.RecordBatch(events); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBatch(events); err != nil {
		t.Fatalf("replaying an identical batch must be idempotent: %v", err)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 2 || total != 2 {
		t.Fatalf("stored=%d agg=%d", stored, total)
	}
	conflict := events[0]
	conflict.InputTokens = 999
	fresh := asyncTestEvent(3, "alice")
	fresh.RecordID = uuid.NewString()
	if err := store.RecordBatch([]usage.Event{fresh, conflict}); err == nil {
		t.Fatal("conflicting record ID must fail the batch")
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 2 || total != 2 {
		t.Fatalf("failed batch must roll back entirely: stored=%d agg=%d", stored, total)
	}
}
