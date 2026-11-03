package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestIntegrityClassificationStallMetricsJSON(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	data, err := json.Marshal(UsageIntegrityStats(db))
	if err != nil {
		t.Fatal(err)
	}
	var metrics map[string]json.RawMessage
	if err := json.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"classification_stalled", "discarded_rounds", "last_discard_reason", "segment_max_tx_ms", "writer_lock_max_ms"} {
		if _, ok := metrics[key]; !ok {
			t.Errorf("missing classifier metric %q", key)
		}
	}
}

func singleObjectSegments(t *testing.T) {
	t.Helper()
	budget, duration, hook := integrityClassifySegmentBudget, integrityClassifySegmentDuration, integrityClassifySegmentBoundary
	integrityClassifySegmentBudget = 1
	integrityClassifySegmentDuration = time.Hour
	integrityClassifySegmentBoundary = nil
	t.Cleanup(func() {
		integrityClassifySegmentBudget, integrityClassifySegmentDuration, integrityClassifySegmentBoundary = budget, duration, hook
	})
}

func TestIntegrityClassificationSegmentsMatchFull(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, _, base := integrityFixture(t)
	segments := 0
	integrityClassifySegmentBoundary = func() { segments++ }
	classifyNow(t, db)
	if segments < 2 {
		t.Fatalf("only %d segments ran", segments)
	}
	integrityState.Lock()
	class := integrityState.classes[db]
	integrityState.Unlock()
	for _, q := range []integrityQuery{
		{client: base.ClientName, from: base.Timestamp.Add(-time.Hour), to: base.Timestamp.Add(time.Hour), windowKeys: true, exact: true},
		{from: base.Timestamp.Add(-4 * time.Hour), to: base.Timestamp.Add(time.Hour), exact: true},
	} {
		if !compareInstalledWithFull(t, db, q, class, nil) {
			t.Fatal("segmented classification did not use Fast")
		}
	}
}

