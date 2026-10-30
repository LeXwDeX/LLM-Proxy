package nosql

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// proofOddIndexRows recomputes the classification with a full scan. The map
// value is the pointer payload: kind byte plus hour.
func proofOddIndexRows(t *testing.T, db *bolt.DB) (map[string]string, map[string]string) {
	t.Helper()
	rawWant := make(map[string]string)
	receiptWant := make(map[string]string)
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		c := raw.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if canonicalRawRowCached(k, v) {
				continue
			}
			kind, hour := proofOddClassifyRaw(tx, k, v)
			rawWant[string(k)] = string([]byte{kind}) + hour
		}
		receipts := tx.Bucket([]byte(BucketUsageReceipts))
		rc := receipts.Cursor()
		for k := range receiptKeys(rc) {
			if canonicalReceiptStamp([]byte(k)) {
				continue
			}
			kind, hour := proofOddClassifyReceipt([]byte(k))
			receiptWant[k] = string([]byte{kind}) + hour
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rawWant, receiptWant
}

// receiptKeys yields every key of the receipt bucket, nested buckets included.
func receiptKeys(c *bolt.Cursor) map[string]struct{} {
	out := make(map[string]struct{})
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		out[string(k)] = struct{}{}
	}
	return out
}

func proofOddIndexMembers(t *testing.T, db *bolt.DB) (map[string]string, map[string]string) {
	t.Helper()
	rawGot := make(map[string]string)
	receiptGot := make(map[string]string)
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketUsageProofOdd))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) == 0 {
				return nil
			}
			switch k[0] {
			case proofOddRawPointer:
				rawGot[string(k[1:])] = string(v)
			case proofOddReceiptPointer:
				receiptGot[string(k[1:])] = string(v)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return rawGot, receiptGot
}

func requireProofOddIndexMatches(t *testing.T, db *bolt.DB) {
	t.Helper()
	rawWant, receiptWant := proofOddIndexRows(t, db)
	rawGot, receiptGot := proofOddIndexMembers(t, db)
	if fmt.Sprint(rawWant) != fmt.Sprint(rawGot) {
		t.Fatalf("raw odd index diverged from a full classification:\nwant=%v\ngot=%v", rawWant, rawGot)
	}
	if fmt.Sprint(receiptWant) != fmt.Sprint(receiptGot) {
		t.Fatalf("receipt odd index diverged from a full classification:\nwant=%v\ngot=%v", receiptWant, receiptGot)
	}
}

