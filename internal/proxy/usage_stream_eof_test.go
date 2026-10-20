package proxy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// A clean connection EOF may omit the blank line after the last SSE event.
// When that last event's data is complete ([DONE] or a fully parsed JSON
// payload) it is treated as dispatched: [DONE] is a terminal and a JSON
// payload takes part in usage and lifecycle observation as usual. Incomplete
// data (truncated or unparsable JSON) and decoder/read errors keep failing.
func TestSSECleanEOFAcceptsCompleteLastEvent(t *testing.T) {
	chat := `{"model":"o3","usage":{"prompt_tokens":12,"completion_tokens":3},"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	claudeStart := `{"type":"message_start","message":{"id":"m","model":"claude-x","usage":{"input_tokens":12,"output_tokens":1}}}`
	claudeDelta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`
	responses := `{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":12,"output_tokens":3}}}`
	gemini := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3,"totalTokenCount":15}}`
	type tc struct {
		name, wire string
		protocol   config.ProtocolType
	}
	var cases []tc
	for _, nl := range []string{"\n", "\r\n", "\r"} {
		for _, tail := range []string{"", nl} {
			label := strings.NewReplacer("\r", "CR", "\n", "LF").Replace(nl) + "/tail=" + strings.NewReplacer("\r", "CR", "\n", "LF").Replace(tail)
			cases = append(cases,
				tc{"chat-done/" + label, "data: " + chat + nl + nl + "data: [DONE]" + tail, config.ProtocolOpenAIChat},
				tc{"messages-stop/" + label, "event: message_start" + nl + "data: " + claudeStart + nl + nl + "event: message_delta" + nl + "data: " + claudeDelta + nl + nl + "event: message_stop" + nl + `data: {"type":"message_stop"}` + tail, config.ProtocolAnthropicMessages},
				tc{"responses-completed/" + label, "event: response.created" + nl + `data: {"type":"response.created","response":{"id":"r","status":"in_progress"}}` + nl + nl + "event: response.completed" + nl + "data: " + responses + tail, config.ProtocolOpenAIResponses},
				tc{"gemini-finish/" + label, "data: " + gemini + tail, config.ProtocolGemini},
			)
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUsageStream("text/event-stream", "", usageParser{protocol: c.protocol})
			// One byte per write: transport fragments must not create events.
			for i := 0; i < len(c.wire); i++ {
				_, _ = u.Write([]byte{c.wire[i]})
			}
			u.Close()
			if u.err != nil || !u.terminal || u.responseStatus == "failed" {
				t.Fatalf("complete last event rejected: err=%v terminal=%v status=%q", u.err, u.terminal, u.responseStatus)
			}
			if !u.found || u.tokens.status != usageStatusComplete || u.tokens.costIncomplete || u.tokens.diagnostic != "" || u.tokens.InputTokens != 12 || u.tokens.OutputTokens != 3 {
				t.Fatalf("usage not complete: %+v found=%v", u.tokens, u.found)
			}
		})
	}
}

func TestSSECleanEOFRejectsIncompleteLastEvent(t *testing.T) {
	chat := `{"model":"o3","usage":{"prompt_tokens":12,"completion_tokens":3},"choices":[]}`
	for _, c := range []struct {
		name, wire string
		protocol   config.ProtocolType
		unexpected bool // last event had data that was not complete
		diagnostic string
	}{
		{"chat-truncated-json", "data: " + chat + "\n\ndata: {\"choices\":[", config.ProtocolOpenAIChat, true, "missing_stream_terminal"},
		{"chat-truncated-done", "data: " + chat + "\n\ndata: [DON", config.ProtocolOpenAIChat, true, "missing_stream_terminal"},
		{"chat-unparsable", "data: " + chat + "\n\ndata: {bad}", config.ProtocolOpenAIChat, true, "missing_stream_terminal"},
		{"chat-blank-data", "data: " + chat + "\n\ndata: \n", config.ProtocolOpenAIChat, true, "missing_stream_terminal"},
		{"messages-truncated-stop", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_st", config.ProtocolAnthropicMessages, true, "missing_stream_terminal"},
		{"responses-truncated-completed", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":12,", config.ProtocolOpenAIResponses, true, "missing_usage"},
		// A complete JSON chunk is dispatched, but Chat still needs [DONE].
		{"chat-complete-json-no-done", "data: " + chat, config.ProtocolOpenAIChat, false, "missing_stream_terminal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUsageStream("text/event-stream", "", usageParser{protocol: c.protocol})
			_, _ = u.Write([]byte(c.wire))
			u.Close()
			if u.terminal || u.tokens.status != usageStatusIncomplete || !u.tokens.costIncomplete || u.tokens.diagnostic != c.diagnostic {
				t.Fatalf("incomplete stream accepted: terminal=%v tokens=%+v", u.terminal, u.tokens)
			}
			if c.unexpected && !errors.Is(u.err, io.ErrUnexpectedEOF) {
				t.Fatalf("err=%v, want io.ErrUnexpectedEOF", u.err)
			}
			if !c.unexpected && u.err != nil {
				t.Fatalf("complete JSON chunk reported as transport error: %v", u.err)
			}
		})
	}
}

// Decoder errors are not a clean EOF: the last event stays unaccepted even
// when the decoded bytes end in a complete [DONE].
func TestSSEDecoderErrorAtLastEventStillFails(t *testing.T) {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	_, _ = io.WriteString(z, `data: {"usage":{"prompt_tokens":12,"completion_tokens":3}}`+"\n\ndata: [DONE]")
	_ = z.Close()
	truncated := b.Bytes()[:b.Len()-8] // drop the CRC32/ISIZE trailer
	u := newUsageStream("text/event-stream", "gzip", usageParser{protocol: config.ProtocolOpenAIChat})
	_, _ = u.Write(truncated)
	u.Close()
	if u.err == nil || u.terminal || u.tokens.status != usageStatusIncomplete || u.tokens.diagnostic != "missing_stream_terminal" {
		t.Fatalf("decoder error accepted: err=%v terminal=%v tokens=%+v", u.err, u.terminal, u.tokens)
	}
}

// The forwarded request outcome follows the same rule.
func TestSSECleanEOFRequestOutcome(t *testing.T) {
	payload := `{"usage":{"prompt_tokens":2,"completion_tokens":1},"choices":[]}`
	for _, c := range []struct {
		name, wire string
		want       requeststats.Outcome
	}{
		{"done-without-blank-line", "data: " + payload + "\n\ndata: [DONE]", requeststats.Success},
		{"done-with-one-newline", "data: " + payload + "\n\ndata: [DONE]\n", requeststats.Success},
		{"truncated-last-json", "data: " + payload + "\n\ndata: {\"choices\":[", requeststats.Failure},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, c.wire)
			}))
			defer up.Close()
			s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL)
			defer s.Close()
			stats := &completionRecorder{}
			s.SetRequestStatsRecorder(stats)
			billing := &adapterEventRecorder{}
			s.SetUsageRecorder(billing)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, statsRequest(t, "/v1/chat/completions"))
			if w.Code != 200 || w.Body.String() != c.wire {
				t.Fatalf("wire changed: %d %q", w.Code, w.Body.String())
			}
			assertCompletions(t, stats, c.want)
			// A truncated last event fails observation before official final
			// usage (no finish reason or [DONE]): zero usage with the marker.
			wantIn, wantOut := int64(2), int64(1)
			if c.want == requeststats.Failure {
				wantIn, wantOut = 0, 0
				if len(billing.events) == 1 && !strings.Contains(billing.events[0].UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
					t.Fatalf("truncated stream not settled: %+v", billing.events[0])
				}
			}
			if len(billing.events) != 1 || billing.events[0].InputTokens != wantIn || billing.events[0].OutputTokens != wantOut {
				t.Fatalf("usage lost: %+v", billing.events)
			}
			if complete := billing.events[0].UsageStatus == usageStatusComplete; complete != (c.want == requeststats.Success) {
				t.Fatalf("usage status=%q diagnostic=%q", billing.events[0].UsageStatus, billing.events[0].UsageDiagnostic)
			}
		})
	}
}
