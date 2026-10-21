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

// An invalid final between two valid finals must not leak its cache/model
// evidence into the next valid final's persisted bundle.
func TestClaudeInvalidFinalTailRecoveryE2E(t *testing.T) {
	_, up, _, requests := compressionUpstream(t)
	alias := uniq("e2e-final-recovery")
	tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": up.URL + "/anthropic", "api_key": uniq("sk-fake-recovery"), "model_mappings": []any{
		map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias},
	}})
	name, key := client(t, []string{tg}, nil)
	iterationPrice(t, tg, "Unfamiliar-Upstream", usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 5})
	directive := "[[fake iterations=advisor_tail_invalid_final_retry pricing=standard usage.in=7 usage.out=2 usage.cached=0]]"
	response, raw := call(t, key, "POST", "/v1/messages", expand(messagesBody(directive, true), alias), nil)
	if response.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
	}
	rows := compressionEvents(t, name, 1)
	if len(rows) != 1 || requests.Load() != 1 {
		t.Fatalf("records=%d requests=%d: %+v", len(rows), requests.Load(), rows)
	}
	root := rows[0]
	if root.Model != "unfamiliar-upstream" || root.InputTokens != 7 || root.OutputTokens != 4 || root.CachedTokens != 0 ||
		root.UsageStatus != usage.UsageStatusComplete || root.CostIncomplete || len(root.Components) != 0 {
		t.Fatalf("rejected final leaked: %+v", root)
	}
	code, aggregate := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
	var result usage.AggregateResult
	if code != 200 {
		t.Fatalf("aggregate HTTP %d: %s", code, aggregate)
	}
	if err := json.Unmarshal(aggregate, &result); err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 1 || math.Abs(result.Totals.EstimatedCost-.00003) > 1e-12 || result.Totals.IncompleteCostCount != 0 {
		t.Fatalf("aggregate=%+v cost=%g want .00003", result.Totals, result.Totals.EstimatedCost)
	}
}

