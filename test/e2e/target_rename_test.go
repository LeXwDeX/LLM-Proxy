//go:build e2e

package e2e

import (
	"math"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Issue #195: renaming a target through the supported admin API keeps the
// pre-rename history priced by the target template (not the global fallback),
// keeps new traffic in the same model group, and never makes accounting fail
// closed (no 503) because no usage or aggregation key is rewritten.
func TestTargetRenameKeepsHistoryPricingAndQuota(t *testing.T) {
	alias := uniq("e2e-rename-alias")
	model := uniq("RenameModel")
	tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA + "/openai/v1", "api_key": uniq("sk-fake-rename"),
		"model_mappings": []any{map[string]any{"upstream": model, "fallback": alias}}})
	renamed := uniq("e2e-renamed")
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/targets/"+renamed, nil) })
	for _, tmpl := range []map[string]any{
		{"targets": []string{tg}, "models": []string{model}, "mode": "flat", "base_rates": usage.CostRates{InputPer1MTokens: 2}},
		{"targets": []string{""}, "models": []string{model}, "mode": "flat", "base_rates": usage.CostRates{InputPer1MTokens: 50}},
	} {
		if code, raw := w.adminDo("POST", "/admin/data/pricing-templates", tmpl); code != 201 {
			t.Fatalf("pricing template HTTP %d: %s", code, raw)
		}
	}
	group := uniq("e2e-rename-group")
	if code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "daily_usd": 100, "cost_coefficient": 1,
		"bindings": []any{map[string]any{"target": tg, "model": model}}}); code != 201 {
		t.Fatalf("model group HTTP %d: %s", code, raw)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
	name, key := client(t, []string{tg}, nil)
	if code, raw := w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "e2e-level", "quota_groups": []any{map[string]any{"name": group}}}); code != 200 {
		t.Fatalf("plan HTTP %d: %s", code, raw)
	}
	send := func() {
		t.Helper()
		body := map[string]any{"model": alias, "messages": []any{map[string]any{"role": "user", "content": "[[fake usage.in=1000000 usage.out=0 usage.cached=0 usage.reasoning=0]]"}}}
		if resp, raw := call(t, key, "POST", "/v1/chat/completions", body, nil); resp.StatusCode != 200 {
			t.Fatalf("chat HTTP %d: %s", resp.StatusCode, raw)
		}
	}
	used := func() float64 {
		t.Helper()
		code, raw := w.adminDo("GET", "/admin/data/quota/"+name, nil)
		if code != 200 {
			t.Fatalf("quota HTTP %d: %s", code, raw)
		}
		got, ok := dig(decode(t, raw), "groups/"+group+"/quotas/daily/used").(float64)
		if !ok {
			t.Fatalf("quota state: %s", raw)
		}
		return got
	}

	send()
	usageFor(t, name, 1)
	if got := used(); math.Abs(got-2) > 1e-9 {
		t.Fatalf("pre-rename used=%g want 2", got)
	}

	code, raw := w.adminDo("POST", "/admin/data/targets/"+tg+"/rename", map[string]any{"new_name": renamed})
	if code != 200 {
		t.Fatalf("rename HTTP %d: %s", code, raw)
	}
	send()
	events := usageFor(t, name, 2)
	seen := map[string]bool{}
	for _, ev := range events {
		seen[ev.Target] = true
		if ev.CostIncomplete {
			t.Fatalf("incomplete cost after rename: %+v", ev)
		}
	}
	if !seen[tg] || !seen[renamed] {
		t.Fatalf("events must keep the write-time target names: %+v", events)
	}
	// 2 (history on the old name, target template) + 2 (new name, same template
	// and group); a global fallback would give 50 per request.
	if got := used(); math.Abs(got-4) > 1e-9 {
		t.Fatalf("post-rename used=%g want 4", got)
	}
	// Renaming back onto the retired name is refused: it already has history.
	if code, raw := w.adminDo("POST", "/admin/data/targets/"+renamed+"/rename", map[string]any{"new_name": tg}); code != 409 {
		t.Fatalf("rename onto retired name HTTP %d: %s", code, raw)
	}
}
