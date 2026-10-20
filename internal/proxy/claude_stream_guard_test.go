package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
)

const claudeStreamStartA = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_A\",\"model\":\"claude-sonnet\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"
const claudeStreamThinking = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hello\"}}\n\n"
const claudeStreamStartB = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_B\"}}\n\n"
const claudeStreamStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
const wantClaudeOverlapError = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Upstream stream error (internal server error): upstream sent message_start for a new message before message_stop of the previous one; the stream was terminated by the proxy.\"}}\n\n"

func TestClaudeStreamGuardOverlap(t *testing.T) {
	var out bytes.Buffer
	g := &claudeStreamGuard{writer: &out}
	prefix := claudeStreamStartA + claudeStreamThinking
	_, err := g.Write([]byte(prefix + claudeStreamStartB + claudeStreamStop))
	if !errors.Is(err, errClaudeStreamOverlap) {
		t.Fatalf("Write error=%v, want overlap", err)
	}
	if out.String() != prefix+wantClaudeOverlapError || g.openID != "msg_A" || g.newID != "msg_B" {
		t.Fatalf("unexpected overlap state/output: %q %q %s", g.openID, g.newID, out.String())
	}
	if _, err := g.Write([]byte(claudeStreamStartB)); !errors.Is(err, errClaudeStreamOverlap) {
		t.Fatalf("terminal Write error=%v", err)
	}
	if err := g.flush(); !errors.Is(err, errClaudeStreamOverlap) || out.String() != prefix+wantClaudeOverlapError {
		t.Fatalf("terminal flush=%v output=%s", err, out.String())
	}
}

func TestClaudeStreamGuardPassthrough(t *testing.T) {
	for name, input := range map[string]string{
		"same-id":                 claudeStreamStartA + claudeStreamStartA + claudeStreamStop,
		"normal":                  claudeStreamStartA + claudeStreamThinking + claudeStreamStop,
		"sequential":              claudeStreamStartA + claudeStreamStop + claudeStreamStartB + claudeStreamStop,
		"truncated":               claudeStreamStartA + "event: ping\ndata: {\"type\":\"ping\"}",
		"opaque":                  "\xef\xbb\xbfevent: extension\ndata: {\"vendor\":true}\n\n: keepalive\n\n" + claudeStreamStartA + claudeStreamStop,
		"data-type-authoritative": claudeStreamStartA + "event: message_stop\ndata: {\"type\":\"ping\"}\n\nevent: message_start\ndata: invalid message_start\n\n" + claudeStreamStop,
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			g := &claudeStreamGuard{writer: &out}
			if n, err := g.Write([]byte(input)); err != nil || n != len(input) {
				t.Fatalf("Write=%d %v", n, err)
			}
			if err := g.flush(); err != nil || out.String() != input {
				t.Fatalf("flush=%v stream changed: %q", err, out.String())
			}
		})
	}
}

func TestClaudeStreamGuardChunking(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r", "mixed"} {
		convert := func(s string) string {
			if newline == "mixed" {
				// One LF line ending followed by a CRLF/LF event boundary.
				return strings.ReplaceAll(s, "\n\n", "\r\n\n")
			}
			return strings.ReplaceAll(s, "\n", newline)
		}
		for _, overlap := range []bool{false, true} {
			prefix := "\xef\xbb\xbf: comment\n\n" + claudeStreamStartA + claudeStreamThinking
			input, want := prefix+claudeStreamStop+claudeStreamStartB+claudeStreamStop, ""
			if overlap {
				input = prefix + claudeStreamStartB + claudeStreamStop
				want = convert(prefix) + wantClaudeOverlapError
			} else {
				want = convert(input)
			}
			input = convert(input)
			for _, chunking := range []string{"whole", "byte", "random"} {
				t.Run(fmt.Sprintf("%q/overlap=%v/%s", newline, overlap, chunking), func(t *testing.T) {
					var out bytes.Buffer
					g := &claudeStreamGuard{writer: &out}
					rng := rand.New(rand.NewSource(42))
					var err error
					for rest := []byte(input); len(rest) > 0 && err == nil; {
						size := len(rest)
						if chunking == "byte" {
							size = 1
						} else if chunking == "random" {
							size = min(len(rest), rng.Intn(37)+1)
						}
						_, err = g.Write(rest[:size])
						rest = rest[size:]
					}
					if err == nil {
						err = g.flush()
					}
					if (overlap && !errors.Is(err, errClaudeStreamOverlap)) || (!overlap && err != nil) || out.String() != want {
						t.Fatalf("error=%v stream changed:\n%q\nwant:\n%q", err, out.String(), want)
					}
				})
			}
		}
	}
}

