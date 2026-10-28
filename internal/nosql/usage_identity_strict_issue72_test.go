package nosql

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// A missing identity is not proven new merely because it is after the capped
// migration watermark. The uncapped boundary and an older higher boundary
// must both be honored, including for generated IDs and batch writes.
func TestIssue72StrictIdentityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		old    time.Time
		future time.Time
		marker string
	}{
		{"new scan uncapped", time.Time{}, boundScanTime.Add(48 * time.Hour), "none"},
		{"v1 old boundary higher", boundScanTime.Add(60 * time.Hour).Truncate(time.Hour), boundScanTime.Add(48 * time.Hour), "v1"},
		{"v2 previously completed", boundScanTime.Truncate(time.Hour), boundScanTime.Add(48 * time.Hour), "v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			future := tc.future.Truncate(time.Hour)
			putAggRaw(t, db, future.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
			if tc.marker != "none" {
				oldWatermarkDone(t, db, tc.old)
			}
			if tc.marker == "v2" {
				if err := db.Update(func(tx *bolt.Tx) error {
					meta := tx.Bucket([]byte(BucketMeta))
					if err := meta.Put([]byte(usageReceiptsUntrustedThroughKey), []byte(future.Add(time.Hour).Format(time.RFC3339Nano))); err != nil {
						return err
					}
					return meta.Put([]byte(usageReceiptsWatermarkDoneV2Key), []byte("1"))
				}); err != nil {
					t.Fatal(err)
				}
			}
			store := clockedStore(db)
			boundary := future.Add(time.Hour)
			if tc.old.After(boundary) {
				boundary = tc.old
			}
			blocked := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaab01", boundary.Add(-time.Nanosecond))
			blocked.Model = "advisor"
			if exists, err := store.RecordExists(blocked); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Errorf("lookup below %s: exists=%v err=%v", boundary, exists, err)
			}
			if added, err := store.RecordOnce(blocked); added || !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Errorf("explicit below %s: added=%v err=%v", boundary, added, err)
			}
			generated := blocked
			generated.RecordID = ""
			if exists, err := store.RecordExists(generated); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Errorf("generated lookup below %s: exists=%v err=%v", boundary, exists, err)
			}
			if added, err := store.RecordOnce(generated); added || !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Errorf("generated below %s: added=%v err=%v", boundary, added, err)
			}
			atBoundary := blocked
			atBoundary.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaab02"
			atBoundary.Timestamp = boundary
			if err := store.RecordBatch([]usage.Event{atBoundary, generated}); !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Errorf("batch containing unverified identity: err=%v", err)
			}
			if rawUsageRowCount(t, db) != 0 {
				t.Fatal("rejected identity or failed batch changed raw history")
			}
			if exists, err := store.RecordExists(atBoundary); exists || err != nil {
				t.Errorf("lookup at %s: exists=%v err=%v", boundary, exists, err)
			}
			if added, err := store.RecordOnce(atBoundary); !added || err != nil {
				t.Errorf("write at %s: added=%v err=%v", boundary, added, err)
			}
		})
	}
}

func TestIssue72StrictUnknownHourAndKnownBundle(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	rows := usage.ExpandEvent(bundle)
	deleteUsageRawAndIndex(t, db, rows[0])
	digest, err := usage.RetryEquivalentDigest(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	putUsageReceipt(t, db, receiptKey(rows[0]), usageReceipt{Version: usageReceiptVersion, Digest: digest})
	putAggRaw(t, db, "not-an-hour|claude|alice|executor|anthropic", []byte(corruptCell))
	oldWatermarkDone(t, db, time.Time{})
	store = clockedStore(db)

	if exists, err := store.RecordExists(bundle); !exists || err != nil {
		t.Errorf("mixed raw/receipt lookup: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(bundle); added || err != nil {
		t.Errorf("mixed raw/receipt replay: added=%v err=%v", added, err)
	}
	conflict := usage.CloneEvent(bundle)
	conflict.Components[0].InputTokens++
	// The conflicting root has only a digest receipt, which does not prove its owner.
	if exists, err := store.RecordExists(conflict); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("conflicting child lookup: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(conflict); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("conflicting child write: added=%v err=%v", added, err)
	}
	for _, at := range []time.Time{boundScanTime.Add(-48 * time.Hour), boundScanTime, boundScanTime.Add(48 * time.Hour)} {
		unknown := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaab03", at)
		if exists, err := store.RecordExists(unknown); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
			t.Errorf("unknown-hour lookup %s: exists=%v err=%v", at, exists, err)
		}
		if added, err := store.RecordOnce(unknown); added || !errors.Is(err, ErrUsageIdentityUnverified) {
			t.Errorf("unknown-hour write %s: added=%v err=%v", at, added, err)
		}
		unknown.RecordID = ""
		if exists, err := store.RecordExists(unknown); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
			t.Errorf("unknown-hour generated lookup %s: exists=%v err=%v", at, exists, err)
		}
		if added, err := store.RecordOnce(unknown); added || !errors.Is(err, ErrUsageIdentityUnverified) {
			t.Errorf("unknown-hour generated %s: added=%v err=%v", at, added, err)
		}
	}
	deleteUsageRawAndIndex(t, db, rows[1])
	before := indexBucketSnapshot(t, db, BucketUsageEvents)
	if exists, err := store.RecordExists(bundle); exists || !errors.Is(err, ErrUsageBundleInvalid) {
		t.Errorf("partial bundle lookup: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(bundle); added || !errors.Is(err, ErrUsageBundleInvalid) {
		t.Errorf("partial bundle write: added=%v err=%v", added, err)
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, BucketUsageEvents)) {
		t.Error("partial bundle wrote raw history")
	}
}

