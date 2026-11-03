package nosql

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Fallback reasons, counted separately (no keys or bytes are ever exported).
const (
	integrityFallbackIncomplete           = "incomplete"
	integrityFallbackEpochChanged         = "epoch_changed"
	integrityFallbackIndexProgressChanged = "index_progress_changed"
	integrityFallbackOverflow             = "anomalous_overflow"
	integrityFallbackPartialRefs          = "partial_refs_fallback"
	integrityFallbackClientScope          = "client_scope_fallback"
)

// integrityForceFast is set only by this package's tests (see
// usage_integrity_force_test.go): every integrity read then classifies its own
// read transaction and takes the fast path. Production code never sets it.
var integrityForceFast atomic.Bool

// integrityClassifyStep, when set by tests, runs at every cancellation check
// of a classification build.
var integrityClassifyStep func()

// integrityClassifySegmentBoundary is called after closing each segment, in tests only.
var integrityClassifySegmentBoundary func()

// Maintenance epoch. Multi-transaction maintenance writes run between
// integrityMutationBegin and its end function: both bump the epoch and the
// in-flight counter covers the window in which a commit may be visible before
// the second bump. Single-transaction changes (ReindexUsageEvent, anomalous
// realtime rows, non-canonical attestations) bump it inside the transaction
// with integrityMarkTx. A classification is only built while nothing is in
// flight, from a snapshot taken under the bbolt writer lock, and is only used
// while the epoch is unchanged and the reader's snapshot is not older.
var (
	integrityEpoch    atomic.Uint64
	integrityInflight atomic.Int64
)

var integrityState = struct {
	sync.Mutex
	classes           map[*bolt.DB]*integrityClassification
	lastDuration      time.Duration
	lastAnomaly       int
	fastReads         uint64
	fallbacks         map[string]uint64
	discardedRounds   uint64
	lastDiscardReason string
	lastSegmentMax    time.Duration
	lastWriterLockMax time.Duration
}{classes: make(map[*bolt.DB]*integrityClassification), fallbacks: make(map[string]uint64)}

// integrityMutationBegin marks the start of a non-realtime write; call the
// returned function after the write transaction ended (commit or rollback).
func integrityMutationBegin() func() {
	integrityInflight.Add(1)
	integrityEpoch.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			integrityEpoch.Add(1)
			integrityInflight.Add(-1)
		})
	}
}

// integrityMarkTx records, inside a write transaction, that the transaction
// creates or changes a classified object outside the realtime contract. The
// epoch bump happens before the commit, so a reader that can see the change
// sees the new epoch. No end mark is needed: a classification only takes its
// snapshot while it holds the bbolt writer lock (see RunClassificationOnce),
// so no marked transaction is in progress at snapshot time. Whether the
// caller commits or rolls back, nothing is left in flight.
func integrityMarkTx(_ *bolt.Tx) {
	integrityEpoch.Add(1)
}

func integrityNoteFallback(reason string) {
	if integrityForceFast.Load() {
		return
	}
	integrityState.Lock()
	integrityState.fallbacks[reason]++
	integrityState.Unlock()
}

// integrityClassFor returns the classification a read in tx may use, or nil.
func integrityClassFor(tx *bolt.Tx) *integrityClassification {
	if integrityForceFast.Load() {
		class, _ := classifyUsageIntegrity(context.Background(), tx)
		return class
	}
	db := tx.DB()
	integrityState.Lock()
	class := integrityState.classes[db]
	integrityState.Unlock()
	reason := ""
	switch {
	case class == nil || class.path != db.Path():
		reason = integrityFallbackIncomplete
	case integrityEpoch.Load() != class.epoch || tx.ID() < class.txid:
		reason = integrityFallbackEpochChanged
	}
	integrityState.Lock()
	if reason != "" {
		integrityState.fallbacks[reason]++
		class = nil
	} else {
		integrityState.fastReads++
	}
	integrityState.Unlock()
	return class
}

type classificationAttempt struct {
	installed     bool
	reason        string
	class         *integrityClassification
	epoch         uint64
	err           error
	duration      time.Duration
	segmentMax    time.Duration
	writerLockMax time.Duration
	measured      bool
}

