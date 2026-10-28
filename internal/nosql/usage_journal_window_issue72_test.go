package nosql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type windowFaultRecorder struct {
	*UsageStore
	failID string
	fail   atomic.Bool
}

func (r *windowFaultRecorder) Record(event usage.Event) error {
	if r.fail.Load() && event.RecordID == r.failID {
		return errors.New("old event temporarily unavailable")
	}
	return r.UsageStore.Record(event)
}

type windowGatedRecorder struct {
	*UsageStore
	started chan struct{}
	gate    chan struct{}
	once    sync.Once
}

type windowAppendingRecorder struct {
	*UsageStore
	journal *UsageJournal
	late    string
	once    sync.Once
	err     error
}

func (r *windowAppendingRecorder) Record(event usage.Event) error {
	r.once.Do(func() {
		r.journal.mu.Lock()
		r.journal.foreign = true
		r.err = r.journal.appendLinesLocked([]byte(r.late + "\n"))
		r.journal.mu.Unlock()
	})
	if r.err != nil {
		return r.err
	}
	return r.UsageStore.Record(event)
}

func (r *windowGatedRecorder) RecordBatch(events []usage.Event) error {
	r.once.Do(func() { close(r.started) })
	<-r.gate
	return r.UsageStore.RecordBatch(events)
}

func windowFixture(id string, at time.Time, client string) usage.Event {
	e := fixedPlainEvent()
	e.RecordID = id
	e.Timestamp = at
	e.ClientName = client
	e.InputTokens = 1_000_000
	return e
}

func TestIssue72WindowDrainSkipsOldForeignAndReplaysAfterRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	from := boundScanTime.Truncate(time.Hour).Add(30 * time.Minute)
	to := from.Add(20 * time.Minute)
	old := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa401", from.Truncate(time.Hour).Add(-time.Nanosecond), "alice")
	current := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa402", from.Truncate(time.Hour).Add(5*time.Minute), "alice")
	primary := &windowFaultRecorder{UsageStore: store, failID: old.RecordID}
	primary.fail.Store(true)
	path := filepath.Join(t.TempDir(), "usage.journal")
	oldLine := "  " + journalLine(t, old) + "  "
	mustWriteFile(t, path, oldLine+"\n"+journalLine(t, current)+"\n")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.DrainClientRange("alice", from, to); err != nil {
		t.Fatalf("old foreign row blocked current window: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, []byte(oldLine+"\n")) {
		t.Fatalf("old raw line not retained: %q %v", got, err)
	}
	if err := journal.DrainClient("alice"); err == nil {
		t.Fatal("full-history drain skipped old failed event")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	pathDB := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(pathDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = clockedStore(db)
	journal, err = NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	for i := 0; i < 2; i++ {
		if _, err := journal.Replay(); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("restart replay duplicated or lost events: %v %v", rows, err)
	}
	byID := map[string]int{}
	for _, row := range rows {
		byID[row.RecordID]++
	}
	if byID[old.RecordID] != 1 || byID[current.RecordID] != 1 {
		t.Fatalf("wrong identities: %v", byID)
	}
	first, last := old.Timestamp.Truncate(time.Hour), current.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	totals, err := store.SumQuotaByClientRange("alice", first, last)
	if err != nil || totals["anthropic:executor"].InputTokens != 2_000_000 {
		t.Fatalf("replayed usage: %v %v", totals, err)
	}
}

func TestIssue72WindowDrainKeepsOldEventForBackgroundReplay(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	from := boundScanTime.Truncate(time.Hour).Add(30 * time.Minute)
	to := from.Add(10 * time.Minute)
	old := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa409", from.Truncate(time.Hour).Add(-time.Nanosecond), "alice")
	primary := &windowFaultRecorder{UsageStore: store, failID: old.RecordID}
	primary.fail.Store(true)
	path := filepath.Join(t.TempDir(), "usage.journal")
	journal, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(old); err != nil {
		t.Fatal(err)
	}
	if err := journal.DrainClientRange("alice", from, to); err != nil {
		t.Fatalf("old row blocked: %v", err)
	}
	primary.fail.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered := make(chan []string, 1)
	journal.Start(ctx, func(clients []string) {
		select {
		case recovered <- clients:
		default:
		}
	})
	select {
	case clients := <-recovered:
		if len(clients) != 1 || clients[0] != "alice" {
			t.Fatalf("unexpected recovery: %v", clients)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("background replay did not recover old row")
	}
	cancel()
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 1 || rows[0].RecordID != old.RecordID {
		t.Fatalf("background recovery not idempotent: %v %v", rows, err)
	}
}

func TestIssue72WindowDrainRejectsBoundaryFutureAndUnknown(t *testing.T) {
	from := boundScanTime.Truncate(time.Hour).Add(30 * time.Minute)
	to := from.Add(10 * time.Minute)
	for _, tc := range []struct {
		name        string
		at          time.Time
		client, raw string
	}{
		{"boundary", from.Truncate(time.Hour), "alice", ""},
		{"future", to.Add(48 * time.Hour), "alice", ""},
		{"unknown_time", time.Time{}, "alice", ""},
		{"unknown_client", from.Add(-24 * time.Hour), "", ""},
		{"unknown_client_sentinel", from.Add(-24 * time.Hour), "unknown", ""},
		{"corrupt", time.Time{}, "", "{broken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa403", tc.at, tc.client)
			primary := &windowFaultRecorder{UsageStore: store, failID: event.RecordID}
			primary.fail.Store(true)
			path := filepath.Join(t.TempDir(), "usage.journal")
			line := tc.raw
			if line == "" {
				line = journalLine(t, event)
			}
			mustWriteFile(t, path, line+"\n")
			journal, err := NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.DrainClientRange("alice", from, to); err == nil {
				t.Fatalf("%s row was skipped", tc.name)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != line+"\n" {
				t.Fatalf("%s evidence changed: %q %v", tc.name, got, err)
			}
		})
	}
}

func TestIssue72WindowDrainOwnedFutureAndUnknown(t *testing.T) {
	from := boundScanTime.Truncate(time.Hour).Add(30 * time.Minute)
	to := from.Add(10 * time.Minute)
	for _, tc := range []struct {
		name, client string
		at           time.Time
		wait         bool
	}{
		{"future", "alice", to.Add(24 * time.Hour), true},
		{"unknown_client", "", from.Add(-24 * time.Hour), true},
		{"unknown_client_sentinel", "unknown", from.Add(-24 * time.Hour), true},
		{"other_client", "bob", from, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			primary := &windowGatedRecorder{UsageStore: store, started: make(chan struct{}), gate: make(chan struct{})}
			journal, err := NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(primary.gate) }); _ = journal.Close() })
			if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{MaxBatch: 1, DrainWaitLimit: 80 * time.Millisecond}); err != nil {
				t.Fatal(err)
			}
			event := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa406", tc.at, tc.client)
			if err := journal.Record(event); err != nil {
				t.Fatal(err)
			}
			select {
			case <-primary.started:
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not start")
			}
			err = journal.DrainClientRange("alice", from, to)
			if tc.wait && !errors.Is(err, ErrUsageJournalDrainTimeout) {
				t.Fatalf("relevant owned event skipped: %v", err)
			}
			if !tc.wait && err != nil {
				t.Fatalf("unrelated owner blocked: %v", err)
			}
		})
	}
}

