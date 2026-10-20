package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestOfficialStreamTransport(t *testing.T) {
	// SSE transport grammar: https://html.spec.whatwg.org/multipage/server-sent-events.html
	payload := `{"model":"o3","usage":{"prompt_tokens":12,"completion_tokens":3},"choices":[]}`
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("newline=%q/gzip=%v", newline, compressed), func(t *testing.T) {
				wire := strings.ReplaceAll(": heartbeat\n\nevent: future_event\ndata: {\"extra\":\"ignored\"}\n\nid: 12\nretry: 1000\ndata: "+payload+"\n\ndata: [DONE]\n\n", "\n", newline)
				data := []byte(wire)
				encoding := ""
				if compressed {
					var b bytes.Buffer
					w := gzip.NewWriter(&b)
					_, _ = w.Write(data)
					_ = w.Close()
					data = b.Bytes()
					encoding = "gzip"
				}
				u := newUsageStream("text/event-stream", encoding, usageParser{protocol: config.ProtocolOpenAIChat})
				for _, b := range data {
					_, _ = u.Write([]byte{b})
				}
				u.Close()
				if u.err != nil || u.tokens.status != usageStatusComplete || u.tokens.InputTokens != 12 || u.tokens.OutputTokens != 3 {
					t.Fatalf("status=%+v err=%v", u.tokens, u.err)
				}
			})
		}
	}
	// The WHATWG grammar dispatches an event only at a blank line. The proxy
	// deliberately relaxes this for a clean EOF: complete last data ([DONE]
	// or fully parsed JSON) counts as dispatched, so a final [DONE] without
	// the trailing blank line is a terminal rather than a failure.
	for _, wire := range []string{
		"data: " + payload + "\n\ndata: [DONE]",
		"data: " + payload + "\n\ndata: [DONE]\n",
	} {
		u := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIChat})
		_, _ = u.Write([]byte(wire))
		u.Close()
		if u.err != nil || !u.terminal || u.tokens.status != usageStatusComplete {
			t.Fatalf("clean EOF after complete [DONE] rejected: err=%v terminal=%v tokens=%+v", u.err, u.terminal, u.tokens)
		}
	}
	// Malformed events and incomplete last data remain failures.
	for _, wire := range []string{
		"data: " + payload + "\n\ndata: {bad}\n\ndata: [DONE]\n\n",
		"data: " + payload + "\n\ndata: {\"choices\":[",
		"data: " + payload + "\n\ndata: [DON",
	} {
		u := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIChat})
		_, _ = u.Write([]byte(wire))
		u.Close()
		if u.err == nil {
			t.Fatal("malformed/truncated transport not detected")
		}
	}
}

func TestOfficialUsageDefensiveCounts(t *testing.T) {
	for _, invalid := range []string{"-1", "1.5", "null", "true", `"12"`, "9223372036854775808"} {
		t.Run(invalid, func(t *testing.T) {
			wire := fmt.Sprintf(`{"usage":{"prompt_tokens":%s,"completion_tokens":2}}`, invalid)
			got, _, _ := (usageParser{protocol: config.ProtocolOpenAIChat}).extractTokens("application/json", []byte(wire))
			if got.status != usageStatusConflict || !got.costIncomplete || got.InputTokens != 0 {
				t.Fatalf("invalid count accepted: %+v", got)
			}
		})
	}
	for _, body := range []string{
		`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":11}}}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":3}}}`,
	} {
		got, _, _ := (usageParser{protocol: config.ProtocolOpenAIChat}).extractTokens("application/json", []byte(body))
		if got.status != usageStatusConflict || !got.costIncomplete {
			t.Fatalf("invalid partition accepted: %+v", got)
		}
	}
}

func TestOfficialSSELeadingBOM(t *testing.T) {
	wire := "\xef\xbb\xbfdata: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	got, _, found := (usageParser{protocol: config.ProtocolOpenAIChat}).extractSSE([]byte(wire))
	if !found || got.status != usageStatusComplete || got.InputTokens != 12 {
		t.Fatalf("BOM lost usage: %+v found=%v", got, found)
	}
}
