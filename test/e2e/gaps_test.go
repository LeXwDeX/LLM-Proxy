//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// Also used by the formal F5 account-level key regression.
func keyDisabledLongTerm(t *testing.T, prefix, adapter, upstream, preset string) error {
	t.Helper()
	k1, k2 := uniq("sk-gapx1"), uniq("sk-gapx2")
	alias := uniq("e2e-gap-key")
	spec := map[string]any{"endpoint_type": "openai", "endpoint": w.urlA + prefix, "api_key": k1, "api_keys": []string{k2},
		"model_mappings": []any{map[string]any{"upstream": upstream, "fallback": alias}}}
	if adapter != "" {
		spec["usage_adapter"] = adapter
	}
	tg := target(t, spec)
	_, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(k1, &fakellm.Scenario{Preset: preset})
	defer w.fakeA.SetKeyScenario(k1, nil)
	from := w.fakeA.LastSeq()
	for i := 0; i < 4 && hits(w.fakeA, k1, from) == 0; i++ {
		chat(t, key, alias, "hi")
	}
	st := keyStatus(t, tg, 0)
	if !st.Exhausted || time.Until(st.CooldownEnd) < time.Hour && !st.CooldownEnd.IsZero() {
		return fmt.Errorf("key 0 exhausted=%v reason=%q cooldown_end in %s", st.Exhausted, st.ExhaustReason, time.Until(st.CooldownEnd).Round(time.Second))
	}
	return nil
}

func TestF10DefaultStreamUsagePassthrough(t *testing.T) {
	alias := uniq("e2e-gap-iu")
	tg := target(t, map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "qwen", "endpoint": w.urlA + "/qwen",
		"openai_prefix": "/compatible-mode", "anthropic_prefix": "/apps/anthropic", "api_key": uniq("sk-gap"),
		"model_mappings": []any{map[string]any{"upstream": "qwen3.8-max", "fallback": alias}}})
	name, key := client(t, []string{tg}, nil)
	call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	ev := usageFor(t, name, 1)[0]
	if ev.UsageStatus != "incomplete" || ev.UsageDiagnostic != "missing_usage" || ev.InputTokens != 0 || ev.OutputTokens != 0 {
		t.Fatalf("default must not inject or estimate usage: %+v", ev)
	}
}
