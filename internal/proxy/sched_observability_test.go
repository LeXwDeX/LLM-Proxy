package proxy

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
)

// TestSchedulingPhaseDerivation 验收 S5 可观测①：targetState 派生调度相位四态
// （ready / muted / half-open / offline），与 S4 状态机权威（mutedUntil + 连续失败）一致，
// 不引入第二熔断。
func TestSchedulingPhaseDerivation(t *testing.T) {
	svc := newCapacityTestService(t)
	now := time.Now()

	// ready：新鲜节点无故障史。
	a := capState(t, svc, "cap-a")
	if got := a.Phase(now); got != "ready" {
		t.Fatalf("fresh target phase = %q, want %q", got, "ready")
	}

	// muted：默认阈值 1，单次失败即进入冷却。
	a.MarkFailure(now, time.Second)
	if got := a.Phase(now); got != "muted" {
		t.Fatalf("after failure phase = %q, want %q", got, "muted")
	}

	// half-open：冷却到期但连续失败仍 ≥ 阈值（恢复窗口）。
	if got := a.Phase(now.Add(2 * time.Second)); got != "half-open" {
		t.Fatalf("after cooldown expiry phase = %q, want %q", got, "half-open")
	}

	// 探测成功清零回 ready。
	a.MarkSuccess(now.Add(2 * time.Second))
	if got := a.Phase(now.Add(2 * time.Second)); got != "ready" {
		t.Fatalf("after probe success phase = %q, want %q", got, "ready")
	}

	// offline：paused 节点优先于一切熔断状态。
	b := capState(t, svc, "cap-b")
	b.Target().Paused = true
	b.MarkFailure(now, time.Second)
	if got := b.Phase(now); got != "offline" {
		t.Fatalf("paused target phase = %q, want %q", got, "offline")
	}
}

// TestAffinityHitAndBreakerTripCounters 验收 S5 可观测②：节点级亲和命中数与
// 熔断次数计数器随真实事件推进（亲和命中走 selectTarget 粘连路径；
// 熔断次数在连续失败达阈值跨入冷却时 +1，未达阈值只累计不计数）。
func TestAffinityHitAndBreakerTripCounters(t *testing.T) {
	svc := newCapacityTestService(t)
	a := capState(t, svc, "cap-a")

	// 熔断计数：阈值以下不计，达阈值恰好 +1。
	st := newBreakerStateForCounters(a, t, 3)
	st.MarkFailure(time.Now(), time.Second)
	st.MarkFailure(time.Now(), time.Second)
	if got := st.breakerTrips.Load(); got != 0 {
		t.Fatalf("trips below threshold = %d, want 0", got)
	}
	st.MarkFailure(time.Now(), time.Second)
	if got := st.breakerTrips.Load(); got != 1 {
		t.Fatalf("trips at threshold = %d, want 1", got)
	}
	// half-open 探测失败延长冷却同样是一次熔断事件。
	time.Sleep(10 * time.Millisecond) // 保证 mutedUntil 过期进入 half-open
	st.MarkFailure(time.Now(), time.Second)
	if got := st.breakerTrips.Load(); got != 2 {
		t.Fatalf("trips after probe failure extension = %d, want 2", got)
	}

	// 亲和命中：粘连记录存在且节点健康时回粘 ⇒ 命中计数 +1。
	p := &auth.Principal{Name: "aff-user"}
	firstState, _, kind, err := svc.selectTarget(p, "10.9.9.9", "", "", nil, nil, "", "/v1/chat/completions", time.Now())
	if err != nil || firstState == nil {
		t.Fatalf("initial pick failed: kind=%v err=%v", kind, err)
	}
	svc.affinity.Set(affinityKey("10.9.9.9", p.Name, ""), firstState.Target().Name, time.Now())
	for i := 1; i <= 3; i++ {
		st2, _, kind2, err := svc.selectTarget(p, "10.9.9.9", "", "", nil, nil, "", "/v1/chat/completions", time.Now())
		if err != nil {
			t.Fatalf("affinity pick #%d: %v", i, err)
		}
		if kind2 != selectionAffinityHit {
			t.Fatalf("pick #%d kind=%v, want selectionAffinityHit", i, kind2)
		}
		if got := st2.affinityHits.Load(); got != int64(i) {
			t.Fatalf("affinity hits #%d = %d, want %d", i, got, i)
		}
	}
}

// newBreakerStateForCounters 以显式阈值复用既有状态构造（与 target_breaker_test 同口径）。
func newBreakerStateForCounters(a *targetState, t *testing.T, threshold int) *targetState {
	t.Helper()
	if a == nil {
		t.Fatal("nil base state")
	}
	a.breakerThreshold = threshold
	return a
}
