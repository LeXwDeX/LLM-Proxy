package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

// Issue #153 B6: an upstream close frame on the pooled path keeps the
// io.ErrUnexpectedEOF classification and its close code reaches diagnostics,
// without replaying the generation.
func TestResponsesWSPooledUpstreamCloseCodeDiagnostics(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			var upgrades, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					http.Error(w, "replayed", 500)
					return
				}
				upgrades.Add(1)
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_x","status":"in_progress"}}`))
				_ = c.Close(websocket.StatusInternalError, "private-close-reason")
			}))
			defer upstream.Close()
			logs := &diagnosticLogBuffer{}
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{
				Name: "openai", EndpointType: "openai", Endpoint: upstream.URL, APIKey: "upstream-test-key", PreferWebSocket: true,
				ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
			}}}
			s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.responsesWS.prewarm = false
			body := `{"model":"openai-test","input":"hi"}`
			if stream {
				body = `{"model":"openai-test","input":"hi","stream":true}`
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			r = r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "client")))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if !stream && w.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if upgrades.Load() != 1 || posts.Load() != 0 {
				t.Fatalf("generation replayed: upgrades=%d posts=%d", upgrades.Load(), posts.Load())
			}
			want := "stream copy failed"
			if !stream {
				want = "websocket generation transport failure"
			}
			var found map[string]any
			logs.mu.Lock()
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == want {
					found = rec
				}
			}
			text := logs.String()
			logs.mu.Unlock()
			if found == nil {
				t.Fatalf("no %q log: %s", want, text)
			}
			if found["upstream_close_code"] != "1011" {
				t.Fatalf("upstream close code missing: %v", found)
			}
			if stream && found["error"] != "unexpected_eof" {
				t.Fatalf("close frame no longer classified as unexpected EOF: %v", found)
			}
			if strings.Contains(text, "private-close-reason") {
				t.Fatal("upstream close reason text leaked into logs")
			}
		})
	}
}

func TestResponsesWSWrapUpstreamCloseKeepsUnexpectedEOF(t *testing.T) {
	closeErr := websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "x"}
	err := wrapResponsesWSUpstreamClose(closeErr)
	var got websocket.CloseError
	if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &got) || got.Code != websocket.StatusPolicyViolation {
		t.Fatalf("wrapped error lost classification: %v", err)
	}
	if responsesWSUpstreamCloseCode(err) != "1008" || responsesWSUpstreamCloseCode(errors.New("other")) != "unknown" {
		t.Fatal("close code diagnostic")
	}
	if plain := errors.New("net"); wrapResponsesWSUpstreamClose(plain) != plain {
		t.Fatal("non-close errors must stay unchanged")
	}
}
