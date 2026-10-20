package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Credential additions detach target health from old in-flight requests while
// preserving shared occupancy, runtime observations and surviving key feedback.
func TestReloadPreservesInflightOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "rate-limit"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := &config.Config{Targets: []config.Target{{Name: "a", Endpoint: "http://example.invalid", EndpointType: "openai", APIKey: "one", APIKeys: []string{"two"}}}}
			svc, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			old := svc.targetsByName["a"]
			old.MarkFailure(time.Now().Add(-time.Minute), time.Second)
			decision, lease := old.admitWithProbeState(time.Now())
			if decision != admitOpen || lease == nil {
				t.Fatal("probe not admitted")
			}
			if !old.acquireSlot() {
				t.Fatal("probe did not acquire target slot")
			}
			for i := 0; i < 3; i++ {
				cfg.Targets[0].APIKeys = append(cfg.Targets[0].APIKeys, "added"+strings.Repeat("x", i))
				if err := svc.ApplyConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			current := svc.targetsByName["a"]
			// #184: a credential change is a new connection identity. The pool is
			// rebuilt (key runtime shared by key value), occupancy is shared, but the
			// target breaker starts fresh and is detached from the old generation.
			if current.keyPool == old.keyPool || current.targetHealthState == old.targetHealthState || current.inFlight != old.inFlight || current.runtimeWindow != old.runtimeWindow {
				t.Fatal("credential reload must replace pool and breaker but share target resources")
			}
			if current.probeSlotBusy(time.Now()) || current.Phase(time.Now()) != phaseReady || current.inFlightCount() != 1 {
				t.Fatal("identity reload must start ready while keeping target occupancy")
			}
			if current.Stats().TotalFailure != 1 {
				t.Fatal("identity reload must keep cumulative counters")
			}
			switch outcome {
			case "success":
				old.MarkSuccess(time.Now())
				old.releaseProbe(lease)
				if current.Phase(time.Now()) != phaseReady || current.probeSlotBusy(time.Now()) {
					t.Fatal("old-generation success changed current target")
				}
			case "failure":
				old.MarkFailure(time.Now(), time.Minute)
				old.releaseProbe(lease)
				if current.Phase(time.Now()) != phaseReady || current.probeSlotBusy(time.Now()) || old.Phase(time.Now()) != phaseMuted {
					t.Fatal("old-generation late failure must not mute the new identity")
				}
			case "rate-limit":
				old.keyPool.markRateLimited(0)
				old.releaseProbe(lease)
				if !reloadKey(current.keyPool, 0).status.Exhausted {
					t.Fatal("in-flight key cooldown lost on config reload")
				}
			}
			old.releaseSlot()
			if current.inFlightCount() != 0 {
				t.Fatal("old probe failed to release current target occupancy")
			}
		})
	}
}

// Reproduce the actual HTTP/config overlap using barriers, without timing sleeps.
func TestReloadDuringHalfOpenHTTPProbe(t *testing.T) {
	for _, change := range []string{"reorder-keys", "append-key"} {
		for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("%s/%d", change, status), func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						close(entered)
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
					} else {
						_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
					}
				}))
				defer upstream.Close()
				defer unblock()
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "a", Endpoint: upstream.URL, EndpointType: "openai", APIKeys: []string{"upstream-secret", "second"}}}}
				svc, err := NewService(cfg, newTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				defer svc.Close()
				old := svc.targetsByName["a"]
				old.MarkFailure(time.Now().Add(-time.Minute), time.Second)
				done := make(chan struct{})
				rr := httptest.NewRecorder()
				principal := newAllowAllPrincipal(t, "probe-client")
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"OK"}]}`))
				req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
				go func() { defer close(done); svc.ServeHTTP(rr, req) }()
				defer func() { unblock(); <-done }()
				select {
				case <-entered:
				case <-done:
					t.Fatalf("probe rejected: %d %s", rr.Code, rr.Body.String())
				case <-time.After(3 * time.Second):
					t.Fatal("probe never reached upstream")
				}
				lease := old.probeLease.Load()
				if lease == nil || old.inFlightCount() != 1 {
					t.Fatal("in-flight request did not hold probe and capacity")
				}
				cfg.Targets[0].APIKeys = []string{"second", "upstream-secret"}
				if change == "append-key" {
					cfg.Targets[0].APIKeys = append(cfg.Targets[0].APIKeys, "third")
				}
				if err := svc.ApplyConfig(cfg); err != nil {
					t.Fatal(err)
				}
				current := svc.targetsByName["a"]
				if change == "reorder-keys" {
					if current.targetHealthState != old.targetHealthState || current.probeLease.Load() != lease {
						t.Fatal("key reordering detached live probe health")
					}
				} else if current.targetHealthState == old.targetHealthState || current.probeLease.Load() != nil || current.Phase(time.Now()) != phaseReady {
					t.Fatal("added key did not publish fresh health")
				}
				unblock()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("probe did not finish")
				}
				wantPhase := phaseReady
				wantFailures := int64(1)
				wantSuccesses := int64(0)
				if change == "reorder-keys" {
					if status == http.StatusServiceUnavailable {
						wantPhase = phaseMuted
						wantFailures++
					} else {
						wantSuccesses++
					}
				}
				stats := current.Stats()
				if rr.Code != status || calls.Load() != 1 || current.Phase(time.Now()) != wantPhase || stats.TotalFailure != wantFailures || stats.TotalSuccess != wantSuccesses || current.probeSlotBusy(time.Now()) || current.inFlightCount() != 0 {
					t.Fatalf("status=%d calls=%d phase=%s stats=%+v inFlight=%d", rr.Code, calls.Load(), current.Phase(time.Now()), stats, current.inFlightCount())
				}
			})
		}
	}
}

func TestReloadBindsAdmissionToSurvivingPool(t *testing.T) {
	cfg := &config.Config{Targets: []config.Target{{Name: "a", Endpoint: "http://example.invalid", EndpointType: "openai", APIKeys: []string{"one", "two"}}}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	old := svc.targetsByName["a"].keyPool
	svc.SetAdmissionManager(admission.NewManager(newTestLogger()))
	if err := svc.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	old.mu.Lock()
	bound := old.onKeyChange != nil
	old.mu.Unlock()
	if !bound {
		t.Fatal("surviving pool lost admission callback")
	}
	// Reordering credentials must not reuse an index-bearing pool.
	cfg.Targets[0].APIKeys = []string{"two", "one"}
	if err := svc.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	current := svc.targetsByName["a"].keyPool
	old.markRateLimited(0)
	if current == old || reloadKey(current, 0).status.Exhausted {
		t.Fatal("old key index contaminated reordered credentials")
	}
	if !reloadKey(current, 1).status.Exhausted {
		t.Fatal("old index 0 feedback did not reach same key at new index 1")
	}
}
