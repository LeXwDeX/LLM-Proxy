package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// websocketCompletingUpstream accepts one native upstream WebSocket and answers
// every response.create with response.completed, counting what it received.
func websocketCompletingUpstream(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			requests.Add(1)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_big", "model": "Codex-Upstream", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// readLargeWebsocketEvent is readWebsocketEvent with a deadline that survives
// multi-MiB messages on a slow -race runner (the shared helper allows 3s).
func readLargeWebsocketEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	var event map[string]any
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatal(err)
	}
	return event
}

// A native client message above the former 8 MiB read limit but below the
// upstream's own limit must reach the upstream (#238).
func TestResponsesWebsocketAcceptsMessageAboveFormerEightMiBLimit(t *testing.T) {
	var requests atomic.Int32
	upstream := websocketCompletingUpstream(t, &requests)
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	payload := map[string]any{"type": "response.create", "model": "codex-fast", "input": strings.Repeat("a", 9<<20)}
	if err := conn.WriteJSON(payload); err != nil {
		t.Fatal(err)
	}
	event := readLargeWebsocketEvent(t, conn)
	if event["type"] != "response.completed" {
		t.Fatalf("9 MiB message was not forwarded: %v", event)
	}
	if requests.Load() != 1 {
		t.Fatalf("upstream requests=%d, want 1", requests.Load())
	}
}

func TestResponsesWebsocketDefaultMessageLimitIsBelowUpstream30MB(t *testing.T) {
	if responsesWebsocketMaxMessage != 29<<20 {
		t.Fatalf("default limit=%d, want 29 MiB", responsesWebsocketMaxMessage)
	}
	service, _, _ := websocketTestProxy(t, "http://127.0.0.1:1")
	if got := service.websocketMessageLimit(); got != responsesWebsocketMaxMessage {
		t.Fatalf("unconfigured service limit=%d, want default %d", got, responsesWebsocketMaxMessage)
	}
	if got := service.SetWebsocketMaxMessage(5 << 20); got != 5<<20 || service.websocketMessageLimit() != 5<<20 {
		t.Fatalf("configured limit=%d", got)
	}
	if got := service.SetWebsocketMaxMessage(0); got != responsesWebsocketMaxMessage {
		t.Fatalf("non-positive limit must fall back to the default, got %d", got)
	}
	if got := service.SetWebsocketMaxMessage(maxRequestBodyBytes * 2); got != maxRequestBodyBytes {
		t.Fatalf("limit above the request body cap must be clamped, got %d", got)
	}
}

// An over-limit client message is rejected with a normal 413 error event: the
// session survives, nothing reaches the upstream, and the target is not
// penalized (#238).
func TestResponsesWebsocketOversizedMessageReturns413AndKeepsSession(t *testing.T) {
	var requests atomic.Int32
	upstream := websocketCompletingUpstream(t, &requests)
	service, server, _ := websocketTestProxy(t, upstream.URL)
	service.SetWebsocketMaxMessage(1 << 20)
	conn := dialWebsocketTest(t, server)
	oversized := map[string]any{"type": "response.create", "model": "codex-fast", "input": strings.Repeat("a", 2<<20)}
	if err := conn.WriteJSON(oversized); err != nil {
		t.Fatal(err)
	}
	event := readLargeWebsocketEvent(t, conn)
	errObj, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(413) || errObj["code"] != "request_too_large" {
		t.Fatalf("oversized message must get a 413 request_too_large event, got %v", event)
	}
	if message, _ := errObj["message"].(string); !strings.Contains(message, "1 MiB") {
		t.Fatalf("message should name the configured limit: %q", message)
	}
	if requests.Load() != 0 {
		t.Fatalf("oversized message reached the upstream: %d", requests.Load())
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if next := readLargeWebsocketEvent(t, conn); next["type"] != "response.completed" {
		t.Fatalf("session did not survive the rejected message: %v", next)
	}
	if requests.Load() != 1 || service.metrics.totalRequests.Load() != 1 || service.metrics.totalFailures.Load() != 0 {
		t.Fatalf("upstream=%d requests=%d failed=%d", requests.Load(), service.metrics.totalRequests.Load(), service.metrics.totalFailures.Load())
	}
}

// Beyond the drain cap the socket is still closed with 1009, so abusive
// traffic cannot make the proxy read without bound.
func TestResponsesWebsocketMessageBeyondDrainCapCloses1009(t *testing.T) {
	var requests atomic.Int32
	upstream := websocketCompletingUpstream(t, &requests)
	service, server, _ := websocketTestProxy(t, upstream.URL)
	service.SetWebsocketMaxMessage(1 << 20)
	service.websocketDrainCap.Store(2 << 20)
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": strings.Repeat("a", 4<<20)})
	var event map[string]any
	conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	err := conn.ReadJSON(&event)
	if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Fatalf("err=%v event=%v, want close 1009", err, event)
	}
	if requests.Load() != 0 {
		t.Fatalf("upstream requests=%d", requests.Load())
	}
}

// The SSE adapter enforces the configured limit, not a package constant.
func TestResponsesWebsocketWriterHonorsConfiguredLimit(t *testing.T) {
	sse := &responsesWebsocketWriter{header: make(http.Header), maxMessage: 1024}
	if _, err := sse.Write([]byte("data: " + strings.Repeat("x", 2048))); err == nil || sse.rejectCode != "upstream_event_too_large" {
		t.Fatalf("oversized SSE event: err=%v code=%q", err, sse.rejectCode)
	}
	failure := &responsesWebsocketWriter{header: make(http.Header), status: http.StatusInternalServerError, maxMessage: 1024}
	if _, err := failure.Write([]byte(strings.Repeat("x", 2048))); err == nil || failure.rejectCode != "upstream_event_too_large" {
		t.Fatalf("oversized error body: err=%v code=%q", err, failure.rejectCode)
	}
}
