package proxy

import (
	"errors"
	"math"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func previewMember(t *testing.T, p CostRoutingPreview, target string) CostRoutingPreviewMember {
	t.Helper()
	for _, m := range p.Members {
		if m.Target == target {
			return m
		}
	}
	t.Fatalf("member %q not found in %+v", target, p.Members)
	return CostRoutingPreviewMember{}
}

func previewCost(t *testing.T, m CostRoutingPreviewMember) float64 {
	t.Helper()
	if m.EstimatedCostUSD == nil {
		t.Fatalf("member %q has no estimated cost", m.Target)
	}
	return *m.EstimatedCostUSD
}

func previewTier(t *testing.T, m CostRoutingPreviewMember) int {
	t.Helper()
	if m.Tier == nil {
		t.Fatalf("member %q has no tier", m.Target)
	}
	return *m.Tier
}

func TestCostRoutingPreviewRanksAliasMembers(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	p := s.CostRoutingPreview("ds", costTestAlias, peak)

	if p.Model != costTestAlias || !p.PricingAvailable || p.PricingError != "" {
		t.Fatalf("preview header = %+v", p)
	}
	if math.Abs(p.Tolerance-costTierTolerance) > 1e-12 {
		t.Fatalf("tolerance = %v", p.Tolerance)
	}
	if p.ReferenceTokens.Input != costRefInputTokens || p.ReferenceTokens.CachedInput != costRefCachedTokens || p.ReferenceTokens.Output != costRefOutputTokens {
		t.Fatalf("reference tokens = %+v", p.ReferenceTokens)
	}
	if len(p.Members) != 2 {
		t.Fatalf("members = %+v, want 2", p.Members)
	}

	official := previewMember(t, p, "official")
	agg := previewMember(t, p, "agg")
	for _, m := range []CostRoutingPreviewMember{official, agg} {
		if !m.Eligible || m.Paused || !m.Priced || m.ResolvedVia != "alias" {
			t.Fatalf("member %q = %+v", m.Target, m)
		}
		if m.Phase != phaseReady || m.InFlight != 0 {
			t.Fatalf("member %q runtime = %+v", m.Target, m)
		}
	}
	if agg.Upstream != "deepseek-chat" || official.Upstream != "DeepSeek-V4.1-Flash" {
		t.Fatalf("upstreams = %q / %q", official.Upstream, agg.Upstream)
	}
	// peak: official 9*1 + 27*0.2 + 0.3*1 = 14.7; agg 4.5*1 + 13.5*0.2 + 2.25*1 = 9.45.
	if got, want := previewCost(t, official), 14.7; math.Abs(got-want) > 1e-9 {
		t.Fatalf("official cost = %v, want %v", got, want)
	}
	if got, want := previewCost(t, agg), 9.45; math.Abs(got-want) > 1e-9 {
		t.Fatalf("agg cost = %v, want %v", got, want)
	}
	if previewTier(t, agg) != 0 || !agg.Cheapest {
		t.Fatalf("agg tier = %+v, want cheapest tier 0", agg)
	}
	if previewTier(t, official) != 1 || official.Cheapest {
		t.Fatalf("official tier = %+v, want tier 1", official)
	}
}

func TestCostRoutingPreviewToleranceBoundary(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	build := func(name string, in float64) *Service {
		t.Helper()
		member := aliasedTarget(name, "ds", "upstream-"+name)
		other := aliasedTarget("other", "ds", "upstream-other")
		return newCostService(t, config.PoolRoutingCost, []config.Target{member, other}, []pricing.PricingTemplate{
			aliasFlatTemplate(name, "upstream-"+name, usage.CostRates{InputPer1MTokens: in}),
			aliasFlatTemplate("other", "upstream-other", usage.CostRates{InputPer1MTokens: 10}),
		})
	}

	// Exactly 1.05x of the cheapest is still in tier 0.
	p := build("boundary", 10.5).CostRoutingPreview("ds", costTestAlias, peak)
	if tier := previewTier(t, previewMember(t, p, "boundary")); tier != 0 {
		t.Fatalf("1.05x tier = %d, want 0", tier)
	}
	// Just above 1.05x moves to tier 1.
	p = build("above", 10.51).CostRoutingPreview("ds", costTestAlias, peak)
	if tier := previewTier(t, previewMember(t, p, "above")); tier != 1 {
		t.Fatalf("1.051x tier = %d, want 1", tier)
	}
}

func TestCostRoutingPreviewEligibilityReasons(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	pausedTarget := aliasedTarget("paused-target", "ds", "upstream-paused")
	pausedTarget.Paused = true
	pausedMapping := aliasedTarget("paused-mapping", "ds", "upstream-paused-mapping")
	pausedMapping.ModelMappings[0].Paused = true
	undeclared := costTarget("undeclared", "ds", "")
	undeclared.ModelMappings = []config.ModelMapping{{Upstream: "other-upstream", Fallback: "other-alias"}}
	unrestricted := costTarget("unrestricted", "ds", "")
	unrestricted.ModelMappings = nil
	s := newCostService(t, config.PoolRoutingCost, []config.Target{
		aliasedTarget("official", "ds", "upstream-official"),
		pausedTarget,
		pausedMapping,
		undeclared,
		unrestricted,
	}, []pricing.PricingTemplate{
		aliasFlatTemplate("official", "upstream-official", usage.CostRates{InputPer1MTokens: 1}),
		aliasFlatTemplate("paused-target", "upstream-paused", usage.CostRates{InputPer1MTokens: 1}),
		aliasFlatTemplate("paused-mapping", "upstream-paused-mapping", usage.CostRates{InputPer1MTokens: 1}),
	})

	p := s.CostRoutingPreview("ds", costTestAlias, peak)
	official := previewMember(t, p, "official")
	if !official.Eligible || !official.Priced || official.ResolvedVia != "alias" {
		t.Fatalf("official = %+v", official)
	}

	pt := previewMember(t, p, "paused-target")
	if !pt.Paused || pt.Eligible || pt.Tier != nil || pt.Reason != "target_paused" {
		t.Fatalf("paused target = %+v", pt)
	}
	pm := previewMember(t, p, "paused-mapping")
	if pm.Eligible || pm.Tier != nil || pm.Reason != "mapping_paused" || pm.ResolvedVia != "none" {
		t.Fatalf("paused mapping = %+v", pm)
	}
	u := previewMember(t, p, "undeclared")
	if u.Eligible || u.Reason != "alias_not_declared" || u.ResolvedVia != "none" {
		t.Fatalf("undeclared = %+v", u)
	}
	un := previewMember(t, p, "unrestricted")
	if !un.Eligible || un.ResolvedVia != "unrestricted" || un.Reason != "unpriced" || un.Tier != nil {
		t.Fatalf("unrestricted = %+v", un)
	}
}

func TestCostRoutingPreviewPricingUnavailable(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	s.SetRateResolverSource(func() (usage.RateResolver, error) {
		return nil, errors.New("pricing store down")
	})
	p := s.CostRoutingPreview("ds", costTestAlias, peak)
	if p.PricingAvailable || p.PricingError != "pricing store down" {
		t.Fatalf("pricing header = %+v", p)
	}
	if len(p.Members) != 2 {
		t.Fatalf("members = %+v", p.Members)
	}
	for _, m := range p.Members {
		if !m.Eligible || m.Priced || m.Tier != nil || m.Reason != "pricing_unavailable" {
			t.Fatalf("member %q = %+v", m.Target, m)
		}
	}
}

func TestCostRoutingPreviewDoesNotTouchRuntimeState(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	for _, state := range s.targetSnapshot() {
		if state.inFlightCount() != 0 || state.affinityHitCount() != 0 {
			t.Fatalf("precondition: %s in_flight=%d affinity=%d", state.Target().Name, state.inFlightCount(), state.affinityHitCount())
		}
	}
	_ = s.CostRoutingPreview("ds", costTestAlias, peak)
	for _, state := range s.targetSnapshot() {
		if state.inFlightCount() != 0 || state.affinityHitCount() != 0 {
			t.Fatalf("preview mutated %s: in_flight=%d affinity=%d", state.Target().Name, state.inFlightCount(), state.affinityHitCount())
		}
	}
}

func TestCostRoutingPreviewUnknownPoolIsEmpty(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	p := s.CostRoutingPreview("missing", costTestAlias, peak)
	if len(p.Members) != 0 {
		t.Fatalf("members = %+v, want empty", p.Members)
	}
	if p.GeneratedAt.IsZero() {
		t.Fatal("generated_at is zero")
	}
}
