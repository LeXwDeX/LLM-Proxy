package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// Observe the retry decision without replacing the HTTP transport or body.
type rejectionLog struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	waiting chan struct{}
	once    sync.Once
}

// A real TCP connection observer: signal the read after the flushed marker
// (default "{}") was consumed, even if TCP split it across reads. It never
// changes bytes or errors and does not replace the standard HTTP transport.
type rejectionReadConn struct {
	net.Conn
	read    bool
	prefix  []byte
	marker  []byte
	waiting chan struct{}
	once    sync.Once
}

func (c *rejectionReadConn) Read(p []byte) (int, error) {
	if c.read {
		c.once.Do(func() { close(c.waiting) })
	}
	n, err := c.Conn.Read(p)
	if !c.read && n > 0 {
		c.prefix = append(c.prefix, p[:n]...)
		marker := c.marker
		if marker == nil {
			marker = []byte("{}")
		}
		c.read = bytes.Contains(c.prefix, marker)
		if c.read {
			c.prefix = nil
		}
	}
	return n, err
}

func (l *rejectionLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiting != nil && bytes.Contains(p, []byte("sleep and retry same key")) {
		l.once.Do(func() { close(l.waiting) })
	}
	return l.buf.Write(p)
}

func (l *rejectionLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func rejectionService(t *testing.T, endpoint, adapter string, logs *rejectionLog) *Service {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}}
	for _, name := range []string{"primary", "alternative"} {
		cfg.Targets = append(cfg.Targets, config.Target{Name: name, EndpointType: "openai", Endpoint: endpoint + "/" + name, UsageAdapter: adapter, APIKey: "k0", APIKeys: []string{"k1"}, MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}})
	}
	s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	transport := &http.Transport{}
	s.httpClient.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	return s
}

func assertRejectionResources(t *testing.T, s *Service) {
	t.Helper()
	for _, state := range s.targetOrder {
		if state.inFlightCount() != 0 {
			t.Error("node slot leaked")
		}
		state.keyPool.mu.Lock()
		for i, n := range state.keyPool.inFlight {
			if n != 0 {
				t.Errorf("key %d inFlight=%d", i, n)
			}
		}
		state.keyPool.mu.Unlock()
	}
}

func awaitRejection(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP synchronization timed out")
	}
}

