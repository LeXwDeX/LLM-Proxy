package nosql

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The same RecordID appended twice (for example a retried Record) must not
// leave a phantom owned count: DrainClient would otherwise wait forever while
// the caller holds the client's accounting gate.
func TestAsyncUsageJournalDuplicateRecordIDDoesNotLeakOwnedCount(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	journal, err := NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage-journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	event := asyncTestEvent(1, "alice")
	event.RecordID = uuid.NewString()
	for i := 0; i < 2; i++ {
		if err := journal.Record(event); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	close(primary.gate)
	journal.Flush()
	journal.mu.Lock()
	owned, counts := len(journal.owned), len(journal.ownedByClient)
	journal.mu.Unlock()
	if owned != 0 || counts != 0 {
		t.Fatalf("after the writer caught up owned=%d ownedByClient=%d, want 0/0", owned, counts)
	}
	drained := make(chan error, 1)
	go func() { drained <- journal.DrainClient("alice") }()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("DrainClient: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DrainClient waited on a phantom owned count")
	}
	if stored, total := countStoredUsage(t, store, "alice"); stored != 1 || total != 1 {
		t.Fatalf("stored=%d agg=%d, want the duplicate committed once", stored, total)
	}
}
