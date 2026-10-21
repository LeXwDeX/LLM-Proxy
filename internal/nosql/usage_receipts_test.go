package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func usageReceiptCount(t *testing.T, db *bolt.DB) int {
	t.Helper()
	count := 0
	if err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketUsageReceipts))
		if bucket == nil {
			return errors.New("receipt bucket is missing")
		}
		cursor := bucket.Cursor()
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func putUsageReceipt(t *testing.T, db *bolt.DB, key string, receipt any) {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageReceipts)).Put([]byte(key), data)
	}); err != nil {
		t.Fatal(err)
	}
}

func receiptKey(event usage.Event) string {
	return usageKey(event.Timestamp, event.RecordID)
}

// resetReceiptTracking simulates an old database that was pruned before
// receipts existed: the derived completion marker is gone and no receipt was
// written.
func resetReceiptTracking(t *testing.T, db *bolt.DB) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Delete([]byte(usageReceiptsWatermarkDoneKey)); err != nil {
			return err
		}
		return meta.Delete([]byte(usageReceiptsWatermarkKey))
	}); err != nil {
		t.Fatal(err)
	}
}

func deleteUsageRawAndIndex(t *testing.T, db *bolt.DB, event usage.Event) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		key := []byte(receiptKey(event))
		data := raw.Get(key)
		if data == nil {
			return nil
		}
		if err := removeClientEventIndex(tx, key, data); err != nil {
			return err
		}
		if err := raw.Delete(key); err != nil {
			return err
		}
		return bumpRawRevision(tx)
	}); err != nil {
		t.Fatal(err)
	}
}

func clearClientIndex(t *testing.T, db *bolt.DB) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{BucketUsageEventsByClient, BucketUsageClientHours} {
			bucket := tx.Bucket([]byte(name))
			cursor := bucket.Cursor()
			var keys [][]byte
			for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
				keys = append(keys, append([]byte(nil), k...))
			}
			for _, key := range keys {
				if err := bucket.Delete(key); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrunedBundleRetryDoesNotResurrectOrDoubleCount(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	aggBefore := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	quotaBefore := indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)

	deleted, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 10)
	if err != nil || deleted != 2 {
		t.Fatalf("prune: deleted=%d err=%v", deleted, err)
	}
	if raw, _ := usageRecordCounts(t, db); raw != 0 {
		t.Fatalf("prune retained %d raw rows", raw)
	}
	if receipts := usageReceiptCount(t, db); receipts != 2 {
		t.Fatalf("receipts=%d, want 2", receipts)
	}
	var receipt map[string]any
	if err := db.View(func(tx *bolt.Tx) error {
		return json.Unmarshal(tx.Bucket([]byte(BucketUsageReceipts)).Get([]byte(receiptKey(event))), &receipt)
	}); err != nil {
		t.Fatal(err)
	}
	if len(receipt) != 2 || receipt["version"] == nil || receipt["digest"] == nil {
		t.Fatalf("receipt retained payload fields: %v", receipt)
	}

	retry := usage.CloneEvent(event)
	retry.PricingRuleID = "executor-rate-v2"
	retry.PricingRuleVersion = 2
	retry.Components[0].PricingRuleID = "advisor-rate-v2"
	retry.Components[0].PricingRuleVersion = 2
	for i, candidate := range []usage.Event{retry, event} {
		added, err := store.RecordOnce(candidate)
		if err != nil || added {
			t.Fatalf("pruned retry %d: added=%v err=%v", i, added, err)
		}
	}
	if raw, agg := usageRecordCounts(t, db); raw != 0 || agg != 2 {
		t.Fatalf("pruned retry resurrected accounting: raw=%d agg=%d", raw, agg)
	}
	if !reflect.DeepEqual(aggBefore, indexBucketSnapshot(t, db, BucketUsageAggHourly)) ||
		!reflect.DeepEqual(quotaBefore, indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)) {
		t.Fatal("pruned retry changed permanent rollups")
	}
}

func TestReceiptConflictAndUnknownVersionRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt any
	}{
		{"unknown version", usageReceipt{Version: 99, Digest: "00"}},
		{"conflicting digest", usageReceipt{Version: usageReceiptVersion, Digest: "00"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			event := fixedIdentityBundle()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 10); err != nil {
				t.Fatal(err)
			}
			aggBefore := indexBucketSnapshot(t, db, BucketUsageAggHourly)
			putUsageReceipt(t, db, receiptKey(event), tc.receipt)

			added, err := store.RecordOnce(event)
			if err == nil || added {
				t.Fatalf("bad receipt accepted: added=%v err=%v", added, err)
			}
			if raw, _ := usageRecordCounts(t, db); raw != 0 {
				t.Fatalf("bad receipt resurrected %d raw rows", raw)
			}
			if !reflect.DeepEqual(aggBefore, indexBucketSnapshot(t, db, BucketUsageAggHourly)) {
				t.Fatal("bad receipt changed permanent rollups")
			}
		})
	}
}

