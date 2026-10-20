package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/gorilla/websocket"
)

func TestResponsesWebsocketHTTPFallbackAccountsTerminalOutcome(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_1", "status": "failed", "error": map[string]any{"type": "invalid_request_error", "code": "invalid_input"}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.failed" {
		t.Fatalf("event=%v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	state, _ := service.targetByName("test-target")
	if service.metrics.totalFailures.Load() != 1 || service.metrics.totalSuccess.Load() != 0 || state.IsMuted(time.Now()) {
		t.Fatal("request-level terminal failure over HTTP fallback must not count as success or mute a healthy target")
	}
	if events := recorder.snapshot(); len(events) != 1 || events[0].StatusCode != 400 || events[0].InputTokens != 10 || events[0].CostIncomplete {
		t.Fatalf("fallback lost final failed-response usage/status: %+v", events)
	}
}

func TestResponsesWebsocketUnsupportedHandshakeFallsBackWithoutLosingHTTPError(t *testing.T) {
	var handshakes, posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			handshakes.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		posts.Add(1)
		var body map[string]any
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "Codex-Upstream" || body["service_tier"] != "ultrafast" || r.Header.Get("Authorization") != "Bearer sk-test-key" {
			t.Errorf("fallback did not preserve assembled request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "permission_error", "code": "policy_denied", "message": "fixture denial"}})
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	event := readWebsocketEvent(t, conn)
	detail, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(403) || detail["code"] != "policy_denied" {
		t.Fatalf("HTTP status and business error must survive native WS fallback: %v", event)
	}
	if handshakes.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("handshakes=%d posts=%d", handshakes.Load(), posts.Load())
	}
	waitWebsocketSlotsReleased(t, service)
}

func TestResponsesWebsocketFirstResponseTimeoutStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.ReadMessage()
		conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	service.requestTimeout.Store(int64(150 * time.Millisecond))
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "error" || event["status"] != float64(504) {
		t.Fatalf("timeout must return a deterministic 504 event: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
}

type blockingWSUsageRecorder struct {
	*websocketUsageRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingWSUsageRecorder) Record(event usage.Event) error {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return r.websocketUsageRecorder.Record(event)
}

func TestResponsesWebsocketCloseWaitsForNativeAccounting(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.ReadMessage()
		conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
		conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	blocked := &blockingWSUsageRecorder{websocketUsageRecorder: recorder, entered: make(chan struct{}), release: make(chan struct{})}
	service.SetUsageRecorder(blocked)
	var release sync.Once
	defer release.Do(func() { close(blocked.release) })
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("event=%v", event)
	}
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("usage recorder was not entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before native accounting finished: %v", err)
	case <-time.After(5100 * time.Millisecond):
	}
	release.Do(func() { close(blocked.release) })
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after accounting was released")
	}
	if events := recorder.snapshot(); len(events) != 1 || events[0].CostIncomplete {
		t.Fatalf("completed usage was not durably observed before Close: %+v", events)
	}
}
