package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
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
// continues degraded: calibration reads stay exhaustive until the background
// classification loop completes a build, exactly as before this step existed.
type usageFastPathBounds struct {
	IndexWait    time.Duration // wait for the client-index migration to end
	ClassifyWait time.Duration // synchronous classification build
}

var defaultUsageFastPathBounds = usageFastPathBounds{IndexWait: 5 * time.Second, ClassifyWait: 20 * time.Second}

// prepareUsageFastPath runs before the quota preload. Without an installed
// classification every client calibration rescans the whole history, which made
// the first requests after a restart hit the 15s calibration timeout (503).
// Order matters: the index migration and the classification invalidate each
// other when they overlap, so wait for the migration first (bounded), then build
// the classification synchronously (bounded). It reports whether the
// classification is installed.
func prepareUsageFastPath(ctx context.Context, db *bolt.DB, indexDone <-chan struct{}, logger *slog.Logger, bounds usageFastPathBounds, classify func(context.Context, *bolt.DB) error) bool {
	started := time.Now()
	wait := time.NewTimer(bounds.IndexWait)
	select {
	case <-indexDone:
	case <-wait.C:
		logger.Warn("usage client index migration still running; building the integrity classification anyway", "waited", bounds.IndexWait.String())
	case <-ctx.Done():
		wait.Stop()
		return false
	}
	wait.Stop()
	buildCtx, cancel := context.WithTimeout(ctx, bounds.ClassifyWait)
	defer cancel()
	if err := classify(buildCtx, db); err != nil && ctx.Err() == nil {
		logger.Warn("usage integrity classification not built before quota preload; calibration reads stay exhaustive until the background build completes", "error", err, "waited", time.Since(started).String())
	}
	installed := nosql.UsageIntegrityStats(db).Complete
	if installed {
		logger.Info("usage integrity classification installed before quota preload", "elapsed", time.Since(started).String())
	}
	return installed
}