func TestPrunedBundleMixedReceiptAndRaw(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	rows := usage.ExpandEvent(event)
	root, child := rows[0], rows[1]
	aggBefore := indexBucketSnapshot(t, db, BucketUsageAggHourly)

	// Simulate a cleanup that removed the root but left its component raw.
	deleteUsageRawAndIndex(t, db, root)
	digest, err := usage.RetryEquivalentDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	putUsageReceipt(t, db, receiptKey(root), usageReceipt{Version: usageReceiptVersion, Digest: digest})
	clearClientIndex(t, db)

	retry := usage.CloneEvent(event)
	retry.PricingRuleID = "executor-rate-v2"
	retry.PricingRuleVersion = 2
	retry.Components[0].PricingRuleID = "advisor-rate-v2"
	retry.Components[0].PricingRuleVersion = 2
	added, err := store.RecordOnce(retry)
	if err != nil || added {
		t.Fatalf("mixed receipt/raw retry: added=%v err=%v", added, err)
	}
	if raw, _ := usageRecordCounts(t, db); raw != 1 {
		t.Fatalf("mixed retry resurrected root or dropped child: raw=%d", raw)
	}
	got, err := store.ReadClientEvents(t.Context(), "alice", event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("mixed retry did not repair the child index from stored bytes: rows=%d err=%v", len(got), err)
	}
	if !reflect.DeepEqual(aggBefore, indexBucketSnapshot(t, db, BucketUsageAggHourly)) {
		t.Fatal("mixed retry changed permanent rollups")
	}

	deleteUsageRawAndIndex(t, db, child)
	added, err = store.RecordOnce(event)
	if err == nil || added {
		t.Fatalf("partially committed bundle accepted: added=%v err=%v", added, err)
	}
	if raw, _ := usageRecordCounts(t, db); raw != 0 {
		t.Fatalf("partially committed bundle wrote %d raw rows", raw)
	}
}

func TestLegacyPrunedDatabaseRejectsUnverifiedIdentity(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	aggBefore := indexBucketSnapshot(t, db, BucketUsageAggHourly)

	// Old prune: raw/index removed, permanent rollups kept, no receipt written.
	for _, row := range usage.ExpandEvent(event) {
		deleteUsageRawAndIndex(t, db, row)
	}
	resetReceiptTracking(t, db)

	added, err := NewUsageStore(db).RecordOnce(event)
	if err == nil || added {
		t.Fatalf("unverifiable legacy identity accepted: added=%v err=%v", added, err)
	}
	if !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("legacy rejection error = %v, want ErrUsageIdentityUnverified", err)
	}
	if raw, agg := usageRecordCounts(t, db); raw != 0 || agg != 2 {
		t.Fatalf("legacy rejection changed accounting: raw=%d agg=%d", raw, agg)
	}
	if !reflect.DeepEqual(aggBefore, indexBucketSnapshot(t, db, BucketUsageAggHourly)) {
		t.Fatal("legacy rejection changed permanent rollups")
	}
	watermarkHour := event.Timestamp.Truncate(time.Hour).Add(time.Hour)
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketMeta)).Get([]byte(usageReceiptsWatermarkKey))
		var got time.Time
		if len(raw) > 0 {
			var parseErr error
			got, parseErr = time.Parse(time.RFC3339Nano, string(raw))
			if parseErr != nil {
				return parseErr
			}
		}
		if !got.Equal(watermarkHour) {
			t.Fatalf("watermark=%v, want %v", got, watermarkHour)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	next := fixedIdentityBundle()
	next.RecordID = fixedLaterRecordID
	next.Timestamp = event.Timestamp.Add(2 * time.Hour)
	next.Components = nil
	added, err = NewUsageStore(db).RecordOnce(next)
	if err != nil || !added {
		t.Fatalf("post-watermark new identity: added=%v err=%v", added, err)
	}
	if _, err := NewUsageStore(db).RecordOnce(event); !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("second legacy retry error=%v", err)
	}
	if raw, _ := usageRecordCounts(t, db); raw != 1 {
		t.Fatalf("legacy retry wrote another row: raw=%d", raw)
	}
}

