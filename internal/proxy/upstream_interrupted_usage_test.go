package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	coderws "github.com/coder/websocket"
	gorillaws "github.com/gorilla/websocket"
)

// Upstream-interrupted usage settlement: a request that may have reached the
// upstream and ended (for any reason other than the client leaving) without
// the protocol's official final usage records zero tokens, cost incomplete,
// upstream_interrupted_no_usage plus the existing specific diagnostic, one
// Warn log with a cause, and the usage_upstream_interrupted_no_usage counter.
// Nothing is retried, replayed or failed over.

func assertInterruptedEvent(t *testing.T, s *Service, events []usage.Event, logs *rejectionLog, cause, phase, transport string, keep ...string) usage.Event {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("usage events=%d want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	tokens := []int64{ev.InputTokens, ev.OutputTokens, ev.CachedTokens, ev.CacheCreationTokens, ev.ExplicitCacheReadTokens, ev.ReasoningTokens, ev.CacheCreation5mTokens, ev.CacheCreation1hTokens, ev.ToolUsePromptTokens, ev.ImageInputTokens, ev.CachedImageInputTokens, ev.ImageOutputTokens}
	for i, n := range tokens {
		if n != 0 {
			t.Errorf("token field %d=%d; only official final usage counts: %+v", i, n, ev)
		}
	}
	if ev.UsageStatus != usageStatusIncomplete || !ev.CostIncomplete || !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) || hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedNoUsage) {
		t.Errorf("interrupted event form: status=%q cost_incomplete=%v diagnostic=%q", ev.UsageStatus, ev.CostIncomplete, ev.UsageDiagnostic)
	}
	for _, d := range keep {
		if !hasGoneDiagnostic(ev.UsageDiagnostic, d) {
			t.Errorf("specific diagnostic %q lost: %q", d, ev.UsageDiagnostic)
		}
	}
	m := s.MetricsSnapshot()
	if m.TotalUsageUpstreamInterruptedNoUsage != 1 || m.TotalUsageClientGoneNoUsage != 0 {
		t.Errorf("counters upstream_interrupted=%d client_gone=%d", m.TotalUsageUpstreamInterruptedNoUsage, m.TotalUsageClientGoneNoUsage)
	}
	out := logs.String()
	if !strings.Contains(out, "上游中断，未获取到计费字段，按 0 token 记录（不重放） / upstream response ended before official usage was received; recorded zero usage (no replay)") ||
		!strings.Contains(out, `"cause":"`+cause+`"`) || !strings.Contains(out, `"phase":"`+phase+`"`) ||
		!strings.Contains(out, `"transport":"`+transport+`"`) || !strings.Contains(out, `"protocol":`) || !strings.Contains(out, `"model":`) {
		t.Errorf("interrupted warning missing fields (cause=%s phase=%s transport=%s): %s", cause, phase, transport, out)
	}
	return ev
}

func assertNoInterruptedSettlement(t *testing.T, s *Service, logs *rejectionLog) {
	t.Helper()
	m := s.MetricsSnapshot()
	if m.TotalUsageUpstreamInterruptedNoUsage != 0 || m.TotalUsageClientGoneNoUsage != 0 {
		t.Errorf("unexpected settlement counters: %+v", m)
	}
	if strings.Contains(logs.String(), "recorded zero usage") {
		t.Errorf("unexpected settlement log: %s", logs.String())
	}
}

