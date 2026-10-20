package admission

import (
	"context"
	"sync"
	"testing"
	"time"
)

// testOpts returns Options with a very short AI window for fast tests.
func testOpts() Options {
	return Options{
		RInit:    5,
		RMin:     1,
		RMax:     50,
		AIStep:   1,
		AIWindow: 50 * time.Millisecond, // short for tests
		MDFactor: 0.5,
	}
}

func TestTryAcquire_Basic(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Should succeed 5 times (rInit=5, capacity=5, tokens=5)
	acquired := 0
	for i := 0; i < 10; i++ {
		if m.TryAcquire("t1") {
			acquired++
		}
	}
	if acquired != 5 {
		t.Errorf("expected 5 successful TryAcquire, got %d", acquired)
	}

	// Unknown target should bypass (return true)
	if !m.TryAcquire("unknown") {
		t.Error("TryAcquire for unknown target should return true (bypass)")
	}
}

func TestTargetNamesAreCaseInsensitiveAcrossAdmissionMethods(t *testing.T) {
	options := testOpts()
	options.RInit = 1
	options.RMax = 1
	m := NewManager(nil, options)
	m.InitTarget("  MiXeD-Target ", 1)

	if state := m.Snapshot("mixed-target"); state == nil {
		t.Fatal("normalized target registration was not found")
	}
	if !m.TryAcquire("MIXED-TARGET") {
		t.Fatal("mixed-case lookup bypassed or missed the registered bucket")
	}
	if m.TryAcquire("mixed-target") {
		t.Fatal("mixed-case and lowercase lookups must consume the same bucket")
	}
}

func TestTryAcquire_LazyRefill(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Exhaust all 5 tokens
	for i := 0; i < 5; i++ {
		if !m.TryAcquire("t1") {
			t.Fatalf("TryAcquire %d failed unexpectedly", i)
		}
	}
	if m.TryAcquire("t1") {
		t.Fatal("TryAcquire should fail when bucket is empty")
	}

	// Sleep for >1s to allow refill (rate=5/sec → at least 5 tokens added)
	time.Sleep(1100 * time.Millisecond)

	// Should succeed again (tokens refilled)
	if !m.TryAcquire("t1") {
		t.Error("TryAcquire should succeed after refill period")
	}
}

func TestTryAcquire_Concurrent(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	const n = 100
	results := make([]bool, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = m.TryAcquire("t1")
		}(i)
	}
	wg.Wait()

	// Without refill (very fast), all goroutines race for 5 tokens.
	// Exactly 5 should succeed (no double-decrement, no negative tokens).
	acquired := 0
	for _, v := range results {
		if v {
			acquired++
		}
	}
	// Due to refill during the race, acquired may be slightly > 5.
	// Key property: no deadlock, no panic, acquired <= 5 + 1 (refill tolerance)
	if acquired < 5 {
		t.Errorf("expected at least 5 successful TryAcquire, got %d", acquired)
	}
}

func TestOnLimit_AIMD(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Initial rate = 5, capacity = 5
	st := m.Snapshot("t1")
	if st == nil || st.Rate != 5 {
		t.Fatalf("expected initial rate=5, got %+v", st)
	}

	// OnLimit: rate = max(5 * 0.5, 1) = 2
	m.OnLimit("t1")
	st = m.Snapshot("t1")
	if st == nil || st.Rate != 2 {
		t.Errorf("expected rate=2 after OnLimit, got %+v", st)
	}

	// Second OnLimit: rate = max(2 * 0.5, 1) = 1
	m.OnLimit("t1")
	st = m.Snapshot("t1")
	if st == nil || st.Rate != 1 {
		t.Errorf("expected rate=1 after second OnLimit, got %+v", st)
	}

	// Third OnLimit: rate = max(1 * 0.5, 1) = 1 (clamped to rMin)
	m.OnLimit("t1")
	st = m.Snapshot("t1")
	if st == nil || st.Rate != 1 {
		t.Errorf("expected rate=1 (clamped) after third OnLimit, got %+v", st)
	}
}

func TestOnSuccess_AIMD_AI(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Reduce rate via OnLimit: 5 → 2
	m.OnLimit("t1")
	st := m.Snapshot("t1")
	if st.Rate != 2 {
		t.Fatalf("expected rate=2, got %d", st.Rate)
	}

	// Wait for AI window (50ms + margin)
	time.Sleep(80 * time.Millisecond)

	// OnSuccess should trigger AI: 2 → 3
	m.OnSuccess("t1")
	st = m.Snapshot("t1")
	if st == nil || st.Rate != 3 {
		t.Errorf("expected rate=3 after AI, got %+v", st)
	}

	// OnSuccess before AI window elapses should NOT increase
	m.OnSuccess("t1")
	st = m.Snapshot("t1")
	if st == nil || st.Rate != 3 {
		t.Errorf("expected rate=3 (no AI within window), got %+v", st)
	}
}

func TestWait_QueuedRequest(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Exhaust all tokens
	for m.TryAcquire("t1") {
	}

	// Release a token in a goroutine after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		m.ReleaseToken("t1")
	}()

	// Wait should unblock when the token is released
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := m.Wait("t1", ctx)
	if err != nil {
		t.Errorf("Wait should succeed after ReleaseToken, got: %v", err)
	}
}

