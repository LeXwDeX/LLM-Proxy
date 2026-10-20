package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

// wsIdleUpstream drops a socket that stays silent for idle (pings count as
// activity), like provider front ends do between a client's turns.
type wsIdleUpstream struct {
	idle time.Duration
	// stallFirst stops reading on the first socket after its first turn, so
	// its pings go unanswered while the socket stays open.
	stallFirst bool
	// slowTurn delays each answer without reading, leaving pings unanswered.
	slowTurn time.Duration
	release  chan struct{}

	upgrades, creates atomic.Int32
}

func (u *wsIdleUpstream) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	attempt := u.upgrades.Add(1)
	extend := func() {
		if u.idle > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(u.idle))
		}
	}
	extend()
	conn.SetPingHandler(func(data string) error {
		extend()
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	for {
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		n := u.creates.Add(1)
		if u.slowTurn > 0 {
			time.Sleep(u.slowTurn)
		}
		id := fmt.Sprintf("resp_%d", n)
		if conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}) != nil {
			return
		}
		extend()
		if u.stallFirst && attempt == 1 {
			<-u.release
			return
		}
	}
}

func wsIdleProxy(t *testing.T, u *wsIdleUpstream, pingAfter, pingTimeout time.Duration) (*Service, *websocket.Conn) {
	t.Helper()
	setWebsocketTestDuration(t, &responsesWebsocketUpstreamPingAfter, pingAfter)
	setWebsocketTestDuration(t, &responsesWebsocketUpstreamPingTimeout, pingTimeout)
	u.release = make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(func() { close(u.release); upstream.Close() })
	service := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	principal := newAllowAllPrincipal(t, "ws-client")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(func() { server.Close(); service.Close() })
	return service, dialWebsocketTest(t, server)
}

func wsIdleTurn(t *testing.T, conn *websocket.Conn, previous string) map[string]any {
	t.Helper()
	request := map[string]any{"type": "response.create", "model": "test-model", "input": "hi"}
	if previous != "" {
		request["previous_response_id"] = previous
	}
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	return readWebsocketEvent(t, conn)
}

// Between turns the proxy pings an idle upstream, so a provider's idle cutoff
// does not force the client to resend its full input.
func TestResponsesWebsocketNativePingsIdleUpstreamBetweenTurns(t *testing.T) {
	u := &wsIdleUpstream{idle: 300 * time.Millisecond}
	service, conn := wsIdleProxy(t, u, 100*time.Millisecond, time.Second)
	if event := wsIdleTurn(t, conn, ""); event["type"] != "response.completed" {
		t.Fatalf("first turn: %v", event)
	}
	time.Sleep(900 * time.Millisecond) // three idle cutoffs without pings
	event := wsIdleTurn(t, conn, "resp_1")
	if event["type"] != "response.completed" {
		t.Fatalf("continuation after idle period: %v", event)
	}
	if u.upgrades.Load() != 1 || u.creates.Load() != 2 {
		t.Fatalf("handshakes=%d creates=%d, want one socket for both turns", u.upgrades.Load(), u.creates.Load())
	}
	if m := service.MetricsSnapshot().ResponsesWS; m.NativeHandshake != 1 || m.NativePingFail != 0 {
		t.Fatalf("native metrics: %+v", m)
	}
}

// An unanswered ping closes the idle socket; the next turn follows the
// existing rules: a continuation gets 409, a full-input turn dials anew.
func TestResponsesWebsocketNativePingFailureClosesIdleUpstream(t *testing.T) {
	u := &wsIdleUpstream{stallFirst: true}
	service, conn := wsIdleProxy(t, u, 50*time.Millisecond, 100*time.Millisecond)
	if event := wsIdleTurn(t, conn, ""); event["type"] != "response.completed" {
		t.Fatalf("first turn: %v", event)
	}
	waitForWS(t, "the failed ping", func() bool { return service.MetricsSnapshot().ResponsesWS.NativePingFail == 1 })
	event := wsIdleTurn(t, conn, "resp_1")
	if event["type"] != "error" || event["status"] != float64(http.StatusConflict) {
		t.Fatalf("continuation on a closed socket: %v", event)
	}
	if event := wsIdleTurn(t, conn, ""); event["type"] != "response.completed" {
		t.Fatalf("full-input turn after ping failure: %v", event)
	}
	if u.upgrades.Load() != 2 || u.creates.Load() != 2 {
		t.Fatalf("handshakes=%d creates=%d", u.upgrades.Load(), u.creates.Load())
	}
}

// A turn in progress is never pinged or interrupted, however long the
// upstream takes to answer.
func TestResponsesWebsocketNativePingSkipsActiveTurn(t *testing.T) {
	u := &wsIdleUpstream{slowTurn: 400 * time.Millisecond}
	service, conn := wsIdleProxy(t, u, 30*time.Millisecond, 50*time.Millisecond)
	for i, previous := range []string{"", "resp_1"} {
		if event := wsIdleTurn(t, conn, previous); event["type"] != "response.completed" {
			t.Fatalf("turn %d: %v", i, event)
		}
	}
	if m := service.MetricsSnapshot().ResponsesWS; u.upgrades.Load() != 1 || m.NativePingFail != 0 {
		t.Fatalf("slow turn interrupted: handshakes=%d metrics=%+v", u.upgrades.Load(), m)
	}
}
