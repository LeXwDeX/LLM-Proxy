package nosql

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func setReceiptWatermark(t *testing.T, db *bolt.DB, watermark time.Time) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if err := putUsageReceiptWatermark(meta, watermark); err != nil {
			return err
		}
		return meta.Put([]byte(usageReceiptsWatermarkDoneKey), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
}

func journalContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func clientInputTokens(t *testing.T, store *UsageStore, client string, at time.Time) int64 {
	t.Helper()
	totals, err := store.SumByClientRange(client, at, at)
	if err != nil {
		t.Fatal(err)
	}
	var tokens int64
	for _, total := range totals {
		tokens += total.InputTokens
	}
	return tokens
}

// TestUsageJournalMovesPermanentRejectionsToSidecar covers every row the
// primary store can never accept. Each one leaves the pending journal for a
// private sidecar with its original bytes and reason, so the client's
// calibration drains while committed accounting stays unchanged.
func TestUsageJournalMovesPermanentRejectionsToSidecar(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		setup  func(t *testing.T, db *bolt.DB, store *UsageStore) usage.Event
	}{
		{
			name:   "identity before watermark",
			reason: "identity_unverified",
			setup: func(t *testing.T, db *bolt.DB, _ *UsageStore) usage.Event {
				setReceiptWatermark(t, db, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
				return fixedPlainEvent()
			},
		},
		{
			name:   "digest conflict with committed row",
			reason: "digest_conflict",
			setup: func(t *testing.T, _ *bolt.DB, store *UsageStore) usage.Event {
				if err := store.Record(fixedPlainEvent()); err != nil {
					t.Fatal(err)
				}
				conflicting := fixedPlainEvent()
				conflicting.InputTokens = 5
				return conflicting
			},
		},
		{
			name:   "invalid bundle",
			reason: "invalid_bundle",
			setup: func(t *testing.T, _ *bolt.DB, _ *UsageStore) usage.Event {
				event := fixedIdentityBundle()
				event.Components = append(event.Components, event.Components[0])
				return event
			},
		},
		{
			name:   "partially committed bundle",
			reason: "invalid_bundle",
			setup: func(t *testing.T, db *bolt.DB, store *UsageStore) usage.Event {
				event := fixedIdentityBundle()
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
				// A component row pruned before receipts existed.
				deleteUsageRawAndIndex(t, db, usage.ExpandEvent(event)[1])
				return event
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDefaultLog(t)
			db := testDB(t)
			store := NewUsageStore(db)
			event := tc.setup(t, db, store)
			rawBefore, aggBefore := usageRecordCounts(t, db)
			tokensBefore := clientInputTokens(t, store, event.ClientName, event.Timestamp)

			path := filepath.Join(t.TempDir(), "usage.journal")
			line := journalLine(t, event)
			mustWriteFile(t, path, line+"\n")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					// The same durable bytes reappear (for example a restored
					// copy): the sidecar must stay a set, not grow.
					mustWriteFile(t, path, line+"\n")
				}
				recovered, err := journal.Replay()
				if err != nil || len(recovered) != 0 {
					t.Fatalf("replay: recovered=%v err=%v", recovered, err)
				}
				if got := journalContent(t, path); got != "" {
					t.Fatalf("permanently rejected line stayed pending: %s", got)
				}
				if err := journal.DrainClient(event.ClientName); err != nil {
					t.Fatalf("DrainClient still blocked: %v", err)
				}
			}
			rejected := readRejectedSidecar(t, path)
			if len(rejected) != 1 {
				t.Fatalf("sidecar entries=%d, want 1: %+v", len(rejected), rejected)
			}
			entry := rejected[0]
			if entry.Line != line || entry.Reason != tc.reason || entry.RecordID != event.RecordID ||
				entry.Client != event.ClientName || entry.Error == "" || entry.RejectedAt.IsZero() {
				t.Fatalf("sidecar entry=%+v, want original line and reason %q", entry, tc.reason)
			}
			info, err := os.Stat(path + ".rejected")
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("sidecar mode=%v, want 0600", info.Mode().Perm())
			}
			if raw, agg := usageRecordCounts(t, db); raw != rawBefore || agg != aggBefore {
				t.Fatalf("rejection changed accounting: raw=%d/%d agg=%d/%d", raw, rawBefore, agg, aggBefore)
			}
			if got := clientInputTokens(t, store, event.ClientName, event.Timestamp); got != tokensBefore {
				t.Fatalf("rejection changed committed tokens: %d, want %d", got, tokensBefore)
			}
			output := logs.String()
			if !strings.Contains(output, "level=ERROR") || !strings.Contains(output, event.RecordID) || !strings.Contains(output, tc.reason) {
				t.Fatalf("rejection not logged at ERROR with record and reason:\n%s", output)
			}
		})
	}
}

