package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// --- Issue #125 P1.1 terminal-review gaps ---

// legacyIntegrityWindow is one named query window around a row's key hour.
type legacyIntegrityWindow struct {
	name string
	q    integrityQuery
}

// legacyIntegrityWindows returns the required excluding and including windows.
// Each carries one of the read variants (exact / windowKeys), so the relaxed
// row is exercised on both the report and quota-calibration paths.
func legacyIntegrityWindows(hour time.Time, client string) []legacyIntegrityWindow {
	hour = hour.UTC().Truncate(time.Hour)
	return []legacyIntegrityWindow{
		{name: "exclude-hour", q: integrityQuery{client: client, from: hour.Add(-2 * time.Hour), to: hour.Add(-time.Hour), exact: true}},
		{name: "include-hour", q: integrityQuery{client: client, from: hour, to: hour, windowKeys: true}},
	}
}

func integrityOutputContains(out []string, want string) bool {
	for _, line := range out {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// requireIntegrityModesError runs Fast and Full, requires their full verdict
// streams to agree, and requires (or forbids) want in either stream.
func requireIntegrityModesError(t *testing.T, f *integrityFuzzDB, q integrityQuery, want string, present bool) {
	t.Helper()
	if !compareIntegrityModes(t, f, q) {
		t.Fatalf("Fast != Full for %s", q)
	}
	for _, fast := range []bool{false, true} {
		out := runIntegrityQuery(f.db, q, fast)
		if got := integrityOutputContains(out, want); got != present {
			t.Fatalf("%s fast=%v: %q present=%v, want %v; output: %v", q, fast, want, got, present, out)
		}
	}
}

// allIntegrityReadsOK reports whether every read in out succeeded. The encoded
// line is "<name> <class> <data>".
func allIntegrityReadsOK(out []string) bool {
	for _, line := range out {
		_, rest, found := strings.Cut(line, " ")
		if !found || !strings.HasPrefix(rest, "ok ") {
			return false
		}
	}
	return true
}

// requireIntegrityModesClean additionally requires Fast == Full with no error.
func requireIntegrityModesClean(t *testing.T, f *integrityFuzzDB, q integrityQuery) {
	t.Helper()
	if !compareIntegrityModes(t, f, q) {
		t.Fatalf("Fast != Full for %s", q)
	}
	for _, fast := range []bool{false, true} {
		out := runIntegrityQuery(f.db, q, fast)
		if !allIntegrityReadsOK(out) {
			t.Fatalf("%s fast=%v: integrity reads were not clean: %v", q, fast, out)
		}
	}
}

func writeLegacyOwnedRaw(t testing.TB, db *bolt.DB, event usage.Event, keyID string) []byte {
	t.Helper()
	event.RecordID = ""
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, keyID))
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, data); err != nil {
			return err
		}
		return writeRawOwner(tx, key, event)
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

func putLegacyRaw(t testing.TB, db *bolt.DB, event usage.Event, key []byte) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}
}

func newLegacyShapeDB(t *testing.T) (*bolt.DB, *integrityFuzzDB) {
	t.Helper()
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	return db, &integrityFuzzDB{db: db, log: []string{t.Name()}}
}

func requireClassifiedAnomalous(t *testing.T, db *bolt.DB, key []byte) {
	t.Helper()
	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatal("classification unavailable")
	}
	if !class.eventSet[string(key)] {
		t.Fatalf("row %q was not classified as anomalous", key)
	}
}

