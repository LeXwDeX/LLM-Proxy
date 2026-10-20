package fakellm

import (
	"fmt"
	"strings"
	"testing"
)

// deepseek_aborted models the documented Chat Completions finish_reason
// "aborted" (https://api-docs.deepseek.com/zh-cn/api/create-chat-completion,
// checked 2026-10-20). It is a normal HTTP 200 reply, not an error envelope.
func TestDeepSeekAbortedPreset(t *testing.T) {
	fake, ts := newTestServer(t)
	for _, path := range []string{"/deepseek/v1/chat/completions", "/v1/chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, viaHeader := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/header=%v", path, stream, viaHeader), func(t *testing.T) {
					directive := "preset=deepseek_aborted usage.in=100 usage.out=7 usage.cached=40"
					content := "[[fake " + directive + "]]"
					hdr := map[string]string(nil)
					if viaHeader {
						content, hdr = "hi", map[string]string{"X-Fake-Scenario": directive}
					}
					from := fake.LastSeq()
					resp, body := post(t, ts.URL+path, fmt.Sprintf(`{"model":"deepseek-chat","stream":%v,"messages":[{"role":"user","content":%q}]}`, stream, content), hdr)
					if resp.StatusCode != 200 {
						t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
					}
					var final map[string]any
					if stream {
						if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
							t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
						}
						events := readSSE(t, body)
						if len(events) < 3 || events[len(events)-1].data != "[DONE]" {
							t.Fatalf("stream must end with [DONE]: %s", body)
						}
						for _, event := range events[:len(events)-2] {
							m := decode(t, []byte(event.data))
							if m["usage"] != nil || dig(m, "choices/0/finish_reason") != nil {
								t.Fatalf("non-final chunk carries usage or finish: %v", m)
							}
						}
						// DeepSeek puts usage on the last data chunk, which also
						// carries the terminal finish reason.
						final = decode(t, []byte(events[len(events)-2].data))
					} else {
						if resp.Header.Get("Content-Type") != "application/json" {
							t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
						}
						final = decode(t, body)
					}
					if final["error"] != nil || dig(final, "choices/0/finish_reason") != "aborted" {
						t.Fatalf("aborted finish missing: %v", final)
					}
					for p, want := range map[string]float64{"usage/prompt_tokens": 100, "usage/completion_tokens": 7, "usage/total_tokens": 107,
						"usage/prompt_cache_hit_tokens": 40, "usage/prompt_cache_miss_tokens": 60, "usage/prompt_tokens_details/cached_tokens": 40} {
						if got := dig(final, p); got != want {
							t.Errorf("%s=%v, want %v", p, got, want)
						}
					}
					records := fake.RequestsSince(from)
					if len(records) != 1 || records[0].Outcome != "ok" || records[0].Flavor != FlavorDeepSeek || records[0].Usage == nil || records[0].Usage.Input != 100 || records[0].Usage.Output != 7 {
						t.Fatalf("journal: %+v", records)
					}
				})
			}
		}
	}
	// The contract only covers Chat Completions; other wires fail loudly.
	for _, path := range []string{"/deepseek/anthropic/v1/messages", "/deepseek/v1/responses"} {
		resp, body := post(t, ts.URL+path, `{"model":"deepseek-chat","max_tokens":8,"messages":[],"input":"hi"}`, map[string]string{"X-Fake-Scenario": "preset=deepseek_aborted"})
		if resp.StatusCode != 400 || !strings.Contains(string(body), "preset deepseek_aborted only applies to Chat Completions") {
			t.Errorf("%s: HTTP %d %s", path, resp.StatusCode, body)
		}
	}
}