func abortUpstream(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

// SSE causes after headers: upstream error, timeout, observation failure,
// missing terminal, missing final usage and the Claude overlap guard.
func TestUpstreamInterruptedSSECauses(t *testing.T) {
	const chatPartial = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n"
	const claudeStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"
	for _, tc := range []struct {
		name, endpointType, path, body, cause string
		encoding                              string
		timeout                               time.Duration
		write                                 func(w http.ResponseWriter, r *http.Request)
		keep                                  []string
	}{
		{name: "upstream_error_anthropic_partial", endpointType: config.EndpointTypeClaude, path: "/v1/messages", cause: "upstream_error",
			write: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, claudeStart)
				w.(http.Flusher).Flush()
				abortUpstream(w)
			}},
		{name: "timeout_chat_partial", endpointType: config.EndpointTypeOpenAI, path: "/v1/chat/completions", cause: "timeout", timeout: 300 * time.Millisecond,
			write: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, chatPartial)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}},
		{name: "observation_failed_unknown_encoding", endpointType: config.EndpointTypeOpenAI, path: "/v1/chat/completions", cause: "observation_failed", encoding: "x-unknown",
			write: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, chatPartial+"data: [DONE]\n\n")
			}},
		{name: "missing_terminal_chat_partial", endpointType: config.EndpointTypeOpenAI, path: "/v1/chat/completions", cause: "missing_terminal", keep: []string{"missing_stream_terminal"},
			write: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, chatPartial)
			}},
		{name: "missing_final_usage_anthropic", endpointType: config.EndpointTypeClaude, path: "/v1/messages", cause: "missing_final_usage", keep: []string{"missing_final_usage"},
			write: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, claudeStart+"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}},
		{name: "claude_overlap", endpointType: config.EndpointTypeClaude, path: "/v1/messages", cause: "claude_overlap", keep: []string{"claude_stream_overlapping_message"},
			write: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, claudeStart+strings.Replace(claudeStart, `"m1"`, `"m2"`, 1))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				tc.write(w, r)
			}))
			defer upstream.Close()
			s, events, _, logs := goneService(t, upstream.URL, tc.endpointType, config.UsageAdapterStandard)
			if tc.timeout > 0 {
				s.setRequestTimeout(tc.timeout)
			}
			body := `{"model":"test-model","stream":true,"messages":[]}`
			if tc.endpointType == config.EndpointTypeClaude {
				body = `{"model":"test-model","stream":true,"max_tokens":8,"messages":[]}`
			}
			s.ServeHTTP(newGoneClientWriter(), goneRequest(t, tc.path, body))
			assertInterruptedEvent(t, s, events.snapshot(), logs, tc.cause, "streaming", "sse", tc.keep...)
			assertNoReplay(t, s, calls.Load())
			assertRejectionResources(t, s)
		})
	}
}

// Sent before response headers: a timeout records one zero event and nothing
// is retried or failed over.
func TestUpstreamInterruptedBeforeHeadersTimeout(t *testing.T) {
	var primary, alternative atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.HasPrefix(r.URL.Path, "/alternative") {
			alternative.Add(1)
			return
		}
		primary.Add(1)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
	s.setRequestTimeout(300 * time.Millisecond)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504", w.Code)
	}
	ev := assertInterruptedEvent(t, s, events.snapshot(), logs, "timeout", "before_headers", "http")
	if ev.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("event status=%d want 504", ev.StatusCode)
	}
	if primary.Load()+alternative.Load() != 1 || s.MetricsSnapshot().TotalRetries != 0 {
		t.Errorf("primary=%d alternative=%d retries=%d", primary.Load(), alternative.Load(), s.MetricsSnapshot().TotalRetries)
	}
}

const (
	wsTestCreated = `{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`
	// A terminal failure that carries no usage.
	wsTestFailedNoUsage = `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`
)

// httpTerminalFailedNoUsageEvent is the HTTP SSE reference: the same terminal
// failure without usage is a complete response without usage, not an
// interruption.
func httpTerminalFailedNoUsageEvent(t *testing.T) usage.Event {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: "+wsTestCreated+"\n\nevent: response.failed\ndata: "+wsTestFailedNoUsage+"\n\n")
	}))
	defer upstream.Close()
	s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterStandard)
	s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/responses", `{"model":"test-model","input":"hi","stream":true}`))
	assertNoInterruptedSettlement(t, s, logs)
	got := events.snapshot()
	if len(got) != 1 {
		t.Fatalf("HTTP reference events=%d: %+v", len(got), got)
	}
	return got[0]
}

// assertTerminalNoUsageEvent: a terminal frame without usage keeps the
// complete-without-usage form (existing diagnostic, no settlement marker,
// log or counter), exactly like HTTP.
func assertTerminalNoUsageEvent(t *testing.T, s *Service, events []usage.Event, logs *rejectionLog, want usage.Event) {
	t.Helper()
	assertNoInterruptedSettlement(t, s, logs)
	if len(events) != 1 {
		t.Fatalf("usage events=%d want 1: %+v", len(events), events)
	}
	ev := events[0]
	if hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) || hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedNoUsage) {
		t.Errorf("terminal frame without usage is not an interruption: %q", ev.UsageDiagnostic)
	}
	if ev.UsageDiagnostic != want.UsageDiagnostic || ev.UsageStatus != want.UsageStatus || ev.CostIncomplete != want.CostIncomplete {
		t.Errorf("WS diverges from HTTP: diagnostic=%q status=%q cost_incomplete=%v; HTTP diagnostic=%q status=%q cost_incomplete=%v",
			ev.UsageDiagnostic, ev.UsageStatus, ev.CostIncomplete, want.UsageDiagnostic, want.UsageStatus, want.CostIncomplete)
	}
}