func TestReceiptBucketRecreationRequiresNewWatermark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	event := fixedIdentityBundle()
	store := NewUsageStore(db)
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	// Simulate an old database that never had the receipt bucket (or lost it):
	// reopening recreates an empty bucket and clears the derived completion
	// marker, forcing the conservative legacy scan.
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
	t.Cleanup(func() { db.Close() })
	if receipts := usageReceiptCount(t, db); receipts != 0 {
		t.Fatalf("recreated receipt bucket retained %d receipts", receipts)
	}
	added, err := NewUsageStore(db).RecordOnce(event)
	if err == nil || added {
		t.Fatalf("empty recreated receipt bucket treated a pruned identity as new: added=%v err=%v", added, err)
	}
	if !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("recreated bucket error=%v, want ErrUsageIdentityUnverified", err)
	}
	if raw, agg := usageRecordCounts(t, db); raw != 0 || agg != 2 {
		t.Fatalf("recreated bucket changed accounting: raw=%d agg=%d", raw, agg)
	}
}

func TestPruneFailsClosedWithoutReceiptBucket(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	rawBefore := indexBucketSnapshot(t, db, BucketUsageEvents)
	indexBefore := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketUsageReceipts)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := NewUsageStore(db).PruneEventsBefore(event.Timestamp.Add(time.Hour), 10); err == nil {
		t.Fatal("prune succeeded without a receipt bucket")
	}
	if !reflect.DeepEqual(rawBefore, indexBucketSnapshot(t, db, BucketUsageEvents)) ||
		!reflect.DeepEqual(indexBefore, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) {
		t.Fatal("failed prune did not roll back raw and index data")
	}
}

func TestPruneQuarantinesCorruptionWithoutReceipt(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	at := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	valid := usage.Event{Timestamp: at, ClientName: "alice", Model: "qwen-max", InputTokens: 1}
	if err := store.Record(valid); err != nil {
		t.Fatal(err)
	}
	badKey := []byte(usageKey(at.Add(time.Second), "bad"))
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(badKey, []byte("bad JSON"))
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.PruneEventsBefore(at.Add(time.Hour), 10)
	if err != nil || deleted != 2 {
		t.Fatalf("prune with corrupt raw: deleted=%d err=%v", deleted, err)
	}
	if receipts := usageReceiptCount(t, db); receipts != 1 {
		t.Fatalf("receipts=%d, want only the decodable event", receipts)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageEventsQuarantine)).Get(badKey) == nil {
			t.Fatal("corrupt raw was not quarantined")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptWatermarkMarksMissingAggregateCell(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	root := usage.ExpandEvent(event)[0]
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageAggHourly)).Delete(aggKey(root.Timestamp, root.EndpointType, root.ClientName, root.Model, root.Target))
	}); err != nil {
		t.Fatal(err)
	}
	resetReceiptTracking(t, db)
	next := fixedIdentityBundle()
	next.RecordID = fixedLaterRecordID
	next.Timestamp = event.Timestamp
	next.Components = nil
	added, err := NewUsageStore(db).RecordOnce(next)
	if err == nil || added {
		t.Fatalf("missing aggregate cell did not establish a conservative boundary: added=%v err=%v", added, err)
	}
	if !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("missing aggregate cell error=%v, want ErrUsageIdentityUnverified", err)
	}
	if raw, agg := usageRecordCounts(t, db); raw != 2 || agg != 1 {
		t.Fatalf("missing aggregate cell changed accounting: raw=%d agg=%d", raw, agg)
	}
}

func TestRawEvidenceWinsOverLegacyWatermark(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		watermark := event.Timestamp.Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339Nano)
		if err := meta.Put([]byte(usageReceiptsWatermarkKey), []byte(watermark)); err != nil {
			return err
		}
		return meta.Put([]byte(usageReceiptsWatermarkDoneKey), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	added, err := NewUsageStore(db).RecordOnce(event)
	if err != nil || added {
		t.Fatalf("retained raw evidence lost to watermark: added=%v err=%v", added, err)
	}
	raw, agg := usageRecordCounts(t, db)
	if raw != 2 || agg != 2 {
		t.Fatalf("raw evidence replay changed accounting: raw=%d agg=%d", raw, agg)
	}
}

