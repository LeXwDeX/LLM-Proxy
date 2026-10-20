package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/gorilla/websocket"
)

type completionRecorder struct {
	mu     sync.Mutex
	events []requeststats.Event
	err    error
}

func (r *completionRecorder) Record(e requeststats.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}
func (r *completionRecorder) outcomes() []requeststats.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]requeststats.Outcome, len(r.events))
	for i, e := range r.events {
		out[i] = e.Outcome
	}
	return out
}
func assertCompletions(t *testing.T, r *completionRecorder, want ...requeststats.Outcome) {
	t.Helper()
	got := r.outcomes()
	if len(got) != len(want) {
		t.Fatalf("completions=%v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("completions=%v want %v", got, want)
		}
	}
}
func statsRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"test-model","stream":true,"messages":[],"input":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "stats-client")))
}

func TestRequestStatsHTTPCompletionAndWire(t *testing.T) {
	for _, tc := range []struct {
		name, path, typ, body string
		status                int
		want                  requeststats.Outcome
	}{
		{"json without usage", "/v1/chat/completions", "application/json", `{"choices":[{"finish_reason":"stop"}]}`, 200, requeststats.Success},
		{"json with usage", "/v1/chat/completions", "application/json", `{"usage":{"prompt_tokens":2,"completion_tokens":1}}`, 200, requeststats.Success},
		{"401", "/v1/chat/completions", "application/json", `{"error":"rejected"}`, 401, requeststats.Failure},
		{"503", "/v1/chat/completions", "application/json", `{"error":"down"}`, 503, requeststats.Failure},
		{"chat missing terminal", "/v1/chat/completions", "text/event-stream", "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n", 200, requeststats.Failure},
		{"chat done no usage", "/v1/chat/completions", "text/event-stream", "data: [DONE]\n\n", 200, requeststats.Success},
		{"responses done no usage", "/v1/responses", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", 200, requeststats.Success},
		{"responses failure synthetic 200", "/v1/responses", "text/event-stream", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", 200, requeststats.Failure},
		{"explicit finish error", "/v1/chat/completions", "application/json", `{"choices":[{"finish_reason":"error"}]}`, 200, requeststats.Failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.typ)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL)
			defer s.Close()
			rec := &completionRecorder{}
			s.SetRequestStatsRecorder(rec)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, statsRequest(t, tc.path))
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("wire changed: %d %s", w.Code, w.Body.String())
			}
			assertCompletions(t, rec, tc.want)
			if s.metrics.totalUsageRecordFailures.Load() != 0 {
				t.Fatal("statistics invoked billing persistence")
			}
		})
	}
}

func TestRequestStatsGzipSSELifecycleAndWire(t *testing.T) {
	var valid bytes.Buffer
	compressed := gzip.NewWriter(&valid)
	if _, err := io.WriteString(compressed, "data: [DONE]\n\n"); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		wire []byte
		want requeststats.Outcome
	}{
		{"malformed header", []byte("not a gzip stream"), requeststats.Failure},
		{"truncated header", []byte{0x1f, 0x8b, 0x08}, requeststats.Failure},
		{"valid terminal without usage", valid.Bytes(), requeststats.Success},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("explicit gzip request header was not forwarded")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(tc.wire)
			}))
			defer up.Close()
			s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL)
			defer s.Close()
			rec := &completionRecorder{}
			s.SetRequestStatsRecorder(rec)
			r := statsRequest(t, "/v1/chat/completions")
			// Explicit Accept-Encoding prevents net/http from decoding the body.
			r.Header.Set("Accept-Encoding", "gzip")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			assertCompletions(t, rec, tc.want)
			if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(w.Body.Bytes(), tc.wire) {
				t.Fatalf("compressed wire changed: status=%d encoding=%q body=%x", w.Code, w.Header().Get("Content-Encoding"), w.Body.Bytes())
			}
			// Statistics must not change the incumbent metrics/health settlement,
			// billing persistence or retry policy, even for invalid compressed SSE.
			metrics := s.MetricsSnapshot()
			if metrics.TotalSuccess != 1 || metrics.TotalFailures != 0 || metrics.TotalRetries != 0 || metrics.TotalUsageRecordFailures != 0 {
				t.Fatalf("existing settlement changed: %+v", metrics)
			}
		})
	}
}

func TestRequestStatsRecorderConfiguredConcurrent(t *testing.T) {
	s, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.HasRequestStatsRecorder() {
		t.Fatal("absent recorder reported configured")
	}
	rec := &completionRecorder{}
	s.SetRequestStatsRecorder(rec)
	if !s.HasRequestStatsRecorder() {
		t.Fatal("installed recorder reported absent")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.SetRequestStatsRecorder(nil)
			s.SetRequestStatsRecorder(rec)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = s.HasRequestStatsRecorder()
		}
	}()
	wg.Wait()
	s.SetRequestStatsRecorder(nil)
	if s.HasRequestStatsRecorder() {
		t.Fatal("removed recorder reported configured")
	}
}

