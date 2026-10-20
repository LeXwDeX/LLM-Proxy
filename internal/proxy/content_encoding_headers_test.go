package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
	"github.com/gorilla/websocket"
)

func TestObservationRepeatedContentEncodingHTTP(t *testing.T) {
	for _, negotiate := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct {
				name     string
				headers  []string
				gzip     bool
				complete bool
			}{
				{"gzip_then_br", []string{"gzip", "br"}, true, false},
				{"empty_then_gzip", []string{"", "gzip"}, false, false},
				{"identity_then_unknown", []string{"identity", "unknown"}, false, false},
				{"single_mixed_case_ows", []string{"  GZip \t"}, true, true},
				{"single_gzip", []string{"gzip"}, true, true},
			} {
				t.Run(tc.name+map[bool]string{false: "/json", true: "/sse"}[streaming]+map[bool]string{false: "/no_accept_encoding", true: "/explicit_accept_encoding"}[negotiate], func(t *testing.T) {
					plain := []byte(`{"model":"test-model","usage":{"prompt_tokens":10,"completion_tokens":2},"choices":[]}`)
					contentType := "application/json"
					if streaming {
						contentType = "text/event-stream"
						plain = []byte("data: " + string(plain) + "\n\ndata: [DONE]\n\n")
					}
					wire := plain
					if tc.gzip {
						wire = observationFixture(t, "gzip", plain)
					}
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", contentType)
						for _, value := range tc.headers {
							w.Header().Add("Content-Encoding", value)
						}
						_, _ = w.Write(wire)
					}))
					defer upstream.Close()
					svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: "standard", Endpoint: upstream.URL, APIKey: "fixture"}}}, newTestLogger())
					if err != nil {
						t.Fatal(err)
					}
					defer svc.Close()
					recorder := &globalAuditRecorder{}
					svc.SetUsageRecorder(recorder)
					store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: 4096, ChannelBuffer: 1}, newTestLogger())
					defer store.Close()
					svc.traceStore = store
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
					if negotiate {
						req.Header.Set("Accept-Encoding", "gzip")
					}
					req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
					response := httptest.NewRecorder()
					svc.ServeHTTP(response, req)
					wantWire, wantHeaders := wire, len(tc.headers)
					// Preserve the prior default Transport's transparent single-gzip behavior.
					if !negotiate && tc.complete {
						wantWire = plain
						wantHeaders = 0
					}
					if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), wantWire) || len(response.Header().Values("Content-Encoding")) != wantHeaders {
						t.Fatalf("wire or encoding headers changed: status=%d headers=%v", response.Code, response.Header())
					}
					if calls.Load() != 1 || len(recorder.events) != 1 {
						t.Fatalf("replayed or missing usage: calls=%d events=%+v", calls.Load(), recorder.events)
					}
					event := recorder.events[0]
					if tc.complete {
						if event.InputTokens != 10 || event.OutputTokens != 2 || event.UsageStatus != "complete" || event.CostIncomplete {
							t.Fatalf("single gzip not observed: %+v", event)
						}
					} else {
						if event.UsageStatus != "incomplete" || !event.CostIncomplete || event.InputTokens != 0 || event.OutputTokens != 0 {
							t.Fatalf("ambiguous encoding incorrectly decoded: %+v", event)
						}
					}
					trace := store.Get("")
					if trace == nil {
						t.Fatal("missing trace")
					}
					want := wire
					if tc.complete {
						want = plain
					}
					if trace.ResponseBody != string(want) {
						t.Fatal("trace decoded an ambiguous encoding or lost valid single coding")
					}
				})
			}
		}
	}
}

