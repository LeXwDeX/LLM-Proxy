package proxy

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The model-level half-open gate: when a model cooldown ends, a single request
// probes the model; the others see "probe in flight" until it settles.

// expireModelLimit ends the cooldown of the model but keeps its entry (streak),
// as time passing does.
func expireModelLimit(t *testing.T, st *targetState, model string) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	ml, ok := st.modelLimits[model]
	if !ok {
		t.Fatalf("no model limit for %s", model)
	}
	ml.until = time.Now().Add(-time.Second)
	st.modelLimits[model] = ml
}

func TestModelProbeGateAdmitsOneRequest(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
	if adm, _, _ := st.admitModel("qwen-max", time.Now()); adm != modelCooling {
		t.Fatalf("running cooldown admission = %v, want cooling", adm)
	}
	expireModelLimit(t, st, incidentQwenUpstream)
	adm, _, probe := st.admitModel("qwen-max", time.Now())
	if adm != modelOpen || probe == nil {
		t.Fatalf("first request after the cooldown: %v probe=%v, want the probe", adm, probe)
	}
	if adm, _, p2 := st.admitModel("qwen-max", time.Now()); adm != modelProbeBusy || p2 != nil {
		t.Fatalf("second request: %v, want probe in flight", adm)
	}
	if view, _ := st.modelView("qwen-max", time.Now()); view != modelProbeBusy {
		t.Fatalf("read-only view = %v", view)
	}
	// Another model of the target is never gated.
	if adm, _, p := st.admitModel("deepseek", time.Now()); adm != modelOpen || p != nil {
		t.Fatalf("other model admission = %v probe=%v", adm, p)
	}
	st.releaseModelProbe(probe)
	if adm, _, p := st.admitModel("qwen-max", time.Now()); adm != modelOpen || p == nil {
		t.Fatalf("after the lease was released the next request probes again: %v", adm)
	}
}

func TestModelProbeLeaseExpires(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
	expireModelLimit(t, st, incidentQwenUpstream)
	_, _, probe := st.admitModel("qwen-max", time.Now())
	if probe == nil {
		t.Fatal("no probe")
	}
	// An abandoned lease must not block the model forever.
	if adm, _, p := st.admitModel("qwen-max", time.Now().Add(breakerProbeLease+time.Second)); adm != modelOpen || p == nil {
		t.Fatalf("an expired lease must be replaced: %v", adm)
	}
}

// All three admission points: sticky hit, the ranked selector and the explicit target.
func TestModelProbeGateAtEveryAdmissionPoint(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	st := env.state("bailian")
	prime := func() {
		env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
		expireModelLimit(t, st, incidentQwenUpstream)
	}
	for _, mode := range []string{"ranked", "sticky", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			st.clearModelLimit(incidentQwenUpstream)
			prime()
			sessionBound, explicit := "", ""
			switch mode {
			case "sticky":
				sessionBound = "bailian"
			case "explicit":
				explicit = "bailian"
			}
			sel := func() (*targetState, selectionTrace, error) {
				trace := selectionTrace{}
				state, lease, _, err := env.svc.selectTargetTraced(env.principal, "192.0.2.7", sessionBound, explicit, nil, nil, "qwen-max", "/v1/chat/completions", time.Now(), &trace)
				if state != nil {
					state.releaseProbe(lease)
				}
				return state, trace, err
			}
			first, trace, err := sel()
			if err != nil || first == nil || trace.modelProbe == nil {
				t.Fatalf("first request: state=%v probe=%v err=%v, want the model probe", first, trace.modelProbe, err)
			}
			second, _, err := sel()
			if err == nil || second != nil {
				t.Fatalf("second request must see the probe in flight, got %v", second)
			}
			if mode != "explicit" {
				var selErr *selectionError
				if !asSelectionError(err, &selErr) || !selErr.ProbeInFlight {
					t.Fatalf("selection error = %#v, want ProbeInFlight", err)
				}
			}
			first.releaseModelProbe(trace.modelProbe)
			if again, tr, err := sel(); err != nil || again == nil || tr.modelProbe == nil {
				t.Fatalf("after release the next request probes: %v %v", again, err)
			}
		})
	}
}

func asSelectionError(err error, out **selectionError) bool {
	se, ok := err.(*selectionError)
	if ok {
		*out = se
	}
	return ok
}

