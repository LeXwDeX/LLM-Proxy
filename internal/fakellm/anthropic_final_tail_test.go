package fakellm

import (
	"fmt"
	"strings"
	"testing"
)

// Synthetic tails are counterexamples, not official Claude sequences.
func TestAnthropicFinalSnapshotTailModes(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct {
		mode       string
		lifecycle  []string
		deltas     int
		emptyUsage bool
		replace    bool
	}{
		{"advisor_tail_nonfinal", []string{"message_delta", "message_delta"}, 2, false, false},
		{"advisor_tail_empty_stop", []string{"message_delta", "message_delta", "message_stop"}, 2, true, false},
		{"advisor_tail_error_empty_stop", []string{"message_delta", "error", "message_delta", "message_stop"}, 2, true, false},
		{"replace_empty_iterations", []string{"message_delta", "message_delta", "message_stop"}, 2, false, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"Executor-M","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"[[fake iterations=%s usage.in=100 usage.out=20 usage.cached=30]]"}]}`, tc.mode)
			resp, raw := post(t, ts.URL+"/v1/messages", body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
			}
			var lifecycle []string
			var deltas []map[string]any
			for _, ev := range readSSE(t, raw) {
				switch ev.name {
				case "message_delta", "message_stop", "error":
					lifecycle = append(lifecycle, ev.name)
				}
				if ev.name == "message_delta" {
					deltas = append(deltas, decode(t, []byte(ev.data)))
				}
			}
			if strings.Join(lifecycle, ",") != strings.Join(tc.lifecycle, ",") || len(deltas) != tc.deltas {
				t.Fatalf("events=%v deltas=%d want %v/%d", lifecycle, len(deltas), tc.lifecycle, tc.deltas)
			}
			final, _ := deltas[0]["usage"].(map[string]any)
			items, _ := final["iterations"].([]any)
			if dig(deltas[0], "delta/stop_reason") != "end_turn" || len(items) != 2 {
				t.Fatalf("first final delta changed: %+v", deltas[0])
			}
			second, _ := deltas[1]["usage"].(map[string]any)
			switch {
			case tc.replace:
				replaced, _ := second["iterations"].([]any)
				if dig(deltas[1], "delta/stop_reason") != "end_turn" || replaced == nil || len(replaced) != 0 {
					t.Fatalf("explicit empty iterations missing: %+v", deltas[1])
				}
			case tc.emptyUsage:
				if len(second) != 0 || dig(deltas[1], "delta/stop_reason") != nil {
					t.Fatalf("empty delta is not empty: %+v", deltas[1])
				}
			default:
				if len(second) != 1 || second["output_tokens"] != float64(20) || dig(deltas[1], "delta/stop_reason") != nil {
					t.Fatalf("non-final tail changed: %+v", deltas[1])
				}
			}
			if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"`+tc.mode+`"}}`, nil); resp.StatusCode != 200 {
				t.Fatalf("control HTTP %d: %s", resp.StatusCode, raw)
			}
		})
	}
	if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"advisor_tail_bogus"}}`, nil); resp.StatusCode != 400 {
		t.Fatalf("invalid tail accepted: %d %s", resp.StatusCode, raw)
	}
}

// Synthetic counterexample: an invalid final between two valid finals must
// carry the exact wire values the proxy validates.
func TestAnthropicInvalidFinalRetryMode(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"model":"Executor-M","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"[[fake iterations=advisor_tail_invalid_final_retry usage.in=100 usage.out=20 usage.cached=30]]"}]}`
	resp, raw := post(t, ts.URL+"/v1/messages", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var deltas []map[string]any
	var lifecycle []string
	for _, ev := range readSSE(t, raw) {
		switch ev.name {
		case "message_delta", "message_stop":
			lifecycle = append(lifecycle, ev.name)
		}
		if ev.name == "message_delta" {
			deltas = append(deltas, decode(t, []byte(ev.data)))
		}
	}
	if strings.Join(lifecycle, ",") != "message_delta,message_delta,message_delta,message_stop" || len(deltas) != 3 {
		t.Fatalf("events=%v deltas=%d", lifecycle, len(deltas))
	}
	valid, _ := deltas[0]["usage"].(map[string]any)
	if dig(deltas[0], "delta/stop_reason") != "end_turn" || valid["input_tokens"] != float64(7) || valid["output_tokens"] != float64(2) || valid["cache_read_input_tokens"] != float64(0) {
		t.Fatalf("first final=%+v", deltas[0])
	}
	invalid, _ := deltas[1]["usage"].(map[string]any)
	if dig(deltas[1], "delta/stop_reason") != "end_turn" || invalid["input_tokens"] != float64(11) || invalid["output_tokens"] != nil || invalid["cache_read_input_tokens"] != float64(1000) {
		t.Fatalf("invalid final=%+v", deltas[1])
	}
	recovered, _ := deltas[2]["usage"].(map[string]any)
	items, _ := recovered["iterations"].([]any)
	if dig(deltas[2], "delta/stop_reason") != "end_turn" || recovered["input_tokens"] != float64(7) || recovered["output_tokens"] != float64(4) || items == nil || len(items) != 0 {
		t.Fatalf("recovered final=%+v", deltas[2])
	}
	if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"advisor_tail_invalid_final_retry"}}`, nil); resp.StatusCode != 200 {
		t.Fatalf("control HTTP %d: %s", resp.StatusCode, raw)
	}
}

// Synthetic counterexample: a non-usage Chat tail after the official usage
// chunk, before [DONE], with another model and service tier.
func TestChatFinalSnapshotTailMode(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"model":"Executor-M","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"[[fake chat_tail=reprice_nonfinal pricing=standard usage.in=7 usage.out=2]]"}]}`
	resp, raw := post(t, ts.URL+"/v1/chat/completions", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	events := readSSE(t, raw)
	if len(events) < 3 || events[len(events)-1].data != "[DONE]" {
		t.Fatalf("events=%+v", events)
	}
	tail := decode(t, []byte(events[len(events)-2].data))
	if tail["model"] != "mutated-tail" || tail["service_tier"] != "priority" {
		t.Fatalf("tail=%+v", tail)
	}
	if _, present := tail["usage"]; present {
		t.Fatalf("tail carries usage: %+v", tail)
	}
	usageChunk := decode(t, []byte(events[len(events)-3].data))
	usage, _ := usageChunk["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(7) || usage["completion_tokens"] != float64(2) || dig(usageChunk, "choices") == nil {
		t.Fatalf("usage chunk=%+v", usageChunk)
	}
	if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"chat_tail":"reprice_nonfinal"}}`, nil); resp.StatusCode != 200 {
		t.Fatalf("control HTTP %d: %s", resp.StatusCode, raw)
	}
	if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"chat_tail":"bogus"}}`, nil); resp.StatusCode != 400 {
		t.Fatalf("invalid tail accepted: %d %s", resp.StatusCode, raw)
	}
}
