package nosql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// awaitUsageRetentionStats waits for one asynchronous snapshot to complete.
// A failure snapshot (transaction_failed, store_unavailable) is complete too.
func awaitUsageRetentionStats(t *testing.T, store *UsageStore) UsageRetentionStats {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stats := store.UsageRetentionSnapshot()
		if stats.Reason != UsageRetentionReasonNotReady {
			return stats
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("usage retention snapshot did not complete in time")
	return UsageRetentionStats{}
}

func waitUsageRetentionIdle(t *testing.T, store *UsageStore) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		store.retention.mu.Lock()
		running := store.retention.refreshing
		store.retention.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("usage retention refresh did not finish in time")
}

func putRetentionKeys(t *testing.T, db *bolt.DB, bucket string, keys ...string) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			var err error
			b, err = tx.CreateBucket([]byte(bucket))
			if err != nil {
				return err
			}
		}
		for _, key := range keys {
			if err := b.Put([]byte(key), []byte("{}")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed bucket %q: %v", bucket, err)
	}
}

func putRetentionMeta(t *testing.T, db *bolt.DB, key, value string) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return fmt.Errorf("meta bucket missing")
		}
		return meta.Put([]byte(key), []byte(value))
	}); err != nil {
		t.Fatalf("seed meta %q: %v", key, err)
	}
}

func TestUsageRetentionStatsEmptyDatabase(t *testing.T) {
	store := NewUsageStore(testDB(t))
	stats := awaitUsageRetentionStats(t, store)

	if !stats.Available || stats.Reason != "" {
		t.Fatalf("empty database snapshot = available %v reason %q, want available", stats.Available, stats.Reason)
	}
	if stats.CapturedAt == nil || stats.CapturedAt.IsZero() {
		t.Fatalf("captured_at missing: %+v", stats)
	}
	if stats.Buckets == nil || stats.Events == nil || stats.Metadata == nil {
		t.Fatalf("empty database snapshot is partial: %+v", stats)
	}
	// OpenDB creates the receipts bucket eagerly; the three optional indexes
	// stay absent until their first write, and that must be observable.
	wantPresent := map[string]bool{
		"receipts":           true,
		"receipt_owners":     false,
		"raw_owners":         false,
		"fresh_attestations": false,
	}
	for name, bucket := range map[string]UsageRetentionBucketUsage{
		"receipts":           stats.Buckets.Receipts,
		"receipt_owners":     stats.Buckets.ReceiptOwners,
		"raw_owners":         stats.Buckets.RawOwners,
		"fresh_attestations": stats.Buckets.FreshAttestations,
	} {
		if bucket.Present != wantPresent[name] {
			t.Fatalf("empty bucket %s present = %v, want %v", name, bucket.Present, wantPresent[name])
		}
		if bucket.Entries != 0 {
			t.Fatalf("empty bucket %s = %+v, want zero entries", name, bucket)
		}
	}
	if stats.Events.Earliest != nil || stats.Events.Latest != nil || stats.Events.InvalidKeys != 0 {
		t.Fatalf("empty event range = %+v, want no times and no invalid keys", stats.Events)
	}
	if stats.Metadata.LegacyUnknownBefore != nil || stats.Metadata.UntrustedThrough != nil ||
		stats.Metadata.LegacyUnknownBeforeInvalid || stats.Metadata.UntrustedThroughInvalid ||
		stats.Metadata.WatermarkDoneV1 || stats.Metadata.WatermarkDoneV2 || stats.Metadata.UnknownAggregateHour {
		t.Fatalf("empty metadata = %+v, want all unset", stats.Metadata)
	}
}