func TestClaudeStreamGuardEventAlignment(t *testing.T) {
	var out bytes.Buffer
	g := &claudeStreamGuard{writer: &out}
	partial := strings.TrimSuffix(claudeStreamStartA, "\n")
	if _, err := g.Write([]byte(partial)); err != nil || out.Len() != 0 {
		t.Fatalf("incomplete event escaped: %v %s", err, out.String())
	}
	if _, err := g.Write([]byte("\n")); err != nil || out.String() != claudeStreamStartA {
		t.Fatalf("complete event missing: %v %s", err, out.String())
	}
}

func TestClaudeStreamGuardLargeEvent(t *testing.T) {
	var out bytes.Buffer
	g := &claudeStreamGuard{writer: &out}
	input := "data: " + strings.Repeat("x", (1<<20)-len("data: "))
	if _, err := g.Write([]byte(input)); err != nil || out.Len() != 0 {
		t.Fatalf("event at the cap must remain buffered: %v bytes=%d", err, out.Len())
	}
	input += "x"
	if _, err := g.Write([]byte("x")); err != nil || out.String() != input {
		t.Fatalf("oversized event not passed through: err=%v bytes=%d", err, out.Len())
	}
	// Once bypassed, lifecycle parsing must stay disabled.
	tail := "\n\n" + claudeStreamStartA + claudeStreamStartB
	if _, err := g.Write([]byte(tail)); err != nil {
		t.Fatal(err)
	}
	if err := g.flush(); err != nil || out.String() != input+tail {
		t.Fatalf("bypass changed stream: %v bytes=%d", err, out.Len())
	}
}

type claudeFailWriter struct{}

func (claudeFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestClaudeStreamGuardWriteFailures(t *testing.T) {
	for _, input := range []string{claudeStreamStartA, "data: truncated"} {
		g := &claudeStreamGuard{writer: claudeFailWriter{}}
		_, err := g.Write([]byte(input))
		if err == nil {
			err = g.flush()
		}
		if !errors.Is(err, io.ErrClosedPipe) || !g.downstreamFailure {
			t.Fatalf("downstream error lost: %v", err)
		}
	}
	var out bytes.Buffer
	g := &claudeStreamGuard{writer: &out}
	if _, err := g.Write([]byte(claudeStreamStartA)); err != nil {
		t.Fatal(err)
	}
	g.writer = claudeFailWriter{}
	if _, err := g.Write([]byte(claudeStreamStartB)); !errors.Is(err, io.ErrClosedPipe) || !g.downstreamFailure || errors.Is(err, errClaudeStreamOverlap) {
		t.Fatalf("synthetic event write failure misclassified: %v", err)
	}
}

func TestClaudeStreamGuardEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, path, ct, encoding string
		status                   int
		websocket, want          bool
	}{
		{"enabled", "/v1/messages", "text/event-stream", "", 200, false, true},
		{"identity-case", "/V1/MESSAGES", "TEXT/EVENT-STREAM; charset=utf-8", "IDENTITY", 201, false, true},
		{"openai", "/v1/chat/completions", "text/event-stream", "", 200, false, false},
		{"openai-case", "/V1/CHAT/COMPLETIONS", "TEXT/EVENT-STREAM", "", 200, false, false},
		{"not-messages", "/V1/MESSAGES_OTHER", "TEXT/EVENT-STREAM", "", 200, false, false},
		{"error", "/V1/MESSAGES", "TEXT/EVENT-STREAM", "", 500, false, false},
		{"json", "/V1/MESSAGES", "application/json", "", 200, false, false},
		{"gzip", "/V1/MESSAGES", "TEXT/EVENT-STREAM", "gzip", 200, false, false},
		{"ws", "/V1/MESSAGES", "TEXT/EVENT-STREAM", "", 200, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.path, nil)
			if tc.websocket {
				r = r.WithContext(context.WithValue(r.Context(), responsesWebsocketContextKey{}, &responsesWebsocketSession{}))
			}
			resp := &http.Response{StatusCode: tc.status, Header: make(http.Header)}
			resp.Header.Set("Content-Type", tc.ct)
			resp.Header.Set("Content-Encoding", tc.encoding)
			if got := claudeStreamGuardEnabled(r, resp); got != tc.want {
				t.Fatalf("enabled=%v want=%v", got, tc.want)
			}
			if !tc.want {
				if allocs := testing.AllocsPerRun(100, func() { claudeStreamGuardEnabled(r, resp) }); allocs != 0 {
					t.Fatalf("disabled guard allocates: %g", allocs)
				}
			}
		})
	}
}

