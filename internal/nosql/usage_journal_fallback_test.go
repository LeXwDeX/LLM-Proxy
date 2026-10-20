package nosql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type capturingRecorder struct {
	mu     sync.Mutex
	events []usage.Event
}

func (r *capturingRecorder) Record(event usage.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

// When the write-ahead append fails, the synchronous fallback must commit the
// same JSON-decoded event the writer and Replay use, so every path produces
// identical primary records (JSON encoding rewrites, e.g., invalid UTF-8).
func TestAsyncUsageJournalAppendFailureFallbackCommitsReplayableEvent(t *testing.T) {
	primary := &capturingRecorder{}
	path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	// A directory in place of the journal makes every append fail.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	event := asyncTestEvent(1, "alice")
	event.RequestID = "req-\xff"
	event.RecordID = "6f1c2a4e-7b1d-4c3a-9e2f-1a2b3c4d5e6f"
	if err := journal.Record(event); err != nil {
		t.Fatalf("Record with failing journal must fall back to the primary: %v", err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var want usage.Event
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	primary.mu.Lock()
	defer primary.mu.Unlock()
	if len(primary.events) != 1 {
		t.Fatalf("primary received %d events, want 1", len(primary.events))
	}
	if !reflect.DeepEqual(primary.events[0], want) {
		t.Fatalf("fallback committed %+v, want the replayable form %+v", primary.events[0], want)
	}
}
