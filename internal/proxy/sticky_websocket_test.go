package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/gorilla/websocket"
)

// A native WS turn goes through the real session and ServeHTTP: when its sticky
// target is cooling for the turn's model, the turn ends with one error event
// carrying the retry hint, not the substitute target's 400.
func TestNativeWebSocketTurnGetsStickyRetryEvent(t *testing.T) {
	env := newIncidentEnv(t, "120", http.StatusBadRequest)
	env.bind("sess-ws") // same session and client model as the WS turn below
	env.limitModel("bailian", incidentDeepseekUpstream, 90*time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.svc.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), env.principal)))
	}))
	t.Cleanup(server.Close)
	headers := http.Header{"X-Session-Id": []string{"sess-ws"}}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, headers)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (HTTP %d)", err, resp.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "deepseek", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	event := readWebsocketEvent(t, conn)
	if event["type"] != "error" || event["status"] != float64(503) {
		t.Fatalf("event = %v, want a 503 error event", event)
	}
	if ra, ok := event["retry_after"].(float64); !ok || ra < 80 || ra > 91 {
		t.Fatalf("retry_after = %v, want about 90", event["retry_after"])
	}
	errObj, _ := event["error"].(map[string]any)
	if errObj["type"] != "upstream_temporarily_unavailable" || strings.Contains(strings.ToLower(toJSON(errObj)), "thinking mode") {
		t.Fatalf("event error = %v, want the proxy's own error without the substitute's body", errObj)
	}
	if m := env.svc.MetricsSnapshot(); m.TotalStickyRetryHinted != 1 {
		t.Fatalf("sticky_retry_hinted = %d, want 1", m.TotalStickyRetryHinted)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
