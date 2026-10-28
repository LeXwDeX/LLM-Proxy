package nosql

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// lockedBuffer collects slog output from concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDefaultLog redirects the process default logger for one test.
func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return out
}

func putQuotaAggRaw(t *testing.T, db *bolt.DB, key string, value []byte) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put([]byte(key), value)
	}); err != nil {
		t.Fatal(err)
	}
}

func explicitPlainEvent(id string, at time.Time) usage.Event {
	event := fixedPlainEvent()
	event.RecordID = id
	event.Timestamp = at
	return event
}

// TestReceiptWatermarkToleratesCorruptLegacyLedger models an upgraded v1.0.1
// database whose readers silently skipped an undecodable cell and a malformed
// key. An unknown hour cannot establish any safe boundary, but a known raw
// identity remains idempotent and the scan still runs only once.
func TestReceiptWatermarkToleratesCorruptLegacyLedger(t *testing.T) {
	logs := captureDefaultLog(t)
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store := NewUsageStore(db)
	now := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	known := explicitPlainEvent("33333333-3333-4333-8333-333333333331", now)
	if err := store.Record(known); err != nil {
		t.Fatal(err)
	}
	const corruptBody = "{not json"
	putAggRaw(t, db, "2025-01-01T00|claude|c|m|t", []byte(corruptBody))
	putAggRaw(t, db, "2025-02-03T04|claude|c", []byte(`{"record_count":1}`))
	putAggRaw(t, db, "not-an-hour|claude|c|m|t", []byte(`{"record_count":1}`))
	putQuotaAggRaw(t, db, "2025-03-01T07|c|t|m", []byte(corruptBody))
	resetReceiptTracking(t, db)
	store = NewUsageStore(db)
	if added, err := store.RecordOnce(known); err != nil || added {
		t.Fatalf("known raw retry: added=%v err=%v", added, err)
	}
	unknown := explicitPlainEvent("33333333-3333-4333-8333-333333333332", now.Add(time.Second))
	if err := store.RecordBatch([]usage.Event{unknown}); !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("RecordBatch admitted an unknown identity: %v", err)
	}

	journalPath := filepath.Join(dir, "usage.db.usage-journal.jsonl")
	pending := explicitPlainEvent("33333333-3333-4333-8333-333333333333", now.Add(2*time.Second))
	mustWriteFile(t, journalPath, journalLine(t, pending)+"\n")
	journal, err := NewUsageJournal(store, journalPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := journal.Replay(); err != nil || len(recovered) != 0 {
		t.Fatalf("Replay committed an unknown identity: recovered=%v err=%v", recovered, err)
	}
	if rejected := readRejectedSidecar(t, journalPath); len(rejected) != 1 || rejected[0].Reason != usageRejectIdentityUnverified {
		t.Fatalf("unknown identity not retained for review: %+v", rejected)
	}
	if _, err := store.PruneEventsBefore(now.Add(-24*time.Hour), 100); err != nil {
		t.Fatalf("PruneEventsBefore blocked by corrupt legacy ledger: %v", err)
	}
	for i := 0; i < 5; i++ {
		if exists, err := store.RecordExists(known); err != nil || !exists {
			t.Fatalf("RecordExists: exists=%v err=%v", exists, err)
		}
	}
	if got := store.receiptScans.Load(); got != 1 {
		t.Fatalf("watermark scans=%d, want exactly 1 for all explicit writes", got)
	}
	if raw := rawUsageRowCount(t, db); raw != 1 {
		t.Fatalf("raw rows=%d, want only the known row", raw)
	}

	// Hours touched by corrupt evidence become untrusted; the newest one wins.
	if got, want := receiptWatermark(t, db), time.Date(2025, 3, 1, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("watermark=%v, want %v", got, want)
	}
	before := explicitPlainEvent("44444444-4444-4444-8444-444444444441", time.Date(2025, 1, 1, 0, 30, 0, 0, time.UTC))
	if added, err := store.RecordOnce(before); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("identity inside a corrupt hour: added=%v err=%v, want ErrUsageIdentityUnverified", added, err)
	}
	after := explicitPlainEvent("44444444-4444-4444-8444-444444444442", time.Date(2025, 3, 1, 8, 0, 0, 0, time.UTC))
	if added, err := store.RecordOnce(after); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("unknown hour must block even at the watermark: added=%v err=%v", added, err)
	}

	output := logs.String()
	for _, key := range []string{"2025-01-01T00|claude|c|m|t", "2025-02-03T04|claude|c", "not-an-hour|claude|c|m|t", "2025-03-01T07|c|t|m"} {
		if !strings.Contains(output, key) {
			t.Fatalf("log does not name untrusted key %q:\n%s", key, output)
		}
	}
	if !strings.Contains(output, "level=ERROR") {
		t.Fatalf("untrusted aggregate evidence not logged at ERROR:\n%s", output)
	}
	if strings.Contains(output, corruptBody) {
		t.Fatalf("log leaked the corrupt cell body:\n%s", output)
	}
}

