package proxy

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func aliasAffinityTarget(name, upstream, alias string) config.Target {
	t := costTarget(name, "ds", "")
	t.ModelMappings = []config.ModelMapping{{Upstream: upstream, Fallback: alias}}
	return t
}

// Set-side affinity keys use the serving member's configured upstream spelling.
// With one alias mapped to different upstream spellings, the Get side must try
// every member's spelling, not just the first one in target order.
func TestIPAffinityAliasWithDivergentUpstreamSpellings(t *testing.T) {
	principal := costPrincipal(t)
	now := costTestAt(2026, 11, 4, 10, 0)
	const ip = "10.0.0.9"
	s := newCostService(t, config.PoolRoutingLoad, []config.Target{
		aliasAffinityTarget("secondary", "glm-5.3-flash", "glm-flash"),
		aliasAffinityTarget("primary", "GLM-5.3-FLASH", "glm-flash"),
	}, nil)

	// The previous request was served by "primary", so the Set side stored its
	// original upstream spelling. The client now asks by the shared alias.
	s.affinity.Set(affinityKey(ip, principal.Name, "GLM-5.3-FLASH"), "primary", now)

	state, lease, kind, err := s.selectTarget(principal, ip, "", "", nil, nil, "glm-flash", costTestPath, now)
	if err != nil {
		t.Fatal(err)
	}
	state.releaseProbe(lease)
	if state.Target().Name != "primary" || kind != selectionAffinityHit {
		t.Fatalf("got %s kind=%v, want sticky primary affinity hit", state.Target().Name, kind)
	}
}