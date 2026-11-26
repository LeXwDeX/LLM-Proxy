package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Derived indexing never gates HTTP readiness. The returned stop function
// cancels and joins this task before the database is closed. The process signal
// context also interrupts the task, including its bounded verification pass.
func startUsageClientIndexMigration(parent context.Context, db *bolt.DB, logger *slog.Logger, migrate func(context.Context, *bolt.DB, int) error) func() {
	stop, _ := startUsageClientIndexMigrationWithDone(parent, db, logger, migrate)
	return stop
}

// startUsageClientIndexMigrationWithDone also returns a channel that closes when
// the migration goroutine exits (complete, interrupted or stopped). Startup uses
// it to run the integrity classification only after the migration's maintenance
// write ended: the two invalidate each other when they overlap.
func startUsageClientIndexMigrationWithDone(parent context.Context, db *bolt.DB, logger *slog.Logger, migrate func(context.Context, *bolt.DB, int) error) (stop func(), finished <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		started := time.Now()
		backoff := time.Second
		for {
			if err := migrate(ctx, db, 0); err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					logger.Info("usage client index migration interrupted; retry on next startup", "duration", time.Since(started).String())
					return
				}
				logger.Error("usage client index migration incomplete; legacy reads remain active; retry with backoff", "error", err, "retry_in", backoff.String(), "duration", time.Since(started).String())
				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				backoff *= 2
				if backoff > time.Minute {
					backoff = time.Minute
				}
				continue
			}
			logger.Info("usage client index migration complete", "duration", time.Since(started).String())
			return
		}
	}()
	return func() {
		cancel()
		<-done
	}, done
}

// usageFastPathBounds bound how long startup may delay the HTTP listener and the
// quota preload to install the integrity classification. Past a bound startup
// continues degraded: calibration reads stay exhaustive until the classification
// loop completes a build, exactly as before this step existed.
type usageFastPathBounds struct {
	MaintenanceWait time.Duration // wait for the startup maintenance writes (index migration, first retention round)
	ClassifyWait    time.Duration // observe the classification build; 0 = do not wait
}

const defaultMaintenanceWait = 5 * time.Second

// usageFastPathBoundsFor builds the startup bounds; the classification wait comes
// from USAGE_INTEGRITY_STARTUP_WAIT_SECONDS.
func usageFastPathBoundsFor(classifyWait time.Duration) usageFastPathBounds {
	return usageFastPathBounds{MaintenanceWait: defaultMaintenanceWait, ClassifyWait: classifyWait}
}

// joinDone returns a channel that closes once every input channel is closed.
func joinDone(chans ...<-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		defer close(out)
		for _, c := range chans {
			<-c
		}
	}()
	return out
}

// prepareUsageFastPath runs before the quota preload. Without an installed
// classification every client calibration rescans the whole history, which made
// the first requests after a restart hit the 15s calibration timeout (503).
//
// Order matters: the startup maintenance writes (index migration, first
// retention round) and the classification invalidate each other when they
// overlap, so wait for them first (bounded). Then start the classification loop
// and only observe it (bounded): the build is never canceled by the startup wait,
// so a build that outlasts it keeps its progress and completes in the background
// (Issue #252; an earlier synchronous build was discarded on timeout and rebuilt
// from scratch). If the maintenance writes did not finish in time the loop is
// still started (its early rebuild builds right after they ended) but nothing
// waits for it. It reports whether the classification is installed.
func prepareUsageFastPath(ctx context.Context, db *bolt.DB, maintenanceDone <-chan struct{}, logger *slog.Logger, bounds usageFastPathBounds, start func(), waitReady func(context.Context, *bolt.DB, time.Duration) bool) bool {
	began := time.Now()
	wait := time.NewTimer(bounds.MaintenanceWait)
	maintenanceFinished := true
	select {
	case <-maintenanceDone:
	case <-wait.C:
		maintenanceFinished = false
		logger.Warn("usage maintenance writes still running; classification starts without being awaited", "waited", bounds.MaintenanceWait.String())
	case <-ctx.Done():
		wait.Stop()
		return false
	}
	wait.Stop()
	start()
	if !maintenanceFinished || bounds.ClassifyWait <= 0 {
		return false
	}
	if !waitReady(ctx, db, bounds.ClassifyWait) {
		if ctx.Err() == nil {
			logger.Warn("usage integrity classification not ready before quota preload; calibration reads stay exhaustive and the build continues in the background", "waited", time.Since(began).String(), "classify_wait", bounds.ClassifyWait.String())
		}
		return false
	}
	logger.Info("usage integrity classification installed before quota preload", "elapsed", time.Since(began).String())
	return true
}
