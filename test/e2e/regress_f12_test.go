//go:build e2e

package e2e

import "testing"

// TestPooledResponsesWSStringInput (F12, #154): upstream WebSocket mode rejects
// a plain-string input, so the pooled HTTP->WS path must wrap it as one user
// message and the request completes over WebSocket, not HTTP.
func TestPooledResponsesWSStringInput(t *testing.T) {
	_, name, key := wsTarget(t, map[string]any{"prefer_websocket": true})
	resp, body := call(t, key, "POST", "/v1/responses", map[string]any{"model": "e2e-ws", "input": "[[fake usage.in=30 usage.out=7]]"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d %.200s", resp.StatusCode, body)
	}
	if rec := lastRecord(t, w.fakeA, "/openai/v1/responses"); !rec.WebSocket {
		t.Fatalf("upstream call did not use WebSocket: %+v", rec)
	}
	if ev := usageFor(t, name, 1)[0]; ev.InputTokens != 30 || ev.OutputTokens != 7 || ev.UsageStatus != "complete" {
		t.Fatalf("billed in=%d out=%d status=%q", ev.InputTokens, ev.OutputTokens, ev.UsageStatus)
	}
}
