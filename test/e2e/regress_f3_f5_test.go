//go:build e2e

package e2e

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// TestRegressF3CreditExhaustedNoSameKeyRetry guards #159 F3: a 429 carrying an
// account-exhaustion code (credit_balance_exhausted) disables the key at once
// instead of first retrying it as a transient rate limit.
func TestRegressF3CreditExhaustedNoSameKeyRetry(t *testing.T) {
	k1, k2 := uniq("sk-f3a"), uniq("sk-f3b")
	alias := uniq("e2e-f3-cr")
	tg := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{k1, k2}, nil)
	_, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(k1, &fakellm.Scenario{Preset: "credit_exhausted"})
	defer w.fakeA.SetKeyScenario(k1, nil)
	from := w.fakeA.LastSeq()
	chat(t, key, alias, "hi")
	chat(t, key, alias, "hi")
	if n := hits(w.fakeA, k1, from); n != 1 {
		t.Fatalf("exhausted key used %d times, want 1", n)
	}
}

// TestRegressF5GLMAccountLevelCodesDisableKey guards #159 F5: GLM 1113
// (arrears) and 1309 (Coding Plan expired) disable the key long-term instead of
// the short unknown-code cooldown.
func TestRegressF5GLMAccountLevelCodesDisableKey(t *testing.T) {
	for _, preset := range []string{"glm_1113", "glm_1309"} {
		t.Run(preset, func(t *testing.T) {
			if err := keyDisabledLongTerm(t, "/glm/api/paas/v4", "glm", "glm-4.6", preset); err != nil {
				t.Fatal(err)
			}
		})
	}
}
