package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

func TestF8StandardErrorEnvelopeLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
	}{
		{"message", `{"error":{"message":"rejected"}}`, "failed"},
		{"string code", `{"error":{"code":"upstream_error"}}`, "failed"},
		{"numeric code", `{"error":{"code":502}}`, "failed"},
		{"type", `{"error":{"type":"server_error","param":null}}`, "failed"},
		{"null", `{"error":null}`, ""},
		{"empty", `{"error":{}}`, ""},
		{"string", `{"error":"generated text"}`, ""},
		{"array", `{"error":[{"message":"not an envelope"}]}`, ""},
		{"invalid members", `{"error":{"message":{},"code":false,"type":[]}}`, ""},
		{"empty members", `{"error":{"message":" ","code":null,"type":""}}`, ""},
		{"metadata only", `{"error":{"metadata":{"message":"provider error","code":502}}}`, ""},
		{"param only", `{"error":{"param":"model"}}`, ""},
		{"text", `{"choices":[{"message":{"content":"error: failed"},"finish_reason":"stop"}]}`, ""},
		{"tool arguments", `{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"{\"error\":{\"message\":\"bad\"}}"}}]},"finish_reason":"tool_calls"}]}`, ""},
		{"tool result", `{"output":[{"type":"function_call_output","output":{"error":{"message":"bad"}}}]}`, ""},
		{"nested error", `{"response":{"error":{"message":"not top-level"},"status":"completed"}}`, "completed"},
		{"responses completed", `{"type":"response.completed","response":{"status":"completed","error":null}}`, "completed"},
		{"responses incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","error":null}}`, "incomplete"},
	} {
		for _, protocol := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolOpenAIImage} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.name, protocol, stream), func(t *testing.T) {
					body, contentType := tc.body, "application/json"
					if stream {
						body, contentType = "data: "+body+"\n\ndata: [DONE]\n\n", "text/event-stream"
					}
					p := usageParser{protocol: protocol}
					tokens, _, _ := p.extractTokens(contentType, []byte(body))
					if tokens.responseStatus != tc.status {
						t.Fatalf("parsed status=%q want %q", tokens.responseStatus, tc.status)
					}
					u := newUsageStream(contentType, "", p)
					_, _ = u.Write([]byte(body))
					u.Close()
					if u.responseStatus != tc.status || u.err != nil {
						t.Fatalf("observed status=%q err=%v want %q", u.responseStatus, u.err, tc.status)
					}
				})
			}
		}
	}
}

func TestF8NativeProtocolLifecycleUnchanged(t *testing.T) {
	for _, tc := range []struct {
		protocol     config.ProtocolType
		body, status string
	}{
		{config.ProtocolAnthropicMessages, `{"error":{"message":"not an Anthropic event"}}`, ""},
		{config.ProtocolAnthropicMessages, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, "failed"},
		{config.ProtocolGemini, `{"error":{"code":500,"message":"native error"}}`, ""},
		{config.ProtocolGemini, `{"promptFeedback":{"blockReason":"SAFETY"}}`, "blocked"},
		{"", `{"error":{"message":"unknown protocol"}}`, ""},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.protocol, tc.status, stream), func(t *testing.T) {
				body, typ := tc.body, "application/json"
				if stream {
					body, typ = "data: "+body+"\n\n", "text/event-stream"
				}
				tokens, _, _ := (usageParser{protocol: tc.protocol}).extractTokens(typ, []byte(body))
				if tokens.responseStatus != tc.status {
					t.Fatalf("status=%q want %q", tokens.responseStatus, tc.status)
				}
			})
		}
	}
}

func TestF8ErrorPreservesUsageAndFailure(t *testing.T) {
	for _, protocol := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses} {
		usage := `{"prompt_tokens":7,"completion_tokens":3}`
		if protocol == config.ProtocolOpenAIResponses {
			usage = `{"input_tokens":7,"output_tokens":3}`
		}
		for _, body := range []string{
			`{"error":{"message":"failed"},"usage":` + usage + `}`,
			"data: {\"error\":{\"message\":\"failed\"}}\n\ndata: {\"usage\":" + usage + "}\n\ndata: [DONE]\n\n",
			"data: {\"error\":{\"code\":502}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":" + usage + "}}\n\ndata: [DONE]\n\n",
		} {
			t.Run(fmt.Sprintf("%s/%s", protocol, body), func(t *testing.T) {
				tokens, _, found := (usageParser{protocol: protocol}).extractTokens("application/json", []byte(body))
				if !found || tokens.InputTokens != 7 || tokens.OutputTokens != 3 || tokens.responseStatus != "failed" {
					t.Fatalf("found=%v tokens=%+v", found, tokens)
				}
			})
		}
	}
}

func TestF8EnvelopeIndependentOfVendorSelection(t *testing.T) {
	for _, adapter := range []string{"standard", "qwen", "glm", "deepseek", "kimi"} {
		// Actual Chat protocol takes priority over provider and vendor selection.
		p := usageParser{protocol: config.ProtocolOpenAIChat, endpointType: config.EndpointTypeGemini, model: "Unfamiliar-Model", usageAdapter: adapter}
		tokens, _, _ := p.extractTokens("application/json", []byte(`{"error":{"code":502}}`))
		if tokens.responseStatus != "failed" {
			t.Fatalf("adapter=%s status=%q", adapter, tokens.responseStatus)
		}
	}
}

func TestF7HTTPFailureCompletion(t *testing.T) {
	up := httptest.NewServer(fakellm.New(fakellm.Options{}))
	defer up.Close()
	s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL)
	defer s.Close()
	stats := &completionRecorder{}
	s.SetRequestStatsRecorder(stats)
	r := statsRequest(t, "/v1/chat/completions")
	r.Header.Set("X-Fake-Scenario", "preset=openrouter_403_key_limit")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("HTTP=%d body=%s", w.Code, w.Body.String())
	}
	assertCompletions(t, stats, requeststats.Failure)
}

func TestF8CompletionFailureExactlyOnce(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/embeddings"} {
		for _, stream := range []bool{false, true} {
			for _, withUsage := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/usage=%v", path, stream, withUsage), func(t *testing.T) {
					body, typ := `{"error":{"code":502,"message":"upstream failed"}`, "application/json"
					if withUsage {
						if path == "/v1/responses" {
							body += `,"usage":{"input_tokens":7,"output_tokens":3}`
						} else {
							body += `,"usage":{"prompt_tokens":7,"completion_tokens":3}`
						}
					}
					body += "}"
					if stream {
						body, typ = "data: "+body+"\n\ndata: [DONE]\n\n", "text/event-stream"
					}
					var calls atomic.Int64
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", typ)
						_, _ = w.Write([]byte(body))
					}))
					defer up.Close()
					s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL)
					defer s.Close()
					stats, billing := &completionRecorder{}, &adapterEventRecorder{}
					s.SetRequestStatsRecorder(stats)
					s.SetUsageRecorder(billing)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, statsRequest(t, path))
					if w.Code != 200 || w.Body.String() != body || calls.Load() != 1 {
						t.Fatalf("HTTP=%d body=%q calls=%d", w.Code, w.Body.String(), calls.Load())
					}
					assertCompletions(t, stats, requeststats.Failure)
					if len(billing.events) != 1 || billing.events[0].ResponseStatus != "failed" {
						t.Fatalf("usage events=%+v", billing.events)
					}
					wantInput, wantOutput := int64(0), int64(0)
					// A Responses stream's official final usage is on its terminal
					// response event; a top-level envelope usage there is partial,
					// so it settles as zero usage (upstream_interrupted_no_usage).
					responsesStream := path == "/v1/responses" && stream
					if withUsage && !responsesStream {
						wantInput, wantOutput = 7, 3
					}
					if withUsage && responsesStream && !strings.Contains(billing.events[0].UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
						t.Fatalf("partial Responses usage not settled: %+v", billing.events[0])
					}
					if billing.events[0].InputTokens != wantInput || billing.events[0].OutputTokens != wantOutput {
						t.Fatalf("usage lost or fabricated: %+v", billing.events[0])
					}
				})
			}
		}
	}
}