func TestUsageRetentionStatsCountsEveryRetentionBucket(t *testing.T) {
	db := testDB(t)
	putRetentionKeys(t, db, BucketUsageReceipts, "receipt-key-secret-1", "receipt-key-secret-2", "receipt-key-secret-3")
	putRetentionKeys(t, db, receiptOwnersBucket, "owner-key-secret-1", "owner-key-secret-2")
	putRetentionKeys(t, db, rawOwnerBucket, "raw-owner-key-secret-1")
	putRetentionKeys(t, db, freshAttestationBucket, "attestation-key-secret-1", "attestation-key-secret-2", "attestation-key-secret-3", "attestation-key-secret-4")

	stats := awaitUsageRetentionStats(t, NewUsageStore(db))
	if !stats.Available {
		t.Fatalf("snapshot unavailable: reason %q", stats.Reason)
	}
	want := map[string]struct {
		got     UsageRetentionBucketUsage
		entries uint64
	}{
		"receipts":           {stats.Buckets.Receipts, 3},
		"receipt_owners":     {stats.Buckets.ReceiptOwners, 2},
		"raw_owners":         {stats.Buckets.RawOwners, 1},
		"fresh_attestations": {stats.Buckets.FreshAttestations, 4},
	}
	for name, check := range want {
		if !check.got.Present {
			t.Errorf("%s bucket not reported present", name)
		}
		if check.got.Entries != check.entries {
			t.Errorf("%s entries = %d, want %d", name, check.got.Entries, check.entries)
		}
		if check.got.InUseBytes == 0 {
			t.Errorf("%s byte estimate = %+v, want non-zero in-use bytes", name, check.got)
		}
	}

	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal retention stats: %v", err)
	}
	for _, secret := range []string{
		"receipt-key-secret", "owner-key-secret", "raw-owner-key-secret", "attestation-key-secret",
		"usage_record_receipts_v1", "usage_receipt_owner_hashes_v1", "usage_raw_owners_v1", "usage_fresh_commit_attestations_v1",
	} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("retention stats leaked %q: %s", secret, encoded)
		}
	}
}

func TestUsageRetentionBucketPageEstimate(t *testing.T) {
	db := testDB(t)
	keys := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		keys = append(keys, fmt.Sprintf("receipt-%03d-%s", i, strings.Repeat("x", 128)))
	}
	putRetentionKeys(t, db, BucketUsageReceipts, keys...)

	stats := awaitUsageRetentionStats(t, NewUsageStore(db))
	bucket := stats.Buckets.Receipts
	if bucket.Entries != 200 {
		t.Fatalf("receipts entries = %d, want 200", bucket.Entries)
	}
	if bucket.Pages < 1 || bucket.InUseBytes == 0 {
		t.Fatalf("receipts page estimate = %+v, want at least one logical page and non-zero bytes", bucket)
	}
}

func TestUsageRetentionEventRangeReportsParseableBoundaries(t *testing.T) {
	db := testDB(t)
	earliest := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	latest := time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)
	putRetentionKeys(t, db, BucketUsageEvents,
		"0000-not-a-time",
		usageKey(earliest, "record-secret-earliest"),
		usageKey(latest, "record-secret-latest"),
		"zzzz-not-a-time",
	)

	stats := awaitUsageRetentionStats(t, NewUsageStore(db))
	if !stats.Available {
		t.Fatalf("snapshot unavailable: reason %q", stats.Reason)
	}
	if stats.Events.Earliest == nil || !stats.Events.Earliest.Equal(earliest) {
		t.Fatalf("earliest = %v, want %v", stats.Events.Earliest, earliest)
	}
	if stats.Events.Latest == nil || !stats.Events.Latest.Equal(latest) {
		t.Fatalf("latest = %v, want %v", stats.Events.Latest, latest)
	}
	if stats.Events.InvalidKeys != 2 {
		t.Fatalf("invalid_keys = %d, want 2", stats.Events.InvalidKeys)
	}

	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal retention stats: %v", err)
	}
	for _, secret := range []string{"record-secret-earliest", "record-secret-latest", "not-a-time"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("event range leaked %q: %s", secret, encoded)
		}
	}
}

func TestUsageRetentionEventRangeAllInvalidNeverGuesses(t *testing.T) {
	db := testDB(t)
	// The middle key is not canonical: its offset does not round-trip through
	// RFC3339Nano in UTC, so it must count as invalid rather than be parsed.
	putRetentionKeys(t, db, BucketUsageEvents,
		"0000-not-a-time",
		"2026-01-02T03:04:05.000000001+00:00_not-canonical",
		"zzzz-not-a-time",
	)

	stats := awaitUsageRetentionStats(t, NewUsageStore(db))
	if stats.Events.Earliest != nil || stats.Events.Latest != nil {
		t.Fatalf("all-invalid range invented times: %+v", stats.Events)
	}
	if stats.Events.InvalidKeys != 3 {
		t.Fatalf("invalid_keys = %d, want 3 (each key counted once)", stats.Events.InvalidKeys)
	}
}

