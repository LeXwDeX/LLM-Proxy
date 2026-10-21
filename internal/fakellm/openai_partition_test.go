package fakellm

import (
	"fmt"
	"testing"
)

// Synthetic partitions exercise the parser, not a vendor contract. The disjoint
// 24/1 sample was observed only on Chat JSON; SSE/Responses are test extensions.
func TestOpenAIOutputPartitionDirectives(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct {
		name, directive string
		text, image     any
	}{
		{"absent", "", nil, nil},
		{"included", "usage.text_out=25", 25.0, nil},
		{"disjoint", "usage.text_out=1", 1.0, nil},
		{"zero", "usage.text_out=0 usage.image_out=0", 0.0, 0.0},
		{"impossible", "usage.text_out=1 usage.image_out=24", 1.0, 24.0},
	} {
		for _, protocol := range []string{"chat/completions", "responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.name, protocol, stream), func(t *testing.T) {
					directive := "[[fake usage.in=8 usage.out=25 usage.reasoning=24 " + tc.directive + "]]"
					body := fmt.Sprintf(`{"model":"partition-model","stream":%v,"stream_options":{"include_usage":true},"input":%q,"messages":[{"role":"user","content":%q}]}`, stream, directive, directive)
					resp, raw := post(t, ts.URL+"/v1/"+protocol, body, nil)
					if resp.StatusCode != 200 {
						t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
					}
					prefix := "usage/completion_tokens_details/"
					if protocol == "responses" {
						prefix = "usage/output_tokens_details/"
					}
					if stream {
						events := readSSE(t, raw)
						if protocol == "responses" {
							if len(events) == 0 || events[len(events)-1].name != "response.completed" {
								t.Fatalf("missing terminal: %s", raw)
							}
							raw = []byte(events[len(events)-1].data)
							prefix = "response/" + prefix
						} else {
							if len(events) < 2 || events[len(events)-1].data != "[DONE]" {
								t.Fatalf("missing terminal: %s", raw)
							}
							raw = []byte(events[len(events)-2].data)
						}
					}
					final := decode(t, raw)
					for field, want := range map[string]any{"text_tokens": tc.text, "image_tokens": tc.image, "reasoning_tokens": 24.0} {
						if got := dig(final, prefix+field); got != want {
							t.Errorf("%s=%v want %v", prefix+field, got, want)
						}
					}
				})
			}
		}
	}
}

func TestOpenAIOutputPartitionControlMerge(t *testing.T) {
	_, ts := newTestServer(t)
	resp, body := post(t, ts.URL+"/_control", `{"scenario":{"usage":{"text_out":25,"image_out":5}}}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("control: %d %s", resp.StatusCode, body)
	}
	for _, directive := range []string{"", "[[fake usage.text_out=0 usage.image_out=0]]", ""} {
		resp, body = post(t, ts.URL+"/v1/responses", fmt.Sprintf(`{"model":"partition-model","input":%q}`, directive), nil)
		if resp.StatusCode != 200 {
			t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
		}
		text, image := 25.0, 5.0
		if directive != "" {
			text, image = 0, 0
		}
		final := decode(t, body)
		if dig(final, "usage/output_tokens_details/text_tokens") != text || dig(final, "usage/output_tokens_details/image_tokens") != image {
			t.Errorf("control merge failed for %q: %s", directive, body)
		}
	}
	for _, directive := range []string{"usage.text_out=bad", "usage.image_out=1.5"} {
		if _, err := ParseDirective(directive); err == nil {
			t.Errorf("accepted malformed count %q", directive)
		}
	}
}