// Upstream WS pool. A terminal frame (response.failed) without usage is a
// complete response without usage, as over HTTP. Only an interruption
// without a terminal frame settles as upstream interrupted, with its actual
// cause.
func TestUpstreamInterruptedWSPool(t *testing.T) {
	reference := httpTerminalFailedNoUsageEvent(t)
	for _, stage := range []string{"terminal_failed", "terminal_failed_json", "no_terminal", "wait_first_event_timeout"} {
		t.Run(stage, func(t *testing.T) {
			var creates, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					http.Error(w, "HTTP was used", 500)
					return
				}
				c, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				creates.Add(1)
				switch stage {
				case "terminal_failed", "terminal_failed_json":
					c.Write(r.Context(), coderws.MessageText, []byte(wsTestCreated))
					c.Write(r.Context(), coderws.MessageText, []byte(wsTestFailedNoUsage))
				case "no_terminal":
					c.Write(r.Context(), coderws.MessageText, []byte(wsTestCreated))
					c.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.output_text.delta","delta":"hi"}`))
					return // the lane drops without a terminal frame
				}
				c.Read(r.Context())
			}))
			defer upstream.Close()
			s, events, _, logs := goneWSPoolService(t, upstream.URL)
			if stage == "wait_first_event_timeout" {
				s.setRequestTimeout(300 * time.Millisecond)
			}
			body := `{"model":"openai-test","input":"hi","stream":true}`
			if stage == "terminal_failed_json" {
				body = `{"model":"openai-test","input":"hi"}`
			}
			s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/responses", body))
			if posts.Load() != 0 || creates.Load() != 1 {
				t.Errorf("creates=%d HTTP=%d; never replayed", creates.Load(), posts.Load())
			}
			switch stage {
			case "terminal_failed":
				assertTerminalNoUsageEvent(t, s, events.snapshot(), logs, reference)
			case "terminal_failed_json":
				assertNoInterruptedSettlement(t, s, logs)
				got := events.snapshot()
				if len(got) != 1 || hasGoneDiagnostic(got[0].UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
					t.Errorf("terminal JSON without usage is not an interruption: %+v", got)
				}
			case "no_terminal":
				assertInterruptedEvent(t, s, events.snapshot(), logs, noUsageCauseUpstreamError, "streaming", "ws")
			default:
				assertInterruptedEvent(t, s, events.snapshot(), logs, "timeout", "before_headers", "ws", "websocket_generation_interrupted_before_usage")
			}
		})
	}
}

// Native downstream WebSocket turn: a terminal failure without usage is a
// complete turn without usage (no settlement), as over HTTP.
func TestNativeWebsocketTerminalFailedWithoutUsage(t *testing.T) {
	reference := httpTerminalFailedNoUsageEvent(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var request map[string]any
		if conn.ReadJSON(&request) != nil {
			return
		}
		conn.WriteMessage(gorillaws.TextMessage, []byte(wsTestCreated))
		conn.WriteMessage(gorillaws.TextMessage, []byte(wsTestFailedNoUsage))
		conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, _ := websocketTestProxy(t, upstream.URL)
	logs := &rejectionLog{}
	service.logger = slog.New(slog.NewJSONHandler(logs, nil))
	events := &goneEventRecorder{}
	service.SetUsageRecorder(events)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
	for readWebsocketEvent(t, conn)["type"] != "response.failed" {
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(events.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	assertTerminalNoUsageEvent(t, service, events.snapshot(), logs, reference)
}

// Official final usage observed before the upstream response is interrupted
// is recorded as a complete event with the official values; only the
// interrupted_after_final_usage marker tells the stream did not end cleanly.
// No zero-usage log or counter.
func TestUpstreamInterruptedAfterFinalUsageRecordsOfficial(t *testing.T) {
	for _, tc := range []struct {
		name, endpointType, adapter, path, body, events string
		abort                                           bool
		in, out, cached                                 int64
	}{
		{"chat_abort_after_final_chunk", config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`,
			"data: " + goneDeepSeekFinal + "\n\n", true, 8, 7, 3},
		{"chat_eof_without_done", config.EndpointTypeOpenAI, config.UsageAdapterStandard, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`,
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n", false, 11, 7, 0},
		{"responses_abort_after_completed", config.EndpointTypeOpenAI, config.UsageAdapterStandard, "/v1/responses", `{"model":"test-model","input":"hi","stream":true}`,
			"event: response.created\ndata: " + wsTestCreated + "\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":7}}}\n\n", true, 11, 7, 0},
		{"anthropic_abort_before_message_stop", config.EndpointTypeClaude, config.UsageAdapterStandard, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":8,"messages":[]}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n", true, 11, 7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.events)
				w.(http.Flusher).Flush()
				if tc.abort {
					abortUpstream(w)
				}
			}))
			defer upstream.Close()
			s, events, _, logs := goneService(t, upstream.URL, tc.endpointType, tc.adapter)
			s.ServeHTTP(newGoneClientWriter(), goneRequest(t, tc.path, tc.body))
			got := events.snapshot()
			if len(got) != 1 {
				t.Fatalf("usage events=%d want 1: %+v", len(got), got)
			}
			ev := got[0]
			if ev.InputTokens != tc.in || ev.OutputTokens != tc.out || ev.CachedTokens != tc.cached || ev.UsageStatus != usageStatusComplete || ev.CostIncomplete {
				t.Errorf("official usage must be recorded complete: %+v", ev)
			}
			if !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) ||
				hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) || hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticClientDisconnectedAfterFinalUsage) {
				t.Errorf("diagnostic=%q want %s", ev.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage)
			}
			assertNoInterruptedSettlement(t, s, logs)
		})
	}
}

