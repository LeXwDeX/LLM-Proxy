package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// C: the platform waits for a briefly unavailable sticky target instead of
// letting the client see a substitute's 4xx.

func TestInProxyWaitReturnsStickyTargetSuccessUnnoticed(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	events := &usageEvents{}
	env.svc.SetUsageRecorder(events)
	env.bind("sess-ds")
	if events.count() != 1 {
		t.Fatalf("bind usage events = %d", events.count())
	}
	// The deepseek model of bailian cools briefly (a 429 with a short window).
	env.limitModel("bailian", incidentDeepseekUpstream, 300*time.Millisecond)

	start := time.Now()
	rr := env.do("deepseek", "sess-ds")
	elapsed := time.Since(start)
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("status=%d target=%q body=%s, want 200 from the sticky bailian", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("returned after %v: the request did not wait for the recovery", elapsed)
	}
	if got := events.count(); got != 2 {
		t.Fatalf("usage events = %d, want exactly one more (the bailian success)", got)
	}
	// The substitute was tried once and its 400 never reached the client.
	if got := env.official.hitsFor(incidentOfficialUpstream); got != 1 {
		t.Fatalf("official hits = %d, want 1", got)
	}
	m := env.svc.MetricsSnapshot()
	if m.TotalInProxyWait != 1 || m.TotalInProxyWaitSuccess != 1 || m.TotalInProxyWaitRejected != 0 {
		t.Fatalf("wait metrics = %d/%d/%d, want 1/1/0", m.TotalInProxyWait, m.TotalInProxyWaitSuccess, m.TotalInProxyWaitRejected)
	}
	if m.TotalStickyRetryHinted != 0 || m.TotalCachedErrorReplayed != 0 {
		t.Fatalf("hinted=%d replayed=%d, want 0/0", m.TotalStickyRetryHinted, m.TotalCachedErrorReplayed)
	}
	// The session binding is the original one.
	if target, _ := env.svc.affinity.Get(sessionAffinityKey("tester", "sess-ds", "deepseek"), time.Now()); target != "bailian" {
		t.Fatalf("session binding = %q, want bailian", target)
	}
}

func TestInProxyWaitSkippedWhenRecoveryBeyondBound(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ds")
	env.limitModel("bailian", incidentDeepseekUpstream, 45*time.Second)
	start := time.Now()
	rr := env.do("deepseek", "sess-ds")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a recovery beyond %v must not be waited for (took %v)", inProxyWaitMax, time.Since(start))
	}
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d Retry-After=%q, want the sticky retry hint", rr.Code, rr.Header().Get("Retry-After"))
	}
	if m := env.svc.MetricsSnapshot(); m.TotalInProxyWait != 0 {
		t.Fatalf("in-proxy waits = %d, want 0", m.TotalInProxyWait)
	}
}

func TestInProxyWaitWithoutStickyWaitsWhenNothingCached(t *testing.T) {
	// qwen-max has a single target, no replacement and no cached 4xx: the
	// plain "all unavailable" case waits for the model cooldown too.
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 200*time.Millisecond)
	env.bailian.setRespond(nil) // the upstream recovered
	start := time.Now()
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusOK || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("status=%d after %v, want 200 after waiting out the cooldown", rr.Code, time.Since(start))
	}
}

func TestInProxyWaitClientCancelStopsWithoutUpstream(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 10*time.Second)
	before := env.bailian.total() + env.official.total()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- env.doCtx(ctx, "qwen-max", "", nil).Code }()
	waitForWaiters(t, env.svc, 1)
	cancel()
	select {
	case code := <-done:
		if code != StatusClientClosedRequest {
			t.Fatalf("status = %d, want 499", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end the wait")
	}
	if got := env.bailian.total() + env.official.total(); got != before {
		t.Fatalf("upstream hits %d -> %d: a cancelled wait must not reach any upstream", before, got)
	}
	if got := env.svc.inProxyWaiting.Load(); got != 0 {
		t.Fatalf("wait slots leaked: %d", got)
	}
	if env.svc.MetricsSnapshot().TotalClientCanceled != 1 {
		t.Fatal("client cancel not settled as 499")
	}
}

func TestInProxyWaitSlotsExhaustedFailFast(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 10*time.Second)
	env.svc.inProxyWaiting.Store(inProxyWaitSlots)
	start := time.Now()
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusServiceUnavailable || time.Since(start) > time.Second {
		t.Fatalf("status=%d after %v, want an immediate 503", rr.Code, time.Since(start))
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 lacks Retry-After")
	}
	m := env.svc.MetricsSnapshot()
	if m.TotalInProxyWaitRejected != 1 || m.TotalInProxyWait != 0 {
		t.Fatalf("rejected=%d waits=%d, want 1/0", m.TotalInProxyWaitRejected, m.TotalInProxyWait)
	}
	if got := env.svc.inProxyWaiting.Load(); got != inProxyWaitSlots {
		t.Fatalf("a rejected request changed the slot count to %d", got)
	}
}