type statsRoundTrip func(*http.Request) (*http.Response, error)

func (f statsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type statsBrokenRead struct{}

func (statsBrokenRead) Read([]byte) (int, error) { return 0, errors.New("upstream read failed") }
func (statsBrokenRead) Close() error             { return nil }

type statsBrokenWriter struct{ *httptest.ResponseRecorder }

func (statsBrokenWriter) Write([]byte) (int, error) { return 0, errors.New("downstream gone") }

func TestRequestStatsFailuresCancelAndLocal(t *testing.T) {
	s := newTestModelService(t, config.EndpointTypeOpenAI, "http://unused.invalid")
	defer s.Close()
	rec := &completionRecorder{}
	s.SetRequestStatsRecorder(rec)
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))
	s.ServeHTTP(httptest.NewRecorder(), statsRequest(t, "/unknown"))
	local := statsRequest(t, "/v1/models")
	local.Method = http.MethodGet
	s.ServeHTTP(httptest.NewRecorder(), local)
	localReject := local.Clone(local.Context())
	localReject.Header.Set(headerProxyTarget, "unknown")
	s.ServeHTTP(httptest.NewRecorder(), localReject)
	s.httpClient.Transport = statsRoundTrip(func(r *http.Request) (*http.Response, error) { return nil, context.Canceled })
	s.ServeHTTP(httptest.NewRecorder(), statsRequest(t, "/v1/chat/completions"))
	s.httpClient.Transport = statsRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: statsBrokenRead{}, Request: r}, nil
	})
	s.ServeHTTP(httptest.NewRecorder(), statsRequest(t, "/v1/chat/completions"))
	s.httpClient.Transport = statsRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})
	s.ServeHTTP(statsBrokenWriter{httptest.NewRecorder()}, statsRequest(t, "/v1/chat/completions"))
	assertCompletions(t, rec, requeststats.Failure, requeststats.Failure, requeststats.Success, requeststats.Failure, requeststats.Canceled, requeststats.Failure, requeststats.Canceled)
}

func TestRequestStatsAdmissionAndDeadline(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "canceled"}[canceled], func(t *testing.T) {
			s := newTestModelService(t, config.EndpointTypeOpenAI, "http://unused.invalid")
			defer s.Close()
			rec := &completionRecorder{}
			s.SetRequestStatsRecorder(rec)
			manager := admission.NewManager(newTestLogger(), admission.Options{RInit: 1, RMin: 1, RMax: 1})
			manager.InitTarget("test-target", 1)
			manager.TryAcquire("test-target")
			s.SetAdmissionManager(manager)
			r := statsRequest(t, "/v1/chat/completions")
			ctx, cancel := context.WithTimeout(r.Context(), time.Millisecond)
			defer cancel()
			if canceled {
				cancel()
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r.WithContext(ctx))
			if w.Code != 429 {
				t.Fatalf("admission status=%d", w.Code)
			}
			want := requeststats.Failure
			if canceled {
				want = requeststats.Canceled
			}
			assertCompletions(t, rec, want)
		})
	}
	s := newTestModelService(t, config.EndpointTypeOpenAI, "http://unused.invalid")
	defer s.Close()
	rec := &completionRecorder{}
	s.SetRequestStatsRecorder(rec)
	s.httpClient.Transport = statsRoundTrip(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	s.ServeHTTP(httptest.NewRecorder(), statsRequest(t, "/v1/chat/completions"))
	assertCompletions(t, rec, requeststats.Failure)
}

