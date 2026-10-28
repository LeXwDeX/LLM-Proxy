package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type failOnceUsageRecorder struct {
	mu        sync.Mutex
	remaining int
	err       error
	next      usage.Recorder
}

func (r *failOnceUsageRecorder) Record(event usage.Event) error {
	r.mu.Lock()
	if r.remaining > 0 {
		r.remaining--
		r.mu.Unlock()
		return r.err
	}
	r.mu.Unlock()
	return r.next.Record(event)
}

type commitThenFailUsageRecorder struct {
	next      usage.Recorder
	err       error
	failFirst bool
}

func (r *commitThenFailUsageRecorder) Record(event usage.Event) error {
	if err := r.next.Record(event); err != nil {
		return err
	}
	if r.failFirst {
		r.failFirst = false
		return r.err
	}
	return nil
}

func TestUsageJournalDefersFailedWriteAndRecoversAfterRestart(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	writeErr := errors.New("temporary usage database failure")
	primary := &failOnceUsageRecorder{remaining: 1, err: writeErr, next: store}
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")

	journal, err := NewUsageJournal(primary, journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	event := usage.Event{
		Timestamp:    time.Date(2026, 9, 26, 1, 2, 3, 0, time.UTC),
		ClientName:   "alice",
		Model:        "qwen-max",
		InputTokens:  41,
		OutputTokens: 17,
		RequestID:    "request-1",
	}
	if err := journal.Record(event); err != nil {
		t.Fatalf("Record should succeed after durable enqueue: %v", err)
	}

	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("durable journal was not created: %v", err)
	}
	journalInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatalf("stat durable journal: %v", err)
	}
	if journalInfo.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions = %04o, want 0600", journalInfo.Mode().Perm())
	}
	items, err := store.List(usage.Filter{ClientName: "alice"})
	if err != nil {
		t.Fatalf("List before replay: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("failed primary write unexpectedly appeared in usage store: %d events", len(items))
	}

	// Reopening simulates process restart with the original outbox file intact.
	restarted, err := NewUsageJournal(primary, journalPath, nil)
	if err != nil {
		t.Fatalf("reopen NewUsageJournal: %v", err)
	}
	recoveredClients, err := restarted.Replay()
	if err != nil {
		t.Fatalf("Replay after restart: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "alice" {
		t.Fatalf("Replay recovered clients %v, want [alice]", recoveredClients)
	}

	items, err = store.List(usage.Filter{ClientName: "alice"})
	if err != nil {
		t.Fatalf("List after replay: %v", err)
	}
	if len(items) != 1 || items[0].InputTokens != 41 || items[0].OutputTokens != 17 {
		t.Fatalf("replayed event mismatch: %+v", items)
	}

	recoveredClients, err = restarted.Replay()
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if len(recoveredClients) != 0 {
		t.Fatalf("second Replay recovered clients %v, want none", recoveredClients)
	}
}

func TestUsageJournalReturnsErrorWhenRecoveryWriteCannotPersist(t *testing.T) {
	writeErr := errors.New("temporary usage database failure")
	primary := &failOnceUsageRecorder{remaining: 1, err: writeErr, next: usage.Recorder(noopUsageRecorder{})}
	journalPath := filepath.Join(t.TempDir(), "missing", "usage-journal.jsonl")
	journal, err := NewUsageJournal(primary, journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}

	err = journal.Record(usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", RequestID: "request-3"})
	if err == nil {
		t.Fatal("Record succeeded without persisting to the primary store or recovery journal")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("Record error %v does not retain the primary failure", err)
	}
}

func TestNewUsageJournalMakesExistingJournalWritableAndPrivate(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	if err := os.WriteFile(journalPath, nil, 0o400); err != nil {
		t.Fatalf("create existing journal: %v", err)
	}
	if err := os.Chmod(journalPath, 0o400); err != nil {
		t.Fatalf("restrict existing journal: %v", err)
	}

	journal, err := NewUsageJournal(noopUsageRecorder{}, journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	if journal == nil {
		t.Fatal("NewUsageJournal returned a nil journal")
	}
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatalf("stat existing journal: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions = %04o, want 0600", info.Mode().Perm())
	}
}

type noopUsageRecorder struct{}

func (noopUsageRecorder) Record(usage.Event) error { return nil }

func TestUsageJournalDeduplicatesUncertainPrimaryCommit(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	commitErr := errors.New("write outcome unknown to caller")
	primary := &commitThenFailUsageRecorder{next: store, err: commitErr, failFirst: true}
	journal, err := NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage-journal.jsonl"), nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}

	event := usage.Event{
		Timestamp:   time.Date(2026, 9, 26, 2, 3, 4, 0, time.UTC),
		ClientName:  "bob",
		Model:       "glm-flash",
		InputTokens: 29,
		RequestID:   "request-2",
		StatusCode:  http.StatusOK,
	}
	if err := journal.Record(event); err != nil {
		t.Fatalf("Record should queue event after uncertain commit: %v", err)
	}

	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "bob" {
		t.Fatalf("Replay recovered clients %v, want [bob]", recoveredClients)
	}
	items, err := store.List(usage.Filter{ClientName: "bob"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].InputTokens != 29 {
		t.Fatalf("uncertain commit was duplicated or lost: %+v", items)
	}
	from := event.Timestamp.Add(-time.Hour)
	to := event.Timestamp.Add(time.Hour)
	total, success, err := store.Count(from, to)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 1 || success != 1 {
		t.Fatalf("uncertain commit changed aggregate twice: total=%d success=%d", total, success)
	}
}

// poisonUsageRecorder always fails for one record ID and succeeds otherwise,
// letting tests model a record that can never be written to the primary store.
type poisonUsageRecorder struct {
	failRecordID string
	err          error
	next         usage.Recorder
}

func (r *poisonUsageRecorder) Record(event usage.Event) error {
	if event.RecordID == r.failRecordID {
		return r.err
	}
	return r.next.Record(event)
}

func (r *poisonUsageRecorder) RegisterUnresolvedUsage(event usage.Event, original []byte) error {
	registrar, ok := r.next.(interface {
		RegisterUnresolvedUsage(usage.Event, []byte) error
	})
	if !ok {
		return errors.New("test recorder cannot register unresolved usage")
	}
	return registrar.RegisterUnresolvedUsage(event, original)
}

func journalLine(t *testing.T, event usage.Event) string {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal journal event: %v", err)
	}
	return string(data)
}

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type registeredTestRecorder struct {
	usage.Recorder
	store *UsageStore
}

func (r registeredTestRecorder) RegisterUnresolvedUsage(event usage.Event, raw []byte) error {
	return r.store.RegisterUnresolvedUsage(event, raw)
}

func withEvidenceStore(t *testing.T, recorder usage.Recorder) registeredTestRecorder {
	t.Helper()
	return registeredTestRecorder{Recorder: recorder, store: NewUsageStore(testDB(t))}
}

func TestUsageJournalReplayToleratesTornTailLine(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	valid := usage.Event{
		Timestamp:  time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC),
		ClientName: "carol",
		RequestID:  "request-torn",
		RecordID:   uuid.NewString(),
	}
	// kill -9 during append leaves a partial JSON line without a trailing newline.
	torn := `{"timestamp":"2026-09-26T03:00:00Z","client_name":"dave","record_id":"`
	mustWriteFile(t, journalPath, journalLine(t, valid)+"\n"+torn)

	journal, err := NewUsageJournal(withEvidenceStore(t, noopUsageRecorder{}), journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay must tolerate a torn tail line: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "carol" {
		t.Fatalf("Replay recovered clients %v, want [carol]", recoveredClients)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal with only dropped lines should be removed, stat err=%v", err)
	}
}

func TestUsageJournalTornTailWithoutNewlineStillReplays(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	valid := usage.Event{
		Timestamp:  time.Date(2026, 9, 26, 3, 30, 0, 0, time.UTC),
		ClientName: "erin",
		RequestID:  "request-nonl",
		RecordID:   uuid.NewString(),
	}
	// A lost trailing newline must not lose a fully written record.
	mustWriteFile(t, journalPath, journalLine(t, valid))

	journal, err := NewUsageJournal(noopUsageRecorder{}, journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "erin" {
		t.Fatalf("Replay recovered clients %v, want [erin]", recoveredClients)
	}
}

func TestUsageJournalReplayQuarantinesCorruptMiddleLine(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	first := usage.Event{Timestamp: time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC), ClientName: "frank", RecordID: uuid.NewString()}
	second := usage.Event{Timestamp: time.Date(2026, 9, 26, 4, 1, 0, 0, time.UTC), ClientName: "gina", RecordID: uuid.NewString()}
	mustWriteFile(t, journalPath, journalLine(t, first)+"\n"+"not-json-at-all\n"+journalLine(t, second)+"\n")

	journal, err := NewUsageJournal(withEvidenceStore(t, noopUsageRecorder{}), journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay must tolerate a corrupt middle line: %v", err)
	}
	if len(recoveredClients) != 2 {
		t.Fatalf("Replay recovered clients %v, want [frank gina]", recoveredClients)
	}
	quarantine, readErr := os.ReadFile(journalPath + ".corrupt")
	if readErr != nil {
		t.Fatalf("corrupt line must be preserved in a quarantine file: %v", readErr)
	}
	if !strings.Contains(string(quarantine), "not-json-at-all") {
		t.Fatalf("quarantine file missing corrupt line: %q", quarantine)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal should be empty after full recovery, stat err=%v", err)
	}
}

func TestUsageJournalReplayQuarantinesLineWithInvalidRecordID(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	valid := usage.Event{Timestamp: time.Date(2026, 9, 26, 5, 0, 0, 0, time.UTC), ClientName: "henry", RecordID: uuid.NewString()}
	badID := `{"timestamp":"2026-09-26T05:00:00Z","client_name":"iris","record_id":"not-a-uuid"}`
	mustWriteFile(t, journalPath, journalLine(t, valid)+"\n"+badID+"\n")

	journal, err := NewUsageJournal(withEvidenceStore(t, noopUsageRecorder{}), journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay must tolerate an invalid record ID: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "henry" {
		t.Fatalf("Replay recovered clients %v, want [henry]", recoveredClients)
	}
	quarantine, readErr := os.ReadFile(journalPath + ".corrupt")
	if readErr != nil {
		t.Fatalf("invalid record ID line must be quarantined: %v", readErr)
	}
	if !strings.Contains(string(quarantine), "not-a-uuid") {
		t.Fatalf("quarantine file missing invalid record ID line: %q", quarantine)
	}
}

func TestUsageJournalReplayKeepsPoisonEventWithoutBlockingOthers(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	poisonErr := errors.New("permanent quota aggregation failure")
	primary := &poisonUsageRecorder{failRecordID: "", err: poisonErr, next: store}
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")

	poison := usage.Event{Timestamp: time.Date(2026, 9, 26, 6, 0, 0, 0, time.UTC), ClientName: "poison-client", RecordID: uuid.NewString()}
	good := usage.Event{Timestamp: time.Date(2026, 9, 26, 6, 1, 0, 0, time.UTC), ClientName: "healthy-client", RecordID: uuid.NewString()}
	mustWriteFile(t, journalPath, journalLine(t, poison)+"\n"+journalLine(t, good)+"\n")
	primary.failRecordID = poison.RecordID

	journal, err := NewUsageJournal(primary, journalPath, nil)
	if err != nil {
		t.Fatalf("NewUsageJournal: %v", err)
	}
	recoveredClients, err := journal.Replay()
	if err == nil {
		t.Fatal("Replay must report the record that cannot be written")
	}
	if !errors.Is(err, poisonErr) {
		t.Fatalf("Replay error %v does not retain the poison failure", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "healthy-client" {
		t.Fatalf("Replay recovered clients %v, want [healthy-client]", recoveredClients)
	}

	// The poison record stays queued for later retries and must not be lost.
	restarted, err := NewUsageJournal(primary, journalPath, nil)
	if err != nil {
		t.Fatalf("reopen NewUsageJournal: %v", err)
	}
	primary.failRecordID = ""
	recoveredClients, err = restarted.Replay()
	if err != nil {
		t.Fatalf("Replay after clearing poison: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "poison-client" {
		t.Fatalf("Replay recovered clients %v, want [poison-client]", recoveredClients)
	}
	items, err := store.List(usage.Filter{ClientName: "poison-client"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("poison event was lost across retries: %+v", items)
	}
}

type appendingUsageRecorder struct {
	journal *UsageJournal
	event   usage.Event
	done    bool
}

func (r *appendingUsageRecorder) Record(event usage.Event) error {
	if !r.done {
		r.done = true
		// Simulate a request appending a fresh failure record while Replay is
		// still draining the snapshot taken before the append.
		r.journal.mu.Lock()
		appendErr := r.journal.appendLocked(r.event)
		r.journal.mu.Unlock()
		if appendErr != nil {
			return appendErr
		}
	}
	return nil
}

func TestUsageJournalReplayPreservesConcurrentAppends(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	seed := usage.Event{Timestamp: time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC), ClientName: "seed-client", RecordID: uuid.NewString()}
	mustWriteFile(t, journalPath, journalLine(t, seed)+"\n")

	journal := &UsageJournal{path: journalPath}
	concurrent := usage.Event{Timestamp: time.Date(2026, 9, 26, 7, 1, 0, 0, time.UTC), ClientName: "concurrent-client", RecordID: uuid.NewString()}
	journal.primary = &appendingUsageRecorder{journal: journal, event: concurrent}

	if _, err := journal.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	recoveredClients, err := journal.Replay()
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if len(recoveredClients) != 1 || recoveredClients[0] != "concurrent-client" {
		t.Fatalf("concurrent append was lost by the rewrite: %v", recoveredClients)
	}
}

func TestUsageJournalDrainClientKeepsOtherClientsPending(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	poison := usage.Event{Timestamp: time.Now().UTC(), ClientName: "other", RecordID: uuid.NewString()}
	good := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", RecordID: uuid.NewString(), InputTokens: 41}
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	mustWriteFile(t, path, journalLine(t, poison)+"\n"+journalLine(t, good)+"\n")
	poisonErr := errors.New("other client remains unavailable")
	journal, err := NewUsageJournal(&poisonUsageRecorder{failRecordID: poison.RecordID, err: poisonErr, next: store}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.DrainClient("alice"); err != nil {
		t.Fatalf("other client's poison record blocked alice: %v", err)
	}
	items, err := store.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(items) != 1 || items[0].InputTokens != 41 {
		t.Fatalf("alice's record was not drained: %+v error=%v", items, err)
	}
	if err := journal.DrainClient("other"); !errors.Is(err, poisonErr) {
		t.Fatalf("other client must retain its replay failure: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), poison.RecordID) || strings.Contains(string(data), good.RecordID) {
		t.Fatalf("drain did not preserve only other client's pending record: %s error=%v", data, err)
	}
}

func TestUsageJournalDrainClientRejectsConcurrentPendingAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	seed := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", RecordID: uuid.NewString()}
	mustWriteFile(t, path, journalLine(t, seed)+"\n")
	journal := &UsageJournal{path: path}
	concurrent := seed
	concurrent.RecordID = uuid.NewString()
	journal.primary = &appendingUsageRecorder{journal: journal, event: concurrent}
	if err := journal.DrainClient("alice"); err == nil {
		t.Fatal("a concurrent pending append must prevent an authoritative snapshot")
	}
	if err := journal.DrainClient("alice"); err != nil {
		t.Fatalf("retry should drain the preserved append: %v", err)
	}
}

type countingUsageRecorder struct {
	events []usage.Event
}

func (r *countingUsageRecorder) Record(event usage.Event) error {
	r.events = append(r.events, event)
	return nil
}

func TestUsageJournalReplayKeepsCorruptLinesWhenQuarantineFails(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	valid := usage.Event{Timestamp: time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC), ClientName: "erin", RecordID: uuid.NewString()}
	corrupt := `{"timestamp":"nope","record_id":"also-nope"}`
	mustWriteFile(t, journalPath, journalLine(t, valid)+"\n"+corrupt+"\n")
	// A directory at the quarantine path makes the quarantine write fail the
	// way a full disk or foreign ownership would.
	if err := os.Mkdir(journalPath+".corrupt", 0o700); err != nil {
		t.Fatalf("seed quarantine dir: %v", err)
	}
	recorder := &countingUsageRecorder{}
	journal := &UsageJournal{path: journalPath, primary: recorder}

	if _, err := journal.Replay(); err == nil || !strings.Contains(err.Error(), "quarantine") {
		t.Fatalf("Replay error %v must report the failed quarantine", err)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("journal was removed despite the failed quarantine: %v", err)
	}
	if !strings.Contains(string(data), journalLine(t, valid)) || !strings.Contains(string(data), corrupt) {
		t.Fatalf("journal was rewritten despite the failed quarantine, corrupt lines lost: %q", data)
	}
	if len(recorder.events) != 1 || recorder.events[0].RecordID != valid.RecordID {
		t.Fatalf("valid event must still drain to the primary store: %+v", recorder.events)
	}
}

type dirSwappingUsageRecorder struct {
	path    string
	swapped bool
	count   int
}

func (r *dirSwappingUsageRecorder) Record(event usage.Event) error {
	r.count++
	if !r.swapped {
		r.swapped = true
		// Simulate the journal becoming unreadable right after the snapshot
		// was drained: a concurrent re-read failure must not let the rewrite
		// run from a stale snapshot and drop concurrently appended events.
		if err := os.Remove(r.path); err != nil {
			return err
		}
		if err := os.Mkdir(r.path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func TestUsageJournalReplaySkipsRewriteWhenRereadFails(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	seed := usage.Event{Timestamp: time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC), ClientName: "frank", RecordID: uuid.NewString()}
	mustWriteFile(t, journalPath, journalLine(t, seed)+"\n")

	journal := &UsageJournal{path: journalPath, primary: &dirSwappingUsageRecorder{path: journalPath}}
	recovered, err := journal.Replay()
	if err == nil || !strings.Contains(err.Error(), "re-read") {
		t.Fatalf("Replay error %v must report the failed re-read instead of rewriting", err)
	}
	if len(recovered) != 1 || recovered[0] != "frank" {
		t.Fatalf("drained records must still be reported as recovered: %v", recovered)
	}
	if info, statErr := os.Lstat(journalPath); statErr != nil || !info.IsDir() {
		t.Fatalf("journal path must stay untouched when the re-read fails: %v", statErr)
	}
}

func TestUsageJournalAppendTrimsTornTailBeforeWrite(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	torn := `{"timestamp":"2026-09-26T03:00:00Z","client_name":"dave","record_id":"`
	mustWriteFile(t, journalPath, torn)

	first := usage.Event{Timestamp: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC), ClientName: "grace", RecordID: uuid.NewString()}
	journal := &UsageJournal{path: journalPath, primary: withEvidenceStore(t, &countingUsageRecorder{})}
	journal.mu.Lock()
	err := journal.appendLocked(first)
	journal.mu.Unlock()
	if err != nil {
		t.Fatalf("appendLocked: %v", err)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if string(data) != journalLine(t, first)+"\n" {
		t.Fatalf("torn tail was not trimmed before the append: %q", data)
	}

	// The same protection applies when a valid line precedes the fragment.
	second := usage.Event{Timestamp: time.Date(2026, 9, 26, 10, 1, 0, 0, time.UTC), ClientName: "grace", RecordID: uuid.NewString()}
	mustWriteFile(t, journalPath, journalLine(t, first)+"\n"+torn)
	journal.mu.Lock()
	err = journal.appendLocked(second)
	journal.mu.Unlock()
	if err != nil {
		t.Fatalf("appendLocked after a valid line: %v", err)
	}
	data, err = os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if want := journalLine(t, first) + "\n" + journalLine(t, second) + "\n"; string(data) != want {
		t.Fatalf("fragment fused with the appended record: %q", data)
	}
	recovered, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recovered) != 1 || recovered[0] != "grace" {
		t.Fatalf("recovered clients %v, want [grace]", recovered)
	}
}

func TestUsageJournalAppendRepairsCompleteTailMissingNewline(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	// A crash can persist the record bytes without the trailing newline; the
	// record itself is complete and must be repaired, not truncated.
	lost := usage.Event{Timestamp: time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC), ClientName: "hank", RecordID: uuid.NewString()}
	mustWriteFile(t, journalPath, journalLine(t, lost))

	next := usage.Event{Timestamp: time.Date(2026, 9, 26, 11, 1, 0, 0, time.UTC), ClientName: "iris", RecordID: uuid.NewString()}
	journal := &UsageJournal{path: journalPath, primary: &countingUsageRecorder{}}
	journal.mu.Lock()
	err := journal.appendLocked(next)
	journal.mu.Unlock()
	if err != nil {
		t.Fatalf("appendLocked: %v", err)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if want := journalLine(t, lost) + "\n" + journalLine(t, next) + "\n"; string(data) != want {
		t.Fatalf("complete tail record was destroyed instead of repaired:\n got %q\nwant %q", data, want)
	}
	recovered, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recovered) != 2 || recovered[0] != "hank" || recovered[1] != "iris" {
		t.Fatalf("repaired record must replay: %v", recovered)
	}
}

func TestUsageJournalAppendRepairsCompleteTailAfterPriorRecords(t *testing.T) {
	for _, padding := range []int{0, 8192} {
		t.Run(fmt.Sprintf("tail-padding-%d", padding), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage-journal.jsonl")
			prior := usage.Event{Timestamp: time.Now().UTC(), ClientName: "prior", RecordID: uuid.NewString()}
			tail := usage.Event{Timestamp: time.Now().UTC(), ClientName: "tail", RecordID: uuid.NewString(), RequestID: strings.Repeat("x", padding)}
			next := usage.Event{Timestamp: time.Now().UTC(), ClientName: "next", RecordID: uuid.NewString()}
			mustWriteFile(t, path, journalLine(t, prior)+"\n"+journalLine(t, tail))
			recorder := &countingUsageRecorder{}
			journal := &UsageJournal{path: path, primary: recorder}
			if err := journal.appendLocked(next); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Replay(); err != nil {
				t.Fatal(err)
			}
			if len(recorder.events) != 3 || recorder.events[0].RecordID != prior.RecordID || recorder.events[1].RecordID != tail.RecordID || recorder.events[2].RecordID != next.RecordID {
				t.Fatalf("complete tail after preceding records was lost: %+v", recorder.events)
			}
		})
	}
}

type failingQuarantineAndRereadUsageRecorder struct {
	path    string
	swapped bool
}

func (r *failingQuarantineAndRereadUsageRecorder) Record(event usage.Event) error {
	if !r.swapped {
		r.swapped = true
		if err := os.Remove(r.path); err != nil {
			return err
		}
		if err := os.Mkdir(r.path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func TestUsageJournalReplayReportsQuarantineAndRereadFailuresTogether(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	valid := usage.Event{Timestamp: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), ClientName: "judy", RecordID: uuid.NewString()}
	corrupt := `{"timestamp":"nope","record_id":"also-nope"}`
	mustWriteFile(t, journalPath, journalLine(t, valid)+"\n"+corrupt+"\n")
	if err := os.Mkdir(journalPath+".corrupt", 0o700); err != nil {
		t.Fatalf("seed quarantine dir: %v", err)
	}

	journal := &UsageJournal{path: journalPath, primary: &failingQuarantineAndRereadUsageRecorder{path: journalPath}}
	_, err := journal.Replay()
	if err == nil {
		t.Fatal("Replay must fail when both the quarantine and the re-read fail")
	}
	if !strings.Contains(err.Error(), "quarantine") || !strings.Contains(err.Error(), "re-read") {
		t.Fatalf("error %v must report both failures", err)
	}
}

func TestUsageJournalAppendRepairsLargeCompleteTailMissingNewline(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	// A complete record larger than the 4KB scan chunk must still be repaired,
	// not truncated: the backward scan has to copy each chunk it prepends.
	padding := strings.Repeat("x", 8192)
	lost := usage.Event{
		Timestamp:  time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC),
		ClientName: "kate",
		RequestID:  padding,
		RecordID:   uuid.NewString(),
	}
	mustWriteFile(t, journalPath, journalLine(t, lost))

	next := usage.Event{Timestamp: time.Date(2026, 9, 27, 3, 1, 0, 0, time.UTC), ClientName: "liam", RecordID: uuid.NewString()}
	journal := &UsageJournal{path: journalPath, primary: &countingUsageRecorder{}}
	journal.mu.Lock()
	err := journal.appendLocked(next)
	journal.mu.Unlock()
	if err != nil {
		t.Fatalf("appendLocked: %v", err)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if want := journalLine(t, lost) + "\n" + journalLine(t, next) + "\n"; string(data) != want {
		t.Fatalf("large complete tail was destroyed instead of repaired (len got %d)", len(data))
	}
	recovered, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recovered) != 2 || recovered[0] != "kate" || recovered[1] != "liam" {
		t.Fatalf("recovered clients %v, want [kate liam]", recovered)
	}
}

func TestUsageJournalReplayKeepsCorruptLinesAppendedDuringDrain(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	seed := usage.Event{Timestamp: time.Date(2026, 9, 27, 4, 0, 0, 0, time.UTC), ClientName: "mia", RecordID: uuid.NewString()}
	lateCorrupt := `{"timestamp":"late-corruption","record_id":"bad"}`
	mustWriteFile(t, journalPath, journalLine(t, seed)+"\n")

	// The primary write stalls long enough for a corrupt line to be appended
	// to the journal between the first read and the rewrite.
	store := NewUsageStore(testDB(t))
	primary := &corruptAppendingUsageRecorder{journalPath: journalPath, line: lateCorrupt, store: store}
	journal := &UsageJournal{path: journalPath, primary: primary}
	if _, err := journal.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	data, readErr := os.ReadFile(journalPath)
	if readErr == nil && strings.Contains(string(data), lateCorrupt) {
		t.Fatalf("corrupt line must not stay in the rewritten journal: %q", data)
	}
	// A fully drained journal is removed outright; the corrupt line survives
	// only in the quarantine file.
	quarantine, err := os.ReadFile(journalPath + ".corrupt")
	if err != nil {
		t.Fatalf("corrupt line appended during the drain was lost entirely: %v", err)
	}
	if !strings.Contains(string(quarantine), lateCorrupt) {
		t.Fatalf("late corrupt line missing from quarantine: %q", quarantine)
	}
	items, err := primary.store.List(usage.Filter{ClientName: "mia"})
	if err != nil || len(items) != 1 {
		t.Fatalf("valid event must still drain: %v %v", items, err)
	}
}

type corruptAppendingUsageRecorder struct {
	journalPath string
	line        string
	store       *UsageStore
	done        bool
}

func (r *corruptAppendingUsageRecorder) Record(event usage.Event) error {
	if !r.done {
		r.done = true
		f, err := os.OpenFile(r.journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(r.line + "\n"); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return r.store.Record(event)
}

func (r *corruptAppendingUsageRecorder) RegisterUnresolvedUsage(event usage.Event, raw []byte) error {
	return r.store.RegisterUnresolvedUsage(event, raw)
}

func TestUsageJournalQuarantineSkipsAlreadyQuarantinedLines(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "usage-journal.jsonl")
	mustWriteFile(t, journalPath, "")
	line := `{"timestamp":"nope","record_id":"also-nope"}`
	journal := &UsageJournal{path: journalPath, primary: withEvidenceStore(t, noopUsageRecorder{})}

	journal.mu.Lock()
	err := journal.quarantineLocked([]string{line})
	journal.mu.Unlock()
	if err != nil {
		t.Fatalf("first quarantine: %v", err)
	}
	journal.mu.Lock()
	err = journal.quarantineLocked([]string{line})
	journal.mu.Unlock()
	if err != nil {
		t.Fatalf("repeat quarantine: %v", err)
	}
	quarantine, err := os.ReadFile(journalPath + ".corrupt")
	if err != nil {
		t.Fatalf("read quarantine: %v", err)
	}
	if got := strings.Count(string(quarantine), line); got != 1 {
		t.Fatalf("quarantine must stay a set; line appears %d times in %q", got, quarantine)
	}
}
