package fakellm

import (
	"strings"
	"testing"
)

// iteration_model_prefix is a synthetic, non-official contract assumption: an
// upstream that echoes every iteration model under its own namespace. Executor
// and compaction items echo the received request model; fixed supplementary
// names gain the prefix unless they already carry it.
func TestAnthropicIterationModelPrefix(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct {
		mode, top string
		models    []string
	}{
		{"advisor", "Vendor/exec", []string{"Vendor/exec", "Vendor/fake-advisor"}},
		{"compaction", "Vendor/exec", []string{"Vendor/exec", "Vendor/exec"}},
		{"fallback_output", "Vendor/fake-fallback", []string{"Vendor/exec", "Vendor/fake-fallback"}},
		{"fallback_chain", "Vendor/fake-fallback", []string{"Vendor/exec", "Vendor/fake-middle", "Vendor/fake-fallback"}},
	} {
		for _, stream := range []bool{false, true} {
			d := `[[fake iterations=` + tc.mode + ` iteration_model_prefix=Vendor/]]`
			body := `{"model":"Vendor/exec","max_tokens":10,"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"messages":[{"role":"user","content":"` + d + `"}]}`
			_, b := post(t, ts.URL+"/v1/messages", body, nil)
			var usage map[string]any
			top := ""
			if !stream {
				payload := decode(t, b)
				usage, _ = payload["usage"].(map[string]any)
				top, _ = payload["model"].(string)
			} else {
				for _, ev := range readSSE(t, b) {
					payload := decode(t, []byte(ev.data))
					if ev.name == "message_start" {
						top, _ = dig(payload, "message/model").(string)
					}
					if ev.name == "message_delta" {
						usage, _ = payload["usage"].(map[string]any)
					}
				}
				if tc.mode == "fallback_output" || tc.mode == "fallback_chain" {
					// Mid-output fallback keeps the executor at message_start.
					tc.top = "Vendor/exec"
				}
			}
			if top != tc.top {
				t.Fatalf("%s stream=%v: top model=%q want %q", tc.mode, stream, top, tc.top)
			}
			items, _ := usage["iterations"].([]any)
			var got []string
			for _, item := range items {
				model, _ := dig(item, "model").(string)
				got = append(got, model)
			}
			if strings.Join(got, ",") != strings.Join(tc.models, ",") {
				t.Fatalf("%s stream=%v: iteration models=%v want %v", tc.mode, stream, got, tc.models)
			}
		}
	}
	// Without the control the documented default shape is unchanged.
	_, b := post(t, ts.URL+"/v1/messages", `{"model":"Vendor/exec","max_tokens":10,"messages":[{"role":"user","content":"[[fake iterations=advisor]]"}]}`, nil)
	items, _ := dig(decode(t, b), "usage/iterations").([]any)
	if len(items) != 2 || dig(items[0], "model") != nil || dig(items[1], "model") != "fake-advisor" {
		t.Fatalf("default iterations changed: %s", b)
	}
	// /_control accepts the same field.
	if resp, raw := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"advisor","iteration_model_prefix":"Other/"}}`, nil); resp.StatusCode != 200 {
		t.Fatalf("control: %d %s", resp.StatusCode, raw)
	}
	_, b = post(t, ts.URL+"/v1/messages", `{"model":"Vendor/exec","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	items, _ = dig(decode(t, b), "usage/iterations").([]any)
	if len(items) != 2 || dig(items[0], "model") != "Other/Vendor/exec" || dig(items[1], "model") != "Other/fake-advisor" {
		t.Fatalf("control iterations: %s", b)
	}
}
