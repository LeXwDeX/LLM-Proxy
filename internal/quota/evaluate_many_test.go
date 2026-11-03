package quota

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// EvaluateMany backs the synchronous quota sync: it must run with bounded
// concurrency (not serially) and still return only after every name finished.
func TestEvaluateManyBoundedConcurrencyAndCompletes(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	var cur, peak, done atomic.Int32
	var mu sync.Mutex
	m.evaluateHook = func(string, bool) {
		n := cur.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		done.Add(1)
	}
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	m.EvaluateMany(names)
	if done.Load() != int32(len(names)) {
		t.Fatalf("returned before completion: %d/%d", done.Load(), len(names))
	}
	if p := peak.Load(); p < 2 || p > preloadConcurrency {
		t.Fatalf("peak concurrency %d, want 2..%d", p, preloadConcurrency)
	}
}
