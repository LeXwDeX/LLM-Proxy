package quota

import (
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const journalBacklogEvents = 1000

// backlogStore counts how the journal reaches the primary store: per-event
// Record calls (serial replays under the accounting gate) versus batches from
// the asynchronous writer, which waits on gate until released.
type backlogStore struct {
	store   *nosql.UsageStore
	gate    chan struct{}
	records atomic.Int64
	batches atomic.Int64
}

func (s *backlogStore) Record(event usage.Event) error {
	s.records.Add(1)
	return s.store.Record(event)
}

func (s *backlogStore) RecordBatch(events []usage.Event) error {
	<-s.gate
	s.batches.Add(1)
	return s.store.RecordBatch(events)
}

// newJournalBacklog charges journalBacklogEvents usage events ($0.01 each)
// through an asynchronous journal whose writer is held back, leaving them all
// owned by the writer and not yet committed.
func newJournalBacklog(tb testing.TB, db *bolt.DB, m *Manager) (*backlogStore, *nosql.UsageJournal) {
	tb.Helper()
	primary := &backlogStore{store: m.usageStore, gate: make(chan struct{})}
	journal, err := nosql.NewUsageJournal(primary, filepath.Join(tb.TempDir(), "usage.jsonl"), nil)
	if err != nil {
		tb.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{}); err != nil {
		tb.Fatal(err)
	}
	m.usageJournal = journal
	at := time.Now().UTC()
	// Concurrent producers share journal fsyncs (group commit), keeping the
	// setup fast; the writer stays blocked so every event remains owned.
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 50; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := next.Add(1) - 1; i < journalBacklogEvents; i = next.Add(1) - 1 {
				if err := m.RecordUsage(journal, usage.Event{Timestamp: at.Add(time.Duration(i)), ClientName: "alice", Target: "upstream",
					EndpointType: "openai", Model: "qwen-max", InputTokens: 1000, StatusCode: 200}); err != nil {
					tb.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return primary, journal
}

func setupBacklogQuota(tb testing.TB) (*bolt.DB, *Manager) {
	tb.Helper()
	db := testOpenDB(tb)
	m := newTestManager(tb, db)
	putQuotaClient(tb, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(tb, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 1000,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	quotaTestCost(tb, db, "qwen-max")
	m.Evaluate("alice")
	return db, m
}

// Calibration while the asynchronous writer is behind must not replay the
// client's queued events one by one under the accounting gate: it waits for
// the writer's batches instead, and the authoritative result equals the
// in-memory charge.
func TestEvaluateWithJournalBacklogWaitsForWriterBatches(t *testing.T) {
	db, m := setupBacklogQuota(t)
	primary, journal := newJournalBacklog(t, db, m)
	defer journal.Close()
	before := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
	if math.Abs(before-10) > 1e-9 {
		t.Fatalf("in-memory charge before calibration = %v, want 10", before)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(primary.gate)
	}()
	started := time.Now()
	m.Evaluate("alice")
	elapsed := time.Since(started)
	status := m.Status("alice")
	after := status.Groups["domestic"].Quotas[DimensionDaily].Used
	t.Logf("Evaluate with %d queued events: %v (per-event replays=%d, writer batches=%d)",
		journalBacklogEvents, elapsed, primary.records.Load(), primary.batches.Load())
	if status.CalibrationError != "" || math.Abs(after-before) > 1e-9 {
		t.Fatalf("calibration changed the quota result: before=%v after=%v err=%q", before, after, status.CalibrationError)
	}
	if got := primary.records.Load(); got != 0 {
		t.Fatalf("calibration replayed %d queued events one by one under the accounting gate", got)
	}
	events, err := m.usageStore.List(usage.Filter{ClientName: "alice", Limit: 2000})
	if err != nil || len(events) != journalBacklogEvents {
		t.Fatalf("stored events=%d err=%v", len(events), err)
	}
}

// BenchmarkEvaluateWithJournalBacklog1000 measures one calibration of a
// client whose 1000 queued events the writer has not committed yet; the
// writer is released when the calibration starts.
//
//	go test ./internal/quota -run '^$' -bench JournalBacklog -benchtime 3x
func BenchmarkEvaluateWithJournalBacklog1000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db, m := setupBacklogQuota(b)
		primary, journal := newJournalBacklog(b, db, m)
		before := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
		b.StartTimer()
		close(primary.gate)
		m.Evaluate("alice")
		b.StopTimer()
		status := m.Status("alice")
		if after := status.Groups["domestic"].Quotas[DimensionDaily].Used; status.CalibrationError != "" || math.Abs(after-before) > 1e-9 {
			b.Fatalf("quota result changed: before=%v after=%v err=%q", before, after, status.CalibrationError)
		}
		if err := journal.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
