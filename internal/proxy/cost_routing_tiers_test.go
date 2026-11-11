package proxy

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// fiveTierService: A=B (cheapest) < C=D < E, all in one cost pool and each
// with capacity 10 so the soft cap (0.8) is reachable.
func fiveTierService(t *testing.T) *Service {
	t.Helper()
	price := func(v float64) usage.CostRates {
		return usage.CostRates{InputPer1MTokens: v, OutputPer1MTokens: v, CacheReadPer1MToken: v}
	}
	var targets []config.Target
	var templates []pricing.PricingTemplate
	for _, m := range []struct {
		name string
		p    float64
	}{{"A", 1}, {"B", 1}, {"C", 2}, {"D", 2}, {"E", 3}} {
		tg := costTarget(m.name, "ds", "")
		tg.MaxConcurrency = 10
		targets = append(targets, tg)
		templates = append(templates, flatTemplate(m.name, price(m.p)))
	}
	return newCostService(t, config.PoolRoutingCost, targets, templates)
}

func TestCostRoutingWalksTiersInPriceOrder(t *testing.T) {
	now := costTestAt(2026, 11, 4, 10, 0)

	t.Run("cheapest tier shares load", func(t *testing.T) {
		s := fiveTierService(t)
		picks := costPicks(t, s, now, nil, 16)
		assertShared(t, picks, "A", "B")
		if picks["C"]+picks["D"]+picks["E"] != 0 {
			t.Fatalf("picks = %v, want only A/B", picks)
		}
	})

	t.Run("A and B down: C and D back each other up", func(t *testing.T) {
		s := fiveTierService(t)
		for _, n := range []string{"A", "B"} {
			st, _ := s.targetByName(n)
			st.MarkFailure(now, time.Minute)
		}
		picks := costPicks(t, s, now, nil, 16)
		assertShared(t, picks, "C", "D")
		if picks["E"] != 0 {
			t.Fatalf("picks = %v, E must wait until C and D are unusable", picks)
		}
	})

	t.Run("A attempted, B muted: request moves to C/D not E", func(t *testing.T) {
		s := fiveTierService(t)
		st, _ := s.targetByName("B")
		st.MarkFailure(now, time.Minute)
		picks := costPicks(t, s, now, map[string]struct{}{"a": {}}, 8)
		if picks["E"] != 0 || picks["C"]+picks["D"] != 8 {
			t.Fatalf("picks = %v, want only C/D", picks)
		}
	})

	t.Run("saturated cheap tier spills to the next tier, not all", func(t *testing.T) {
		s := fiveTierService(t)
		for _, n := range []string{"A", "B"} {
			st, _ := s.targetByName(n)
			st.inFlight.Store(8) // at the soft cap
		}
		picks := costPicks(t, s, now, nil, 16)
		if picks["E"] != 0 || picks["A"]+picks["B"] != 0 {
			t.Fatalf("picks = %v, want spill to C/D only", picks)
		}
	})

	t.Run("only E left", func(t *testing.T) {
		s := fiveTierService(t)
		for _, n := range []string{"A", "B", "C", "D"} {
			st, _ := s.targetByName(n)
			st.MarkFailure(now, time.Minute)
		}
		assertOnly(t, costPicks(t, s, now, nil, 4), "E")
	})
}

// Failover candidates are chosen by client alias + request protocol: each
// member must resolve the alias through its own model_mappings and support
// the request path; price tiers are walked only among those members.
func TestCostRoutingTiersByAliasAndProtocol(t *testing.T) {
	now := costTestAt(2026, 11, 4, 10, 0)
	const messagesPath = "/v1/messages"
	cheapOpenAI := aliasedTarget("cheap-openai", "ds", "ds-up-a") // cheapest, OpenAI only
	midClaude := aliasedTarget("mid-claude", "ds", "ds-up-b")
	midClaude.EndpointType = config.EndpointTypeClaude
	topClaude := aliasedTarget("top-claude", "ds", "ds-up-c")
	topClaude.EndpointType = config.EndpointTypeClaude
	noAlias := costTarget("no-alias", "ds", "") // cheapest of all, other model
	noAlias.EndpointType = config.EndpointTypeClaude
	noAlias.ModelMappings = []config.ModelMapping{{Upstream: "other", Fallback: "other-alias"}}
	s := newCostService(t, config.PoolRoutingCost,
		[]config.Target{cheapOpenAI, midClaude, topClaude, noAlias},
		[]pricing.PricingTemplate{
			aliasFlatTemplate("cheap-openai", "ds-up-a", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1}),
			aliasFlatTemplate("mid-claude", "ds-up-b", usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 2}),
			aliasFlatTemplate("top-claude", "ds-up-c", usage.CostRates{InputPer1MTokens: 3, OutputPer1MTokens: 3}),
			aliasFlatTemplate("no-alias", "other", usage.CostRates{InputPer1MTokens: 0.1, OutputPer1MTokens: 0.1}),
		})
	pick := func(path string, attempted map[string]struct{}) string {
		state, lease, _, err := s.selectTarget(nil, "", "", "", nil, attempted, costTestAlias, path, now)
		if err != nil {
			t.Fatalf("selectTarget %s: %v", path, err)
		}
		state.releaseProbe(lease)
		return state.Target().Name
	}
	if got := pick(costTestPath, nil); got != "cheap-openai" {
		t.Fatalf("chat path picked %s, want cheap-openai", got)
	}
	if got := pick(messagesPath, nil); got != "mid-claude" {
		t.Fatalf("messages path picked %s, want mid-claude (cheap-openai lacks the protocol, no-alias lacks the alias)", got)
	}
	if got := pick(messagesPath, map[string]struct{}{"mid-claude": {}}); got != "top-claude" {
		t.Fatalf("messages failover picked %s, want top-claude", got)
	}
}
