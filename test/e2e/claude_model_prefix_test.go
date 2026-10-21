//go:build e2e

package e2e

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Issue #36: a ModelPrefix target sends "Vendor/<upstream>" and the fake
// upstream (iteration_model_prefix, a synthetic non-official assumption)
// echoes that spelling in usage.iterations. Rows must carry the unprefixed
// upstream names and consume the same model groups that admission checked.
// Counterexamples: a target without ModelPrefix, or an echo whose prefix is not
// the target's, keeps the unverifiable plan conservative.
func TestClaudeIterationModelPrefixBilling(t *testing.T) {
	exec := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: .5}
	adv := usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheReadPer1MToken: 1}
	fb := usage.CostRates{InputPer1MTokens: 30, OutputPer1MTokens: 40, CacheReadPer1MToken: 2}
	type row struct {
		model, typ string
		in, out    int64
	}
	for _, tc := range []struct {
		name, targetPrefix, echoPrefix, mode string
		root                                 row
		rootCached                           int64
		diagnostic                           string
		child                                *row
	}{
		{name: "advisor", targetPrefix: "Vendor/", echoPrefix: "Vendor/", mode: "advisor",
			root: row{"unfamiliar-upstream", "", 70, 20}, rootCached: 30, child: &row{"fake-advisor", "advisor_message", 7, 3}},
		{name: "compaction", targetPrefix: "Vendor/", echoPrefix: "Vendor/", mode: "compaction",
			root: row{"unfamiliar-upstream", "", 70, 20}, rootCached: 30, child: &row{"unfamiliar-upstream", "compaction", 11, 2}},
		{name: "compaction_on_demand", targetPrefix: "Vendor/", echoPrefix: "Vendor/", mode: "compaction_on_demand",
			root: row{"unfamiliar-upstream", "", 0, 0}, child: &row{"unfamiliar-upstream", "compaction", 11, 2}},
		{name: "fallback", targetPrefix: "Vendor/", echoPrefix: "Vendor/", mode: "fallback_output",
			root: row{"fake-fallback", "fallback_message", 70, 20}, rootCached: 30, child: &row{"unfamiliar-upstream", "message", 13, 4}},
		{name: "no_prefix_target", targetPrefix: "", echoPrefix: "Vendor/", mode: "advisor",
			root: row{"unfamiliar-upstream", "", 70, 20}, rootCached: 30, diagnostic: "multi_model_iterations"},
		{name: "other_prefix_echo", targetPrefix: "Vendor/", echoPrefix: "Other/", mode: "advisor",
			root: row{"unfamiliar-upstream", "", 70, 20}, rootCached: 30, diagnostic: "multi_model_iterations"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				alias := uniq("e2e-prefix-iteration")
				spec := map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-fake-prefix-iteration"),
					"model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias},
						map[string]any{"upstream": "fake-advisor", "fallback": uniq("e2e-prefix-advisor")}, map[string]any{"upstream": "fake-fallback", "fallback": uniq("e2e-prefix-fallback")}}}
				if tc.targetPrefix != "" {
					spec["model_prefix"] = tc.targetPrefix
				}
				tg := target(t, spec)
				models := []string{"Unfamiliar-Upstream", "fake-advisor", "fake-fallback"}
				rates := []usage.CostRates{exec, adv, fb}
				groups := make([]string, len(models))
				for i, model := range models {
					iterationPrice(t, tg, model, rates[i])
					group := uniq("e2e-prefix-group")
					if code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "cost_coefficient": 1, "bindings": []any{map[string]any{"target": tg, "model": model}}}); code != 201 {
						t.Fatalf("model group HTTP %d: %s", code, raw)
					}
					t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
					groups[i] = group
				}
				name, key := client(t, []string{tg}, nil)
				quotaGroups := []any{}
				for _, group := range groups {
					quotaGroups = append(quotaGroups, map[string]any{"name": group, "coefficient": 1})
				}
				if code, raw := w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "e2e-level", "quota_groups": quotaGroups}); code != 200 {
					t.Fatalf("plan HTTP %d: %s", code, raw)
				}
				directive := "[[fake iterations=" + tc.mode + " iteration_model_prefix=" + tc.echoPrefix + " pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]"
				body := expand(messagesBody(directive, stream), alias).(map[string]any)
				// Explicit supplementary identities are what admission checks.
				body["tools"] = []any{map[string]any{"type": "advisor_20260301", "name": "advisor", "model": "fake-advisor"}}
				body["fallbacks"] = []any{map[string]any{"model": "fake-fallback"}}
				seq := w.fakeA.LastSeq()
				response, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
				}
				records := w.fakeA.RequestsSince(seq)
				if len(records) != 1 || records[0].Model != tc.targetPrefix+"Unfamiliar-Upstream" {
					t.Fatalf("wire model: %+v", records)
				}
				wantRows := 1
				if tc.child != nil {
					wantRows = 2
				}
				rows := compressionEvents(t, name, wantRows)
				if len(rows) != wantRows {
					t.Fatalf("records=%d want %d: %+v", len(rows), wantRows, rows)
				}
				var root, child *usage.Event
				for i := range rows {
					if rows[i].Supplementary {
						child = &rows[i]
					} else {
						root = &rows[i]
					}
				}
				if root == nil || root.Model != tc.root.model || root.IterationType != tc.root.typ || root.InputTokens != tc.root.in || root.OutputTokens != tc.root.out || root.CachedTokens != tc.rootCached || root.UsageDiagnostic != tc.diagnostic || root.CostIncomplete != (tc.diagnostic != "") {
					t.Fatalf("root=%+v", root)
				}
				used := make([]float64, len(models))
				add := func(model string, cost float64) {
					for i, m := range models {
						if strings.EqualFold(m, model) {
							used[i] += cost
							return
						}
					}
					t.Fatalf("model %q has no group", model)
				}
				rootRate := exec
				if tc.root.model == "fake-fallback" {
					rootRate = fb
				}
				add(tc.root.model, rootRate.CostFor(tc.root.in, tc.root.out, tc.rootCached, 0))
				if tc.child != nil {
					if child == nil || child.ParentRecordID != root.RecordID || child.Model != tc.child.model || child.IterationType != tc.child.typ || child.InputTokens != tc.child.in || child.OutputTokens != tc.child.out || child.CostIncomplete {
						t.Fatalf("child=%+v root=%+v", child, root)
					}
					childRate := exec
					if tc.child.model == "fake-advisor" {
						childRate = adv
					}
					add(tc.child.model, childRate.CostFor(tc.child.in, tc.child.out, 0, 0))
				}
				code, raw := w.adminDo("GET", "/admin/data/quota/"+name, nil)
				if code != 200 {
					t.Fatalf("quota HTTP %d: %s", code, raw)
				}
				state := decode(t, raw)
				for i, group := range groups {
					got, _ := dig(state, "groups/"+group+"/quotas/daily/used").(float64)
					if math.Abs(got-used[i]) > 1e-12 {
						t.Fatalf("group for %s used=%g want=%g: %s", models[i], got, used[i], raw)
					}
				}
			})
		}
	}
}