func TestRequestStatsTargetFailoverOnce(t *testing.T) {
	s, err := NewService(&config.Config{Targets: []config.Target{
		{Name: "first", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://first.invalid", APIKey: "test", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
		{Name: "second", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://second.invalid", APIKey: "test", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
	}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var attempts int
	s.httpClient.Transport = statsRoundTrip(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("connection refused")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})
	rec := &completionRecorder{}
	s.SetRequestStatsRecorder(rec)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, statsRequest(t, "/v1/chat/completions"))
	assertCompletions(t, rec, requeststats.Success)
	if attempts != 2 || w.Code != 200 || s.metrics.totalTargetRetries.Load() != 1 {
		t.Fatalf("attempts=%d status=%d retries=%d", attempts, w.Code, s.metrics.totalTargetRetries.Load())
	}
}

func TestRequestStatsExistingProtocolTerminals(t *testing.T) {
	for _, tc := range []struct{ name, ep, path, body string }{
		{"anthropic", config.EndpointTypeClaude, "/v1/messages", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"gemini", config.EndpointTypeGemini, "/v1beta/models/test-model:streamGenerateContent", "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n"},
		{"image", config.EndpointTypeOpenAIImage, "/v1/images/generations", "data: {\"type\":\"image_generation.completed\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			s := newTestModelService(t, tc.ep, up.URL)
			defer s.Close()
			rec := &completionRecorder{}
			s.SetRequestStatsRecorder(rec)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, statsRequest(t, tc.path))
			assertCompletions(t, rec, requeststats.Success)
			if w.Code != 200 || w.Body.String() != tc.body {
				t.Fatalf("wire=%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRequestStatsRetryOnceAndRecordFailureIsNonBilling(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(402)
			io.WriteString(w, `{"error":{"message":"insufficient balance"}}`)
			return
		}
		io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer up.Close()
	s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL, func(target *config.Target) { target.APIKeys = []string{"second-key"} })
	defer s.Close()
	rec := &completionRecorder{err: errors.New("stats disk unavailable")}
	s.SetRequestStatsRecorder(rec)
	billing := &adapterEventRecorder{}
	s.SetUsageRecorder(billing)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, statsRequest(t, "/v1/chat/completions"))
	assertCompletions(t, rec, requeststats.Success)
	if w.Code != 200 || calls.Load() != 2 || s.RequestStatsRecordFailures() != 1 || len(billing.events) != 1 || billing.events[0].InputTokens != 2 || s.metrics.totalUsageRecordFailures.Load() != 0 {
		t.Fatalf("status=%d calls=%d stats failures=%d billing=%v", w.Code, calls.Load(), s.RequestStatsRecordFailures(), billing.events)
	}
}

func TestRequestStatsWriteFailureDoesNotChangeQuotaRejection(t *testing.T) {
	s := newTestModelService(t, config.EndpointTypeOpenAI, "http://unused.invalid")
	defer s.Close()
	manager := buildQuotaExceededManager(t, "tester")
	s.SetQuotaManager(manager)
	s.httpClient.Transport = statsRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("quota rejection reached upstream")
		return nil, errors.New("unexpected upstream request")
	})
	before, exceeded := manager.Check("tester")
	if !exceeded {
		t.Fatal("fixture did not exceed quota")
	}
	baseline := httptest.NewRecorder()
	s.ServeHTTP(baseline, statsRequest(t, "/v1/chat/completions"))
	if baseline.Code < 400 {
		t.Fatalf("fixture did not reject request: %d", baseline.Code)
	}
	rec := &completionRecorder{err: errors.New("stats unavailable")}
	s.SetRequestStatsRecorder(rec)
	billing := &adapterEventRecorder{}
	s.SetUsageRecorder(billing)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, statsRequest(t, "/v1/chat/completions"))
	assertCompletions(t, rec, requeststats.Failure)
	after, exceeded := manager.Check("tester")
	if w.Code != baseline.Code || w.Body.String() != baseline.Body.String() || !exceeded || before != after || len(billing.events) != 0 || s.RequestStatsRecordFailures() != 1 || s.metrics.totalTargetRetries.Load() != 0 || s.metrics.totalKeyRetries.Load() != 0 {
		t.Fatalf("baseline=%d %s response=%d %s quota before=%+v after=%+v billing=%v stats_failures=%d", baseline.Code, baseline.Body.String(), w.Code, w.Body.String(), before, after, billing.events, s.RequestStatsRecordFailures())
	}
}

func TestRequestStatsNativeWebsocketTurnsAndFallbackMissingTerminal(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "http fallback"}[fallback], func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fallback {
					if isResponsesWebsocketUpgrade(r) {
						w.WriteHeader(426)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for _, typ := range []string{"response.completed", "response.failed"} {
					var create map[string]any
					if conn.ReadJSON(&create) != nil {
						return
					}
					conn.WriteJSON(map[string]any{"type": typ, "response": map[string]any{"id": "resp_1", "status": strings.TrimPrefix(typ, "response.")}})
				}
			}))
			defer up.Close()
			s, server, _ := websocketTestProxy(t, up.URL)
			rec := &completionRecorder{}
			s.SetRequestStatsRecorder(rec)
			conn := dialWebsocketTest(t, server)
			// Invalid session messages are not completion samples.
			conn.WriteJSON(map[string]any{"type": "invalid"})
			readWebsocketEvent(t, conn)
			for i := 0; i < 2; i++ {
				conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
				event := readWebsocketEvent(t, conn)
				if fallback {
					if event["type"] != "response.created" {
						t.Fatalf("event=%v", event)
					}
					event = readWebsocketEvent(t, conn)
					if event["type"] != "error" {
						t.Fatalf("missing disconnect error: %v", event)
					}
				}
			}
			deadline := time.Now().Add(time.Second)
			for len(rec.outcomes()) < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if fallback {
				assertCompletions(t, rec, requeststats.Failure, requeststats.Failure)
			} else {
				assertCompletions(t, rec, requeststats.Success, requeststats.Failure)
			}
		})
	}
}
