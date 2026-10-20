package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/gorilla/websocket"
)

func setWebsocketTestDuration(t *testing.T, target *time.Duration, value time.Duration) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

func expectWebsocketClose(t *testing.T, conn *websocket.Conn, code int) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != code {
			t.Fatalf("expected close code %d, got %v", code, err)
		}
		return
	}
}

// completingUpstream answers each create with a completed response. When hold
// is non-nil it first sends response.created and waits for hold to close.
func completingUpstream(t *testing.T, started chan<- struct{}, hold <-chan struct{}) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request map[string]any
			if conn.ReadJSON(&request) != nil {
				return
			}
			if hold != nil {
				conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress"}})
				if started != nil {
					started <- struct{}{}
				}
				<-hold
			}
			conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}}})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestResponsesWebsocketIdleSessionIsClosed(t *testing.T) {
	setWebsocketTestDuration(t, &responsesWebsocketIdleTimeout, 200*time.Millisecond)
	upstream := completingUpstream(t, nil, nil)
	_, server, _ := websocketTestProxy(t, upstream.URL)

	idle := dialWebsocketTest(t, server)
	expectWebsocketClose(t, idle, websocket.CloseNormalClosure)

	// The timer restarts after each turn rather than limiting the whole session.
	active := dialWebsocketTest(t, server)
	time.Sleep(120 * time.Millisecond)
	active.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	if event := readWebsocketEvent(t, active); event["type"] != "response.completed" {
		t.Fatalf("turn did not complete: %v", event)
	}
	expectWebsocketClose(t, active, websocket.CloseNormalClosure)
}

func TestResponsesWebsocketLimitsSessionsPerClient(t *testing.T) {
	previous := responsesWebsocketPerClient
	responsesWebsocketPerClient = 1
	t.Cleanup(func() { responsesWebsocketPerClient = previous })
	upstream := completingUpstream(t, nil, nil)
	_, server, _ := websocketTestProxy(t, upstream.URL)
	first := dialWebsocketTest(t, server)
	headers := http.Header{"Authorization": []string{"Bearer proxy-client-secret"}}
	url := "ws" + strings.TrimPrefix(server.URL, "http") + responsesPath
	if _, resp, err := websocket.DefaultDialer.Dial(url, headers); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second session for the same client was not rejected with 429: err=%v resp=%v", err, resp)
	}
	first.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, _, err := websocket.DefaultDialer.Dial(url, headers)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session slot not released after disconnect: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResponsesWebsocketDrainFinishesActiveTurn(t *testing.T) {
	started := make(chan struct{}, 1)
	hold := make(chan struct{})
	upstream := completingUpstream(t, started, hold)
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not started")
	}
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		drained <- service.DrainResponsesWebsockets(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	close(hold)
	if event := readWebsocketEvent(t, conn); event["type"] != "response.created" {
		t.Fatalf("unexpected first event: %v", event)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("active turn was not allowed to finish during drain: %v", event)
	}
	expectWebsocketClose(t, conn, websocket.CloseGoingAway)
	if err := <-drained; err != nil {
		t.Fatalf("drain did not complete: %v", err)
	}
	if events := recorder.snapshot(); len(events) != 1 || events[0].CostIncomplete || events[0].InputTokens != 3 {
		t.Fatalf("drained turn lost complete usage: %+v", events)
	}
	if _, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, http.Header{"Authorization": []string{"Bearer proxy-client-secret"}}); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new session accepted while draining: err=%v", err)
	}
}

func TestResponsesWebsocketDrainClosesIdleSession(t *testing.T) {
	upstream := completingUpstream(t, nil, nil)
	service, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := service.DrainResponsesWebsockets(ctx); err != nil {
		t.Fatalf("idle drain: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("idle session delayed drain")
	}
	expectWebsocketClose(t, conn, websocket.CloseGoingAway)
}

func TestResponsesWebsocketSendsKeepalivePings(t *testing.T) {
	setWebsocketTestDuration(t, &responsesWebsocketPingInterval, 50*time.Millisecond)
	upstream := completingUpstream(t, nil, nil)
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	var pings atomic.Int32
	conn.SetPingHandler(func(data string) error {
		pings.Add(1)
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for pings.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("keepalive pings=%d", pings.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResponsesWebsocketOnlyInterceptsResponsesUpgrade(t *testing.T) {
	var upstreamPaths atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	service, _, _ := websocketTestProxy(t, upstream.URL)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"codex-fast","messages":[]}`))
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "ws-client")))
	w := httptest.NewRecorder()
	service.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "WebSocket is supported only") || upstreamPaths.Load() != 1 {
		t.Fatalf("non-Responses upgrade header hijacked the request: status=%d body=%s", w.Code, w.Body.String())
	}
}
