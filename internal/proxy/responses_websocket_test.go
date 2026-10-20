package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/gorilla/websocket"
)

type websocketUsageRecorder struct {
	mu     sync.Mutex
	events []usage.Event
}

func (r *websocketUsageRecorder) Record(e usage.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}
func (r *websocketUsageRecorder) snapshot() []usage.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]usage.Event(nil), r.events...)
}

func websocketTestProxy(t *testing.T, endpoint string) (*Service, *httptest.Server, *websocketUsageRecorder) {
	t.Helper()
	service := newTestModelService(t, config.EndpointTypeOpenAI, endpoint, func(target *config.Target) {
		target.ModelMappings = []config.ModelMapping{{Upstream: "Codex-Upstream", Fallback: "codex-fast"}}
		target.CustomBody = map[string]any{"service_tier": "ultrafast"}
		target.CustomHeaders = map[string]string{"X-Saved-Setting": "yes"}
		target.MaxConcurrency = 1
		target.APIKeys = []string{"sk-other-key"}
	})
	recorder := &websocketUsageRecorder{}
	service.SetUsageRecorder(recorder)
	principal := newAllowAllPrincipal(t, "ws-client")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(func() { server.Close(); service.Close() })
	return service, server, recorder
}

func dialWebsocketTest(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": []string{"Bearer proxy-client-secret"}, "X-Api-Key": []string{"proxy-client-secret"}}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, headers)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket: %v (HTTP %d)", err, resp.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readWebsocketEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var event map[string]any
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestResponsesWebsocketPersistentTurnsAndAccounting(t *testing.T) {
	var connections atomic.Int32
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != responsesPath || r.Header.Get("Authorization") != "Bearer sk-test-key" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Saved-Setting") != "yes" {
			t.Errorf("upstream path/header mismatch: %s %v", r.URL.Path, r.Header)
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		connections.Add(1)
		for i := 0; i < 2; i++ {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				t.Error(err)
				return
			}
			requests.Add(1)
			if request["type"] != "response.create" || request["model"] != "Codex-Upstream" || request["service_tier"] != "ultrafast" {
				t.Errorf("request=%v", request)
			}
			if _, ok := request["stream"]; ok {
				t.Error("transport stream field sent upstream")
			}
			if _, ok := request["background"]; ok {
				t.Error("transport background field sent upstream")
			}
			if i == 0 && (request["generate"] != false || request["client_metadata"] == nil) {
				t.Error("Codex warmup or unknown metadata lost")
			}
			if i == 1 && request["previous_response_id"] != "resp_1" {
				t.Errorf("continuation lost: %v", request)
			}
			conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "model": "Codex-Upstream", "status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}})
		}
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	for i := 0; i < 2; i++ {
		payload := map[string]any{"type": "response.create", "model": "CODEX-FAST", "input": "hi", "store": false}
		if i == 1 {
			payload["previous_response_id"] = "resp_1"
		} else {
			payload["generate"] = false
			payload["client_metadata"] = map[string]any{"traceparent": "fixture"}
			payload["stream"] = false
			payload["background"] = false
		}
		if err := conn.WriteJSON(payload); err != nil {
			t.Fatal(err)
		}
		event := readWebsocketEvent(t, conn)
		if event["type"] != "response.completed" {
			t.Fatalf("event=%v", event)
		}
	}
	deadline := time.Now().Add(time.Second)
	for len(recorder.snapshot()) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	events := recorder.snapshot()
	if connections.Load() != 1 || requests.Load() != 2 || len(events) != 2 {
		t.Fatalf("connections=%d requests=%d usage=%v", connections.Load(), requests.Load(), events)
	}
	if service.metrics.totalRequests.Load() != 2 || service.metrics.totalSuccess.Load() != 2 {
		t.Fatalf("transport counted as business request: requests=%d success=%d", service.metrics.totalRequests.Load(), service.metrics.totalSuccess.Load())
	}
	if events[0].RequestID == "" || events[0].RequestID == events[1].RequestID {
		t.Fatal("turns must have distinct request IDs")
	}
	for _, event := range events {
		if event.Model != "codex-upstream" || event.InputTokens != 10 || event.OutputTokens != 2 || event.CostIncomplete {
			t.Errorf("usage=%+v", event)
		}
	}
}

