package fakellm

import (
	"fmt"
	"strings"
	"testing"
)

func TestAnthropicFallbackStartModel(t *testing.T) {
	_, ts := newTestServer(t)
	for _, mode := range []string{"fallback_zero_unknown", "fallback_sticky", "fallback_output"} {
		t.Run(mode, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"Executor-M","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"[[fake iterations=%s]]"}]}`, mode)
			resp, raw := post(t, ts.URL+"/v1/messages", body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
			}
			want := "fake-fallback"
			if mode == "fallback_output" {
				want = "Executor-M"
			}
			start, boundary := false, false
			for _, ev := range readSSE(t, raw) {
				payload := decode(t, []byte(ev.data))
				if ev.name == "message_start" {
					start = true
					if dig(payload, "message/model") != want {
						t.Fatalf("start=%+v want model %s", payload, want)
					}
				}
				if dig(payload, "content_block/type") == "fallback" {
					boundary = true
					if dig(payload, "content_block/to/model") != "fake-fallback" {
						t.Fatalf("boundary=%+v", payload)
					}
				}
			}
			if !start || boundary != (mode != "fallback_sticky") {
				t.Fatalf("start=%v boundary=%v", start, boundary)
			}
		})
	}
}

func TestAnthropicRefusalAndStreamGapModes(t *testing.T) {
	_, ts := newTestServer(t)
	for _, mode := range []string{"refusal_free", "refusal_paid", "refusal_null", "refusal_unknown", "refusal_missing_output", "message_refusal_free", "advisor_refusal_free", "compaction_refusal_free", "fallback_refusal_unknown", "fallback_missing_input", "advisor_early_only"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", mode, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"Executor-M","max_tokens":100,"stream":%v,"messages":[{"role":"user","content":"[[fake iterations=%s usage.in=100 usage.out=20 usage.cached=30 usage.reasoning=0]]"}]}`, stream, mode)
				resp, raw := post(t, ts.URL+"/v1/messages", body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
				}
				var final map[string]any
				early := false
				if stream {
					for _, ev := range readSSE(t, raw) {
						payload := decode(t, []byte(ev.data))
						if ev.name == "message_delta" {
							if dig(payload, "delta/stop_reason") == nil {
								early = dig(payload, "usage/iterations") != nil
							} else {
								final = payload
							}
						}
						if strings.Contains(mode, "refusal") && dig(payload, "delta/type") == "text_delta" {
							t.Fatal("zero output refusal emitted text")
						}
					}
				} else {
					final = decode(t, raw)
					if strings.Contains(mode, "refusal") && len(final["content"].([]any)) != 0 {
						t.Fatalf("zero output refusal content: %+v", final)
					}
				}
				u, _ := final["usage"].(map[string]any)
				if strings.Contains(mode, "refusal") {
					stop := final
					if stream {
						stop, _ = final["delta"].(map[string]any)
					}
					if stop["stop_reason"] != "refusal" {
						t.Fatalf("stop=%+v", stop)
					}
					if mode == "refusal_missing_output" {
						if _, ok := u["output_tokens"]; ok {
							t.Fatalf("output not omitted: %+v", u)
						}
					} else if u["output_tokens"] != float64(0) {
						t.Fatalf("refusal usage=%+v", u)
					}
				}
				if mode == "fallback_missing_input" && (u["input_tokens"] == nil) != stream {
					t.Fatalf("fallback input presence: %+v", u)
				}
				if mode == "advisor_early_only" && stream && (!early || u["iterations"] != nil) {
					t.Fatalf("early=%v final=%+v", early, final)
				}
				if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"`+mode+`"}}`, nil); resp.StatusCode != 200 {
					t.Fatalf("control HTTP %d: %s", resp.StatusCode, raw)
				}
			})
		}
	}
}

func TestAnthropicUsageThenErrorOrdering(t *testing.T) {
	_, ts := newTestServer(t)
	for _, mode := range []string{"advisor_then_error", "advisor_refusal_free_then_error", "advisor_intermediate_then_error", "advisor_stop_then_error"} {
		t.Run(mode, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"Executor-M","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"[[fake iterations=%s usage.in=7 usage.out=2 usage.cached=0 usage.cw=0 usage.reasoning=0]]"}]}`, mode)
			resp, raw := post(t, ts.URL+"/v1/messages", body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
			}
			var lifecycle []string
			for _, ev := range readSSE(t, raw) {
				switch ev.name {
				case "message_delta", "message_stop", "error":
					lifecycle = append(lifecycle, ev.name)
				}
				if ev.name == "message_delta" {
					payload := decode(t, []byte(ev.data))
					wantReason := any("end_turn")
					if mode == "advisor_intermediate_then_error" || mode == "advisor_stop_then_error" {
						wantReason = nil
					}
					if mode == "advisor_refusal_free_then_error" {
						wantReason = "refusal"
					}
					if dig(payload, "delta/stop_reason") != wantReason || dig(payload, "usage/iterations") == nil {
						t.Fatalf("wrong usage phase: %+v", payload)
					}
				}
			}
			want := "message_delta,error"
			if mode == "advisor_intermediate_then_error" {
				want = "message_delta,error,message_stop"
			}
			if mode == "advisor_stop_then_error" {
				want = "message_delta,message_stop,error"
			}
			if strings.Join(lifecycle, ",") != want {
				t.Fatalf("events=%v want %s", lifecycle, want)
			}
			if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"`+mode+`"}}`, nil); resp.StatusCode != 200 {
				t.Fatalf("control HTTP %d: %s", resp.StatusCode, raw)
			}
		})
	}
}
