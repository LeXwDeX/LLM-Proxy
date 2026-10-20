package nosql

import (
	"path/filepath"
	"testing"
)

// In asynchronous mode the periodic replay and calibration must not read the
// whole journal under its lock (blocking the group-commit syncer) when every
// line is committed or owned by the writer; a foreign line re-enables the read.
func TestAsyncUsageJournalSkipsFileReadsWithoutForeignEvents(t *testing.T) {
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
	for i := 0; i < 4; i++ {
		if err := journal.Record(asyncTestEvent(i, "alice")); err != nil {
			t.Fatal(err)
		}
	}
	reads := journal.fileReads.Load()
	// Writer still blocked: every line is owned. The ticker replay skips them.
	if recovered, err := journal.replay(nil, true); err != nil || len(recovered) != 0 {
		t.Fatalf("ticker replay recovered=%v err=%v", recovered, err)
	}
	close(primary.gate)
	journal.Flush()
	if err := journal.DrainClient("alice"); err != nil {
		t.Fatalf("DrainClient: %v", err)
	}
	if err := journal.DrainClient("bob"); err != nil {
		t.Fatalf("DrainClient(bob): %v", err)
	}
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	if got := journal.fileReads.Load() - reads; got != 0 {
		t.Fatalf("journal file read %d times without foreign events", got)
	}
	// A foreign event (writer commit failure) must still be found.
	primary.fail.Store(true)
	if err := journal.Record(asyncTestEvent(9, "carol")); err != nil {
		t.Fatal(err)
	}
	journal.Flush()
	primary.fail.Store(false)
	recovered, err := journal.replay(nil, true)
	if err != nil || len(recovered) != 1 || recovered[0] != "carol" {
		t.Fatalf("foreign replay recovered=%v err=%v", recovered, err)
	}
	if stored, total := countStoredUsage(t, store, "carol"); stored != 1 || total != 5 {
		t.Fatalf("carol stored=%d agg=%d", stored, total)
	}
}
