package proxy

import (
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
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/gorilla/websocket"
)

// Issue #153: WS and HTTP are only different transports. These tests pin the
// native WS turn error events and retry/health criteria to the HTTP path.

func parityEventJSON(t *testing.T, event map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// B1: proxy-local JSON errors keep their structured error object, and a
// Retry-After header is exposed as retry_after.
func TestResponsesWebsocketTurnErrorEventKeepsLocalJSON(t *testing.T) {
	resetsAt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name        string
		write       func(http.ResponseWriter)
		status      int
		code        string
		retryAfter  any // nil: absent
		wantMessage string
		check       func(t *testing.T, errObj map[string]any)
	}{
		{name: "quota429", status: 429, code: "quota_exceeded",
			write: func(w http.ResponseWriter) {
				writeQuotaExceededResponse(w, quota.ExceededInfo{Scope: "client", Dimension: "daily", Limit: 1, Used: 2, ResetsAt: resetsAt})
			},
			check: func(t *testing.T, errObj map[string]any) {
				q, _ := errObj["quota"].(map[string]any)
				if q["resets_at"] != resetsAt.Format(time.RFC3339) {
					t.Fatalf("quota.resets_at lost: %v", errObj)
				}
			}},
		{name: "accounting503", status: 503, code: "accounting_unavailable",
			write: func(w http.ResponseWriter) {
				writeQuotaExceededResponse(w, quota.ExceededInfo{Scope: quota.ScopeAccounting})
			}},
		{name: "upstreamLimited429", status: 429, code: "upstream_rate_limited", retryAfter: float64(5),
			write: func(w http.ResponseWriter) { writeUpstreamLimitedResponse(w, false, time.Time{}) }},
		{name: "upstreamQuota429", status: 429, code: "upstream_quota_exhausted",
			write: func(w http.ResponseWriter) { writeUpstreamLimitedResponse(w, true, resetsAt) },
			check: func(t *testing.T, errObj map[string]any) {
				if errObj["resets_at"] != resetsAt.Format(time.RFC3339) {
					t.Fatalf("resets_at lost: %v", errObj)
				}
			}},
		{name: "plainText400", status: 400, code: "proxy_request_failed", wantMessage: "model is not allowed",
			write: func(w http.ResponseWriter) { http.Error(w, "model is not allowed", 400) }},
		{name: "empty502", status: 502, code: "proxy_request_failed", wantMessage: http.StatusText(502),
			write: func(w http.ResponseWriter) { w.WriteHeader(502) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &responsesWebsocketWriter{header: make(http.Header)}
			tc.write(writer)
			event := parityEventJSON(t, responsesWebsocketTurnErrorEvent(&responsesWebsocketSession{}, writer))
			errObj, _ := event["error"].(map[string]any)
			if event["type"] != "error" || event["status"] != float64(tc.status) || errObj == nil || errObj["code"] != tc.code {
				t.Fatalf("event=%v", event)
			}
			if tc.wantMessage != "" && errObj["message"] != tc.wantMessage {
				t.Fatalf("message=%v", errObj["message"])
			}
			if tc.check != nil {
				tc.check(t, errObj)
			}
			if tc.name == "upstreamQuota429" {
				if ra, ok := event["retry_after"].(float64); !ok || ra < 1 {
					t.Fatalf("retry_after=%v", event["retry_after"])
				}
			} else if event["retry_after"] != tc.retryAfter {
				t.Fatalf("retry_after=%v want %v", event["retry_after"], tc.retryAfter)
			}
		})
	}
}

// B1: a recorded session failure still wins and keeps its message.
func TestResponsesWebsocketTurnErrorEventSessionFailure(t *testing.T) {
	writer := &responsesWebsocketWriter{header: make(http.Header)}
	http.Error(writer, "Bad Gateway", 502)
	event := parityEventJSON(t, responsesWebsocketTurnErrorEvent(&responsesWebsocketSession{failure: "upstream failed"}, writer))
	errObj, _ := event["error"].(map[string]any)
	if errObj["code"] != "proxy_request_failed" || errObj["message"] != "upstream failed" {
		t.Fatalf("event=%v", event)
	}
}

