package proxy

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
)

// PR #205 review: how prewritten bindings settle when requests overlap
// (P2-1) and when a response outlives the binding's sliding TTL (P2-2).

type settleFixture struct {
	s       *Service
	p       *auth.Principal
	ipKey   string
	sessKey string
	t0      time.Time
}

func newSettleFixture(t *testing.T) *settleFixture {
	t.Helper()
	up := prewriteUpstream(t, "json", 200, "")
	s, p := newRejectionService(t, rejectionTarget("only", up.URL, "k"))
	ip, sess := prewriteKeys(p)
	return &settleFixture{s: s, p: p, ipKey: ip, sessKey: sess, t0: time.Now()}
}

func (f *settleFixture) inputs(kind selectionKind, bound string) bindingInputs {
	st, _ := f.s.targetByName("only")
	return bindingInputs{
		principal: f.p, target: st.Target(), clientIP: "192.0.2.1",
		selKind: kind, affinityBound: kind == selectionAffinityHit,
		sessionKey: f.sessKey, sessionBoundTarget: bound,
		model: prewriteTestModel, upstreamModel: prewriteTestModel, path: "/v1/chat/completions",
	}
}

// fresh is a request that found no binding; hit one that found A's prewrite.
func (f *settleFixture) fresh() bindingInputs { return f.inputs(selectionRoundRobin, "") }
func (f *settleFixture) hit() bindingInputs   { return f.inputs(selectionAffinityHit, "only") }

func (f *settleFixture) live(key string, at time.Time) bool {
	_, ok := f.s.affinity.Get(key, at)
	return ok
}

// P2-1: A prewrites, B (which found A's prewrite) refreshes it; both fail.
// Neither order may leave the never-delivered binding behind.
func TestOverlappingFailedPrewritesLeaveNoBinding(t *testing.T) {
	for _, aFirst := range []bool{true, false} {
		f := newSettleFixture(t)
		a := f.s.writeBindings(f.fresh(), f.t0)
		b := f.s.writeBindings(f.hit(), f.t0.Add(time.Second))
		end := f.t0.Add(2 * time.Second)
		if aFirst {
			f.s.settlePrewrittenBindings(a, f.fresh(), false, end)
			f.s.settlePrewrittenBindings(b, f.hit(), false, end)
		} else {
			f.s.settlePrewrittenBindings(b, f.hit(), false, end)
			f.s.settlePrewrittenBindings(a, f.fresh(), false, end)
		}
		for _, k := range []string{f.ipKey, f.sessKey} {
			if f.live(k, end) {
				t.Errorf("aFirst=%v: binding %q of two failed requests survived", aFirst, k)
			}
		}
	}
}

// A fails, B (which refreshed A's prewrite) succeeds: B's confirmed binding.
func TestFailedThenSucceededOverlapKeepsBinding(t *testing.T) {
	f := newSettleFixture(t)
	a := f.s.writeBindings(f.fresh(), f.t0)
	b := f.s.writeBindings(f.hit(), f.t0.Add(time.Second))
	end := f.t0.Add(2 * time.Second)
	f.s.settlePrewrittenBindings(a, f.fresh(), false, end)
	f.s.settlePrewrittenBindings(b, f.hit(), true, end)
	for _, k := range []string{f.ipKey, f.sessKey} {
		if got, _ := f.s.affinity.Get(k, end); got != "only" {
			t.Errorf("binding %q = %q, want B's confirmed %q", k, got, "only")
		}
	}
	// Confirmed: a later failed request on top of it takes nothing away.
	c := f.s.writeBindings(f.hit(), end.Add(time.Second))
	f.s.settlePrewrittenBindings(c, f.hit(), false, end.Add(2*time.Second))
	for _, k := range []string{f.ipKey, f.sessKey} {
		if !f.live(k, end.Add(2*time.Second)) {
			t.Errorf("confirmed binding %q removed by a later failed request", k)
		}
	}
}

// A succeeds, B (which refreshed A's prewrite) fails: A's confirmed binding.
func TestSucceededThenFailedOverlapKeepsBinding(t *testing.T) {
	f := newSettleFixture(t)
	a := f.s.writeBindings(f.fresh(), f.t0)
	b := f.s.writeBindings(f.hit(), f.t0.Add(time.Second))
	end := f.t0.Add(2 * time.Second)
	f.s.settlePrewrittenBindings(a, f.fresh(), true, end)
	f.s.settlePrewrittenBindings(b, f.hit(), false, end)
	for _, k := range []string{f.ipKey, f.sessKey} {
		if got, _ := f.s.affinity.Get(k, end); got != "only" {
			t.Errorf("binding %q = %q, want A's confirmed %q", k, got, "only")
		}
	}
}

// P2-2: the response outlived the sliding TTL, so the prewritten binding
// lapsed before settlement. A success rebuilds it at the completion time.
func TestLongResponseSuccessRebuildsLapsedSlidingBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     func(*settleFixture) string
		elapsed time.Duration
	}{
		{"ip", func(f *settleFixture) string { return f.ipKey }, affinityTTL + time.Minute},
		{"session", func(f *settleFixture) string { return f.sessKey }, sessionAffinityTTL + time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSettleFixture(t)
			a := f.s.writeBindings(f.fresh(), f.t0)
			end := f.t0.Add(tc.elapsed)
			f.s.settlePrewrittenBindings(a, f.fresh(), true, end)
			key := tc.key(f)
			got, ok := f.s.affinity.Get(key, end)
			if !ok || got != "only" {
				t.Fatalf("binding after a long successful response = %q (live %v), want %q", got, ok, "only")
			}
			f.s.affinity.mu.RLock()
			last := f.s.affinity.entries[key].lastUsed
			f.s.affinity.mu.RUnlock()
			if !last.Equal(end) {
				t.Fatalf("sliding start = %v, want the completion time %v", last, end)
			}
		})
	}
}