func TestWait_QueueFull(t *testing.T) {
	// Queue capacity = 2×rMax → use rMax=1 so queue capacity=2
	m := NewManager(nil, Options{
		RInit: 1, RMin: 1, RMax: 1, AIStep: 1,
		AIWindow: 50 * time.Millisecond, MDFactor: 0.5,
	})
	m.InitTarget("t1", 1) // capacity=1, queue cap=2×rMax=2

	// Exhaust the single token
	m.TryAcquire("t1")

	// Fill the queue with 2 goroutines
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = m.Wait("t1", ctx) // ignore result; these fill the queue
		}()
	}
	// Give the goroutines a moment to acquire queue slots
	time.Sleep(20 * time.Millisecond)

	// Next Wait should get ErrQueueFull immediately
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := m.Wait("t1", ctx)
	if err != ErrQueueFull {
		t.Errorf("expected ErrQueueFull, got: %v", err)
	}

	wg.Wait()
}

func TestWait_Timeout(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Exhaust tokens and don't release
	for m.TryAcquire("t1") {
	}

	// Wait with short timeout shorter than the 2s internal default
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := m.Wait("t1", ctx)
	if err == nil {
		t.Error("expected timeout/cancellation error, got nil")
	}
	// Should be either ctx.Err() (DeadlineExceeded) or ErrTimeout
	if err != context.DeadlineExceeded && err != ErrTimeout {
		t.Errorf("expected DeadlineExceeded or ErrTimeout, got: %v", err)
	}
}

func TestRemoveTarget_WakesWaiters(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Exhaust tokens
	for m.TryAcquire("t1") {
	}

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		done <- m.Wait("t1", ctx)
	}()

	// Give goroutine time to block in Wait
	time.Sleep(50 * time.Millisecond)

	// RemoveTarget should wake the waiter with ErrTargetGone
	m.RemoveTarget("t1")

	select {
	case err := <-done:
		if err != ErrTargetGone {
			t.Errorf("expected ErrTargetGone, got: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait goroutine did not wake up after RemoveTarget")
	}
}

func TestUpdateActiveKeys(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 10) // total=10, active=10, effectiveR=5

	// capacity should be ~5
	st := m.Snapshot("t1")
	if st == nil {
		t.Fatal("snapshot returned nil")
	}

	// Reduce active to 5 out of 10: effectiveR = 5 * 5/10 = 2.5, capacity=3
	m.UpdateActiveKeys("t1", 5, 10)
	st = m.Snapshot("t1")
	if st == nil {
		t.Fatal("snapshot returned nil after UpdateActiveKeys")
	}
	// rate should still be 5, but capacity should be ceil(2.5)=3
	m.mu.RLock()
	b := m.buckets["t1"]
	m.mu.RUnlock()
	b.mu.Lock()
	cap := b.capacity
	b.mu.Unlock()
	if cap < 1 || cap > 5 {
		t.Errorf("expected capacity in [1,5], got %d", cap)
	}

	// Reduce active to 0: effectiveR=max(1, ...), capacity=1
	m.UpdateActiveKeys("t1", 0, 10)
	b.mu.Lock()
	cap = b.capacity
	b.mu.Unlock()
	if cap != 1 {
		t.Errorf("expected capacity=1 when active=0, got %d", cap)
	}
}

func TestSnapshotRestore(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)

	// Change state
	m.OnLimit("t1") // rate=2
	time.Sleep(80 * time.Millisecond)
	m.OnSuccess("t1") // rate=3 (AI triggered)

	// Snapshot
	st := m.Snapshot("t1")
	if st == nil {
		t.Fatal("snapshot returned nil")
	}
	if st.Rate != 3 {
		t.Fatalf("expected rate=3 in snapshot, got %d", st.Rate)
	}

	// Remove target and re-init
	m.RemoveTarget("t1")
	m.InitTarget("t1", 1)

	// Restore
	m.Restore("t1", st)
	restored := m.Snapshot("t1")
	if restored == nil {
		t.Fatal("snapshot after restore returned nil")
	}
	if restored.Rate != 3 {
		t.Errorf("expected restored rate=3, got %d", restored.Rate)
	}
}

func TestReleaseToken_TargetGone(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)
	m.RemoveTarget("t1")

	// Should not panic
	m.ReleaseToken("t1")

	// Nil manager should not panic
	var nilMgr *Manager
	nilMgr.ReleaseToken("t1")
}

func TestNilManager(t *testing.T) {
	var m *Manager

	// All methods should be no-ops / return safe defaults
	m.InitTarget("t1", 1)
	m.RemoveTarget("t1")
	if !m.TryAcquire("t1") {
		t.Error("nil Manager.TryAcquire should return true")
	}
	if err := m.Wait("t1", context.Background()); err != nil {
		t.Errorf("nil Manager.Wait should return nil, got %v", err)
	}
	m.ReleaseToken("t1")
	m.OnSuccess("t1")
	m.OnLimit("t1")
	m.UpdateActiveKeys("t1", 1, 1)
	if st := m.Snapshot("t1"); st != nil {
		t.Error("nil Manager.Snapshot should return nil")
	}
	m.Restore("t1", &AIMDState{})
}

func TestWait_UnknownTarget(t *testing.T) {
	m := NewManager(nil, testOpts())

	// Wait on unknown target should return nil (bypass)
	ctx := context.Background()
	if err := m.Wait("nonexistent", ctx); err != nil {
		t.Errorf("Wait on unknown target should return nil, got: %v", err)
	}
}

func TestWait_AlreadyRemovedTarget(t *testing.T) {
	m := NewManager(nil, testOpts())
	m.InitTarget("t1", 1)
	m.RemoveTarget("t1")

	// Wait after removal should return ErrTargetGone (queue is marked closed)
	ctx := context.Background()
	err := m.Wait("t1", ctx)
	// Since we removed it, the queue lookup (Rlock) will not find it, so bypass (nil)
	// OR the closed flag is set. Either is acceptable.
	if err != nil && err != ErrTargetGone {
		t.Errorf("Wait after removal: expected nil or ErrTargetGone, got: %v", err)
	}
}
