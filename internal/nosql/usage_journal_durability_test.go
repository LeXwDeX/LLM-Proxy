package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
)

func issue66ConflictFixture(t *testing.T) (*UsageStore, usage.Event, string) {
	t.Helper()
	store := NewUsageStore(testDB(t))
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	event.InputTokens++
	event.RequestID = "req-雪☃-fixed"
	line := journalLine(t, event)
	return store, event, line
}

func issue66Read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIssue66SidecarSyncFailureNeverTreatsReadableBytesAsDurable(t *testing.T) {
	for _, kind := range []string{"rejected", "corrupt"} {
		for _, stage := range []string{"file", "directory"} {
			t.Run(kind+"/"+stage, func(t *testing.T) {
				store, rejected, _ := issue66ConflictFixture(t)
				line := journalLine(t, rejected)
				if kind == "corrupt" {
					line = "bad-json-\xff"
				}
				path := filepath.Join(t.TempDir(), "usage.journal")
				original := []byte(line + "\n")
				mustWriteFile(t, path, string(original))
				injected := errors.New("injected sidecar sync failure")
				for attempt := 0; attempt < 3; attempt++ {
					// A fresh instance makes the retry independent of process memory.
					j, err := NewUsageJournal(store, path, nil)
					if err != nil {
						t.Fatal(err)
					}
					if attempt < 2 && stage == "file" {
						j.syncFile = func(f *os.File) error {
							if f.Name() == path+"."+kind {
								return injected
							}
							return f.Sync()
						}
					} else if attempt < 2 {
						j.syncDir = func(string) error { return injected }
					}
					_, err = j.Replay()
					if attempt < 2 {
						if !errors.Is(err, injected) {
							t.Fatalf("attempt %d: sync failure hidden: %v", attempt, err)
						}
						if got := issue66Read(t, path); !bytes.Equal(got, original) {
							t.Fatalf("attempt %d: source changed before sidecar durability: %q", attempt, got)
						}
					} else if err != nil {
						t.Fatalf("retry after sync recovery: %v", err)
					}
				}
				if got := issue66Read(t, path); len(got) != 0 {
					t.Fatalf("durably saved source remained: %q", got)
				}
				side := issue66Read(t, path+"."+kind)
				if kind == "rejected" {
					entries := readRejectedSidecar(t, path)
					if len(entries) != 1 || !bytes.Equal(entries[0].Line, []byte(line)) {
						t.Fatalf("rejected entries: %+v", entries)
					}
				} else if !bytes.Equal(side, original) {
					t.Fatalf("corrupt sidecar duplicated or changed bytes: %q", side)
				}
				info, err := os.Stat(path + "." + kind)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("sidecar permission: %v %v", info, err)
				}
			})
		}
	}
}

func TestIssue66RejectedFailureAndCompactionKeepOriginalBytes(t *testing.T) {
	store, _, line := issue66ConflictFixture(t)
	path := filepath.Join(t.TempDir(), "usage.journal")
	original := []byte("  " + line + "  \n")
	mustWriteFile(t, path, string(original))
	j, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("sidecar sync failed")
	j.syncFile = func(f *os.File) error {
		if f.Name() == path+".rejected" {
			return injected
		}
		return f.Sync()
	}
	if _, err := j.Replay(); !errors.Is(err, injected) {
		t.Fatalf("expected rejected sidecar failure: %v", err)
	}
	if got := issue66Read(t, path); !bytes.Equal(got, original) {
		t.Fatalf("failed sidecar changed pending raw line: got %q want %q", got, original)
	}
	if err := j.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	other := asyncTestEvent(8, "other")
	if err := j.Record(other); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got := issue66Read(t, path); !bytes.Equal(got, original) {
		t.Fatalf("async compaction changed pending raw line: got %q want %q", got, original)
	}
}

type issue66AppendingRecorder struct {
	store   *UsageStore
	journal *UsageJournal
	event   usage.Event
	once    bool
}