func proofOddReadyNow(t *testing.T, db *bolt.DB) bool {
	t.Helper()
	var ready bool
	if err := db.View(func(tx *bolt.Tx) error {
		ready = proofOddIndexReady(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ready
}

func clearProofScanCacheForTest() {
	proofScanCache.Lock()
	proofScanCache.decodeFailed = make(map[proofRawDigest]struct{})
	proofScanCache.scopes = make(map[proofRawDigest]proofRawScope)
	proofScanCache.canonical = make(map[string]proofRawDigest)
	proofScanCache.Unlock()
}

// canonicalHistory stores canonical rows the way supported writers do, but
// without touching aggregates: a proof only needs them to stay out of scope.
func canonicalHistory(t *testing.T, db *bolt.DB, n int, base time.Time) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			e := scaleEvent(700000+i, base.Add(time.Duration(i)*time.Minute))
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, data); err != nil {
				return err
			}
			if err := writeRawOwner(tx, key, e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The persisted index lets a write batch and startup warm-up ignore canonical
// history entirely. Before #114 each proof transaction classified every raw
// row with a full ForEach and warm-up decoded each one.
func TestProofOddIndexAvoidsHistoryClassification(t *testing.T) {
	const history = 600
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	canonicalHistory(t, db, history, base)
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 128); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
	// A restarted process has no content memos.
	clearProofScanCacheForTest()
	// Warm-up is O(global odd rows), which is zero here, not O(history).
	if visited, err := WarmUsageProofScanCache(db); err != nil || visited != 0 {
		t.Fatalf("warm-up visited=%d err=%v, want 0 canonical rows", visited, err)
	}
	event := scaleEvent(710000, base.Add(48*time.Hour))
	beforeParse := proofRawParses.Load()
	beforeFull := proofOddFullClassifications.Load()
	if err := store.RecordBatch([]usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	if delta := proofOddFullClassifications.Load() - beforeFull; delta != 0 {
		t.Fatalf("write batch classified %d rows by full scan, want 0", delta)
	}
	if delta := proofRawParses.Load() - beforeParse; delta >= history/10 {
		t.Fatalf("write batch parsed %d rows, want history (%d) to stay out of scope", delta, history)
	}
}

// Migration is bounded per transaction and resumes after cancellation.
func TestProofOddIndexMigrationResumes(t *testing.T) {
	const history = 300
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-9 * time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < history; i++ {
			e := scaleEvent(720000+i, base.Add(time.Duration(i)*time.Minute))
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := raw.Put([]byte(usageKey(e.Timestamp, e.RecordID)), data); err != nil {
				return err
			}
			// Legacy-shaped row: no record id, so it is an odd row.
			legacy := e
			legacy.RecordID = ""
			legacy.ClientName = fmt.Sprintf("legacy-%02d", i%7)
			ld, err := json.Marshal(legacy)
			if err != nil {
				return err
			}
			if err := raw.Put([]byte(usageKey(e.Timestamp, fmt.Sprintf("legacy-%d", i))), ld); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := MigrateUsageProofOddIndex(ctx, db, 7); err == nil {
		t.Fatal("cancelled migration returned nil")
	}
	if proofOddReadyNow(t, db) {
		t.Fatal("cancelled migration must not mark the index ready")
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 7); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not finish")
	}
	requireProofOddIndexMatches(t, db)
}

// Every supported writer must keep the persisted classification in step with
// a full scan, including rewrites, retention deletes and quarantine.
func TestProofOddIndexFollowsSupportedWriters(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	events := make([]usage.Event, 4)
	for i := range events {
		events[i] = scaleEvent(730000+i, base.Add(time.Duration(i)*time.Minute))
	}
	if err := store.RecordBatch(events); err != nil {
		t.Fatal(err)
	}
	// Out-of-band odd rows are indexed by the migration, which is the documented
	// repair for unsupported writers. One is corrupt and retention quarantines it.
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if err := raw.Put([]byte(usageKey(base.Add(30*time.Second), "legacy-x")), []byte(`{"ClientName":"c01","Timestamp":"`+base.Add(30*time.Second).Format(time.RFC3339Nano)+`","Model":"qwen-max"}`)); err != nil {
			return err
		}
		return raw.Put([]byte(usageKey(base.Add(31*time.Second), "garbage-x")), []byte("not json"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 2); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
	// A retention pass writes receipts, removes raw rows and quarantines
	// corrupt rows in the same transaction.
	if _, err := store.PruneEventsBefore(base.Add(90*time.Second), 1000); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
	// ReindexUsageEvent rewrites one retained row in place.
	var key []byte
	var before, after []byte
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		k, v := raw.Cursor().First()
		if k == nil {
			return fmt.Errorf("no retained raw row")
		}
		key = append([]byte(nil), k...)
		before = append([]byte(nil), v...)
		var e usage.Event
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		e.InputTokens++
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		after = data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		// The contract validates that raw still holds before, then the caller
		// stores after in the same transaction.
		if err := ReindexUsageEvent(tx, key, before, after); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, after)
	}); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
}

// With the index ready, the scoped proof must return item-for-item the same
// verdicts as the exhaustive scan, including after random tampering. Tampering
// is out of contract, so the index is rebuilt from the tampered state first.
func TestProofOddIndexScopedVerdictsMatchExhaustive(t *testing.T) {
	rng := rand.New(rand.NewSource(114114))
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	rounds := 4
	if raceEnabled {
		rounds = 1
	}
	for round := 0; round < rounds; round++ {
		db, store := seedScaleStore(t, 180+rng.Intn(80), base)
		if round%2 == 0 {
			if _, err := store.PruneEventsBefore(base.Add(time.Duration(5+rng.Intn(20))*time.Minute), 1000); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
				return err
			}
			raw := tx.Bucket([]byte(BucketUsageEvents))
			receipts := tx.Bucket([]byte(BucketUsageReceipts))
			for i := 0; i < rng.Intn(6); i++ {
				at := base.Add(time.Duration(rng.Intn(24*3600)) * time.Second)
				switch rng.Intn(6) {
				case 0: // legacy row with a parseable key stamp
					e := fixedPlainEvent()
					e.RecordID = ""
					e.ClientName = fmt.Sprintf("c%02d", rng.Intn(5))
					e.Timestamp = at
					data, _ := json.Marshal(e)
					_ = raw.Put([]byte(usageKey(at, fmt.Sprintf("legacy-%d", i))), data)
				case 1: // unparseable key stamp
					_ = raw.Put([]byte(fmt.Sprintf("weird-%d", i)), []byte(`{"client_name":"c01"}`))
				case 2: // malformed bytes at a parseable stamp
					_ = raw.Put([]byte(usageKey(at, fmt.Sprintf("garbage-%d", i))), []byte("not json"))
				case 3: // offset-zone key whose data is canonical
					e := scaleEvent(740000+i, at)
					data, _ := json.Marshal(e)
					_ = raw.Put([]byte(at.In(time.FixedZone("x", 8*3600)).Format(time.RFC3339Nano)+"_"+e.RecordID), data)
				case 4: // ownerless odd receipt
					_ = receipts.Put([]byte(fmt.Sprintf("badreceipt-%d", i)), []byte(`{"version":1,"digest":"x"}`))
				case 5: // offset-zone receipt stamp
					_ = receipts.Put([]byte(at.In(time.FixedZone("x", 8*3600)).Format(time.RFC3339Nano)+"_offset"), []byte(`{"version":1,"digest":"x"}`))
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := ResetUsageProofOddIndex(db); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUsageProofOddIndex(context.Background(), db, 64); err != nil {
			t.Fatal(err)
		}
		requireProofOddIndexMatches(t, db)

		type cellRef struct {
			source string
			key    []byte
		}
		var cells []cellRef
		var existing [][]usage.Event
		var existingKeys [][]string
		var candidates [][]usage.Event
		_ = db.View(func(tx *bolt.Tx) error {
			c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			i := 0
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var e usage.Event
				if json.Unmarshal(v, &e) != nil || usageKey(e.Timestamp, e.RecordID) != string(k) {
					continue
				}
				if i%17 == 0 {
					cells = append(cells, cellRef{BucketUsageAggHourly, aggKey(e.Timestamp, e.EndpointType, e.ClientName, e.Model, e.Target)}, cellRef{BucketQuotaUsageAggHourly, quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)})
					existing = append(existing, []usage.Event{e})
					existingKeys = append(existingKeys, []string{string(k)})
					fresh := e
					fresh.RecordID = fmt.Sprintf("00000000-0000-4000-8000-%012x", 750000+i)
					fresh.Timestamp = e.Timestamp.Add(time.Millisecond)
					candidates = append(candidates, []usage.Event{fresh})
				}
				i++
			}
			return nil
		})
		evaluate := func(skip, summary bool) (string, int) {
			proofScopeSkipEnabled = skip
			defer func() { proofScopeSkipEnabled = true }()
			var out []string
			trues := 0
			_ = db.View(func(tx *bolt.Tx) error {
				ctx := context.Background()
				if summary {
					ctx = withTxScopeSummary(ctx, tx)
				}
				for i := range existing {
					fresh := freshScopeProvenContext(ctx, tx, candidates[i])
					cont := continuationScopeProvenMemoContext(ctx, tx, candidates[i], nil)
					rawProven, _ := usageAccountingRawProvenContext(ctx, tx, existing[i], existingKeys[i])
					retained, _ := usageRetainedRollupsProvenContext(ctx, tx, existing[i], existingKeys[i])
					var ids string
					for _, c := range cells[2*i : 2*i+2] {
						n, x, ok, _ := currentRollupIdentitiesContext(ctx, tx, c.source, c.key, nil)
						ids += fmt.Sprint(n, x, ok, ";")
					}
					out = append(out, fmt.Sprint(fresh, cont, rawProven, retained, ids))
					for _, b := range []bool{fresh, cont, rawProven, retained} {
						if b {
							trues++
						}
					}
				}
				return nil
			})
			return fmt.Sprint(out), trues
		}
		ref, _ := evaluate(false, false)
		skipOnly, _ := evaluate(true, false)
		beforeFull := proofOddFullClassifications.Load()
		scoped, _ := evaluate(true, true)
		if delta := proofOddFullClassifications.Load() - beforeFull; delta != 0 {
			t.Fatalf("round %d: scoped proof fell back to %d full classifications; the index was ready", round, delta)
		}
		if ref != skipOnly || ref != scoped {
			t.Fatalf("round %d: scans disagree\nexhaustive=%s\nskip=%s\nscoped=%s", round, ref, skipOnly, scoped)
		}
	}
}

// A ready marker that stops matching the objects, or a missing/damaged marker,
// must send the scoped walk back to the exhaustive classification. Falling
// through would let an undetected overlap prove a bundle that is not proven.
func TestProofOddIndexDamageFallsBack(t *testing.T) {
	const history = 40
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	canonicalHistory(t, db, history, base)
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		e := fixedPlainEvent()
		e.RecordID = ""
		e.Timestamp = base.Add(10 * time.Minute)
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return raw.Put([]byte(usageKey(e.Timestamp, "legacy-damaged")), data)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 4); err != nil {
		t.Fatal(err)
	}
	hours := map[time.Time]bool{base.Truncate(time.Hour): true}
	scan := func(t *testing.T) int64 {
		t.Helper()
		before := proofOddFullClassifications.Load()
		if err := db.View(func(tx *bolt.Tx) error {
			ctx := withTxScopeSummary(context.Background(), tx)
			return forEachRawInHours(ctx, tx, tx.Bucket([]byte(BucketUsageEvents)), hours, func(k, v []byte) error { return nil })
		}); err != nil {
			t.Fatal(err)
		}
		return proofOddFullClassifications.Load() - before
	}
	if delta := scan(t); delta != 0 {
		t.Fatalf("intact index fell back to %d full classifications", delta)
	}
	// An index built with other location semantics (no scheme marker, as an
	// older classification would leave it) is not trusted and is rebuilt.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(proofOddSchemeKey))
	}); err != nil {
		t.Fatal(err)
	}
	if delta := scan(t); delta == 0 {
		t.Fatal("missing scheme marker did not force the full-scan fallback")
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 4); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild an index with a missing scheme marker")
	}
	requireProofOddIndexMatches(t, db)
	if delta := scan(t); delta != 0 {
		t.Fatalf("rebuilt index fell back to %d full classifications", delta)
	}
	// Out-of-band rewrite of an indexed odd row: digest mismatch.
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		key := []byte(usageKey(base.Add(10*time.Minute), "legacy-damaged"))
		value := raw.Get(key)
		return raw.Put(key, append(append([]byte(nil), value...), ' '))
	}); err != nil {
		t.Fatal(err)
	}
	if delta := scan(t); delta == 0 {
		t.Fatal("rewritten indexed row did not force the full-scan fallback")
	}
	// Marker loss also forces the fallback.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(proofOddReadyKey))
	}); err != nil {
		t.Fatal(err)
	}
	if delta := scan(t); delta == 0 {
		t.Fatal("missing ready marker did not force the full-scan fallback")
	}
}

