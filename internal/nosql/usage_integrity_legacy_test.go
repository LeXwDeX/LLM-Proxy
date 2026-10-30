package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// Issue #125 P1.1: rows written before the record id was stored have no
// record_id in the body. Their key still carries the event id, so
// usageKey(event) != key, but the key stamp is exactly the event time and they
// have no raw owner. The exhaustive direct check never compares key and event
// for unowned rows (usage_direct_integrity.go:161), so such rows only
// accumulate cells of their own hour and are inert outside it.

func writeLegacyRawWithoutRecordID(t testing.TB, db *bolt.DB, event usage.Event, keyID string) []byte {
	t.Helper()
	event.RecordID = ""
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, keyID))
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

func classifyIntegrity(t testing.TB, db *bolt.DB) *integrityClassification {
	t.Helper()
	var class *integrityClassification
	if err := db.View(func(tx *bolt.Tx) error {
		var reason string
		class, reason = classifyUsageIntegrity(context.Background(), tx)
		if class == nil {
			t.Logf("classification unavailable: %s", reason)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return class
}

func TestIntegrityLegacyRawWithoutRecordIDIsWindowExempt(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	event := fixedPlainEvent()
	event.Timestamp = integrityFuzzBase.Add(30*time.Hour + 12*time.Minute + 345*time.Millisecond)
	key := writeLegacyRawWithoutRecordID(t, db, event, "legacy-0")

	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatal("classification unavailable")
	}
	if class.eventSet[string(key)] {
		t.Fatalf("legacy row without record_id was classified as anomalous")
	}
	if class.anomalous != 0 {
		t.Fatalf("anomalous objects = %d, want 0", class.anomalous)
	}
}

// TestIntegrityLegacyRawRowsWithV2IndexProgress covers the post-migration
// shape: the v2 index has a scan proof and entry for the legacy row, both keyed
// by the event client/time and the raw key (never by record_id).
func TestIntegrityLegacyRawRowsWithV2IndexProgress(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	event := fixedPlainEvent()
	event.Timestamp = integrityFuzzBase.Add(33*time.Hour + 12*time.Minute)
	key := writeLegacyRawWithoutRecordID(t, db, event, "legacy-3")
	if err := MigrateUsageClientIndex(context.Background(), db, 2); err != nil {
		t.Fatalf("index migration: %v", err)
	}

	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatal("classification unavailable")
	}
	if !class.indexClassified {
		t.Fatal("index entries were not classified under v2 progress")
	}
	if class.anomalous != 0 {
		t.Fatalf("anomalous objects = %d, want 0", class.anomalous)
	}
	if class.eventSet[string(key)] {
		t.Fatal("legacy raw row was classified as anomalous")
	}
	indexKey := clientEventIndexKey(event, key)
	if indexEntryInClass(class, indexKey) {
		t.Fatal("legacy raw row's index entry was classified as anomalous")
	}
}

func indexEntryInClass(class *integrityClassification, key []byte) bool {
	for _, k := range class.index {
		if string(k) == string(key) {
			return true
		}
	}
	return false
}

func TestIntegrityLegacyRawWithoutRecordIDOutsideHourStaysAnomalous(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	event := fixedPlainEvent()
	event.Timestamp = integrityFuzzBase.Add(31*time.Hour + 12*time.Minute)
	key := writeLegacyRawWithoutRecordID(t, db, event, "legacy-1")

	// The body moves to another hour while the key keeps its stamp: the row
	// now accumulates cells the key hour does not cover, so it must stay in A.
	moved, err := json.Marshal(usage.Event{
		Timestamp:    event.Timestamp.Add(2 * time.Hour),
		ClientName:   event.ClientName,
		Target:       event.Target,
		Model:        event.Model,
		EndpointType: event.EndpointType,
		InputTokens:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, moved)
	}); err != nil {
		t.Fatal(err)
	}

	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatal("classification unavailable")
	}
	if !class.eventSet[string(key)] {
		t.Fatalf("legacy row whose event hour differs from its key hour was not classified as anomalous")
	}
}

func TestIntegrityLegacyRawWithoutRecordIDWithOtherInstantStaysAnomalous(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	event := fixedPlainEvent()
	event.Timestamp = integrityFuzzBase.Add(32*time.Hour + 12*time.Minute)
	key := writeLegacyRawWithoutRecordID(t, db, event, "legacy-2")

	// Same hour, different instant: the observed legacy shape is exact equality,
	// so the minimal relaxation does not cover it.
	moved, err := json.Marshal(usage.Event{
		Timestamp:    event.Timestamp.Add(time.Minute),
		ClientName:   event.ClientName,
		Target:       event.Target,
		Model:        event.Model,
		EndpointType: event.EndpointType,
		InputTokens:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, moved)
	}); err != nil {
		t.Fatal(err)
	}

	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatal("classification unavailable")
	}
	if !class.eventSet[string(key)] {
		t.Fatalf("legacy row with a non-equal timestamp was not classified as anomalous")
	}
}

// TestIntegrityManyLegacyRawRowsDoNotOverflow reproduces P1.1: a production
// clone has ~200k such rows, which overflowed |A| and disabled the fast path.
func TestIntegrityManyLegacyRawRowsDoNotOverflow(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	base := integrityFuzzBase.Add(60 * time.Hour)
	rows := integrityAnomalyLimit + 1
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < rows; i++ {
			event := fixedPlainEvent()
			event.Timestamp = base.Add(time.Duration(i) * time.Second)
			event.RecordID = ""
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			key := []byte(usageKey(event.Timestamp, fmt.Sprintf("legacy-%d", i)))
			if err := raw.Put(key, data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatalf("classification unavailable: %d legacy rows overflowed A", rows)
	}
	if class.anomalous != 0 {
		t.Fatalf("anomalous objects = %d, want 0", class.anomalous)
	}
}