func TestUsageRetentionMetadata(t *testing.T) {
	t.Run("watermarks and markers", func(t *testing.T) {
		db := testDB(t)
		legacy := time.Date(2026, 2, 3, 4, 5, 6, 789, time.UTC)
		untrusted := time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC)
		putRetentionMeta(t, db, usageReceiptsWatermarkKey, legacy.Format(time.RFC3339Nano))
		putRetentionMeta(t, db, usageReceiptsUntrustedThroughKey, untrusted.Format(time.RFC3339Nano))
		putRetentionMeta(t, db, usageReceiptsWatermarkDoneKey, "1")
		putRetentionMeta(t, db, usageReceiptsUnknownHourKey, "1")

		stats := awaitUsageRetentionStats(t, NewUsageStore(db))
		meta := stats.Metadata
		if meta.LegacyUnknownBefore == nil || !meta.LegacyUnknownBefore.Equal(legacy) || meta.LegacyUnknownBeforeInvalid {
			t.Fatalf("legacy watermark = %+v, want %v", meta, legacy)
		}
		if meta.UntrustedThrough == nil || !meta.UntrustedThrough.Equal(untrusted) || meta.UntrustedThroughInvalid {
			t.Fatalf("untrusted through = %+v, want %v", meta, untrusted)
		}
		if !meta.WatermarkDoneV1 || meta.WatermarkDoneV2 || !meta.UnknownAggregateHour {
			t.Fatalf("markers = %+v, want done_v1 and unknown_hour only", meta)
		}
	})

	t.Run("corrupt values are flagged and never echoed", func(t *testing.T) {
		db := testDB(t)
		putRetentionMeta(t, db, usageReceiptsWatermarkKey, "not-a-timestamp")
		putRetentionMeta(t, db, usageReceiptsUntrustedThroughKey, "also-not-a-timestamp")
		putRetentionMeta(t, db, usageReceiptsWatermarkDoneV2Key, "1")

		stats := awaitUsageRetentionStats(t, NewUsageStore(db))
		meta := stats.Metadata
		if meta.LegacyUnknownBefore != nil || !meta.LegacyUnknownBeforeInvalid {
			t.Fatalf("corrupt legacy watermark = %+v, want invalid and no value", meta)
		}
		if meta.UntrustedThrough != nil || !meta.UntrustedThroughInvalid {
			t.Fatalf("corrupt untrusted watermark = %+v, want invalid and no value", meta)
		}
		if meta.WatermarkDoneV1 || !meta.WatermarkDoneV2 {
			t.Fatalf("markers = %+v, want only done_v2", meta)
		}

		encoded, err := json.Marshal(stats)
		if err != nil {
			t.Fatalf("marshal retention stats: %v", err)
		}
		for _, secret := range []string{"not-a-timestamp", "also-not-a-timestamp"} {
			if bytes.Contains(encoded, []byte(secret)) {
				t.Fatalf("metadata leaked raw value %q: %s", secret, encoded)
			}
		}
	})
}

func TestUsageRetentionSnapshotUnavailableWhenDatabaseClosed(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	stats := awaitUsageRetentionStats(t, store)
	if stats.Available || stats.Reason != UsageRetentionReasonTransactionFailed {
		t.Fatalf("closed database snapshot = available %v reason %q, want %q", stats.Available, stats.Reason, UsageRetentionReasonTransactionFailed)
	}
	if stats.Buckets != nil || stats.Events != nil || stats.Metadata != nil {
		t.Fatalf("unavailable snapshot carried detail: %+v", stats)
	}
}

func TestUsageRetentionSnapshotNilStore(t *testing.T) {
	var store *UsageStore
	for name, s := range map[string]*UsageStore{"nil store": store, "nil database": NewUsageStore(nil)} {
		stats := s.UsageRetentionSnapshot()
		if stats.Available || stats.Reason != UsageRetentionReasonStoreUnavailable {
			t.Fatalf("%s snapshot = available %v reason %q, want %q", name, stats.Available, stats.Reason, UsageRetentionReasonStoreUnavailable)
		}
	}
}

