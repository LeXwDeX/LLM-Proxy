//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsInput is the array form of a Responses input; upstream WebSocket mode
// rejects the plain-string shorthand.
func wsInput(text string) []any {
	return []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
}

func wsTurn(t *testing.T, conn *websocket.Conn, payload map[string]any) []map[string]any {
	t.Helper()
	if err := conn.WriteJSON(payload); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	var evs []map[string]any
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("ws read after %d events: %v", len(evs), err)
		}
		evs = append(evs, ev)
		switch ev["type"] {
		case "response.completed", "response.failed", "response.incomplete", "error":
			return evs
		}
	}
}

func wsTarget(t *testing.T, extra map[string]any) (string, string, string) {
	spec := map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": w.urlA + "/openai/v1",
		"api_key": "sk-up-ws", "supports_responses": true,
		"model_mappings": []any{map[string]any{"upstream": "gpt-ws", "fallback": "e2e-ws"}}}
	for k, v := range extra {
		spec[k] = v
	}
	tg := target(t, spec)
	name, key := client(t, []string{tg}, nil)
	return tg, name, key
}

// TestNativeResponsesWebSocket: client WS → proxy → upstream WS, several
// turns on one connection, continuation, an upstream error frame, billing.
func TestNativeResponsesWebSocket(t *testing.T) {
	_, name, key := wsTarget(t, nil)
	wsURL := "ws" + strings.TrimPrefix(w.base, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	defer conn.Close()

	evs := wsTurn(t, conn, map[string]any{"type": "response.create", "model": "e2e-ws", "input": wsInput("[[fake usage.in=50 usage.out=10 usage.cached=20]]")})
	last := evs[len(evs)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("turn 1: %v", last)
	}
	id, _ := dig(last, "response/id").(string)
	evs = wsTurn(t, conn, map[string]any{"type": "response.create", "model": "e2e-ws", "previous_response_id": id, "input": wsInput("[[fake usage.in=50 usage.out=10 usage.cached=20]]")})
	if last := evs[len(evs)-1]; last["type"] != "response.completed" {
		t.Fatalf("turn 2: %v", last)
	}
	evs = wsTurn(t, conn, map[string]any{"type": "response.create", "model": "e2e-ws", "input": wsInput("[[fake preset=rate_limit]]")})
	if last := evs[len(evs)-1]; last["type"] != "error" {
		t.Fatalf("turn 3 should surface the upstream error frame: %v", last)
	}
	rec := lastRecord(t, w.fakeA, "/openai/v1/responses")
	if !rec.WebSocket || rec.Model != "gpt-ws" || rec.Auth != "bearer" {
		t.Fatalf("upstream turn %+v", rec)
	}
	evsBilled := usageFor(t, name, 2)
	ok := 0
	for _, ev := range evsBilled {
		if ev.StatusCode == 200 {
			checkBilled(t, ev, billed{in: 30, out: 10, cached: 20})
			ok++
		}
	}
	if ok != 2 {
		t.Fatalf("billed %d successful turns, want 2", ok)
	}
}

// TestPooledHTTPToUpstreamWebSocket: HTTP clients, prefer_websocket target.
// The proxy must speak WS upstream. Fresh requests prefer new lanes up to the
// per-client cap (4); beyond that idle lanes are reused.
func TestPooledHTTPToUpstreamWebSocket(t *testing.T) {
	_, name, key := wsTarget(t, map[string]any{"prefer_websocket": true})
	before := w.fakeA.LastSeq()
	for i, stream := range []bool{false, true, false, true, false, true} {
		resp, body := call(t, key, "POST", "/v1/responses", map[string]any{"model": "e2e-ws", "stream": stream,
			"input": wsInput("[[fake usage.in=40 usage.out=8]]")}, nil)
		if resp.StatusCode != 200 || !strings.Contains(string(body), "fake reply") {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
		}
	}
	upgrades, turns := 0, 0
	for _, r := range w.fakeA.RequestsSince(before) {
		if r.WebSocket && r.Status == 101 {
			upgrades++
		} else if r.WebSocket {
			turns++
		}
	}
	if turns != 6 || upgrades < 1 || upgrades > 4 {
		t.Fatalf("upstream WS upgrades=%d turns=%d; want 6 turns over at most 4 lanes", upgrades, turns)
	}
	for _, ev := range usageFor(t, name, 6) {
		checkBilled(t, ev, billed{in: 40, out: 8})
	}
}

// TestPooledWebSocketHandshakeFallback: an upstream that rejects the upgrade
// with 426 is served over HTTP and the client never notices.
func TestPooledWebSocketHandshakeFallback(t *testing.T) {
	_, _, key := wsTarget(t, map[string]any{"prefer_websocket": true,
		"custom_headers": map[string]any{"X-Fake-Scenario": "ws_status=426"}})
	resp, body := call(t, key, "POST", "/v1/responses", map[string]any{"model": "e2e-ws", "input": wsInput("hi")}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("fallback: %d %s", resp.StatusCode, body)
	}
	rec := lastRecord(t, w.fakeA, "/openai/v1/responses")
	if rec.WebSocket || rec.Method != "POST" {
		t.Fatalf("expected HTTP fallback, last upstream record %+v", rec)
	}
}
