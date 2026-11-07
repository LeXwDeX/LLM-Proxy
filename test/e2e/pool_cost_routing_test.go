//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
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
	_, update := priceTemplateWithID(t, spec)
	return update
}

// priceTemplateWithID also returns the stored template ID, which usage events
// carry as pricing_rule_id.
func priceTemplateWithID(t *testing.T, spec map[string]any) (string, func(map[string]any)) {
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
	return id, func(next map[string]any) {
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

// poolMemberWithAlias maps one client-facing alias to a member-specific
// upstream model. Different members can spell the upstream completely
// differently while sharing the alias.
func poolMemberWithAlias(t *testing.T, poolName, endpoint, upstream, alias string) string {
	return target(t, map[string]any{"endpoint_type": "openai", "endpoint": endpoint, "api_key": uniq("sk-fake-cost"),
		"pool": poolName, "model_mappings": []any{map[string]any{"upstream": upstream, "fallback": alias}}})
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

// The client alias is the cost comparison unit even when each member forwards
// it under a completely different upstream spelling. Routing picks the cheaper
// member, billing records that member's upstream, and the request still costs
// exactly the serving member's price.
func TestPoolCostRoutingComparesByClientAliasE2E(t *testing.T) {
	alias := uniq("e2e-cost-alias")
	poolName := routingPool(t, "cost")
	// Mixed case proves the wire model keeps its configured spelling while the
	// usage identity is normalized.
	upstreamA := uniq("E2E-Upstream-A")
	upstreamB := uniq("E2E-Upstream-B")
	a := poolMemberWithAlias(t, poolName, w.urlA+"/v1", upstreamA, alias)
	b := poolMemberWithAlias(t, poolName, w.urlB+"/v1", upstreamB, alias)
	clientName, key := client(t, nil, []string{poolName})

	// A is at its scheduled peak price right now; B is flat half price.
	// Turning A into the cheaper member would require editing a template, which
	// also reprices history, so the test keeps prices fixed and exercises the
	// counter-example in the other direction.
	idA, _ := priceTemplateWithID(t, currentPeakPrice(a, upstreamA, 10, 1))
	idB, _ := priceTemplateWithID(t, flatPrice(b, upstreamB, 5, 5))
	ruleFor := map[string]string{a: idA, b: idB}
	upstreamFor := map[string]string{a: upstreamA, b: upstreamB}

	// 1. The alias compares both members and picks the cheaper B.
	costRoutingCall(t, key, alias)
	events := usageFor(t, clientName, 1)
	if events[0].Target != b || events[0].Model != strings.ToLower(upstreamB) {
		t.Fatalf("alias request served by %s/%s, want %s/%s", events[0].Target, events[0].Model, b, strings.ToLower(upstreamB))
	}
	if events[0].PricingRuleID != idB {
		t.Fatalf("pricing rule = %q, want serving member's template %q", events[0].PricingRuleID, idB)
	}
	if got := lastRecord(t, w.fakeB, "/v1/chat/completions").Model; got != upstreamB {
		t.Fatalf("fake B received model %q, want configured spelling %q", got, upstreamB)
	}

	// 2. A's upstream name is not reverse-expanded to the shared alias: only A
	// declares it, so A serves even though B is cheaper.
	costRoutingCall(t, key, upstreamA)
	events = usageFor(t, clientName, 2)
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Target]++
	}
	if counts[a] != 1 || counts[b] != 1 {
		t.Fatalf("serving targets = %v, want one request on each member", counts)
	}
	if got := lastRecord(t, w.fakeA, "/v1/chat/completions").Model; got != upstreamA {
		t.Fatalf("fake A received model %q, want configured spelling %q", got, upstreamA)
	}

	// 3. The alias again picks B; billing keeps each event at its serving
	// member's unchanged price (B=5, A=10), records that member's upstream in
	// lower case and cites that member's pricing template.
	costRoutingCall(t, key, alias)
	events = usageFor(t, clientName, 3)
	counts = map[string]int{}
	want := 0.0
	for _, ev := range events {
		counts[ev.Target]++
		if ev.CostIncomplete {
			t.Fatalf("incomplete cost: %+v", ev)
		}
		wantUpstream, ok := upstreamFor[ev.Target]
		if !ok {
			t.Fatalf("unexpected target %q", ev.Target)
		}
		if ev.Model != strings.ToLower(wantUpstream) {
			t.Fatalf("event model = %q, want lower-cased member upstream %q", ev.Model, strings.ToLower(wantUpstream))
		}
		if ev.PricingRuleID != ruleFor[ev.Target] {
			t.Fatalf("event target %s pricing rule = %q, want %q", ev.Target, ev.PricingRuleID, ruleFor[ev.Target])
		}
		switch ev.Target {
		case a:
			want += float64(ev.InputTokens+ev.OutputTokens) * 10 / 1e6
		case b:
			want += float64(ev.InputTokens+ev.OutputTokens) * 5 / 1e6
		}
	}
	if counts[a] != 1 || counts[b] != 2 {
		t.Fatalf("serving targets = %v, want two alias requests on %s and one upstream-name request on %s", counts, b, a)
	}
	if got := clientCost(t, clientName); math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %g, want %g (each event at its serving member's price)", got, want)
	}
}
