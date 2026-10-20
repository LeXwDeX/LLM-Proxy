package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

func TestResponsesWebsocketTerminalKeyFailureUsesHealthyKey(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, reason string
			status             int
		}{
			{"rate429", `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, "rate_limited", 429},
			{"quota429", `{"error":{"type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}`, "quota_exceeded_api", 429},
			{"billing402", `{"error":{"type":"billing_error"}}`, "billing_error", 402},
			{"invalid401", `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`, "invalid_token", 401},
		} {
			t.Run(fmt.Sprintf("fallback=%v/%s", fallback, tc.name), func(t *testing.T) {
				var creates, healthy atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if fallback && websocket.IsWebSocketUpgrade(r) {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					var conn *websocket.Conn
					if !fallback {
						var err error
						conn, err = (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						if _, _, err = conn.ReadMessage(); err != nil {
							return
						}
					}
					creates.Add(1)
					var event any
					if r.Header.Get("Authorization") == "Bearer sk-test-key" {
						var envelope map[string]json.RawMessage
						_ = json.Unmarshal([]byte(tc.body), &envelope)
						event = map[string]any{"type": "error", "status": tc.status, "error": envelope["error"]}
					} else {
						healthy.Add(1)
						event = map[string]any{"type": "response.completed", "response": map[string]any{"id": "healthy", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}
					}
					if fallback {
						w.Header().Set("Content-Type", "text/event-stream")
						data, _ := json.Marshal(event)
						fmt.Fprintf(w, "data: %s\n\n", data)
					} else {
						_ = conn.WriteJSON(event)
						_, _, _ = conn.ReadMessage()
					}
				}))
				defer upstream.Close()
				service, server, _ := websocketTestProxy(t, upstream.URL)
				conn := dialWebsocketTest(t, server)
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "first"}); err != nil {
					t.Fatal(err)
				}
				if event := readWebsocketEvent(t, conn); event["type"] != "error" || event["status"] != float64(tc.status) {
					t.Fatalf("first event=%v", event)
				}
				waitWebsocketSlotsReleased(t, service)
				state, _ := service.targetByName("test-target")
				state.keyPool.mu.Lock()
				first, second := state.keyPool.entries[0], state.keyPool.entries[1]
				state.keyPool.mu.Unlock()
				if !first.exhausted || first.exhaustReason != tc.reason || !first.cooldownEnd.After(time.Now()) || second.exhausted || state.IsMuted(time.Now()) {
					t.Errorf("incorrect key feedback: exhausted=%v reason=%s healthy_exhausted=%v muted=%v", first.exhausted, first.exhaustReason, second.exhausted, state.IsMuted(time.Now()))
				}
				if tc.name == "quota429" && !first.cooldownEnd.Equal(permanentCooldownEnd) {
					t.Errorf("quota incorrectly received temporary cooldown: %s", first.cooldownEnd)
				}
				if !strings.Contains(first.lastError, "error") {
					t.Errorf("error body was not retained: %q", first.lastError)
				}
				if creates.Load() != 1 || healthy.Load() != 0 {
					t.Fatalf("generation replayed before next turn: creates=%d healthy=%d", creates.Load(), healthy.Load())
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "next"}); err != nil {
					t.Fatal(err)
				}
				if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
					t.Fatalf("next turn event=%v", event)
				}
				waitWebsocketSlotsReleased(t, service)
				if creates.Load() != 2 || healthy.Load() != 1 || state.inFlight.Load() != 0 {
					t.Fatalf("next turn/release failed: creates=%d healthy=%d in_flight=%d", creates.Load(), healthy.Load(), state.inFlight.Load())
				}
			})
		}
	}
}