func TestIssue72WindowDrainChecksConcurrentAppends(t *testing.T) {
	from := boundScanTime.Truncate(time.Hour).Add(30 * time.Minute)
	to := from.Add(10 * time.Minute)
	for _, tc := range []struct {
		name        string
		at          time.Time
		client, raw string
		blocked     bool
	}{
		{"old", from.Truncate(time.Hour).Add(-time.Nanosecond), "alice", "", false},
		{"current", from.Truncate(time.Hour), "alice", "", true},
		{"unknown", from.Add(-24 * time.Hour), "", "", true},
		{"corrupt", time.Time{}, "", "{broken", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			seed := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa407", from.Truncate(time.Hour), "alice")
			late := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa408", tc.at, tc.client)
			line := tc.raw
			if line == "" {
				line = journalLine(t, late)
			}
			path := filepath.Join(t.TempDir(), "usage.journal")
			mustWriteFile(t, path, journalLine(t, seed)+"\n")
			primary := &windowAppendingRecorder{UsageStore: store, late: line}
			journal, err := NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			primary.journal = journal
			err = journal.DrainClientRange("alice", from, to)
			if tc.blocked && err == nil {
				t.Fatalf("concurrent %s line was skipped", tc.name)
			}
			if !tc.blocked && err != nil {
				t.Fatalf("old concurrent line blocked: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte(line+"\n")) {
				t.Fatalf("concurrent raw line lost: %q %v", data, err)
			}
			rows, err := store.List(usage.Filter{ClientName: "alice"})
			if err != nil || len(rows) != 1 || rows[0].RecordID != seed.RecordID {
				t.Fatalf("seed not committed exactly once: %v %v", rows, err)
			}
		})
	}
}

func TestIssue72WindowDrainWaitsOnlyRelevantOwned(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			from := boundScanTime.Truncate(time.Hour).Add(30 * time.Minute)
			to := from.Add(10 * time.Minute)
			primary := &windowGatedRecorder{UsageStore: store, started: make(chan struct{}), gate: make(chan struct{})}
			path := filepath.Join(t.TempDir(), "usage.journal")
			journal, err := NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(primary.gate) }); _ = journal.Close() })
			if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{MaxBatch: 1, DrainWaitLimit: 80 * time.Millisecond}); err != nil {
				t.Fatal(err)
			}
			old := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa404", from.Truncate(time.Hour).Add(-time.Nanosecond), "alice")
			if err := journal.Record(old); err != nil {
				t.Fatal(err)
			}
			select {
			case <-primary.started:
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not start")
			}
			if mixed {
				current := windowFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa405", from.Truncate(time.Hour), "alice")
				if err := journal.Record(current); err != nil {
					t.Fatal(err)
				}
			}
			err = journal.DrainClientRange("alice", from, to)
			if mixed && !errors.Is(err, ErrUsageJournalDrainTimeout) {
				t.Fatalf("in-window owned event did not wait: %v", err)
			}
			if !mixed && err != nil {
				t.Fatalf("old owned event blocked range: %v", err)
			}
			if err := journal.DrainClient("alice"); !errors.Is(err, ErrUsageJournalDrainTimeout) {
				t.Fatalf("full-history owned wait changed: %v", err)
			}
			release.Do(func() { close(primary.gate) })
			journal.Flush()
			if err := journal.DrainClientRange("alice", from, to); err != nil {
				t.Fatalf("drain after recovery: %v", err)
			}
		})
	}
}
