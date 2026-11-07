package proxy

import (
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// CostReferenceTokens is the fixed token mix used by cost ranking and this
// preview: uncached input, cache-read input, output.
type CostReferenceTokens struct {
	Input       int64 `json:"input"`
	CachedInput int64 `json:"cached_input"`
	Output      int64 `json:"output"`
}

// CostRoutingPreview is a read-only, alias-first snapshot of how one cost pool
// ranks its members for a client request name at a given instant. It never
// acquires probe leases, reads affinity records, or changes selection; the
// caller (admin UI) renders it as a routing estimate, not a bill.
type CostRoutingPreview struct {
	Model            string                     `json:"model"`
	GeneratedAt      time.Time                  `json:"generated_at"`
	PricingAvailable bool                       `json:"pricing_available"`
	PricingError     string                     `json:"pricing_error,omitempty"`
	Tolerance        float64                    `json:"tolerance"`
	ReferenceTokens  CostReferenceTokens        `json:"reference_tokens"`
	Members          []CostRoutingPreviewMember `json:"members"`
}

// CostRoutingPreviewMember describes one member of the previewed pool.
// EstimatedCostUSD and Tier are pointers so an unpriced or ineligible member
// serializes as null rather than a misleading zero.
type CostRoutingPreviewMember struct {
	Target                 string   `json:"target"`
	Upstream               string   `json:"upstream,omitempty"`
	ResolvedVia            string   `json:"resolved_via"` // alias | upstream | unrestricted | none
	Eligible               bool     `json:"eligible"`
	Paused                 bool     `json:"paused"`
	Priced                 bool     `json:"priced"`
	EstimatedCostUSD       *float64 `json:"estimated_cost_usd"`
	Tier                   *int     `json:"tier"` // 0 cheapest tier, 1 other priced tier, null otherwise
	Cheapest               bool     `json:"cheapest"`
	Reason                 string   `json:"reason,omitempty"` // target_paused | mapping_paused | alias_not_declared | unpriced | pricing_unavailable
	Phase                  string   `json:"phase"`
	InFlight               int64    `json:"in_flight"`
	MaxConcurrency         int      `json:"max_concurrency"`
	AffinitySoftCapReached bool     `json:"affinity_soft_cap_reached"`
}

// CostRoutingPreview builds a diagnostic snapshot for one pool and one client
// request name. It reuses the same resolver and reference mix as selection so
// the ranking cannot drift from cost routing; it deliberately does not reuse
// selectTarget because that would acquire probes or apply affinity.
func (s *Service) CostRoutingPreview(poolName, model string, now time.Time) CostRoutingPreview {
	model = strings.TrimSpace(model)
	preview := CostRoutingPreview{
		Model:       model,
		GeneratedAt: now,
		Tolerance:   costTierTolerance,
		ReferenceTokens: CostReferenceTokens{
			Input:       costRefInputTokens,
			CachedInput: costRefCachedTokens,
			Output:      costRefOutputTokens,
		},
		Members: []CostRoutingPreviewMember{},
	}

	var resolver usage.RateResolver
	if source := s.rateSource.Load(); source == nil {
		preview.PricingError = "pricing source not configured"
	} else if r, err := (*source)(); err != nil {
		// Keep the API response free of internal store details; the raw error
		// stays in the server log for operators.
		preview.PricingError = "pricing source unavailable"
		if s.logger != nil {
			s.logger.Warn("cost routing preview: pricing source unavailable", "error", err)
		}
	} else if r == nil {
		preview.PricingError = "pricing resolver unavailable"
	} else {
		resolver = r
		preview.PricingAvailable = true
	}

	poolKey := strings.ToLower(strings.TrimSpace(poolName))
	if poolKey == "" {
		return preview
	}
	best := 0.0
	haveBest := false
	for _, state := range s.targetSnapshot() {
		if state == nil || state.Target() == nil {
			continue
		}
		t := state.Target()
		if strings.ToLower(strings.TrimSpace(t.Pool)) != poolKey {
			continue
		}
		member := buildPreviewMember(state, model, resolver, now)
		if member.Priced && member.EstimatedCostUSD != nil && member.Eligible {
			if !haveBest || *member.EstimatedCostUSD < best {
				best = *member.EstimatedCostUSD
				haveBest = true
			}
		}
		preview.Members = append(preview.Members, member)
	}

	if !haveBest {
		return preview
	}
	for i := range preview.Members {
		m := &preview.Members[i]
		if !m.Priced || !m.Eligible || m.EstimatedCostUSD == nil {
			continue
		}
		tier := 1
		if *m.EstimatedCostUSD <= best*(1+costTierTolerance) {
			tier = 0
			m.Cheapest = true
		}
		m.Tier = &tier
	}
	sortPreviewMembers(preview.Members)
	return preview
}

// sortPreviewMembers mirrors the routing groups: eligible priced members first
// (cheapest tier before other priced tiers, then by cost), then eligible
// unpriced members, then ineligible members; target name breaks ties.
func sortPreviewMembers(members []CostRoutingPreviewMember) {
	group := func(m CostRoutingPreviewMember) int {
		switch {
		case m.Eligible && m.Priced:
			return 0
		case m.Eligible:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		a, b := members[i], members[j]
		if group(a) != group(b) {
			return group(a) < group(b)
		}
		if a.Priced && b.Priced && a.Tier != nil && b.Tier != nil {
			if *a.Tier != *b.Tier {
				return *a.Tier < *b.Tier
			}
			if a.EstimatedCostUSD != nil && b.EstimatedCostUSD != nil && *a.EstimatedCostUSD != *b.EstimatedCostUSD {
				return *a.EstimatedCostUSD < *b.EstimatedCostUSD
			}
		}
		return a.Target < b.Target
	})
}

func buildPreviewMember(state *targetState, model string, resolver usage.RateResolver, now time.Time) CostRoutingPreviewMember {
	t := state.Target()
	member := CostRoutingPreviewMember{
		Target:                 t.Name,
		Paused:                 t.Paused,
		Phase:                  state.Phase(now),
		InFlight:               state.inFlightCount(),
		MaxConcurrency:         t.MaxConcurrency,
		AffinitySoftCapReached: !state.belowAffinitySoftCap(),
	}
	upstream, ok := resolveUpstreamModel(t, model)
	member.Upstream = upstream
	if !ok || upstream == "" || model == "" {
		member.ResolvedVia = "none"
		member.Eligible = false
		switch {
		case t.Paused:
			member.Reason = "target_paused"
		case pausedMappingMatches(t, model):
			member.Reason = "mapping_paused"
		default:
			member.Reason = "alias_not_declared"
		}
		return member
	}
	member.ResolvedVia = previewResolvedVia(t, model, upstream)
	if t.Paused {
		member.Eligible = false
		member.Reason = "target_paused"
		return member
	}
	if resolver == nil {
		member.Eligible = true
		member.Reason = "pricing_unavailable"
		return member
	}
	member.Eligible = true
	rates, priced := resolver.RateFor(t.Name, upstream, now)
	if !priced {
		member.Reason = "unpriced"
		return member
	}
	cost := rates.CostFor(costRefInputTokens, costRefOutputTokens, costRefCachedTokens, 0)
	member.Priced = true
	member.EstimatedCostUSD = &cost
	return member
}

// previewResolvedVia reports which declaration of the request name produced the
// member's upstream. Upstream matches take precedence, mirroring buildModelIndex.
func previewResolvedVia(t *Target, model, upstream string) string {
	if t.allowedModelIdx == nil {
		return "unrestricted"
	}
	key := strings.ToLower(strings.TrimSpace(model))
	for _, m := range t.ModelMappings {
		if m.Paused {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(m.Upstream), key) &&
			strings.EqualFold(strings.TrimSpace(m.Upstream), strings.TrimSpace(upstream)) {
			return "upstream"
		}
	}
	for _, m := range t.ModelMappings {
		if m.Paused {
			continue
		}
		for _, alias := range config.SplitFallbackAliases(m.Fallback) {
			if strings.EqualFold(strings.TrimSpace(alias), key) &&
				strings.EqualFold(strings.TrimSpace(m.Upstream), strings.TrimSpace(upstream)) {
				return "alias"
			}
		}
	}
	return "none"
}

// pausedMappingMatches reports whether the request name would have matched a
// paused mapping, so the preview can distinguish "paused" from "never declared".
func pausedMappingMatches(t *Target, model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return false
	}
	for _, m := range t.ModelMappings {
		if !m.Paused {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(m.Upstream), key) {
			return true
		}
		for _, alias := range config.SplitFallbackAliases(m.Fallback) {
			if strings.EqualFold(strings.TrimSpace(alias), key) {
				return true
			}
		}
	}
	return false
}
