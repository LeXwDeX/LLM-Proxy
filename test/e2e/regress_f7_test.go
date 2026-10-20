//go:build e2e

package e2e

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// Standard keeps the wire contract and does not opt into vendor patches.
func TestRegressF7Standard(t *testing.T) { vendorStandardGuard(t, "F7") }

func TestRegressF7AllInflightKeysPassThrough(t *testing.T) {
	p, alias := pool(t), uniq("e2e-f7-exhausted")
	keysA := []string{uniq("sk-f7a0"), uniq("sk-f7a1")}
	keysB := []string{uniq("sk-f7b0"), uniq("sk-f7b1")}
	tgA := keyedTarget(t, w.urlA, "/openrouter/api/v1", alias, keysA, map[string]any{"pool": p, "usage_adapter": "openrouter"})
	tgB := keyedTarget(t, w.urlB, "/openrouter/api/v1", alias, keysB, map[string]any{"pool": p, "usage_adapter": "openrouter"})
	_, key := client(t, nil, []string{p})
	fromA := w.fakeA.LastSeq()
	if resp, wire := chat(t, key, alias, "hi"); resp.StatusCode != 200 {
		t.Fatalf("warmup: %d %s", resp.StatusCode, wire)
	}
	bad, other, badKeys, otherKeys, tg := w.fakeA, w.fakeB, keysA, keysB, tgA
	if hits(w.fakeA, keysA[0], fromA)+hits(w.fakeA, keysA[1], fromA) == 0 {
		bad, other, badKeys, otherKeys, tg = w.fakeB, w.fakeA, keysB, keysA, tgB
	}
	for _, k := range badKeys {
		bad.SetKeyScenario(k, &fakellm.Scenario{Preset: "openrouter_402_inflight"})
		defer bad.SetKeyScenario(k, nil)
	}
	fromBad, fromOther := bad.LastSeq(), other.LastSeq()
	resp, wire := chat(t, key, alias, "hi")
	if resp.StatusCode != 402 || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("HTTP %d Retry-After=%q body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), wire)
	}
	for i, k := range badKeys {
		if hits(bad, k, fromBad) != 1 {
			t.Fatalf("key%d was not tried exactly once", i)
		}
		st := keyStatus(t, tg, i)
		if !st.Exhausted || st.ExhaustReason != "rate_limited" {
			t.Fatalf("key%d: %+v", i, st)
		}
	}
	for _, k := range otherKeys {
		if hits(other, k, fromOther) != 0 {
			t.Fatal("unexpected target failover")
		}
	}
}

func vendorStandardGuard(t *testing.T, selected string) {
	for _, tc := range []struct {
		name, prefix, preset, body string
		status                     int
	}{
		{"F7", "/openrouter/api/v1", "openrouter_403_key_limit", `{"error":{"code":403,"message":"Key limit exceeded","metadata":{"provider_name":"fake"}},"user_id":"user_fake"}`, 403},
		{"F11", "/minimax/v1", "minimax_1008", `{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`, 200},
	} {
		if tc.name != selected {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			p, alias := pool(t), uniq("e2e-boundary")
			keysA := []string{uniq("sk-boundary-a0"), uniq("sk-boundary-a1")}
			keysB := []string{uniq("sk-boundary-b0"), uniq("sk-boundary-b1")}
			tgA := keyedTarget(t, w.urlA, tc.prefix, alias, keysA, map[string]any{"pool": p})
			tgB := keyedTarget(t, w.urlB, tc.prefix, alias, keysB, map[string]any{"pool": p})
			name, key := client(t, nil, []string{p})
			for _, k := range keysA {
				w.fakeA.SetKeyScenario(k, &fakellm.Scenario{Preset: tc.preset})
				defer w.fakeA.SetKeyScenario(k, nil)
			}
			for _, k := range keysB {
				w.fakeB.SetKeyScenario(k, &fakellm.Scenario{Preset: tc.preset})
				defer w.fakeB.SetKeyScenario(k, nil)
			}
			fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
			for i := 0; i < 2; i++ {
				resp, wire := chat(t, key, alias, "hi")
				if resp.StatusCode != tc.status || string(wire) != tc.body {
					t.Fatalf("wire changed: HTTP %d %q", resp.StatusCode, wire)
				}
			}
			total := 0
			for _, k := range keysA {
				total += hits(w.fakeA, k, fromA)
			}
			for _, k := range keysB {
				total += hits(w.fakeB, k, fromB)
			}
			if total != 2 {
				t.Fatalf("two client requests sent %d upstream requests (retry/failover)", total)
			}
			for _, tg := range []string{tgA, tgB} {
				for i := 0; i < 2; i++ {
					if st := keyStatus(t, tg, i); st.Exhausted {
						t.Fatalf("unsupported policy disabled key: %+v", st)
					}
				}
			}
			// Non-2xx has no billing event under the existing contract. The
			// HTTP failure completion is covered by TestF7HTTPFailureCompletion.
			if tc.status >= 400 {
				return
			}
			events := usageFor(t, name, 2)
			if len(events) != 2 {
				t.Fatalf("two requests recorded %d usage events", len(events))
			}
			for _, ev := range events {
				if ev.StatusCode != tc.status || ev.UsageStatus == "complete" || ev.InputTokens != 0 || ev.OutputTokens != 0 {
					t.Fatalf("HTTP failure lost or usage fabricated: %+v", ev)
				}
			}
		})
	}
}

func TestRegressF7OpenRouter(t *testing.T) {
	for _, tc := range []struct {
		preset, reason string
		status, hits   int
	}{
		{"openrouter_402_inflight", "rate_limited", 200, 1},
		{"openrouter_403_budget", "quota_exceeded_api", 200, 1},
		{"openrouter_401", "invalid_token", 200, 1},
		{"openrouter_403_key_limit", "", 403, 1},
		{"openrouter_403_moderation", "", 403, 1},
		{"openrouter_402_weight", "", 402, 1},
		{"openrouter_429", "rate_limited", 200, 2},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			k0, k1, alias := uniq("sk-f7a"), uniq("sk-f7b"), uniq("e2e-f7")
			tg := keyedTarget(t, w.urlA, "/openrouter/api/v1", alias, []string{k0, k1}, map[string]any{"usage_adapter": "openrouter"})
			_, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(k0, &fakellm.Scenario{Preset: tc.preset})
			defer w.fakeA.SetKeyScenario(k0, nil)
			from := w.fakeA.LastSeq()
			resp, wire := chat(t, key, alias, "hi")
			if resp.StatusCode != tc.status {
				t.Fatalf("HTTP %d %s", resp.StatusCode, wire)
			}
			if n := hits(w.fakeA, k0, from); n != tc.hits {
				t.Fatalf("hits=%d want=%d", n, tc.hits)
			}
			st := keyStatus(t, tg, 0)
			if st.Exhausted != (tc.reason != "") || st.ExhaustReason != tc.reason {
				t.Fatalf("state=%+v", st)
			}
			if tc.reason != "" && hits(w.fakeA, k1, from) != 1 {
				t.Fatal("healthy key not selected")
			}
		})
	}
}