// Unchanged: a complete response that carries no usage, official usage with a
// value conflict, requests provably never sent and non-2xx rejections.
func TestUpstreamInterruptedExclusions(t *testing.T) {
	t.Run("complete_without_usage", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer upstream.Close()
		s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterStandard)
		s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
		got := events.snapshot()
		if len(got) != 1 || got[0].UsageDiagnostic != "missing_usage" {
			t.Fatalf("missing_usage changed: %+v", got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
	t.Run("final_conflict", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":3,\"prompt_cache_miss_tokens\":2}}\n\n")
			w.(http.Flusher).Flush()
			abortUpstream(w)
		}))
		defer upstream.Close()
		s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
		s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
		got := events.snapshot()
		// Status quo: the interrupted copy marks it incomplete but keeps the
		// official (conflicting) values and their diagnostic.
		if len(got) != 1 || got[0].InputTokens != 8 || got[0].OutputTokens != 7 || !hasGoneDiagnostic(got[0].UsageDiagnostic, "deepseek_hit_miss_total") {
			t.Fatalf("conflicting official usage changed: %+v", got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
	t.Run("final_conflict_client_gone", func(t *testing.T) {
		upstreamCanceled := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":3,\"prompt_cache_miss_tokens\":2}}\n\n: sent\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(upstreamCanceled)
		}))
		defer upstream.Close()
		s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterDeepSeek)
		w := newGoneClientWriter()
		cancel, done := serveGone(s, w, goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
		defer cancel()
		waitGoneBody(t, w, ": sent")
		cancel()
		awaitGone(t, upstreamCanceled, "upstream cancellation")
		awaitGone(t, done, "request end")
		got := events.snapshot()
		if len(got) != 1 || got[0].InputTokens != 8 || got[0].OutputTokens != 7 || !hasGoneDiagnostic(got[0].UsageDiagnostic, "deepseek_hit_miss_total") {
			t.Fatalf("conflicting official usage changed on disconnect: %+v", got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
	t.Run("never_sent", func(t *testing.T) {
		s, events, _, logs := goneService(t, "http://127.0.0.1:1", config.EndpointTypeOpenAI, config.UsageAdapterStandard)
		s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
		if got := events.snapshot(); len(got) != 0 {
			t.Fatalf("connection refused sent nothing; events=%+v", got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
	t.Run("non_2xx", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			http.Error(w, `{"error":{"message":"bad"}}`, 400)
		}))
		defer upstream.Close()
		s, events, _, logs := goneService(t, upstream.URL, config.EndpointTypeOpenAI, config.UsageAdapterStandard)
		s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
		if got := events.snapshot(); len(got) != 0 {
			t.Fatalf("non-2xx rejection recorded usage: %+v", got)
		}
		assertNoInterruptedSettlement(t, s, logs)
	})
}