func TestReceiptWatermarkMigrationRetryable(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
	var saved []byte
	if err := db.View(func(tx *bolt.Tx) error {
		saved = append([]byte(nil), tx.Bucket([]byte(BucketUsageAggHourly)).Get(key)...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resetReceiptTracking(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageAggHourly)).Put(key, []byte("not-json"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := NewUsageStore(db).EnsureUsageReceipts(); err == nil {
		t.Fatal("corrupt aggregate cell did not block watermark migration")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageReceiptsWatermarkDoneKey)) != nil {
			t.Fatal("failed migration published a completion marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageAggHourly)).Put(key, saved)
	}); err != nil {
		t.Fatal(err)
	}
	if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatalf("repaired migration: %v", err)
	}
	if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
}

func TestReceiptWatermarkNoFalseRejectWhenHistoryIntact(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	next := fixedIdentityBundle()
	next.RecordID = fixedLaterRecordID
	next.Components = nil
	added, err := NewUsageStore(db).RecordOnce(next)
	if err != nil || !added {
		t.Fatalf("intact history rejected a new explicit identity: added=%v err=%v", added, err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket([]byte(BucketMeta)).Get([]byte(usageReceiptsWatermarkKey)); len(raw) != 0 {
			t.Fatalf("intact history established a non-empty watermark: %q", raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func fixedPlainEvent() usage.Event {
	event := fixedIdentityBundle()
	event.Components = nil
	return event
}

var replayCosts = usage.CostTable{
	"executor": {InputPer1MTokens: 2},
	"advisor":  {InputPer1MTokens: 5},
}

func costUnitTotal(units []CostUnit) float64 {
	var total float64
	for _, unit := range units {
		total += unit.CostUSD
	}
	return total
}

// Pause before the primary write to exercise retention during replay.
type gatedUsageRecorder struct {
	next    usage.Recorder
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *gatedUsageRecorder) Record(event usage.Event) error {
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	return r.next.Record(event)
}

func TestJournalReplayAfterPruneDoesNotDoubleCount(t *testing.T) {
	for _, tc := range []struct {
		name        string
		event       func() usage.Event
		wantCost    float64
		wantRecords int64
	}{
		{"plain", fixedPlainEvent, 2, 1},
		{"bundle", fixedIdentityBundle, 12, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := tc.event()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "usage.db")
			journalPath := filepath.Join(dir, "usage-journal.jsonl")
			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if db != nil {
					db.Close()
				}
			}()
			store := NewUsageStore(db)
			journal, err := NewUsageJournal(&commitThenFailUsageRecorder{
				next:      store,
				err:       errors.New("isolated uncertain commit"),
				failFirst: true,
			}, journalPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Record(event); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			deleted, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 10)
			if err != nil || deleted != int(tc.wantRecords) {
				t.Fatalf("prune: deleted=%d err=%v", deleted, err)
			}
			if receipts := usageReceiptCount(t, db); receipts != int(tc.wantRecords) {
				t.Fatalf("receipts after prune=%d, want %d", receipts, tc.wantRecords)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			filter := usage.Filter{From: &from, To: &to}
			beforeAgg, err := store.Aggregate(filter, "hour", replayCosts)
			if err != nil {
				t.Fatal(err)
			}
			beforeUnits, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
			if err != nil {
				t.Fatal(err)
			}
			if beforeAgg.Totals.Requests != 1 || beforeAgg.Totals.EstimatedCost != tc.wantCost {
				t.Fatalf("pre-replay aggregate requests/cost=%+v, want requests=1 cost=%v", beforeAgg.Totals, tc.wantCost)
			}
			if cost := costUnitTotal(beforeUnits); cost != tc.wantCost {
				t.Fatalf("pre-replay cost units=%v, want %v", cost, tc.wantCost)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			db, err = OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			store = NewUsageStore(db)
			restarted, err := NewUsageJournal(store, journalPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()

			clients, err := restarted.Replay()
			if err != nil {
				t.Fatalf("replay after prune: %v", err)
			}
			if len(clients) != 1 || clients[0] != "alice" {
				t.Fatalf("recovered clients=%v", clients)
			}
			for i := 0; i < 2; i++ {
				if clients, err := restarted.Replay(); err != nil || len(clients) != 0 {
					t.Fatalf("replay %d: clients=%v err=%v", i+2, clients, err)
				}
			}
			if raw, agg := usageRecordCounts(t, db); raw != 0 || agg != tc.wantRecords {
				t.Fatalf("replay after prune resurrected accounting: raw=%d agg=%d", raw, agg)
			}
			if receipts := usageReceiptCount(t, db); receipts != int(tc.wantRecords) {
				t.Fatalf("receipts after replay=%d, want %d", receipts, tc.wantRecords)
			}
			afterAgg, err := store.Aggregate(filter, "hour", replayCosts)
			if err != nil {
				t.Fatal(err)
			}
			afterUnits, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeAgg, afterAgg) || !reflect.DeepEqual(beforeUnits, afterUnits) {
				t.Fatalf("replay changed retained accounting: agg=%+v units=%+v", afterAgg.Totals, afterUnits)
			}
		})
	}
}

func TestJournalReplayInterleavedWithPruneAndNewRecord(t *testing.T) {
	for _, tc := range []struct {
		name        string
		event       func() usage.Event
		wantCost    float64
		wantRecords int64
	}{
		{"plain", fixedPlainEvent, 2, 1},
		{"bundle", fixedIdentityBundle, 12, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.event()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "usage.db")
			journalPath := filepath.Join(dir, "usage-journal.jsonl")
			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if db != nil {
					db.Close()
				}
			}()
			store := NewUsageStore(db)
			journal, err := NewUsageJournal(&commitThenFailUsageRecorder{
				next:      store,
				err:       errors.New("isolated uncertain commit"),
				failFirst: true,
			}, journalPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Record(original); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}

			release := make(chan struct{})
			unblock := func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}
			gate := &gatedUsageRecorder{next: store, entered: make(chan struct{}), release: release}
			replaying, err := NewUsageJournal(gate, journalPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan struct{})
			var replayErr error
			go func() {
				defer close(done)
				_, replayErr = replaying.Replay()
			}()
			defer func() {
				unblock()
				select {
				case <-done:
					if err := replaying.Close(); err != nil {
						t.Errorf("close replay journal: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Error("replay goroutine did not exit")
				}
			}()
			select {
			case <-gate.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("replay did not pause before the primary write")
			}

			deleted, err := store.PruneEventsBefore(original.Timestamp.Add(time.Hour), 10)
			if err != nil || deleted != int(tc.wantRecords) {
				t.Fatalf("prune: deleted=%d err=%v", deleted, err)
			}
			if receipts := usageReceiptCount(t, db); receipts != int(tc.wantRecords) {
				t.Fatalf("receipts after interleaved prune=%d, want %d", receipts, tc.wantRecords)
			}

			fresh := fixedPlainEvent()
			fresh.RecordID = fixedLaterRecordID
			fresh.Timestamp = original.Timestamp.Add(2 * time.Hour)
			fresh.RequestID = "req-fresh"
			if err := store.Record(fresh); err != nil {
				t.Fatal(err)
			}

			unblock()
			select {
			case <-done:
				if replayErr != nil {
					t.Fatalf("replay: %v", replayErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("replay did not finish after release")
			}

			if raw, agg := usageRecordCounts(t, db); raw != 1 || agg != tc.wantRecords+1 {
				t.Fatalf("interleaved replay resurrection: raw=%d agg=%d", raw, agg)
			}
			if receipts := usageReceiptCount(t, db); receipts != int(tc.wantRecords) {
				t.Fatalf("receipts after interleaved replay=%d, want %d", receipts, tc.wantRecords)
			}
			from := original.Timestamp.Truncate(time.Hour)
			to := fresh.Timestamp.Add(time.Hour)
			filter := usage.Filter{From: &from, To: &to}
			wantCost := tc.wantCost + 2
			agg, err := store.Aggregate(filter, "hour", replayCosts)
			if err != nil {
				t.Fatal(err)
			}
			if agg.Totals.Requests != 2 || agg.Totals.EstimatedCost != wantCost {
				t.Fatalf("interleaved aggregate requests/cost=%+v, want requests=2 cost=%v", agg.Totals, wantCost)
			}
			units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
			if err != nil {
				t.Fatal(err)
			}
			if cost := costUnitTotal(units); cost != wantCost {
				t.Fatalf("interleaved cost units=%v, want %v", cost, wantCost)
			}
			rows, err := store.List(usage.Filter{ClientName: "alice"})
			if err != nil || len(rows) != 1 || rows[0].RecordID != fixedLaterRecordID {
				t.Fatalf("new record lost or replaced: rows=%+v err=%v", rows, err)
			}

			beforeAgg, beforeUnits := agg, units
			if clients, err := replaying.Replay(); err != nil || len(clients) != 0 {
				t.Fatalf("second replay: clients=%v err=%v", clients, err)
			}
			again, err := store.Aggregate(filter, "hour", replayCosts)
			if err != nil {
				t.Fatal(err)
			}
			againUnits, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeAgg, again) || !reflect.DeepEqual(beforeUnits, againUnits) ||
				again.Totals.Requests != 2 || again.Totals.EstimatedCost != wantCost {
				t.Fatalf("second replay changed accounting: agg=%+v units=%+v", again.Totals, againUnits)
			}
			if raw, aggCount := usageRecordCounts(t, db); raw != 1 || aggCount != tc.wantRecords+1 {
				t.Fatalf("second replay changed rows: raw=%d agg=%d", raw, aggCount)
			}
		})
	}
}