func TestInProxyWaitAtMostOncePerRequest(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 200*time.Millisecond)
	// The cooldown is extended while the request waits, so the re-selection
	// fails again: the request must not wait a second time.
	go func() {
		time.Sleep(60 * time.Millisecond)
		env.state("bailian").mu.Lock()
		ml := env.state("bailian").modelLimits[incidentQwenUpstream]
		ml.until = time.Now().Add(20 * time.Second)
		env.state("bailian").modelLimits[incidentQwenUpstream] = ml
		env.state("bailian").mu.Unlock()
	}()
	start := time.Now()
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after the single wait", rr.Code)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v: the request waited more than once", time.Since(start))
	}
	if m := env.svc.MetricsSnapshot(); m.TotalInProxyWait != 1 || m.TotalInProxyWaitSuccess != 0 {
		t.Fatalf("waits=%d success=%d, want 1/0", m.TotalInProxyWait, m.TotalInProxyWaitSuccess)
	}
	if ra, _ := strconv.Atoi(rr.Header().Get("Retry-After")); ra < 15 {
		t.Fatalf("Retry-After = %q, want the extended cooldown", rr.Header().Get("Retry-After"))
	}
}

func TestInProxyWaitHoldsNoCapacityOrProbe(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 400*time.Millisecond)
	done := make(chan struct{})
	go func() { env.do("qwen-max", ""); close(done) }()
	waitForWaiters(t, env.svc, 1)
	for _, name := range []string{"bailian", "official"} {
		st := env.state(name)
		if st.inFlightCount() != 0 || st.probeLease.Load() != nil {
			t.Fatalf("%s holds inFlight=%d probe=%v while a request waits", name, st.inFlightCount(), st.probeLease.Load())
		}
	}
	<-done
	for _, name := range []string{"bailian", "official"} {
		if env.state(name).inFlightCount() != 0 {
			t.Fatalf("%s inFlight leaked", name)
		}
	}
}

func TestInProxyWaitWakeupGets429Back(t *testing.T) {
	// After waking, the target is tried again; if it still rate limits, the
	// client gets that 429 (not a 503, not a substitute's 4xx).
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 150*time.Millisecond)
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's 429 after the wait", rr.Code)
	}
	if got := env.bailian.hitsFor(incidentQwenUpstream); got != 1 {
		t.Fatalf("qwen upstream hits = %d, want 1 (only the retry after waking)", got)
	}
}

func TestInProxyWaitStreamingWritesNothingBeforeWake(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 250*time.Millisecond)
	env.bailian.setRespond(nil)
	rec := newTimedRecorder()
	start := time.Now()
	r := newStreamRequest(env, "qwen-max")
	env.svc.ServeHTTP(rec, r)
	if first := rec.firstWrite(); first.IsZero() || first.Sub(start) < 200*time.Millisecond {
		t.Fatalf("first byte written %v after start: the stream opened before the wake-up", first.Sub(start))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestInProxyWaitJitterBounded(t *testing.T) {
	s := &Service{}
	for i := 0; i < 200; i++ {
		if j := s.inProxyWaitJitter(); j < 0 || j > inProxyWaitJitterMax {
			t.Fatalf("jitter %v outside [0, %v]", j, inProxyWaitJitterMax)
		}
	}
	s.inProxyWaitJitterOverride = -1
	if s.inProxyWaitJitter() != 0 {
		t.Fatal("negative override must disable jitter")
	}
}

func waitForWaiters(t *testing.T, s *Service, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.inProxyWaiting.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("no request reached the in-proxy wait (waiting=%d)", s.inProxyWaiting.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newStreamRequest(env *incidentEnv, model string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "192.0.2.7:4321"
	return r.WithContext(withPrincipalCtx(env))
}

// The wait holds nothing, whatever the request did before it: not after a
// substitute target was already tried, not with a model probe or a key's
// in-flight count.
func TestInProxyWaitHoldsNothingAfterSubstituteWasTried(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-hold")
	env.limitModel("bailian", incidentDeepseekUpstream, 600*time.Millisecond)
	done := make(chan int, 1)
	go func() { done <- env.do("deepseek", "sess-hold").Code }()
	waitForWaiters(t, env.svc, 1)
	// The substitute (official) answered with a 400 and was dequeued before the wait.
	if got := env.official.hitsFor(incidentOfficialUpstream); got != 1 {
		t.Fatalf("official hits = %d, want the substitute tried once before waiting", got)
	}
	for _, name := range []string{"bailian", "official"} {
		st := env.state(name)
		if st.inFlightCount() != 0 || st.probeLease.Load() != nil {
			t.Fatalf("%s holds inFlight=%d probe=%v while a request waits", name, st.inFlightCount(), st.probeLease.Load())
		}
		st.mu.RLock()
		for model, ml := range st.modelLimits {
			if ml.probe != nil {
				st.mu.RUnlock()
				t.Fatalf("%s holds a model probe for %s while a request waits", name, model)
			}
		}
		st.mu.RUnlock()
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the wait", code)
	}
}

// A parent deadline that ends during the wait settles as a 504 failure, not a
// cancel, and reaches no upstream.
func TestInProxyWaitParentDeadlineIs504(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	env.limitModel("bailian", incidentQwenUpstream, 10*time.Second)
	before := env.bailian.total() + env.official.total()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rr := env.doCtx(ctx, "qwen-max", "", nil)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 when the parent deadline ends the wait", rr.Code)
	}
	if got := env.bailian.total() + env.official.total(); got != before {
		t.Fatalf("upstream hits %d -> %d during a wait", before, got)
	}
	if env.svc.inProxyWaiting.Load() != 0 {
		t.Fatal("slot leaked")
	}
	if m := env.svc.MetricsSnapshot(); m.TotalClientCanceled != 0 {
		t.Fatalf("a deadline is not a client cancel (canceled=%d)", m.TotalClientCanceled)
	}
}