// TestIntegrityLegacyRawNegativeShapes pins the negative shapes around the
// Issue #125 P1.1 relaxation: only an unowned row with an empty record_id and
// a key stamp equal to its event time may be skipped outside its key hour.
func TestIntegrityLegacyRawNegativeShapes(t *testing.T) {
	t.Run("owned non-uuid suffix", func(t *testing.T) {
		db, f := newLegacyShapeDB(t)
		event := fixedPlainEvent()
		event.Timestamp = integrityFuzzBase.Add(30*time.Hour + 12*time.Minute + 345*time.Millisecond)
		key := writeLegacyOwnedRaw(t, db, event, "legacy-owned-1")
		// The owned|| branch is the only defence here: without it Fast would
		// skip the row outside its hour while Full reports corrupt raw usage.
		for _, w := range legacyIntegrityWindows(event.Timestamp, "") {
			requireIntegrityModesError(t, f, w.q, "corrupt raw usage", true)
		}
		requireClassifiedAnomalous(t, db, key)
	})

	t.Run("owned uuid suffix", func(t *testing.T) {
		db, f := newLegacyShapeDB(t)
		event := fixedPlainEvent()
		event.Timestamp = integrityFuzzBase.Add(31*time.Hour + 12*time.Minute + 345*time.Millisecond)
		key := writeLegacyOwnedRaw(t, db, event, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		requireClassifiedAnomalous(t, db, key)
		// boundRawOutsideHour hides the owned mismatch outside the hour, but the
		// row is still visited in-window and must fail there in both modes.
		for _, w := range legacyIntegrityWindows(event.Timestamp, "") {
			requireIntegrityModesError(t, f, w.q, "corrupt raw usage", w.name == "include-hour")
		}
	})

	t.Run("body record id differs from key suffix", func(t *testing.T) {
		db, f := newLegacyShapeDB(t)
		event := fixedPlainEvent()
		event.RecordID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		event.Timestamp = integrityFuzzBase.Add(32*time.Hour + 12*time.Minute + 345*time.Millisecond)
		key := []byte(usageKey(event.Timestamp, "legacy-other"))
		putLegacyRaw(t, db, event, key)
		requireClassifiedAnomalous(t, db, key)
		// Full itself does not report this shape; the row is only conservatively
		// visited because key and body disagree.
		for _, w := range legacyIntegrityWindows(event.Timestamp, "bob") {
			requireIntegrityModesError(t, f, w.q, "corrupt raw usage", false)
		}
	})

	t.Run("non-canonical key stamp", func(t *testing.T) {
		db, f := newLegacyShapeDB(t)
		event := fixedPlainEvent()
		event.RecordID = ""
		event.Timestamp = integrityFuzzBase.Add(35*time.Hour + 12*time.Minute + 345*time.Millisecond)
		key := []byte(event.Timestamp.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339Nano) + "_legacy-1")
		putLegacyRaw(t, db, event, key)
		requireClassifiedAnomalous(t, db, key)
		for _, w := range legacyIntegrityWindows(event.Timestamp, "bob") {
			requireIntegrityModesError(t, f, w.q, "corrupt raw usage", false)
		}
	})

	t.Run("zero timestamp", func(t *testing.T) {
		db, f := newLegacyShapeDB(t)
		event := fixedPlainEvent()
		event.RecordID = ""
		event.Timestamp = time.Time{}
		key := []byte(usageKey(time.Time{}, "legacy-zero"))
		putLegacyRaw(t, db, event, key)
		requireClassifiedAnomalous(t, db, key)
		windows := []legacyIntegrityWindow{
			{name: "exclude-hour", q: integrityQuery{from: time.Time{}.Add(time.Hour), to: time.Time{}.Add(2 * time.Hour), exact: true}},
			{name: "include-hour", q: integrityQuery{from: time.Time{}, to: time.Time{}, windowKeys: true}},
		}
		// The exhaustive check rejects a zero timestamp regardless of window.
		for _, w := range windows {
			requireIntegrityModesError(t, f, w.q, "corrupt raw usage", true)
		}
	})
}

type legacyV2IndexFixture struct {
	db        *bolt.DB
	f         *integrityFuzzDB
	eventA    usage.Event
	keyA      []byte
	indexKeyA []byte
	h1        time.Time
}

// newLegacyV2IndexFixture builds the post-migration legacy shape: two rows
// without record ids, the later one becoming v2 progress Through so damaging
// row A's proof cannot break progress loading.
func newLegacyV2IndexFixture(t *testing.T) *legacyV2IndexFixture {
	t.Helper()
	db := testDB(t)
	store := clockedStore(db)
	if err := store.Record(fixedPlainEvent()); err != nil {
		t.Fatal(err)
	}
	h1 := integrityFuzzBase.Add(40 * time.Hour)
	eventA := fixedPlainEvent()
	eventA.Timestamp = h1
	eventA.RecordID = ""
	keyA := writeLegacyRawWithoutRecordID(t, db, eventA, "legacy-a")

	eventB := fixedPlainEvent()
	eventB.Timestamp = h1.Add(2 * time.Hour)
	eventB.RecordID = ""
	keyB := writeLegacyRawWithoutRecordID(t, db, eventB, "legacy-b")

	if err := MigrateUsageClientIndex(context.Background(), db, 2); err != nil {
		t.Fatalf("index migration: %v", err)
	}
	var p *indexProgress
	if err := db.View(func(tx *bolt.Tx) error {
		var err error
		p, err = loadIndexProgress(tx)
		return err
	}); err != nil {
		t.Fatalf("load index progress: %v", err)
	}
	if p == nil {
		t.Fatal("index progress unavailable after migration")
	}
	if !bytes.Equal(p.Through, keyB) {
		t.Fatalf("index progress through %q, want legacy row B %q", p.Through, keyB)
	}

	fixture := &legacyV2IndexFixture{
		db:        db,
		f:         &integrityFuzzDB{db: db, log: []string{t.Name()}},
		eventA:    eventA,
		keyA:      keyA,
		indexKeyA: clientEventIndexKey(eventA, keyA),
		h1:        h1,
	}
	class := classifyIntegrity(t, db)
	if class == nil {
		t.Fatal("classification unavailable")
	}
	if !class.indexClassified {
		t.Fatal("index entries were not classified under v2 progress")
	}
	if indexEntryInClass(class, fixture.indexKeyA) {
		t.Fatal("intact legacy row's index entry was classified as anomalous")
	}
	return fixture
}

