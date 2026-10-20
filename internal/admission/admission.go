// Package admission provides upstream capacity admission control for the LLM proxy.
//
// It implements a token bucket + AIMD adaptive rate + bounded queue mechanism
// to throttle outgoing requests per-target, preventing overwhelming upstream
// rate limits while allowing graceful degradation.
//
// Design principles:
//   - Nil-safe: all methods check `if m == nil { return ... }`
//   - Zero-lock fast path: TryAcquire uses atomic + CAS only, no mutex
//   - Pure rate bucket: tokens are supplied ONLY by time-based refill at rate R.
//     ReleaseToken is intentionally a no-op — returning tokens on completion would
//     add a second supply source (R + completion rate), so the bucket would never
//     empty under steady load and AIMD's R would stop binding throughput.
//   - Queue close on remove: RemoveTarget closes the done channel, waking blocked Waiters with ErrTargetGone
//   - Target names are normalized consistently for registration and lookup.
//   - Effective capacity: effectiveR = R × active/total, with minimum 1 token
package admission

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by Wait.
var (
	// ErrQueueFull is returned when the bounded queue for a target is full.
	ErrQueueFull = errors.New("admission: queue full")
	// ErrTimeout is returned when Wait exceeds the 2s internal timeout.
	ErrTimeout = errors.New("admission: wait timeout")
	// ErrTargetGone is returned when the target is removed while Wait is pending.
	ErrTargetGone = errors.New("admission: target removed")
)

// AIMDState is a snapshot of the AIMD state for a target, used for hot-reload
// (ApplyConfig) to preserve rate-limiting state across configuration reloads.
type AIMDState struct {
	Rate     int64 // current rate (tokens/sec)
	LastFail int64 // unixNano of last OnLimit (AI window reference)
	Tokens   int64 // current token count
}

// Options configures the AIMD parameters of a Manager. Zero values are replaced
// with sensible defaults. This is primarily useful in tests; production uses
// the defaults from NewManager.
type Options struct {
	RInit    int64         // initial rate (tokens/sec), default 5
	RMin     int64         // minimum rate after MD, default 1
	RMax     int64         // maximum rate after AI, default 50
	AIStep   int64         // additive increase step, default 1
	AIWindow time.Duration // quiet window required before AI fires, default 10s
	MDFactor float64       // multiplicative decrease factor, default 0.5
}

// Manager is the top-level admission controller. It manages a set of per-target
// token buckets and queues, protected by a single RWMutex for map access.
// A nil *Manager is safe to use: all methods return immediately (no-op).
type Manager struct {
	buckets map[string]*targetBucket
	queues  map[string]*admissionQueue
	mu      sync.RWMutex // protects buckets and queues maps
	logger  *slog.Logger

	// AIMD constants (read-only after construction)
	rInit    int64
	rMin     int64
	rMax     int64
	aiStep   int64
	aiWindow time.Duration
	mdFactor float64
}

func normalizeTargetName(target string) string {
	return strings.ToLower(strings.TrimSpace(target))
}

// targetBucket holds per-target token bucket + AIMD state.
// tokens/lastRefill/rateA/capA are atomic for lock-free fast-path reads.
// rate/capacity/lastFail/active/total/effectiveR are mu-protected and are
// written only during AIMD updates or key-change notifications.
type targetBucket struct {
	target string

	// Lock-free fast path fields
	tokens     atomic.Int64  // current token count
	lastRefill atomic.Int64  // unixNano of last refill
	rateA      atomic.Uint64 // float64 bits of effective R (tokens/sec)
	capA       atomic.Int64  // current capacity (ceil(effectiveR)), mirrors capacity

	// AIMD state (mu-protected)
	mu         sync.Mutex
	rate       int64   // current R (tokens/sec)
	capacity   int64   // = ceil(effectiveR), minimum 1
	lastFail   int64   // unixNano of last OnLimit (AI window reference)
	active     int64   // active key count
	total      int64   // total key count
	effectiveR float64 // R × active/total
}

// admissionQueue is a bounded FIFO queue for waiting requests per target.
// ch holds queue slots (send on enqueue, receive on release).
// done is closed when the target is removed, waking all blocked Waiters.
// closed is set atomically when the target is removed.
type admissionQueue struct {
	target string
	ch     chan struct{} // bounded FIFO, capacity = 2×capacity
	done   chan struct{} // closed on RemoveTarget to wake waiters in select
	closed atomic.Bool
}