func TestNon2xxParentCancellation(t *testing.T) {
	for _, mode := range []string{"read429", "read402", "wait429", "waitGLM", "deadline402"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			flushed, disconnected := make(chan struct{}), make(chan struct{})
			logs := &rejectionLog{waiting: make(chan struct{})}
			reading := strings.HasPrefix(mode, "read") || mode == "deadline402"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if calls.Add(1) != 1 {
					w.WriteHeader(500)
					return
				}
				if mode == "waitGLM" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"1302\",\"message\":\"rate limit\",\"type\":\"rate_limit_error\"}}\n\n")
				} else {
					status := 429
					if strings.HasSuffix(mode, "402") {
						status = 402
					}
					if !reading {
						w.Header().Set("Content-Length", "2")
					}
					w.WriteHeader(status)
					io.WriteString(w, "{}")
				}
				w.(http.Flusher).Flush()
				close(flushed)
				if reading || mode == "waitGLM" {
					<-r.Context().Done()
				}
				close(disconnected)
			}))
			defer server.Close()
			adapter := "standard"
			if mode == "waitGLM" {
				adapter = "glm"
			}
			s := rejectionService(t, server.URL, adapter, logs)
			bodyRead := make(chan struct{})
			if reading {
				s.httpClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return &rejectionReadConn{Conn: conn, waiting: bodyRead}, nil
				}
			}
			stats := &completionRecorder{}
			s.SetRequestStatsRecorder(stats)
			req := statsRequest(t, "/v1/chat/completions")
			ctx, cancel := context.WithCancel(req.Context())
			if mode == "deadline402" {
				cancel()
				ctx, cancel = context.WithTimeout(req.Context(), 250*time.Millisecond)
			}
			defer cancel()
			rr, done := httptest.NewRecorder(), make(chan struct{})
			go func() { s.ServeHTTP(rr, req.WithContext(ctx)); close(done) }()
			awaitRejection(t, flushed)
			if reading {
				awaitRejection(t, bodyRead)
			}
			if !reading {
				awaitRejection(t, logs.waiting)
				// The decision is observed first. Cancel inside the one-second
				// backoff, not at an arbitrary delay from starting the request.
				timer := time.NewTimer(100 * time.Millisecond)
				<-timer.C
			}
			if mode != "deadline402" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(600 * time.Millisecond):
				t.Error("parent termination did not interrupt retry wait")
				awaitRejection(t, done)
			}
			awaitRejection(t, disconnected)
			want := StatusClientClosedRequest
			if mode == "deadline402" {
				want = 504
				assertCompletions(t, stats, requeststats.Failure)
			} else {
				assertCompletions(t, stats, requeststats.Canceled)
			}
			if rr.Code != want || calls.Load() != 1 {
				t.Errorf("status=%d calls=%d; want %d/1", rr.Code, calls.Load(), want)
			}
			if s.metrics.totalKeyRetries.Load() != 0 || s.metrics.totalTargetRetries.Load() != 0 {
				t.Error("counted retry after cancellation")
			}
			if mode != "deadline402" && s.metrics.totalClientCanceled.Load() != 1 {
				t.Error("canceled count != 1")
			}
			if reading {
				category := "context_canceled"
				if mode == "deadline402" {
					category = "deadline_exceeded"
				}
				if !strings.Contains(logs.String(), "partial_error_body") || !strings.Contains(logs.String(), category) {
					t.Error("missing interrupted body diagnostic")
				}
			}
			if !reading && s.targetOrder[0].keyPool.exhaustReasonAt(0) == "" && s.targetOrder[1].keyPool.exhaustReasonAt(0) == "" {
				t.Error("cancellation unmarked exhausted key")
			}
			assertRejectionResources(t, s)
			for _, state := range s.targetOrder {
				if state.IsMuted(time.Now()) {
					t.Error("cancellation muted target")
				}
			}
		})
	}
}

