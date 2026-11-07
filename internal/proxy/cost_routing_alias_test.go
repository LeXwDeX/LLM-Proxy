package proxy

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const costTestAlias = "ds-v4-flash"

func aliasedTarget(name, pool, upstream string) config.Target {
	t := costTarget(name, pool, "")
	t.ModelMappings = []config.ModelMapping{{Upstream: upstream, Fallback: costTestAlias}}
	return t
}

func aliasFlatTemplate(target, model string, rates usage.CostRates) pricing.PricingTemplate {
	return pricing.PricingTemplate{
		ID:        "flat-" + target + "-" + model,
		Targets:   []string{target},
		Models:    []string{model},
		Mode:      pricing.ModeFlat,
		BaseRates: rates,
	}
}

func aliasPeakValley(target, model string) pricing.PricingTemplate {
	peak := usage.CostRates{InputPer1MTokens: 9, OutputPer1MTokens: 27, CacheReadPer1MToken: 0.3}
	idle := usage.CostRates{InputPer1MTokens: 4.5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 0.15}
	weekdays := []int{1, 2, 3, 4, 5}
	return pricing.PricingTemplate{
		ID:        "schedule-" + target + "-" + model,
		Targets:   []string{target},
		Models:    []string{model},
		Mode:      pricing.ModeSchedule,
		Timezone:  "+08:00",
		BaseRates: idle,
		Periods: []pricing.PricingPeriod{
			{Weekdays: weekdays, Start: "09:00", End: "12:00", Rates: peak},
			{Weekdays: weekdays, Start: "14:00", End: "18:00", Rates: peak},
		},
	}
}

func costPicksForModel(t *testing.T, s *Service, now time.Time, model string, attempted map[string]struct{}, n int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		state, lease, _, err := s.selectTarget(nil, "", "", "", nil, attempted, model, costTestPath, now)
		if err != nil {
			t.Fatalf("selectTarget at %s: %v", now, err)
		}
		state.releaseProbe(lease)
		counts[state.Target().Name]++
	}
	return counts
}

// aliasService wires two pool members whose upstream spellings are unrelated
// but whose client-facing alias is identical. The official member uses a
// peak/valley template; the aggregator member is half price all day.
func aliasService(t *testing.T, strategy string) *Service {
	t.Helper()
	return newCostService(t, strategy,
		[]config.Target{
			aliasedTarget("official", "ds", "DeepSeek-V4.1-Flash"),
			aliasedTarget("agg", "ds", "deepseek-chat"),
		},
		[]pricing.PricingTemplate{
			aliasPeakValley("official", "DeepSeek-V4.1-Flash"),
			aliasFlatTemplate("agg", "deepseek-chat", usage.CostRates{InputPer1MTokens: 4.5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 2.25}),
		})
}

// The comparison unit is the client alias: both members declare ds-v4-flash
// even though they forward it as different upstream names.
func TestCostRoutingComparesMembersByClientAlias(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	idle := costTestAt(2026, 11, 4, 20, 0)

	s := aliasService(t, config.PoolRoutingCost)
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 6), "agg")
	assertOnly(t, costPicksForModel(t, s, idle, costTestAlias, nil, 6), "official")

	// Alias matching stays case-insensitive without changing the forwarded name.
	assertOnly(t, costPicksForModel(t, s, peak, "DS-V4-Flash", nil, 6), "agg")
}

// An upstream-name request does not reverse-resolve into the shared alias:
// it only compares members that accept that exact upstream spelling.
func TestCostRoutingUpstreamRequestStaysWithinDeclaringMembers(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	idle := costTestAt(2026, 11, 4, 20, 0)
	s := aliasService(t, config.PoolRoutingCost)

	// At idle the official member is cheaper, but deepseek-chat is only
	// declared by the aggregator member, so the request stays there.
	assertOnly(t, costPicksForModel(t, s, idle, "deepseek-chat", nil, 4), "agg")
	// At peak the aggregator is cheaper, but this upstream name is only
	// declared by the official member.
	assertOnly(t, costPicksForModel(t, s, peak, "DeepSeek-V4.1-Flash", nil, 4), "official")
}