func (r *issue66AppendingRecorder) Record(event usage.Event) error {
	if !r.once {
		r.once = true
		r.journal.mu.Lock()
		err := r.journal.appendLocked(r.event)
		r.journal.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return r.store.Record(event)
}

func (r *issue66AppendingRecorder) RegisterUnresolvedUsage(event usage.Event, original []byte) error {
	return r.store.RegisterUnresolvedUsage(event, original)
}

func TestIssue66ReplayPreservesConcurrentConflictingIdentity(t *testing.T) {
	store := NewUsageStore(testDB(t))
	first := fixedPlainEvent()
	conflict := first
	conflict.InputTokens++
	path := filepath.Join(t.TempDir(), "usage.journal")
	mustWriteFile(t, path, journalLine(t, first)+"\n")
	j, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.primary = &issue66AppendingRecorder{store: store, journal: j, event: conflict}
	if _, err := j.Replay(); err != nil {
		t.Fatal(err)
	}
	if got := issue66Read(t, path); !bytes.Equal(got, []byte(journalLine(t, conflict)+"\n")) {
		t.Fatalf("concurrent conflicting payload was lost: %q", got)
	}
	if _, err := j.Replay(); err != nil {
		t.Fatal(err)
	}
	entries := readRejectedSidecar(t, path)
	if len(entries) != 1 || entries[0].Reason != usageRejectDigestConflict || string(entries[0].Line) != journalLine(t, conflict) {
		t.Fatalf("conflict not preserved for review: %+v", entries)
	}
	if got := clientInputTokens(t, store, first.ClientName, first.Timestamp); got != first.InputTokens {
		t.Fatalf("conflict changed accounting: %d", got)
	}
}

type issue66ReplayGate struct {
	store   *UsageStore
	entered chan struct{}
	release chan struct{}
}

func (r *issue66ReplayGate) Record(event usage.Event) error {
	r.entered <- struct{}{}
	<-r.release
	return r.store.Record(event)
}

func (r *issue66ReplayGate) RegisterUnresolvedUsage(event usage.Event, original []byte) error {
	return r.store.RegisterUnresolvedUsage(event, original)
}

func TestIssue66ConcurrentReplaysDoNotDuplicateRejection(t *testing.T) {
	store, event, _ := issue66ConflictFixture(t)
	path := filepath.Join(t.TempDir(), "usage.journal")
	mustWriteFile(t, path, journalLine(t, event)+"\n")
	gate := &issue66ReplayGate{store: store, entered: make(chan struct{}, 2), release: make(chan struct{})}
	j, err := NewUsageJournal(gate, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := j.Replay(); finished <- err }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			close(gate.release)
			t.Fatal("concurrent replay did not reach the ledger")
		}
	}
	close(gate.release)
	for i := 0; i < 2; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	entries := readRejectedSidecar(t, path)
	if len(entries) != 1 || string(entries[0].Line) != journalLine(t, event) || len(issue66Read(t, path)) != 0 {
		t.Fatalf("concurrent rejection was not preserved once: %+v", entries)
	}
}

func TestIssue66AsyncCompactionPreservesForeignConflictingIdentity(t *testing.T) {
	store := NewUsageStore(testDB(t))
	first := fixedPlainEvent()
	conflict := first
	conflict.InputTokens++
	path := filepath.Join(t.TempDir(), "usage.journal")
	foreign := "  " + journalLine(t, conflict) + "  \n"
	mustWriteFile(t, path, foreign)
	j, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := j.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got := issue66Read(t, path); string(got) != foreign {
		t.Fatalf("compaction lost or changed conflicting foreign line: %q", got)
	}
	if _, err := j.Replay(); err != nil {
		t.Fatal(err)
	}
	entries := readRejectedSidecar(t, path)
	if len(entries) != 1 || string(entries[0].Line) != strings.TrimSuffix(foreign, "\n") {
		t.Fatalf("foreign conflict not preserved: %+v", entries)
	}
	if got := clientInputTokens(t, store, first.ClientName, first.Timestamp); got != first.InputTokens {
		t.Fatalf("conflict changed accounting: %d", got)
	}
}

