package nosql

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #252: a maintenance write invalidates the installed classification. With
// EarlyRebuild the loop notices the changed epoch, waits for a quiet period and
// rebuilds at once instead of waiting for the next periodic round.

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
	time.Sleep(300 * time.Millisecond)
	if UsageIntegrityStats(db).Complete {
		t.Fatal("rebuilt early although EarlyRebuild is off")
	}
}

// While a maintenance write is in flight the loop must not even attempt a
// build: an attempt would count `incomplete` and reset the discard ladder.
func TestEarlyRebuildWaitsForInflightWriters(t *testing.T) {
	productionIntegrityMode(t)
	db, _, _ := integrityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartUsageIntegrityClassificationWith(ctx, db, earlyOptions(), nil)
	defer stop()
	if !WaitUsageIntegrityReady(ctx, db, 5*time.Second) {
		t.Fatal("first build did not complete")
	}
	end := integrityMutationBegin()
	before := UsageIntegrityStats(db).FallbackReads[integrityFallbackIncomplete]
	time.Sleep(300 * time.Millisecond)
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
	time.Sleep(100 * time.Millisecond)
	if UsageIntegrityStats(db).Complete {
		t.Fatal("built during an in-flight maintenance write")
	}
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
	time.Sleep(300 * time.Millisecond)
	if UsageIntegrityStats(db).Complete || builds.Load() != before {
		t.Fatalf("early rebuild exceeded its budget (complete=%v builds %d -> %d)", UsageIntegrityStats(db).Complete, before, builds.Load())
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
	time.Sleep(400 * time.Millisecond)
	stats := UsageIntegrityStats(db)
	if stats.Complete || stats.DiscardedRounds != 1 {
		t.Fatalf("expected one discard and no early retry: %+v", stats)
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