// The tail fixtures are synthetic counterexamples, not official Claude
// sequences. Each case checks the persisted bundle and its resolved cost.
func TestClaudeConfirmedFinalSnapshotE2E(t *testing.T) {
	executor := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: .5}
	advisor := usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 20}
	fallback := usage.CostRates{InputPer1MTokens: 30, OutputPer1MTokens: 40, CacheReadPer1MToken: 2}
	for _, tc := range []struct {
		mode           string
		rows           int
		rootModel      string
		rootIn         int64
		rootOut        int64
		rootCached     int64
		rootFree       bool
		childModel     string
		childIn        int64
		childOut       int64
		diagnostic     string
		failed         bool
		costIncomplete bool
		incompleteRows int64
		expectedCost   float64
	}{
		{"advisor_tail_nonfinal", 2, "unfamiliar-upstream", 70, 20, 30, false, "fake-advisor", 7, 3, "interrupted_after_final_usage", false, false, 0, .000365},
		{"advisor_tail_empty_stop", 2, "unfamiliar-upstream", 70, 20, 30, false, "fake-advisor", 7, 3, "", false, false, 0, .000365},
		{"advisor_tail_error_empty_stop", 2, "unfamiliar-upstream", 70, 20, 30, false, "fake-advisor", 7, 3, "interrupted_after_final_usage", true, false, 0, .000365},
		{"advisor_refusal_free_tail_nonfinal", 2, "unfamiliar-upstream", 70, 0, 30, true, "fake-advisor", 7, 3, "interrupted_after_final_usage", false, false, 0, .00013},
		{"replace_empty_iterations", 1, "unfamiliar-upstream", 70, 20, 30, false, "", 0, 0, "", false, false, 0, .000235},
		{"fallback_output_tail_nonfinal", 2, "fake-fallback", 70, 20, 30, false, "unfamiliar-upstream", 13, 4, "interrupted_after_final_usage", false, false, 0, .003002},
		{"advisor_tail_invalid_iteration_retry", 2, "unfamiliar-upstream", 70, 20, 30, false, "fake-advisor", 7, 3, "", false, false, 0, .000365},
		{"advisor_tail_omitted_iterations_retry", 2, "unfamiliar-upstream", 70, 20, 30, false, "fake-advisor", 7, 3, "", false, false, 0, .000365},
		{"advisor_tail_message_mismatch_retry", 2, "unfamiliar-upstream", 70, 20, 30, false, "fake-advisor", 7, 3, "", false, false, 0, .000365},
		{"advisor_tail_error_retry_final", 1, "unfamiliar-upstream", 50, 9, 0, false, "", 0, 0, "interrupted_after_final_usage", true, false, 0, .000136},
		{"advisor_tail_retry_final", 1, "unfamiliar-upstream", 50, 9, 0, false, "", 0, 0, "", false, false, 0, .000136},
		{"advisor_tail_cost_incomplete_retry", 1, "unfamiliar-upstream", 50, 9, 0, false, "", 0, 0, "server_tool_charges", false, true, 1, .000136},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			_, up, _, requests := compressionUpstream(t)
			alias := uniq("e2e-final")
			tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": up.URL + "/anthropic", "api_key": uniq("sk-fake-final"), "model_mappings": []any{
				map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias},
				map[string]any{"upstream": "fake-advisor", "fallback": uniq("e2e-advisor")},
				map[string]any{"upstream": "fake-fallback", "fallback": uniq("e2e-fallback")},
			}})
			name, key := client(t, []string{tg}, nil)
			iterationPrice(t, tg, "Unfamiliar-Upstream", executor)
			iterationPrice(t, tg, "fake-advisor", advisor)
			iterationPrice(t, tg, "fake-fallback", fallback)
			directive := "[[fake iterations=" + tc.mode + " pricing=standard usage.in=100 usage.out=20 usage.cached=30 usage.cw=0 usage.cw1h=0 usage.reasoning=0]]"
			response, raw := call(t, key, "POST", "/v1/messages", expand(messagesBody(directive, true), alias), nil)
			if response.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
			}
			rows := compressionEvents(t, name, tc.rows)
			if len(rows) != tc.rows || requests.Load() != 1 {
				t.Fatalf("records=%d requests=%d want %d/1: %+v", len(rows), requests.Load(), tc.rows, rows)
			}
			var root, child *usage.Event
			for i := range rows {
				if rows[i].Supplementary {
					child = &rows[i]
				} else {
					root = &rows[i]
				}
			}
			if root == nil || root.Model != tc.rootModel || root.InputTokens != tc.rootIn || root.OutputTokens != tc.rootOut || root.CachedTokens != tc.rootCached || root.NonBillable != tc.rootFree || root.UsageStatus != usage.UsageStatusComplete || root.CostIncomplete != tc.costIncomplete || (root.ResponseStatus == "failed") != tc.failed {
				t.Fatalf("root=%+v failed=%v", root, tc.failed)
			}
			if tc.diagnostic == "" {
				if root.UsageDiagnostic != "" {
					t.Fatalf("unexpected diagnostic=%q", root.UsageDiagnostic)
				}
			} else if !strings.Contains(root.UsageDiagnostic, tc.diagnostic) {
				t.Fatalf("root diagnostic=%q want %s", root.UsageDiagnostic, tc.diagnostic)
			}
			if tc.rows == 1 {
				if child != nil || len(root.Components) != 0 {
					t.Fatalf("explicit empty iterations kept children: root=%+v child=%+v", root, child)
				}
			} else if child == nil || child.Model != tc.childModel || child.InputTokens != tc.childIn || child.OutputTokens != tc.childOut || child.NonBillable || len(root.Components) != 1 {
				t.Fatalf("child=%+v root=%+v", child, root)
			}
			code, aggregate := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
			var result usage.AggregateResult
			if code != 200 {
				t.Fatalf("aggregate HTTP %d: %s", code, aggregate)
			}
			if err := json.Unmarshal(aggregate, &result); err != nil {
				t.Fatal(err)
			}
			if result.Totals.Requests != 1 || math.Abs(result.Totals.EstimatedCost-tc.expectedCost) > 1e-12 || result.Totals.IncompleteCostCount != tc.incompleteRows {
				t.Fatalf("aggregate=%+v cost=%g want=%g incomplete=%d", result.Totals, result.Totals.EstimatedCost, tc.expectedCost, tc.incompleteRows)
			}
			if got := fmt.Sprintf("%.9f", result.Totals.EstimatedCost); got != fmt.Sprintf("%.9f", tc.expectedCost) {
				t.Fatalf("cost=%s want %.9f", got, tc.expectedCost)
			}
		})
	}
}
