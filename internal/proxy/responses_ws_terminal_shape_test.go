package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

func TestResponsesWSTerminalResponseShape(t *testing.T) {
	for _, typ := range []string{"response.completed", "response.failed", "response.incomplete"} {
		for _, tc := range []struct{ name, field string }{
			{"missing", ""}, {"null", `,"response":null`}, {"number", `,"response":42`},
			{"string", `,"response":"bad"`}, {"boolean", `,"response":false`}, {"array", `,"response":[]`},
		} {
			t.Run(typ+"/"+tc.name, func(t *testing.T) {
				var creates, posts, upgrades atomic.Int32
				u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						posts.Add(1)
						http.Error(w, "replay", 500)
						return
					}
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					upgrades.Add(1)
					for {
						if _, _, err = c.Read(r.Context()); err != nil {
							return
						}
						creates.Add(1)
						if err = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"`+typ+`"`+tc.field+`}`)); err != nil {
							return
						}
					}
				}))
				defer u.Close()
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{
					{Name: "primary", EndpointType: "openai", Endpoint: u.URL, APIKey: "first-key", APIKeys: []string{"second-key"}, PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}}},
					{Name: "secondary", EndpointType: "openai", Endpoint: u.URL, APIKey: "fallback-key", PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}}},
				}}
				s, err := NewService(cfg, newTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				recorder := &adapterEventRecorder{}
				s.SetUsageRecorder(recorder)
				w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi","stream":false}`)
				if w.Code != http.StatusBadGateway {
					t.Errorf("invalid terminal: status=%d body=%s", w.Code, w.Body.String())
				}
				if creates.Load() != 1 || posts.Load() != 0 || upgrades.Load() != 1 {
					t.Errorf("generation replayed: creates=%d posts=%d upgrades=%d", creates.Load(), posts.Load(), upgrades.Load())
				}
				s.responsesWS.mu.Lock()
				entries := len(s.responsesWS.entries)
				s.responsesWS.mu.Unlock()
				if entries != 0 {
					t.Errorf("invalid connection retained: %d", entries)
				}
				for _, status := range s.TargetStatuses(time.Now()) {
					if status.InFlight != 0 {
						t.Error("target admission was not released")
					}
				}
				if len(recorder.events) != 1 || !recorder.events[0].CostIncomplete {
					t.Errorf("generation not recorded incomplete: %+v", recorder.events)
				}
			})
		}
	}
}

func TestResponsesWSTerminalResponsePreservesObject(t *testing.T) {
	const snapshot = `{"id":"resp_valid","status":"completed","output":[],"extra":{"integer":9007199254740993,"false":false,"zero":0,"null":null}}`
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":`+snapshot+`}`))
		_, _, _ = c.Read(r.Context())
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi","stream":false}`)
	if w.Code != http.StatusOK || w.Body.String() != snapshot {
		t.Fatalf("snapshot altered: %d %s", w.Code, w.Body.String())
	}
}