func parityTwoTargetProxy(t *testing.T, a, b config.Target) (*Service, *httptest.Server, *auth.Principal) {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{a, b}}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	principal := newAllowAllPrincipal(t, "parity-fixture")
	service.affinity.Set(affinityKey("127.0.0.1", principal.Name, "codex-fast"), a.Name, time.Now())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(func() { server.Close(); service.Close() })
	return service, server, principal
}

func parityWSUpstream(t *testing.T, creates *atomic.Int32) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			creates.Add(1)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_b", "status": "completed"}})
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

const paritySSECompleted = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_a\",\"status\":\"completed\"}}\n\n"

// B2: on a target remembered as WebSocket-unsupported, a connection refused
// HTTP fallback POST provably sent nothing and fails over like the native dial.
func TestResponsesWebsocketFallbackPreSendErrorFailsOver(t *testing.T) {
	var posts atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // upgrade unsupported: remembered
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, paritySSECompleted)
	}))
	var creates atomic.Int32
	b := parityWSUpstream(t, &creates)
	service, server, _ := parityTwoTargetProxy(t,
		config.Target{Name: "a", EndpointType: config.EndpointTypeOpenAI, Endpoint: a.URL, APIKey: "a-key", ModelMappings: []config.ModelMapping{{Upstream: "A", Fallback: "codex-fast"}}},
		config.Target{Name: "b", EndpointType: config.EndpointTypeOpenAI, Endpoint: b.URL, APIKey: "b-key", ModelMappings: []config.ModelMapping{{Upstream: "B", Fallback: "codex-fast"}}})
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" || posts.Load() != 1 {
		t.Fatalf("first turn: %v posts=%d", event, posts.Load())
	}
	a.Close() // connection refused from now on
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "again"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("pre-send fallback failure did not fail over: %v", event)
	}
	if creates.Load() != 1 || service.metrics.totalTargetRetries.Load() != 1 {
		t.Fatalf("creates on b=%d target retries=%d", creates.Load(), service.metrics.totalTargetRetries.Load())
	}
}

// B9: a fallback transport error after the request may have been sent is
// still not replayed, and the client message carries no upstream URL.
func TestResponsesWebsocketFallbackTransportErrorHidesUpstreamURL(t *testing.T) {
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err == nil {
			_ = c.Close() // request sent, no response: ambiguous
		}
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	errObj, _ := event["error"].(map[string]any)
	message, _ := errObj["message"].(string)
	host := strings.TrimPrefix(upstream.URL, "http://")
	if event["type"] != "error" || message == "" || strings.Contains(message, host) || strings.Contains(message, "/v1/responses") || strings.Contains(message, "http://") {
		t.Fatalf("upstream URL leaked or event wrong: %v", event)
	}
	if posts.Load() != 1 {
		t.Fatalf("ambiguous POST replayed: posts=%d", posts.Load())
	}
}

// B3: a native upgrade handshake that times out sent no response.create. It
// fails over once like other pre-send network errors and marks the target.
func TestResponsesWebsocketHandshakeTimeoutFailsOverAndMarksTarget(t *testing.T) {
	var aRequests atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aRequests.Add(1)
		<-r.Context().Done() // never answers the upgrade
	}))
	defer a.Close()
	var creates atomic.Int32
	b := parityWSUpstream(t, &creates)
	service, server, _ := parityTwoTargetProxy(t,
		config.Target{Name: "a", EndpointType: config.EndpointTypeOpenAI, Endpoint: a.URL, APIKey: "a-key", ModelMappings: []config.ModelMapping{{Upstream: "A", Fallback: "codex-fast"}}},
		config.Target{Name: "b", EndpointType: config.EndpointTypeOpenAI, Endpoint: b.URL, APIKey: "b-key", ModelMappings: []config.ModelMapping{{Upstream: "B", Fallback: "codex-fast"}}})
	client := *service.httpClient
	client.Timeout = 300 * time.Millisecond // bounds the handshake below 5s
	service.httpClient = &client
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("handshake timeout did not fail over: %v", event)
	}
	stateA, _ := service.targetByName("a")
	if creates.Load() != 1 || aRequests.Load() != 1 || service.metrics.totalTargetRetries.Load() != 1 || stateA.Stats().TotalFailure != 1 {
		t.Fatalf("creates=%d a=%d retries=%d a stats=%+v", creates.Load(), aRequests.Load(), service.metrics.totalTargetRetries.Load(), stateA.Stats())
	}
}