func TestNon2xxReadPolicyRealHTTP(t *testing.T) {
	const sentinel = "sentinel-secret-do-not-log"
	for _, adapter := range []string{"standard", "qwen", "glm", "deepseek", "kimi", "openrouter", "minimax"} {
		for _, mode := range []string{"timeout", "truncated", "complete", "bounded"} {
			for _, status := range []int{429, 402, 400, 503} {
				if adapter != "standard" && status != 429 && status != 402 {
					continue
				}
				if mode == "bounded" && (adapter != "standard" || status != 400) {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%d", adapter, mode, status), func(t *testing.T) {
					var calls, healthy atomic.Int32
					var firstPath string
					var pathMu sync.Mutex
					body := `{"message":"` + sentinel + `"}`
					if mode == "bounded" {
						body = strings.Repeat("x", 8192)
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						io.Copy(io.Discard, r.Body)
						calls.Add(1)
						pathMu.Lock()
						if firstPath == "" {
							firstPath = r.URL.Path
						}
						if r.URL.Path != firstPath {
							t.Error("unexpected target failover")
						}
						pathMu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						if r.Header.Get("Authorization") == "Bearer k1" {
							healthy.Add(1)
							io.WriteString(w, `{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
							return
						}
						if mode == "truncated" {
							w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
						}
						w.WriteHeader(status)
						io.WriteString(w, body)
						if mode == "timeout" {
							w.(http.Flusher).Flush()
							<-r.Context().Done()
						}
					}))
					defer server.Close()
					logs := &rejectionLog{}
					s := rejectionService(t, server.URL, adapter, logs)
					stats := &completionRecorder{}
					s.SetRequestStatsRecorder(stats)
					s.setRequestTimeout(100 * time.Millisecond)
					req := statsRequest(t, "/v1/chat/completions")
					rr := httptest.NewRecorder()
					s.ServeHTTP(rr, req)
					wantCalls, wantHealthy, wantStatus := int32(1), int32(0), status
					if status == 429 {
						wantCalls, wantHealthy, wantStatus = 3, 1, 200
					}
					if status == 402 {
						wantCalls, wantHealthy, wantStatus = 2, 1, 200
					}
					if calls.Load() != wantCalls || healthy.Load() != wantHealthy || rr.Code != wantStatus {
						t.Fatalf("calls=%d healthy=%d status=%d; want %d/%d/%d", calls.Load(), healthy.Load(), rr.Code, wantCalls, wantHealthy, wantStatus)
					}
					if req.Context().Err() != nil {
						t.Fatal("parent context ended")
					}
					if wantStatus == 200 {
						assertCompletions(t, stats, requeststats.Success)
					} else {
						assertCompletions(t, stats, requeststats.Failure)
					}
					if s.metrics.totalKeyRetries.Load() != int64(wantCalls-1) || s.metrics.totalTargetRetries.Load() != 0 {
						t.Error("unexpected retry counts")
					}
					for _, state := range s.targetOrder {
						if !strings.Contains(firstPath, "/"+state.Target().Name+"/") {
							continue
						}
						wantReason := ""
						if status == 429 {
							wantReason = "rate_limited"
						}
						if status == 402 {
							wantReason = "billing_error"
						}
						if got := state.keyPool.exhaustReasonAt(0); got != wantReason {
							t.Errorf("bad key reason=%q want %q", got, wantReason)
						}
						if got := state.keyPool.exhaustReasonAt(1); got != "" {
							t.Errorf("healthy key reason=%q", got)
						}
					}
					if wantCalls == 1 && rr.Body.String() != body {
						t.Errorf("body replay changed: %q", rr.Body.String())
					}
					partial := 0
					for _, line := range strings.Split(logs.String(), "\n") {
						if !strings.Contains(line, "partial_error_body") {
							continue
						}
						partial++
						category := "unexpected_eof"
						if mode == "timeout" {
							category = "deadline_exceeded"
						}
						if !strings.Contains(line, category) || strings.Contains(line, sentinel) || strings.Contains(line, "Bearer") || strings.Contains(line, server.URL) {
							t.Errorf("unsafe or missing diagnostic category: %s", line)
						}
						var fields map[string]any
						if err := json.Unmarshal([]byte(line), &fields); err != nil {
							t.Fatal(err)
						}
						if fields["status"] != float64(status) || fields["bytes_read"] != float64(len(body)) || fields["request_id"] == nil || fields["target"] == nil {
							t.Errorf("missing diagnostic fields: %s", line)
						}
					}
					wantPartial := int(wantCalls - wantHealthy)
					if mode == "complete" || mode == "bounded" {
						wantPartial = 0
					}
					if partial != wantPartial {
						t.Errorf("partial_error_body count=%d want %d", partial, wantPartial)
					}
					assertRejectionResources(t, s)
				})
			}
		}
	}
}

func TestNon2xxNoHeadersStillCannotRetry(t *testing.T) {
	for _, mode := range []string{"child_timeout", "parent_deadline", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			received, disconnected := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 {
					close(received)
					<-r.Context().Done()
					close(disconnected)
				}
			}))
			defer server.Close()
			logs := &rejectionLog{}
			s := rejectionService(t, server.URL, "standard", logs)
			stats := &completionRecorder{}
			s.SetRequestStatsRecorder(stats)
			req := statsRequest(t, "/v1/chat/completions")
			ctx, cancel := context.WithCancel(req.Context())
			if mode == "parent_deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(req.Context(), 250*time.Millisecond)
			} else if mode == "child_timeout" {
				s.setRequestTimeout(250 * time.Millisecond)
			}
			defer cancel()
			rr, done := httptest.NewRecorder(), make(chan struct{})
			go func() { s.ServeHTTP(rr, req.WithContext(ctx)); close(done) }()
			awaitRejection(t, received)
			if mode == "parent_cancel" {
				cancel()
			}
			awaitRejection(t, done)
			awaitRejection(t, disconnected)
			wantStatus, outcome := 504, requeststats.Failure
			if mode == "parent_cancel" {
				wantStatus, outcome = StatusClientClosedRequest, requeststats.Canceled
			}
			if rr.Code != wantStatus || calls.Load() != 1 || s.MetricsSnapshot().TotalRetries != 0 {
				t.Errorf("status=%d calls=%d retries=%d", rr.Code, calls.Load(), s.MetricsSnapshot().TotalRetries)
			}
			if mode == "child_timeout" && ctx.Err() != nil {
				t.Error("parent context ended")
			}
			assertCompletions(t, stats, outcome)
			assertRejectionResources(t, s)
			for _, state := range s.targetOrder {
				if state.IsMuted(time.Now()) {
					t.Error("context termination muted target")
				}
			}
			if strings.Contains(logs.String(), "partial_error_body") {
				t.Error("no response body exists to diagnose")
			}
		})
	}
}

func TestCanceledResponseHandoffRecordsUsage(t *testing.T) {
	for _, mode := range []string{"http", "ws_buffered", "ws_sent_error", "ws_local"} {
		t.Run(mode, func(t *testing.T) {
			const body = `{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			s := rejectionService(t, upstream.URL, "standard", &rejectionLog{})
			events, stats := &adapterEventRecorder{}, &completionRecorder{}
			s.SetUsageRecorder(events)
			s.SetRequestStatsRecorder(stats)
			req := statsRequest(t, "/v1/chat/completions")
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			transport := s.httpClient.Transport
			s.httpClient.Transport = coreReviewTransport(func(r *http.Request) (*http.Response, error) {
				var resp *http.Response
				if mode == "http" {
					var err error
					resp, err = transport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
				} else {
					// Unit boundary fixture for an already-buffered WS response.
					// The HTTP case above is the real-network evidence.
					calls.Add(1)
					wsBody := &responsesWSBody{ReadCloser: io.NopCloser(strings.NewReader(body)), generationSent: mode != "ws_local", local: mode == "ws_local"}
					status := 200
					if mode != "ws_buffered" {
						status = 429
						wsBody.ReadCloser = io.NopCloser(strings.NewReader(`{"error":{"message":"limited"}}`))
						wsBody.setOutcome(true, status, nil)
						wsBody.noMute.Store(true)
					}
					resp = &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: wsBody, Request: r}
				}
				cancel() // deterministic handoff, before either service guard
				return resp, nil
			})
			s.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
			wantEvents := 1
			if mode == "ws_local" {
				wantEvents = 0 // proxy-local response never sent a generation
			}
			if len(events.events) != wantEvents {
				t.Fatalf("usage events=%d; canceled response must still settle", len(events.events))
			}
			if wantEvents == 1 {
				event := events.events[0]
				if mode == "ws_buffered" || (mode == "http" && !event.CostIncomplete) {
					if event.InputTokens != 7 || event.OutputTokens != 3 {
						t.Errorf("known usage lost: %+v", event)
					}
				}
				if mode == "ws_sent_error" && !event.CostIncomplete {
					t.Errorf("ambiguous sent generation not incomplete: %+v", event)
				}
			}
			assertCompletions(t, stats, requeststats.Canceled)
			if calls.Load() != 1 || s.MetricsSnapshot().TotalRetries != 0 {
				t.Error("canceled handoff retried")
			}
			assertRejectionResources(t, s)
			for _, state := range s.targetOrder {
				if state.IsMuted(time.Now()) {
					t.Error("canceled handoff muted target")
				}
			}
		})
	}
}

func TestMinimaxCanceledPeekHandoffRealHTTP(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "child_timeout"} {
		for _, tc := range []struct {
			name, body string
			knownUsage bool
		}{
			{"usage", `{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`, true},
			{"partial", `{"choices":[{"message":{"content":"unfinished`, false},
			{"rejection", `{"base_resp":{"status_code":1008}}`, false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				reading, disconnected := make(chan struct{}), make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					if calls.Add(1) != 1 {
						w.WriteHeader(500)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, tc.body)
					w.(http.Flusher).Flush() // no EOF, even for complete JSON
					<-r.Context().Done()
					close(disconnected)
				}))
				defer upstream.Close()
				s := rejectionService(t, upstream.URL, "minimax", &rejectionLog{})
				s.httpClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return &rejectionReadConn{Conn: conn, marker: []byte(tc.body), waiting: reading}, nil
				}
				events, stats := &adapterEventRecorder{}, &completionRecorder{}
				s.SetUsageRecorder(events)
				s.SetRequestStatsRecorder(stats)
				req := statsRequest(t, "/v1/chat/completions")
				ctx, cancel := context.WithCancel(req.Context())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(req.Context(), 2*time.Second)
				} else if mode == "child_timeout" {
					s.setRequestTimeout(2 * time.Second)
				}
				defer cancel()
				rr, done := httptest.NewRecorder(), make(chan struct{})
				go func() { s.ServeHTTP(rr, req.WithContext(ctx)); close(done) }()
				awaitRejection(t, reading) // standard Transport consumed all prefix bytes
				if ctx.Err() != nil {
					t.Fatal("parent ended before the observed body read")
				}
				if mode == "cancel" {
					cancel()
				}
				awaitRejection(t, done)
				awaitRejection(t, disconnected)
				if len(events.events) != 1 {
					t.Fatalf("usage events=%d; HTTP 200 peek must still settle", len(events.events))
				}
				event := events.events[0]
				if tc.knownUsage {
					if event.InputTokens != 7 || event.OutputTokens != 3 {
						t.Errorf("known usage lost: %+v", event)
					}
				} else if !event.CostIncomplete {
					t.Errorf("missing incomplete usage: %+v", event)
				}
				outcome := requeststats.Failure
				if mode == "cancel" {
					outcome = requeststats.Canceled
				}
				assertCompletions(t, stats, outcome)
				if rr.Code != 200 || rr.Body.String() != tc.body {
					t.Errorf("response prefix lost: status=%d body=%q", rr.Code, rr.Body.String())
				}
				if calls.Load() != 1 || s.MetricsSnapshot().TotalRetries != 0 {
					t.Error("interrupted MiniMax peek retried")
				}
				if mode == "child_timeout" && ctx.Err() != nil {
					t.Error("child timeout ended parent")
				}
				assertRejectionResources(t, s)
				for _, state := range s.targetOrder {
					if state.IsMuted(time.Now()) {
						t.Error("interrupted peek muted target")
					}
					for _, entry := range state.keyPool.entries {
						if entry.exhausted {
							t.Error("interrupted peek disabled key")
						}
					}
				}
				t.Logf("usage events=1 tokens=%d/%d incomplete=%v calls=%d", event.InputTokens, event.OutputTokens, event.CostIncomplete, calls.Load())
			})
		}
	}
}

