package proxy

import (
	"context"
	"io"
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

// A canceled request can win selection but never contact the upstream. It must
// leave the recovery window available to the next request without health feedback.
func TestCanceledHalfOpenSelectionReleasesProbe(t *testing.T) {
	svc := newSingleTargetService(t)
	defer svc.Close()
	state := svc.targetOrder[0]
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	before := state.Stats()
	principal := newAllowAllPrincipal(t, "probe-client")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	req = req.WithContext(auth.WithPrincipal(ctx, principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != StatusClientClosedRequest {
		t.Fatalf("canceled status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if state.probeSlotBusy(time.Now()) {
		t.Fatal("canceled request retained the half-open probe")
	}
	if state.inFlightCount() != 0 || state.Stats() != before {
		t.Fatalf("cancellation changed occupancy or health: in_flight=%d before=%+v after=%+v", state.inFlightCount(), before, state.Stats())
	}
	if decision, lease := state.admitWithProbeState(time.Now()); decision != admitOpen || lease == nil {
		t.Fatalf("next request cannot probe immediately: decision=%d", decision)
	} else {
		state.releaseProbe(lease)
	}
}

func TestHalfOpenQuotaRejectionReleasesProbe(t *testing.T) {
	svc := newSingleTargetService(t)
	defer svc.Close()
	state := svc.targetOrder[0]
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	before := state.Stats()
	svc.SetQuotaManager(buildQuotaExceededManager(t, "tester"))
	principal := newAllowAllPrincipal(t, "quota-client")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("quota status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if state.probeSlotBusy(time.Now()) || state.Stats() != before {
		t.Fatalf("local quota rejection changed probe or health: busy=%t before=%+v after=%+v", state.probeSlotBusy(time.Now()), before, state.Stats())
	}
}

func TestHalfOpenAdmissionTimeoutReleasesProbe(t *testing.T) {
	svc := newSingleTargetService(t)
	defer svc.Close()
	state := svc.targetOrder[0]
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	before := state.Stats()
	manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 1, RMin: 1, RMax: 1})
	svc.SetAdmissionManager(manager)
	if !manager.TryAcquire(state.Target().Name) {
		t.Fatal("admission fixture did not consume its initial token")
	}
	principal := newAllowAllPrincipal(t, "admission-client")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	req = req.WithContext(auth.WithPrincipal(ctx, principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("admission status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if state.probeSlotBusy(time.Now()) || state.Stats() != before {
		t.Fatalf("admission rejection changed probe or health: busy=%t before=%+v after=%+v", state.probeSlotBusy(time.Now()), before, state.Stats())
	}
}

func TestHalfOpenAllKeysUnavailableReleasesProbe(t *testing.T) {
	svc := newTestModelService(t, config.EndpointTypeOpenAI, "http://unused.invalid", func(target *config.Target) {
		target.APIKeys = []string{"other-key"}
	})
	defer svc.Close()
	state := svc.targetOrder[0]
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	before := state.Stats()
	for i := range state.keyPool.entries {
		state.keyPool.blockKey(i)
	}
	principal := newAllowAllPrincipal(t, "unavailable-client")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("all keys unavailable status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if state.probeSlotBusy(time.Now()) || state.Stats() != before {
		t.Fatalf("all keys unavailable changed probe or health: busy=%t before=%+v after=%+v", state.probeSlotBusy(time.Now()), before, state.Stats())
	}
}

func TestExpiredProbeCannotReleaseSuccessor(t *testing.T) {
	for _, health := range []string{"success", "failure", "none"} {
		t.Run(health, func(t *testing.T) {
			state := newBreakerState(t, 1)
			start := time.Now().Add(-time.Hour)
			state.MarkFailure(start, time.Second)
			firstAt := start.Add(2 * time.Second)
			decision, first := state.admitWithProbeState(firstAt)
			if decision != admitOpen || first == nil {
				t.Fatal("first probe was not admitted")
			}
			// The old request is still running when its lease expires. A new
			// request can take the slot and owns a distinct object.
			secondAt := time.Unix(0, first.until+1)
			decision, second := state.admitWithProbeState(secondAt)
			if decision != admitOpen || second == nil || second == first {
				t.Fatal("successor did not acquire an independent probe lease")
			}
			switch health {
			case "success":
				state.MarkSuccess(secondAt)
			case "failure":
				state.MarkFailure(secondAt, time.Second)
			}
			state.releaseProbe(first)
			state.releaseProbe(nil) // A normal request has no authority over this slot.
			if got := state.probeLease.Load(); got != second {
				t.Fatalf("old/non-probe completion released successor: got=%p want=%p", got, second)
			}
			state.releaseProbe(second)
			if state.probeLease.Load() != nil {
				t.Fatal("successor did not release its own lease")
			}
		})
	}
}

// Non-identity saves must retain the shared probe state and the successor's
// exclusive lease even when the request holding the previous lease completes.
func TestReloadedOldProbeCannotReleaseSuccessor(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "none"} {
		t.Run(outcome, func(t *testing.T) {
			svc := newSingleTargetService(t)
			defer svc.Close()
			old := svc.targetOrder[0]
			start := time.Now().Add(-time.Hour)
			old.MarkFailure(start, time.Second)
			decision, first := old.admitWithProbeState(start.Add(2 * time.Second))
			if decision != admitOpen || first == nil {
				t.Fatal("old generation did not acquire probe")
			}
			cfg := &config.Config{Targets: []config.Target{{Name: "only", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://127.0.0.1:1", APIKey: "k", MaxConcurrency: 3}}}
			if err := svc.ApplyConfig(cfg); err != nil {
				t.Fatal(err)
			}
			current := svc.targetOrder[0]
			if current == old || current.targetHealthState != old.targetHealthState {
				t.Fatal("non-identity reload did not preserve shared probe health")
			}
			secondAt := time.Unix(0, first.until+1)
			decision, second := current.admitWithProbeState(secondAt)
			if decision != admitOpen || second == nil || second == first {
				t.Fatal("new generation did not acquire successor probe")
			}
			switch outcome {
			case "success":
				old.MarkSuccess(secondAt)
			case "failure":
				old.MarkFailure(secondAt, time.Second)
			}
			old.releaseProbe(first)
			if current.probeLease.Load() != second {
				t.Fatal("old generation cleared new generation's probe")
			}
			current.releaseProbe(second)
			if current.probeLease.Load() != nil {
				t.Fatal("successor did not release its own lease")
			}
		})
	}
}

func TestReloadedIdentityChangeDetachesOldProbe(t *testing.T) {
	svc := newSingleTargetService(t)
	defer svc.Close()
	old := svc.targetOrder[0]
	start := time.Now().Add(-time.Hour)
	old.MarkFailure(start, time.Second)
	decision, first := old.admitWithProbeState(start.Add(2 * time.Second))
	if decision != admitOpen || first == nil {
		t.Fatal("old generation did not acquire probe")
	}
	cfg := &config.Config{Targets: []config.Target{{Name: "only", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://127.0.0.1:1", APIKey: "changed-key"}}}
	if err := svc.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	current := svc.targetOrder[0]
	// #184: a changed key is a new identity; the breaker is detached and ready.
	if current == old || current.targetHealthState == old.targetHealthState {
		t.Fatal("identity reload did not detach target breaker state")
	}
	if current.probeLease.Load() != nil || current.Phase(time.Now()) != phaseReady {
		t.Fatal("new identity must start ready without a probe lease")
	}
	secondAt := time.Unix(0, first.until+1)
	old.MarkFailure(secondAt, time.Second)
	old.releaseProbe(first)
	if decision, _ := current.admitWithProbeState(secondAt); decision != admitOpen || current.probeLease.Load() != nil {
		t.Fatal("old generation probe outcome affected the new identity")
	}
}

func TestHalfOpenStreamingHeadersReleaseProbe(t *testing.T) {
	firstHeader, releaseFirst := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if calls.Add(1) == 1 {
			close(firstHeader)
			<-releaseFirst
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	svc := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	defer svc.Close()
	state := svc.targetOrder[0]
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	principal := newAllowAllPrincipal(t, "stream-client")
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","stream":true}`))
		return r.WithContext(auth.WithPrincipal(r.Context(), principal))
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		svc.ServeHTTP(httptest.NewRecorder(), request())
	}()
	defer func() {
		close(releaseFirst)
		<-firstDone
	}()
	select {
	case <-firstHeader:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not reach upstream")
	}
	deadline := time.Now().Add(3 * time.Second)
	for (state.Stats().TotalSuccess == 0 || state.probeSlotBusy(time.Now())) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if state.Stats().TotalSuccess == 0 || state.probeSlotBusy(time.Now()) {
		t.Fatal("stream headers did not settle health and release probe promptly")
	}
	select {
	case <-firstDone:
		t.Fatal("fixture stream ended before the second request")
	default:
	}
	second := httptest.NewRecorder()
	svc.ServeHTTP(second, request())
	if second.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("second request did not route while first stream was open: status=%d calls=%d", second.Code, calls.Load())
	}
}

func TestHalfOpenCanceledOrTimedOutUpstreamReleasesProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"cancel", StatusClientClosedRequest},
		{"deadline", http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			releaseUpstream := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-r.Context().Done():
				case <-releaseUpstream:
				}
			}))
			defer upstream.Close()
			defer close(releaseUpstream)
			svc := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
			defer svc.Close()
			state := svc.targetOrder[0]
			state.Target().MaxConcurrency = 1
			state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
			before := state.Stats()
			principal := newAllowAllPrincipal(t, "inflight-client")
			timeout := 200 * time.Millisecond
			if tc.name == "cancel" {
				timeout = 5 * time.Second
			}
			parent, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
			req = req.WithContext(auth.WithPrincipal(parent, principal))
			result := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				svc.ServeHTTP(result, req)
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("half-open request did not reach upstream")
			}
			if !state.probeSlotBusy(time.Now()) || state.inFlightCount() != 1 {
				t.Fatal("in-flight probe did not hold lease and capacity")
			}
			if tc.name == "cancel" {
				// A competing request is rejected for capacity without disturbing
				// the active probe's lease.
				other := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
				other = other.WithContext(auth.WithPrincipal(other.Context(), principal))
				blocked := httptest.NewRecorder()
				svc.ServeHTTP(blocked, other)
				if blocked.Code != http.StatusServiceUnavailable || !state.probeSlotBusy(time.Now()) {
					t.Fatalf("capacity competitor disturbed probe: status=%d busy=%t", blocked.Code, state.probeSlotBusy(time.Now()))
				}
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled or timed-out probe did not return")
			}
			if result.Code != tc.status || state.probeSlotBusy(time.Now()) || state.inFlightCount() != 0 || state.Stats() != before {
				t.Fatalf("probe cleanup: status=%d busy=%t in_flight=%d before=%+v after=%+v", result.Code, state.probeSlotBusy(time.Now()), state.inFlightCount(), before, state.Stats())
			}
		})
	}
}