// TestReceiptWatermarkFailureBacksOff keeps a failing migration from
// rescanning the database on every explicit write.
func TestReceiptWatermarkFailureBacksOff(t *testing.T) {
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket([]byte(BucketUsageReceipts))
	}); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	store := NewUsageStore(db)
	store.now = func() time.Time { return clock }
	event := fixedPlainEvent()
	for i := 0; i < 5; i++ {
		if _, err := store.RecordOnce(event); err == nil {
			t.Fatal("missing receipt bucket did not fail the explicit write")
		}
	}
	if got := store.receiptScans.Load(); got != 1 {
		t.Fatalf("scans during backoff=%d, want 1", got)
	}
	clock = clock.Add(time.Hour)
	if _, err := store.RecordOnce(event); err == nil {
		t.Fatal("missing receipt bucket did not fail after backoff")
	}
	if got := store.receiptScans.Load(); got != 2 {
		t.Fatalf("scans after backoff=%d, want 2", got)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket([]byte(BucketUsageReceipts))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordOnce(event); err == nil {
		t.Fatal("repaired migration retried before its backoff elapsed")
	}
	clock = clock.Add(time.Hour)
	if added, err := store.RecordOnce(event); err != nil || !added {
		t.Fatalf("repaired migration: added=%v err=%v", added, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.RecordOnce(event); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.receiptScans.Load(); got != 3 {
		t.Fatalf("scans after success=%d, want 3", got)
	}
}

// TestCountSkipsMalformedAggregateCells matches the v1.0.1 read tolerance for
// the admin request totals while logging the skipped evidence.
func TestCountSkipsMalformedAggregateCells(t *testing.T) {
	logs := captureDefaultLog(t)
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	hour := event.Timestamp.Truncate(time.Hour)
	putAggRaw(t, db, hour.Format(hourLayout)+"|claude|c|m|t", []byte("{not json"))
	putAggRaw(t, db, "v2|"+hour.Format(hourLayout)+"|zz|00|00|00", []byte(`{"requests":7,"success":7}`))
	total, success, err := store.Count(hour, hour)
	if err != nil || total != 1 || success != 1 {
		t.Fatalf("Count=(%d,%d,%v), want (1,1,nil)", total, success, err)
	}
	output := logs.String()
	if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "skipped=2") {
		t.Fatalf("Count did not warn about skipped cells:\n%s", output)
	}
	if strings.Contains(output, "not json") {
		t.Fatalf("Count warning leaked the cell body:\n%s", output)
	}
}

func readRejectedSidecar(t *testing.T, path string) []rejectedUsageLine {
	t.Helper()
	data, err := os.ReadFile(path + ".rejected")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []rejectedUsageLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry rejectedUsageLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("sidecar line %q: %v", line, err)
		}
		lines = append(lines, entry)
	}
	return lines
}

func rawUsageRowCount(t *testing.T, db *bolt.DB) int {
	t.Helper()
	count := 0
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).ForEach(func(_, _ []byte) error {
			count++
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestCountSkipWarningIsRateLimited keeps the overview from logging one WARN
// per request while a malformed cell persists.
func TestCountSkipWarningIsRateLimited(t *testing.T) {
	logs := captureDefaultLog(t)
	db := testDB(t)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := NewUsageStore(db)
	store.now = func() time.Time { return clock }
	hour := clock.Truncate(time.Hour)
	putAggRaw(t, db, hour.Format(hourLayout)+"|claude|c|m|t", []byte("{not json"))
	warnings := func() int { return strings.Count(logs.String(), "usage count skipped malformed") }
	for i := 0; i < 3; i++ {
		if _, _, err := store.Count(hour, hour); err != nil {
			t.Fatal(err)
		}
	}
	if got := warnings(); got != 1 {
		t.Fatalf("warnings within one minute=%d, want 1", got)
	}
	clock = clock.Add(61 * time.Second)
	if _, _, err := store.Count(hour, hour); err != nil {
		t.Fatal(err)
	}
	if got := warnings(); got != 2 {
		t.Fatalf("warnings after the interval=%d, want 2", got)
	}
	if out := logs.String(); !strings.Contains(out, "suppressed_warnings=2") || strings.Contains(out, "not json") {
		t.Fatalf("second warning lacks the suppressed count or leaks the body:\n%s", out)
	}
}