// Settlement: success clears the model; a 429 cools it again; any other upstream
// answer ends the entry too (reachable, no longer rate limited); a cancel releases
// the lease so the next request probes.
func TestModelProbeSettlement(t *testing.T) {
	t.Run("success clears", func(t *testing.T) {
		env := newIncidentEnv(t, "", http.StatusBadRequest)
		env.bailian.setRespond(nil)
		st := env.state("bailian")
		env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
		expireModelLimit(t, st, incidentQwenUpstream)
		if rr := env.do("qwen-max", ""); rr.Code != http.StatusOK {
			t.Fatalf("probe request status = %d", rr.Code)
		}
		st.mu.RLock()
		_, kept := st.modelLimits[incidentQwenUpstream]
		st.mu.RUnlock()
		if kept {
			t.Fatal("a successful probe must clear the model entry")
		}
	})
	t.Run("429 re-cools", func(t *testing.T) {
		env := newIncidentEnv(t, "120", http.StatusBadRequest)
		st := env.state("bailian")
		env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
		expireModelLimit(t, st, incidentQwenUpstream)
		if rr := env.do("qwen-max", ""); rr.Code != http.StatusTooManyRequests {
			t.Fatalf("probe request status = %d, want the upstream 429", rr.Code)
		}
		if adm, until := st.modelView("qwen-max", time.Now()); adm != modelCooling || time.Until(until) < 100*time.Second {
			t.Fatalf("after a 429 probe: %v until %v, want a fresh cooldown", adm, until)
		}
		st.mu.RLock()
		streak := st.modelLimits[incidentQwenUpstream].streak
		st.mu.RUnlock()
		if streak != 2 {
			t.Fatalf("streak = %d, want 2 (the probe's 429 counts)", streak)
		}
	})
	t.Run("a non-429 answer ends the model entry", func(t *testing.T) {
		env := newIncidentEnv(t, "", http.StatusBadRequest)
		env.bailian.setRespond(func(string) (int, http.Header, string) {
			return http.StatusBadRequest, nil, `{"error":{"message":"bad","type":"invalid_request_error"}}`
		})
		st := env.state("bailian")
		env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
		expireModelLimit(t, st, incidentQwenUpstream)
		if rr := env.do("qwen-max", ""); rr.Code != http.StatusBadRequest {
			t.Fatalf("probe request status = %d", rr.Code)
		}
		// The model answered: it is reachable and no longer gated.
		if adm, _, p := st.admitModel("qwen-max", time.Now()); adm != modelOpen || p != nil {
			t.Fatalf("after a 400 the model must be open without a probe: %v probe=%v", adm, p)
		}
	})
	t.Run("cancel releases the lease", func(t *testing.T) {
		env := newIncidentEnv(t, "", http.StatusBadRequest)
		release := make(chan struct{})
		env.bailian.setRespond(func(string) (int, http.Header, string) {
			<-release
			return 0, nil, ""
		})
		t.Cleanup(func() { close(release) })
		st := env.state("bailian")
		env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
		expireModelLimit(t, st, incidentQwenUpstream)
		ctx, cancel := contextWithCancel()
		done := make(chan struct{})
		go func() { env.doCtx(ctx, "qwen-max", "", nil); close(done) }()
		deadline := time.Now().Add(3 * time.Second)
		for st.inFlightCount() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if view, _ := st.modelView("qwen-max", time.Now()); view != modelProbeBusy {
			t.Fatalf("in flight: view = %v, want probe busy", view)
		}
		cancel()
		<-done
		if view, _ := st.modelView("qwen-max", time.Now()); view != modelOpen {
			t.Fatalf("after cancel: view = %v, want the lease released", view)
		}
	})
}

// N waiters wake together: one probes while the others wait for it, so the
// upstream never sees two requests of the model while the probe is in flight,
// and everyone is served afterwards.
func TestInProxyWaitConcurrentWaitersRace(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 150*time.Millisecond)
	var arrivals, violations atomic.Int32
	var firstInFlight atomic.Bool
	env.bailian.setRespond(func(string) (int, http.Header, string) {
		if arrivals.Add(1) == 1 {
			firstInFlight.Store(true)
			time.Sleep(400 * time.Millisecond)
			firstInFlight.Store(false)
			return 0, nil, ""
		}
		if firstInFlight.Load() {
			violations.Add(1)
		}
		return 0, nil, ""
	})
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = env.do("qwen-max", "").Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("waiter %d status = %d, want 200 after the probe settled", i, c)
		}
	}
	if violations.Load() != 0 {
		t.Fatalf("%d requests reached the upstream while the probe was in flight, want 0", violations.Load())
	}
	if env.svc.inProxyWaiting.Load() != 0 {
		t.Fatal("slots leaked")
	}
}

// A probe that takes longer than the short polling bound ends the waiters with a
// 503 and Retry-After instead of letting them hang.
func TestInProxyWaitProbePollingIsBounded(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 300*time.Millisecond)
	release := make(chan struct{})
	env.bailian.setRespond(func(string) (int, http.Header, string) {
		<-release
		return 0, nil, ""
	})
	type result struct {
		code       int
		retryAfter string
		elapsed    time.Duration
	}
	results := make(chan result, 2)
	start := time.Now()
	for i := 0; i < 2; i++ {
		go func() {
			rr := env.do("qwen-max", "")
			results <- result{rr.Code, rr.Header().Get("Retry-After"), time.Since(start)}
		}()
	}
	// Both wake together: one probes (blocked upstream), the other polls for it.
	got := <-results
	close(release)
	probe := <-results
	if got.code != http.StatusServiceUnavailable || got.retryAfter == "" {
		t.Fatalf("polling waiter: status=%d Retry-After=%q, want 503 with Retry-After", got.code, got.retryAfter)
	}
	if got.elapsed < 3*time.Second || got.elapsed > 5*time.Second {
		t.Fatalf("polling waiter took %v, want about the wait plus the %v polling bound", got.elapsed, inProxyProbeWaitMax)
	}
	if probe.code != http.StatusOK {
		t.Fatalf("probe status = %d", probe.code)
	}
}
