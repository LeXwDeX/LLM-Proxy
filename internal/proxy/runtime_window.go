package proxy

import (
	"sync"
	"time"
)

const runtimeWindowSeconds = 15 * 60

// RuntimeWindow is the last 15 minutes of scheduling activity, at one-second
// resolution. InFlightPeak includes requests spanning the entire window.
type RuntimeWindow struct {
	InFlightPeak int64 `json:"in_flight_peak"`
	AffinityHits int64 `json:"affinity_hits"`
	BreakerTrips int64 `json:"breaker_trips"`
}

type runtimeBucket struct {
	RuntimeWindow
	second int64
}

// targetRuntimeWindow has bounded storage regardless of request volume. The
// same instance survives configuration reloads of a target with the same name.
type targetRuntimeWindow struct {
	mu          sync.Mutex
	buckets     [runtimeWindowSeconds]runtimeBucket
	initialized bool
	lastSecond  int64
	inFlight    int64
}

// advance carries current occupancy through elapsed seconds, so a long stream
// remains visible even when no new requests arrive and nobody polls metrics.
// Callers hold mu. Backward clock adjustments cannot erase newer buckets.
func (w *targetRuntimeWindow) advance(now time.Time) *runtimeBucket {
	second := now.Unix()
	if w.initialized && second < w.lastSecond {
		second = w.lastSecond
	}
	start := second
	if w.initialized {
		start = w.lastSecond + 1
		if start < second-runtimeWindowSeconds+1 {
			start = second - runtimeWindowSeconds + 1
		}
	}
	for s := start; s <= second; s++ {
		i := s % runtimeWindowSeconds
		if i < 0 {
			i += runtimeWindowSeconds
		}
		w.buckets[i] = runtimeBucket{second: s, RuntimeWindow: RuntimeWindow{InFlightPeak: w.inFlight}}
	}
	w.lastSecond = second
	w.initialized = true
	i := second % runtimeWindowSeconds
	if i < 0 {
		i += runtimeWindowSeconds
	}
	return &w.buckets[i]
}

func (w *targetRuntimeWindow) recordLocked(now time.Time, inFlightDelta, affinity, breaker int64) {
	b := w.advance(now)
	w.inFlight += inFlightDelta
	if w.inFlight < 0 {
		w.inFlight = 0
	}
	if w.inFlight > b.InFlightPeak {
		b.InFlightPeak = w.inFlight
	}
	b.AffinityHits += affinity
	b.BreakerTrips += breaker
}

func (w *targetRuntimeWindow) record(now time.Time, inFlightDelta, affinity, breaker int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked(now, inFlightDelta, affinity, breaker)
}

func (w *targetRuntimeWindow) snapshot(now time.Time) RuntimeWindow {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.advance(now)
	var result RuntimeWindow
	for _, b := range w.buckets {
		if b.second <= w.lastSecond-runtimeWindowSeconds || b.second > w.lastSecond {
			continue
		}
		if b.InFlightPeak > result.InFlightPeak {
			result.InFlightPeak = b.InFlightPeak
		}
		result.AffinityHits += b.AffinityHits
		result.BreakerTrips += b.BreakerTrips
	}
	return result
}