func TestUsageRetentionSnapshotNotReadyThenRefreshesOnce(t *testing.T) {
	store := NewUsageStore(testDB(t))
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var loads atomic.Int64
	store.retention.load = func() UsageRetentionStats {
		loads.Add(1)
		once.Do(func() { close(started) })
		<-release
		return store.loadUsageRetentionStats()
	}

	first := store.UsageRetentionSnapshot()
	if first.Available || first.Reason != UsageRetentionReasonNotReady {
		t.Fatalf("first snapshot = available %v reason %q, want %q", first.Available, first.Reason, UsageRetentionReasonNotReady)
	}
	if first.CapturedAt != nil || first.Buckets != nil || first.Events != nil || first.Metadata != nil {
		t.Fatalf("not-ready snapshot carried detail: %+v", first)
	}
	<-started
	second := store.UsageRetentionSnapshot()
	if second.Available || second.Reason != UsageRetentionReasonNotReady {
		t.Fatalf("second snapshot = available %v reason %q, want %q", second.Available, second.Reason, UsageRetentionReasonNotReady)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("refreshes while one is in flight = %d, want 1", got)
	}

	close(release)
	waitUsageRetentionIdle(t, store)
	stats := awaitUsageRetentionStats(t, store)
	if !stats.Available {
		t.Fatalf("snapshot after refresh = available %v reason %q", stats.Available, stats.Reason)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("total refreshes = %d, want 1", got)
	}
}

func TestUsageRetentionSnapshotStaleReadReturnsPreviousSnapshot(t *testing.T) {
	store := NewUsageStore(testDB(t))
	ready := awaitUsageRetentionStats(t, store)
	if !ready.Available || ready.CapturedAt == nil {
		t.Fatalf("initial snapshot = %+v", ready)
	}

	oldInterval := UsageRetentionRefreshInterval
	UsageRetentionRefreshInterval = time.Nanosecond
	t.Cleanup(func() { UsageRetentionRefreshInterval = oldInterval })

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var loads atomic.Int64
	store.retention.load = func() UsageRetentionStats {
		loads.Add(1)
		once.Do(func() { close(started) })
		<-release
		return store.loadUsageRetentionStats()
	}

	type result struct{ stats UsageRetentionStats }
	done := make(chan result, 1)
	go func() { done <- result{store.UsageRetentionSnapshot()} }()
	select {
	case got := <-done:
		if !got.stats.Available || got.stats.CapturedAt == nil || !got.stats.CapturedAt.Equal(*ready.CapturedAt) {
			t.Fatalf("stale read = %+v, want the previous snapshot", got.stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale read blocked on the database instead of returning the cached snapshot")
	}
	<-started

	again := store.UsageRetentionSnapshot()
	if !again.Available || again.CapturedAt == nil || !again.CapturedAt.Equal(*ready.CapturedAt) {
		t.Fatalf("concurrent stale read = %+v, want the previous snapshot", again)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("refreshes during the stale window = %d, want 1", got)
	}

	UsageRetentionRefreshInterval = oldInterval
	close(release)
	waitUsageRetentionIdle(t, store)
}

func TestUsageRetentionSnapshotConcurrentReadsTriggerOneRefresh(t *testing.T) {
	store := NewUsageStore(testDB(t))
	ready := awaitUsageRetentionStats(t, store)
	if !ready.Available || ready.CapturedAt == nil {
		t.Fatalf("initial snapshot = %+v", ready)
	}

	oldInterval := UsageRetentionRefreshInterval
	UsageRetentionRefreshInterval = time.Nanosecond
	t.Cleanup(func() { UsageRetentionRefreshInterval = oldInterval })

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var loads atomic.Int64
	store.retention.load = func() UsageRetentionStats {
		loads.Add(1)
		once.Do(func() { close(started) })
		<-release
		return store.loadUsageRetentionStats()
	}

	const readers = 64
	results := make([]UsageRetentionStats, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = store.UsageRetentionSnapshot()
		}(i)
	}
	<-started
	wg.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("concurrent readers started %d refreshes, want 1", got)
	}
	for i, stats := range results {
		if !stats.Available || stats.CapturedAt == nil || !stats.CapturedAt.Equal(*ready.CapturedAt) {
			t.Fatalf("reader %d saw %+v, want the previous snapshot", i, stats)
		}
	}

	UsageRetentionRefreshInterval = oldInterval
	close(release)
	waitUsageRetentionIdle(t, store)
}