func TestResponsesWebsocketRejectsChangedModelBeforeUpstream(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var request map[string]any
			if conn.ReadJSON(&request) != nil {
				return
			}
			requests.Add(1)
			conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed"}})
		}
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if readWebsocketEvent(t, conn)["type"] != "response.completed" {
		t.Fatal("first turn failed")
	}
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "unauthorized-model", "input": "hi", "previous_response_id": "resp_1"})
	event := readWebsocketEvent(t, conn)
	if event["type"] != "error" || event["status"] != float64(400) {
		t.Fatalf("event=%v", event)
	}
	if requests.Load() != 1 {
		t.Fatal("disallowed model reached upstream")
	}
}

func TestResponsesWebsocketTerminalErrorsAreNotReplayed(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete", "error", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var request map[string]any
				if conn.ReadJSON(&request) != nil {
					return
				}
				requests.Add(1)
				if kind != "disconnect" {
					response := map[string]any{"status": "failed"}
					if kind == "response.failed" {
						response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 2}
					}
					conn.WriteJSON(map[string]any{"type": kind, "error": map[string]any{"code": "rate_limit_exceeded"}, "response": response})
				}
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			event := readWebsocketEvent(t, conn)
			expected := kind
			if kind == "disconnect" {
				expected = "error"
			}
			if event["type"] != expected {
				t.Fatalf("event=%v", event)
			}
			if requests.Load() != 1 {
				t.Fatalf("inference replayed %d times", requests.Load())
			}
			waitWebsocketSlotsReleased(t, service)
			events := recorder.snapshot()
			if len(events) != 1 {
				t.Fatalf("missing failed usage event: %+v", events)
			}
			if kind == "response.failed" {
				if events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete {
					t.Fatalf("final failed-response usage lost: %+v", events)
				}
			} else if !events[0].CostIncomplete {
				t.Fatalf("missing terminal usage marked complete: %+v", events)
			}

			if service.metrics.totalSuccess.Load() != 0 || service.metrics.totalFailures.Load() != 1 {
				t.Fatalf("failed event counted as success: success=%d failures=%d", service.metrics.totalSuccess.Load(), service.metrics.totalFailures.Load())
			}
		})
	}
}

func TestResponsesWebsocketHandshakeRejectionIsNotRetried(t *testing.T) {
	for _, status := range []int{200, 302, 400, 401, 403, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(status) }))
			defer upstream.Close()
			service, server, _ := websocketTestProxy(t, upstream.URL)
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			event := readWebsocketEvent(t, conn)
			expectedStatus := status
			if status < 400 {
				expectedStatus = 502
			}
			if event["type"] != "error" || event["status"] != float64(expectedStatus) {
				t.Fatalf("event=%v", event)
			}
			wantAttempts := int32(1)
			if status == http.StatusBadRequest {
				wantAttempts = 2 // Handshake rejects before a create; HTTP retains its 400.
			}
			if attempts.Load() != wantAttempts {
				t.Fatalf("handshake retried %d times", attempts.Load())
			}
			waitWebsocketSlotsReleased(t, service)
		})
	}
}

func waitWebsocketSlotsReleased(t *testing.T, service *Service) {
	t.Helper()
	state, _ := service.targetByName("test-target")
	deadline := time.Now().Add(2 * time.Second)
	for state.inFlight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if state.inFlight.Load() != 0 {
		t.Fatalf("target slots leaked: %d", state.inFlight.Load())
	}
	if state.keyPool != nil {
		state.keyPool.mu.Lock()
		defer state.keyPool.mu.Unlock()
		for index, count := range state.keyPool.inFlight {
			if count != 0 {
				t.Fatalf("key %d slots leaked: %d", index, count)
			}
		}
	}
}

