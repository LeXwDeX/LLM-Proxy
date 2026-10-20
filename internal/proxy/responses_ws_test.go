package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

func newWSTestService(t *testing.T, endpoint string, enabled bool) *Service {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{
		Name: "openai", EndpointType: "openai", Endpoint: endpoint, APIKey: "upstream-test-key", PreferWebSocket: enabled,
		ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
	}}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func wsTestRequest(t *testing.T, s *Service, client, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients(client, client)); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate(client)
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestResponsesWSReuseAndAccounting(t *testing.T) {
	var upgrades, posts, creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			http.Error(w, "HTTP was used", 500)
			return
		}
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer upstream-test-key" {
			http.Error(w, "wrong handshake", 400)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		upgrades.Add(1)
		for {
			_, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var payload map[string]any
			if json.Unmarshal(msg, &payload) != nil || payload["type"] != "response.create" || payload["model"] != "UpstreamCase" || payload["stream"] != nil {
				t.Errorf("invalid create: %s", msg)
				return
			}
			n := creates.Add(1)
			if n > 1 && payload["previous_response_id"] != nil {
				t.Error("proxy invented conversation continuation")
			}
			id := fmt.Sprintf("resp_%d", n)
			for _, event := range []string{
				`{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`,
				`{"type":"response.function_call_arguments.delta","delta":"{}","item_id":"call_1"}`,
				`{"type":"response.output_text.delta","delta":"hello"}`,
				`{"type":"response.completed","response":{"id":"` + id + `","model":"UpstreamCase","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`,
			} {
				if c.Write(r.Context(), websocket.MessageText, []byte(event)) != nil {
					return
				}
			}
		}
	}))
	defer upstream.Close()
	s := newWSTestService(t, upstream.URL, true)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	recorder := &adapterEventRecorder{}
	s.SetUsageRecorder(recorder)
	for i := 0; i < 2; i++ {
		w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hello","stream":true}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.completed\n") || !strings.Contains(w.Body.String(), "response.function_call_arguments.delta") {
			t.Fatalf("response %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	// The first lane still holds resp_1 for a possible continuation, so the
	// second fresh request opens its own lane (bounded by the per-client cap).
	if upgrades.Load() != 2 || creates.Load() != 2 || posts.Load() != 0 {
		t.Fatalf("upgrades=%d creates=%d HTTP=%d", upgrades.Load(), creates.Load(), posts.Load())
	}
	if len(recorder.events) != 2 || recorder.events[0].InputTokens != 8 || recorder.events[0].CachedTokens != 2 || recorder.events[0].OutputTokens != 5 || recorder.events[0].CostIncomplete {
		t.Fatalf("accounting: %+v", recorder.events)
	}
	if s.TargetStatuses(time.Now())[0].InFlight != 0 {
		t.Fatal("request capacity leaked")
	}
}

func TestResponsesWSFallbackAndEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, path, body         string
		enabled                  bool
		handshakeStatus, wantGET int
	}{
		{"unsupported", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`, true, 426, 1},
		{"disabled", "/v1/responses", `{"model":"openai-test","input":"hi"}`, false, 426, 0},
		{"chat", "/v1/chat/completions", `{"model":"openai-test","messages":[]}`, true, 426, 0},
		{"background", "/v1/responses", `{"model":"openai-test","input":"hi","background":true}`, true, 426, 0},
		{"unauthorized", "/v1/responses", `{"model":"openai-test","input":"hi"}`, true, 401, 1},
		{"forbidden", "/v1/responses", `{"model":"openai-test","input":"hi"}`, true, 403, 1},
		{"rate_limited", "/v1/responses", `{"model":"openai-test","input":"hi"}`, true, 429, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets, posts atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
					http.Error(w, "handshake", tc.handshakeStatus)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"http_response","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer u.Close()
			s := newWSTestService(t, u.URL, tc.enabled)
			w := wsTestRequest(t, s, "client", tc.path, tc.body)
			wantPOST, wantStatus := int32(1), 200
			if tc.handshakeStatus == 401 || tc.handshakeStatus == 403 || tc.handshakeStatus == 429 {
				wantPOST, wantStatus = 0, tc.handshakeStatus
			}
			if gets.Load() != int32(tc.wantGET) || posts.Load() != wantPOST || w.Code != wantStatus {
				t.Fatalf("GET=%d POST=%d response=%d %s", gets.Load(), posts.Load(), w.Code, w.Body.String())
			}
		})
	}
}

func TestResponsesWSNoReplayAfterSend(t *testing.T) {
	var creates, posts atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			http.Error(w, "duplicate", 500)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err == nil {
			creates.Add(1)
		}
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	recorder := &adapterEventRecorder{}
	s.SetUsageRecorder(recorder)
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
	if creates.Load() != 1 || posts.Load() != 0 || w.Code != 502 {
		t.Fatalf("creates=%d POST=%d status=%d", creates.Load(), posts.Load(), w.Code)
	}
	if len(recorder.events) != 1 || !recorder.events[0].CostIncomplete {
		t.Fatalf("ambiguous generation was not recorded incomplete: %+v", recorder.events)
	}
}

func TestResponsesWSCanceledHandshakeDoesNotFallback(t *testing.T) {
	var posts atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		<-r.Context().Done()
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"openai-test","input":"hi"}`))
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("client", "client")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("client")
	r = r.WithContext(auth.WithPrincipal(ctx, principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if posts.Load() != 0 {
		t.Fatalf("canceled request retried HTTP %d times", posts.Load())
	}
}

func TestResponsesWSHandshakeTimeoutAndCapacityDoNotFallback(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		t.Run(fmt.Sprint("capacity=", capacity), func(t *testing.T) {
			var posts atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.WriteHeader(200)
					return
				}
				<-r.Context().Done()
			}))
			defer u.Close()
			s := newWSTestService(t, u.URL, true)
			s.requestTimeout.Store(int64(8 * time.Second))
			if capacity {
				s.responsesWS.mu.Lock()
				for i := 0; i < responsesWSMax; i++ {
					s.responsesWS.entries[&responsesWSConn{created: time.Now(), used: time.Now(), busy: true}] = struct{}{}
				}
				s.responsesWS.mu.Unlock()
			}
			w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hello"}`)
			// A handshake timeout never falls back; a full pool sends nothing over
			// WS, so it uses HTTP for this request instead.
			want, wantPosts := 504, int32(0)
			if capacity {
				want, wantPosts = 200, 1
			}
			if posts.Load() != wantPosts || w.Code != want {
				t.Fatalf("status=%d HTTP POST=%d", w.Code, posts.Load())
			}
		})
	}
}
