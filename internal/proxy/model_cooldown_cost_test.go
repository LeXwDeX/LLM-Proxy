package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Model-level cooldowns and the cost tiers: a member whose upstream model for
// the requested alias is rate limited is not admitted for that alias, so the
// tier counts as unavailable and traffic overflows; other aliases of the same
// member keep their cheap tier.

func TestCostRoutingModelCooldownOverflowsToNextTier(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	agg, _ := s.targetByName("agg")

	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 6), "agg")
	s.markTargetFailure(agg, time.Now(), http.StatusTooManyRequests, nil, false, "deepseek-chat")
	// agg's deepseek-chat is limited: the whole cheapest tier is unavailable, so
	// the alias spills to the official member, and returns when it recovers.
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 6), "official")
	agg.clearModelLimit("deepseek-chat")
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 6), "agg")
}

func TestCostRoutingModelCooldownKeepsOtherAliasOnCheapTier(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	agg, _ := s.targetByName("agg")
	// A different upstream model of agg (here the official spelling, which agg
	// does not declare) being limited does not touch the ds alias.
	s.markTargetFailure(agg, time.Now(), http.StatusTooManyRequests, nil, false, "some-other-model")
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 6), "agg")
}

func TestCostRoutingPreviewReflectsModelCooldown(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	agg, _ := s.targetByName("agg")
	s.markTargetFailure(agg, peak, http.StatusTooManyRequests, nil, false, "deepseek-chat")

	p := s.CostRoutingPreview("ds", costTestAlias, peak)
	aggMember := previewMember(t, p, "agg")
	if aggMember.Phase != phaseMuted || aggMember.ModelCooldownUntil == nil || !aggMember.ModelCooldownUntil.After(peak) {
		t.Fatalf("agg member for the limited alias = %+v, want muted with a cooldown deadline", aggMember)
	}
	if !aggMember.Eligible || !aggMember.Priced {
		t.Fatalf("a cooling member keeps its configured eligibility and price: %+v", aggMember)
	}
	if official := previewMember(t, p, "official"); official.Phase != phaseReady || official.ModelCooldownUntil != nil {
		t.Fatalf("official member = %+v", official)
	}
	// The target-level phase is untouched, and the preview changed nothing.
	if agg.Phase(peak) != phaseReady {
		t.Fatal("preview or model cooldown moved the target phase")
	}
	// After the deadline the alias reads ready again.
	later := peak.Add(time.Minute)
	if m := previewMember(t, s.CostRoutingPreview("ds", costTestAlias, later), "agg"); m.Phase != phaseReady || m.ModelCooldownUntil != nil {
		t.Fatalf("after the cooldown agg = %+v", m)
	}
}