// The same for a hit on an established sliding binding that lapsed in flight
// (recreated only on a preferred tier, which a plain pool always is).
func TestLongResponseSuccessRebuildsLapsedHitBinding(t *testing.T) {
	f := newSettleFixture(t)
	f.s.affinity.Set(f.ipKey, "only", f.t0)
	f.s.affinity.SetWithTTL(f.sessKey, "only", f.t0, sessionAffinityTTL)
	b := f.s.writeBindings(f.hit(), f.t0.Add(time.Second))
	end := f.t0.Add(sessionAffinityTTL + time.Hour)
	f.s.settlePrewrittenBindings(b, f.hit(), true, end)
	for _, k := range []string{f.ipKey, f.sessKey} {
		if got, _ := f.s.affinity.Get(k, end); got != "only" {
			t.Errorf("hit binding %q = %q after a long response, want it rebuilt", k, got)
		}
	}
}

// A temporary binding keeps its absolute expiry: not renewed, not rebuilt.
func TestLongResponseDoesNotReviveTemporaryBinding(t *testing.T) {
	s, p, _ := cheapExpensiveService(t, "cost")
	cheap, _ := s.targetByName("cheap")
	cheap.MarkFailure(time.Now(), time.Hour)
	pricey, _ := s.targetByName("pricey")
	ipKey := affinityKey("192.0.2.1", p.Name, costTestModel)
	sessKey := sessionAffinityKey(p.Name, "sess-temp-long", costTestModel)
	in := bindingInputs{principal: p, target: pricey.Target(), clientIP: "192.0.2.1", selKind: selectionRoundRobin,
		sessionKey: sessKey, model: costTestModel, upstreamModel: costTestModel, path: costTestPath}
	t0 := time.Now()
	undos := s.writeBindings(in, t0)
	if !s.affinity.IsTemporary(ipKey, t0) || !s.affinity.IsTemporary(sessKey, t0) {
		t.Fatal("setup: the overflow bindings are not temporary")
	}
	end := t0.Add(temporarySessionAffinityTTL + time.Minute)
	s.settlePrewrittenBindings(undos, in, true, end)
	for _, k := range []string{ipKey, sessKey} {
		if _, ok := s.affinity.Get(k, end); ok {
			t.Errorf("temporary binding %q outlived its absolute expiry", k)
		}
	}
}

// A binding another request replaced meanwhile is left alone.
func TestLongResponseDoesNotOverwriteReplacedBinding(t *testing.T) {
	f := newSettleFixture(t)
	a := f.s.writeBindings(f.fresh(), f.t0)
	end := f.t0.Add(affinityTTL + time.Minute)
	f.s.affinity.SetWithTTL(f.ipKey, "other", end.Add(-time.Second), 0)
	f.s.settlePrewrittenBindings(a, f.fresh(), true, end)
	if got, _ := f.s.affinity.Get(f.ipKey, end); got != "other" {
		t.Fatalf("ip binding = %q, the replacement was overwritten", got)
	}
}

// Three overlapping requests settle in every order with every outcome: a
// binding survives exactly when at least one of them was delivered in full.
func TestOverlappingSettlementProperty(t *testing.T) {
	perms := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for mask := 0; mask < 8; mask++ {
		for _, order := range perms {
			f := newSettleFixture(t)
			ins := [3]bindingInputs{f.fresh(), f.hit(), f.hit()}
			var undos [3][]affinityUndo
			for i := range ins {
				undos[i] = f.s.writeBindings(ins[i], f.t0.Add(time.Duration(i)*time.Second))
			}
			end := f.t0.Add(10 * time.Second)
			any := false
			for _, i := range order {
				ok := mask&(1<<i) != 0
				any = any || ok
				f.s.settlePrewrittenBindings(undos[i], ins[i], ok, end)
			}
			for _, k := range []string{f.ipKey, f.sessKey} {
				if got := f.live(k, end); got != any {
					t.Errorf("success mask %03b, settle order %v: binding %q live=%v, want %v", mask, order, k, got, any)
				}
			}
		}
	}
}

// Bindings written after delivery (WS and native-WS turns, which never
// prewrite) are already settled: they must not stay tagged as a pending
// prewrite, or every later turn would chain onto the previous one forever.
func TestDeliveredBindingsAreNotLeftPending(t *testing.T) {
	f := newSettleFixture(t)
	at := f.t0
	f.s.writeDeliveredBindings(f.fresh(), at)
	for i := 0; i < 50; i++ {
		at = at.Add(time.Second)
		f.s.writeDeliveredBindings(f.hit(), at)
	}
	f.s.affinity.mu.RLock()
	defer f.s.affinity.mu.RUnlock()
	for _, key := range []string{f.ipKey, f.sessKey} {
		e, ok := f.s.affinity.entries[key]
		if !ok || e.expired(at) || e.targetName != "only" {
			t.Fatalf("%s: binding missing after delivered turns: %+v", key, e)
		}
		if e.prov != nil {
			t.Fatalf("%s: a delivered binding is still a pending prewrite", key)
		}
		if !e.lastUsed.Equal(at) {
			t.Fatalf("%s: lastUsed = %v, want the last delivery %v", key, e.lastUsed, at)
		}
	}
}