// TestUsageJournalTransientFailureStaysPendingWithoutSidecar is the
// fail-closed counterexample: a storage failure is not a verdict on the row.
func TestUsageJournalTransientFailureStaysPendingWithoutSidecar(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	event := fixedPlainEvent()
	path := filepath.Join(t.TempDir(), "usage.journal")
	line := journalLine(t, event)
	mustWriteFile(t, path, line+"\n")
	journal, err := NewUsageJournal(NewUsageStore(db), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err == nil {
		t.Fatal("replay against a closed database reported success")
	}
	if err := journal.DrainClient(event.ClientName); err == nil {
		t.Fatal("transient failure did not block calibration")
	}
	if got := journalContent(t, path); got != line {
		t.Fatalf("transient failure removed the pending line: %q", got)
	}
	if rejected := readRejectedSidecar(t, path); len(rejected) != 0 {
		t.Fatalf("transient failure wrote a sidecar: %+v", rejected)
	}
}

// TestUsageJournalKeepsRejectedLineWhenSidecarFails never drops a row from the
// journal before a durable sidecar copy exists.
func TestUsageJournalKeepsRejectedLineWhenSidecarFails(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	setReceiptWatermark(t, db, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	event := fixedPlainEvent()
	path := filepath.Join(t.TempDir(), "usage.journal")
	line := journalLine(t, event)
	mustWriteFile(t, path, line+"\n")
	if err := os.Mkdir(path+".rejected", 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err == nil {
		t.Fatal("replay hid a failed sidecar write")
	}
	if err := journal.DrainClient(event.ClientName); err == nil {
		t.Fatal("unpersisted rejection unblocked calibration")
	}
	if got := journalContent(t, path); got != line {
		t.Fatalf("line dropped without a durable sidecar copy: %q", got)
	}
}

// TestAsyncJournalConflictMovesToSidecarOnDrain follows the production
// write-ahead path: the writer's failed commit becomes a foreign line that the
// next calibration drain classifies and moves aside.
func TestAsyncJournalConflictMovesToSidecarOnDrain(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "usage.journal")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	conflicting := fixedPlainEvent()
	conflicting.InputTokens = 5
	if err := journal.Record(conflicting); err != nil {
		t.Fatal(err)
	}
	journal.Flush()
	if err := journal.DrainClient(conflicting.ClientName); err != nil {
		t.Fatalf("DrainClient blocked by a permanent conflict: %v", err)
	}
	rejected := readRejectedSidecar(t, path)
	if len(rejected) != 1 || rejected[0].Reason != "digest_conflict" {
		t.Fatalf("sidecar=%+v, want one digest_conflict", rejected)
	}
	if raw, agg := usageRecordCounts(t, db); raw != 1 || agg != 1 {
		t.Fatalf("conflict changed accounting: raw=%d agg=%d", raw, agg)
	}
	if got := clientInputTokens(t, store, conflicting.ClientName, conflicting.Timestamp); got != fixedPlainEvent().InputTokens {
		t.Fatalf("committed tokens=%d, want %d", got, fixedPlainEvent().InputTokens)
	}
}
