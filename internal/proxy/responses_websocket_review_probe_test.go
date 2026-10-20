package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestResponsesWebsocketHandshakeRejectionEndsHealthProbe(t *testing.T) {
	for _, tc := range []struct {
		status    int
		noPool    bool
		unhealthy bool
	}{
		{http.StatusInternalServerError, false, true},
		{http.StatusServiceUnavailable, false, true},
		{http.StatusForbidden, false, false},
		{http.StatusTooManyRequests, true, true},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var handshakes, creates atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if handshakes.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					fmt.Fprint(w, `{"error":{"type":"server_error","code":"fixture_rejection"}}`)
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
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "recovered", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
				_, _, _ = conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			state, _ := service.targetByName("test-target")
			if tc.noPool {
				state.keyPool = nil
			}
			service.quietPeriod = time.Nanosecond
			state.MarkFailure(time.Now().Add(-time.Hour), time.Nanosecond)
			conn := dialWebsocketTest(t, server)
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "probe"}); err != nil {
				t.Fatal(err)
			}
			if event := readWebsocketEvent(t, conn); event["type"] != "error" || event["status"] != float64(tc.status) {
				t.Fatalf("first event=%v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			if state.probeSlotBusy(time.Now()) {
				t.Error("completed HTTP rejection left the half-open probe occupied")
			}
			stats := state.Stats()
			if tc.unhealthy {
				if stats.TotalFailure != 2 || stats.ConsecutiveFailure != 2 || stats.LastFailure.Before(time.Now().Add(-time.Minute)) {
					t.Errorf("ordinary upstream failure was not recorded: %+v", stats)
				}
			} else if stats.TotalFailure != 1 || stats.ConsecutiveFailure != 0 || state.IsMuted(time.Now()) {
				t.Errorf("reachable request rejection did not finish the probe: %+v", stats)
			}
			if handshakes.Load() != 1 || creates.Load() != 0 || len(recorder.snapshot()) != 0 {
				t.Fatalf("rejected handshake replayed or charged: handshakes=%d creates=%d usage=%d", handshakes.Load(), creates.Load(), len(recorder.snapshot()))
			}
			// The deliberately short test cooldown has expired. Recovery must
			// start a new probe rather than wait for an orphaned two-minute lease.
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "next"}); err != nil {
				t.Fatal(err)
			}
			if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
				t.Fatalf("next turn after cooldown=%v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			if handshakes.Load() != 2 || creates.Load() != 1 || state.probeSlotBusy(time.Now()) || service.metrics.totalFailures.Load() != 1 || service.metrics.totalSuccess.Load() != 1 {
				t.Fatalf("next turn lifecycle mismatch: handshakes=%d creates=%d stats=%+v", handshakes.Load(), creates.Load(), state.Stats())
			}
		})
	}
}
