package nosql

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// DrainClient's wait for the writer is bounded: a stalled database fails the
// drain (the caller abandons this calibration) instead of holding the gate.
// The events stay owned and are committed once the writer recovers.
func TestAsyncUsageJournalDrainClientWaitIsBounded(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	journal, err := NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage-journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if journal.drainWait != defaultUsageJournalDrainWait || defaultUsageJournalDrainWait != 10*time.Second {
		t.Fatalf("default drain wait = %v", journal.drainWait)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{DrainWaitLimit: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(asyncTestEvent(1, "alice")); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = journal.DrainClient("alice")
	elapsed := time.Since(started)
	if !errors.Is(err, ErrUsageJournalDrainTimeout) {
		t.Fatalf("DrainClient with a stalled writer = %v, want ErrUsageJournalDrainTimeout", err)
	}
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("DrainClient returned after %v, want the 100ms bound", elapsed)
	}
	if stored, _ := countStoredUsage(t, store, "alice"); stored != 0 {
		t.Fatalf("stalled commit stored=%d", stored)
	}
	close(primary.gate)
	journal.Flush()
	if err := journal.DrainClient("alice"); err != nil {
		t.Fatalf("DrainClient after recovery: %v", err)
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 1 || total != 1 {
		t.Fatalf("stored=%d agg=%d, want 1", stored, total)
	}
}
