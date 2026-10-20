package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// A sent attempt that fails with a retryable network error before response
// headers only logs (will_retry=true) and counts; one client request maps to
// at most one usage event. The final outcome owns the event.

func closeAfterRequest(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func TestRetryableSentFailureThenSuccessRecordsOneEvent(t *testing.T) {
	var primary, alternative atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/primary") {
			primary.Add(1)
			closeAfterRequest(w, r)
			return
		}
		alternative.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+goneDeepSeekFinal+"\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
	if w.Code != 200 || primary.Load() != 1 || alternative.Load() != 1 {
		t.Fatalf("status=%d primary=%d alternative=%d", w.Code, primary.Load(), alternative.Load())
	}
	got := events.snapshot()
	if len(got) != 1 || got[0].Target != "alternative" || got[0].InputTokens != 8 || got[0].OutputTokens != 7 || got[0].UsageStatus != usageStatusComplete {
		t.Fatalf("want exactly the official event of the final attempt: %+v", got)
	}
	if n := s.MetricsSnapshot().TotalUsageUpstreamInterruptedNoUsage; n != 1 {
		t.Errorf("retried sent attempt counter=%d want 1", n)
	}
	out := logs.String()
	if !strings.Contains(out, `"will_retry":true`) || !strings.Contains(out, `"cause":"upstream_error"`) {
		t.Errorf("retried attempt warning missing will_retry/cause: %s", out)
	}
}

func TestRetryableSentFailuresAllFailRecordOneZeroEvent(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		closeAfterRequest(w, r)
	}))
	defer upstream.Close()
	s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
	if w.Code < 500 || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d; both targets tried once", w.Code, calls.Load())
	}
	got := events.snapshot()
	if len(got) != 1 {
		t.Fatalf("usage events=%d want exactly 1: %+v", len(got), got)
	}
	ev := got[0]
	if ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.UsageStatus != usageStatusIncomplete || !ev.CostIncomplete || !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
		t.Fatalf("final zero event form: %+v", ev)
	}
	if n := s.MetricsSnapshot().TotalUsageUpstreamInterruptedNoUsage; n != 2 {
		t.Errorf("counter=%d want 2 (one per sent attempt)", n)
	}
	if strings.Count(logs.String(), `"will_retry":true`) != 1 {
		t.Errorf("want one will_retry warning: %s", logs.String())
	}
}

// The pending zero-usage event belongs to the sent attempt: when a later
// target rewrites the model path and fails before sending, the recorded path
// and protocol stay those of the attempt that may have reached the upstream.
func TestPendingNoUsageKeepsSentAttemptPath(t *testing.T) {
	sent := httptest.NewServer(http.HandlerFunc(closeAfterRequest))
	defer sent.Close()
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := refused.URL
	refused.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}}
	cfg.Targets = []config.Target{
		{Name: "primary", EndpointType: config.EndpointTypeGemini, Endpoint: sent.URL, APIKey: "k0", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "gem-a", Fallback: "gem"}}},
		{Name: "alternative", EndpointType: config.EndpointTypeGemini, Endpoint: refusedURL, APIKey: "k1", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "gem-b", Fallback: "gem"}}},
	}
	logs := &rejectionLog{}
	s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transport := &http.Transport{}
	s.httpClient.Transport = transport
	defer transport.CloseIdleConnections()
	events := &goneEventRecorder{}
	s.SetUsageRecorder(events)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, goneRequest(t, "/v1beta/models/gem:streamGenerateContent", `{"contents":[]}`))
	if w.Code < 500 {
		t.Fatalf("status=%d; both targets fail", w.Code)
	}
	got := events.snapshot()
	if len(got) != 1 {
		t.Fatalf("usage events=%d want 1: %+v\n%s", len(got), got, logs.String())
	}
	ev := got[0]
	if ev.Target != "primary" || ev.Path != "/v1beta/models/gem-a:streamGenerateContent" || ev.UsageProtocol != string(config.ProtocolGemini) {
		t.Fatalf("event must carry the sent attempt's target/path/protocol: target=%q path=%q protocol=%q", ev.Target, ev.Path, ev.UsageProtocol)
	}
	if !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
		t.Errorf("diagnostic=%q", ev.UsageDiagnostic)
	}
}

