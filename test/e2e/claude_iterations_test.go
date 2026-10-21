//go:build e2e

package e2e

import (
	"testing"
)

// Issue #7: a real proxy process persists Claude usage.iterations judgments
// for JSON and SSE. One executor "message" item equal to top-level keeps the
// cost complete; an "advisor_message" item is a separately priced model and
// stays conservative. Top-level counters are billed once either way. All
// upstream calls terminate in the in-process deterministic fakellm server.
func TestClaudeUsageIterationsPersisted(t *testing.T) {
	for _, tc := range []struct {
		mode, diagnostic string
		incomplete       bool
	}{{"message", "", false}, {"advisor", "multi_model_iterations", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			alias := "e2e-iterations-" + tc.mode
			tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": expand("{A}/anthropic", alias).(string), "api_key": uniq("sk-fake-iterations"), "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			compressionPrice(t, tg, "unfamiliar-upstream")
			for _, stream := range []bool{false, true} {
				body := expand(messagesBody("[[fake iterations="+tc.mode+" pricing=standard usage.in=100 usage.out=20 usage.cached=30]]", stream), alias)
				resp, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("stream=%v HTTP %d %s", stream, resp.StatusCode, raw)
				}
			}
			events := usageFor(t, name, 2)
			if len(events) != 2 {
				t.Fatalf("events=%d", len(events))
			}
			for _, ev := range events {
				if ev.InputTokens != 70 || ev.OutputTokens != 20 || ev.CachedTokens != 30 || ev.UsageStatus != "complete" || ev.UsageAdapter != "standard_anthropic" ||
					ev.CostIncomplete != tc.incomplete || ev.UsageDiagnostic != tc.diagnostic {
					t.Fatalf("persisted event %+v", ev)
				}
			}
		})
	}
}
