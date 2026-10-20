package fakellm

import (
	"fmt"
	"strings"
	"testing"
)

func TestMinimaxChatOfficialResponse(t *testing.T) {
	_, ts := newTestServer(t)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			resp, body := post(t, ts.URL+"/minimax/v1/chat/completions", fmt.Sprintf(`{"model":"m","stream":%v,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"[[fake usage.in=100 usage.out=20 usage.cached=30]]"}]}`, stream), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
			}
			var final map[string]any
			if stream {
				events := readSSE(t, body)
				if len(events) < 3 || events[len(events)-1].data != "[DONE]" {
					t.Fatalf("incomplete stream: %s", body)
				}
				for i, event := range events[:len(events)-1] {
					m := decode(t, []byte(event.data))
					if _, ok := m["base_resp"]; ok {
						t.Error("ChatCompletionChunk must not contain base_resp")
					}
					if i < len(events)-2 {
						if usage, ok := m["usage"]; !ok || usage != nil {
							t.Errorf("non-final chunk usage must be null: %v", m)
						}
					} else {
						final = m
						if choices, ok := m["choices"].([]any); !ok || len(choices) != 0 {
							t.Errorf("usage chunk must have empty choices: %v", m)
						}
					}
				}
			} else {
				final = decode(t, body)
				if dig(final, "base_resp/status_code") != float64(0) || dig(final, "base_resp/status_msg") != "" {
					t.Errorf("success base_resp: %v", final["base_resp"])
				}
			}
			for path, want := range map[string]float64{"usage/prompt_tokens": 100, "usage/completion_tokens": 20, "usage/total_tokens": 120, "usage/total_characters": 0, "usage/prompt_tokens_details/cached_tokens": 30} {
				if got := dig(final, path); got != want {
					t.Errorf("%s=%v, want %v", path, got, want)
				}
			}
			for _, path := range []string{"system_fingerprint", "service_tier", "choices/0/logprobs", "choices/0/message/refusal", "usage/completion_tokens_details", "usage/prompt_tokens_details/audio_tokens"} {
				key := path
				m := final
				if last := strings.LastIndex(path, "/"); last >= 0 {
					m, _ = dig(final, path[:last]).(map[string]any)
					key = path[last+1:]
				}
				if _, exists := m[key]; exists {
					t.Errorf("undocumented field %s", path)
				}
			}
		})
	}
}

func TestMinimaxStreamUsageRequiresOptIn(t *testing.T) {
	fake, ts := newTestServer(t)
	for _, options := range []string{"", `,"stream_options":{"include_usage":false}`} {
		from := fake.LastSeq()
		_, body := post(t, ts.URL+"/minimax/v1/chat/completions", `{"model":"m","stream":true,"messages":[]`+options+`}`, nil)
		for _, event := range readSSE(t, body) {
			if event.data == "[DONE]" {
				continue
			}
			m := decode(t, []byte(event.data))
			if usage, exists := m["usage"]; !exists || usage != nil {
				t.Errorf("usage without opt-in: %v", m)
			}
		}
		records := fake.RequestsSince(from)
		if len(records) != 1 {
			t.Fatalf("records=%d, want 1", len(records))
		}
		for _, record := range records {
			if record.Usage != nil {
				t.Errorf("journal billed stream without usage: %+v", record.Usage)
			}
		}
	}
}

func TestMinimaxOfficialPresets(t *testing.T) {
	_, ts := newTestServer(t)
	for _, code := range []int{1002, 1004, 1008, 1041, 2013, 2045, 2049, 2056} {
		resp, body := post(t, ts.URL+"/minimax/v1/chat/completions", `{"messages":[]}`, map[string]string{"X-Fake-Scenario": fmt.Sprintf("preset=minimax_%d", code)})
		m := decode(t, body)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || dig(m, "base_resp/status_code") != float64(code) || dig(m, "base_resp/status_msg") == "" {
			t.Errorf("code %d: HTTP %d %s", code, resp.StatusCode, body)
		}
	}
	if _, ok := lookupPreset("minimax_1008_sse"); ok {
		t.Error("undocumented SSE preset still exists")
	}
	// Reuse protocol-standard presets: MiniMax documents the same ErrorResponse.
	for preset, status := range map[string]int{"rate_limit": 429, "overloaded": 529} {
		resp, body := post(t, ts.URL+"/minimax/anthropic/v1/messages", `{"messages":[]}`, map[string]string{"X-Fake-Scenario": "preset=" + preset})
		m := decode(t, body)
		if resp.StatusCode != status || m["type"] != "error" || m["request_id"] == nil || dig(m, "error/type") != defaultErrType(status) || m["base_resp"] != nil {
			t.Errorf("%s: HTTP %d %s", preset, resp.StatusCode, body)
		}
	}
}