// A sent attempt without official usage followed by a final non-2xx
// response: the non-2xx response is passed through and never billed or
// observed, so the pending sent attempt still records its one zero-usage
// event (the request may have run upstream).
func TestRetryableSentFailureThenNon2xxRecordsZeroEvent(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var primary, alternative atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/primary") {
					primary.Add(1)
					closeAfterRequest(w, r)
					return
				}
				alternative.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"rejected","type":"invalid_request_error"},"usage":{"prompt_tokens":9,"completion_tokens":9}}`)
			}))
			defer upstream.Close()
			s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterStandard)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
			if w.Code != status || primary.Load() != 1 || alternative.Load() != 1 {
				t.Fatalf("status=%d primary=%d alternative=%d", w.Code, primary.Load(), alternative.Load())
			}
			got := events.snapshot()
			if len(got) != 1 {
				t.Fatalf("usage events=%d want exactly 1: %+v\n%s", len(got), got, logs.String())
			}
			ev := got[0]
			if ev.Target != "primary" || ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.UsageStatus != usageStatusIncomplete || !ev.CostIncomplete || !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
				t.Fatalf("want the sent attempt's zero event, never the non-2xx usage: %+v", ev)
			}
			if n := s.MetricsSnapshot().TotalUsageUpstreamInterruptedNoUsage; n != 1 {
				t.Errorf("counter=%d want 1 (announced once, recorded quietly)", n)
			}
			out := logs.String()
			if strings.Count(out, `"will_retry":true`) != 1 || !strings.Contains(out, `"cause":"upstream_error"`) {
				t.Errorf("retried attempt warning missing will_retry/cause: %s", out)
			}
		})
	}
}

// Without a sent attempt a final non-2xx response records no usage event:
// directly, or after a dial failure that never reached an upstream.
func TestNon2xxWithoutSentAttemptRecordsNoEvent(t *testing.T) {
	rejecting := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"rejected"}}`)
	}
	t.Run("direct", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(rejecting))
		defer upstream.Close()
		s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterStandard)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", w.Code)
		}
		if got := events.snapshot(); len(got) != 0 {
			t.Fatalf("usage events=%d want 0: %+v", len(got), got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
	t.Run("after dial failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(rejecting))
		defer upstream.Close()
		refused := httptest.NewServer(http.NotFoundHandler())
		refusedURL := refused.URL
		refused.Close()
		cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}}
		cfg.Targets = []config.Target{
			{Name: "primary", EndpointType: config.EndpointTypeOpenAI, Endpoint: refusedURL, APIKey: "k0", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
			{Name: "alternative", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "k1", MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}},
		}
		logs := &rejectionLog{}
		s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		transport := &http.Transport{}
		s.httpClient.Transport = transport
		defer transport.CloseIdleConnections()
		events := &goneEventRecorder{}
		s.SetUsageRecorder(events)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := events.snapshot(); len(got) != 0 {
			t.Fatalf("usage events=%d want 0: %+v", len(got), got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
}

// The pending zero-usage event names the model as normal events do: the
// resolved upstream model, never the target's ModelPrefix wire spelling.
func TestPendingNoUsageModelMatchesNormalEvents(t *testing.T) {
	var interrupt atomic.Bool
	var mu sync.Mutex
	var wireModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		wireModels = append(wireModels, extractModelFromJSON(body))
		mu.Unlock()
		if interrupt.Load() {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"x","usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}}
	cfg.Targets = []config.Target{{Name: "primary", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, UsageAdapter: config.UsageAdapterStandard,
		APIKey: "k0", ModelPrefix: "pfx/", ModelMappings: []config.ModelMapping{{Upstream: "Up-Model", Fallback: "test-model"}}}}
	logs := &rejectionLog{}
	s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transport := &http.Transport{}
	s.httpClient.Transport = transport
	defer transport.CloseIdleConnections()
	events := &goneEventRecorder{}
	s.SetUsageRecorder(events)

	s.ServeHTTP(httptest.NewRecorder(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
	interrupt.Store(true)
	s.ServeHTTP(httptest.NewRecorder(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
	got := events.snapshot()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || len(wireModels) != 2 || wireModels[1] != "pfx/Up-Model" {
		t.Fatalf("events=%+v wire=%v", got, wireModels)
	}
	normal, pending := got[0], got[1]
	if !hasGoneDiagnostic(pending.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) || normal.UsageStatus != usageStatusComplete {
		t.Fatalf("want one normal and one pending zero event: %+v", got)
	}
	if pending.Model != normal.Model || strings.Contains(pending.Model, "pfx/") {
		t.Fatalf("pending model %q, normal model %q: want the same upstream model without the prefix", pending.Model, normal.Model)
	}
	if out := strings.ToLower(logs.String()); !strings.Contains(out, `"model":"`+normal.Model+`"`) || strings.Contains(out, `"model":"pfx/`) {
		t.Errorf("pending warning model differs: %s", logs.String())
	}
}
