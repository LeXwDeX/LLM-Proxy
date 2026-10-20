package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	coderws "github.com/coder/websocket"
	gorillaws "github.com/gorilla/websocket"
)

// Client-gone usage settlement: a client that disconnects still cancels the
// upstream at once (no drain, no retry, no replay). When the request may have
// reached the upstream and the protocol's official final usage was not yet
// observed, exactly one zero-token incomplete event is recorded with
// client_disconnected_no_usage; final usage observed before the disconnect is
// recorded as is. A cancellation provably before any request byte records
// nothing.

type goneEventRecorder struct {
	mu     sync.Mutex
	events []usage.Event
}

func (r *goneEventRecorder) Record(e usage.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *goneEventRecorder) snapshot() []usage.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]usage.Event(nil), r.events...)
}

// goneClientWriter is a thread-safe downstream whose body can be polled.
type goneClientWriter struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
}

func newGoneClientWriter() *goneClientWriter { return &goneClientWriter{header: make(http.Header)} }

func (w *goneClientWriter) Header() http.Header { return w.header }
func (w *goneClientWriter) WriteHeader(int)     {}
func (w *goneClientWriter) Flush()              {}
func (w *goneClientWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}
func (w *goneClientWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func waitGoneBody(t *testing.T, w *goneClientWriter, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(w.String(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("downstream never received %q: %q", marker, w.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func awaitGone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func goneService(t *testing.T, endpoint, endpointType, adapter string) (*Service, *goneEventRecorder, *completionRecorder, *rejectionLog) {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}}
	for _, name := range []string{"primary", "alternative"} {
		cfg.Targets = append(cfg.Targets, config.Target{Name: name, EndpointType: endpointType, Endpoint: endpoint + "/" + name, UsageAdapter: adapter, APIKey: "k0", APIKeys: []string{"k1"}, MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}})
	}
	logs := &rejectionLog{}
	s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	transport := &http.Transport{}
	s.httpClient.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	events, stats := &goneEventRecorder{}, &completionRecorder{}
	s.SetUsageRecorder(events)
	s.SetRequestStatsRecorder(stats)
	return s, events, stats, logs
}

func goneRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(auth.WithPrincipal(r.Context(), newAllowAllPrincipal(t, "gone-client")))
}

func serveGone(s *Service, w http.ResponseWriter, r *http.Request) (context.CancelFunc, chan struct{}) {
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	go func() { s.ServeHTTP(w, r.WithContext(ctx)); close(done) }()
	return cancel, done
}

func hasGoneDiagnostic(diagnostic, want string) bool {
	for _, part := range strings.Split(diagnostic, ";") {
		if part == want {
			return true
		}
	}
	return false
}

// assertZeroGoneEvent checks the unified zero-usage form and its log/counter.
func assertZeroGoneEvent(t *testing.T, s *Service, events []usage.Event, logs *rejectionLog, phase, transport string) usage.Event {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("usage events=%d want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	tokens := []int64{ev.InputTokens, ev.OutputTokens, ev.CachedTokens, ev.CacheCreationTokens, ev.ExplicitCacheReadTokens, ev.ReasoningTokens, ev.CacheCreation5mTokens, ev.CacheCreation1hTokens, ev.ToolUsePromptTokens, ev.ImageInputTokens, ev.CachedImageInputTokens, ev.ImageOutputTokens}
	for i, n := range tokens {
		if n != 0 {
			t.Errorf("token field %d=%d; partial values must be zeroed: %+v", i, n, ev)
		}
	}
	if ev.UsageStatus != usageStatusIncomplete || !ev.CostIncomplete || !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedNoUsage) {
		t.Errorf("zero event form: status=%q cost_incomplete=%v diagnostic=%q", ev.UsageStatus, ev.CostIncomplete, ev.UsageDiagnostic)
	}
	if got := s.MetricsSnapshot().TotalUsageClientGoneNoUsage; got != 1 {
		t.Errorf("usage_client_gone_no_usage=%d want 1", got)
	}
	out := logs.String()
	if !strings.Contains(out, "连接断开，未获取到计费字段，按 0 token 记录（不重放） / client disconnected before upstream usage was received; recorded zero usage (no replay)") ||
		!strings.Contains(out, `"phase":"`+phase+`"`) || !strings.Contains(out, `"transport":"`+transport+`"`) ||
		!strings.Contains(out, `"protocol":`) || !strings.Contains(out, `"model":`) || !strings.Contains(out, `"target":`) {
		t.Errorf("zero-usage warning missing or incomplete (phase=%s transport=%s): %s", phase, transport, out)
	}
	for _, secret := range []string{"k0", "k1", "Bearer"} {
		if strings.Contains(out, `"`+secret+`"`) {
			t.Errorf("log leaked %q", secret)
		}
	}
	return ev
}

func assertNoReplay(t *testing.T, s *Service, calls int32) {
	t.Helper()
	if calls != 1 || s.MetricsSnapshot().TotalRetries != 0 {
		t.Errorf("calls=%d retries=%d; a disconnect must not retry or replay", calls, s.MetricsSnapshot().TotalRetries)
	}
}

const goneDeepSeekFinal = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_cache_hit_tokens":3,"prompt_cache_miss_tokens":8}}`

// Official final usage observed before the disconnect keeps its exact values;
// the upstream is still canceled at once.
func TestClientGoneAfterFinalUsageRecordsOfficial(t *testing.T) {
	for _, tc := range []struct {
		name, adapter string
		chunks        []string
		in, cached    int64
	}{
		{"deepseek_last_chunk", config.UsageAdapterDeepSeek, []string{goneDeepSeekFinal}, 8, 3},
		{"openai_usage_chunk", config.UsageAdapterStandard, []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`}, 11, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstreamCanceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
				for _, chunk := range tc.chunks {
					io.WriteString(w, "data: "+chunk+"\n\n")
				}
				io.WriteString(w, ": final-sent\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done() // [DONE] never arrives
				close(upstreamCanceled)
			}))
			defer upstream.Close()
			s, events, stats, _ := goneService(t, upstream.URL, config.EndpointTypeOpenAI, tc.adapter)
			w := newGoneClientWriter()
			cancel, done := serveGone(s, w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
			defer cancel()
			waitGoneBody(t, w, "final-sent")
			cancel()
			awaitGone(t, upstreamCanceled, "upstream cancellation")
			awaitGone(t, done, "request end")
			got := events.snapshot()
			if len(got) != 1 {
				t.Fatalf("usage events=%d want 1", len(got))
			}
			ev := got[0]
			if ev.InputTokens != tc.in || ev.CachedTokens != tc.cached || ev.OutputTokens != 7 || ev.UsageStatus != usageStatusComplete || ev.CostIncomplete || hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedNoUsage) {
				t.Errorf("official usage lost: %+v", ev)
			}
			if !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedAfterFinalUsage) || hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) {
				t.Errorf("diagnostic=%q want %s", ev.UsageDiagnostic, diagnosticClientDisconnectedAfterFinalUsage)
			}
			if s.MetricsSnapshot().TotalUsageClientGoneNoUsage != 0 {
				t.Error("official usage counted as no-usage")
			}
			assertCompletions(t, stats, requeststats.Canceled)
			assertNoReplay(t, s, calls.Load())
			assertRejectionResources(t, s)
		})
	}
}

// Without official final usage the event is zeroed, partial values included.
func TestClientGoneBeforeFinalUsageRecordsZero(t *testing.T) {
	for _, tc := range []struct {
		name, endpointType, adapter, path, body, first string
	}{
		{"deepseek_chat", config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"anthropic_partial_input", config.EndpointTypeClaude, config.UsageAdapterStandard, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":8,"messages":[]}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"},
		{"responses_created", config.EndpointTypeOpenAI, config.UsageAdapterStandard, "/v1/responses", `{"model":"test-model","stream":true,"input":"hi"}`,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\"}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstreamCanceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.first+": first-sent\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamCanceled)
			}))
			defer upstream.Close()
			s, events, stats, logs := goneService(t, upstream.URL, tc.endpointType, tc.adapter)
			w := newGoneClientWriter()
			cancel, done := serveGone(s, w, goneRequest(t, tc.path, tc.body))
			defer cancel()
			waitGoneBody(t, w, "first-sent")
			canceledAt := time.Now()
			cancel()
			awaitGone(t, upstreamCanceled, "upstream cancellation")
			if elapsed := time.Since(canceledAt); elapsed > 2*time.Second {
				t.Errorf("upstream canceled after %v; must be immediate", elapsed)
			}
			awaitGone(t, done, "request end")
			assertZeroGoneEvent(t, s, events.snapshot(), logs, "streaming", "sse")
			assertCompletions(t, stats, requeststats.Canceled)
			assertNoReplay(t, s, calls.Load())
			assertRejectionResources(t, s)
			for _, state := range s.targetOrder {
				if state.IsMuted(time.Now()) {
					t.Error("client disconnect muted a target")
				}
			}
		})
	}
}

// Non-streaming JSON: a complete body observed before the disconnect is
// official; a partial body is zeroed.
func TestClientGoneNonStreamingJSON(t *testing.T) {
	full := `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_cache_hit_tokens":3,"prompt_cache_miss_tokens":8}}`
	for _, complete := range []bool{true, false} {
		name := map[bool]string{true: "complete_body", false: "partial_body"}[complete]
		t.Run(name, func(t *testing.T) {
			body := full
			if !complete {
				body = full[:40]
			}
			upstreamCanceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
				w.(http.Flusher).Flush()
				<-r.Context().Done() // the chunked body never ends
				close(upstreamCanceled)
			}))
			defer upstream.Close()
			s, events, stats, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
			w := newGoneClientWriter()
			cancel, done := serveGone(s, w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","messages":[]}`))
			defer cancel()
			waitGoneBody(t, w, body)
			time.Sleep(20 * time.Millisecond)
			cancel()
			awaitGone(t, upstreamCanceled, "upstream cancellation")
			awaitGone(t, done, "request end")
			if complete {
				got := events.snapshot()
				if len(got) != 1 || got[0].InputTokens != 8 || got[0].OutputTokens != 7 || got[0].UsageStatus != usageStatusComplete {
					t.Fatalf("complete JSON usage lost: %+v", got)
				}
			} else {
				assertZeroGoneEvent(t, s, events.snapshot(), logs, "streaming", "http")
			}
			assertCompletions(t, stats, requeststats.Canceled)
			assertRejectionResources(t, s)
		})
	}
}

// Sent but no response headers yet: the upstream is canceled at once, nothing
// is retried or failed over, and one zero event records the attempt.
func TestClientGoneBeforeHeadersRecordsZero(t *testing.T) {
	var primary, alternative atomic.Int32
	received, upstreamCanceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.HasPrefix(r.URL.Path, "/alternative") {
			alternative.Add(1)
			return
		}
		primary.Add(1)
		close(received)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	s, events, stats, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
	cancel, done := serveGone(s, newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
	defer cancel()
	awaitGone(t, received, "upstream request")
	cancel()
	awaitGone(t, upstreamCanceled, "upstream cancellation")
	awaitGone(t, done, "request end")
	ev := assertZeroGoneEvent(t, s, events.snapshot(), logs, "before_headers", "http")
	if ev.StatusCode != StatusClientClosedRequest {
		t.Errorf("status=%d want 499", ev.StatusCode)
	}
	if primary.Load()+alternative.Load() != 1 || s.MetricsSnapshot().TotalRetries != 0 {
		t.Errorf("primary=%d alternative=%d retries=%d; no retry or failover", primary.Load(), alternative.Load(), s.MetricsSnapshot().TotalRetries)
	}
	assertCompletions(t, stats, requeststats.Canceled)
	assertRejectionResources(t, s)
}

// A cancellation while still dialing provably sent nothing: no event.
func TestClientGoneBeforeSendRecordsNothing(t *testing.T) {
	s, events, stats, _ := goneService(t, "http://127.0.0.1:9", config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
	dialing := make(chan struct{})
	var once sync.Once
	s.httpClient.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		once.Do(func() { close(dialing) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cancel, done := serveGone(s, newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
	defer cancel()
	awaitGone(t, dialing, "dial")
	cancel()
	awaitGone(t, done, "request end")
	if got := events.snapshot(); len(got) != 0 {
		t.Errorf("nothing was sent; usage events=%+v", got)
	}
	if s.MetricsSnapshot().TotalUsageClientGoneNoUsage != 0 || s.MetricsSnapshot().TotalRetries != 0 {
		t.Error("pre-send cancellation counted or retried")
	}
	assertCompletions(t, stats, requeststats.Canceled)
}

func goneWSPoolService(t *testing.T, endpoint string) (*Service, *goneEventRecorder, *completionRecorder, *rejectionLog) {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}, Targets: []config.Target{{
		Name: "openai", EndpointType: "openai", Endpoint: endpoint, APIKey: "upstream-test-key", PreferWebSocket: true, UsageAdapter: config.UsageAdapterStandard,
		ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
	}}}
	logs := &rejectionLog{}
	s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	events, stats := &goneEventRecorder{}, &completionRecorder{}
	s.SetUsageRecorder(events)
	s.SetRequestStatsRecorder(stats)
	return s, events, stats, logs
}

// Upstream WebSocket pool (HTTP /v1/responses over an upstream WS lane).
func TestClientGoneUpstreamWSPool(t *testing.T) {
	for _, stage := range []string{"handshake", "wait_first_event", "streaming"} {
		t.Run(stage, func(t *testing.T) {
			var creates, posts atomic.Int32
			reached, upstreamGone := make(chan struct{}), make(chan struct{})
			var reachedOnce, goneOnce sync.Once
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					http.Error(w, "HTTP was used", 500)
					return
				}
				if stage == "handshake" {
					reachedOnce.Do(func() { close(reached) })
					<-r.Context().Done()
					goneOnce.Do(func() { close(upstreamGone) })
					return
				}
				c, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				defer goneOnce.Do(func() { close(upstreamGone) })
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				creates.Add(1)
				switch stage {
				case "streaming":
					c.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`))
					c.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
				}
				reachedOnce.Do(func() { close(reached) })
				c.Read(r.Context()) // hold the lane until the proxy closes it
			}))
			defer upstream.Close()
			s, events, stats, logs := goneWSPoolService(t, upstream.URL)
			w := newGoneClientWriter()
			cancel, done := serveGone(s, w, goneRequest(t, "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`))
			defer cancel()
			awaitGone(t, reached, "upstream stage")
			if stage == "streaming" {
				waitGoneBody(t, w, "response.created")
			}
			cancel()
			awaitGone(t, done, "request end")
			awaitGone(t, upstreamGone, "upstream lane close")
			if posts.Load() != 0 || creates.Load() > 1 {
				t.Errorf("creates=%d HTTP=%d; a sent WS generation is never replayed", creates.Load(), posts.Load())
			}
			switch stage {
			case "handshake":
				if got := events.snapshot(); len(got) != 0 {
					t.Errorf("no response.create was sent; events=%+v", got)
				}
			case "wait_first_event":
				ev := assertZeroGoneEvent(t, s, events.snapshot(), logs, "before_headers", "ws")
				if !hasGoneDiagnostic(ev.UsageDiagnostic, "websocket_generation_interrupted_before_usage") {
					t.Errorf("existing WS diagnostic lost: %q", ev.UsageDiagnostic)
				}
			default:
				assertZeroGoneEvent(t, s, events.snapshot(), logs, "streaming", "ws")
			}
			assertCompletions(t, stats, requeststats.Canceled)
		})
	}
}

// Native downstream WebSocket session, per turn.
func TestClientGoneNativeWebsocketTurn(t *testing.T) {
	for _, stage := range []string{"no_create", "streaming"} {
		t.Run(stage, func(t *testing.T) {
			reached, upstreamClosed := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				defer close(upstreamClosed)
				var request map[string]any
				if conn.ReadJSON(&request) != nil {
					return
				}
				conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "r1", "status": "in_progress"}})
				conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "hi"})
				close(reached)
				conn.ReadMessage()
			}))
			defer upstream.Close()
			service, server, _ := websocketTestProxy(t, upstream.URL)
			logs := &rejectionLog{}
			service.logger = slog.New(slog.NewJSONHandler(logs, nil))
			events := &goneEventRecorder{}
			service.SetUsageRecorder(events)
			conn := dialWebsocketTest(t, server)
			if stage == "no_create" {
				conn.Close()
				time.Sleep(100 * time.Millisecond)
				if got := events.snapshot(); len(got) != 0 {
					t.Fatalf("no turn was sent; events=%+v", got)
				}
				return
			}
			conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
			awaitGone(t, reached, "upstream turn")
			if readWebsocketEvent(t, conn)["type"] != "response.created" {
				t.Fatal("turn did not start")
			}
			conn.Close()
			awaitGone(t, upstreamClosed, "upstream close")
			deadline := time.Now().Add(2 * time.Second)
			for len(events.snapshot()) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			assertZeroGoneEvent(t, service, events.snapshot(), logs, "ws_turn", "ws")
		})
	}
}