// A cheap member that does not declare the requested alias is not a candidate.
func TestCostRoutingMemberWithoutAliasDoesNotJoinComparison(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	targets := []config.Target{
		aliasedTarget("official", "ds", "DeepSeek-V4.1-Flash"),
		aliasedTarget("agg", "ds", "deepseek-chat"),
	}
	other := costTarget("other", "ds", "")
	other.ModelMappings = []config.ModelMapping{{Upstream: "other-model", Fallback: "other-alias"}}
	targets = append(targets, other)
	s := newCostService(t, config.PoolRoutingCost, targets, []pricing.PricingTemplate{
		aliasPeakValley("official", "DeepSeek-V4.1-Flash"),
		aliasFlatTemplate("agg", "deepseek-chat", usage.CostRates{InputPer1MTokens: 4.5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 2.25}),
		aliasFlatTemplate("other", "other-model", usage.CostRates{InputPer1MTokens: 0.01, OutputPer1MTokens: 0.01}),
	})
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 6), "agg")

	// When both members that declare the alias are muted, the request fails
	// instead of falling through to the cheap member that never declared it.
	official, _ := s.targetByName("official")
	agg, _ := s.targetByName("agg")
	official.MarkFailure(peak, time.Minute)
	agg.MarkFailure(peak, time.Minute)
	if state, lease, _, err := s.selectTarget(nil, "", "", "", nil, nil, costTestAlias, costTestPath, peak); err == nil {
		state.releaseProbe(lease)
		t.Fatal("alias request unexpectedly used a member that does not declare it")
	}
}

// A paused mapping is removed from the target's model index, so it cannot be
// requested or compared even when its price would be lower.
func TestCostRoutingPausedAliasMappingDoesNotCompete(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	targets := []config.Target{
		aliasedTarget("official", "ds", "DeepSeek-V4.1-Flash"),
		aliasedTarget("paused", "ds", "deepseek-chat"),
	}
	targets[1].ModelMappings[0].Paused = true
	s := newCostService(t, config.PoolRoutingCost, targets, []pricing.PricingTemplate{
		aliasPeakValley("official", "DeepSeek-V4.1-Flash"),
		aliasFlatTemplate("paused", "deepseek-chat", usage.CostRates{InputPer1MTokens: 0.01, OutputPer1MTokens: 0.01}),
	})
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 4), "official")
}

// A client name can be one member's upstream spelling and another member's
// fallback alias. Both declare it and compete; this pins the existing shared
// name-space behavior and is not a claim that the two upstream models are
// equivalent.
func TestCostRoutingSharedUpstreamAndAliasNameComparesDeclaringMembers(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	direct := costTarget("direct", "ds", "")
	direct.ModelMappings = []config.ModelMapping{{Upstream: "deepseek-chat"}}
	aliased := costTarget("aliased", "ds", "")
	aliased.ModelMappings = []config.ModelMapping{{Upstream: "DeepSeek-V4.1-Flash", Fallback: "deepseek-chat"}}
	s := newCostService(t, config.PoolRoutingCost,
		[]config.Target{direct, aliased},
		[]pricing.PricingTemplate{
			aliasFlatTemplate("direct", "deepseek-chat", usage.CostRates{InputPer1MTokens: 100, OutputPer1MTokens: 100}),
			aliasFlatTemplate("aliased", "DeepSeek-V4.1-Flash", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1}),
		})
	assertOnly(t, costPicksForModel(t, s, peak, "deepseek-chat", nil, 4), "aliased")
}

