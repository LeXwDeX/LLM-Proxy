//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func iterationPrice(t *testing.T, targetName, model string, rates usage.CostRates) {
	t.Helper()
	code, raw := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{
		"target": targetName, "model": model, "mode": "flat", "base_rates": rates,
	})
	if code != 201 {
		t.Fatalf("price HTTP %d: %s", code, raw)
	}
	var result struct {
		Template struct {
			ID string `json:"id"`
		} `json:"template"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Template.ID == "" {
		t.Fatalf("price ID: %s", raw)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+result.Template.ID, nil) })
}

// The real proxy process must persist each request atomically, expose each
// model's counters, price those counters once, and still count one request.
// JSON and final SSE usage have the same externally visible billing result.
func TestClaudeIterationBilling(t *testing.T) {
	executor := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: .5}
	advisor := usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheReadPer1MToken: 1}
	fallback := usage.CostRates{InputPer1MTokens: 30, OutputPer1MTokens: 40, CacheReadPer1MToken: 2}
	for _, tc := range []struct {
		mode                              string
		rootModel                         string
		rootInput, rootOutput, rootCached int64
		rootFree                          bool
		diagnostic                        string
		childModel, childType             string
		childInput, childOutput           int64
		childFree                         bool
	}{
		{mode: "message", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30},
		{mode: "advisor", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30, childModel: "fake-advisor", childType: "advisor_message", childInput: 7, childOutput: 3},
		{mode: "compaction", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30, childModel: "unfamiliar-upstream", childType: "compaction", childInput: 11, childOutput: 2},
		{mode: "compaction_on_demand", rootModel: "unfamiliar-upstream", childModel: "unfamiliar-upstream", childType: "compaction", childInput: 11, childOutput: 2},
		{mode: "fallback_output", rootModel: "fake-fallback", rootInput: 70, rootOutput: 20, rootCached: 30, childModel: "unfamiliar-upstream", childType: "message", childInput: 13, childOutput: 4},
		{mode: "fallback_zero_unknown", rootModel: "fake-fallback", rootInput: 70, rootOutput: 20, rootCached: 30, diagnostic: "unknown_fallback_refusal_billing", childModel: "unfamiliar-upstream", childType: "message", childInput: 13, childFree: true},
		{mode: "fallback_sticky", rootModel: "fake-fallback", rootInput: 70, rootOutput: 20, rootCached: 30},
		{mode: "fallback_refusal_free", rootModel: "fake-fallback", rootInput: 70, rootCached: 30, rootFree: true},
		{mode: "fallback_refusal_paid", rootModel: "fake-fallback", rootInput: 70, rootCached: 30},
		{mode: "fallback_refusal_null", rootModel: "fake-fallback", rootInput: 70, rootCached: 30, rootFree: true},
		{mode: "fallback_refusal_missing_category", rootModel: "fake-fallback", rootInput: 70, rootCached: 30, rootFree: true, diagnostic: "unknown_fallback_refusal_billing"},
		{mode: "cache_missing", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, diagnostic: "unknown_usage_iterations"},
		{mode: "cache_mismatch", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30, diagnostic: "iterations_usage_mismatch"},
		{mode: "iterations_unknown", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30, diagnostic: "unknown_usage_iterations"},
		{mode: "iterations_invalid", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30, diagnostic: "unknown_usage_iterations"},
		{mode: "iterations_mismatch", rootModel: "unfamiliar-upstream", rootInput: 70, rootOutput: 20, rootCached: 30, diagnostic: "iterations_usage_mismatch"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.mode, stream), func(t *testing.T) {
				alias := uniq("e2e-iteration")
				tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-fake-iteration"), "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
				name, key := client(t, []string{tg}, nil)
				iterationPrice(t, tg, "Unfamiliar-Upstream", executor)
				iterationPrice(t, tg, "fake-advisor", advisor)
				iterationPrice(t, tg, "fake-fallback", fallback)
				directive := "[[fake iterations=" + tc.mode + " pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]"
				response, raw := call(t, key, "POST", "/v1/messages", expand(messagesBody(directive, stream), alias), nil)
				if response.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
				}
				wantRows := 1
				if tc.childModel != "" {
					wantRows++
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
				if root == nil || root.RecordID == "" || root.RequestID == "" || root.Model != tc.rootModel || root.InputTokens != tc.rootInput || root.OutputTokens != tc.rootOutput || root.CachedTokens != tc.rootCached || root.NonBillable != tc.rootFree || root.CostIncomplete != (tc.diagnostic != "") || root.UsageDiagnostic != tc.diagnostic || root.UsageStatus != usage.UsageStatusComplete || root.UsageAdapter != "standard_anthropic" {
					t.Fatalf("root=%+v", root)
				}
				if len(root.Components) != wantRows-1 {
					t.Fatalf("bundle lost component evidence: %+v", root)
				}
				if wantRows == 2 {
					if child == nil || child.RecordID == "" || child.RecordID == root.RecordID || child.ParentRecordID != root.RecordID || child.RequestID != root.RequestID || child.Target != tg || child.ClientName != name || !child.Timestamp.Equal(root.Timestamp) || child.Model != tc.childModel || child.IterationType != tc.childType || child.InputTokens != tc.childInput || child.OutputTokens != tc.childOutput || child.CachedTokens != 0 || child.NonBillable != tc.childFree {
						t.Fatalf("child=%+v parent=%+v", child, root)
					}
					if child.PricingContext == nil || child.PricingContext.TotalInputTokens != tc.childInput || !child.PricingContext.TotalInputValid {
						t.Fatalf("child inherited executor context: %+v", child)
					}
					if child.CostIncomplete != tc.childFree || (tc.childFree && child.UsageDiagnostic != tc.diagnostic) {
						t.Fatalf("child diagnostic: %+v", child)
					}
				}
				rate := executor
				if tc.rootModel == "fake-fallback" {
					rate = fallback
				}
				wantCost := rate.CostFor(tc.rootInput, tc.rootOutput, tc.rootCached, 0)
				if tc.rootFree {
					wantCost = 0
				}
				if child != nil && !tc.childFree {
					rate = executor
					if tc.childModel == "fake-advisor" {
						rate = advisor
					}
					wantCost += rate.CostFor(tc.childInput, tc.childOutput, 0, 0)
				}
				code, raw := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
				var result usage.AggregateResult
				if code != 200 {
					t.Fatalf("aggregate HTTP %d: %s", code, raw)
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				totals := result.Totals
				if totals.Requests != 1 || totals.InputTokens != tc.rootInput+tc.childInput || totals.OutputTokens != tc.rootOutput+tc.childOutput || totals.CachedTokens != tc.rootCached || math.Abs(totals.EstimatedCost-wantCost) > 1e-12 {
					t.Fatalf("aggregate=%+v want requests=1 cost=%.12f", totals, wantCost)
				}
				if (totals.IncompleteCostCount > 0) != (tc.diagnostic != "") {
					t.Fatalf("incomplete aggregate=%+v", totals)
				}
			})
		}
	}
}

func TestClaudeAdvisorQuotaUsesEachModelGroupOnce(t *testing.T) {
	for _, coefficient := range []float64{.5, 0} {
		t.Run(fmt.Sprintf("advisor_coefficient=%g", coefficient), func(t *testing.T) {
			alias := uniq("e2e-iteration-quota")
			tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-fake-iteration-quota"), "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}, map[string]any{"upstream": "fake-advisor", "fallback": uniq("e2e-advisor")}}})
			executorRate := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: .5}
			advisorRate := usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheReadPer1MToken: 1}
			iterationPrice(t, tg, "Unfamiliar-Upstream", executorRate)
			iterationPrice(t, tg, "fake-advisor", advisorRate)
			groups := []string{uniq("e2e-executor-group"), uniq("e2e-advisor-group")}
			for i, model := range []string{"Unfamiliar-Upstream", "fake-advisor"} {
				factor := 1.0
				if i == 1 {
					factor = coefficient
				}
				group := groups[i]
				code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "cost_coefficient": factor, "bindings": []any{map[string]any{"target": tg, "model": model}}})
				if code != 201 {
					t.Fatalf("model group HTTP %d: %s", code, raw)
				}
				t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
			}
			name, key := client(t, []string{tg}, nil)
			code, raw := w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "e2e-level", "quota_groups": []any{map[string]any{"name": groups[0], "coefficient": 2}, map[string]any{"name": groups[1], "coefficient": 3}}})
			if code != 200 {
				t.Fatalf("plan HTTP %d: %s", code, raw)
			}
			for _, stream := range []bool{false, true} {
				body := expand(messagesBody("[[fake iterations=advisor pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]", stream), alias)
				response, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
				}
			}
			usageFor(t, name, 4)
			code, raw = w.adminDo("GET", "/admin/data/quota/"+name, nil)
			if code != 200 {
				t.Fatalf("quota HTTP %d: %s", code, raw)
			}
			state := decode(t, raw)
			expected := []float64{2 * executorRate.CostFor(70, 20, 30, 0), 2 * advisorRate.CostFor(7, 3, 0, 0) * coefficient}
			for i, group := range groups {
				for _, period := range []string{"daily", "weekly", "monthly"} {
					got, ok := dig(state, "groups/"+group+"/quotas/"+period+"/used").(float64)
					if !ok || math.Abs(got-expected[i]) > 1e-12 {
						t.Fatalf("%s %s used=%g want=%g", group, period, got, expected[i])
					}
				}
			}
		})
	}
}

func TestClaudeIterationMissingPriceRemainsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		mode, unpricedModel string
		rows                int
	}{
		{"advisor", "fake-advisor", 2}, {"fallback_sticky", "fake-fallback", 1},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.mode, stream), func(t *testing.T) {
				alias := uniq("e2e-iteration-no-price")
				tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-fake-iteration-no-price"), "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
				name, key := client(t, []string{tg}, nil)
				rates := compressionPrice(t, tg, "unfamiliar-upstream")
				body := expand(messagesBody("[[fake iterations="+tc.mode+" pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]", stream), alias)
				response, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
				}
				rows := compressionEvents(t, name, tc.rows)
				found := false
				for _, row := range rows {
					if row.Model == tc.unpricedModel {
						found = true
						if !row.CostIncomplete || row.UsageDiagnostic != "missing_iteration_price" {
							t.Fatalf("missing model price treated as complete: %+v", row)
						}
					}
				}
				if !found {
					t.Fatalf("unpriced iteration not persisted: %+v", rows)
				}
				code, raw := w.adminDo("GET", "/admin/data/usage/aggregate?client_name="+url.QueryEscape(name), nil)
				var result usage.AggregateResult
				if code != 200 {
					t.Fatalf("aggregate HTTP %d: %s", code, raw)
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				wantCost := 0.0
				if tc.mode == "advisor" {
					wantCost = rates.CostFor(70, 20, 30, 0)
				}
				if result.Totals.Requests != 1 || result.Totals.IncompleteCostCount != 1 || math.Abs(result.Totals.EstimatedCost-wantCost) > 1e-12 {
					t.Fatalf("incomplete aggregate=%+v", result.Totals)
				}
			})
		}
	}
}

func TestClaudeFallbackChainPricesEveryAttempt(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			mode := "fallback_chain"
			if unknown {
				mode = "fallback_chain_unknown"
			}
			t.Run(fmt.Sprintf("%s/stream=%v", mode, stream), func(t *testing.T) {
				alias := uniq("e2e-fallback-chain")
				tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-fake-fallback-chain"), "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
				name, key := client(t, []string{tg}, nil)
				rates := map[string]usage.CostRates{
					"unfamiliar-upstream": {InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: .5},
					"fake-middle":         {InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheReadPer1MToken: 1},
					"fake-fallback":       {InputPer1MTokens: 30, OutputPer1MTokens: 40, CacheReadPer1MToken: 2},
				}
				for model, price := range rates {
					iterationPrice(t, tg, model, price)
				}
				body := expand(messagesBody("[[fake iterations="+mode+" pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]", stream), alias)
				response, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
				}
				rows := compressionEvents(t, name, 3)
				if len(rows) != 3 {
					t.Fatalf("records=%d want3: %+v", len(rows), rows)
				}
				indexed := make(map[string]usage.Event)
				for _, row := range rows {
					indexed[row.Model] = row
				}
				root, first, middle := indexed["fake-fallback"], indexed["unfamiliar-upstream"], indexed["fake-middle"]
				firstOutput := int64(4)
				if unknown {
					firstOutput = 0
				}
				if root.RecordID == "" || root.RequestID == "" || root.Supplementary || root.IterationType != "fallback_message" || root.InputTokens != 70 || root.OutputTokens != 20 || root.CachedTokens != 30 || root.CostIncomplete != unknown {
					t.Fatalf("terminal model attribution: %+v", root)
				}
				if !first.Supplementary || first.InputTokens != 13 || first.OutputTokens != firstOutput || first.NonBillable != unknown || first.CostIncomplete != unknown || first.ParentRecordID != root.RecordID || first.IterationType != "message" {
					t.Fatalf("first declined attempt: %+v", first)
				}
				if !middle.Supplementary || middle.InputTokens != 5 || middle.OutputTokens != 2 || middle.NonBillable || middle.CostIncomplete || middle.ParentRecordID != root.RecordID || middle.IterationType != "message" {
					t.Fatalf("middle declined attempt: %+v", middle)
				}
				if first.RecordID == middle.RecordID || first.RequestID != root.RequestID || middle.RequestID != root.RequestID {
					t.Fatalf("chain identities: %+v", rows)
				}
				wantCost := rates["fake-fallback"].CostFor(70, 20, 30, 0) + rates["fake-middle"].CostFor(5, 2, 0, 0)
				if !unknown {
					wantCost += rates["unfamiliar-upstream"].CostFor(13, 4, 0, 0)
				}
				code, raw := w.adminDo("GET", "/admin/data/usage/aggregate?client_name="+url.QueryEscape(name), nil)
				var result usage.AggregateResult
				if code != 200 {
					t.Fatalf("aggregate HTTP %d: %s", code, raw)
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if result.Totals.Requests != 1 || result.Totals.InputTokens != 88 || result.Totals.OutputTokens != 22+firstOutput || math.Abs(result.Totals.EstimatedCost-wantCost) > 1e-12 || (result.Totals.IncompleteCostCount > 0) != unknown {
					t.Fatalf("fallback chain aggregate=%+v", result.Totals)
				}
			})
		}
	}
}
