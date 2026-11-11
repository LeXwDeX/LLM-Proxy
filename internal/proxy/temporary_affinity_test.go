package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func cheapExpensiveService(t *testing.T, strategy string) (*Service, *auth.Principal, map[string]int) {
	t.Helper()
	hits := map[string]int{}
	upstream := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[name]++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	cheap, pricey := upstream("cheap"), upstream("pricey")
	s := newCostService(t, strategy,
		[]config.Target{costTarget("cheap", "ds", cheap.URL), costTarget("pricey", "ds", pricey.URL)},
		[]pricing.PricingTemplate{
			flatTemplate("cheap", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1, CacheReadPer1MToken: 1}),
			flatTemplate("pricey", usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 10, CacheReadPer1MToken: 10}),
		})
	return s, costPrincipal(t), hits
}

func serveCostSession(s *Service, p *auth.Principal, session string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, costTestPath, bytes.NewBufferString(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", session)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

// A session born while the cheap tier is cooling binds to the expensive tier
// only temporarily: the binding never slides and expires, so the session
// returns to the cheap tier after it recovers.
func TestOverflowBindingIsTemporary(t *testing.T) {
	s, p, hits := cheapExpensiveService(t, config.PoolRoutingCost)
	cheap, _ := s.targetByName("cheap")
	cheap.MarkFailure(time.Now(), time.Hour)

	if rr := serveCostSession(s, p, "sess-new"); rr.Code != http.StatusOK || hits["pricey"] != 1 {
		t.Fatalf("status=%d hits=%v", rr.Code, hits)
	}
	sessKey := sessionAffinityKey(p.Name, "sess-new", costTestModel)
	if got, _ := s.affinity.Get(sessKey, time.Now()); got != "pricey" || !s.affinity.IsTemporary(sessKey, time.Now()) {
		t.Fatalf("session binding = %q temporary=%v, want a temporary pricey binding", got, s.affinity.IsTemporary(sessKey, time.Now()))
	}
	// Further use does not extend it.
	_ = serveCostSession(s, p, "sess-new")
	if _, ok := s.affinity.Get(sessKey, time.Now().Add(temporarySessionAffinityTTL+time.Second)); ok {
		t.Fatal("temporary binding slid past its absolute expiry")
	}
	ipKey := affinityKey("192.0.2.1", p.Name, costTestModel)
	if !s.affinity.IsTemporary(ipKey, time.Now()) {
		t.Fatal("IP binding to the overflow tier must be temporary")
	}

	// After recovery and expiry the session lands on the cheap tier, sliding.
	cheap.MarkSuccess(time.Now())
	for _, k := range []string{sessKey, ipKey} {
		s.affinity.mu.Lock()
		delete(s.affinity.entries, k) // simulate expiry
		s.affinity.mu.Unlock()
	}
	if rr := serveCostSession(s, p, "sess-new"); rr.Code != http.StatusOK || hits["cheap"] != 1 {
		t.Fatalf("after recovery status=%d hits=%v", rr.Code, hits)
	}
	if got, _ := s.affinity.Get(sessKey, time.Now()); got != "cheap" || s.affinity.IsTemporary(sessKey, time.Now()) {
		t.Fatalf("recovered binding = %q temporary=%v, want a sliding cheap binding", got, s.affinity.IsTemporary(sessKey, time.Now()))
	}
}

// An established sliding binding is never demoted, even to a pricier target.
func TestEstablishedBindingStaysSliding(t *testing.T) {
	s, p, _ := cheapExpensiveService(t, config.PoolRoutingCost)
	sessKey := sessionAffinityKey(p.Name, "sess-old", costTestModel)
	s.affinity.SetWithTTL(sessKey, "pricey", time.Now(), sessionAffinityTTL)
	if rr := serveCostSession(s, p, "sess-old"); rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if got, _ := s.affinity.Get(sessKey, time.Now()); got != "pricey" || s.affinity.IsTemporary(sessKey, time.Now()) {
		t.Fatalf("established binding = %q temporary=%v, want sliding pricey", got, s.affinity.IsTemporary(sessKey, time.Now()))
	}
}

// Performance-first pools do not compare prices: every binding slides.
func TestLoadPoolBindingsAreNotTemporary(t *testing.T) {
	s, p, _ := cheapExpensiveService(t, config.PoolRoutingLoad)
	cheap, _ := s.targetByName("cheap")
	cheap.MarkFailure(time.Now(), time.Hour)
	if rr := serveCostSession(s, p, "sess-load"); rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if s.affinity.IsTemporary(sessionAffinityKey(p.Name, "sess-load", costTestModel), time.Now()) {
		t.Fatal("performance-first pool binding must not be temporary")
	}
}

// H2: a request that hit a temporary binding and finished after its expiry
// must not renew it.
func TestExpiredTemporaryBindingIsNotRenewedByInFlightHit(t *testing.T) {
	m := newAffinityMap()
	now := time.Now()
	m.SetTemporary("k", "pricey", now, sessionAffinityTTL, temporarySessionAffinityTTL)
	end := now.Add(temporarySessionAffinityTTL + time.Second)
	if m.Touch("k", "pricey", end, sessionAffinityTTL) {
		t.Fatal("Touch reported an expired temporary binding as live")
	}
	if _, ok := m.Get("k", end); ok {
		t.Fatal("expired temporary binding came back")
	}
	// A live sliding binding slides.
	m.SetWithTTL("s", "cheap", now, sessionAffinityTTL)
	if !m.Touch("s", "cheap", now.Add(time.Hour), sessionAffinityTTL) {
		t.Fatal("live sliding binding not refreshed")
	}
	if _, ok := m.Get("s", now.Add(time.Hour+sessionAffinityTTL-time.Minute)); !ok {
		t.Fatal("sliding binding did not slide")
	}
}

// M2: with priced cost-pool members eligible, an unpriced member reached by
// overflow is not preferred.
func TestUnpricedOverflowBindingIsTemporary(t *testing.T) {
	now := costTestAt(2026, 11, 4, 10, 0)
	s := newCostService(t, config.PoolRoutingCost,
		[]config.Target{costTarget("priced", "ds", ""), costTarget("unpriced", "ds", "")},
		[]pricing.PricingTemplate{flatTemplate("priced", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1})})
	unpriced, _ := s.targetByName("unpriced")
	priced, _ := s.targetByName("priced")
	if s.inPreferredCostTier(unpriced.Target(), nil, costTestModel, costTestPath, now) {
		t.Fatal("unpriced overflow member must not be preferred while a priced member is eligible")
	}
	if !s.inPreferredCostTier(priced.Target(), nil, costTestModel, costTestPath, now) {
		t.Fatal("cheapest priced member must be preferred")
	}
}