func TestResponsesWebsocketCancellationReleasesSlots(t *testing.T) {
	for _, disconnect := range []bool{true, false} {
		name := "timeout"
		if disconnect {
			name = "client disconnect"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			upstreamClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				defer close(upstreamClosed)
				var request map[string]any
				if conn.ReadJSON(&request) != nil {
					return
				}
				close(started)
				conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			state.MarkFailure(time.Now().Add(-time.Minute), time.Second)
			before := state.Stats()
			if !disconnect {
				service.requestTimeout.Store(int64(time.Second))
			}
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not started")
			}
			if disconnect {
				conn.Close()
			} else if readWebsocketEvent(t, conn)["type"] != "error" {
				t.Fatal("timeout did not return error")
			}
			select {
			case <-upstreamClosed:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream leaked after cancellation")
			}
			waitWebsocketSlotsReleased(t, service)
			leaseDeadline := time.Now().Add(time.Second)
			for state.probeSlotBusy(time.Now()) && time.Now().Before(leaseDeadline) {
				time.Sleep(time.Millisecond)
			}
			if state.Stats() != before || state.IsMuted(time.Now()) || state.probeSlotBusy(time.Now()) {
				t.Fatalf("client cancellation or timeout changed half-open health/lease: before=%+v after=%+v busy=%t", before, state.Stats(), state.probeSlotBusy(time.Now()))
			}

			deadline := time.Now().Add(time.Second)
			for len(recorder.snapshot()) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if events := recorder.snapshot(); len(events) != 1 || !events[0].CostIncomplete {
				t.Fatalf("interrupted usage=%+v", events)
			}
		})
	}
}

func TestResponsesWebsocketRevalidatesConnectionCredentials(t *testing.T) {
	for _, test := range []struct {
		name    string
		clients []config.Client
		status  int
	}{
		{"ACL revoked", []config.Client{{Name: "tester", AccessKey: "proxy-client-secret", AllowedTargets: []string{"other-target"}}}, 403},
		{"key revoked", nil, 401},
		{"IP restricted", []config.Client{{Name: "tester", AccessKey: "proxy-client-secret", IPAddress: "10.0.0.1"}}, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for {
					var request map[string]any
					if conn.ReadJSON(&request) != nil {
						return
					}
					requests.Add(1)
					conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed"}})
				}
			}))
			defer upstream.Close()
			service := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
			defer service.Close()
			store := auth.NewStore()
			if err := store.LoadFromConfig([]config.Client{{Name: "tester", AccessKey: "proxy-client-secret"}}); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(auth.Middleware(store, newTestLogger())(service))
			defer server.Close()
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "test-model", "input": "hi"})
			if readWebsocketEvent(t, conn)["type"] != "response.completed" {
				t.Fatal("first turn failed")
			}
			if err := store.LoadFromConfig(test.clients); err != nil {
				t.Fatal(err)
			}
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "test-model", "input": "hi", "previous_response_id": "resp_1"})
			event := readWebsocketEvent(t, conn)
			if event["type"] != "error" || event["status"] != float64(test.status) {
				t.Fatalf("event=%v", event)
			}
			if requests.Load() != 1 {
				t.Fatal("revoked credential reached upstream")
			}
		})
	}
}

func TestResponsesWebsocketContinuationDoesNotChangeCredential(t *testing.T) {
	var connections atomic.Int32
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var request map[string]any
			if conn.ReadJSON(&request) != nil {
				return
			}
			requests.Add(1)
			conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed"}})
		}
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if readWebsocketEvent(t, conn)["type"] != "response.completed" {
		t.Fatal("first turn failed")
	}
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 60}, Targets: []config.Target{{Name: "test-target", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "changed-key", ModelMappings: []config.ModelMapping{{Upstream: "Codex-Upstream", Fallback: "codex-fast"}}}}}
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi", "previous_response_id": "resp_1"})
	event := readWebsocketEvent(t, conn)
	if event["type"] != "error" || event["status"] != float64(409) {
		t.Fatalf("event=%v", event)
	}
	details, _ := event["error"].(map[string]any)
	if !strings.Contains(details["message"].(string), "full input") {
		t.Fatalf("recovery instruction missing: %v", event)
	}
	if connections.Load() != 1 || requests.Load() != 1 {
		t.Fatal("continuation crossed credential boundary")
	}
}