func (fx *legacyV2IndexFixture) rewriteProofA(t *testing.T, mutate func(*indexProof)) {
	t.Helper()
	if err := fx.db.Update(func(tx *bolt.Tx) error {
		proofs := tx.Bucket([]byte(indexProofsV2))
		var proof indexProof
		if err := json.Unmarshal(proofs.Get(fx.keyA), &proof); err != nil {
			return err
		}
		mutate(&proof)
		proof.Binding = proof.binding(fx.keyA)
		data, err := json.Marshal(proof)
		if err != nil {
			return err
		}
		return proofs.Put(fx.keyA, data)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityLegacyV2IndexDamage(t *testing.T) {
	t.Run("missing proof", func(t *testing.T) {
		fx := newLegacyV2IndexFixture(t)
		if err := fx.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(indexProofsV2)).Delete(fx.keyA)
		}); err != nil {
			t.Fatal(err)
		}
		class := classifyIntegrity(t, fx.db)
		if class == nil {
			t.Fatal("classification unavailable")
		}
		if !indexEntryInClass(class, fx.indexKeyA) {
			t.Fatal("index entry without proof was not classified as anomalous")
		}
		// An invalid, unattributable proof fails checkOrphans at every hour.
		for _, w := range legacyIntegrityWindows(fx.h1, "") {
			requireIntegrityModesError(t, fx.f, w.q, "invalid client index proof", true)
		}
	})

	t.Run("proof points at another hour", func(t *testing.T) {
		fx := newLegacyV2IndexFixture(t)
		h3 := fx.h1.Add(5 * time.Hour)
		fx.rewriteProofA(t, func(proof *indexProof) {
			other := fx.eventA
			other.Timestamp = h3
			proof.IndexKey = clientEventIndexKey(other, fx.keyA)
		})
		class := classifyIntegrity(t, fx.db)
		if class == nil {
			t.Fatal("classification unavailable")
		}
		if !indexEntryInClass(class, fx.indexKeyA) {
			t.Fatal("index entry whose proof names another hour was not classified as anomalous")
		}
		for _, w := range legacyIntegrityWindows(fx.h1, "") {
			present := w.name == "include-hour"
			requireIntegrityModesError(t, fx.f, w.q, "invalid client index proof", present)
		}
		onlyOther := integrityQuery{from: h3, to: h3, exact: true}
		requireIntegrityModesError(t, fx.f, onlyOther, "invalid client index proof", true)
	})

	t.Run("stale raw digest stays exempt", func(t *testing.T) {
		fx := newLegacyV2IndexFixture(t)
		if err := fx.db.Update(func(tx *bolt.Tx) error {
			raw := tx.Bucket([]byte(BucketUsageEvents))
			var event usage.Event
			if err := json.Unmarshal(raw.Get(fx.keyA), &event); err != nil {
				return err
			}
			event.InputTokens++
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			return raw.Put(fx.keyA, data)
		}); err != nil {
			t.Fatal(err)
		}
		class := classifyIntegrity(t, fx.db)
		if class == nil {
			t.Fatal("classification unavailable")
		}
		// The stale digest makes the proof invalid, but the original hour is
		// still the entry's own hour, so the entry stays out of A.
		if indexEntryInClass(class, fx.indexKeyA) {
			t.Fatal("index entry with a stale proof digest was not exempt")
		}
		for _, w := range legacyIntegrityWindows(fx.h1, "") {
			if w.name == "include-hour" {
				// Outside A, the entry is only reached because the window itself
				// includes its hour; Fast must therefore still report it.
				requireIntegrityModesError(t, fx.f, w.q, "invalid client index proof", true)
			} else {
				requireIntegrityModesClean(t, fx.f, w.q)
			}
		}
	})
}
