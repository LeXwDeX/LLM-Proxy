package nosql

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Issue #252: a maintenance write invalidates the installed classification. With
// EarlyRebuild the loop notices the changed epoch, waits for a quiet period and
// rebuilds at once instead of waiting for the next periodic round.

// assertStaysIncomplete fails at the first moment the classification is
// installed within window; it never asserts on a fixed sleep's outcome alone.
func assertStaysIncomplete(t *testing.T, db *bolt.DB, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if UsageIntegrityStats(db).Complete {
			t.Fatal("classification was rebuilt although it must not be")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// countEarlyPolls counts the loop's epoch polls. A negative assertion about an
// early rebuild waits for enough polls (each at least EarlyPoll long) that the
// quiet period has provably elapsed, instead of sleeping for a guessed time.
func countEarlyPolls(t *testing.T) *atomic.Int64 {
	t.Helper()
	var polls atomic.Int64
	integrityEarlyPollHook = func() { polls.Add(1) }
	t.Cleanup(func() { integrityEarlyPollHook = nil })
	return &polls
}

func waitForPolls(t *testing.T, polls *atomic.Int64, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for polls.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("loop made %d polls, want %d", polls.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func earlyOptions() ClassificationLoopOptions {
	return ClassificationLoopOptions{Interval: time.Hour, EarlyRebuild: true, EarlyQuiet: 30 * time.Millisecond, EarlyPoll: 5 * time.Millisecond, EarlyBudget: 2}
}

func TestEarlyRebuildAfterMaintenanceWrite(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassificationWith(ctx, db, earlyOptions(), nil)
	defer stop()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("first build did not complete")
	}
	integrityMutationBegin()()
	if UsageIntegrityStats(db).Complete {
		t.Fatal("maintenance write must invalidate the classification")
	}
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("classification was not rebuilt after the maintenance write ended")
	}
}

// The default entry point keeps the periodic-only behavior.
func TestNoEarlyRebuildWithoutOption(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassification(ctx, db, time.Hour, nil)
	defer stop()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("first build did not complete")
	}
	integrityMutationBegin()()
	assertStaysIncomplete(t, db, 300*time.Millisecond)
}

// While a maintenance write is in flight the loop must not even attempt a
// build: an attempt would count `incomplete` and reset the discard ladder.
func TestEarlyRebuildWaitsForInflightWriters(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	// Installed before the loop starts: the hook variable is read by its goroutine.
	polls := countEarlyPolls(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassificationWith(ctx, db, earlyOptions(), nil)
	defer stop()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("first build did not complete")
	}
	end := integrityMutationBegin()
	before := UsageIntegrityStats(db).FallbackReads[integrityFallbackIncomplete]
	// 24 polls of >= 5ms each outlast the 30ms quiet period several times over.
	start := polls.Load()
	waitForPolls(t, polls, start+24)
	if got := UsageIntegrityStats(db).FallbackReads[integrityFallbackIncomplete]; got != before {
		t.Fatalf("attempted builds during an in-flight write: incomplete %d -> %d", before, got)
	}
	end()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("classification was not rebuilt after the write ended")
	}
}

// Startup: the first attempt can meet a maintenance write that is still in
// flight (it returns `incomplete`). Without early rebuild the loop then idles a
// whole interval; with it the build starts once the write ended.
func TestEarlyRebuildFirstAttemptDuringMaintenance(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	end := integrityMutationBegin()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassificationWith(ctx, db, earlyOptions(), nil)
	defer stop()
	assertStaysIncomplete(t, db, 100*time.Millisecond)
	end()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("first build did not start after the maintenance write ended")
	}
}

// A steady stream of invalidations must not turn into a rebuild storm: at most
// EarlyBudget early rebuilds per periodic window.
func TestEarlyRebuildBudgetBoundsStorms(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	var builds atomic.Int64
	integrityClassifySegmentBoundary = func() { builds.Add(1) }
	t.Cleanup(func() { integrityClassifySegmentBoundary = nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassificationWith(ctx, db, earlyOptions(), nil)
	defer stop()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("first build did not complete")
	}
	for i := 0; i < 2; i++ {
		integrityMutationBegin()()
		if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
			t.Fatalf("early rebuild %d did not happen", i+1)
		}
	}
	before := builds.Load()
	integrityMutationBegin()()
	assertStaysIncomplete(t, db, 300*time.Millisecond)
	if builds.Load() != before {
		t.Fatalf("early rebuild exceeded its budget (builds %d -> %d)", before, builds.Load())
	}
}

// A consistency discard puts the loop on the 5s/15s/45s ladder; early rebuild
// must not shorten it.
func TestEarlyRebuildDoesNotShortenDiscardLadder(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	var once atomic.Bool
	integrityClassifySegmentBoundary = func() {
		if once.CompareAndSwap(false, true) {
			integrityMutationBegin()()
		}
	}
	t.Cleanup(func() { integrityClassifySegmentBoundary = nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassificationWith(ctx, db, earlyOptions(), nil)
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	for UsageIntegrityStats(db).DiscardedRounds != 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if UsageIntegrityStats(db).DiscardedRounds != 1 {
		t.Fatalf("the forced discard never happened: %+v", UsageIntegrityStats(db))
	}
	assertStaysIncomplete(t, db, 400*time.Millisecond)
	if stats := UsageIntegrityStats(db); stats.DiscardedRounds != 1 {
		t.Fatalf("expected no early retry inside the ladder wait: %+v", stats)
	}
}

func TestEarlyRebuildLoopStopsWhileWaiting(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	stop := StartUsageIntegrityClassificationWith(context.Background(), db, earlyOptions(), nil)
	if !WaitUsageIntegrityReady(context.Background(), db, 5*time.Second) {
		t.Fatal("first build did not complete")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return while the loop was waiting")
	}
}

// The early-rebuild budget is per periodic window: waiting out a (shorter)
// discard-ladder delay must not refill it.
func TestEarlyRebuildBudgetResetsOnlyOnPeriodicDeadline(t *testing.T) {
	ctx := context.Background()
	streak := 2
	if !waitNextClassification(ctx, 5*time.Millisecond, false, false, 0, time.Second, time.Millisecond, 2, &streak) || streak != 2 {
		t.Fatalf("a ladder wait refilled the early-rebuild budget (streak=%d)", streak)
	}
	if !waitNextClassification(ctx, 5*time.Millisecond, true, false, 0, time.Second, time.Millisecond, 2, &streak) || streak != 0 {
		t.Fatalf("a periodic deadline must refill the budget (streak=%d)", streak)
	}
}

// After a consistency discard the loop is on the 5s/15s/45s ladder; once the
// ladder ends its delay equals the interval, and early rebuild must still stay
// off for that attempt (the epoch is evidently churning).
func TestEarlyRebuildNotAllowedAfterConsistencyDiscard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attempt classificationAttempt
		want    bool
	}{
		{"epoch changed discard", classificationAttempt{reason: integrityFallbackEpochChanged}, false},
		{"index progress changed discard", classificationAttempt{reason: integrityFallbackIndexProgressChanged}, false},
		{"incomplete (first attempt met a writer)", classificationAttempt{reason: integrityFallbackIncomplete}, true},
		{"overflow", classificationAttempt{reason: integrityFallbackOverflow}, true},
		{"installed", classificationAttempt{installed: true}, true},
		{"canceled or failed", classificationAttempt{err: context.Canceled, reason: integrityFallbackEpochChanged}, true},
	} {
		if got := earlyRebuildAllowed(&tc.attempt); got != tc.want {
			t.Errorf("%s: earlyRebuildAllowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