func TestResponsesWebsocketHandshakeFinalFailureAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		status, attempts int
		unhealthy        bool
	}{
		{"ordinary403", `{"error":{"message":"denied"}}`, 403, 1, false},
		{"unknown401", `{"error":{"message":"invalid_api_key"}}`, 401, 1, false},
		{"malformed401", `{"error":{"code":"invalid_api_key"}} trailing`, 401, 1, false},
		{"ordinary503", `{"error":{"message":"unavailable"}}`, 503, 1, true},
		{"accountConvergence", `{"error":{"code":"invalid_api_key","message":"account disabled"}}`, 401, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", "rejected-request")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			state.MarkFailure(time.Now().Add(-time.Hour), time.Nanosecond)
			conn := dialWebsocketTest(t, server)
			_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			if event := readWebsocketEvent(t, conn); event["type"] != "error" || event["status"] != float64(tc.status) {
				t.Fatalf("final failure=%v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			stats := state.Stats()
			wantFailure, wantSuccess := int64(1), int64(tc.attempts)
			if tc.unhealthy {
				wantFailure++ // includes the old failure used to enter half-open
				wantSuccess--
			}
			if attempts.Load() != int32(tc.attempts) || stats.TotalFailure != wantFailure || stats.TotalSuccess != wantSuccess || state.IsMuted(time.Now()) != tc.unhealthy || state.probeSlotBusy(time.Now()) || service.metrics.totalFailures.Load() != 1 || service.metrics.totalSuccess.Load() != 0 || service.metrics.totalTargetRetries.Load() != 0 {
				t.Fatalf("final rejection did not settle once: attempts=%d stats=%+v successes=%d failures=%d", attempts.Load(), stats, service.metrics.totalSuccess.Load(), service.metrics.totalFailures.Load())
			}
			if events := recorder.snapshot(); len(events) != 0 {
				t.Fatalf("no generation was sent, but usage was recorded: %+v", events)
			}
			// A second already-buffered frame would prove the final failure was
			// emitted twice. Closing is safe here: no further turn is needed.
			conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Error("more than one downstream error for one client turn")
			}
		})
	}
}

func TestResponsesWebsocketHandshakeRateLimitBounds(t *testing.T) {
	for _, keyCount := range []int{2, 5} {
		t.Run(fmt.Sprint(keyCount), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			keys := make([]string, keyCount)
			for i := range keys {
				keys[i] = fmt.Sprintf("local-key-%d", i)
			}
			state.keyPool = newKeyPool(state.target.Name, keys, "", service.logger)
			conn := dialWebsocketTest(t, server)
			_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil || event["status"] != float64(429) {
				t.Fatalf("bounded final rejection=%v err=%v", event, err)
			}
			waitWebsocketSlotsReleased(t, service)
			wantAttempts := int32(2 * keyCount)
			if keyCount == 5 {
				wantAttempts = 9 // four switches, then the global safety valve
			}
			if attempts.Load() != wantAttempts || state.Stats().TotalSuccess != int64(wantAttempts) || state.Stats().TotalFailure != 0 || service.metrics.totalFailures.Load() != 1 || service.metrics.totalSuccess.Load() != 0 || len(recorder.snapshot()) != 0 {
				t.Fatalf("unbounded/double-counted rejection: attempts=%d stats=%+v failures=%d", attempts.Load(), state.Stats(), service.metrics.totalFailures.Load())
			}
		})
	}
}

func TestResponsesWebsocketHandshakeCancellationDuringRetry(t *testing.T) {
	first := make(chan struct{})
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			close(first)
		}
		w.WriteHeader(429)
	}))
	defer upstream.Close()
	service, _, recorder := websocketTestProxy(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &responsesWebsocketSession{}
	req := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(`{"model":"codex-fast","input":"hi"}`))
	req = req.WithContext(auth.WithPrincipal(context.WithValue(ctx, responsesWebsocketContextKey{}, session), newAllowAllPrincipal(t, "local-client")))
	done := make(chan struct{})
	go func() { service.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	<-first
	state, _ := service.targetByName("test-target")
	deadline := time.Now().Add(time.Second)
	for {
		state.keyPool.mu.Lock()
		retrying := state.keyPool.entries[0].consecutiveFails == 1
		state.keyPool.mu.Unlock()
		if retrying {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test never reached the same-key retry boundary")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled retry did not terminate")
	}
	waitWebsocketSlotsReleased(t, service)
	if attempts.Load() != 1 || len(recorder.snapshot()) != 0 {
		t.Fatalf("canceled turn replayed or invented usage: attempts=%d usage=%+v", attempts.Load(), recorder.snapshot())
	}
}

