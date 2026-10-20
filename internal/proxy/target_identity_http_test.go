package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Exercise the client-visible single-target failure, local cooldown and recovery
// sequence. The late request variant settles an old-generation failure only after
// the replacement credential has already succeeded.
func TestTargetIdentityHTTPRecovery(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "idle"
		if late {
			name = "old-request-in-flight"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if late && call == 1 {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer replacement-key" {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			cfg := &config.Config{
				Server:  config.ServerConfig{RequestTimeoutSeconds: 5},
				Targets: []config.Target{{Name: "only", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "original-key", UsageAdapter: config.UsageAdapterStandard}},
			}
			svc, err := NewService(cfg, newTestLogger())
			if err != nil {
				unblock()
				upstream.Close()
				t.Fatal(err)
			}
			var lateDone chan struct{}
			defer func() {
				unblock()
				if lateDone != nil {
					<-lateDone
				}
				_ = svc.Close()
				upstream.Close()
			}()
			principal := newAllowAllPrincipal(t, "identity-http-client")
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"OK"}]}`))
				req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
				rr := httptest.NewRecorder()
				svc.ServeHTTP(rr, req)
				return rr
			}
			var lateResult *httptest.ResponseRecorder
			if late {
				lateDone = make(chan struct{})
				go func() { defer close(lateDone); lateResult = request() }()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("old request did not reach upstream")
				}
			}
			// An actual upstream 503 creates the cooldown, with no retry.
			wantCalls := int32(1)
			if late {
				wantCalls++
			}
			if rr := request(); rr.Code != http.StatusServiceUnavailable || calls.Load() != wantCalls {
				t.Fatalf("upstream failure: status=%d calls=%d want=%d", rr.Code, calls.Load(), wantCalls)
			}
			old := svc.targetOrder[0]
			if old.Phase(time.Now()) != phaseMuted {
				t.Fatal("upstream 503 did not mute the target")
			}
			assertLocalMute := func() {
				t.Helper()
				rr := request()
				if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "all targets muted") || calls.Load() != wantCalls {
					t.Fatalf("local cooldown: status=%d body=%s calls=%d want=%d", rr.Code, rr.Body.String(), calls.Load(), wantCalls)
				}
			}
			assertLocalMute()
			cfg.Targets[0].MaxConcurrency = 3
			if err := svc.ApplyConfig(cfg); err != nil {
				t.Fatal(err)
			}
			assertLocalMute()
			cfg.Targets[0].APIKey = "replacement-key"
			if err := svc.ApplyConfig(cfg); err != nil {
				t.Fatal(err)
			}
			current := svc.targetOrder[0]
			wantCalls++
			if rr := request(); rr.Code != http.StatusOK || calls.Load() != wantCalls {
				t.Fatalf("credential replacement did not recover immediately: status=%d body=%s calls=%d want=%d", rr.Code, rr.Body.String(), calls.Load(), wantCalls)
			}
			beforeLate := current.Stats()
			if late {
				unblock()
				select {
				case <-lateDone:
				case <-time.After(3 * time.Second):
					t.Fatal("old request did not finish")
				}
				if lateResult.Code != http.StatusServiceUnavailable || old.Stats().TotalFailure != 2 {
					t.Fatalf("old failure was not settled: status=%d stats=%+v", lateResult.Code, old.Stats())
				}
				if current.Stats() != beforeLate {
					t.Fatal("late old failure changed current health")
				}
			}
			wantCalls++
			if rr := request(); rr.Code != http.StatusOK || calls.Load() != wantCalls || current.Phase(time.Now()) != phaseReady {
				t.Fatalf("recovered target became unavailable: status=%d calls=%d phase=%s", rr.Code, calls.Load(), current.Phase(time.Now()))
			}
			if current.inFlightCount() != 0 || current.probeSlotBusy(time.Now()) {
				t.Fatal("requests leaked target capacity or probe lease")
			}
		})
	}
}
