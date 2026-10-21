//go:build e2e

package e2e

import (
	"encoding/json"
	"math"
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// tierPrice installs a flat template whose fast condition replaces the base
// rates. Only a fast ActualServiceTier can select it.
func tierPrice(t *testing.T, targetName, model string, base, fast usage.CostRates) {
	t.Helper()
	code, raw := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{
		"target": targetName, "model": model, "mode": "flat", "base_rates": base,
		"conditions": []any{map[string]any{"id": "fast", "service_tier": "fast", "rates": fast}},
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

// The Chat usage chunk is the official final usage. The synthetic non-usage
// tail carries another model and service_tier before [DONE]; neither may
// change the persisted response model or the billed tier.
func TestChatFinalSnapshotTailE2E(t *testing.T) {
	_, up, _, requests := compressionUpstream(t)
	alias := uniq("e2e-chat-final")
	tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-fake-chat-final"), "model_mappings": []any{
		map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias},
	}})
	name, key := client(t, []string{tg}, nil)
	tierPrice(t, tg, "Unfamiliar-Upstream",
		usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4},
		usage.CostRates{InputPer1MTokens: 20, OutputPer1MTokens: 40})
	directive := "[[fake chat_tail=reprice_nonfinal pricing=standard usage.in=7 usage.out=2]]"
	response, raw := call(t, key, "POST", "/v1/chat/completions", expand(chatBody(directive, true), alias), map[string]string{"Accept-Encoding": "identity"})
	if response.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", response.StatusCode, raw)
	}
	rows := compressionEvents(t, name, 1)
	if len(rows) != 1 || requests.Load() != 1 {
		t.Fatalf("records=%d requests=%d: %+v", len(rows), requests.Load(), rows)
	}
	root := rows[0]
	if root.Model != "unfamiliar-upstream" || root.ResponseModel != "unfamiliar-upstream" || root.InputTokens != 7 || root.OutputTokens != 2 ||
		root.UsageStatus != usage.UsageStatusComplete || root.CostIncomplete {
		t.Fatalf("tail changed the final usage: %+v", root)
	}
	if c := root.PricingContext; c == nil || c.ResponseServiceTier.Raw != "default" || c.ActualServiceTier != "standard" {
		t.Fatalf("tail changed the pricing context: %+v", c)
	}
	code, aggregate := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
	var result usage.AggregateResult
	if code != 200 {
		t.Fatalf("aggregate HTTP %d: %s", code, aggregate)
	}
	if err := json.Unmarshal(aggregate, &result); err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 1 || math.Abs(result.Totals.EstimatedCost-.000022) > 1e-12 || result.Totals.IncompleteCostCount != 0 {
		t.Fatalf("aggregate=%+v cost=%g want .000022", result.Totals, result.Totals.EstimatedCost)
	}
}