// B3: parent cancellation during the native handshake never fails over.
func TestResponsesWebsocketHandshakeParentCancelDoesNotFailOver(t *testing.T) {
	started := make(chan struct{}, 1)
	ended := make(chan struct{}, 1)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		ended <- struct{}{}
	}))
	defer a.Close()
	var creates atomic.Int32
	b := parityWSUpstream(t, &creates)
	service, server, _ := parityTwoTargetProxy(t,
		config.Target{Name: "a", EndpointType: config.EndpointTypeOpenAI, Endpoint: a.URL, APIKey: "a-key", ModelMappings: []config.ModelMapping{{Upstream: "A", Fallback: "codex-fast"}}},
		config.Target{Name: "b", EndpointType: config.EndpointTypeOpenAI, Endpoint: b.URL, APIKey: "b-key", ModelMappings: []config.ModelMapping{{Upstream: "B", Fallback: "codex-fast"}}})
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake did not start")
	}
	_ = conn.Close() // client leaves: parent context canceled
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake was not canceled")
	}
	stateA, _ := service.targetByName("a")
	deadline := time.Now().Add(2 * time.Second)
	for stateA.inFlight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if creates.Load() != 0 || service.metrics.totalTargetRetries.Load() != 0 || stateA.Stats().TotalFailure != 0 {
		t.Fatalf("canceled handshake failed over or marked: creates=%d retries=%d stats=%+v", creates.Load(), service.metrics.totalTargetRetries.Load(), stateA.Stats())
	}
}

// B4: an upstream event the adapter rejects ends the turn with an error event;
// the client session stays usable.
func TestResponsesWebsocketFallbackInvalidEventKeepsSession(t *testing.T) {
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if posts.Add(1) == 1 {
			fmt.Fprint(w, "data: {not json\n\n")
			return
		}
		fmt.Fprint(w, paritySSECompleted)
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	errObj, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(502) || errObj["code"] != "upstream_invalid_event" || !strings.Contains(fmt.Sprint(errObj["message"]), "not replayed") {
		t.Fatalf("event=%v", event)
	}
	// Invalid upstream content is still upstream failure evidence (unchanged);
	// clear the resulting cooldown so the next turn can be routed.
	state, _ := service.targetByName("test-target")
	if state.Stats().TotalFailure != 1 {
		t.Fatalf("invalid upstream event health: %+v", state.Stats())
	}
	state.MarkSuccess(time.Now())
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "again"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("session did not continue: %v", event)
	}
	if posts.Load() != 2 {
		t.Fatalf("posts=%d", posts.Load())
	}
}

// B4: an upstream error body over the native message limit is reported as
// upstream_event_too_large instead of dropping the client socket.
func TestResponsesWebsocketFallbackOversizedErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		chunk := strings.Repeat("x", 64<<10)
		for written := 0; written <= responsesWebsocketMaxMessage; written += len(chunk) {
			if _, err := fmt.Fprint(w, chunk); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	errObj, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(502) || errObj["code"] != "upstream_event_too_large" {
		t.Fatalf("event=%v", event)
	}
}

