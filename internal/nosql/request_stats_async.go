package nosql

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	bolt "go.etcd.io/bbolt"
)

const (
	defaultRequestStatsQueueSize = 16384
	defaultRequestStatsMaxBatch  = 1024
)

var (
	// ErrRequestStatsQueueFull reports a completion dropped because the
	// background writer is behind. Callers count it as a record failure.
	ErrRequestStatsQueueFull = errors.New("request stats queue full; completion dropped")
	// ErrRequestStatsClosed reports a completion recorded after Close.
	ErrRequestStatsClosed = errors.New("request stats recorder closed")
)

func validateRequestStatsEvent(event requeststats.Event) error {
	if event.Timestamp.IsZero() || event.Timestamp.Unix() < 0 {
		return fmt.Errorf("invalid request stats timestamp")
	}
	if event.Outcome != requeststats.Success && event.Outcome != requeststats.Failure && event.Outcome != requeststats.Canceled {
		return fmt.Errorf("invalid request stats outcome %q", event.Outcome)
	}
	return nil
}

type requestStatsDelta struct{ total, success, failure, canceled uint64 }

func applyRequestStatsDelta(b *bolt.Bucket, key []byte, delta requestStatsDelta) error {
	c, err := decodeRequestCounters(b.Get(key))
	if err != nil {
		return err
	}
	if c.Total > math.MaxUint64-delta.total {
		return fmt.Errorf("request stats overflow")
	}
	c.Total += delta.total
	c.Success += delta.success
	c.Failure += delta.failure
	c.Canceled += delta.canceled
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return b.Put(key, raw)
}

// RecordBatch folds events into per-minute deltas and writes them in one
// transaction. If that fails, each minute is retried on its own so a corrupt
// cell only loses its own events. It returns how many events were not stored.
func (s *RequestStatsStore) RecordBatch(events []requeststats.Event) (int, error) {
	deltas := make(map[uint64]*requestStatsDelta)
	invalid := 0
	var invalidErr error
	for _, event := range events {
		if err := validateRequestStatsEvent(event); err != nil {
			invalid++
			invalidErr = err
			continue
		}
		minute := uint64(event.Timestamp.UTC().Unix() / 60)
		delta := deltas[minute]
		if delta == nil {
			delta = &requestStatsDelta{}
			deltas[minute] = delta
		}
		delta.total++
		switch event.Outcome {
		case requeststats.Success:
			delta.success++
		case requeststats.Failure:
			delta.failure++
		case requeststats.Canceled:
			delta.canceled++
		}
	}
	minutes := make([]uint64, 0, len(deltas))
	for minute := range deltas {
		minutes = append(minutes, minute)
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	key := func(minute uint64) []byte {
		raw := make([]byte, 8)
		binary.BigEndian.PutUint64(raw, minute) // same layout as requestMinuteKey
		return raw
	}
	write := func(group []uint64) error {
		return s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketRequestStats))
			if b == nil {
				return fmt.Errorf("request stats bucket unavailable")
			}
			for _, minute := range group {
				if err := applyRequestStatsDelta(b, key(minute), *deltas[minute]); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if len(minutes) == 0 || write(minutes) == nil {
		return invalid, invalidErr
	}
	failed, firstErr := invalid, invalidErr
	for _, minute := range minutes {
		if err := write([]uint64{minute}); err != nil {
			failed += int(deltas[minute].total)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return failed, firstErr
}

// AsyncRequestStatsOptions tunes AsyncRequestStatsRecorder.
type AsyncRequestStatsOptions struct {
	QueueSize int // bounded queue; a full queue drops. Default 16384.
	MaxBatch  int // events folded into one write. Default 1024.
}

// AsyncRequestStatsRecorder takes request completion statistics off the
// response path: Record only enqueues, a background writer folds batches
// into minute cells. Statistics are non-billing, so a full queue drops the
// completion and reports it instead of delaying the response.
type AsyncRequestStatsRecorder struct {
	store    *RequestStatsStore
	logger   *slog.Logger
	maxBatch int

	mu     sync.RWMutex
	closed bool
	queue  chan requeststats.Event
	done   chan struct{}

	pendingMu   sync.Mutex
	pendingCond *sync.Cond
	pending     int

	failures atomic.Int64
	dropped  atomic.Int64
}

// NewAsyncRequestStatsRecorder starts the background writer.
func NewAsyncRequestStatsRecorder(store *RequestStatsStore, opts AsyncRequestStatsOptions, logger *slog.Logger) *AsyncRequestStatsRecorder {
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultRequestStatsQueueSize
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = defaultRequestStatsMaxBatch
	}
	r := &AsyncRequestStatsRecorder{
		store:    store,
		logger:   logger,
		maxBatch: opts.MaxBatch,
		queue:    make(chan requeststats.Event, opts.QueueSize),
		done:     make(chan struct{}),
	}
	r.pendingCond = sync.NewCond(&r.pendingMu)
	go r.run()
	return r
}

// Record validates and enqueues without blocking.
func (r *AsyncRequestStatsRecorder) Record(event requeststats.Event) error {
	if err := validateRequestStatsEvent(event); err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrRequestStatsClosed
	}
	r.addPending(1)
	select {
	case r.queue <- event:
		return nil
	default:
		r.addPending(-1)
		r.dropped.Add(1)
		return ErrRequestStatsQueueFull
	}
}

func (r *AsyncRequestStatsRecorder) run() {
	defer close(r.done)
	for first := range r.queue {
		batch := []requeststats.Event{first}
	collect:
		for len(batch) < r.maxBatch {
			select {
			case next, ok := <-r.queue:
				if !ok {
					break collect
				}
				batch = append(batch, next)
			default:
				break collect
			}
		}
		if failed, err := r.store.RecordBatch(batch); failed > 0 {
			r.failures.Add(int64(failed))
			if r.logger != nil {
				r.logger.Error("request stats record failed", "events", failed, "error", err)
			}
		}
		r.addPending(-len(batch))
	}
}

// Failures reports completions accepted by Record that could not be written.
func (r *AsyncRequestStatsRecorder) Failures() int64 { return r.failures.Load() }

// Dropped reports completions rejected because the queue was full. Callers
// also count them as record failures from the returned error.
func (r *AsyncRequestStatsRecorder) Dropped() int64 { return r.dropped.Load() }

// Flush waits until every accepted completion has been written or counted.
func (r *AsyncRequestStatsRecorder) Flush() {
	r.pendingMu.Lock()
	for r.pending > 0 {
		r.pendingCond.Wait()
	}
	r.pendingMu.Unlock()
}

// Close stops accepting completions and writes every queued one. It must run
// before the database is closed. It is idempotent.
func (r *AsyncRequestStatsRecorder) Close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
	return nil
}

func (r *AsyncRequestStatsRecorder) addPending(delta int) {
	r.pendingMu.Lock()
	r.pending += delta
	if r.pending <= 0 {
		r.pending = 0
		r.pendingCond.Broadcast()
	}
	r.pendingMu.Unlock()
}
