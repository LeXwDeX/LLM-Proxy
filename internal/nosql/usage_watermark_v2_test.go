package nosql

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const (
	watermarkV2DoneForTest  = "usage_receipts_watermark_done_v2"
	unknownHourForTest      = "usage_receipts_unknown_aggregate_hour_v2"
	untrustedThroughForTest = "usage_receipts_untrusted_through_v2"
)

func watermarkMeta(t *testing.T, db *bolt.DB, key string) string {
	t.Helper()
	var value string
	if err := db.View(func(tx *bolt.Tx) error {
		value = string(tx.Bucket([]byte(BucketMeta)).Get([]byte(key)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// Simulates a database whose old algorithm completed before this version.
func oldWatermarkDone(t *testing.T, db *bolt.DB, watermark time.Time) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if err := putUsageReceiptWatermark(meta, watermark); err != nil {
			return err
		}
		if err := meta.Delete([]byte(watermarkV2DoneForTest)); err != nil {
			return err
		}
		return meta.Put([]byte(usageReceiptsWatermarkDoneKey), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWatermarkV2LegacyPruneBackfillAndReplay(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	pruned := fixedPlainEvent()
	if err := store.Record(pruned); err != nil {
		t.Fatal(err)
	}
	moveAggCellToLegacy(t, db, pruned)
	deleteUsageRawAndIndex(t, db, pruned) // Old retention did not write receipts.
	if count := usageReceiptCount(t, db); count != 0 {
		t.Fatalf("legacy cleanup created %d receipts", count)
	}
	earlier := pruned
	earlier.RecordID = fixedLaterRecordID
	earlier.Timestamp = pruned.Timestamp.Add(-48 * time.Hour)
	if err := store.Record(earlier); err != nil {
		t.Fatal(err)
	}
	corruptKey := string(legacyAggKey(pruned.Timestamp, pruned.EndpointType, pruned.ClientName, pruned.Model, pruned.Target))
	putAggRaw(t, db, corruptKey, []byte(corruptCell))
	oldWatermarkDone(t, db, time.Time{})
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageAggBuiltMetaKey), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	// Match main's startup order: quota backfill sees only the earlier raw row.
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	beforeRaw := indexBucketSnapshot(t, db, BucketUsageEvents)
	beforeAgg := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	beforeQuota := indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)
	upgraded := clockedStore(db)
	if err := upgraded.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	want := pruned.Timestamp.Truncate(time.Hour).Add(time.Hour)
	if got := receiptWatermark(t, db); got.Before(want) {
		t.Errorf("legacy done marker hid corrupt pruned hour: watermark=%v want at least %v", got, want)
	}
	if got := watermarkMeta(t, db, watermarkV2DoneForTest); got == "" {
		t.Error("old done marker was not replaced by a completed v2 scan")
	}
	if added, err := upgraded.RecordOnce(earlier); err != nil || added {
		t.Errorf("existing raw identity: added=%v err=%v", added, err)
	}

	path := filepath.Join(t.TempDir(), "legacy-usage.journal")
	line := journalLine(t, pruned)
	firstTime := pruned
	firstTime.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3"
	firstTime.Timestamp = firstTime.Timestamp.Add(time.Minute)
	firstTimeLine := journalLine(t, firstTime)
	mustWriteFile(t, path, line+"\n"+firstTimeLine+"\n")
	journal, err := NewUsageJournal(upgraded, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	for i := 0; i < 2; i++ {
		if _, err := journal.Replay(); err != nil {
			t.Errorf("replay %d: %v", i, err)
		}
	}
	rejected := readRejectedSidecar(t, path)
	if len(rejected) != 2 || rejected[0].Reason != usageRejectIdentityUnverified || string(rejected[0].Line) != line ||
		rejected[1].Reason != usageRejectIdentityUnverified || string(rejected[1].Line) != firstTimeLine {
		t.Errorf("unknown replay lost its original evidence: %+v", rejected)
	}
	if !reflect.DeepEqual(beforeRaw, indexBucketSnapshot(t, db, BucketUsageEvents)) ||
		!reflect.DeepEqual(beforeAgg, indexBucketSnapshot(t, db, BucketUsageAggHourly)) ||
		!reflect.DeepEqual(beforeQuota, indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)) {
		t.Error("unknown replay changed raw or rollup evidence")
	}
	if count := usageReceiptCount(t, db); count != 0 {
		t.Errorf("unknown identity produced %d fabricated receipts", count)
	}
	from, to := pruned.Timestamp.Truncate(time.Hour), pruned.Timestamp.Truncate(time.Hour)
	filter := usage.Filter{ClientName: pruned.ClientName, From: &from, To: &to}
	units, err := upgraded.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
		t.Errorf("missing historical quota evidence was reported as a complete cost: units=%+v err=%v", units, err)
	}
	quota, err := upgraded.SumQuotaByClientRange(pruned.ClientName, from, to)
	if err == nil || len(quota) != 0 {
		t.Errorf("quota fallback did not report the corrupt historical aggregate: quota=%+v err=%v", quota, err)
	}
	if _, err := upgraded.Aggregate(filter, "hour", replayCosts); err == nil {
		t.Error("corrupt original aggregate was reported as complete")
	}
}

func TestWatermarkV2RescansCorruptHourAfterEarlierRaw(t *testing.T) {
	db := testDB(t)
	earlier := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", boundScanTime.Add(-48*time.Hour))
	seedRawThenResetWatermark(t, db, earlier)
	bad := boundScanTime.Add(-time.Hour).Truncate(time.Hour)
	putAggRaw(t, db, bad.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	if err := clockedStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if got, want := receiptWatermark(t, db), bad.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("earlier surviving raw was treated as proof of intact later history: got %v want %v", got, want)
	}
}

func TestWatermarkV2NeverLowersOldBoundary(t *testing.T) {
	for _, withDone := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_v1_done", true: "v1_done"}[withDone], func(t *testing.T) {
			db := testDB(t)
			old := boundScanTime.Add(-time.Hour).Truncate(time.Hour)
			oldWatermarkDone(t, db, old)
			if !withDone {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageReceiptsWatermarkDoneKey))
				}); err != nil {
					t.Fatal(err)
				}
			}
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			if got := receiptWatermark(t, db); !got.Equal(old) {
				t.Errorf("old boundary fell from %v to %v", old, got)
			}
			if got := watermarkMeta(t, db, watermarkV2DoneForTest); got == "" {
				t.Error("new algorithm completion not persisted")
			}
			oldEvent := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", old.Add(-time.Minute))
			if added, err := store.RecordOnce(oldEvent); added || !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Errorf("old unverified identity: added=%v err=%v", added, err)
			}
		})
	}
}