// B7: native handshake 429s follow the HTTP AIMD rule: no signal per key
// attempt; one signal only when every key is exhausted.
func TestResponsesWebsocketHandshake429AIMDMatchesHTTP(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keys     []string
		wantRate int64
	}{
		{"one key limited, next key succeeds", []string{"limited-key", "healthy-key"}, 64},
		{"all keys exhausted", []string{"limited-key"}, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer limited-key" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded"}}`)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "ok", "status": "completed"}})
				_, _, _ = conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, _ := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			state.keyPool = newKeyPool(state.target.Name, tc.keys, "", service.logger)
			manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 64, RMin: 1, RMax: 64, AIWindow: time.Hour})
			manager.InitTarget("test-target", len(tc.keys))
			service.SetAdmissionManager(manager)
			conn := dialWebsocketTest(t, server)
			_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil {
				t.Fatal(err)
			}
			waitWebsocketSlotsReleased(t, service)
			if got := manager.Snapshot("test-target").Rate; got != tc.wantRate {
				t.Fatalf("AIMD rate=%d want %d (event=%v)", got, tc.wantRate, event)
			}
		})
	}
}

// B3 (pooled HTTP->WS): a handshake timeout fails over once and marks the
// target; it never falls back to HTTP on the same target.
func TestResponsesWSHandshakeTimeoutFailsOverToNextTarget(t *testing.T) {
	var aPosts atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			aPosts.Add(1)
		}
		<-r.Context().Done()
	}))
	defer a.Close()
	var bPosts atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		bPosts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_b","object":"response","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer b.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{
		{Name: "a", EndpointType: "openai", Endpoint: a.URL, APIKey: "a-key", PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "A", Fallback: "openai-test"}}},
		{Name: "b", EndpointType: "openai", Endpoint: b.URL, APIKey: "b-key", ModelMappings: []config.ModelMapping{{Upstream: "B", Fallback: "openai-test"}}},
	}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client := *s.httpClient
	client.Timeout = 300 * time.Millisecond
	s.httpClient = &client
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("client", "client")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("client")
	s.affinity.Set(affinityKey("192.0.2.1", principal.Name, "openai-test"), "a", time.Now())
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"openai-test","input":"hello"}`))
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	stateA, _ := s.targetByName("a")
	if w.Code != 200 || aPosts.Load() != 0 || bPosts.Load() != 1 || stateA.Stats().TotalFailure != 1 {
		t.Fatalf("status=%d aPosts=%d bPosts=%d a stats=%+v body=%s", w.Code, aPosts.Load(), bPosts.Load(), stateA.Stats(), w.Body.String())
	}
}

// parityBlockingWriter blocks every write until released, so the pooled lane's
// consumer stops reading frames.
type parityBlockingWriter struct {
	header  http.Header
	release chan struct{}
	status  int
	n       atomic.Int64
}

func (w *parityBlockingWriter) Header() http.Header { return w.header }
func (w *parityBlockingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *parityBlockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// B8: a lane closed because the downstream consumer fell behind is not an
// upstream failure.
func TestResponsesWSBacklogCloseIsNotUpstreamFailure(t *testing.T) {
	previous := responsesWSFrameBacklog
	responsesWSFrameBacklog = 50 * time.Millisecond
	defer func() { responsesWSFrameBacklog = previous }()
	upstream := newCoderWSUpstream(t, func(send func(string) error) {
		for i := 0; i < 40; i++ {
			if send(`{"type":"response.output_text.delta","delta":"x"}`) != nil {
				return
			}
		}
		_ = send(`{"type":"response.completed","response":{"id":"r","status":"completed"}}`)
	})
	s := newWSTestService(t, upstream.URL, true)
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("client", "client")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("client")
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"openai-test","input":"hello","stream":true}`))
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := &parityBlockingWriter{header: make(http.Header), release: make(chan struct{})}
	go func() {
		time.Sleep(400 * time.Millisecond) // well past the backlog limit
		close(w.release)
	}()
	s.ServeHTTP(w, r)
	state, _ := s.targetByName("openai")
	if stats := state.Stats(); stats.TotalFailure != 0 || state.IsMuted(time.Now()) {
		t.Fatalf("slow downstream counted as upstream failure: %+v", stats)
	}
}

func newCoderWSUpstream(t *testing.T, turn func(send func(string) error)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		turn(func(message string) error {
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			return conn.WriteMessage(websocket.TextMessage, []byte(message))
		})
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	return server
}
