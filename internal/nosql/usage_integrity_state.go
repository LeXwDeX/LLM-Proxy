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
	integrityFallbackIncomplete   = "incomplete"
	integrityFallbackEpochChanged = "epoch_changed"
	integrityFallbackOverflow     = "anomalous_overflow"
	integrityFallbackPartialRefs  = "partial_refs_fallback"
)

// integrityForceFast is set only by this package's tests (see
// usage_integrity_force_test.go): every integrity read then classifies its own
// read transaction and takes the fast path. Production code never sets it.
var integrityForceFast atomic.Bool

// integrityClassifyStep, when set by tests, runs at every cancellation check
// of a classification build.
var integrityClassifyStep func()

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
	classes      map[*bolt.DB]*integrityClassification
	lastDuration time.Duration
	lastAnomaly  int
	fastReads    uint64
	fallbacks    map[string]uint64
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

// RunClassificationOnce builds the anomalous-object classification for db in
// one read transaction and installs it for later reads. The snapshot is taken
// while holding the bbolt writer lock, so no write transaction (and hence no
// transaction marked by integrityMarkTx) is in progress at that point. A
// maintenance write in flight, an epoch change during the build, or ctx ending
// discards the result; on cancellation it returns ctx.Err().
func RunClassificationOnce(ctx context.Context, db *bolt.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	writer, err := db.Begin(true)
	if err != nil {
		return err
	}
	// Read the epoch before checking for maintenance writes in flight: their
	// begin increments the in-flight count before the epoch.
	epoch := integrityEpoch.Load()
	if integrityInflight.Load() != 0 {
		_ = writer.Rollback()
		integrityDropClass(db, integrityFallbackIncomplete)
		return nil
	}
	tx, err := db.Begin(false)
	_ = writer.Rollback()
	if err != nil {
		return err
	}
	started := time.Now()
	class, reason := classifyUsageIntegrity(ctx, tx)
	_ = tx.Rollback()
	if err := ctx.Err(); err != nil {
		return err
	}
	if class != nil && (integrityEpoch.Load() != epoch || integrityInflight.Load() != 0) {
		class, reason = nil, integrityFallbackEpochChanged
	}
	integrityState.Lock()
	defer integrityState.Unlock()
	integrityState.lastDuration = time.Since(started)
	if class == nil {
		delete(integrityState.classes, db)
		integrityState.fallbacks[reason]++
		return nil
	}
	class.epoch = epoch
	integrityState.classes[db] = class
	integrityState.lastAnomaly = class.anomalous
	return nil
}

func integrityDropClass(db *bolt.DB, reason string) {
	integrityState.Lock()
	delete(integrityState.classes, db)
	integrityState.fallbacks[reason]++
	integrityState.Unlock()
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
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := RunClassificationOnce(ctx, db); err != nil && ctx.Err() == nil && logger != nil {
				logger.Warn("usage integrity classification failed; integrity reads stay exhaustive", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// UsageIntegrityMetrics is the exported, key-free state of the classifier.
type UsageIntegrityMetrics struct {
	Complete        bool              `json:"complete"`
	Epoch           uint64            `json:"epoch"`
	AnomalousCount  int               `json:"anomalous_count"`
	LastDurationMS  float64           `json:"last_duration_ms"`
	FastReads       uint64            `json:"fast_reads"`
	FallbackReads   map[string]uint64 `json:"fallback_reads"`
	InflightWriters int64             `json:"inflight_writers"`
}

// UsageIntegrityStats reports the classifier state for db.
func UsageIntegrityStats(db *bolt.DB) UsageIntegrityMetrics {
	integrityState.Lock()
	defer integrityState.Unlock()
	class := integrityState.classes[db]
	epoch := integrityEpoch.Load()
	m := UsageIntegrityMetrics{
		Complete:        class != nil && class.epoch == epoch,
		Epoch:           epoch,
		AnomalousCount:  integrityState.lastAnomaly,
		LastDurationMS:  float64(integrityState.lastDuration.Microseconds()) / 1000,
		FastReads:       integrityState.fastReads,
		FallbackReads:   make(map[string]uint64),
		InflightWriters: integrityInflight.Load(),
	}
	for _, reason := range []string{integrityFallbackIncomplete, integrityFallbackEpochChanged, integrityFallbackOverflow, integrityFallbackPartialRefs} {
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