func TestClaudeStreamGuardDataType(t *testing.T) {
	var out bytes.Buffer
	g := &claudeStreamGuard{writer: &out}
	a := "\xef\xbb\xbfdata: {\"type\":\"message_start\",\n" + "data: \"message\":{\"id\":\"msg_A\"}}\n\n"
	// An event label or a string inside a delta must not close the lifecycle.
	other := "event: message_stop\ndata: {\"type\":\"content_block_delta\",\"text\":\"message_stop\"}\n\n"
	b := "event: ping\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_B\"}}\n\n"
	if _, err := g.Write([]byte(a + other + b)); !errors.Is(err, errClaudeStreamOverlap) || out.String() != a+other+wantClaudeOverlapError {
		t.Fatalf("data JSON type not authoritative: %v %s", err, out.String())
	}
}

func TestClaudeStreamOverlapService(t *testing.T) {
	wire := claudeStreamStartA + claudeStreamThinking + claudeStreamStartB + claudeStreamStop
	for _, tc := range []struct {
		name, path, endpointType, encoding string
		guard                              bool
		input                              string
	}{
		{"messages", "/v1/messages", "claude", "", true, wire},
		{"gzip-passthrough", "/v1/messages", "claude", "gzip", false, wire},
		{"openai-passthrough", "/v1/chat/completions", "openai", "", false, wire},
		{"messages-truncated", "/v1/messages", "claude", "", false, claudeStreamStartA + "event: ping\ndata: {\"type\":\"ping\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamWire := []byte(tc.input)
			if tc.encoding == "gzip" {
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				if _, err := gz.Write(upstreamWire); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				upstreamWire = compressed.Bytes()
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.Write(upstreamWire)
			}))
			defer upstream.Close()
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "fake-claude", Endpoint: upstream.URL, EndpointType: tc.endpointType, APIKey: "fake-key", ModelMappings: []config.ModelMapping{{Upstream: "claude-sonnet"}}}}}
			cfg.TraceStore.Enabled = true
			cfg.TraceStore.DiskPath = filepath.Join(t.TempDir(), "trace.log")
			var logs bytes.Buffer
			svc, err := NewService(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			recorder := &adapterEventRecorder{}
			svc.SetUsageRecorder(recorder)
			store := auth.NewStore()
			if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
				t.Fatal(err)
			}
			principal, _ := store.Authenticate("token")
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"claude-sonnet","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			// Explicit encoding prevents net/http from silently decompressing the fixture.
			req.Header.Set("Accept-Encoding", "identity")
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			rr := httptest.NewRecorder()
			appmiddleware.RequestID()(svc).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status=%d upstream calls=%d", rr.Code, calls.Load())
			}
			want := string(upstreamWire)
			if tc.guard {
				want = claudeStreamStartA + claudeStreamThinking + wantClaudeOverlapError
			}
			if rr.Body.String() != want {
				t.Fatalf("unexpected downstream stream:\n%s", rr.Body.String())
			}
			traces := svc.ListTrace(0, 10)
			if len(traces) != 1 || traces[0].ResponseBody != tc.input {
				t.Fatalf("trace did not preserve upstream stream: %+v", traces)
			}
			if tc.guard {
				// The guard ends the stream before official final usage: zero
				// tokens, the overlap diagnostic plus the no-official-usage marker.
				if ev := recorder.events; len(ev) != 1 || ev[0].UsageStatus != "incomplete" || !ev[0].CostIncomplete || ev[0].UsageDiagnostic != "claude_stream_overlapping_message;upstream_interrupted_no_usage" || ev[0].InputTokens != 0 || ev[0].OutputTokens != 0 {
					t.Fatalf("overlap usage=%+v", recorder.events)
				}
				if strings.Count(logs.String(), `"msg":"claude stream overlapping message_start"`) != 1 || strings.Contains(logs.String(), "stream copy failed") || !strings.Contains(logs.String(), `"open_message_id":"msg_A"`) || !strings.Contains(logs.String(), `"new_message_id":"msg_B"`) {
					t.Fatalf("unexpected overlap logs: %s", logs.String())
				}
				if rr.Header().Get("Content-Length") != "" {
					t.Fatal("modified stream kept upstream Content-Length")
				}
			}
		})
	}
}
