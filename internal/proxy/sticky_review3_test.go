package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Review round 3 on the wait, the probe gates and the sticky verdict.

// N2: a model cooldown set because every key is limited is not an upstream
// answer: it must not recover a half-open target or clear its counters.
func TestSetModelCooldownDoesNotRecoverHalfOpenTarget(t *testing.T) {
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	st.MarkFailure(now.Add(-time.Hour), time.Nanosecond) // cooldown over, failure streak 1: half-open
	st.mu.Lock()
	st.consecutiveSentTimeouts = 2
	st.mu.Unlock()
	if st.Phase(now) != phaseHalfOpen {
		t.Fatalf("setup phase = %s", st.Phase(now))
	}
	before := st.Stats()
	if !st.setModelCooldown(now, "m", 5*time.Second, 429, "upstream_429") {
		t.Fatal("setModelCooldown refused")
	}
	if got := st.Phase(now); got != phaseHalfOpen {
		t.Fatalf("phase = %s, want half-open: no upstream answered", got)
	}
	after := st.Stats()
	if after.ConsecutiveFailure != before.ConsecutiveFailure || after.TotalFailure != before.TotalFailure {
		t.Fatalf("failure counters moved: %+v -> %+v", before, after)
	}
	st.mu.RLock()
	timeouts := st.consecutiveSentTimeouts
	st.mu.RUnlock()
	if timeouts != 2 {
		t.Fatalf("sent-timeout streak = %d, want 2 untouched", timeouts)
	}
	// A real 429 answer still recovers the half-open target.
	st2 := newTargetState(&Target{Name: "t2"}, newTestLogger())
	st2.MarkFailure(now.Add(-time.Hour), time.Nanosecond)
	(&Service{quietPeriod: time.Minute}).markTargetFailure(st2, now, 429, nil, false, "m")
	if st2.Phase(now) != phaseReady {
		t.Fatalf("a 429 answer must recover the target, phase = %s", st2.Phase(now))
	}
}

// N4: a request that meets a probe in flight (no wait before) polls for it; the
// poll is its one wait.
func TestProbeInFlightIsPolledEvenWithoutAPriorWait(t *testing.T) {
	env := newIncidentEnv(t, "", http.StatusBadRequest)
	st := env.state("bailian")
	env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
	expireModelLimit(t, st, incidentQwenUpstream)
	release := make(chan struct{})
	env.bailian.setRespond(func(string) (int, http.Header, string) {
		<-release
		return 0, nil, ""
	})
	probe := make(chan int, 1)
	go func() { probe <- env.do("qwen-max", "").Code }()
	deadline := time.Now().Add(3 * time.Second)
	for env.bailian.hitsFor(incidentQwenUpstream) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	start := time.Now()
	rr := env.do("qwen-max", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("second request status = %d after %v, want 200 once the probe succeeded", rr.Code, time.Since(start))
	}
	if code := <-probe; code != http.StatusOK {
		t.Fatalf("probe status = %d", code)
	}
	if m := env.svc.MetricsSnapshot(); m.TotalInProxyWait != 1 || m.TotalInProxyWaitSuccess != 1 {
		t.Fatalf("waits=%d success=%d, want the poll counted as the request's one wait", m.TotalInProxyWait, m.TotalInProxyWaitSuccess)
	}
}

// N4: the model's probe that gets any non-429 answer ends the model entry, like
// a reachable answer ends a target-level probe.
func TestModelProbeNonRateLimitAnswerClearsTheEntry(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			env := newIncidentEnv(t, "", http.StatusBadRequest)
			env.bailian.setRespond(func(string) (int, http.Header, string) {
				return status, nil, `{"error":{"message":"x","type":"invalid_request_error"}}`
			})
			st := env.state("bailian")
			env.svc.markTargetFailure(st, time.Now(), 429, nil, false, incidentQwenUpstream)
			expireModelLimit(t, st, incidentQwenUpstream)
			if rr := env.do("qwen-max", ""); rr.Code != status {
				t.Fatalf("probe request status = %d", rr.Code)
			}
			st.mu.RLock()
			_, kept := st.modelLimits[incidentQwenUpstream]
			st.mu.RUnlock()
			if kept {
				t.Fatal("the model entry must end with a non-429 answer")
			}
			if adm, _, p := st.admitModel("qwen-max", time.Now()); adm != modelOpen || p != nil {
				t.Fatalf("the next request is still gated: %v probe=%v", adm, p)
			}
		})
	}
}

// N8: any bound target that answered this request ends the hold, whichever
// spelling came first.
func TestStickyVerdictEndsWhenAnyBoundTargetAnswered(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.limitModel("bailian", incidentDeepseekUpstream, 60*time.Second)
	now := time.Now()
	bound := []string{"bailian", "official"}
	if _, pending := env.svc.stickyVerdictPending(bound, false, nil, nil, nil, "deepseek", "/v1/chat/completions", now); !pending {
		t.Fatal("setup: the cooling bound target should hold the request")
	}
	answered := map[string]struct{}{"official": {}}
	if v, pending := env.svc.stickyVerdictPending(bound, false, nil, nil, answered, "deepseek", "/v1/chat/completions", now); pending {
		t.Fatalf("a bound target already answered: its verdict stands, got a hold on %s", v.state.Target().Name)
	}
}