// A writer that advances the raw revision without maintaining the proof index
// (an older binary after a downgrade, or any out-of-band edit that mimics one)
// must make the persisted classification stale: proofs fall back immediately,
// and the next migration discards and rebuilds it instead of trusting entries
// that no longer describe the data.
func TestProofOddIndexStaleRevisionFallsBackAndRebuilds(t *testing.T) {
	const history = 40
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	canonicalHistory(t, db, history, base)
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 4); err != nil {
		t.Fatal(err)
	}
	hours := map[time.Time]bool{base.Truncate(time.Hour): true}
	scan := func(t *testing.T) int64 {
		t.Helper()
		before := proofOddFullClassifications.Load()
		if err := db.View(func(tx *bolt.Tx) error {
			ctx := withTxScopeSummary(context.Background(), tx)
			return forEachRawInHours(ctx, tx, tx.Bucket([]byte(BucketUsageEvents)), hours, func(k, v []byte) error { return nil })
		}); err != nil {
			t.Fatal(err)
		}
		return proofOddFullClassifications.Load() - before
	}
	if delta := scan(t); delta != 0 {
		t.Fatalf("intact index fell back to %d full classifications", delta)
	}
	// Simulate an older binary: it writes an odd raw row the index does not
	// know about and advances only the raw revision, as a writer that predates
	// the index does. bumpRawRevision itself now maintains the index, so the
	// revision meta key is advanced directly here.
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		e := fixedPlainEvent()
		e.RecordID = ""
		e.Timestamp = base.Add(10 * time.Minute)
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := raw.Put([]byte(usageKey(e.Timestamp, "legacy-old-binary")), data); err != nil {
			return err
		}
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawRevision(tx)+1)
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageRawRevisionKey), revision[:])
	}); err != nil {
		t.Fatal(err)
	}
	if proofOddReadyNow(t, db) {
		t.Fatal("a raw revision the index did not observe still reported the index ready")
	}
	if delta := scan(t); delta == 0 {
		t.Fatal("stale revision did not force the full-scan fallback")
	}
	// Migration must rebuild rather than accept the ready marker written for
	// the older revision.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 4); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild the index after a stale revision")
	}
	requireProofOddIndexMatches(t, db)
	if delta := scan(t); delta != 0 {
		t.Fatalf("rebuilt index fell back to %d full classifications", delta)
	}
}

