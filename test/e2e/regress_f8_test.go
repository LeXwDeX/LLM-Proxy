//go:build e2e

package e2e

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// #160 F8 is a standard OpenAI envelope, not an OpenRouter-specific rule.
func TestRegressF8StandardErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, preset, body string
		input, output              int64
	}{
		{"gateway preset", "/openrouter/api/v1", "openrouter_200_error", `{"error":{"code":502,"message":"Provider returned error","metadata":{"provider_name":"fake","raw":"upstream failed"}}}`, 0, 0},
		{"standard with usage", "/openai/v1", "", `{"error":{"message":"failed"},"usage":{"prompt_tokens":7,"completion_tokens":3}}`, 7, 3},
		{"sse usage after error", "/openai/v1", "", "data: {\"error\":{\"code\":502,\"message\":\"failed\"}}\n\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", 7, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, alias := pool(t), uniq("e2e-f8")
			keysA, keysB := []string{uniq("sk-f8-a0"), uniq("sk-f8-a1")}, []string{uniq("sk-f8-b0"), uniq("sk-f8-b1")}
			tgA := keyedTarget(t, w.urlA, tc.prefix, alias, keysA, map[string]any{"pool": p})
			tgB := keyedTarget(t, w.urlB, tc.prefix, alias, keysB, map[string]any{"pool": p})
			name, key := client(t, nil, []string{p})
			scenario := &fakellm.Scenario{Status: 200, Preset: tc.preset}
			if tc.preset == "" {
				scenario.Body = tc.body
			}
			for _, k := range keysA {
				w.fakeA.SetKeyScenario(k, scenario)
				defer w.fakeA.SetKeyScenario(k, nil)
			}
			for _, k := range keysB {
				w.fakeB.SetKeyScenario(k, scenario)
				defer w.fakeB.SetKeyScenario(k, nil)
			}
			fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
			resp, wire := chat(t, key, alias, "hi")
			if resp.StatusCode != 200 || string(wire) != tc.body {
				t.Fatalf("wire changed: HTTP %d %q", resp.StatusCode, wire)
			}
			events := usageFor(t, name, 1)
			if len(events) != 1 {
				t.Fatalf("usage recorded %d times", len(events))
			}
			ev := events[0]
			if ev.ResponseStatus != "failed" || ev.StatusCode != 200 || ev.InputTokens != tc.input || ev.OutputTokens != tc.output {
				t.Fatalf("event=%+v", ev)
			}
			total := 0
			for _, k := range keysA {
				total += hits(w.fakeA, k, fromA)
			}
			for _, k := range keysB {
				total += hits(w.fakeB, k, fromB)
			}
			if total != 1 {
				t.Fatalf("request sent %d times (retry/failover)", total)
			}
			for _, tg := range []string{tgA, tgB} {
				for i := 0; i < 2; i++ {
					if st := keyStatus(t, tg, i); st.Exhausted {
						t.Fatalf("standard error disabled key: %+v", st)
					}
				}
			}
		})
	}
}
