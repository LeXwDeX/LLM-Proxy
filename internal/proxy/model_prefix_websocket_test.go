package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/gorilla/websocket"
)

func TestModelPrefixWebsocketTransports(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "http over upstream ws"
		if native {
			name = "native downstream ws"
		}
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var req map[string]any
				if err := conn.ReadJSON(&req); err != nil {
					t.Error(err)
					return
				}
				if req["model"] != "Vendor/Fable5" {
					t.Errorf("WS model=%v", req["model"])
				}
				conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_prefix", "model": "Vendor/Fable5", "status": "completed", "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}}})
			}))
			defer up.Close()
			cfg := prefixConfig(t, up.URL, "openai", "Vendor/", true)
			cfg.Targets[0].PreferWebSocket = true
			s, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			rec := &websocketUsageRecorder{}
			s.SetUsageRecorder(rec)
			principal := newAllowAllPrincipal(t, "fixture")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
			}))
			defer server.Close()
			if native {
				conn := dialWebsocketTest(t, server)
				defer conn.Close()
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "fable", "input": "hi"}); err != nil {
					t.Fatal(err)
				}
				if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
					t.Fatalf("event=%v", event)
				}
			} else {
				resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"fable","input":"hi"}`))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("status=%d", resp.StatusCode)
				}
			}
			deadline := time.Now().Add(time.Second)
			for len(rec.snapshot()) < 1 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			events := rec.snapshot()
			if len(events) != 1 || events[0].Model != "fable5" {
				t.Fatalf("usage=%+v", events)
			}
		})
	}
}