func TestRepeatedEncodingNon2xxDoesNotClassifyRawPrefix(t *testing.T) {
	body := []byte(`{"error":{"code":"invalid_api_key","message":"invalid api key"}}`)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Add("Content-Encoding", "identity")
		w.Header().Add("Content-Encoding", "unknown")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	svc := newTestModelService(t, "openai", upstream.URL, func(target *config.Target) { target.APIKeys = []string{"second"} })
	defer svc.Close()
	req := statsRequest(t, "/v1/chat/completions")
	req.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	svc.ServeHTTP(response, req)
	if calls.Load() != 1 || response.Code != 401 || !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatalf("ambiguous raw error classified/replayed: calls=%d status=%d", calls.Load(), response.Code)
	}
	state, _ := svc.targetByName("test-target")
	state.keyPool.mu.Lock()
	defer state.keyPool.mu.Unlock()
	for _, key := range state.keyPool.entries {
		if key.exhausted || key.blocked {
			t.Fatal("ambiguous body changed key availability")
		}
	}
}

func TestRepeatedEncodingNativeWebsocketConsumers(t *testing.T) {
	for _, headers := range [][]string{{"gzip", "br"}, {"identity", "unknown"}, {"", "gzip"}} {
		t.Run(strings.Join(headers, "_"), func(t *testing.T) {
			plain := []byte(`{"error":{"code":"invalid_api_key","message":"invalid api key"}}`)
			body := plain
			if headers[0] == "gzip" {
				body = observationFixture(t, "gzip", plain)
			}
			var calls atomic.Int32
			client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 401, Header: http.Header{"Content-Encoding": headers}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
			})}
			req, _ := http.NewRequest(http.MethodPost, "http://fixture.invalid/v1/responses", strings.NewReader(`{"model":"m"}`))
			_, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
			var handshake *responsesWebsocketHandshakeError
			if !errors.As(err, &handshake) || handshake.complete || len(handshake.body) > 0 || calls.Load() != 1 {
				t.Fatalf("ambiguous handshake retained classification envelope: error=%v handshake=%+v calls=%d", err, handshake, calls.Load())
			}
			if len(handshake.header.Values("Content-Encoding")) != 2 {
				t.Fatal("handshake coding metadata discarded")
			}
		})
	}
	t.Run("fallback_no_raw_frame", func(t *testing.T) {
		var calls atomic.Int32
		plain := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"wrongly-decoded\"}}\n\n")
		source := &nativeReviewGzipBody{ReadCloser: io.NopCloser(bytes.NewReader(plain))}
		client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if r.Method == http.MethodGet {
				return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"identity", "unknown"}, "Content-Type": {"text/event-stream"}}, Body: source, Request: r}, nil
		})}
		req, _ := http.NewRequest(http.MethodPost, "http://fixture.invalid/v1/responses", strings.NewReader(`{"model":"m"}`))
		response, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		_ = response.Body.Close()
		if source.closed.Load() != 1 {
			t.Fatalf("unsupported coding source closed %d times", source.closed.Load())
		}
		if readErr == nil || len(data) > 0 || calls.Load() != 2 || response.Uncompressed || len(response.Header.Values("Content-Encoding")) != 2 {
			t.Fatalf("ambiguous fallback consumed raw frame: data=%q error=%v calls=%d headers=%v", data, readErr, calls.Load(), response.Header)
		}
	})
}

func TestRepeatedEncodingClaudeGuardDoesNotInspectWire(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for _, values := range [][]string{{"identity", "unknown"}, {"", "gzip"}, {"identity", ""}} {
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": values}}
		if claudeStreamGuardEnabled(req, response) {
			t.Fatalf("ambiguous wire enabled identity-only Claude guard: %v", values)
		}
	}
}

func TestRepeatedEncodingMinimaxDoesNotPeekRawError(t *testing.T) {
	wire := []byte(`{"base_resp":{"status_code":1008}}`)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Content-Encoding", "")
		w.Header().Add("Content-Encoding", "unknown")
		_, _ = w.Write(wire)
	}))
	defer upstream.Close()
	svc := newTestModelService(t, "openai", upstream.URL, func(target *config.Target) { target.UsageAdapter = "minimax"; target.APIKeys = []string{"second"} })
	defer svc.Close()
	req := statsRequest(t, "/v1/chat/completions")
	req.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	svc.ServeHTTP(response, req)
	if calls.Load() != 1 || response.Code != 200 || !bytes.Equal(response.Body.Bytes(), wire) {
		t.Fatalf("encoded MiniMax raw error triggered key switching: calls=%d status=%d", calls.Load(), response.Code)
	}
}