func runClassificationAttempt(ctx context.Context, db *bolt.DB) (attempt classificationAttempt) {
	var started time.Time
	defer func() {
		if attempt.measured {
			attempt.duration = time.Since(started)
		}
	}()
	var build *classificationBuild
	for {
		if attempt.err = ctx.Err(); attempt.err != nil {
			return
		}
		lockStarted := time.Now()
		writer, err := db.Begin(true)
		if err != nil {
			attempt.err = err
			return
		}
		// The in-flight count is incremented before the maintenance epoch.
		epoch := integrityEpoch.Load()
		inflight := integrityInflight.Load()
		if inflight != 0 || (build != nil && epoch != build.epoch) {
			_ = writer.Rollback()
			if d := time.Since(lockStarted); d > attempt.writerLockMax {
				attempt.writerLockMax = d
			}
			attempt.reason = integrityFallbackEpochChanged
			if build == nil {
				attempt.reason = integrityFallbackIncomplete
			}
			return
		}
		tx, err := db.Begin(false)
		_ = writer.Rollback()
		if d := time.Since(lockStarted); d > attempt.writerLockMax {
			attempt.writerLockMax = d
		}
		if err != nil {
			attempt.err = err
			return
		}
		if build == nil {
			started = time.Now()
			attempt.measured = true
			build, attempt.reason = newClassificationBuild(tx)
			if build == nil {
				_ = tx.Rollback()
				return
			}
			build.epoch = epoch
		} else {
			progress, classified := classificationIndexProgress(tx)
			if classified != build.indexClassified || !sameIndexProgress(progress, build.indexProgress) {
				_ = tx.Rollback()
				attempt.reason = integrityFallbackIndexProgressChanged
				return
			}
		}
		segmentStarted := time.Now()
		build.runSegment(ctx, tx, integrityClassifySegmentBudget, integrityClassifySegmentDuration)
		_ = tx.Rollback()
		if d := time.Since(segmentStarted); d > attempt.segmentMax {
			attempt.segmentMax = d
		}
		if integrityClassifySegmentBoundary != nil {
			integrityClassifySegmentBoundary()
		}
		if attempt.err = ctx.Err(); attempt.err != nil {
			return
		}
		if build.canceled || build.overflow {
			attempt.class, attempt.reason = build.finish()
			return
		}
		if build.stage == classifyDone {
			attempt.class, attempt.reason = build.finish()
			attempt.epoch = build.epoch
			attempt.err = ctx.Err()
			return
		}
	}
}

// recordClassificationAttempt preserves the existing fallback counts. Errors,
// including cancellation, leave an earlier installed class unchanged.
func recordClassificationAttempt(ctx context.Context, db *bolt.DB, attempt *classificationAttempt) uint64 {
	if attempt.class != nil && attempt.err == nil {
		// Validate and install while holding the writer lock, so progress cannot
		// change between the final snapshot and publication.
		var writer *bolt.Tx
		writer, attempt.err = db.Begin(true)
		if attempt.err == nil {
			defer writer.Rollback()
			if attempt.err = ctx.Err(); attempt.err == nil {
				if integrityEpoch.Load() != attempt.epoch || integrityInflight.Load() != 0 {
					attempt.class, attempt.reason = nil, integrityFallbackEpochChanged
				} else {
					var tx *bolt.Tx
					tx, attempt.err = db.Begin(false)
					if attempt.err == nil {
						progress, classified := classificationIndexProgress(tx)
						_ = tx.Rollback()
						if classified != attempt.class.indexClassified || !sameIndexProgress(progress, attempt.class.indexProgress) {
							attempt.class, attempt.reason = nil, integrityFallbackIndexProgressChanged
						}
						if attempt.err = ctx.Err(); attempt.err == nil && (integrityEpoch.Load() != attempt.epoch || integrityInflight.Load() != 0) {
							attempt.class, attempt.reason = nil, integrityFallbackEpochChanged
						}
					}
				}
			}
		}
	}
	integrityState.Lock()
	defer integrityState.Unlock()
	if attempt.err != nil {
		return integrityState.discardedRounds
	}
	if attempt.measured {
		integrityState.lastDuration = attempt.duration
	}
	if attempt.segmentMax > integrityState.lastSegmentMax {
		integrityState.lastSegmentMax = attempt.segmentMax
	}
	if attempt.writerLockMax > integrityState.lastWriterLockMax {
		integrityState.lastWriterLockMax = attempt.writerLockMax
	}
	if attempt.class == nil {
		delete(integrityState.classes, db)
		integrityState.fallbacks[attempt.reason]++
		integrityState.discardedRounds++
		integrityState.lastDiscardReason = attempt.reason
		return integrityState.discardedRounds
	}
	attempt.class.epoch = attempt.epoch
	integrityState.classes[db] = attempt.class
	integrityState.lastAnomaly = attempt.class.anomalous
	integrityState.discardedRounds = 0
	integrityState.lastDiscardReason = ""
	attempt.installed = true
	return 0
}

