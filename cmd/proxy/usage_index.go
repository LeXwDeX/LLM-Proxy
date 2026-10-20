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
	}
}
