package fakellm

import (
	"fmt"
	"testing"
)

// Gemini's OpenAI-compatible Chat reports reasoning and text as disjoint output
// parts (reasoning + text = completion). The shape mirrors a redacted real
// request observed on 2026-10-20; it is an observation, not an official example.
func TestGeminiOpenAICompatChatUsageDisjointReasoning(t *testing.T) {
	_, ts := newTestServer(t)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			resp, body := post(t, ts.URL+"/gemini/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%v,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"[[fake usage.in=8 usage.out=25 usage.reasoning=24]]"}]}`, stream), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
			}
			var final map[string]any
			if stream {
				events := readSSE(t, body)
				if len(events) < 2 || events[len(events)-1].data != "[DONE]" {
					t.Fatalf("incomplete stream: %s", body)
				}
				final = decode(t, []byte(events[len(events)-2].data))
			} else {
				final = decode(t, body)
			}
			for path, want := range map[string]any{
				"usage/prompt_tokens":                              8.0,
				"usage/completion_tokens":                          25.0,
				"usage/total_tokens":                               33.0,
				"usage/prompt_tokens_details/cached_tokens":        0.0,
				"usage/prompt_tokens_details/cache_write_tokens":   0.0,
				"usage/prompt_tokens_details/text_tokens":          8.0,
				"usage/completion_tokens_details/reasoning_tokens": 24.0,
				"usage/completion_tokens_details/text_tokens":      1.0,
			} {
				if got := dig(final, path); got != want {
					t.Errorf("%s=%v, want %v", path, got, want)
				}
			}
		})
	}
}