func TestRepeatedEncodingNativeGenerationDoesNotReplayOrLeak(t *testing.T) {
	for _, negotiate := range []bool{false, true} {
		for _, values := range [][]string{{"gzip", "br"}, {"identity", "unknown"}} {
			t.Run(strings.Join(values, "_")+map[bool]string{false: "/no_accept_encoding", true: "/explicit_accept_encoding"}[negotiate], func(t *testing.T) {
				plain := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"untrusted\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
				wire := plain
				if values[0] == "gzip" {
					wire = observationFixture(t, "gzip", plain)
				}
				var handshakes, posts atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						handshakes.Add(1)
						w.WriteHeader(404)
						return
					}
					posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					for _, v := range values {
						w.Header().Add("Content-Encoding", v)
					}
					_, _ = w.Write(wire)
				}))
				defer upstream.Close()
				svc, server, recorder := websocketTestProxy(t, upstream.URL)
				base := svc.httpClient.Transport
				if base == nil {
					base = http.DefaultTransport
				}
				var source atomic.Pointer[nativeReviewGzipBody]
				svc.httpClient.Transport = nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
					resp, err := base.RoundTrip(r)
					if err == nil && r.Method == http.MethodPost {
						body := &nativeReviewGzipBody{ReadCloser: resp.Body}
						source.Store(body)
						resp.Body = body
					}
					return resp, err
				})
				var conn *websocket.Conn
				if negotiate {
					conn = nativeReviewDialGzip(t, server)
				} else {
					var err error
					conn, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, nil)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
					t.Fatal(err)
				}
				if event := readWebsocketEvent(t, conn); event["type"] == "response.completed" {
					t.Fatalf("raw undecoded body generated terminal success: %v", event)
				}
				waitWebsocketSlotsReleased(t, svc)
				events := recorder.snapshot()
				if len(events) != 1 || !events[0].CostIncomplete || events[0].InputTokens != 0 || events[0].OutputTokens != 0 || handshakes.Load() != 1 || posts.Load() != 1 || svc.metrics.totalSuccess.Load() != 0 {
					t.Fatalf("unknown generation replayed/accepted or dropped accounting: events=%+v handshakes=%d posts=%d", events, handshakes.Load(), posts.Load())
				}
				if body := source.Load(); body == nil || body.closed.Load() != 1 {
					t.Fatalf("unknown native source not closed exactly once: %v", body)
				}
			})
		}
	}
}