func TestIssue66AsyncOwnedConflictingIdentitiesStaySeparate(t *testing.T) {
	store := NewUsageStore(testDB(t))
	primary := &gatedBatchRecorder{store: store, gate: make(chan struct{})}
	first := fixedPlainEvent()
	conflict := first
	conflict.InputTokens++
	path := filepath.Join(t.TempDir(), "usage.journal")
	j, err := NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.EnableAsyncCommit(UsageJournalAsyncOptions{MaxBatch: 1}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []usage.Event{first, conflict} {
		if err := j.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	close(primary.gate)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got := issue66Read(t, path); string(got) != journalLine(t, conflict)+"\n" {
		t.Fatalf("writer lost its conflicting queued event: %q", got)
	}
	if _, err := j.Replay(); err != nil {
		t.Fatal(err)
	}
	entries := readRejectedSidecar(t, path)
	if len(entries) != 1 || entries[0].Reason != usageRejectDigestConflict || string(entries[0].Line) != journalLine(t, conflict) {
		t.Fatalf("queued conflict not preserved: %+v", entries)
	}
	if got := clientInputTokens(t, store, first.ClientName, first.Timestamp); got != first.InputTokens {
		t.Fatalf("queued conflict changed accounting: %d", got)
	}
}

func TestIssue66TornSidecarKeepsFragmentAndSeparateCompleteLine(t *testing.T) {
	for _, kind := range []string{"rejected", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			store, _, line := issue66ConflictFixture(t)
			if kind == "corrupt" {
				line = "bad-json-line"
			}
			path := filepath.Join(t.TempDir(), "usage.journal")
			mustWriteFile(t, path, line+"\n")
			fragment := "partial-sidecar"
			j, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, path+"."+kind, fragment)
			if _, err := j.Replay(); err != nil {
				t.Fatal(err)
			}
			side := issue66Read(t, path+"."+kind)
			if !bytes.HasPrefix(side, []byte(fragment+"\n")) {
				t.Fatalf("torn fragment glued to complete entry: %q", side)
			}
			if kind == "rejected" {
				var entry rejectedUsageLine
				if err := json.Unmarshal(bytes.TrimSpace(side[len(fragment)+1:]), &entry); err != nil || string(entry.Line) != line {
					t.Fatalf("complete rejection missing: %+v, %v", entry, err)
				}
			} else if string(side) != fragment+"\n"+line+"\n" {
				t.Fatalf("complete corruption missing: %q", side)
			}
		})
	}
}

func TestIssue72TornExistingRejectedSidecarBlocksActivation(t *testing.T) {
	store, _, line := issue66ConflictFixture(t)
	path := filepath.Join(t.TempDir(), "usage.journal")
	mustWriteFile(t, path, line+"\n")
	mustWriteFile(t, path+".rejected", "partial-sidecar")
	if _, err := NewUsageJournal(store, path, nil); !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("torn sidecar must block activation: %v", err)
	}
	if got := issue66Read(t, path); string(got) != line+"\n" {
		t.Fatalf("activation failure changed active journal: %q", got)
	}
	if got := issue66Read(t, path+".rejected"); string(got) != "partial-sidecar" {
		t.Fatalf("activation failure changed sidecar: %q", got)
	}
}

func TestIssue66CompleteSidecarTailIsSyncedAndNotDuplicated(t *testing.T) {
	for _, kind := range []string{"rejected", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			store, event, _ := issue66ConflictFixture(t)
			line := journalLine(t, event)
			if kind == "corrupt" {
				line = "bad-json-line"
			}
			path := filepath.Join(t.TempDir(), "usage.journal")
			mustWriteFile(t, path, line+"\n")
			j, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := j.Replay(); err != nil {
				t.Fatal(err)
			}
			side := issue66Read(t, path+"."+kind)
			mustWriteFile(t, path+"."+kind, string(bytes.TrimSuffix(side, []byte("\n"))))
			mustWriteFile(t, path, line+"\n")
			fail := errors.New("tail sync failed")
			j.syncFile = func(f *os.File) error {
				if f.Name() == path+"."+kind {
					return fail
				}
				return f.Sync()
			}
			if _, err := j.Replay(); !errors.Is(err, fail) {
				t.Fatalf("tail sync failure hidden: %v", err)
			}
			if got := issue66Read(t, path); string(got) != line+"\n" {
				t.Fatalf("source removed before tail sync: %q", got)
			}
			j.syncFile = nil
			if _, err := j.Replay(); err != nil {
				t.Fatal(err)
			}
			if got := issue66Read(t, path+"."+kind); !bytes.Equal(got, side) {
				t.Fatalf("completed tail duplicated or changed: %q", got)
			}
		})
	}
}

