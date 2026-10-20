package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Official final usage of a stream, before an upstream interruption:
//   - Chat: the upstream sent [DONE], or a chunk carrying usage arrived when
//     every choice seen so far had a finish_reason (or the chunk's choices
//     are empty: the trailing OpenAI usage chunk). One finished choice of
//     several, or a finish_reason without the usage that follows, is not.
//   - Gemini: a chunk carrying usageMetadata arrived when every candidate
//     seen so far had a finishReason.
//
// Official final usage interrupted later records the official values
// (interrupted_after_final_usage); otherwise the event is zero
// (upstream_interrupted_no_usage).
func TestFinalUsageRequiresEveryChoiceFinished(t *testing.T) {
	const chatPath, geminiPath = "/v1/chat/completions", "/v1beta/models/test-model:streamGenerateContent?alt=sse"
	const chatBody, geminiBody = `{"model":"test-model","stream":true,"n":2,"messages":[]}`, `{"contents":[]}`
	chunk := func(s string) string { return "data: " + s + "\n\n" }
	for _, tc := range []struct {
		name, endpointType, path, body string
		events                         []string
		final                          bool
		out                            int64
	}{
		{"chat_n2_one_choice_finished", config.EndpointTypeOpenAI, chatPath, chatBody, []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}],"usage":{"prompt_tokens":11,"completion_tokens":1}}`,
			`{"choices":[{"index":1,"delta":{"content":"b"}}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`,
		}, false, 0},
		{"chat_n2_all_choices_finished", config.EndpointTypeOpenAI, chatPath, chatBody, []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}],"usage":{"prompt_tokens":11,"completion_tokens":1}}`,
			`{"choices":[{"index":1,"delta":{"content":"b"}}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`,
			`{"choices":[{"index":1,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
		}, true, 4},
		{"chat_prompt_only_usage_then_finish", config.EndpointTypeOpenAI, chatPath, chatBody, []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}],"usage":{"prompt_tokens":11}}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}, false, 0},
		{"chat_finish_then_trailing_usage_chunk", config.EndpointTypeOpenAI, chatPath, chatBody, []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}],"usage":null}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		}, true, 7},
		{"chat_finish_and_usage_same_chunk", config.EndpointTypeOpenAI, chatPath, chatBody, []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		}, true, 7},
		{"gemini_two_candidates_one_finished", config.EndpointTypeGemini, geminiPath, geminiBody, []string{
			`{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]}},{"index":1,"content":{"parts":[{"text":"b"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":2}}`,
			`{"candidates":[{"index":0,"content":{"parts":[{"text":"c"}]},"finishReason":"STOP"},{"index":1,"content":{"parts":[{"text":"d"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4}}`,
		}, false, 0},
		{"gemini_two_candidates_all_finished", config.EndpointTypeGemini, geminiPath, geminiBody, []string{
			`{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]}},{"index":1,"content":{"parts":[{"text":"b"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":2}}`,
			`{"candidates":[{"index":0,"content":{"parts":[{"text":"c"}]},"finishReason":"STOP"},{"index":1,"content":{"parts":[{"text":"d"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4}}`,
		}, true, 4},
		{"gemini_single_candidate_finished", config.EndpointTypeGemini, geminiPath, geminiBody, []string{
			`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":1}}`,
			`{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":3}}`,
		}, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range tc.events {
					io.WriteString(w, chunk(event))
				}
				w.(http.Flusher).Flush()
				abortUpstream(w) // interrupted before [DONE] / the stream end
			}))
			defer upstream.Close()
			s, events, _, logs := goneService(t, upstream.URL, tc.endpointType, config.UsageAdapterStandard)
			s.ServeHTTP(newGoneClientWriter(), goneRequest(t, tc.path, tc.body))
			if !tc.final {
				assertInterruptedEvent(t, s, events.snapshot(), logs, noUsageCauseUpstreamError, "streaming", "sse")
				return
			}
			got := events.snapshot()
			if len(got) != 1 {
				t.Fatalf("usage events=%d want 1: %+v", len(got), got)
			}
			ev := got[0]
			if ev.InputTokens != 11 || ev.OutputTokens != tc.out || ev.UsageStatus != usageStatusComplete || ev.CostIncomplete ||
				!hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) {
				t.Fatalf("official final usage must be recorded: %+v", ev)
			}
			if strings.Contains(logs.String(), "recorded zero usage") {
				t.Errorf("official usage settled as zero: %s", logs.String())
			}
		})
	}
}

// Regular vendor shapes (fakellm flavors) keep their official final usage
// when the stream is interrupted after it.
func TestFinalUsageVendorShapes(t *testing.T) {
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	trailing := `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
	for _, tc := range []struct {
		name, adapter string
		events        []string
	}{
		{"deepseek_usage_on_finish", config.UsageAdapterDeepSeek, []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_cache_hit_tokens":3,"prompt_cache_miss_tokens":8}}`}},
		{"glm_usage_on_finish", config.UsageAdapterGLM, []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`}},
		{"openrouter_finish_repeated_on_usage", config.UsageAdapterOpenRouter, []string{finish, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`}},
		{"openai_trailing_usage", config.UsageAdapterStandard, []string{finish, trailing}},
		{"qwen_trailing_usage", config.UsageAdapterQwen, []string{finish, trailing}},
		{"kimi_trailing_usage", config.UsageAdapterKimi, []string{finish, trailing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
				for _, event := range tc.events {
					io.WriteString(w, "data: "+event+"\n\n")
				}
				w.(http.Flusher).Flush()
				abortUpstream(w)
			}))
			defer upstream.Close()
			s, events, _, _ := goneService(t, upstream.URL, config.EndpointTypeOpenAI, tc.adapter)
			s.ServeHTTP(newGoneClientWriter(), goneRequest(t, "/v1/chat/completions", `{"model":"test-model","stream":true,"messages":[]}`))
			got := events.snapshot()
			if len(got) != 1 || got[0].OutputTokens != 7 || got[0].UsageStatus != usageStatusComplete ||
				!hasGoneDiagnostic(got[0].UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) {
				t.Fatalf("vendor final usage lost: %+v", got)
			}
		})
	}
}
