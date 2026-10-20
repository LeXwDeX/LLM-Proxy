package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

// keyedWSUpstream rejects the upgrade for keys in reject with the given status
// and body; any other key completes one generation per create.
func keyedWSUpstream(t *testing.T, reject map[string]bool, status int, body string) *httptest.Server {
	t.Helper()
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"ok","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func twoKeyWSService(t *testing.T, endpoint string) *Service {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{
		Name: "openai", EndpointType: "openai", Endpoint: endpoint, APIKey: "fixture-key-a", APIKeys: []string{"fixture-key-b"},
		PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
	}}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestResponsesWSAccountLevelHandshakeConvergenceMutesTarget(t *testing.T) {
	u := keyedWSUpstream(t, map[string]bool{"fixture-key-a": true, "fixture-key-b": true}, http.StatusUnauthorized,
		`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"bad key"}}`)
	s := twoKeyWSService(t, u.URL)
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("converged account failure was not passed through: %d %s", w.Code, w.Body.String())
	}
	// Same account-level failure on every key is a target failure, as on HTTP.
	if state, _ := s.targetByName("openai"); !state.IsMuted(time.Now()) {
		t.Fatal("account-level handshake convergence did not count as a target failure")
	}
}