func TestCanceledGLMPeekRecordsIncompleteUsage(t *testing.T) {
	var calls atomic.Int32
	disconnected, reading := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {}") // no event boundary: peek must wait
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(disconnected)
	}))
	defer upstream.Close()
	logs := &rejectionLog{}
	s := rejectionService(t, upstream.URL, "glm", logs)
	s.httpClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &rejectionReadConn{Conn: conn, waiting: reading}, nil
	}
	events, stats := &adapterEventRecorder{}, &completionRecorder{}
	s.SetUsageRecorder(events)
	s.SetRequestStatsRecorder(stats)
	req := statsRequest(t, "/v1/chat/completions")
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { s.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx)); close(done) }()
	awaitRejection(t, reading)
	cancel()
	awaitRejection(t, done)
	awaitRejection(t, disconnected)
	if len(events.events) != 1 || !events.events[0].CostIncomplete {
		t.Errorf("incomplete generation event lost: %+v", events.events)
	}
	assertCompletions(t, stats, requeststats.Canceled)
	if calls.Load() != 1 || s.MetricsSnapshot().TotalRetries != 0 {
		t.Error("canceled GLM generation retried")
	}
	if strings.Contains(logs.String(), "partial_error_body") {
		t.Error("successful response mislabeled as non-2xx error body")
	}
	assertRejectionResources(t, s)
	for _, state := range s.targetOrder {
		if state.IsMuted(time.Now()) {
			t.Error("canceled GLM generation muted target")
		}
	}
}
