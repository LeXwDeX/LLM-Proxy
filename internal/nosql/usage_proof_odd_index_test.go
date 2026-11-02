package nosql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
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

// countingErrContext makes the cancellation point deterministic: the migration
// loop's own check consumes the first Err call, and the next one, made inside
// the verification pass, reports cancellation. Verification runs in one write
// transaction that bbolt cannot interrupt, so this pins the interior check that
// keeps a large index from outliving the startup budget.
type countingErrContext struct {
	context.Context
	calls int
	limit int
}

func (c *countingErrContext) Err() error {
	c.calls++
	if c.calls > c.limit {
		return context.Canceled
	}
	return nil
}

func TestProofOddIndexVerificationObservesCancellation(t *testing.T) {
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-9 * time.Hour)
	// More odd rows than one verification checkpoint, so the pass reaches an
	// interior check rather than finishing first.
	const history = 600
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < history; i++ {
			e := scaleEvent(740000+i, base.Add(time.Duration(i)*time.Minute))
			// Legacy-shaped row: no record id, so it is an odd row.
			legacy := e
			legacy.RecordID = ""
			data, err := json.Marshal(legacy)
			if err != nil {
				return err
			}
			if err := raw.Put([]byte(usageKey(e.Timestamp, fmt.Sprintf("legacy-%d", i))), data); err != nil {
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
	if err := MigrateUsageProofOddIndex(context.Background(), db, 50); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not finish")
	}

	ctx := &countingErrContext{Context: context.Background(), limit: 1}
	if err := MigrateUsageProofOddIndex(ctx, db, 50); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification returned %v, want context.Canceled", err)
	}
	// The interrupted pass must roll back instead of discarding a sound index.
	if !proofOddReadyNow(t, db) {
		t.Fatal("cancelled verification discarded the completed index")
	}
	requireProofOddIndexMatches(t, db)
	if err := MigrateUsageProofOddIndex(context.Background(), db, 50); err != nil {
		t.Fatalf("later start could not verify the index: %v", err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("later start discarded the completed index")
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
	// A single damaged round can legitimately leave every proof false, but the
	// whole test must compare at least one true verdict; otherwise both scans
	// could report the same all-false string and prove nothing.
	proofTrues := 0
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
		ref, refTrues := evaluate(false, false)
		proofTrues += refTrues
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
	if proofTrues == 0 {
		t.Fatalf("every exhaustive proof verdict is false across all rounds, so comparing the scans proves nothing")
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

// A build interrupted by the pre-scheme binary leaves entries located by the
// old semantics (owner-bound rows under their key hour), staged progress in the
// phase or raw cursor keys, and no scheme marker. Resuming that state and
// stamping the current scheme would trust hour-located owner rows, so a scoped
// proof could skip a row the exhaustive walk rejects. The migration must
// discard such a partial build and classify every row again.
func TestProofOddIndexRebuildsLegacyPartialBuilds(t *testing.T) {
	for _, legacyState := range []string{"raw_cursor", "receipts_cursor", "entries_only"} {
		t.Run(legacyState, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
			// A supported write establishes the owner source and a receipt.
			if err := store.RecordBatch([]usage.Event{scaleEvent(780000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
				t.Fatal(err)
			}
			odd := fixedPlainEvent()
			odd.RecordID = ""
			odd.ClientName = "legacy-scheme1"
			odd.Timestamp = base.Add(10 * time.Minute)
			oddKey := []byte(usageKey(odd.Timestamp, "legacy-scheme1-key"))
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
			// Reproduce the pre-scheme partial build: the row was classified by
			// its key hour without an owner rule, no scheme marker exists, and
			// progress sits exactly where the old migration leaves it.
			if err := db.Update(func(tx *bolt.Tx) error {
				hour := odd.Timestamp.UTC().Truncate(time.Hour).Format(hourLayout)
				b := proofOddBucket(tx)
				if err := b.Put(proofOddLocationKey(proofOddRawHour, hour, oddKey), proofOddDigest(oddKey, oddData)); err != nil {
					return err
				}
				if err := b.Put(proofOddPointerKey(proofOddRawPointer, oddKey), proofOddPointerValue(proofOddRawHour, hour)); err != nil {
					return err
				}
				meta := proofOddMeta(tx)
				if legacyState == "receipts_cursor" {
					// A partial receipt scan implies a receipt to resume after.
					if err := writeUsageReceipt(tx, oddKey, "legacy-digest"); err != nil {
						return err
					}
				}
				switch legacyState {
				case "raw_cursor":
					// The interrupted raw scan had already passed this row.
					return meta.Put([]byte(proofOddRawCursorKey), []byte(usageKey(odd.Timestamp.Add(time.Second), "past")))
				case "receipts_cursor":
					// The raw phase finished; the receipt scan stopped partway.
					if err := meta.Put([]byte(proofOddPhaseKey), []byte("receipts")); err != nil {
						return err
					}
					k, _ := tx.Bucket([]byte(BucketUsageReceipts)).Cursor().First()
					if k == nil {
						return fmt.Errorf("test setup: no receipt to point the cursor at")
					}
					return meta.Put([]byte(proofOddReceiptCursorKey), append([]byte(nil), k...))
				}
				// entries_only: the old semantics already wrote entries but no
				// progress marker survived. Unlike the cursor states, this
				// shape recovers under the old implementation's full rescan,
				// so it is not a fail-open reproduction; it guards the rule
				// that entries without the current scheme are reset and
				// rebuilt rather than resumed under the new semantics.
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				meta := proofOddMeta(tx)
				if meta.Get([]byte(proofOddSchemeKey)) != nil || meta.Get([]byte(proofOddReadyKey)) != nil {
					return fmt.Errorf("legacy partial build unexpectedly carried scheme/ready markers")
				}
				pointer := proofOddBucket(tx).Get(proofOddPointerKey(proofOddRawPointer, oddKey))
				if kind, _, ok := proofOddParsePointer(pointer); !ok || kind != proofOddRawHour {
					return fmt.Errorf("legacy partial build pointer = %v, want hour-located", pointer)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := MigrateUsageProofOddIndex(context.Background(), db, 4); err != nil {
				t.Fatal(err)
			}
			if !proofOddReadyNow(t, db) {
				t.Fatal("migration did not finish after discarding the legacy partial build")
			}
			requireProofOddIndexMatches(t, db)
			if err := db.View(func(tx *bolt.Tx) error {
				meta := proofOddMeta(tx)
				if string(meta.Get([]byte(proofOddSchemeKey))) != proofOddScheme {
					return fmt.Errorf("scheme = %q, want %q", meta.Get([]byte(proofOddSchemeKey)), proofOddScheme)
				}
				pointer := proofOddBucket(tx).Get(proofOddPointerKey(proofOddRawPointer, oddKey))
				if kind, _, ok := proofOddParsePointer(pointer); !ok || kind != proofOddRawGlobal {
					return fmt.Errorf("rebuilt owner-bound row pointer = %v, want global", pointer)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Owner damage after the rebuild must not be hidden by an
			// hour-located entry.
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(rawOwnerBucket)).Put(oddKey, []byte(`{"version":1}`))
			}); err != nil {
				t.Fatal(err)
			}
			clearProofScanCacheForTest()
			candidate := []usage.Event{scaleEvent(781000, base.Add(2*time.Hour+5*time.Minute))}
			exhaustive := proofFreshVerdict(t, db, candidate, false)
			scoped := proofFreshVerdict(t, db, candidate, true)
			if exhaustive {
				t.Fatal("the reference proof accepted a scope next to a damaged owner entry")
			}
			if scoped != exhaustive {
				t.Fatalf("scoped verdict %v, exhaustive verdict %v after owner damage", scoped, exhaustive)
			}
		})
	}
}

// proofOddInterruptContext cancels after a fixed number of Err probes, so a
// test can stop the migration after an exact number of committed transactions
// without racing a timer.
type proofOddInterruptContext struct {
	remaining int
}

func (c *proofOddInterruptContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *proofOddInterruptContext) Done() <-chan struct{}       { return nil }
func (c *proofOddInterruptContext) Err() error {
	c.remaining--
	if c.remaining < 0 {
		return context.Canceled
	}
	return nil
}
func (c *proofOddInterruptContext) Value(any) any { return nil }

// A fresh build must record its scheme in the same transaction as the first raw
// batch. Otherwise a restart of an interrupted scheme-2 migration would see
// progress without a scheme and start over, and an interrupted legacy build
// could be resumed with entries classified by the old semantics.
func TestProofOddIndexFirstRawTransactionRecordsScheme(t *testing.T) {
	const rows = 4
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < rows; i++ {
			e := fixedPlainEvent()
			e.RecordID = ""
			e.ClientName = fmt.Sprintf("legacy-%d", i)
			e.Timestamp = base.Add(time.Duration(i) * time.Minute)
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := raw.Put([]byte(usageKey(e.Timestamp, fmt.Sprintf("legacy-scheme-%d", i))), data); err != nil {
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
	// Two Err probes: the loop entry of the first transaction and the first row
	// of the raw batch. The third probe cancels after that transaction commits.
	ctx := &proofOddInterruptContext{remaining: 2}
	if err := MigrateUsageProofOddIndex(ctx, db, 1); err == nil {
		t.Fatal("interrupted migration returned nil")
	}
	if proofOddReadyNow(t, db) {
		t.Fatal("interrupted migration marked the index ready")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		meta := proofOddMeta(tx)
		if string(meta.Get([]byte(proofOddSchemeKey))) != proofOddScheme {
			return fmt.Errorf("scheme after the first committed raw transaction = %q, want %q", meta.Get([]byte(proofOddSchemeKey)), proofOddScheme)
		}
		if meta.Get([]byte(proofOddRawCursorKey)) == nil {
			return fmt.Errorf("interrupted raw scan did not persist its cursor")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A scheme-matching partial build resumes instead of starting over: the
	// sentinel would not survive a bucket reset.
	if err := db.Update(func(tx *bolt.Tx) error {
		return proofOddBucket(tx).Put([]byte("sentinel"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 1); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("resumed migration did not finish")
	}
	requireProofOddIndexMatches(t, db)
	if err := db.View(func(tx *bolt.Tx) error {
		if proofOddBucket(tx).Get([]byte("sentinel")) == nil {
			return fmt.Errorf("migration discarded a scheme-matching partial build")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A migration interrupted after the raw phase is bound to the raw revision that
// phase scanned. If a writer advances the revision before the receipt phase
// finishes without maintaining the index (an older binary or an out-of-band
// edit), stamping the live revision would report a complete index that never
// observed part of the data. The migration must discard and rebuild, and the
// scoped proof must never be more permissive than the exhaustive walk meanwhile.
func TestProofOddIndexCrossPhaseRevisionBumpRebuilds(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	recorded := scaleEvent(790000, base.Add(3*time.Hour+2*time.Minute))
	recordedKey := []byte(usageKey(recorded.Timestamp, recorded.RecordID))
	if err := store.RecordBatch([]usage.Event{recorded}); err != nil {
		t.Fatal(err)
	}
	// Produce through the fresh-attestation path, as the other scope tests do.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	candidate := []usage.Event{scaleEvent(791000, base.Add(2*time.Hour+5*time.Minute))}
	if !proofFreshVerdict(t, db, candidate, false) || !proofFreshVerdict(t, db, candidate, true) {
		t.Fatal("healthy history: both scans must prove the empty candidate scope")
	}
	// Rewind to the state an interrupted build leaves after its raw phase:
	// scheme and the raw-scan revision snapshot are present, the receipt phase
	// has not finished, and neither the ready nor the completion revision is set.
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := proofOddMeta(tx)
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawRevision(tx))
		if err := meta.Put([]byte(proofOddRawRevisionKey), revision[:]); err != nil {
			return err
		}
		if err := meta.Delete([]byte(proofOddReadyKey)); err != nil {
			return err
		}
		if err := meta.Delete([]byte(proofOddRevisionKey)); err != nil {
			return err
		}
		return meta.Put([]byte(proofOddPhaseKey), []byte("receipts"))
	}); err != nil {
		t.Fatal(err)
	}
	// An out-of-band writer rewrites the row's data (same key, timestamp moved
	// into the candidate hour, a different client) and advances only the raw
	// revision. The row was canonical when the raw phase scanned it, so the
	// index holds no entry for it and the resumed receipt phase would stamp the
	// changed revision without ever scanning the change.
	moved := recorded
	moved.Timestamp = base.Add(2*time.Hour + 5*time.Minute)
	moved.ClientName = "moved-client"
	movedData, err := json.Marshal(moved)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put(recordedKey, movedData); err != nil {
			return err
		}
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawRevision(tx)+1)
		return proofOddMeta(tx).Put([]byte(usageRawRevisionKey), revision[:])
	}); err != nil {
		t.Fatal(err)
	}
	clearProofScanCacheForTest()
	if proofOddReadyNow(t, db) {
		t.Fatal("an unfinished build reported the index ready")
	}
	exhaustive := proofFreshVerdict(t, db, candidate, false)
	scoped := proofFreshVerdict(t, db, candidate, true)
	if exhaustive {
		t.Fatal("the reference proof accepted a scope the rewritten row reaches")
	}
	if scoped != exhaustive {
		t.Fatalf("scoped verdict %v, exhaustive verdict %v across the revision window", scoped, exhaustive)
	}
	// Resuming must not stamp the live revision onto the old raw scan: it has
	// to discard the classification and rebuild from the current rows, after
	// which the scoped proof agrees with the exhaustive walk again.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not converge after rebuilding across the revision window")
	}
	exhaustive = proofFreshVerdict(t, db, candidate, false)
	scoped = proofFreshVerdict(t, db, candidate, true)
	if exhaustive {
		t.Fatal("the reference proof accepted a scope the rewritten row reaches")
	}
	if scoped != exhaustive {
		t.Fatalf("after rebuild: scoped verdict %v, exhaustive verdict %v", scoped, exhaustive)
	}
	requireProofOddIndexMatches(t, db)
}

// proofOddRewindToReceipts reproduces the state an interrupted build leaves
// after its raw phase: the raw-scan revision snapshot and the receipt phase are
// present, the ready and completion revision markers are not, and a sentinel
// entry records whether a later migration reset the bucket or resumed it.
func proofOddRewindToReceipts(t *testing.T, db *bolt.DB) uint64 {
	t.Helper()
	var live uint64
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := proofOddMeta(tx)
		live = rawRevision(tx)
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], live)
		if err := meta.Put([]byte(proofOddRawRevisionKey), revision[:]); err != nil {
			return err
		}
		if err := meta.Delete([]byte(proofOddReadyKey)); err != nil {
			return err
		}
		if err := meta.Delete([]byte(proofOddRevisionKey)); err != nil {
			return err
		}
		if err := meta.Put([]byte(proofOddPhaseKey), []byte("receipts")); err != nil {
			return err
		}
		return proofOddBucket(tx).Put([]byte("sentinel"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	return live
}

func proofOddSentinelPresent(t *testing.T, db *bolt.DB) bool {
	t.Helper()
	var present bool
	if err := db.View(func(tx *bolt.Tx) error {
		present = proofOddBucket(tx).Get([]byte("sentinel")) != nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return present
}

// A supported write between the raw and receipt phases updates the index in its
// own transaction and advances the receipt-phase revision snapshot, so a build
// interrupted after its raw phase resumes across starts instead of rebuilding.
// Without the snapshot advance the next start treats the build as stale, resets
// it and rescans the whole history: correct, but the #114 performance goal is
// lost and "next start resumes" is false.
func TestProofOddIndexResumesAfterSupportedWriteBetweenPhases(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	if err := store.RecordBatch([]usage.Event{scaleEvent(800000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	rawBefore := proofOddRewindToReceipts(t, db)
	// A real supported write after the raw phase: RecordBatch maintains the
	// index and bumps the raw revision in the same transaction.
	if err := store.RecordBatch([]usage.Event{scaleEvent(801000, base.Add(4*time.Hour+5*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	var snapshot, live uint64
	if err := db.View(func(tx *bolt.Tx) error {
		live = rawRevision(tx)
		stored := proofOddMeta(tx).Get([]byte(proofOddRawRevisionKey))
		if len(stored) != 8 {
			return fmt.Errorf("raw revision snapshot missing after a supported write")
		}
		snapshot = binary.BigEndian.Uint64(stored)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if live != rawBefore+1 {
		t.Fatalf("live revision = %d after the supported write, want %d", live, rawBefore+1)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	// Check the resume outcome first: a build without the snapshot advance is
	// reset here, which is the user-visible failure this case guards.
	if !proofOddSentinelPresent(t, db) {
		t.Fatal("the receipt phase reset a partial build that a supported write had kept current")
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("resumed migration did not finish")
	}
	requireProofOddIndexMatches(t, db)
	// The snapshot was read before the migration, which deletes it on success.
	if snapshot != live {
		t.Fatalf("receipt-phase snapshot = %d after the supported write, want the live revision %d", snapshot, live)
	}
}

// A supported write must not advance the receipt-phase snapshot past a
// revision only an out-of-band writer produced. Such a writer bumps the raw
// revision without maintaining the index, so the raw phase never observed its
// change; adopting the revision here would launder that inconsistency into a
// resumable build and stamp a complete index over unobserved data. The write
// after the out-of-band bump still maintains its own change; only the snapshot
// advance is withheld, and the next migration rebuilds.
func TestProofOddIndexSupportedWriteDoesNotLaunderOutOfBandBump(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	if err := store.RecordBatch([]usage.Event{scaleEvent(810000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	rawBefore := proofOddRewindToReceipts(t, db)
	// Simulate an older binary or another unsupported writer: it advances only
	// the raw revision and leaves the classification untouched.
	if err := db.Update(func(tx *bolt.Tx) error {
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawBefore+1)
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageRawRevisionKey), revision[:])
	}); err != nil {
		t.Fatal(err)
	}
	// The next supported write bumps the revision again and maintains the index
	// for its own change, but must not adopt the unobserved revision.
	if err := store.RecordBatch([]usage.Event{scaleEvent(811000, base.Add(4*time.Hour+5*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	var snapshot uint64
	if err := db.View(func(tx *bolt.Tx) error {
		stored := proofOddMeta(tx).Get([]byte(proofOddRawRevisionKey))
		if len(stored) != 8 {
			return fmt.Errorf("raw revision snapshot missing after a supported write")
		}
		snapshot = binary.BigEndian.Uint64(stored)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	// The build must be discarded: resuming would bind the live revision to a
	// raw scan that never saw the out-of-band change.
	if proofOddSentinelPresent(t, db) {
		t.Fatal("the receipt phase resumed after an unobserved out-of-band revision bump")
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild after the out-of-band bump")
	}
	requireProofOddIndexMatches(t, db)
	// The snapshot was read before the migration, which tears it down on reset.
	if snapshot != rawBefore {
		t.Fatalf("receipt-phase snapshot = %d after an out-of-band bump, want %d", snapshot, rawBefore)
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
		// Added by concurrent branches: both only read these buckets (#146
		// retention stats; #125 P2 client-scoped quota reads).
		"usage_retention_stats.go": true, "usage_integrity_clientscope.go": true,
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

// A completed index whose stored revision lags the live one (an older binary
// or an out-of-band writer advanced the raw revision without maintaining the
// classification) must not be re-stamped as current by the next supported
// write: that write only maintained its own row, so adopting the live revision
// would hide everything the out-of-band writer did.
func TestProofOddIndexCompletedWriteDoesNotLaunderOutOfBandBump(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	if err := store.RecordBatch([]usage.Event{scaleEvent(820000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not complete")
	}
	// An older binary or an out-of-band editor inserts an odd row the index
	// never sees and advances only the raw revision.
	if err := db.Update(func(tx *bolt.Tx) error {
		e := fixedPlainEvent()
		e.RecordID = ""
		e.ClientName = "out-of-band"
		e.Timestamp = base.Add(2*time.Hour + 4*time.Minute)
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(e.Timestamp, "legacy-completed")), data); err != nil {
			return err
		}
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawRevision(tx)+1)
		return proofOddMeta(tx).Put([]byte(usageRawRevisionKey), revision[:])
	}); err != nil {
		t.Fatal(err)
	}
	// The next supported write maintains its own row and advances the revision
	// again; it must not adopt the revision it never observed.
	if err := store.RecordBatch([]usage.Event{scaleEvent(821000, base.Add(5*time.Hour+6*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if proofOddReadyNow(t, db) {
		t.Fatal("a supported write laundered an out-of-band revision bump into a current index")
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild after the out-of-band bump")
	}
	requireProofOddIndexMatches(t, db)
}

// A raw phase that resumes across starts must not stamp the live revision when
// it finishes: an out-of-band writer that advanced the revision while the scan
// was paused can insert an odd row behind the persisted cursor, which the
// resumed scan never visits. Stamping the live revision would mark the index
// ready over data it never observed.
func TestProofOddIndexRawPhaseOutOfBandBumpRebuilds(t *testing.T) {
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < 3; i++ {
			e := fixedPlainEvent()
			e.RecordID = ""
			e.ClientName = fmt.Sprintf("legacy-%d", i)
			e.Timestamp = base.Add(time.Duration(10*(i+1)) * time.Minute)
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := raw.Put([]byte(usageKey(e.Timestamp, fmt.Sprintf("legacy-raw-%d", i))), data); err != nil {
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
	// Stop after the first raw batch commits, leaving a persisted cursor.
	ctx := &proofOddInterruptContext{remaining: 2}
	if err := MigrateUsageProofOddIndex(ctx, db, 1); err == nil {
		t.Fatal("interrupted migration returned nil")
	}
	// The out-of-band row sorts before the cursor, so the resumed scan never
	// reaches it.
	if err := db.Update(func(tx *bolt.Tx) error {
		e := fixedPlainEvent()
		e.RecordID = ""
		e.ClientName = "out-of-band"
		e.Timestamp = base.Add(time.Minute)
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(e.Timestamp, "legacy-before-cursor")), data); err != nil {
			return err
		}
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawRevision(tx)+1)
		return proofOddMeta(tx).Put([]byte(usageRawRevisionKey), revision[:])
	}); err != nil {
		t.Fatal(err)
	}
	// Resuming must notice the revision it never observed and rebuild.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not finish")
	}
	requireProofOddIndexMatches(t, db)
}

// An index bucket that keeps its markers and revision but loses one location
// entry must not read as "this range has no odd rows": the scoped proof would
// skip a row the exhaustive walk acts on. The loaders reconcile the entries
// they visited with the per-kind counts the writers maintain, and the startup
// migration verifies the same counts against the bucket.
func TestProofOddIndexMissingLocationEntryFallsBack(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	if err := store.RecordBatch([]usage.Event{scaleEvent(830000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	// Produce through the fresh-attestation path, as the other scope tests do.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
	}); err != nil {
		t.Fatal(err)
	}
	// The proof names one hour; the odd row below sits in that hour so the
	// exhaustive walk acts on it.
	candidate := []usage.Event{scaleEvent(831000, base.Add(2*time.Hour+10*time.Minute))}
	// A row whose key stamp cannot be parsed is classified global: it is
	// relevant at every hour, and its empty client matches every proof.
	odd := fixedPlainEvent()
	odd.RecordID = ""
	odd.ClientName = ""
	odd.Timestamp = candidate[0].Timestamp
	oddData, err := json.Marshal(odd)
	if err != nil {
		t.Fatal(err)
	}
	globalKey := []byte("zzz_legacy-global")
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(globalKey, oddData)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not complete")
	}
	// Drop the location entry, keep the pointer: the bucket no longer lists
	// the row, but every marker still says the index is complete.
	if err := db.Update(func(tx *bolt.Tx) error {
		return proofOddBucket(tx).Delete(proofOddLocationKey(proofOddRawGlobal, "", globalKey))
	}); err != nil {
		t.Fatal(err)
	}
	exhaustive := proofFreshVerdict(t, db, candidate, false)
	scoped := proofFreshVerdict(t, db, candidate, true)
	if exhaustive {
		t.Fatal("the reference proof accepted a scope the global odd row reaches")
	}
	if scoped != exhaustive {
		t.Fatalf("scoped verdict %v, exhaustive verdict %v after a missing location entry", scoped, exhaustive)
	}
	// A later migration must rebuild the classification instead of trusting
	// the markers, after which the index matches a full classification again.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild after a missing location entry")
	}
	requireProofOddIndexMatches(t, db)
}

// A location entry whose recorded digest no longer matches the stored bytes is
// invisible to the markers and to the per-kind counts: nothing advanced them,
// yet the scoped loaders must refuse the entry and fall back. The startup
// migration has to notice the same damage and rebuild, otherwise a single
// corrupted digest puts every scoped proof on the full scan until an operator
// resets the index by hand.
func TestProofOddIndexDigestDamageRebuilds(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	if err := store.RecordBatch([]usage.Event{scaleEvent(840000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	// A row whose key stamp cannot be parsed is classified global and stays
	// observable from every hour.
	odd := fixedPlainEvent()
	odd.RecordID = ""
	odd.ClientName = ""
	odd.Timestamp = base.Add(10 * time.Minute)
	oddKey := []byte("zzz_legacy-digest")
	oddData, err := json.Marshal(odd)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
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
	// Corrupt the recorded digest while leaving the pointer and the counts in
	// place. Only a digest check against the bucket can see this.
	if err := db.Update(func(tx *bolt.Tx) error {
		return proofOddBucket(tx).Put(proofOddLocationKey(proofOddRawGlobal, "", oddKey), bytes.Repeat([]byte{0xAB}, sha256.Size))
	}); err != nil {
		t.Fatal(err)
	}
	// The scoped loaders already refuse the damaged entry and fall back.
	if err := db.View(func(tx *bolt.Tx) error {
		if _, ok := proofOddLoadGlobal(tx, proofOddRawGlobal, tx.Bucket([]byte(BucketUsageEvents))); ok {
			t.Fatal("the scoped loader accepted an entry with a damaged digest")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The startup migration must discard and rebuild instead of trusting the
	// markers and leaving proofs on a permanent fallback.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild after digest damage")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		got := tx.Bucket([]byte(bucketUsageProofOdd)).Get(proofOddLocationKey(proofOddRawGlobal, "", oddKey))
		if want := proofOddDigest(oddKey, oddData); !bytes.Equal(got, want) {
			return fmt.Errorf("digest not repaired: got %x want %x", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
}

// Writing an already-present receipt again is how a supported writer repairs
// an index entry lost to an out-of-band restore: the receipt bucket keeps the
// source, so the second write re-notes the classification without adding a
// duplicate entry or disturbing the counts.
func TestProofOddIndexReceiptRewriteRepairsEntry(t *testing.T) {
	db := testDB(t)
	key := []byte("legacy-receipt_1")
	digest := "0f1e2d3c"
	if err := db.Update(func(tx *bolt.Tx) error {
		return writeUsageReceipt(tx, key, digest)
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
	// Simulate a restore of the receipt source without the derived index: the
	// entry, its pointer and its count are all gone.
	if err := db.Update(func(tx *bolt.Tx) error {
		b := proofOddBucket(tx)
		if err := b.Delete(proofOddLocationKey(proofOddReceiptGlobal, "", key)); err != nil {
			return err
		}
		if err := b.Delete(proofOddPointerKey(proofOddReceiptPointer, key)); err != nil {
			return err
		}
		return b.Delete(proofOddCountKey(proofOddReceiptGlobal, ""))
	}); err != nil {
		t.Fatal(err)
	}
	// The idempotent rewrite restores the entry and leaves the counts equal to
	// the bucket, so the scoped loaders accept the index again.
	if err := db.Update(func(tx *bolt.Tx) error {
		return writeUsageReceipt(tx, key, digest)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		index := proofOddBucket(tx)
		pointer := index.Get(proofOddPointerKey(proofOddReceiptPointer, key))
		if !bytes.Equal(pointer, proofOddPointerValue(proofOddReceiptGlobal, "")) {
			return fmt.Errorf("pointer after repair = %x", pointer)
		}
		receipt := tx.Bucket([]byte(BucketUsageReceipts)).Get(key)
		if got, want := index.Get(proofOddLocationKey(proofOddReceiptGlobal, "", key)), proofOddDigest(key, receipt); !bytes.Equal(got, want) {
			return fmt.Errorf("location digest after repair = %x, want %x", got, want)
		}
		if _, ok := proofOddLoadGlobal(tx, proofOddReceiptGlobal, tx.Bucket([]byte(BucketUsageReceipts))); !ok {
			return fmt.Errorf("the repaired index did not satisfy the receipt loader")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Rewriting a receipt with a different payload stays an error; the repair
	// path cannot launder a conflicting source.
	if err := db.Update(func(tx *bolt.Tx) error {
		return writeUsageReceipt(tx, key, "different")
	}); err == nil {
		t.Fatal("a conflicting receipt digest was accepted")
	}
	requireProofOddIndexMatches(t, db)
}

// This pins a known out-of-band boundary of the Issue #114 contract; it is not
// a statement of intended semantics. Removing a receipt's location entry, its
// pointer, and its per-kind count together leaves an index that is
// self-consistent in every way the index can check: Ready stays true, the
// global loader accepts an empty set, and the startup migration's O(index)
// verification finds nothing to reconcile, so it does not rebuild. A scoped
// proof then skips the receipt while the exhaustive walk treats it as
// overlapping the proved hours. The documented repair for this class of
// out-of-band restore is ResetUsageProofOddIndex followed by
// MigrateUsageProofOddIndex before serving.
func TestProofOddIndexTripleDeleteIsOutOfBandBoundary(t *testing.T) {
	db := testDB(t)
	// fixedPlainEvent and the clocked store keep the scope deterministic.
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	candidate := []usage.Event{fixedPlainEvent()}
	// An unparseable key stamp makes the receipt global, and with no readable
	// owner receiptMayOverlap reports it as relevant to every hour.
	receiptKey := []byte("legacy-receipt-boundary_1")
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		return writeUsageReceipt(tx, receiptKey, "0f1e2d3c")
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
	// Simulate a partial restore of the receipt side: the entry, its pointer,
	// and its count all disappear, yet every remaining marker stays in place.
	if err := db.Update(func(tx *bolt.Tx) error {
		b := proofOddBucket(tx)
		if err := b.Delete(proofOddLocationKey(proofOddReceiptGlobal, "", receiptKey)); err != nil {
			return err
		}
		if err := b.Delete(proofOddPointerKey(proofOddReceiptPointer, receiptKey)); err != nil {
			return err
		}
		return b.Delete(proofOddCountKey(proofOddReceiptGlobal, ""))
	}); err != nil {
		t.Fatal(err)
	}
	// (a) Every index-internal check still reports a trusted index.
	if stats := UsageProofIndexStats(db); !stats.Ready {
		t.Fatalf("triple deletion left the index untrusted: %+v", stats)
	}
	// (b) The global loader reads the missing receipt as "no global receipts".
	if err := db.View(func(tx *bolt.Tx) error {
		keys, ok := proofOddLoadGlobal(tx, proofOddReceiptGlobal, tx.Bucket([]byte(BucketUsageReceipts)))
		if !ok {
			return fmt.Errorf("the receipt loader rejected the self-consistent index")
		}
		if len(keys) != 0 {
			return fmt.Errorf("loaded %d global receipts, want 0", len(keys))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// (c) The startup migration verifies the remaining state and trusts it, so
	// the missing entry stays missing.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration discarded a self-consistent index")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		b := proofOddBucket(tx)
		if got := b.Get(proofOddLocationKey(proofOddReceiptGlobal, "", receiptKey)); got != nil {
			return fmt.Errorf("migration rebuilt the missing location entry: %x", got)
		}
		if got := b.Get(proofOddPointerKey(proofOddReceiptPointer, receiptKey)); got != nil {
			return fmt.Errorf("migration rebuilt the missing pointer: %x", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// (d) The two paths disagree: the exhaustive walk visits the restored
	// receipt and rejects the scope, while the scoped walk trusts the
	// self-consistent index and skips it. This is the documented boundary, not
	// the intended result.
	if err := db.View(func(tx *bolt.Tx) error {
		exhaustive := freshScopeProvenContext(context.Background(), tx, candidate)
		scoped := freshScopeProvenContext(withTxScopeSummary(context.Background(), tx), tx, candidate)
		if exhaustive {
			t.Fatal("exhaustive walk proved a scope the global receipt may overlap")
		}
		if !scoped {
			t.Fatal("scoped walk did not trust the self-consistent index")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// (e) The documented repair restores agreement and a matching index.
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not complete after reset")
	}
	requireProofOddIndexMatches(t, db)
	if err := db.View(func(tx *bolt.Tx) error {
		exhaustive := freshScopeProvenContext(context.Background(), tx, candidate)
		scoped := freshScopeProvenContext(withTxScopeSummary(context.Background(), tx), tx, candidate)
		if exhaustive || scoped {
			return fmt.Errorf("after repair exhaustive=%v scoped=%v, want both false", exhaustive, scoped)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Warm-up on a ready index visits only the global odd rows: a canonical
// history of any size and the hour-located odd rows (reached when their hour is
// proved) stay unread. The exhaustive fallback would visit every row, so the
// visited count distinguishes the fast path from the fallback.
func TestWarmUsageProofScanCacheVisitsOnlyGlobalOddRows(t *testing.T) {
	const history = 120
	db := testDB(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-12 * time.Hour)
	canonicalHistory(t, db, history, base)
	// One global odd row (unparseable stamp) and one hour-located odd row
	// (parseable stamp, no owner).
	global := fixedPlainEvent()
	global.RecordID = ""
	global.ClientName = "legacy-global"
	global.Timestamp = base.Add(30 * time.Minute)
	globalData, err := json.Marshal(global)
	if err != nil {
		t.Fatal(err)
	}
	hourly := fixedPlainEvent()
	hourly.RecordID = ""
	hourly.ClientName = "legacy-hourly"
	hourly.Timestamp = base.Add(90 * time.Minute)
	hourlyData, err := json.Marshal(hourly)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
			return err
		}
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if err := raw.Put([]byte("zzz_legacy-global-warm"), globalData); err != nil {
			return err
		}
		return raw.Put([]byte(usageKey(hourly.Timestamp, "legacy-hourly-warm")), hourlyData)
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
	clearProofScanCacheForTest()
	visited, err := WarmUsageProofScanCache(db)
	if err != nil {
		t.Fatal(err)
	}
	if visited != 1 {
		t.Fatalf("warm-up visited %d rows, want only the single global odd row", visited)
	}
}

// Startup warm-up on a ready index only classifies global odd rows, so the
// legacy history is left for the scoped proof walk to skip by key prefix. The
// first RecordOnce after that warm-up must use the same transaction scope
// summary as RecordBatch; otherwise it re-parses every hour-located legacy row
// while holding the bbolt writer, which is what warm-up exists to prevent.
func TestRecordOnceAfterReadyIndexWarmupDoesNotReparseLegacyRows(t *testing.T) {
	const legacyRows = 300
	db := testDB(t)
	base := fixedPlainEvent().Timestamp.UTC().Truncate(time.Hour)
	legacyRawHistory(t, db, legacyRows, base.Add(-2*time.Hour))
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	requireProofOddIndexMatches(t, db)
	clearProofScanCacheForTest()
	// The legacy rows carry a parseable key stamp and no raw owner, so they are
	// hour-located odd rows and not part of the global set warm-up visits.
	if visited, err := WarmUsageProofScanCache(db); err != nil || visited != 0 {
		t.Fatalf("warm-up visited=%d err=%v, want 0 hour-located odd rows (test premise)", visited, err)
	}
	event := fixedPlainEvent()
	event.ClientName = "live-client"
	event.RecordID = "40000000-0000-4000-8000-000000000001"
	event.Timestamp = base // an hour no legacy row occupies
	before := proofRawParses.Load()
	added, err := store.RecordOnce(event)
	t.Logf("RecordOnce after warm-up: added=%v err=%v", added, err)
	if delta := proofRawParses.Load() - before; delta >= legacyRows/10 {
		t.Fatalf("first RecordOnce after warm-up parsed %d raw rows; the %d hour-located legacy rows must stay out of the proof scope", delta, legacyRows)
	}
}

// TestUsageProofIndexMetricsReportsReadinessAndCounters pins the operator-facing
// view of Issue #114: a fresh database has no trusted index, a completed
// migration makes it ready, a reset puts proofs back on the fallback, and the
// exported counters move with the fallback scan and index loads.
func TestUsageProofIndexMetricsReportsReadinessAndCounters(t *testing.T) {
	db := testDB(t)
	if got := UsageProofIndexStats(db); got.Ready {
		t.Fatalf("fresh database reports a ready proof index: %+v", got)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("metrics-weird-1"), []byte(`{"client_name":"c01"}`))
	}); err != nil {
		t.Fatalf("seed odd row: %v", err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 0); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	before := UsageProofIndexStats(db)
	if !before.Ready {
		t.Fatalf("completed migration reports no ready proof index: %+v", before)
	}

	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		keys, ok := proofOddLoadGlobal(tx, proofOddRawGlobal, raw)
		if !ok || len(keys) != 1 {
			t.Fatalf("load global odd keys: ok=%v keys=%d, want the seeded row", ok, len(keys))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := UsageProofIndexStats(db); after.IndexLoads <= before.IndexLoads {
		t.Fatalf("index loads = %d, want more than %d after loading the index", after.IndexLoads, before.IndexLoads)
	}

	if err := db.View(func(tx *bolt.Tx) error {
		summary := &txScopeSummary{}
		keys := summary.fallbackRaw(tx.Bucket([]byte(BucketUsageEvents)))
		if len(keys) != 1 {
			t.Fatalf("fallback classified %d odd keys, want the seeded row", len(keys))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := UsageProofIndexStats(db); after.FullClassifications <= before.FullClassifications {
		t.Fatalf("full classifications = %d, want more than %d after a fallback scan", after.FullClassifications, before.FullClassifications)
	}

	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := UsageProofIndexStats(db); got.Ready {
		t.Fatalf("reset database still reports a ready proof index: %+v", got)
	}
}

// TestUsageProofIndexMetricsWithoutDatabase pins the nil paths: no store and
// no database report counters but never a ready index.
func TestUsageProofIndexMetricsWithoutDatabase(t *testing.T) {
	var store *UsageStore
	if got := store.UsageProofIndexStats(); got.Ready {
		t.Fatalf("nil store reports a ready proof index: %+v", got)
	}
	if got := UsageProofIndexStats(nil); got.Ready {
		t.Fatalf("nil database reports a ready proof index: %+v", got)
	}
}
