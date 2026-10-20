package main

import (
	"log/slog"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// persistenceDrainTimeout bounds the shutdown drain. Every acknowledged usage
// event is already fsynced to the recovery journal, so giving up only defers
// its bbolt commit to the next start's Replay; statistics still queued are
// lost (they are non-billing).
const persistenceDrainTimeout = 30 * time.Second

// usagePersistence owns the background writers that keep bbolt commits off
// the response path:
//   - usage: the recovery journal in write-ahead mode. Record returns after
//     the event is synced to the journal (and, through quota.RecordUsage, after
//     the in-memory quota counter is charged); bbolt commits are batched in
//     the background and a crash before them is recovered by the startup
//     Replay, idempotently by RecordID.
//   - request completion statistics: a bounded queue; a full queue drops the
//     completion and reports it through record_failures.
type usagePersistence struct {
	journal      *nosql.UsageJournal
	stats        *nosql.AsyncRequestStatsRecorder
	logger       *slog.Logger
	drainTimeout time.Duration
}

// startUsagePersistence must run after the startup journal Replay so that
// leftovers from a previous process are already committed.
func startUsagePersistence(journal *nosql.UsageJournal, statsStore *nosql.RequestStatsStore, logger *slog.Logger) (*usagePersistence, error) {
	if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{}); err != nil {
		return nil, err
	}
	return &usagePersistence{
		journal:      journal,
		stats:        nosql.NewAsyncRequestStatsRecorder(statsStore, nosql.AsyncRequestStatsOptions{}, logger),
		logger:       logger,
		drainTimeout: persistenceDrainTimeout,
	}, nil
}

// drain runs once request handlers and hijacked WebSocket turns have
// finished and before the database closes: it commits every queued usage
// event and statistic, then replays whatever is still pending in the journal
// (failed commits, events deferred by a full queue, leftovers). It gives up
// after drainTimeout so a stalled database cannot hold the shutdown forever.
func (p *usagePersistence) drain(onRecovered func([]string)) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.drainQueues(onRecovered)
	}()
	timer := time.NewTimer(p.drainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		p.logger.Error("usage persistence drain timed out; acknowledged usage stays in the recovery journal and is replayed on the next start",
			"timeout", p.drainTimeout.String(),
			"usage_pending", p.journal.AsyncPending(),
		)
	}
}

func (p *usagePersistence) drainQueues(onRecovered func([]string)) {
	if err := p.journal.Close(); err != nil {
		p.logger.Warn("usage writer drain incomplete; pending events stay in the recovery journal", "error", err)
	}
	if err := p.stats.Close(); err != nil {
		p.logger.Warn("request stats writer drain failed", "error", err)
	}
	recoveredClients, err := p.journal.Replay()
	if err != nil {
		p.logger.Warn("final usage journal replay failed during shutdown", "error", err)
		return
	}
	if len(recoveredClients) > 0 {
		p.logger.Info("recovered pending usage events during shutdown", "clients", len(recoveredClients))
		if onRecovered != nil {
			onRecovered(recoveredClients)
		}
	}
}