// N8: Retry-After is the earlier of the sticky target's recovery and the
// earliest recovery of the other candidates.
func TestStickyVerdictRetryAfterIsTheEarlierRecovery(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest, func(cfg *config.Config) {
		cfg.Targets = append(cfg.Targets, config.Target{Name: "third", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k-t",
			ModelMappings: []config.ModelMapping{{Upstream: "ds-third", Fallback: "deepseek"}}})
	})
	env.bind("sess-ra")
	third := env.state("third")
	third.MarkFailureFor(time.Now(), 20*time.Second, 502, "")
	third.lastEarlyProbe.Store(time.Now().UnixNano()) // no early probe: the all-cooling answer is under test
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	rr := env.do("deepseek", "sess-ra")
	ra, _ := strconv.Atoi(rr.Header().Get("Retry-After"))
	if rr.Code != http.StatusServiceUnavailable || ra < 15 || ra > 21 {
		t.Fatalf("status=%d Retry-After=%q, want the earlier recovery (~20s) not the sticky target's 90s", rr.Code, rr.Header().Get("Retry-After"))
	}
}

// Weak-test replacement: a request that held a model probe on a target that
// then failed before sending must not keep the lease while it waits.
func TestInProxyWaitReleasesModelProbeBeforeWaiting(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest, func(cfg *config.Config) {
		cfg.Targets = append(cfg.Targets, config.Target{Name: "third", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k-t",
			ModelMappings: []config.ModelMapping{{Upstream: "ds-third", Fallback: "deepseek"}}})
	})
	env.bind("sess-lease")
	third := env.state("third")
	third.ClearMute()
	env.svc.markTargetFailure(third, time.Now(), 429, nil, false, "ds-third")
	expireModelLimit(t, third, "ds-third") // third's next deepseek request is its model probe
	env.limitModel("bailian", incidentDeepseekUpstream, 700*time.Millisecond)
	done := make(chan int, 1)
	go func() { done <- env.do("deepseek", "sess-lease").Code }()
	waitForWaiters(t, env.svc, 1)
	// The request took third's probe, failed to reach it (connection refused) and
	// is now waiting for its sticky target.
	third.mu.RLock()
	probe := third.modelLimits["ds-third"].probe
	third.mu.RUnlock()
	if probe != nil || third.probeLease.Load() != nil || third.inFlightCount() != 0 {
		t.Fatalf("the waiting request still holds third's model probe=%v lease=%v inFlight=%d", probe, third.probeLease.Load(), third.inFlightCount())
	}
	if env.official.hitsFor(incidentOfficialUpstream) != 1 {
		t.Fatal("setup: the request should have tried official too")
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the recovered sticky target", code)
	}
}

// Service-level: after the wait the request goes back to the sticky target even
// when another candidate recovered meanwhile and would win the load balancer.
func TestStickyTargetIsTriedFirstAfterWaitEndToEnd(t *testing.T) {
	var thirdHits int
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"third","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(third.Close)
	// third comes first in the target order, so the load balancer would hand it the
	// half-open probe as soon as it is admissible.
	env := newIncidentEnv(t, "120", http.StatusBadRequest, func(cfg *config.Config) {
		cfg.Targets = append([]config.Target{{Name: "third", EndpointType: "openai", Endpoint: third.URL, APIKey: "k-t",
			ModelMappings: []config.ModelMapping{{Upstream: "ds-third", Fallback: "deepseek"}}}}, cfg.Targets...)
	})
	env.svc.affinity.SetWithTTL(sessionAffinityKey("tester", "sess-first", "deepseek"), "bailian", time.Now(), sessionAffinityTTL)
	thirdState := env.state("third")
	thirdState.lastEarlyProbe.Store(time.Now().UnixNano())
	// third is down for a moment and recovers before the sticky target does.
	thirdState.MarkFailureFor(time.Now(), 150*time.Millisecond, 502, "")
	// The sticky target is cooling at target level (no model probe of its own).
	env.state("bailian").MarkFailureFor(time.Now(), 400*time.Millisecond, 502, "")
	env.state("bailian").lastEarlyProbe.Store(time.Now().UnixNano())
	rr := env.do("deepseek", "sess-first")
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "bailian" {
		t.Fatalf("status=%d target=%q, want the request back on the sticky bailian", rr.Code, rr.Header().Get("X-Proxy-Target"))
	}
	if thirdHits != 0 {
		t.Fatalf("third served %d requests, want 0", thirdHits)
	}
}