// RunClassificationOnce immediately builds one classification from short read
// transactions. A changed epoch or index progress discards the entire round.
func RunClassificationOnce(ctx context.Context, db *bolt.DB) error {
	attempt := runClassificationAttempt(ctx, db)
	recordClassificationAttempt(ctx, db, &attempt)
	return attempt.err
}

// ForgetUsageIntegrityClassification drops db's classification (for example
// before the database is closed).
func ForgetUsageIntegrityClassification(db *bolt.DB) {
	integrityState.Lock()
	delete(integrityState.classes, db)
	integrityState.Unlock()
}

// UsageIntegrityClassificationInterval is the default rebuild interval.
const UsageIntegrityClassificationInterval = 5 * time.Minute

// StartUsageIntegrityClassification rebuilds the classification now and then
// every interval until ctx ends. The returned function stops it and waits.
func StartUsageIntegrityClassification(ctx context.Context, db *bolt.DB, interval time.Duration, logger *slog.Logger) func() {
	if interval <= 0 {
		interval = UsageIntegrityClassificationInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ForgetUsageIntegrityClassification(db)
		for {
			attempt := runClassificationAttempt(ctx, db)
			discarded := recordClassificationAttempt(ctx, db, &attempt)
			if attempt.err != nil && ctx.Err() == nil && logger != nil {
				logger.Warn("usage integrity classification failed; integrity reads stay exhaustive", "error", attempt.err)
			}
			delay := interval
			if attempt.err == nil && !attempt.installed {
				delay = classificationDiscardDelay(discarded, interval)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func classificationDiscardDelay(discarded uint64, interval time.Duration) time.Duration {
	delay := 5 * time.Second
	if discarded == 2 {
		delay = 15 * time.Second
	} else if discarded >= 3 {
		delay = 45 * time.Second
	}
	if delay > interval {
		return interval
	}
	return delay
}

// UsageIntegrityMetrics is the exported, key-free state of the classifier.
type UsageIntegrityMetrics struct {
	Complete              bool              `json:"complete"`
	Epoch                 uint64            `json:"epoch"`
	AnomalousCount        int               `json:"anomalous_count"`
	LastDurationMS        float64           `json:"last_duration_ms"`
	FastReads             uint64            `json:"fast_reads"`
	FallbackReads         map[string]uint64 `json:"fallback_reads"`
	InflightWriters       int64             `json:"inflight_writers"`
	ClassificationStalled bool              `json:"classification_stalled"`
	DiscardedRounds       uint64            `json:"discarded_rounds"`
	LastDiscardReason     string            `json:"last_discard_reason"`
	SegmentMaxTxMS        float64           `json:"segment_max_tx_ms"`
	WriterLockMaxMS       float64           `json:"writer_lock_max_ms"`
}

// UsageIntegrityStats reports the classifier state for db.
func UsageIntegrityStats(db *bolt.DB) UsageIntegrityMetrics {
	integrityState.Lock()
	defer integrityState.Unlock()
	class := integrityState.classes[db]
	epoch := integrityEpoch.Load()
	m := UsageIntegrityMetrics{
		Complete:              class != nil && class.epoch == epoch,
		Epoch:                 epoch,
		AnomalousCount:        integrityState.lastAnomaly,
		LastDurationMS:        float64(integrityState.lastDuration.Microseconds()) / 1000,
		FastReads:             integrityState.fastReads,
		FallbackReads:         make(map[string]uint64),
		InflightWriters:       integrityInflight.Load(),
		ClassificationStalled: integrityState.discardedRounds >= 3,
		DiscardedRounds:       integrityState.discardedRounds,
		LastDiscardReason:     integrityState.lastDiscardReason,
		SegmentMaxTxMS:        float64(integrityState.lastSegmentMax.Microseconds()) / 1000,
		WriterLockMaxMS:       float64(integrityState.lastWriterLockMax.Microseconds()) / 1000,
	}
	for _, reason := range []string{integrityFallbackIncomplete, integrityFallbackEpochChanged, integrityFallbackIndexProgressChanged, integrityFallbackOverflow, integrityFallbackPartialRefs, integrityFallbackClientScope} {
		m.FallbackReads[reason] = integrityState.fallbacks[reason]
	}
	return m
}

// IntegrityStats reports the integrity classifier state of the store's
// database. It never includes keys or stored bytes.
func (s *UsageStore) IntegrityStats() UsageIntegrityMetrics {
	if s == nil || s.db == nil {
		return UsageIntegrityMetrics{FallbackReads: map[string]uint64{}}
	}
	return UsageIntegrityStats(s.db)
}