// Compare all accumulated sets and counts with the original single-snapshot
// entry point, including malformed objects, nested buckets and the proof prefix.
func TestIntegrityClassificationSegmentsMatchSingleSnapshot(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	for seed := int64(0); seed < 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			f := buildIntegrityFuzzDB(t, rand.New(rand.NewSource(seed)))
			t.Cleanup(func() { ForgetUsageIntegrityClassification(f.db) })
			if err := f.db.Update(func(tx *bolt.Tx) error {
				for _, name := range []string{rawOwnerBucket, BucketUsageEvents, BucketUsageReceipts, freshAttestationBucket, BucketQuotaUsageAggHourly, BucketUsageEventsByClient} {
					bucket := tx.Bucket([]byte(name))
					if bucket == nil {
						continue
					}
					if err := bucket.Put([]byte("odd-segment-key"), []byte("bad")); err != nil {
						return err
					}
					if _, err := bucket.CreateBucket([]byte("nested-segment-key")); err != nil {
						return err
					}
				}
				if proofs, err := rollupProofSource(tx); err == nil && proofs != nil {
					return proofs.Put(append(append([]byte(BucketQuotaUsageAggHourly), 0), []byte("odd-segment-key")...), []byte("bad"))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var single *integrityClassification
			if err := f.db.View(func(tx *bolt.Tx) error {
				var reason string
				single, reason = classifyUsageIntegrity(context.Background(), tx)
				if single == nil {
					t.Fatalf("single snapshot: %s", reason)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			classifyNow(t, f.db)
			integrityState.Lock()
			segmented := *integrityState.classes[f.db]
			integrityState.Unlock()
			segmented.epoch = single.epoch
			segmented.duration = single.duration
			if !reflect.DeepEqual(&segmented, single) {
				t.Fatalf("segmented classification differs from single snapshot: anomaly counts %d and %d", segmented.anomalous, single.anomalous)
			}
		})
	}
}

func TestIntegrityClassificationSegmentEpochChange(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, _, _ := integrityFixture(t)
	classifyNow(t, db)
	before := UsageIntegrityStats(db)
	calls := 0
	integrityClassifySegmentBoundary = func() {
		calls++
		if calls == 1 {
			if err := db.Update(func(tx *bolt.Tx) error {
				integrityMarkTx(tx)
				return tx.Bucket([]byte(BucketMeta)).Put([]byte("segment-test"), []byte("changed"))
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	after := UsageIntegrityStats(db)
	if calls != 1 || after.Complete || after.LastDiscardReason != integrityFallbackEpochChanged || after.FallbackReads[integrityFallbackEpochChanged] != before.FallbackReads[integrityFallbackEpochChanged]+1 {
		t.Fatalf("epoch change did not discard round: calls=%d before=%+v after=%+v", calls, before, after)
	}
	integrityState.Lock()
	class := integrityState.classes[db]
	integrityState.Unlock()
	if class != nil {
		t.Fatal("epoch change left a classification installed")
	}
}

func TestIntegrityClassificationSegmentIndexProgressChange(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, _, _ := integrityFixture(t)
	classifyNow(t, db)
	before := UsageIntegrityStats(db)
	epoch := integrityEpoch.Load()
	calls := 0
	integrityClassifySegmentBoundary = func() {
		calls++
		if calls != 1 {
			return
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			p, ok := classificationIndexProgress(tx)
			if !ok || p == nil {
				return errors.New("fixture has no index progress")
			}
			p.Through = nil
			p.Generation = "00000000000000000000000000000000"
			if err := saveIndexProgress(tx, p); err != nil {
				return err
			}
			changed, ok := classificationIndexProgress(tx)
			if !ok || changed == nil || !sameIndexProgress(changed, p) {
				return errors.New("updated progress is not valid")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	after := UsageIntegrityStats(db)
	if integrityEpoch.Load() != epoch || calls != 1 || after.Complete || after.LastDiscardReason != integrityFallbackIndexProgressChanged || after.FallbackReads[integrityFallbackIndexProgressChanged] != before.FallbackReads[integrityFallbackIndexProgressChanged]+1 {
		t.Fatalf("index change did not discard round: calls=%d before=%+v after=%+v", calls, before, after)
	}
}

func TestIntegrityClassificationSegmentsCancelKeepsPreviousClass(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, _, _ := integrityFixture(t)
	classifyNow(t, db)
	integrityState.Lock()
	old := integrityState.classes[db]
	integrityState.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	integrityClassifySegmentBoundary = cancel
	if err := RunClassificationOnce(ctx, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled round: %v", err)
	}
	integrityState.Lock()
	got := integrityState.classes[db]
	integrityState.Unlock()
	if got != old {
		t.Fatal("cancellation replaced or removed the previous class")
	}
}

func TestIntegrityClassificationDiscardStallAndBackoff(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	ForgetUsageIntegrityClassification(db)
	integrityState.Lock()
	integrityState.discardedRounds = 0
	integrityState.lastDiscardReason = ""
	integrityState.Unlock()

	// A non-consistency failure (incomplete) must not be labelled as stalled.
	attempt := classificationAttempt{reason: integrityFallbackIncomplete}
	recordClassificationAttempt(context.Background(), db, &attempt)
	if stats := UsageIntegrityStats(db); stats.DiscardedRounds != 0 || stats.ClassificationStalled {
		t.Fatalf("incomplete counted as stall: %+v", stats)
	}

	for i, wantDelay := range []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second} {
		attempt = classificationAttempt{reason: integrityFallbackEpochChanged}
		recordClassificationAttempt(context.Background(), db, &attempt)
		stats := UsageIntegrityStats(db)
		if stats.DiscardedRounds != uint64(i+1) || stats.ClassificationStalled != (i >= 2) || stats.LastDiscardReason != integrityFallbackEpochChanged {
			t.Fatalf("consistency discard %d: %+v", i+1, stats)
		}
		if got := classificationDiscardDelay(stats.DiscardedRounds, time.Minute); got != wantDelay {
			t.Fatalf("discard %d delay: %s, want %s", i+1, got, wantDelay)
		}
	}
	if got := classificationDiscardDelay(4, time.Minute); got != time.Minute {
		t.Fatalf("discard 4 delay=%s, want interval", got)
	}
	if got := classificationDiscardDelay(1, time.Second); got != time.Second {
		t.Fatalf("delay exceeded interval: %s", got)
	}

	// Index progress changes are the other consistency discard reason.
	attempt = classificationAttempt{reason: integrityFallbackIndexProgressChanged}
	recordClassificationAttempt(context.Background(), db, &attempt)
	if stats := UsageIntegrityStats(db); stats.DiscardedRounds != 4 || !stats.ClassificationStalled || stats.LastDiscardReason != integrityFallbackIndexProgressChanged {
		t.Fatalf("index progress discard: %+v", stats)
	}

	classifyNow(t, db)
	stats := UsageIntegrityStats(db)
	if stats.DiscardedRounds != 0 || stats.ClassificationStalled || stats.LastDiscardReason != "" {
		t.Fatalf("successful round did not clear stall: %+v", stats)
	}
}

func TestIntegrityClassificationSegmentCursor(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	var build *classificationBuild
	if err := db.View(func(tx *bolt.Tx) error {
		var reason string
		build, reason = newClassificationBuild(tx)
		if build == nil {
			t.Fatalf("build: %s", reason)
		}
		build.runSegment(context.Background(), tx, 1, 0)
		if build.visited != 1 || build.stage != classifyRawOwner || len(build.cursor) == 0 {
			t.Fatalf("first segment did not save next key: stage=%d visited=%d", build.stage, build.visited)
		}
		cursor := bytes.Clone(build.cursor)
		c := tx.Bucket([]byte(rawOwnerBucket)).Cursor()
		_, _ = c.First()
		next, _ := c.Next()
		if !bytes.Equal(cursor, next) {
			t.Fatal("cursor is not the next unprocessed key")
		}
		build.runSegment(context.Background(), tx, 1, 0)
		if build.visited != 2 || build.stage == classifyRawOwner {
			t.Fatalf("second segment skipped or repeated a key: stage=%d visited=%d", build.stage, build.visited)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityClassificationSegmentDuration(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	if err := db.View(func(tx *bolt.Tx) error {
		build, reason := newClassificationBuild(tx)
		if build == nil {
			t.Fatalf("build: %s", reason)
		}
		build.runSegment(context.Background(), tx, 0, time.Nanosecond)
		if build.visited != 1 || len(build.cursor) == 0 {
			t.Fatalf("duration limit did not stop after one object: visited=%d", build.visited)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityClassificationSegmentRealtimeWrite(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, store, base := integrityFixture(t)
	epoch := integrityEpoch.Load()
	var oldID int
	if err := db.View(func(tx *bolt.Tx) error { oldID = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	calls := 0
	integrityClassifySegmentBoundary = func() {
		calls++
		if calls == 1 {
			event := base
			event.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa91"
			event.Timestamp = base.Timestamp.Add(time.Minute)
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	classifyNow(t, db)
	integrityState.Lock()
	class := integrityState.classes[db]
	integrityState.Unlock()
	if integrityEpoch.Load() != epoch || class.txid <= oldID {
		t.Fatalf("realtime write changed epoch or last segment txid: old=%d class=%d", oldID, class.txid)
	}
	q := integrityQuery{client: base.ClientName, from: base.Timestamp.Add(-time.Hour), to: base.Timestamp.Add(time.Hour), windowKeys: true, exact: true}
	if !compareInstalledWithFull(t, db, q, class, nil) {
		t.Fatal("realtime write prevented segmented Fast read")
	}
}

func TestIntegrityClassificationFinalSnapshotValidation(t *testing.T) {
	for _, changeProgress := range []bool{false, true} {
		t.Run(fmt.Sprintf("progress=%t", changeProgress), func(t *testing.T) {
			productionIntegrityMode(t)
			singleObjectSegments(t)
			db, _, _ := integrityFixture(t)
			segments := 0
			integrityClassifySegmentBoundary = func() { segments++ }
			classifyNow(t, db)
			last := segments
			segments = 0
			var lastSegmentID int
			integrityClassifySegmentBoundary = func() {
				segments++
				if segments != last {
					return
				}
				if err := db.View(func(tx *bolt.Tx) error { lastSegmentID = tx.ID(); return nil }); err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					meta := tx.Bucket([]byte(BucketMeta))
					if changeProgress {
						return meta.Put([]byte(indexProgressV2), []byte("invalid-progress"))
					}
					return meta.Put([]byte("segment-test"), []byte("newer-snapshot"))
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := UsageIntegrityStats(db)
			if err := RunClassificationOnce(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			if segments != last || lastSegmentID == 0 {
				t.Fatalf("did not reach last segment: %d/%d", segments, last)
			}
			stats := UsageIntegrityStats(db)
			if changeProgress {
				if stats.Complete || stats.FallbackReads[integrityFallbackIndexProgressChanged] != before.FallbackReads[integrityFallbackIndexProgressChanged]+1 {
					t.Fatalf("final progress change was not discarded: %+v", stats)
				}
				return
			}
			integrityState.Lock()
			class := integrityState.classes[db]
			integrityState.Unlock()
			if class == nil {
				t.Fatal("classification was not installed")
			}
			if class.txid != lastSegmentID {
				t.Fatalf("class txid %d is not last segment txid %d", class.txid, lastSegmentID)
			}
		})
	}
}

func TestIntegrityClassificationSegmentDurations(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, _, _ := integrityFixture(t)
	classifyNow(t, db)
	stats := UsageIntegrityStats(db)
	if stats.WriterLockMaxMS <= 0 {
		t.Fatalf("writer_lock_max_ms=%v, want >0", stats.WriterLockMaxMS)
	}
	if stats.SegmentMaxTxMS < 0 {
		t.Fatalf("segment_max_tx_ms=%v, want >=0", stats.SegmentMaxTxMS)
	}
}

func TestIntegrityClassificationStallLoopRecovers(t *testing.T) {
	productionIntegrityMode(t)
	singleObjectSegments(t)
	db, _, _ := integrityFixture(t)
	ForgetUsageIntegrityClassification(db)
	integrityState.Lock()
	integrityState.discardedRounds = 0
	integrityState.lastDiscardReason = ""
	integrityState.Unlock()

	var mutate atomic.Bool
	mutate.Store(true)
	integrityClassifySegmentBoundary = func() {
		if !mutate.Load() {
			return
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			integrityMarkTx(tx)
			return tx.Bucket([]byte(BucketMeta)).Put([]byte("segment-loop-test"), []byte("changed"))
		}); err != nil {
			t.Errorf("mutate epoch: %v", err)
		}
	}
	stop := StartUsageIntegrityClassification(context.Background(), db, 20*time.Millisecond, nil)
	defer stop()

	deadline := time.Now().Add(5 * time.Second)
	for {
		stats := UsageIntegrityStats(db)
		if stats.ClassificationStalled && stats.DiscardedRounds >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("classifier did not stall: %+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mutate.Store(false)
	for {
		if UsageIntegrityStats(db).Complete {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("classifier did not recover after stall: %+v", UsageIntegrityStats(db))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
