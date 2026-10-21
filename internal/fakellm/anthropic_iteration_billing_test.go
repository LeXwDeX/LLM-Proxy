package fakellm

import (
	"fmt"
	"strings"
	"testing"
)

func TestAnthropicIterationBillingShapes(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct {
		mode                               string
		count                              int
		rootIn, rootOut, firstIn, firstOut float64
		lastType                           string
	}{
		{"advisor", 2, 70, 20, 70, 20, "advisor_message"},
		{"compaction", 2, 70, 20, 11, 2, "message"},
		{"compaction_on_demand", 1, 0, 0, 11, 2, "compaction"},
		{"fallback_output", 2, 70, 20, 13, 4, "fallback_message"},
		{"fallback_zero_unknown", 2, 70, 20, 13, 0, "fallback_message"},
		{"fallback_sticky", 1, 70, 20, 70, 20, "fallback_message"},
		{"fallback_chain", 3, 70, 20, 13, 4, "fallback_message"},
		{"fallback_chain_unknown", 3, 70, 20, 13, 0, "fallback_message"},
		{"fallback_refusal_free", 1, 70, 0, 70, 0, "fallback_message"},
		{"fallback_refusal_paid", 1, 70, 0, 70, 0, "fallback_message"},
		{"fallback_refusal_null", 1, 70, 0, 70, 0, "fallback_message"},
		{"fallback_refusal_missing_category", 1, 70, 0, 70, 0, "fallback_message"},
		{"cache_missing", 2, 70, 20, 35, 10, "message"},
		{"cache_mismatch", 2, 70, 20, 35, 10, "message"},
		{"iterations_unknown", 1, 70, 20, 70, 20, "unknown"},
		{"iterations_invalid", 1, 70, 20, -1, 20, "message"},
		{"iterations_mismatch", 1, 70, 20, 71, 20, "message"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.mode, stream), func(t *testing.T) {
				directive := `[[fake iterations=` + tc.mode + ` usage.in=100 usage.out=20 usage.cached=30 usage.cw5m=0 usage.cw1h=0 usage.reasoning=0]]`
				body := fmt.Sprintf(`{"model":"Executor-M","max_tokens":100,"stream":%v,"messages":[{"role":"user","content":%q}]}`, stream, directive)
				resp, b := post(t, ts.URL+"/v1/messages", body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
				}
				envelope := map[string]any{}
				if stream {
					for _, ev := range readSSE(t, b) {
						if ev.name == "message_start" && dig(decode(t, []byte(ev.data)), "message/usage/iterations") != nil {
							t.Fatal("early iterations")
						}
						if ev.name == "message_delta" {
							envelope = decode(t, []byte(ev.data))
						}
					}
				} else {
					envelope = decode(t, b)
				}
				u, _ := envelope["usage"].(map[string]any)
				items, _ := u["iterations"].([]any)
				if len(items) != tc.count || u["input_tokens"] != tc.rootIn || u["output_tokens"] != tc.rootOut {
					t.Fatalf("usage=%+v", u)
				}
				if dig(items[0], "input_tokens") != tc.firstIn || dig(items[0], "output_tokens") != tc.firstOut || dig(items[len(items)-1], "type") != tc.lastType {
					t.Fatalf("items=%+v", items)
				}
				if strings.HasPrefix(tc.mode, "fallback_") && dig(items[len(items)-1], "model") != "fake-fallback" {
					t.Fatalf("fallback model: %+v", items)
				}
				if strings.HasPrefix(tc.mode, "fallback_refusal_") {
					stop := envelope
					if stream {
						stop, _ = envelope["delta"].(map[string]any)
					}
					if stop["stop_reason"] != "refusal" {
						t.Fatalf("refusal missing: %+v", stop)
					}
					details, _ := stop["stop_details"].(map[string]any)
					switch tc.mode {
					case "fallback_refusal_free":
						if details["category"] != "cyber" {
							t.Fatalf("%+v", details)
						}
					case "fallback_refusal_paid":
						if details["category"] != "bio" {
							t.Fatalf("%+v", details)
						}
					case "fallback_refusal_null":
						value, ok := details["category"]
						if !ok || value != nil {
							t.Fatalf("%+v", details)
						}
					case "fallback_refusal_missing_category":
						if _, ok := details["category"]; ok {
							t.Fatalf("%+v", details)
						}
					}
				}
				if resp, b := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"`+tc.mode+`"}}`, nil); resp.StatusCode != 200 {
					t.Fatalf("control rejected: %d %s", resp.StatusCode, b)
				}
			})
		}
	}
}