func TestWatermarkV2PersistsUnattributedAndFutureEvidence(t *testing.T) {
	db := testDB(t)
	putAggRaw(t, db, "not-an-hour|claude|alice|executor|anthropic", []byte(corruptCell))
	future := boundScanTime.Add(48 * time.Hour).Truncate(time.Hour)
	putAggRaw(t, db, future.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	if err := clockedStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if got := watermarkMeta(t, db, unknownHourForTest); got == "" {
		t.Error("unattributed corrupt hour is not persisted as unknown")
	}
	if got := watermarkMeta(t, db, untrustedThroughForTest); got != future.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("future untrusted hour hidden by scan-hour cap: got %q want %q", got, future.Add(time.Hour).Format(time.RFC3339Nano))
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := clockedStore(reopened).EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if got := watermarkMeta(t, reopened, unknownHourForTest); got == "" {
		t.Error("restart lost the unknown-hour diagnostic")
	}
	if got := watermarkMeta(t, reopened, untrustedThroughForTest); got != future.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("restart lost future untrusted evidence: %q", got)
	}
}

func TestWatermarkV2BoundaryAndKnownIdentities(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	boundary := boundScanTime.Add(-time.Hour).Truncate(time.Hour)
	raw := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4", boundary.Add(-time.Minute))
	receipted := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa5", boundary.Add(-2*time.Minute))
	if err := store.Record(raw); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(receipted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(receipted.Timestamp.Add(2*time.Second), 10); err != nil {
		t.Fatal(err)
	}
	oldWatermarkDone(t, db, boundary)
	store = clockedStore(db)
	for _, event := range []usage.Event{raw, receipted} {
		if added, err := store.RecordOnce(event); err != nil || added {
			t.Errorf("verified identity %s: added=%v err=%v", event.RecordID, added, err)
		}
		conflict := event
		conflict.InputTokens++
		want := ErrUsageRecordConflict
		if event.RecordID == receipted.RecordID {
			want = ErrUsageIdentityUnverified
		}
		if added, err := store.RecordOnce(conflict); added || !errors.Is(err, want) {
			t.Errorf("conflicting identity %s: added=%v err=%v", event.RecordID, added, err)
		}
	}
	unknown := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa6", boundary.Add(-time.Nanosecond))
	if added, err := store.RecordOnce(unknown); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Errorf("unverified just below boundary: added=%v err=%v", added, err)
	}
	fresh := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa7", boundary)
	if added, err := store.RecordOnce(fresh); err != nil || !added {
		t.Errorf("fresh identity at boundary: added=%v err=%v", added, err)
	}
	if added, err := store.RecordOnce(fresh); err != nil || added {
		t.Errorf("fresh identity retry: added=%v err=%v", added, err)
	}
}

