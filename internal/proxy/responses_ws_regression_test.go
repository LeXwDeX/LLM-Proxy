package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

func TestResponsesWSHandshakeNetworkFailover(t *testing.T) {
	for _, preferred := range []bool{false, true} {
		t.Run(fmt.Sprint(preferred), func(t *testing.T) {
			dead := httptest.NewServer(http.NotFoundHandler())
			deadURL := dead.URL
			dead.Close()
			var reached atomic.Int32
			live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				if r.Method == http.MethodPost {
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"model":"LiveCase"`) {
						t.Errorf("fallback reused previous target model: %s", body)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"live","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}`))
					return
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				_, body, err := c.Read(r.Context())
				if err != nil {
					return
				}
				if !strings.Contains(string(body), `"model":"LiveCase"`) {
					t.Errorf("fallback reused previous target model: %s", body)
				}
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"live","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
			}))
			defer live.Close()
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{
				{Name: "dead", EndpointType: "openai", Endpoint: deadURL, APIKey: "fixture-dead", PreferWebSocket: preferred, ModelMappings: []config.ModelMapping{{Upstream: "DeadCase", Fallback: "openai-test"}}},
				{Name: "live", EndpointType: "openai", Endpoint: live.URL, APIKey: "fixture-live", PreferWebSocket: preferred, ModelMappings: []config.ModelMapping{{Upstream: "LiveCase", Fallback: "openai-test"}}},
			}}
			s, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.affinity.Set(affinityKey("192.0.2.1", "client", "openai-test"), "dead", time.Now())
			w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
			if w.Code != 200 || reached.Load() != 1 || s.metrics.totalTargetRetries.Load() != 1 {
				t.Fatalf("connection failure before generation did not fail over: status=%d live_requests=%d retries=%d body=%s", w.Code, reached.Load(), s.metrics.totalTargetRetries.Load(), w.Body.String())
			}
			for _, status := range s.TargetStatuses(time.Now()) {
				if status.InFlight != 0 {
					t.Fatalf("target %s leaked in-flight capacity", status.Name)
				}
			}
		})
	}
}

func TestResponsesWSKeyFailureUsesHealthyKeyOnNextRequest(t *testing.T) {
	for _, phase := range []string{"handshake", "first_error", "late_error", "late_failed"} {
		for _, tc := range []struct {
			name, body, reason string
			status             int
		}{
			{"rate429", `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"rate limit"}}`, "rate_limited", http.StatusTooManyRequests},
			{"quota402", `{"error":{"type":"billing_error","code":"organization_spend_limit_exceeded","message":"quota exhausted"}}`, "billing_error", http.StatusPaymentRequired},
			{"quota429", `{"error":{"type":"insufficient_quota","code":"organization_spend_limit_exceeded","message":"quota exhausted"}}`, "quota_exceeded_api", http.StatusTooManyRequests},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				status, errorBody, wantReason := tc.status, tc.body, tc.reason
				var attempts, creates, posts atomic.Int32
				u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					if r.Method == http.MethodPost {
						posts.Add(1)
						http.Error(w, "unexpected replay", 500)
						return
					}
					failedKey := r.Header.Get("Authorization") == "Bearer fixture-key-a"
					if failedKey && phase == "handshake" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = w.Write([]byte(errorBody))
						return
					}
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					if _, _, err := c.Read(r.Context()); err != nil {
						return
					}
					creates.Add(1)
					if !failedKey {
						_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"healthy","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
						return
					}
					if phase != "first_error" {
						_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"failed","status":"in_progress"}}`))
					}
					var envelope map[string]json.RawMessage
					_ = json.Unmarshal([]byte(errorBody), &envelope)
					event := fmt.Sprintf(`{"type":"error","status":%d,"error":%s}`, status, envelope["error"])
					if phase == "late_failed" {
						event = fmt.Sprintf(`{"type":"response.failed","status":%d,"response":{"id":"failed","status":"failed","error":%s}}`, status, envelope["error"])
					}
					_ = c.Write(r.Context(), websocket.MessageText, []byte(event))
				}))
				defer u.Close()
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{Name: "openai", EndpointType: "openai", Endpoint: u.URL, APIKey: "fixture-key-a", APIKeys: []string{"fixture-key-b"}, PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}}}}}
				s, err := NewService(cfg, newTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				// Exact upstream handshake counts below; background spare lanes are covered
				// by responses_ws_prewarm_test.go.
				s.responsesWS.prewarm = false
				w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
				if phase == "handshake" {
					// A rejected upgrade sent no generation, so the key pool may rotate
					// within this request exactly as it does for an HTTP response.
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"healthy"`) || attempts.Load() < 2 || creates.Load() != 1 || posts.Load() != 0 {
						t.Fatalf("handshake rejection did not rotate keys in-request: status=%d attempts=%d creates=%d posts=%d body=%s", w.Code, attempts.Load(), creates.Load(), posts.Load(), w.Body.String())
					}
				} else {
					wantStatus := status
					if strings.HasPrefix(phase, "late_") {
						wantStatus = 200
					}
					if w.Code != wantStatus || attempts.Load() != 1 || posts.Load() != 0 {
						t.Fatalf("generation replayed or status changed: status=%d attempts=%d posts=%d body=%s", w.Code, attempts.Load(), posts.Load(), w.Body.String())
					}
				}
				attemptsBefore := attempts.Load()
				state, _ := s.targetByName("openai")
				state.keyPool.mu.Lock()
				first, second := state.keyPool.entries[0], state.keyPool.entries[1]
				state.keyPool.mu.Unlock()
				if !first.exhausted || first.exhaustReason != wantReason || !first.cooldownEnd.After(time.Now()) || second.exhausted || state.IsMuted(time.Now()) {
					t.Fatalf("incorrect key feedback: exhausted=%v reason=%s healthy_exhausted=%v muted=%v", first.exhausted, first.exhaustReason, second.exhausted, state.IsMuted(time.Now()))
				}
				if tc.name == "quota429" && (!first.cooldownEnd.Equal(permanentCooldownEnd) || !strings.Contains(first.lastError, "organization_spend_limit_exceeded")) {
					t.Fatalf("quota key received transient rate-limit cooling: end=%s error=%s", first.cooldownEnd, first.lastError)
				}
				w = wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"next"}`)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"healthy"`) || attempts.Load() != attemptsBefore+1 || posts.Load() != 0 {
					t.Fatalf("next request did not use healthy key: status=%d attempts=%d creates=%d posts=%d body=%s", w.Code, attempts.Load(), creates.Load(), posts.Load(), w.Body.String())
				}
				if s.TargetStatuses(time.Now())[0].InFlight != 0 {
					t.Fatal("request capacity leaked")
				}
			})
		}
	}
}

