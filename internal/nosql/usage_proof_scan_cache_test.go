package nosql

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// legacyRawHistory stores rows the way pre-record_id releases did: no record
// id, no raw owner, and no accounting-proof tracking marker.
func legacyRawHistory(t *testing.T, db *bolt.DB, n int, at time.Time) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		raw := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < n; i++ {
			e := fixedPlainEvent()
			e.RecordID = ""
			e.ClientName = fmt.Sprintf("legacy-%03d", i%50)
			e.Timestamp = at.Add(time.Duration(i) * time.Millisecond)
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := raw.Put([]byte(usageKey(e.Timestamp, fmt.Sprintf("legacy%06d", i))), data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Every usage batch on an upgraded database proves its scope against the whole
// raw history inside the bbolt write transaction. Re-parsing unchanged legacy
// rows on each batch held the single writer for seconds and blocked admission.
func TestProofScansDoNotReparseUnchangedLegacyRows(t *testing.T) {
	const legacyRows = 400
	db := testDB(t)
	base := fixedPlainEvent().Timestamp.UTC().Truncate(time.Hour)
	legacyRawHistory(t, db, legacyRows, base.Add(-2*time.Hour))
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	batch := func(prefix int) []usage.Event {
		events := make([]usage.Event, 3)
		for i := range events {
			events[i] = fixedPlainEvent()
			events[i].ClientName = "live-client"
			events[i].RecordID = fmt.Sprintf("%08d-0000-4000-8000-%012x", prefix, i+1)
			events[i].Timestamp = base.Add(time.Duration(prefix*10+i) * time.Millisecond)
		}
		return events
	}
	if err := store.RecordBatch(batch(1)); err != nil {
		t.Fatal(err)
	}
	before := proofRawParses.Load()
	if err := store.RecordBatch(batch(2)); err != nil {
		t.Fatal(err)
	}
	if delta := proofRawParses.Load() - before; delta >= legacyRows/10 {
		t.Fatalf("second batch re-parsed %d raw rows; unchanged legacy history (%d rows) must not be parsed again", delta, legacyRows)
	}
}

// The cache must follow row content, not row keys: a changed value at the same
// key is classified again, so an overlap cannot hide behind a stale result.
func TestProofRawOverlapFollowsRowContent(t *testing.T) {
	db := testDB(t)
	at := fixedPlainEvent().Timestamp.UTC()
	key := []byte(usageKey(at, "legacy-overwrite"))
	put := func(client string) {
		e := fixedPlainEvent()
		e.RecordID = ""
		e.ClientName = client
		e.Timestamp = at
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(key, data) }); err != nil {
			t.Fatal(err)
		}
	}
	owners := map[string]bool{receiptClientHash("live-client"): true}
	hours := map[time.Time]bool{at.Truncate(time.Hour): true}
	overlap := func() bool {
		var got bool
		if err := db.View(func(tx *bolt.Tx) error {
			got = proofRawMayOverlap(tx, key, tx.Bucket([]byte(BucketUsageEvents)).Get(key), owners, hours)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	put("other-client")
	if overlap() {
		t.Fatal("row of another client must not overlap")
	}
	put("live-client")
	if !overlap() {
		t.Fatal("rewritten row now belongs to the proven client and must overlap")
	}
}

// Startup warm-up classifies legacy rows in a read transaction, so the first
// batch after a restart does not parse the history while holding the writer.
func TestWarmUsageProofScanCacheCoversFirstBatch(t *testing.T) {
	const legacyRows = 300
	db := testDB(t)
	base := fixedPlainEvent().Timestamp.UTC().Truncate(time.Hour)
	// A distinct hour keeps these rows' bytes out of other tests' cache entries.
	legacyRawHistory(t, db, legacyRows, base.Add(-7*time.Hour))
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if visited, err := WarmUsageProofScanCache(db); err != nil || visited != legacyRows {
		t.Fatalf("warm-up visited=%d err=%v, want %d rows", visited, err, legacyRows)
	}
	event := fixedPlainEvent()
	event.ClientName = "live-client"
	event.RecordID = "30000000-0000-4000-8000-000000000001"
	before := proofRawParses.Load()
	if err := store.RecordBatch([]usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	if delta := proofRawParses.Load() - before; delta >= legacyRows/10 {
		t.Fatalf("first batch after warm-up parsed %d raw rows", delta)
	}
}