func TestWatermarkV2FailureRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	bad := boundScanTime.Add(-time.Hour).Truncate(time.Hour)
	putAggRaw(t, db, bad.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	oldWatermarkDone(t, db, time.Time{})
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(untrustedThroughForTest), []byte("broken timestamp"))
	}); err != nil {
		t.Fatal(err)
	}
	clock := boundScanTime
	store := NewUsageStore(db)
	store.now = func() time.Time { return clock }
	if err := store.EnsureUsageReceipts(); err == nil {
		t.Fatal("invalid diagnostic accepted")
	}
	if got := receiptWatermark(t, db); !got.IsZero() {
		t.Errorf("failed transaction left partial watermark %v", got)
	}
	if got := watermarkMeta(t, db, watermarkV2DoneForTest); got != "" {
		t.Errorf("failed transaction marked complete: %q", got)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(untrustedThroughForTest))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureUsageReceipts(); err == nil || store.receiptScans.Load() != 1 {
		t.Fatalf("failed attempt did not back off: err=%v scans=%d", err, store.receiptScans.Load())
	}
	clock = clock.Add(usageReceiptRetryMin)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if got, want := receiptWatermark(t, db), bad.Add(time.Hour); !got.Equal(want) {
		t.Errorf("retry watermark=%v, want %v", got, want)
	}
	if got := watermarkMeta(t, db, watermarkV2DoneForTest); got == "" {
		t.Error("successful retry did not persist completion")
	}
}

func TestWatermarkV2ReceiptBucketRecreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	old := boundScanTime.Add(-time.Hour).Truncate(time.Hour)
	oldWatermarkDone(t, db, old)
	if err := clockedStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketUsageReceipts)) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{usageReceiptsWatermarkDoneKey, watermarkV2DoneForTest} {
		if got := watermarkMeta(t, db, key); got != "" {
			t.Errorf("recreated receipt bucket kept marker %s=%q", key, got)
		}
	}
	if got := receiptWatermark(t, db); !got.Equal(old) {
		t.Errorf("recreated receipt bucket lost old boundary %v: got %v", old, got)
	}
	if err := clockedStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if got := receiptWatermark(t, db); !got.Equal(old) {
		t.Errorf("rescan lowered old boundary %v to %v", old, got)
	}
	if got := watermarkMeta(t, db, watermarkV2DoneForTest); got == "" {
		t.Error("rescan did not restore the v2 marker")
	}
}

func TestWatermarkV2ConcurrentMigrationAndWrite(t *testing.T) {
	db := testDB(t)
	bad := boundScanTime.Add(-time.Hour).Truncate(time.Hour)
	putAggRaw(t, db, bad.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
	oldWatermarkDone(t, db, time.Time{})
	beforeAgg := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	stores := []*UsageStore{clockedStore(db), clockedStore(db)}
	event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", bad.Add(5*time.Minute))
	event.Model = "advisor"
	var added bool
	var errorsByWorker [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errorsByWorker[0] = stores[0].EnsureUsageReceipts()
	}()
	go func() {
		defer wg.Done()
		added, errorsByWorker[1] = stores[1].RecordOnce(event)
	}()
	wg.Wait()
	if errorsByWorker[0] != nil || added || !errors.Is(errorsByWorker[1], ErrUsageIdentityUnverified) {
		t.Fatalf("concurrent scan/write: added=%v migration=%v write=%v", added, errorsByWorker[0], errorsByWorker[1])
	}
	if got, want := receiptWatermark(t, db), bad.Add(time.Hour); !got.Equal(want) {
		t.Errorf("watermark=%v, want %v", got, want)
	}
	if !reflect.DeepEqual(beforeAgg, indexBucketSnapshot(t, db, BucketUsageAggHourly)) || rawUsageRowCount(t, db) != 0 {
		t.Error("concurrent scan/write changed accounting evidence")
	}
	for _, store := range stores {
		if err := store.EnsureUsageReceipts(); err != nil {
			t.Fatal(err)
		}
	}
}

// A current or future corrupt hour cannot prove that an absent tuple is new.
func TestWatermarkV2CurrentAndFutureUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"current", boundScanTime.Add(-10 * time.Minute)},
		{"future", boundScanTime.Add(48 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8", tc.at)
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			moveAggCellToLegacy(t, db, event)
			deleteUsageRawAndIndex(t, db, event)
			earlier := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa9", boundScanTime.Add(-48*time.Hour))
			if err := store.Record(earlier); err != nil {
				t.Fatal(err)
			}
			key := legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
			putAggRaw(t, db, string(key), []byte(corruptCell))
			oldWatermarkDone(t, db, time.Time{})
			if err := BackfillQuotaUsageAgg(db); err != nil {
				t.Fatal(err)
			}
			store = clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			want := event.Timestamp.Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339Nano)
			if got := watermarkMeta(t, db, untrustedThroughForTest); got != want {
				t.Fatalf("untrusted evidence=%q, want %q", got, want)
			}
			added, err := store.RecordOnce(event)
			if added || !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Fatalf("previously pruned identity was not rejected: added=%v err=%v", added, err)
			}
		})
	}
}