// proofFreshVerdict returns the fresh-scope verdict for one candidate bundle,
// optionally through the persisted classification (scoped) or the exhaustive
// pre-#114 walk (reference).
func proofFreshVerdict(t *testing.T, db *bolt.DB, rows []usage.Event, scoped bool) bool {
	t.Helper()
	var proven bool
	if err := db.View(func(tx *bolt.Tx) error {
		ctx := context.Background()
		if scoped {
			ctx = withTxScopeSummary(ctx, tx)
		}
		proven = freshScopeProvenContext(ctx, tx, rows)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return proven
}

// An owner entry that becomes unreadable after the index was built makes
// proofRawMayOverlap treat its raw row as relevant at every hour. The scoped
// walk must return the same verdict as the exhaustive walk: the row has to stay
// global in the persisted classification (or the index must fall back), never
// be hidden under its own hour while another hour is proved.
func TestProofOddIndexOwnerDamageMatchesExhaustive(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	// A canonical row establishes receipt and watermark state. It shares no
	// hour with the proved scope and is skipped by key prefix.
	if err := store.RecordBatch([]usage.Event{scaleEvent(760000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	odd := fixedPlainEvent()
	odd.RecordID = ""
	odd.ClientName = "legacy-owner"
	odd.Timestamp = base.Add(10 * time.Minute)
	oddKey := []byte(usageKey(odd.Timestamp, "legacy-owner-key"))
	oddData, err := json.Marshal(odd)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put(oddKey, oddData); err != nil {
			return err
		}
		return writeRawOwner(tx, oddKey, odd)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
	candidate := []usage.Event{scaleEvent(761000, base.Add(2*time.Hour+5*time.Minute))}
	if !proofFreshVerdict(t, db, candidate, false) || !proofFreshVerdict(t, db, candidate, true) {
		t.Fatal("healthy owner: both scans must prove the empty candidate scope")
	}
	// Corrupt one owner entry after the index observed it.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(rawOwnerBucket)).Put(oddKey, []byte(`{"version":1}`))
	}); err != nil {
		t.Fatal(err)
	}
	clearProofScanCacheForTest()
	exhaustive := proofFreshVerdict(t, db, candidate, false)
	scoped := proofFreshVerdict(t, db, candidate, true)
	if exhaustive {
		t.Fatal("the reference proof accepted a scope next to a damaged owner entry")
	}
	if scoped != exhaustive {
		t.Fatalf("scoped verdict %v, exhaustive verdict %v after owner damage", scoped, exhaustive)
	}
	// The owner-bound row must be global so every proof re-reads its owner.
	if err := db.View(func(tx *bolt.Tx) error {
		pointer := tx.Bucket([]byte(bucketUsageProofOdd)).Get(proofOddPointerKey(proofOddRawPointer, oddKey))
		kind, _, ok := proofOddParsePointer(pointer)
		if !ok || kind != proofOddRawGlobal {
			return fmt.Errorf("owner-bound odd row indexed as %v, want global", pointer)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Losing the owner source after the index was built (the bucket disappears
// while its meta markers stay, or any other rawOwnerSource inconsistency) makes
// readRawOwner fail for every odd row, so the exhaustive walk treats all of
// them as relevant at any hour. The scoped walk must stop trusting hour-located
// entries before that divergence can prove a scope the reference rejects.
func TestProofOddIndexOwnerSourceLossMatchesExhaustive(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	// The supported writer creates the owner bucket and its markers.
	if err := store.RecordBatch([]usage.Event{scaleEvent(770000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	odd := fixedPlainEvent()
	odd.RecordID = ""
	odd.ClientName = "legacy-unowned"
	odd.Timestamp = base.Add(10 * time.Minute)
	oddKey := []byte(usageKey(odd.Timestamp, "legacy-unowned-key"))
	oddData, err := json.Marshal(odd)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		// No owner is written for this row, so it is hour-located on its own.
		return tx.Bucket([]byte(BucketUsageEvents)).Put(oddKey, oddData)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
	candidate := []usage.Event{scaleEvent(771000, base.Add(2*time.Hour+5*time.Minute))}
	if !proofFreshVerdict(t, db, candidate, false) || !proofFreshVerdict(t, db, candidate, true) {
		t.Fatal("healthy owner source: both scans must prove the empty candidate scope")
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket([]byte(rawOwnerBucket))
	}); err != nil {
		t.Fatal(err)
	}
	clearProofScanCacheForTest()
	exhaustive := proofFreshVerdict(t, db, candidate, false)
	scoped := proofFreshVerdict(t, db, candidate, true)
	if exhaustive {
		t.Fatal("the reference proof accepted a scope while the owner source was missing")
	}
	if scoped != exhaustive {
		t.Fatalf("scoped verdict %v, exhaustive verdict %v after owner source loss", scoped, exhaustive)
	}
	// The revision and scheme are untouched, so the structural check still
	// passes; readiness itself must fail while the owner source is unreadable,
	// and that is what forces the fallback.
	if err := db.View(func(tx *bolt.Tx) error {
		if !proofOddIndexCurrent(tx) {
			return fmt.Errorf("index structure stopped being current after owner source loss")
		}
		if proofOddIndexReady(tx) {
			return fmt.Errorf("index reported ready while the owner source was missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Migration must terminate instead of rebuilding forever while the source
	// is unreadable; the proof-path readiness check carries the fallback.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if proofOddReadyNow(t, db) {
		t.Fatal("the index reported ready after migrating with the owner source missing")
	}
}

// The repair entry point is called from maintenance code with a database the
// caller opened, so it reports a missing one instead of panicking.
func TestResetUsageProofOddIndexRejectsNilDatabase(t *testing.T) {
	if err := ResetUsageProofOddIndex(nil); err == nil {
		t.Fatal("reset accepted a nil database")
	}
}

// Guard: every file that can mutate raw usage rows or receipts is listed here
// and maintains the persisted classification in the same transaction.
func TestProofOddIndexWritersAreEnumerated(t *testing.T) {
	// Files that read the raw or receipt buckets; new ones must be reviewed.
	readers := map[string]bool{
		"db.go": true, "migrate.go": true, "usage.go": true,
		"usage_accounting_proof.go": true, "usage_activity.go": true, "usage_agg.go": true,
		"usage_backfill_snapshot.go": true, "usage_client_index.go": true,
		"usage_client_index_exceptions.go": true, "usage_client_index_migration.go": true,
		"usage_client_index_v2.go": true, "usage_conflict_scope.go": true, "usage_costs.go": true,
		"usage_direct_integrity.go": true, "usage_fresh_attestation.go": true,
		"usage_integrity_class.go": true, "usage_proof_odd_index.go": true,
		"usage_proof_scan_cache.go": true, "usage_quarantine.go": true,
		"usage_receipts.go": true, "usage_recheck.go": true, "usage_rollup_proof.go": true,
	}
	writers := map[string][]string{
		"usage_receipts.go":     {"proofOddNoteRaw", "proofOddNoteReceipt"},
		"usage_client_index.go": {"proofOddNoteRaw"},
		"migrate.go":            {"proofOddNoteRaw"},
		"usage.go":              {"proofOddRemoveRaw"},
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		if (strings.Contains(text, "BucketUsageEvents") || strings.Contains(text, "BucketUsageReceipts")) && !readers[f] {
			t.Errorf("%s references the raw or receipt bucket; review whether it mutates them and must maintain the proof odd index", f)
		}
	}
	for f, want := range writers {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range want {
			if !strings.Contains(string(src), marker) {
				t.Errorf("%s is a supported raw/receipt writer but does not call %s", f, marker)
			}
		}
	}
	// Direct index writes stay inside usage_proof_odd_index.go.
	files, _ = filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "usage_proof_odd_index.go" {
			continue
		}
		src, _ := os.ReadFile(f)
		if regexp.MustCompile(`\.(Put|Delete)\(\s*proofOdd(PointerKey|LocationKey)\(`).Match(src) {
			t.Errorf("%s writes proof odd index entries outside usage_proof_odd_index.go", f)
		}
	}
}