func TestResponsesWebsocketRejectsUnsupportedEventAndProvider(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	for _, eventType := range []string{"response.append", "response.steer", "invalid"} {
		conn.WriteJSON(map[string]any{"type": eventType, "model": "codex-fast", "input": "hi"})
		event := readWebsocketEvent(t, conn)
		if event["type"] != "error" || event["status"] != float64(400) {
			t.Fatalf("event=%v", event)
		}
	}
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi", "stream_id": "planner"})
	if event := readWebsocketEvent(t, conn); event["status"] != float64(400) {
		t.Errorf("parallel stream accepted: %v", event)
	}
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi", "background": true})
	if readWebsocketEvent(t, conn)["status"] != float64(400) {
		t.Fatal("background accepted")
	}
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 60}, Targets: []config.Target{{Name: "test-target", EndpointType: config.EndpointTypeAzureOpenAI, Endpoint: upstream.URL, APIKey: "key", ModelMappings: []config.ModelMapping{{Upstream: "Codex-Upstream", Fallback: "codex-fast"}}}}}
	if err := service.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	if event["status"] != float64(400) {
		t.Fatalf("event=%v", event)
	}
	if requests.Load() != 0 {
		t.Fatal("unsupported event/provider reached upstream")
	}
}

func TestResponsesWebsocketNetworkFailureBeforeSendCanFailover(t *testing.T) {
	unavailable := httptest.NewServer(http.NotFoundHandler())
	unavailableURL := unavailable.URL
	unavailable.Close()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer live-key" {
			t.Error("wrong failover credential")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		requests.Add(1)
		if request["model"] != "LiveCodex" || request["service_tier"] != "ultrafast" {
			t.Errorf("failover assembly=%v", request)
		}
		conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed"}})
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{
		{Name: "dead", EndpointType: config.EndpointTypeOpenAI, Endpoint: unavailableURL, APIKey: "dead-key", ModelMappings: []config.ModelMapping{{Upstream: "DeadCodex", Fallback: "codex-fast"}}},
		{Name: "live", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "live-key", ModelMappings: []config.ModelMapping{{Upstream: "LiveCodex", Fallback: "codex-fast"}}, CustomBody: map[string]any{"service_tier": "ultrafast"}},
	}}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	principal := newAllowAllPrincipal(t, "network-fixture")
	service.affinity.Set(affinityKey("127.0.0.1", principal.Name, "codex-fast"), "dead", time.Now())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	defer server.Close()
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("event=%v", event)
	}
	if requests.Load() != 1 || service.metrics.totalTargetRetries.Load() != 1 {
		t.Fatalf("requests=%d target retries=%d", requests.Load(), service.metrics.totalTargetRetries.Load())
	}
}

func TestResponsesWebsocketUpstreamPingBetweenTurns(t *testing.T) {
	pong := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed"}})
		var once sync.Once
		conn.SetPongHandler(func(string) error { once.Do(func() { close(pong) }); return nil })
		conn.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(time.Second))
		if conn.ReadJSON(&request) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2", "status": "completed"}})
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if readWebsocketEvent(t, conn)["type"] != "response.completed" {
		t.Fatal("first turn failed")
	}
	select {
	case <-pong:
	case <-time.After(time.Second):
		t.Fatal("upstream ping not answered while idle")
	}
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi", "previous_response_id": "resp_1"})
	if readWebsocketEvent(t, conn)["type"] != "response.completed" {
		t.Fatal("second turn failed")
	}
}

func TestResponsesWebsocketServiceCloseCancelsIdleAndActiveConnections(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			closed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				defer close(closed)
				var request map[string]any
				if conn.ReadJSON(&request) != nil {
					return
				}
				close(started)
				if !active {
					conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
				}
				conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upstream not started")
			}
			if !active {
				readWebsocketEvent(t, conn)
			}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Error("upstream not closed with service")
			}
			waitWebsocketSlotsReleased(t, service)
			if events := recorder.snapshot(); len(events) != 1 || events[0].CostIncomplete != active {
				t.Fatalf("final usage missing after Close: %+v", events)
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Fatal("downstream left open after Close")
			}
		})
	}
}

