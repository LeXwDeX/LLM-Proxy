//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func routingPool(t *testing.T, strategy string) string {
	t.Helper()
	name := uniq("e2e-cost-pool")
	if code, body := w.adminDo("POST", "/admin/data/pools", map[string]any{"name": name, "routing_strategy": strategy}); code != 201 {
		t.Fatalf("create pool: %d %s", code, body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pools/"+name, nil) })
	return name
}

// priceTemplate installs a template and returns a function that rewrites it.
func priceTemplate(t *testing.T, spec map[string]any) func(map[string]any) {
	t.Helper()
	code, raw := w.adminDo("POST", "/admin/data/pricing-templates", spec)
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
	id := result.Template.ID
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+id, nil) })
	return func(next map[string]any) {
		t.Helper()
		if code, raw := w.adminDo("PUT", "/admin/data/pricing-templates/"+id, next); code != 200 {
			t.Fatalf("update price HTTP %d: %s", code, raw)
		}
	}
}

func flatPrice(target, model string, in, out float64) map[string]any {
	return map[string]any{"target": target, "model": model, "mode": "flat",
		"base_rates": usage.CostRates{InputPer1MTokens: in, OutputPer1MTokens: out}}
}

// currentPeakPrice is a schedule template whose peak window covers the
// current +08:00 hour (with an hour of margin each side) on today's weekday.
func currentPeakPrice(target, model string, peak, idle float64) map[string]any {
	cst := time.Now().In(time.FixedZone("CST", 8*3600))
	start, end := max(cst.Hour()-1, 0), min(cst.Hour()+2, 24)
	weekday := int(cst.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	return map[string]any{
		"target": target, "model": model, "mode": "schedule", "timezone": "+08:00",
		"start":      time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour).Format(time.RFC3339),
		"base_rates": usage.CostRates{InputPer1MTokens: idle, OutputPer1MTokens: idle},
		"periods": []any{map[string]any{
			"weekdays": []int{weekday}, "start": fmt.Sprintf("%02d:00", start), "end": fmt.Sprintf("%02d:00", end),
			"rates": usage.CostRates{InputPer1MTokens: peak, OutputPer1MTokens: peak},
		}},
	}
}

func poolMember(t *testing.T, poolName, upstream, model string) string {
	return target(t, map[string]any{"endpoint_type": "openai", "endpoint": upstream, "api_key": uniq("sk-fake-cost"),
		"pool": poolName, "model_mappings": []any{map[string]any{"upstream": model}}})
}

func costRoutingCall(t *testing.T, key, model string) {
	t.Helper()
	resp, body := call(t, key, "POST", "/v1/chat/completions",
		map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "cost routing"}}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
}

func clientCost(t *testing.T, clientName string) float64 {
	t.Helper()
	code, raw := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(clientName), nil)
	if code != 200 {
		t.Fatalf("aggregate HTTP %d: %s", code, raw)
	}
	var result usage.AggregateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Totals.IncompleteCostCount != 0 {
		t.Fatalf("incomplete cost: %+v", result.Totals)
	}
	return result.Totals.EstimatedCost
}

// A cost pool sends traffic to the member that is cheapest right now and,
// after a price change, moves the next request (including the sticky client)
// to the new cheapest member. Billing still uses the serving member's price.
func TestPoolCostRoutingFollowsPricesE2E(t *testing.T) {
	model := uniq("e2e-cost-model")
	poolName := routingPool(t, "cost")
	official := poolMember(t, poolName, w.urlA+"/v1", model)
	agg := poolMember(t, poolName, w.urlB+"/v1", model)
	clientName, key := client(t, nil, []string{poolName})

	priceTemplate(t, currentPeakPrice(official, model, 10, 1)) // peak right now
	setAgg := priceTemplate(t, flatPrice(agg, model, 5, 5))

	for i := 0; i < 3; i++ {
		costRoutingCall(t, key, model)
	}
	events := usageFor(t, clientName, 3)
	var tokens int64
	for _, ev := range events {
		if ev.Target != agg || ev.CostIncomplete {
			t.Fatalf("peak request served by %s (incomplete=%v), want %s", ev.Target, ev.CostIncomplete, agg)
		}
		tokens += ev.InputTokens + ev.OutputTokens
	}
	if got, want := clientCost(t, clientName), float64(tokens)*5/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %g, want %g at the serving member's price", got, want)
	}

	// The aggregator becomes the expensive member: the next request switches,
	// although the client already has IP affinity to it.
	setAgg(flatPrice(agg, model, 50, 50))
	costRoutingCall(t, key, model)
	events = usageFor(t, clientName, 4)
	switched := 0
	for _, ev := range events {
		if ev.Target == official {
			switched++
		}
	}
	if switched != 1 {
		t.Fatalf("after the price change %d requests went to %s, want exactly the next one", switched, official)
	}
}

// Counter-example: a load pool ignores prices and keeps the sticky member.
func TestPoolLoadRoutingIgnoresPricesE2E(t *testing.T) {
	model := uniq("e2e-load-model")
	poolName := routingPool(t, "load")
	a := poolMember(t, poolName, w.urlA+"/v1", model)
	b := poolMember(t, poolName, w.urlB+"/v1", model)
	clientName, key := client(t, nil, []string{poolName})
	setA := priceTemplate(t, flatPrice(a, model, 1, 1))
	setB := priceTemplate(t, flatPrice(b, model, 1, 1))

	costRoutingCall(t, key, model)
	first := usageFor(t, clientName, 1)[0].Target
	if first == a {
		setA(flatPrice(a, model, 100, 100))
	} else {
		setB(flatPrice(b, model, 100, 100))
	}
	costRoutingCall(t, key, model)
	for _, ev := range usageFor(t, clientName, 2) {
		if ev.Target != first {
			t.Fatalf("load pool moved to %s after a price change, want sticky %s", ev.Target, first)
		}
	}
}
