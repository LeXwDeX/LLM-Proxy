//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Each case reads persisted events, hourly cost and all quota periods. The
// stream-gap fixtures are synthetic counterexamples, not real-API evidence.
func TestClaudeRefusalAndStreamBilling(t *testing.T) {
	for _, tc := range []struct {
		mode, diagnostic string
		free             bool
		output           int64
		child            string
	}{
		{mode: "refusal_free", free: true},
		{mode: "refusal_general_harms", free: true},
		{mode: "refusal_null", free: true},
		{mode: "refusal_paid"},
		{mode: "refusal_frontier"},
		{mode: "refusal_reasoning"},
		{mode: "refusal_unknown", free: true, diagnostic: "unknown_refusal_billing"},
		{mode: "refusal_missing_category", free: true, diagnostic: "unknown_refusal_billing"},
		{mode: "refusal_missing_output"},
		{mode: "refusal_nonzero", output: 20},
		{mode: "refusal_nonrefusal", output: 20},
		{mode: "message_refusal_free", free: true},
		{mode: "advisor_refusal_free", free: true, child: "advisor"},
		{mode: "compaction_refusal_free", free: true, child: "compaction"},
		{mode: "fallback_refusal_unknown", free: true, diagnostic: "unknown_fallback_refusal_billing"},
		{mode: "fallback_zero_unknown", output: 20, child: "zero", diagnostic: "unknown_fallback_refusal_billing"},
		{mode: "fallback_sticky", output: 20},
		{mode: "fallback_output", output: 20, child: "message"},
		{mode: "fallback_missing_input", output: 20, child: "message"},
		{mode: "advisor_early_only", output: 20, child: "advisor"},
		{mode: "advisor_then_error", output: 20, child: "advisor"},
		{mode: "advisor_refusal_free_then_error", free: true, child: "advisor"},
		{mode: "advisor_intermediate_then_error", output: 20, child: "advisor"},
		{mode: "advisor_stop_then_error", output: 20, child: "advisor"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.mode, stream), func(t *testing.T) {
				alias := uniq("e2e-refusal")
				tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-fake-refusal"), "model_mappings": []any{
					map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias},
					map[string]any{"upstream": "fake-advisor", "fallback": uniq("e2e-advisor")},
					map[string]any{"upstream": "fake-fallback", "fallback": uniq("e2e-fallback")},
				}})
				rates := map[string]usage.CostRates{
					"unfamiliar-upstream": {InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: .5},
					"fake-advisor":        {InputPer1MTokens: 10, OutputPer1MTokens: 20},
					"fake-fallback":       {InputPer1MTokens: 30, OutputPer1MTokens: 40, CacheReadPer1MToken: 2},
				}
				bindings := []any{}
				for model, rate := range rates {
					iterationPrice(t, tg, model, rate)
					bindings = append(bindings, map[string]any{"target": tg, "model": model})
				}
				group := uniq("e2e-refusal-group")
				code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "cost_coefficient": 1, "bindings": bindings})
				if code != 201 {
					t.Fatalf("group HTTP %d: %s", code, raw)
				}
				t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
				name, key := client(t, []string{tg}, nil)
				code, raw = w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "e2e-level", "quota_groups": []any{map[string]any{"name": group, "coefficient": 1}}})
				if code != 200 {
					t.Fatalf("plan HTTP %d: %s", code, raw)
				}
				body := expand(messagesBody("[[fake iterations="+tc.mode+" pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]", stream), alias)
				response, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
				}
				rootModel, in, cached, childKind, diagnostic := "unfamiliar-upstream", int64(70), int64(30), tc.child, tc.diagnostic
				status := usage.UsageStatusComplete
				output := tc.output
				if tc.mode == "refusal_missing_output" {
					status = usage.UsageStatusIncomplete
					if stream {
						in, cached, diagnostic = 0, 0, "missing_final_usage"
					}
				}
				if strings.HasPrefix(tc.mode, "fallback_") {
					rootModel = "fake-fallback"
				}
				if stream && tc.mode == "advisor_early_only" {
					childKind, diagnostic = "", "missing_final_iterations"
				}
				if stream && tc.mode == "fallback_missing_input" {
					rootModel, in, cached, childKind, diagnostic, status = "fake-fallback", 0, 0, "", "missing_fallback_input_usage", usage.UsageStatusIncomplete
				}
				if stream && tc.mode == "advisor_intermediate_then_error" {
					in, cached, output, childKind, diagnostic, status = 0, 0, 0, "", "upstream_interrupted_no_usage", usage.UsageStatusIncomplete
				}
				childModel, childIn, childOut, childFree := "", int64(0), int64(0), false
				switch childKind {
				case "advisor":
					childModel, childIn, childOut = "fake-advisor", 7, 3
				case "compaction":
					childModel, childIn, childOut = "unfamiliar-upstream", 11, 2
				case "message":
					childModel, childIn, childOut = "unfamiliar-upstream", 13, 4
				case "zero":
					childModel, childIn, childFree = "unfamiliar-upstream", 13, true
				}
				wantRows := 1
				if childKind != "" {
					wantRows++
				}
				rows := compressionEvents(t, name, wantRows)
				if len(rows) != wantRows {
					t.Fatalf("records=%d want %d: %+v", len(rows), wantRows, rows)
				}
				var root *usage.Event
				for i := range rows {
					row := &rows[i]
					if !row.Supplementary {
						root = row
						continue
					}
					if row.Model != childModel || row.InputTokens != childIn || row.OutputTokens != childOut || row.NonBillable != childFree {
						t.Fatalf("child=%+v", row)
					}
					if stream && strings.HasSuffix(tc.mode, "_then_error") && (row.PricingContext == nil || row.PricingContext.TotalInputTokens != childIn) {
						t.Fatalf("confirmed child pricing context lost: %+v", row)
					}
				}
				incomplete := diagnostic != "" || status != usage.UsageStatusComplete
				if root == nil || root.Model != rootModel || root.InputTokens != in || root.OutputTokens != output || root.CachedTokens != cached || root.NonBillable != tc.free || root.UsageStatus != status || root.CostIncomplete != incomplete || (diagnostic != "" && !strings.Contains(root.UsageDiagnostic, diagnostic)) || len(root.Components) != wantRows-1 {
					t.Fatalf("root=%+v diagnostic=%s", root, diagnostic)
				}
				if stream && strings.HasSuffix(tc.mode, "_then_error") {
					if root.ResponseStatus != "failed" {
						t.Fatalf("error response became successful: %+v", root)
					}
					if status == usage.UsageStatusComplete && (root.PricingContext == nil || root.PricingContext.TotalInputTokens != in+cached || !strings.Contains(root.UsageDiagnostic, "interrupted_after_final_usage")) {
						t.Fatalf("final pricing context or late-error diagnostic lost: %+v", root)
					}
				}
				wantCost := rates[rootModel].CostFor(in, output, cached, 0)
				if tc.free {
					wantCost = 0
				}
				if childModel != "" && !childFree {
					wantCost += rates[childModel].CostFor(childIn, childOut, 0, 0)
				}
				code, raw = w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
				var agg usage.AggregateResult
				if code != 200 {
					t.Fatalf("aggregate HTTP %d: %s", code, raw)
				}
				if err := json.Unmarshal(raw, &agg); err != nil {
					t.Fatal(err)
				}
				if agg.Totals.Requests != 1 || agg.Totals.InputTokens != in+childIn || agg.Totals.OutputTokens != output+childOut || math.Abs(agg.Totals.EstimatedCost-wantCost) > 1e-12 || (agg.Totals.IncompleteCostCount > 0) != incomplete {
					t.Fatalf("aggregate=%+v cost=%g", agg.Totals, wantCost)
				}
				code, raw = w.adminDo("GET", "/admin/data/quota/"+name, nil)
				if code != 200 {
					t.Fatalf("quota HTTP %d: %s", code, raw)
				}
				state := decode(t, raw)
				for _, period := range []string{"daily", "weekly", "monthly"} {
					got, ok := dig(state, "groups/"+group+"/quotas/"+period+"/used").(float64)
					if !ok || math.Abs(got-wantCost) > 1e-12 {
						t.Fatalf("%s quota=%g want=%g", period, got, wantCost)
					}
				}
			})
		}
	}
}