// A failed migration must still allow an entire verified bundle to deduplicate;
// it must not admit a new bundle or hide a partial bundle.
func TestIssue72StrictKnownBundleBeforeFailedMigration(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	rows := usage.ExpandEvent(bundle)
	deleteUsageRawAndIndex(t, db, rows[0])
	digest, err := usage.RetryEquivalentDigest(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	putUsageReceipt(t, db, receiptKey(rows[0]), usageReceipt{Version: usageReceiptVersion, Digest: digest})
	oldWatermarkDone(t, db, time.Time{})
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageReceiptsUntrustedThroughKey), []byte("invalid"))
	}); err != nil {
		t.Fatal(err)
	}
	store = clockedStore(db)
	if exists, err := store.RecordExists(bundle); !exists || err != nil {
		t.Errorf("verified bundle lookup after failed migration: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(bundle); added || err != nil {
		t.Errorf("verified bundle retry after failed migration: added=%v err=%v", added, err)
	}
	if err := store.RecordBatch([]usage.Event{bundle}); err != nil {
		t.Errorf("verified bundle batch after failed migration: %v", err)
	}
	unknown := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaab05", boundScanTime)
	if exists, err := store.RecordExists(unknown); exists || err == nil {
		t.Errorf("unknown lookup after failed migration: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(unknown); added || err == nil {
		t.Errorf("unknown write after failed migration: added=%v err=%v", added, err)
	}
	if err := store.RecordBatch([]usage.Event{bundle, unknown}); err == nil {
		t.Error("unknown batch accepted after failed migration")
	}
	deleteUsageRawAndIndex(t, db, rows[1])
	if exists, err := store.RecordExists(bundle); exists || !errors.Is(err, ErrUsageBundleInvalid) {
		t.Errorf("partial bundle lookup after failed migration: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(bundle); added || !errors.Is(err, ErrUsageBundleInvalid) {
		t.Errorf("partial bundle write after failed migration: added=%v err=%v", added, err)
	}
}

// Losing a previously initialized receipt bucket loses identities with no
// guaranteed time bound, even if other rollups are also gone.
func TestIssue72StrictRecreatedReceiptBucketHasUnknownRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaab04", boundScanTime.Add(48*time.Hour))
	store := clockedStore(db)
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageReceipts)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketUsageAggHourly)).Delete(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = clockedStore(db)
	if exists, err := store.RecordExists(event); exists || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("lost receipt lookup: exists=%v err=%v", exists, err)
	}
	if added, err := store.RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("lost receipt replay: added=%v err=%v", added, err)
	}
	if rawUsageRowCount(t, db) != 0 {
		t.Error("lost receipt replay resurrected raw history")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := clockedStore(db).RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("lost receipt after second restart: added=%v err=%v", added, err)
	}
}

// A real new event in an untrusted range cannot be distinguished from a
// retry. If it cannot be committed, its evidence must remain durable and
// quota calibration must not treat the empty ledger as complete.
func TestIssue72StrictNewEventCannotDisappearFromAccounting(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			db := testDB(t)
			future := boundScanTime.Add(48 * time.Hour).Truncate(time.Hour)
			putAggRaw(t, db, future.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
			store := clockedStore(db)
			path := filepath.Join(t.TempDir(), "usage.journal")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if async {
				if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			event := boundEvent("", boundScanTime)
			event.Model = "advisor"
			event.RequestID = "issue72-untrusted-new-event"
			if err := journal.Record(event); err != nil {
				t.Fatalf("durable journal record: %v", err)
			}
			journal.Flush()
			_, _ = journal.Replay() // A transient failure is safe only if evidence stays pending.
			if rawUsageRowCount(t, db) != 0 {
				return
			}
			data, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			preserved := strings.Contains(string(data), event.RequestID)
			for _, entry := range readRejectedSidecar(t, path) {
				preserved = preserved || strings.Contains(string(entry.Line), event.RequestID)
			}
			if !preserved {
				t.Fatal("uncommitted new event has no durable journal or rejected evidence")
			}
			if err := journal.DrainClient(event.ClientName); err == nil {
				t.Fatal("uncommitted new event was removed from accounting, but quota calibration was allowed")
			}
		})
	}
}
