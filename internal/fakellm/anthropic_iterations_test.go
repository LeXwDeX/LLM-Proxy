package fakellm

import (
	"strings"
	"testing"
)

// iterations=message|advisor follows the advisor tool doc shape (checked
// 2026-10-21): the executor item equals top-level, advisor usage is separate.
func TestAnthropicUsageIterations(t *testing.T) {
	_, ts := newTestServer(t)
	for _, mode := range []string{"message", "advisor"} {
		d := `[[fake iterations=` + mode + ` usage.in=100 usage.out=10 usage.cached=40 usage.cw5m=20]]`
		body := `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"` + d + `"}]}`
		_, b := post(t, ts.URL+"/v1/messages", body, nil)
		usages := []any{dig(decode(t, b), "usage")}
		_, b = post(t, ts.URL+"/v1/messages", strings.Replace(body, `"max_tokens":10`, `"max_tokens":10,"stream":true`, 1), nil)
		for _, ev := range readSSE(t, b) {
			if ev.name == "message_start" && dig(decode(t, []byte(ev.data)), "message/usage/iterations") != nil {
				t.Fatalf("%s: message_start carries iterations", mode)
			}
			if ev.name == "message_delta" {
				usages = append(usages, dig(decode(t, []byte(ev.data)), "usage"))
			}
		}
		if len(usages) != 2 {
			t.Fatalf("%s: usages=%d", mode, len(usages))
		}
		for _, u := range usages {
			usage, _ := u.(map[string]any)
			items, _ := usage["iterations"].([]any)
			want := 1
			if mode == "advisor" {
				want = 2
			}
			if len(items) != want {
				t.Fatalf("%s: iterations=%v", mode, usage["iterations"])
			}
			first, _ := items[0].(map[string]any)
			for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
				if first[key] != usage[key] {
					t.Fatalf("%s: executor %s=%v top=%v", mode, key, first[key], usage[key])
				}
			}
			if first["type"] != "message" || (mode == "advisor" && dig(items[1], "type") != "advisor_message") {
				t.Fatalf("%s: iterations=%v", mode, items)
			}
		}
	}
	resp, b := post(t, ts.URL+"/v1/messages", `{"x":"[[fake iterations=bogus]]"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "iterations") {
		t.Fatalf("bad iterations directive: %d %s", resp.StatusCode, b)
	}
}

// /_control JSON validates iterations like the directive path; an unset value
// keeps the default response without iterations.
func TestAnthropicUsageIterationsControl(t *testing.T) {
	_, ts := newTestServer(t)
	for _, body := range []string{`{"scenario":{"iterations":"bogus"}}`, `{"keys":{"k":{"iterations":"bogus"}}}`} {
		resp, b := post(t, ts.URL+"/_control", body, nil)
		if resp.StatusCode != 400 || !strings.Contains(string(b), "iterations") {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, b)
		}
	}
	msg := `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	_, b := post(t, ts.URL+"/v1/messages", msg, nil)
	if dig(decode(t, b), "usage/iterations") != nil {
		t.Fatalf("default carries iterations: %s", b)
	}
	if resp, b := post(t, ts.URL+"/_control", `{"scenario":{"iterations":"advisor"}}`, nil); resp.StatusCode != 200 {
		t.Fatalf("valid control: %d %s", resp.StatusCode, b)
	}
	_, b = post(t, ts.URL+"/v1/messages", msg, nil)
	if items, _ := dig(decode(t, b), "usage/iterations").([]any); len(items) != 2 {
		t.Fatalf("advisor control: %s", b)
	}
}
