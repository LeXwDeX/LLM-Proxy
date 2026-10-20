//go:build e2e && live

package e2e

// Real-upstream check of the Responses WebSocket paths against the Codex CLI
// proxy (cliproxy). It calls a real (OpenAI-family) model, so it only runs
// with the extra "live" tag and when the operator supplies credentials:
//
//	set -a; . ./cliproxy.env; set +a   # 内含 CLIPROXY_BASE_URL / CLIPROXY_DEV_KEY
//	go test -tags 'e2e live' -count=1 -v -run Live ./test/e2e/
//
// CLIPROXY_DEV_KEY is read from the environment only and never printed.
// CLIPROXY_MODEL picks the upstream model (default gpt-6-luna).

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func liveTarget(t *testing.T, preferWS bool) (string, string) {
	t.Helper()
	base, key := os.Getenv("CLIPROXY_BASE_URL"), os.Getenv("CLIPROXY_DEV_KEY")
	if base == "" || key == "" {
		t.Skip("CLIPROXY_BASE_URL / CLIPROXY_DEV_KEY not set")
	}
	model := os.Getenv("CLIPROXY_MODEL")
	if model == "" {
		model = "gpt-6-luna"
	}
	tg := target(t, map[string]any{"endpoint_type": "openai", "usage_adapter": "standard",
		"endpoint": strings.TrimRight(base, "/") + "/v1", "api_key": key, "supports_responses": true,
		"prefer_websocket": preferWS,
		"model_mappings":   []any{map[string]any{"upstream": model, "fallback": "live-codex"}}})
	name, ck := client(t, []string{tg}, nil)
	return name, ck
}

const livePrompt = "Reply with the single word OK."

func TestLiveCodexNativeWebSocket(t *testing.T) {
	name, key := liveTarget(t, false)
	wsURL := "ws" + strings.TrimPrefix(w.base, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		t.Fatalf("dial proxy: %v (resp %v)", err, resp)
	}
	defer conn.Close()
	turn := func(prev string) map[string]any {
		req := map[string]any{"type": "response.create", "model": "live-codex", "store": false,
			"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": livePrompt}}}}}
		if prev != "" {
			req["previous_response_id"] = prev
		}
		evs := wsTurn(t, conn, req)
		last := evs[len(evs)-1]
		t.Logf("turn: %d events, terminal %v", len(evs), last["type"])
		if last["type"] != "response.completed" {
			t.Fatalf("terminal event %v", last)
		}
		return last
	}
	first := turn("")
	id, _ := dig(first, "response/id").(string)
	turn(id)
	for _, ev := range usageFor(t, name, 2) {
		if ev.StatusCode != 200 || ev.UsageStatus != "complete" || ev.InputTokens+ev.CachedTokens == 0 || ev.OutputTokens == 0 {
			t.Errorf("usage event %+v", ev)
		}
	}
}

func TestLiveCodexPooledWebSocket(t *testing.T) {
	name, key := liveTarget(t, true)
	arrayInput := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": livePrompt}}}}
	for _, stream := range []bool{false, true} {
		resp, body := call(t, key, "POST", "/v1/responses", map[string]any{"model": "live-codex", "stream": stream, "store": false,
			"input": arrayInput}, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("stream=%v: HTTP %d %.300s", stream, resp.StatusCode, body)
		}
		if stream && !strings.Contains(string(body), "response.completed") {
			t.Fatalf("stream missing response.completed: %.300s", body)
		}
	}
	for _, ev := range usageFor(t, name, 2) {
		if ev.StatusCode != 200 || ev.UsageStatus != "complete" || ev.OutputTokens == 0 {
			t.Errorf("usage event %+v", ev)
		}
	}
}

// F12: OpenAI's HTTP Responses API accepts a plain-string input, but the
// upstream WebSocket mode rejects it. The pooled HTTP->WS path forwards the
// body verbatim, so a common client request fails with 400 instead of being
// normalised (or served over HTTP).
func TestLiveCodexPooledStringInput(t *testing.T) {
	_, key := liveTarget(t, true)
	resp, body := call(t, key, "POST", "/v1/responses", map[string]any{"model": "live-codex", "store": false, "input": livePrompt}, nil)
	if resp.StatusCode != 200 {
		t.Skipf("KNOWN GAP F12 [defect]: string input over pooled upstream WS — HTTP %d %.200s", resp.StatusCode, body)
	}
	t.Errorf("gap F12 now behaves as desired: promote it to a regression test")
}