func TestResponsesWebsocketHandshakeExhaustionFailoverAdmission(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("denied=%v", denied), func(t *testing.T) {
			var rejected, creates atomic.Int32
			dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rejected.Add(1)
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":{"code":"credit_balance_exhausted"}}`)
			}))
			defer dead.Close()
			live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				var frame map[string]any
				if conn.ReadJSON(&frame) != nil {
					return
				}
				creates.Add(1)
				if frame["model"] != "LiveCase" || r.Header.Get("Authorization") != "Bearer live-key" {
					t.Errorf("failover did not rebuild the selected target request: %v", frame)
				}
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "live", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
			}))
			defer live.Close()
			service, _, recorder := websocketTestProxy(t, dead.URL)
			if err := service.ApplyConfig(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{
				{Name: "test-target", EndpointType: "openai", Endpoint: dead.URL, APIKey: "dead-key", APIKeys: []string{"dead-key-2"}, MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "DeadCase", Fallback: "codex-fast"}}},
				{Name: "live", EndpointType: "openai", Endpoint: live.URL, APIKey: "live-key", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "LiveCase", Fallback: "codex-fast"}}},
			}}); err != nil {
				t.Fatal(err)
			}
			if denied {
				manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 1, RMin: 1, RMax: 1})
				manager.InitTarget("test-target", 1)
				manager.InitTarget("live", 1)
				if !manager.TryAcquire("live") {
					t.Fatal("could not exhaust the live target admission token")
				}
				service.SetAdmissionManager(manager)
			}
			session := &responsesWebsocketSession{}
			defer func() {
				if session.conn != nil {
					_ = session.conn.Close()
				}
			}()
			req := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(`{"model":"codex-fast","input":"hi"}`))
			service.affinity.Set(affinityKey(clientIP(req), "tester", "codex-fast"), "test-target", time.Now())
			ctx, cancel := context.WithTimeout(auth.WithPrincipal(context.WithValue(req.Context(), responsesWebsocketContextKey{}, session), newAllowAllPrincipal(t, "local-client")), 100*time.Millisecond)
			defer cancel()
			writer := httptest.NewRecorder()
			service.ServeHTTP(writer, req.WithContext(ctx))
			wantStatus, wantCreates := 200, int32(1)
			if denied {
				wantStatus, wantCreates = 429, 0
			}
			state, _ := service.targetByName("test-target")
			if writer.Code != wantStatus || creates.Load() != wantCreates || rejected.Load() != 2 || service.metrics.totalTargetRetries.Load() < 1 || state.IsMuted(time.Now()) || state.Stats().TotalSuccess != 2 || int32(len(recorder.snapshot())) != wantCreates {
				t.Fatalf("native exhaustion/admission failed: status=%d creates=%d rejected=%d retries=%d stats=%+v", writer.Code, creates.Load(), rejected.Load(), service.metrics.totalTargetRetries.Load(), state.Stats())
			}
			waitWebsocketSlotsReleased(t, service)
			for _, status := range service.TargetStatuses(time.Now()) {
				if status.InFlight != 0 {
					t.Fatalf("failover target %s leaked slots: %d", status.Name, status.InFlight)
				}
			}
		})
	}
}

func TestResponsesWebsocketHandshakeKeyFailureReleasesProbeLease(t *testing.T) {
	var healthy atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-test-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		healthy.Add(1)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "healthy", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	state, _ := service.targetByName("test-target")
	state.MarkFailure(time.Now().Add(-time.Hour), time.Nanosecond)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "probe"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("probe event=%v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	if state.probeSlotBusy(time.Now()) {
		t.Error("key-specific handshake rejection left the target probe lease occupied")
	}
	if healthy.Load() != 1 {
		t.Fatalf("healthy key was not used exactly once: %d", healthy.Load())
	}
}