func TestIssue66SidecarDurableBeforeFailedSourceRewrite(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	first := fixedPlainEvent()
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.InputTokens++
	conflictLine := journalLine(t, conflict)
	pending := first
	pending.RecordID = uuid.NewString()
	pending.RequestID = "pending"
	pending.InputTokens = 250000
	pendingLine := "  " + journalLine(t, pending) + "  "
	path := filepath.Join(t.TempDir(), "usage.journal")
	original := []byte(conflictLine + "\n" + pendingLine + "\n")
	mustWriteFile(t, path, string(original))
	primaryErr := errors.New("temporary primary failure")
	j, err := NewUsageJournal(&poisonUsageRecorder{failRecordID: pending.RecordID, err: primaryErr, next: store}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rewriteErr := errors.New("temporary rewrite sync failure")
	j.syncFile = func(f *os.File) error {
		if strings.HasPrefix(filepath.Base(f.Name()), ".usage-journal-") {
			return rewriteErr
		}
		return f.Sync()
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := j.Replay(); !errors.Is(err, rewriteErr) || !errors.Is(err, primaryErr) {
			t.Fatalf("attempt %d lost rewrite or primary error: %v", attempt, err)
		}
		if got := issue66Read(t, path); !bytes.Equal(got, original) {
			t.Fatalf("attempt %d changed source after failed rewrite: %q", attempt, got)
		}
		if entries := readRejectedSidecar(t, path); len(entries) != 1 || string(entries[0].Line) != conflictLine {
			t.Fatalf("attempt %d sidecar duplicated or changed: %+v", attempt, entries)
		}
	}
	j.syncFile = nil
	if _, err := j.Replay(); !errors.Is(err, primaryErr) {
		t.Fatalf("transient row should still fail: %v", err)
	}
	if got := issue66Read(t, path); string(got) != pendingLine+"\n" {
		t.Fatalf("pending raw line changed after successful rewrite: %q", got)
	}
	j.primary = store
	for i := 0; i < 2; i++ {
		if _, err := j.Replay(); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List(usage.Filter{ClientName: first.ClientName})
	if err != nil || len(items) != 2 {
		t.Fatalf("unexpected committed events: %+v, %v", items, err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: first.ClientName, From: &from, To: &to}
	computed, err := store.Aggregate(filter, "hour", replayCosts)
	if err != nil {
		t.Fatal(err)
	}
	if computed.Totals.EstimatedCost != 2.5 || computed.Totals.Requests != 2 {
		t.Fatalf("incorrect replayed usage cost: %+v", computed.Totals)
	}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || costUnitTotal(units) != 2.5 {
		t.Fatalf("incorrect quota cost: %+v, %v", units, err)
	}
}

func TestIssue66TornJournalTailSavedBeforeReplayOrAppend(t *testing.T) {
	for _, action := range []string{"replay", "append"} {
		t.Run(action, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.journal")
			torn := []byte("{\"unfinished\":\"\xff")
			mustWriteFile(t, path, string(torn))
			j, err := NewUsageJournal(withEvidenceStore(t, noopUsageRecorder{}), path, nil)
			if err != nil {
				t.Fatal(err)
			}
			fail := errors.New("torn tail sidecar sync failed")
			j.syncFile = func(f *os.File) error {
				if f.Name() == path+".corrupt" {
					return fail
				}
				return f.Sync()
			}
			next := fixedPlainEvent()
			for attempt := 0; attempt < 2; attempt++ {
				if action == "replay" {
					_, err = j.Replay()
				} else {
					j.mu.Lock()
					err = j.appendLocked(next)
					j.mu.Unlock()
				}
				if !errors.Is(err, fail) {
					t.Fatalf("attempt %d did not report sidecar failure: %v", attempt, err)
				}
				if got := issue66Read(t, path); !bytes.Equal(got, torn) {
					t.Fatalf("attempt %d discarded torn source: %q", attempt, got)
				}
			}
			j.syncFile = nil
			if action == "replay" {
				_, err = j.Replay()
			} else {
				j.mu.Lock()
				err = j.appendLocked(next)
				j.mu.Unlock()
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := issue66Read(t, path+".corrupt"); !bytes.Equal(got, append(torn, '\n')) {
				t.Fatalf("torn sidecar changed or duplicated: %q", got)
			}
			if action == "append" && string(issue66Read(t, path)) != journalLine(t, next)+"\n" {
				t.Fatal("append did not retain the new event")
			}
		})
	}
}