func TestResponsesWSHandshakeErrorBodyPreservation(t *testing.T) {
	// Incompressible text ensures the compressed error also exceeds Dial's 1KiB limit.
	noise := make([]byte, 12000)
	rng := rand.New(rand.NewSource(1))
	for i := range noise {
		noise[i] = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"[rng.Intn(62)]
	}
	want := []byte(`{"error":{"type":"permission_error","message":"` + string(noise) + `"}}`)
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint("gzip=", compressed), func(t *testing.T) {
			wire := want
			if compressed {
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				_, _ = zw.Write(want)
				_ = zw.Close()
				wire = buf.Bytes()
			}
			var requests atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", fmt.Sprint(len(wire)))
				if compressed {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(http.StatusForbidden)
				// Delay the body until after acquire returns to catch premature dial cancellation.
				w.(http.Flusher).Flush()
				time.Sleep(20 * time.Millisecond)
				_, _ = w.Write(wire)
			}))
			defer u.Close()
			for _, preferred := range []bool{false, true} {
				s := newWSTestService(t, u.URL, preferred)
				if compressed {
					transport := s.httpClient.Transport.(*http.Transport).Clone()
					transport.DisableCompression = true
					s.httpClient.Transport = transport
				}
				w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
				got := w.Body.Bytes()
				if w.Header().Get("Content-Length") != fmt.Sprint(len(wire)) || !bytes.Equal(got, wire) {
					t.Errorf("prefer=%v HTTP error body or headers changed: bytes=%d length=%s expected=%d", preferred, len(got), w.Header().Get("Content-Length"), len(wire))
				}
				if compressed && w.Header().Get("Content-Encoding") == "gzip" {
					gr, err := gzip.NewReader(bytes.NewReader(got))
					if err != nil {
						t.Fatalf("invalid gzip error response: %v", err)
					}
					got, err = io.ReadAll(gr)
					_ = gr.Close()
					if err != nil {
						t.Fatalf("incomplete gzip error response: %v", err)
					}
				}
				if w.Code != http.StatusForbidden || !bytes.Equal(got, want) || !json.Valid(got) {
					t.Errorf("prefer=%v body truncated: status=%d body_bytes=%d wire_bytes=%d content_length=%s", preferred, w.Code, len(got), w.Body.Len(), w.Header().Get("Content-Length"))
				}
			}
			if requests.Load() != 2 {
				t.Fatalf("handshake made extra requests: %d", requests.Load())
			}
		})
	}
}

func TestResponsesWSHandshakeTimeoutEndsBeforeErrorBody(t *testing.T) {
	want := `{"error":{"type":"permission_error","message":"complete body"}}`
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(want)))
		w.WriteHeader(http.StatusForbidden)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(want))
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	// The headers complete within the handshake timeout. The retained body
	// belongs to the request context and may arrive after that timeout.
	s.httpClient.Timeout = 100 * time.Millisecond
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if w.Code != http.StatusForbidden || w.Body.String() != want {
		t.Fatalf("handshake timeout canceled returned error body: status=%d body=%s", w.Code, w.Body.String())
	}
	if s.TargetStatuses(time.Now())[0].InFlight != 0 {
		t.Fatal("error body leaked target capacity")
	}
}

func TestResponsesWSSentGenerationDoesNotFailOver(t *testing.T) {
	var creates, liveRequests atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		liveRequests.Add(1)
		_, _ = w.Write([]byte(`{"id":"unexpected_replay"}`))
	}))
	defer live.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{
		{Name: "first", EndpointType: "openai", Endpoint: u.URL, APIKey: "fixture-first", PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "FirstCase", Fallback: "openai-test"}}},
		{Name: "live", EndpointType: "openai", Endpoint: live.URL, APIKey: "fixture-live", PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "LiveCase", Fallback: "openai-test"}}},
	}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.affinity.Set(affinityKey("192.0.2.1", "client", "openai-test"), "first", time.Now())
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if w.Code != http.StatusBadGateway || creates.Load() != 1 || liveRequests.Load() != 0 || s.metrics.totalTargetRetries.Load() != 0 {
		t.Fatalf("generation replayed after send: status=%d creates=%d fallback_requests=%d retries=%d", w.Code, creates.Load(), liveRequests.Load(), s.metrics.totalTargetRetries.Load())
	}
	for _, status := range s.TargetStatuses(time.Now()) {
		if status.InFlight != 0 {
			t.Fatalf("target %s leaked in-flight capacity", status.Name)
		}
	}
}