func TestGLMEncodedRawSSEDoesNotClassifyQuota(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		trusted bool
		brotli  bool
	}{
		{"identity", []string{"identity"}, true, false},
		// br is decodable for usage only; the GLM probe still trusts identity alone.
		{"br_not_probed", []string{"br"}, false, true},
		{"unknown", []string{"x-unknown"}, false, false},
		{"repeated", []string{"identity", "unknown"}, false, false},
		{"empty_first", []string{"", "gzip"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errorWire := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"1308\",\"message\":\"quota exhausted\"}}\n\n")
			if tc.brotli {
				errorWire = observationFixture(t, "br", errorWire)
			}
			successWire := []byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2},\"choices\":[]}\n\ndata: [DONE]\n\n")
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					for _, v := range tc.headers {
						w.Header().Add("Content-Encoding", v)
					}
					_, _ = w.Write(errorWire)
				} else {
					_, _ = w.Write(successWire)
				}
			}))
			defer upstream.Close()
			svc := newTestModelService(t, "openai", upstream.URL, func(target *config.Target) { target.UsageAdapter = "glm"; target.APIKeys = []string{"second"} })
			defer svc.Close()
			recorder := &globalAuditRecorder{}
			svc.SetUsageRecorder(recorder)
			req := statsRequest(t, "/v1/chat/completions")
			req.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			svc.ServeHTTP(response, req)
			wantCalls := int32(1)
			wantWire := errorWire
			if tc.trusted {
				wantCalls = 2
				wantWire = successWire
			}
			if calls.Load() != wantCalls || response.Code != 200 || !bytes.Equal(response.Body.Bytes(), wantWire) {
				t.Fatalf("raw GLM body classified: calls=%d want=%d status=%d", calls.Load(), wantCalls, response.Code)
			}
			if !tc.trusted {
				if len(response.Header().Values("Content-Encoding")) != len(tc.headers) || len(recorder.events) != 1 || !recorder.events[0].CostIncomplete || recorder.events[0].UsageStatus != "incomplete" {
					t.Fatalf("unknown GLM wire/header/accounting changed: headers=%v events=%+v", response.Header(), recorder.events)
				}
				state, _ := svc.targetByName("test-target")
				state.keyPool.mu.Lock()
				defer state.keyPool.mu.Unlock()
				for _, key := range state.keyPool.entries {
					if key.blocked || key.exhausted {
						t.Fatal("unknown GLM body changed key availability")
					}
				}
			}
		})
	}
}

func TestGLMEncodedRaw429KeepsGenericRetry(t *testing.T) {
	for _, code := range []string{"1308", "1113"} {
		for _, tc := range []struct {
			name    string
			headers []string
			trusted bool
			brotli  bool
		}{
			{"identity", []string{"identity"}, true, false},
			// Non-2xx classification still reads only identity or gzip prefixes.
			{"br_not_classified", []string{"br"}, false, true},
			{"unknown", []string{"x-unknown"}, false, false},
			{"repeated", []string{"identity", "unknown"}, false, false},
		} {
			t.Run(code+"/"+tc.name, func(t *testing.T) {
				errorBody := []byte(`{"error":{"code":"` + code + `","message":"quota exhausted"}}`)
				if tc.brotli {
					errorBody = observationFixture(t, "br", errorBody)
				}
				var first, second atomic.Value
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if calls.Add(1) == 1 {
						first.Store(r.Header.Get("Authorization"))
						for _, v := range tc.headers {
							w.Header().Add("Content-Encoding", v)
						}
						w.WriteHeader(429)
						_, _ = w.Write(errorBody)
					} else {
						second.Store(r.Header.Get("Authorization"))
						_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
					}
				}))
				defer upstream.Close()
				svc := newTestModelService(t, "openai", upstream.URL, func(target *config.Target) { target.UsageAdapter = "glm"; target.APIKeys = []string{"second"} })
				defer svc.Close()
				req := statsRequest(t, "/v1/chat/completions")
				req.Header.Set("Accept-Encoding", "gzip")
				response := httptest.NewRecorder()
				svc.ServeHTTP(response, req)
				if response.Code != 200 || calls.Load() != 2 {
					t.Fatalf("HTTP429 retry behavior changed: status=%d calls=%d", response.Code, calls.Load())
				}
				sameKey := first.Load() == second.Load()
				if sameKey == tc.trusted {
					t.Fatalf("raw encoded GLM code altered generic same-key retry: trusted=%v sameKey=%v", tc.trusted, sameKey)
				}
				if !tc.trusted {
					state, _ := svc.targetByName("test-target")
					state.keyPool.mu.Lock()
					defer state.keyPool.mu.Unlock()
					for _, key := range state.keyPool.entries {
						if key.blocked || key.exhaustReason == glmExhaustReason || isGLMAccountLevelReason(key.exhaustReason) {
							t.Fatalf("raw encoded GLM quota/permanent feedback: %+v", key)
						}
					}
				}
			})
		}
	}
}