// NewManager creates a new Manager with default AIMD parameters.
// If opts is provided, it overrides the defaults. Pass nil for production defaults.
// If logger is nil, slog.Default() is used.
func NewManager(logger *slog.Logger, opts ...Options) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.RInit <= 0 {
		o.RInit = 5
	}
	if o.RMin <= 0 {
		o.RMin = 1
	}
	if o.RMax <= 0 {
		o.RMax = 50
	}
	if o.AIStep <= 0 {
		o.AIStep = 1
	}
	if o.AIWindow <= 0 {
		o.AIWindow = 10 * time.Second
	}
	if o.MDFactor <= 0 || o.MDFactor >= 1 {
		o.MDFactor = 0.5
	}
	return &Manager{
		buckets:  make(map[string]*targetBucket),
		queues:   make(map[string]*admissionQueue),
		logger:   logger,
		rInit:    o.RInit,
		rMin:     o.RMin,
		rMax:     o.RMax,
		aiStep:   o.AIStep,
		aiWindow: o.AIWindow,
		mdFactor: o.MDFactor,
	}
}

// InitTarget registers or updates a target in the Manager.
// On first registration, a token bucket is created with rate=rInit and
// capacity=ceil(rInit). If totalKeys>0, all keys are assumed active initially.
// If the target already exists, only totalKeys (and active) are updated;
// the AIMD state is preserved.
func (m *Manager) InitTarget(target string, totalKeys int) {
	if m == nil {
		return
	}
	target = normalizeTargetName(target)
	if target == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if b, ok := m.buckets[target]; ok {
		// Update totalKeys on existing bucket
		b.mu.Lock()
		b.total = int64(totalKeys)
		if b.active < 1 || b.active > b.total {
			b.active = b.total
		}
		if b.active < 0 {
			b.active = 0
		}
		b.syncEffectiveLocked()
		b.mu.Unlock()
		// Also update queue capacity if needed (skip to avoid disrupting waiters)
		return
	}

	rate := m.rInit
	effectiveR := float64(rate)
	if totalKeys > 0 {
		active := int64(totalKeys)
		effectiveR = float64(rate) * float64(active) / float64(int64(totalKeys))
	}
	if effectiveR < 1 {
		effectiveR = 1
	}
	capacity := int64(math.Ceil(effectiveR))
	if capacity < 1 {
		capacity = 1
	}

	b := &targetBucket{
		target:     target,
		rate:       rate,
		capacity:   capacity,
		active:     int64(totalKeys),
		total:      int64(totalKeys),
		effectiveR: effectiveR,
	}
	if b.active < 0 {
		b.active = 0
	}
	b.tokens.Store(capacity)
	b.lastRefill.Store(time.Now().UnixNano())
	b.rateA.Store(math.Float64bits(effectiveR))
	b.capA.Store(capacity)

	// 队列容量按 AIMD 可达上限 rMax 预留（而非创建时的 2×rInit）：否则 AIMD 升速
	// 后排队位仍是初始容量的 2 倍，突发时过早返回 ErrQueueFull。channel 容量无法
	// 动态调整，故直接按上界分配（struct{} 零字节，成本可忽略）。
	qCap := int(2 * m.rMax)
	if qCap < 2 {
		qCap = 2
	}
	q := &admissionQueue{
		target: target,
		ch:     make(chan struct{}, qCap),
		done:   make(chan struct{}),
	}

	m.buckets[target] = b
	m.queues[target] = q
}

// RemoveTarget removes the target from the Manager and wakes all blocked
// Waiters with ErrTargetGone via the done channel. If the target is not
// found, logs a warning.
func (m *Manager) RemoveTarget(target string) {
	if m == nil {
		return
	}
	target = normalizeTargetName(target)

	m.mu.Lock()
	q, qok := m.queues[target]
	if !qok {
		m.mu.Unlock()
		m.logger.Warn("[admission] RemoveTarget: target not found", "target", target)
		return
	}
	delete(m.buckets, target)
	delete(m.queues, target)
	m.mu.Unlock()

	q.closed.Store(true)
	close(q.done) // wake all waiters blocked in select
	// Don't close q.ch: waiters release their slots via defer <-q.ch which
	// reads from a closed channel (returns zero value) — harmless. Leaving it
	// open is safe since no more sends will occur once the queue is deleted.
}