// Pricing remains keyed by the member's upstream model. A template that only
// declares the alias price must not make an otherwise unpriced member cheap.
func TestCostRoutingDoesNotUseAliasPriceFallback(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := newCostService(t, config.PoolRoutingCost,
		[]config.Target{
			aliasedTarget("priced", "ds", "upstream-priced"),
			aliasedTarget("alias-only", "ds", "upstream-unpriced"),
		},
		[]pricing.PricingTemplate{
			aliasFlatTemplate("priced", "upstream-priced", usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 10}),
			aliasFlatTemplate("alias-only", costTestAlias, usage.CostRates{InputPer1MTokens: 0.01, OutputPer1MTokens: 0.01}),
		})
	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 4), "priced")
}

func debugCapture(s *Service) (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	previous := s.logger
	s.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf, func() { s.logger = previous }
}

// The selection diagnostic names the client alias first, then each admitted
// member's own upstream spelling and estimated cost.
func TestCostRoutingDebugLogsAliasFirstCandidates(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)
	buf, restore := debugCapture(s)
	defer restore()

	assertOnly(t, costPicksForModel(t, s, peak, costTestAlias, nil, 1), "agg")
	out := buf.String()
	for _, want := range []string{
		"cost routing candidates",
		"alias=ds-v4-flash",
		"admitted=2",
		"tier=1",
		"official→DeepSeek-V4.1-Flash[$",
		"agg→deepseek-chat[$",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug log missing %q:\n%s", want, out)
		}
	}
}

// A sticky target yielding to a cheaper tier logs both sides, alias-first, so
// operators can explain the rebind without reading pricing templates.
func TestCostRoutingDebugLogsRebindReason(t *testing.T) {
	principal := costPrincipal(t)
	peak := costTestAt(2026, 11, 4, 10, 0)
	const ip = "10.0.0.10"
	s := aliasService(t, config.PoolRoutingCost)
	buf, restore := debugCapture(s)
	defer restore()
	s.affinity.Set(affinityKey(ip, principal.Name, costTestAlias), "official", peak)

	state, lease, kind, err := s.selectTarget(principal, ip, "", "", nil, nil, costTestAlias, costTestPath, peak)
	if err != nil {
		t.Fatal(err)
	}
	state.releaseProbe(lease)
	if state.Target().Name != "agg" || kind != selectionCostRebind {
		t.Fatalf("got %s kind=%v, want agg costRebind", state.Target().Name, kind)
	}
	out := buf.String()
	for _, want := range []string{
		"cost routing rebind",
		"alias=ds-v4-flash",
		"bound_target=official",
		"bound_upstream=DeepSeek-V4.1-Flash",
		"candidate_target=agg",
		"candidate_upstream=deepseek-chat",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rebind log missing %q:\n%s", want, out)
		}
	}
}

// Load pools ignore prices even when members map the same alias to different
// upstream names; the alias comparison is a cost-pool-only concept.
func TestCostRoutingAliasLoadPoolsKeepLoadBalancing(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingLoad)
	assertShared(t, costPicksForModel(t, s, peak, costTestAlias, nil, 8), "official", "agg")
}

// Cost ranking never overrides an explicit target or the client's target ACL.
func TestCostRoutingDoesNotOverrideExplicitTargetOrACL(t *testing.T) {
	principal := costPrincipal(t)
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := aliasService(t, config.PoolRoutingCost)

	state, lease, kind, err := s.selectTarget(principal, "", "official", "official", nil, nil, costTestAlias, costTestPath, peak)
	if err != nil {
		t.Fatal(err)
	}
	state.releaseProbe(lease)
	if state.Target().Name != "official" || kind != selectionExplicit {
		t.Fatalf("explicit target = %s kind=%v, want official selection", state.Target().Name, kind)
	}

	// Only official is in the ACL, so the cheaper agg member must not appear.
	state, lease, _, err = s.selectTarget(principal, "", "", "", map[string]struct{}{"official": {}}, nil, costTestAlias, costTestPath, peak)
	if err != nil {
		t.Fatal(err)
	}
	state.releaseProbe(lease)
	if state.Target().Name != "official" {
		t.Fatalf("ACL request served by %s, want official", state.Target().Name)
	}
}