func TestResponsesWebsocketHandshakeRotatesBeforeGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, adapter, body, reason string
		status, badAttempts         int
	}{
		{"invalid401", "standard", `{"error":{"code":"invalid_api_key"}}`, "invalid_token", 401, 1},
		{"billing402", "standard", `{"error":{"type":"billing_error"}}`, "billing_error", 402, 1},
		{"empty402", "standard", "", "billing_error", 402, 1},
		{"malformed402", "standard", `{"error":`, "billing_error", 402, 1},
		{"rate429", "standard", `{"error":{"code":"rate_limit_exceeded"}}`, "rate_limited", 429, 2},
		{"empty429", "standard", "", "rate_limited", 429, 2},
		{"longQuota429", "standard", fmt.Sprintf(`{"error":{"message":%q,"code":"credit_balance_exhausted"}}`, strings.Repeat("detail ", 800)), "quota_exceeded_api", 429, 1},
		{"longKimiQuota429", "kimi", fmt.Sprintf(`{"error":{"message":%q,"type":"exceeded_current_quota_error"}}`, strings.Repeat("detail ", 800)), "quota_exceeded_api", 429, 1},
		{"longGLMQuota429", "glm", fmt.Sprintf(`{"error":{"message":%q,"code":"1308"}}`, strings.Repeat("detail ", 800)), glmExhaustReason, 429, 1},
		{"qwen403", "qwen", `{"error":{"code":"AccessDenied.Unpurchased"}}`, "AccessDenied.Unpurchased", 403, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rejected, creates atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("X-Saved-Setting") != "yes" || r.Header.Get("X-Request-Id") == "" {
					t.Errorf("assembled handshake settings lost: %s %v", r.Method, r.Header)
				}
				if r.Header.Get("Authorization") == "Bearer sk-test-key" {
					rejected.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				creates.Add(1)
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "healthy", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
				_, _, _ = conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			state.target.UsageAdapter = tc.adapter
			// The successful key must clear its old rate-limit/failure counters.
			state.keyPool.entries[1].consecutiveRateLimit = 2
			state.keyPool.entries[1].consecutiveFails = 1
			conn := dialWebsocketTest(t, server)
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
				t.Fatal(err)
			}
			if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
				t.Fatalf("intermediate rejection escaped instead of same-turn completion: %v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			state.keyPool.mu.Lock()
			bad, good := state.keyPool.entries[0], state.keyPool.entries[1]
			state.keyPool.mu.Unlock()
			if rejected.Load() != int32(tc.badAttempts) || creates.Load() != 1 || bad.exhaustReason != tc.reason || !bad.exhausted || good.exhausted || good.consecutiveRateLimit != 0 || good.consecutiveFails != 0 {
				t.Fatalf("incorrect rotation/feedback: rejected=%d creates=%d bad=%+v good=%+v", rejected.Load(), creates.Load(), bad, good)
			}
			if tc.body != "" && bad.lastError == "" {
				t.Error("rejection diagnostics not retained")
			}
			stats := state.Stats()
			if stats.TotalSuccess != int64(tc.badAttempts+1) || stats.TotalFailure != 0 || state.IsMuted(time.Now()) || service.metrics.totalSuccess.Load() != 1 || service.metrics.totalFailures.Load() != 0 {
				t.Fatalf("attempt health or final client metrics counted incorrectly: stats=%+v success=%d failure=%d", stats, service.metrics.totalSuccess.Load(), service.metrics.totalFailures.Load())
			}
			if events := recorder.snapshot(); len(events) != 1 || events[0].StatusCode != 200 {
				t.Fatalf("pre-generation rejection invented usage: %+v", events)
			}
		})
	}
}