// TryAcquire is the zero-lock fast path for acquiring a token.
// Returns true if a token was successfully consumed, false if the bucket is empty.
// For unknown targets (not in the Manager), returns true (bypass).
// All operations are atomic (CAS loop), no mutex is acquired.
func (m *Manager) TryAcquire(target string) bool {
	if m == nil {
		return true
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	b, ok := m.buckets[target]
	m.mu.RUnlock()
	if !ok {
		return true // bypass unknown targets
	}

	// Lazy refill (atomic only): compute elapsed time and add tokens
	now := time.Now().UnixNano()
	last := b.lastRefill.Load()
	elapsed := now - last
	if elapsed < 0 {
		// Clock skew protection: reset lastRefill to now without adding tokens
		b.lastRefill.CompareAndSwap(last, now)
	} else {
		rate := math.Float64frombits(b.rateA.Load())
		capacity := b.capA.Load()
		if rate < 1 {
			rate = 1
		}
		if capacity < 1 {
			capacity = 1
		}
		// add = elapsed(ns) * rate / 1e9 → tokens to add
		add := int64(float64(elapsed) * rate / float64(time.Second))
		if add > 0 {
			// 先 CAS lastRefill 抢占本轮补币权：只有胜者加币，同一 elapsed 窗口
			// 不会被并发 goroutine 重复入账（旧实现先加币后 CAS lastRefill，
			// 失败方币已加入 → 有界超量放行）。add==0 时不推进 lastRefill，
			// 保留不足一币的碎片时间累积。
			if b.lastRefill.CompareAndSwap(last, now) {
				for {
					cur := b.tokens.Load()
					nv := cur + add
					if nv > capacity {
						nv = capacity
					}
					if nv <= cur {
						break // nothing to add (already at cap)
					}
					if b.tokens.CompareAndSwap(cur, nv) {
						break
					}
					// CAS failed: concurrent consume moved tokens, re-read and retry
				}
			}
		}
	}

	// CAS loop to consume a token
	for {
		cur := b.tokens.Load()
		if cur <= 0 {
			return false
		}
		if b.tokens.CompareAndSwap(cur, cur-1) {
			return true
		}
	}
}

// Wait is the slow path: enqueue the request and wait for a token.
// The queue is bounded (2×capacity); if full, returns ErrQueueFull immediately.
// Waits up to min(2s, ctx deadline) with a ticker at 1/rate interval.
// Returns nil on success, or one of: ErrQueueFull, ErrTimeout, ErrTargetGone, ctx.Err().
// For unknown targets, returns nil (bypass).
func (m *Manager) Wait(target string, ctx context.Context) error {
	if m == nil {
		return nil
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	q, ok := m.queues[target]
	m.mu.RUnlock()
	if !ok {
		return nil // bypass unknown targets
	}
	if q.closed.Load() {
		return ErrTargetGone
	}

	// Acquire a queue slot (bounded FIFO)
	select {
	case q.ch <- struct{}{}:
	default:
		return ErrQueueFull
	}
	// Release the slot when Wait exits (on all paths)
	defer func() {
		// Safe even if q.ch is closed (reading from closed channel returns zero)
		<-q.ch
	}()

	// Compute ticker interval from current rate
	m.mu.RLock()
	b, bok := m.buckets[target]
	m.mu.RUnlock()
	if !bok {
		return ErrTargetGone
	}

	rate := math.Float64frombits(b.rateA.Load())
	if rate < 1 {
		rate = 1
	}
	interval := time.Duration(float64(time.Second) / rate)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Timeout: min(2s, remaining ctx deadline)
	timeout := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return ErrTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			if m.TryAcquire(target) {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrTimeout
		case <-q.done:
			return ErrTargetGone
		}
	}
}

// ReleaseToken is a deliberate no-op kept for call-site compatibility.
//
// 纠正（P5）：旧实现在请求完成时往桶里归还 1 币，与 TryAcquire 的时间补币
// 叠加后稳态供币速率 = R + 完成速率：只要请求持续完成，桶永不空，AIMD
// 调出的 R 完全不再约束吞吐（退化为并发限制器）。令牌桶是速率限制器，
// 消耗的币不归还，仅由时间按 R 补充。
func (m *Manager) ReleaseToken(target string) {
	// no-op: tokens are replenished exclusively by the time-based refill in
	// TryAcquire so that AIMD's R actually bounds the outgoing request rate.
	_ = target
}

// OnSuccess signals a successful upstream response for the given target.
// Triggers Additive Increase (AI): if no failure occurred within the AI window,
// rate is increased by aiStep (up to rMax).
func (m *Manager) OnSuccess(target string) {
	if m == nil {
		return
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	b, ok := m.buckets[target]
	m.mu.RUnlock()
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UnixNano()
	window := m.aiWindow.Nanoseconds()
	if now-b.lastFail >= window {
		newRate := b.rate + m.aiStep
		if newRate > m.rMax {
			newRate = m.rMax
		}
		b.rate = newRate
		b.lastFail = now // reset AI window: at most one AI increment per window
		b.syncEffectiveLocked()
	}
}

// OnLimit signals an upstream rate-limit response (HTTP 429) for the given target.
// Triggers Multiplicative Decrease (MD): rate is reduced by mdFactor
// (clamped to rMin). Sets lastFail to now, resetting the AI window.
func (m *Manager) OnLimit(target string) {
	if m == nil {
		return
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	b, ok := m.buckets[target]
	m.mu.RUnlock()
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	newRate := int64(float64(b.rate) * m.mdFactor)
	if newRate < m.rMin {
		newRate = m.rMin
	}
	b.rate = newRate
	b.lastFail = time.Now().UnixNano()
	b.syncEffectiveLocked()
}

// UpdateActiveKeys updates the active/total key counts for a target and
// recalculates the effective capacity. Called when a key is marked exhausted,
// recovered, blocked, or unblocked. effectiveR = R × active/total (min 1).
func (m *Manager) UpdateActiveKeys(target string, active, total int) {
	if m == nil {
		return
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	b, ok := m.buckets[target]
	m.mu.RUnlock()
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.active = int64(active)
	b.total = int64(total)
	if b.active < 0 {
		b.active = 0
	}
	if b.total < 0 {
		b.total = 0
	}
	b.syncEffectiveLocked()
}

// Snapshot returns a copy of the current AIMD state for the given target.
// Returns nil if the target is not found or m is nil.
// Used during ApplyConfig to preserve AIMD state across config reloads.
func (m *Manager) Snapshot(target string) *AIMDState {
	if m == nil {
		return nil
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	b, ok := m.buckets[target]
	m.mu.RUnlock()
	if !ok {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return &AIMDState{
		Rate:     b.rate,
		LastFail: b.lastFail,
		Tokens:   b.tokens.Load(),
	}
}

// Restore sets the AIMD state for a target from a previously captured snapshot.
// The rate is clamped to [rMin, rMax]. If the target is not found, silently no-ops.
// Used after ApplyConfig to bring the new bucket back to the state of the old one.
func (m *Manager) Restore(target string, state *AIMDState) {
	if m == nil || state == nil {
		return
	}
	target = normalizeTargetName(target)
	m.mu.RLock()
	b, ok := m.buckets[target]
	m.mu.RUnlock()
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.rate = state.Rate
	if b.rate < m.rMin {
		b.rate = m.rMin
	}
	if b.rate > m.rMax {
		b.rate = m.rMax
	}
	b.lastFail = state.LastFail
	b.syncEffectiveLocked()

	// Restore tokens, clamped to current capacity
	tokens := state.Tokens
	capacity := b.capA.Load()
	if capacity < 1 {
		capacity = 1
	}
	if tokens > capacity {
		tokens = capacity
	}
	if tokens < 0 {
		tokens = 0
	}
	b.tokens.Store(tokens)
}

// syncEffectiveLocked recalculates effectiveR, capacity, and updates the atomic
// mirrors (rateA, capA). Clamps tokens to new capacity.
// Must be called while holding b.mu.
func (b *targetBucket) syncEffectiveLocked() {
	if b.total > 0 && b.active >= 0 {
		b.effectiveR = float64(b.rate) * float64(b.active) / float64(b.total)
	} else {
		b.effectiveR = float64(b.rate)
	}
	if b.effectiveR < 1 {
		b.effectiveR = 1
	}
	newCap := int64(math.Ceil(b.effectiveR))
	if newCap < 1 {
		newCap = 1
	}
	b.capacity = newCap
	b.rateA.Store(math.Float64bits(b.effectiveR))
	b.capA.Store(b.capacity)

	// Clamp tokens to new capacity (capacity may have decreased)
	for {
		cur := b.tokens.Load()
		if cur <= b.capacity {
			break
		}
		if b.tokens.CompareAndSwap(cur, b.capacity) {
			break
		}
	}
}
