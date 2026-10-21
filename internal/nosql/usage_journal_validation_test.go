package nosql

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type recordingUsageRecorder struct {
	mu     sync.Mutex
	events []usage.Event
	err    error
}

func (r *recordingUsageRecorder) Record(event usage.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return r.err
}

func (r *recordingUsageRecorder) recorded() []usage.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]usage.Event(nil), r.events...)
}

func invalidBundleCases() []struct {
	name  string
	event usage.Event
} {
	base := usage.Event{
		Timestamp:  time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC),
		ClientName: "alice", Model: "model", EndpointType: "openai", Target: "upstream",
	}
	duplicate := base
	duplicate.Components = []usage.Component{{IterationIndex: 1}, {IterationIndex: 1}}
	negative := base
	negative.Components = []usage.Component{{IterationIndex: -1}}
	supplementary := base
	supplementary.Supplementary = true
	supplementary.Components = []usage.Component{{IterationIndex: 0}}
	return []struct {
		name  string
		event usage.Event
	}{
		{"duplicate iteration index", duplicate},
		{"negative iteration index", negative},
		{"supplementary with components", supplementary},
	}
}

func TestUsageJournalRejectsInvalidBundleBeforeSyncFallback(t *testing.T) {
	for _, tc := range invalidBundleCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "usage-journal.jsonl")
			primary := &recordingUsageRecorder{err: errors.New("primary unavailable")}
			journal, err := NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()

			if err := journal.Record(tc.event); err == nil {
				t.Fatal("invalid bundle was accepted and reached the fallback path")
			}
			if got := primary.recorded(); len(got) != 0 {
				t.Fatalf("invalid bundle reached the primary recorder: %+v", got)
			}
			if data, err := os.ReadFile(path); err == nil && len(data) != 0 {
				t.Fatalf("invalid bundle left journal bytes: %s", data)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func TestUsageJournalRejectsInvalidBundleBeforeAsyncAppend(t *testing.T) {
	for _, tc := range invalidBundleCases() {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
			primary := &recordingUsageRecorder{}
			journal, err := NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
				t.Fatal(err)
			}
			defer journal.Close()

			if err := journal.Record(tc.event); err == nil {
				t.Fatal("invalid bundle was accepted by the asynchronous path")
			}
			journal.Flush()
			if pending := journal.AsyncPending(); pending != 0 {
				t.Fatalf("invalid bundle was queued: pending=%d", pending)
			}
			if got := primary.recorded(); len(got) != 0 {
				t.Fatalf("invalid bundle reached the primary recorder: %+v", got)
			}
			if data, err := os.ReadFile(path); err == nil && len(data) != 0 {
				t.Fatalf("invalid bundle left journal bytes: %s", data)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func TestUsageJournalOrdinaryEventKeepsOneIdentityAndTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-journal.jsonl")
	primary := &recordingUsageRecorder{err: errors.New("primary unavailable")}
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}

	// An ordinary event has no RecordID and no Timestamp. Both defaults are
	// filled exactly once before the journal fallback persists the payload.
	before := time.Now().UTC()
	if err := journal.Record(usage.Event{ClientName: "alice", Model: "model", EndpointType: "openai", Target: "upstream"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var queued usage.Event
	if err := json.Unmarshal(data, &queued); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(queued.RecordID); err != nil {
		t.Fatalf("journal did not persist one generated identity: %q", queued.RecordID)
	}
	if queued.Timestamp.IsZero() || queued.Timestamp.Before(before) || queued.Timestamp.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("journal timestamp=%v, want the default current UTC time", queued.Timestamp)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	// Replay must reuse the identity already written to the journal.
	db := testDB(t)
	restarted, err := NewUsageJournal(NewUsageStore(db), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Replay(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := NewUsageStore(db).List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("replayed rows=%d err=%v", len(rows), err)
	}
	if rows[0].RecordID != queued.RecordID {
		t.Fatalf("replay generated another identity: journal=%q raw=%q", queued.RecordID, rows[0].RecordID)
	}
}