func TestResponsesWebsocketPrettyJSONRetainsEventAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		event := map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{map[string]any{"text": "中文\n下一行"}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}}
		wire, _ := json.MarshalIndent(event, "", "  ")
		conn.WriteMessage(websocket.TextMessage, wire)
		conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	response, _ := event["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if event["type"] != "response.completed" || len(output) != 1 || output[0].(map[string]any)["text"] != "中文\n下一行" {
		t.Fatalf("event changed: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	if events := recorder.snapshot(); len(events) != 1 || events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete {
		t.Fatalf("usage=%+v", events)
	}
}

func TestResponsesWebsocketRequestFailureDoesNotMuteHealthyTarget(t *testing.T) {
	for index, failure := range []map[string]any{
		{"type": "response.incomplete", "response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}},
		{"type": "response.incomplete", "response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "content_filter"}}},
		{"type": "error", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "invalid_input"}},
		{"type": "error", "status": 429, "error": map[string]any{"type": "rate_limit_error", "code": "rate_limit_exceeded"}},
		{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"type": "invalid_request_error", "code": "invalid_input"}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}},
	} {
		t.Run(failure["type"].(string), func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for {
					var request map[string]any
					if conn.ReadJSON(&request) != nil {
						return
					}
					attempt := requests.Add(1)
					if attempt == 1 {
						conn.WriteJSON(failure)
					} else {
						conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2", "status": "completed", "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}}})
					}
				}
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			state.mu.Lock()
			state.consecutiveFailure = 1
			state.mutedUntil = time.Now().Add(-time.Second)
			state.mu.Unlock()
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			if event := readWebsocketEvent(t, conn); event["type"] != failure["type"] {
				t.Fatalf("first event=%v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			state, _ = service.targetByName("test-target")
			if state.probeSlotBusy(time.Now()) {
				t.Error("healthy failed turn left half-open probe occupied")
			}
			if state.IsMuted(time.Now()) {
				t.Error("request-level terminal event muted healthy target")
			}
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "fixed request"})
			if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
				t.Fatalf("healthy target rejected next turn: %v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			if requests.Load() != 2 {
				t.Fatalf("requests=%d", requests.Load())
			}
			if len(recorder.snapshot()) != 2 {
				t.Fatalf("failed-turn usage was lost: %+v", recorder.snapshot())
			}
			events := recorder.snapshot()
			expectedStatuses := []int{200, 200, 400, 429, 400}
			if events[0].StatusCode != expectedStatuses[index] || events[1].StatusCode != 200 {
				t.Fatalf("usage statuses=%d/%d want %d/200", events[0].StatusCode, events[1].StatusCode, expectedStatuses[index])
			}

			if service.metrics.totalFailures.Load() != 1 || service.metrics.totalSuccess.Load() != 1 {
				t.Fatal("turn outcome statistics changed")
			}
		})
	}
}

func TestResponsesWebsocketServerFailureStillMutesTarget(t *testing.T) {
	for index, failure := range []map[string]any{
		{"type": "error", "status": 503, "error": map[string]any{"type": "server_error"}},
		{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "server_error"}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}},
	} {
		t.Run(failure["type"].(string), func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var request map[string]any
				if conn.ReadJSON(&request) != nil {
					return
				}
				requests.Add(1)
				conn.WriteJSON(failure)
				conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			conn := dialWebsocketTest(t, server)
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			if event := readWebsocketEvent(t, conn); event["type"] != failure["type"] {
				t.Fatalf("event=%v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			state, _ := service.targetByName("test-target")
			if !state.IsMuted(time.Now()) {
				t.Fatal("real server fault did not enter existing breaker policy")
			}
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			if event := readWebsocketEvent(t, conn); event["status"] != float64(503) {
				t.Fatalf("muted target accepted next request: %v", event)
			}
			expectedStatuses := []int{503, 500}
			if events := recorder.snapshot(); len(events) != 1 || events[0].StatusCode != expectedStatuses[index] {
				t.Fatalf("server error usage status=%+v", events)
			}
			if requests.Load() != 1 || len(recorder.snapshot()) != 1 {
				t.Fatal("server-failed inference was replayed or usage was lost")
			}
		})
	}
}
