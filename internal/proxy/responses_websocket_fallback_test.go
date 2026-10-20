package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

func TestResponsesWebsocketHTTPFallbackContinuationIdentity(t *testing.T) {
	for _, change := range []string{"none", "credential", "endpoint"} {
		t.Run(change, func(t *testing.T) {
			var posts atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
					"id": "resp_1", "status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2},
				}})
				fmt.Fprintf(w, "data: %s\n\n", event)
			})
			upstream := httptest.NewServer(handler)
			defer upstream.Close()
			other := httptest.NewServer(handler)
			defer other.Close()
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{
				Name: "test-target", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "fixture-key-a",
				ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "test-model"}},
			}}}
			service, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			principal := newAllowAllPrincipal(t, "fallback-client")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
			}))
			defer server.Close()
			conn := dialWebsocketTest(t, server)
			send := func(previous bool) map[string]any {
				t.Helper()
				request := map[string]any{"type": "response.create", "model": "test-model", "input": "full history"}
				if previous {
					request["previous_response_id"] = "resp_1"
				}
				if err := conn.WriteJSON(request); err != nil {
					t.Fatal(err)
				}
				return readWebsocketEvent(t, conn)
			}
			if event := send(false); event["type"] != "response.completed" {
				t.Fatalf("first event=%v", event)
			}
			waitWebsocketSlotsReleased(t, service)
			switch change {
			case "credential":
				cfg.Targets[0].APIKey = "fixture-key-b"
			case "endpoint":
				cfg.Targets[0].Endpoint = other.URL
			}
			if change != "none" {
				if err := service.ApplyConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			event := send(true)
			if change == "none" {
				if event["type"] != "response.completed" || posts.Load() != 2 {
					t.Fatalf("same identity continuation failed: event=%v posts=%d", event, posts.Load())
				}
				return
			}
			if event["type"] != "error" || event["status"] != float64(http.StatusConflict) || posts.Load() != 1 {
				t.Fatalf("continuation crossed changed %s: event=%v posts=%d", change, event, posts.Load())
			}
			waitWebsocketSlotsReleased(t, service)
			if event := send(false); event["type"] != "response.completed" || posts.Load() != 2 {
				t.Fatalf("full history did not recover: event=%v posts=%d", event, posts.Load())
			}
		})
	}
}