func TestHalfOpenKeySwitchKeepsProbeUntilFinalResponse(t *testing.T) {
	secondEntered, releaseSecond := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseSecond) }) }
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer sk-test-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"credit_balance_exhausted","type":"insufficient_quota"}}`))
			return
		}
		close(secondEntered)
		<-releaseSecond
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	svc := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL, func(target *config.Target) {
		target.APIKeys = []string{"other-key"}
	})
	defer svc.Close()
	state := svc.targetOrder[0]
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	principal := newAllowAllPrincipal(t, "key-switch-client")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	ctx, cancel := context.WithCancel(auth.WithPrincipal(req.Context(), principal))
	req = req.WithContext(ctx)
	result := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.ServeHTTP(result, req)
	}()
	defer func() {
		cancel()
		unblock()
		<-done
	}()
	select {
	case <-secondEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("second key was not reached")
	}
	if !state.probeSlotBusy(time.Now()) || state.Stats().TotalSuccess != 0 {
		t.Fatal("intermediate key rejection released the active probe")
	}
	unblock()
	<-done
	if result.Code != http.StatusOK || calls.Load() != 2 || state.probeSlotBusy(time.Now()) {
		t.Fatalf("final key response did not settle probe: status=%d calls=%d busy=%t", result.Code, calls.Load(), state.probeSlotBusy(time.Now()))
	}
}

func TestHalfOpenExplicitCapacityRaceReleasesProbe(t *testing.T) {
	svc := newSingleTargetService(t)
	defer svc.Close()
	state := svc.targetOrder[0]
	state.Target().MaxConcurrency = 1
	state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
	before := state.Stats()
	if !state.acquireSlot() {
		t.Fatal("failed to preoccupy capacity fixture")
	}
	principal := newAllowAllPrincipal(t, "capacity-client")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set(headerProxyTarget, state.Target().Name)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || state.probeSlotBusy(time.Now()) || state.Stats() != before || state.inFlightCount() != 1 {
		t.Fatalf("post-selection capacity rejection leaked probe or changed health/slot: status=%d busy=%t before=%+v after=%+v in_flight=%d", rr.Code, state.probeSlotBusy(time.Now()), before, state.Stats(), state.inFlightCount())
	}
	state.releaseSlot()
	selected, lease, _, err := svc.selectTarget(principal, "127.0.0.1", "", strings.ToLower(state.Target().Name), nil, nil, "test-model", "/v1/chat/completions", time.Now())
	if err != nil || selected != state || lease == nil {
		t.Fatalf("next request could not probe immediately: selected=%v lease=%v err=%v", selected, lease, err)
	}
	state.releaseProbe(lease)
}
