//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// #160 F6: explicit Kimi governs unfamiliar models on a fake gateway. Standard
// stays standard on the same gateway. Only quota/auth types disable keys.
func TestRegressF6KimiKeyPolicy(t *testing.T) {
	for _, tc := range []struct {
		adapter, preset, reason string
		hits                    int
		permanent               bool
	}{
		{"kimi", "kimi_quota", "quota_exceeded_api", 1, true},
		{"kimi", "kimi_invalid_key", "invalid_token", 1, false},
		{"kimi", "kimi_rate", "rate_limited", 2, false},
		{"kimi", "kimi_overloaded", "rate_limited", 2, false},
		{"standard", "kimi_quota", "rate_limited", 2, false},
	} {
		t.Run(tc.adapter+"/"+tc.preset, func(t *testing.T) {
			k0, k1 := uniq("sk-f6a"), uniq("sk-f6b")
			alias := uniq("e2e-f6")
			tg := target(t, map[string]any{"endpoint_type": "openai", "usage_adapter": tc.adapter, "endpoint": w.urlA + "/kimi/v1", "api_key": k0, "api_keys": []string{k1},
				"model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
			_, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(k0, &fakellm.Scenario{Preset: tc.preset})
			defer w.fakeA.SetKeyScenario(k0, nil)
			from := w.fakeA.LastSeq()
			chat(t, key, alias, "hi")
			chat(t, key, alias, "hi")
			st := keyStatus(t, tg, 0)
			if n := hits(w.fakeA, k0, from); n != tc.hits {
				t.Fatalf("key0 hit %d times, want %d", n, tc.hits)
			}
			if n := hits(w.fakeA, k1, from); n != 2 {
				t.Fatalf("healthy key hit %d times, want 2", n)
			}
			if !st.Exhausted || st.ExhaustReason != tc.reason {
				t.Fatalf("state=%+v want=%s", st, tc.reason)
			}
			longTerm := st.CooldownEnd.IsZero() || time.Until(st.CooldownEnd) > time.Hour
			if longTerm != tc.permanent {
				t.Fatalf("unexpected cooldown %+v", st)
			}
		})
	}
}

func TestRegressF6KimiOutboundCacheTTL(t *testing.T) {
	for _, adapter := range []string{"kimi", "standard"} {
		for _, stream := range []bool{false, true} {
			t.Run(adapter+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				alias := uniq("e2e-f6-cache")
				tg := target(t, map[string]any{"endpoint_type": "openai", "usage_adapter": adapter, "endpoint": w.urlA + "/kimi/v1", "api_key": uniq("sk-f6"),
					"custom_body":    map[string]any{"prompt_cache_options": map[string]any{"ttl": "1h"}},
					"model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
				name, key := client(t, []string{tg}, nil)
				body := chatBody(stdUsage, stream)
				body["model"] = alias
				body["prompt_cache_options"] = map[string]any{"ttl": "5m"} // overridden outbound
				resp, wire := call(t, key, "POST", "/v1/chat/completions", body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("HTTP %d %s", resp.StatusCode, wire)
				}
				ev := usageFor(t, name, 1)[0]
				want := billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}
				wantAdapter := "standard_openai"
				if adapter == "kimi" {
					want.cw1h = 20
					wantAdapter = "kimi"
				}
				checkBilled(t, ev, want)
				if ev.UsageAdapter != wantAdapter || ev.CostIncomplete {
					t.Fatalf("usage=%+v", ev)
				}
			})
		}
	}
}
