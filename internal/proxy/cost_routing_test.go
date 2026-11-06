package proxy

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const (
	costTestModel = "deepseek-v4-pro"
	costTestPath  = "/v1/chat/completions"
)

var costTestCST = time.FixedZone("CST", 8*3600)

func costTestAt(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, costTestCST)
}

func costTarget(name, pool string, endpoint string) config.Target {
	if endpoint == "" {
		endpoint = "http://127.0.0.1:1"
	}
	return config.Target{
		Name:          name,
		EndpointType:  "openai",
		UsageAdapter:  config.UsageAdapterStandard,
		Endpoint:      endpoint,
		APIKey:        "key-" + name,
		Pool:          pool,
		ModelMappings: []config.ModelMapping{{Upstream: costTestModel}},
	}
}

func flatTemplate(target string, rates usage.CostRates) pricing.PricingTemplate {
	return pricing.PricingTemplate{
		ID:        "flat-" + target,
		Targets:   []string{target},
		Models:    []string{costTestModel},
		Mode:      pricing.ModeFlat,
		BaseRates: rates,
	}
}

// officialPeakValley mirrors a vendor price page: weekday 9-12 and 14-18
// (+08:00) are peak, every other hour and holidays are half price.
func officialPeakValley(target string) pricing.PricingTemplate {
	peak := usage.CostRates{InputPer1MTokens: 9, OutputPer1MTokens: 27, CacheReadPer1MToken: 0.3}
	idle := usage.CostRates{InputPer1MTokens: 4.5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 0.15}
	weekdays := []int{1, 2, 3, 4, 5}
	return pricing.PricingTemplate{
		ID:        "schedule-" + target,
		Targets:   []string{target},
		Models:    []string{costTestModel},
		Mode:      pricing.ModeSchedule,
		Timezone:  "+08:00",
		BaseRates: idle,
		Periods: []pricing.PricingPeriod{
			{Weekdays: weekdays, Start: "09:00", End: "12:00", Rates: peak},
			{Weekdays: weekdays, Start: "14:00", End: "18:00", Rates: peak},
		},
		DateExceptions: []pricing.DateException{{StartDate: "2026-10-01", EndDate: "2026-10-08", Rates: idle}},
	}
}

// aggregatorHalfPrice is half of the peak list price all day, with a cache
// price that is worse than the official idle cache price.
func aggregatorHalfPrice(target string) pricing.PricingTemplate {
	return flatTemplate(target, usage.CostRates{InputPer1MTokens: 4.5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 2.25})
}

func newCostService(t *testing.T, strategy string, targets []config.Target, templates []pricing.PricingTemplate) *Service {
	t.Helper()
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: targets,
		Pools:   []config.DeploymentPool{{Name: "ds", DisplayName: "DeepSeek 部署池", RoutingStrategy: strategy}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if templates != nil {
		resolver := pricing.NewResolver(templates, nil)
		s.SetRateResolverSource(func() (usage.RateResolver, error) { return resolver, nil })
	}
	return s
}

func costPicks(t *testing.T, s *Service, now time.Time, attempted map[string]struct{}, n int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		state, lease, _, err := s.selectTarget(nil, "", "", "", nil, attempted, costTestModel, costTestPath, now)
		if err != nil {
			t.Fatalf("selectTarget at %s: %v", now, err)
		}
		state.releaseProbe(lease)
		counts[state.Target().Name]++
	}
	return counts
}

func assertOnly(t *testing.T, counts map[string]int, want string) {
	t.Helper()
	if len(counts) != 1 || counts[want] == 0 {
		t.Fatalf("picks = %v, want only %q", counts, want)
	}
}

func assertShared(t *testing.T, counts map[string]int, names ...string) {
	t.Helper()
	for _, name := range names {
		if counts[name] == 0 {
			t.Fatalf("picks = %v, want traffic on every one of %v", counts, names)
		}
	}
}

func peakValleyService(t *testing.T, strategy string) *Service {
	return newCostService(t, strategy,
		[]config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")},
		[]pricing.PricingTemplate{officialPeakValley("official"), aggregatorHalfPrice("agg")})
}

func TestCostRoutingFollowsScheduleAtBoundaries(t *testing.T) {
	s := peakValleyService(t, config.PoolRoutingCost)
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"before morning peak", costTestAt(2026, 11, 4, 8, 59), "official"},
		{"morning peak starts", costTestAt(2026, 11, 4, 9, 0), "agg"},
		{"morning peak last minute", costTestAt(2026, 11, 4, 11, 59), "agg"},
		{"lunch is idle", costTestAt(2026, 11, 4, 12, 0), "official"},
		{"afternoon peak starts", costTestAt(2026, 11, 4, 14, 0), "agg"},
		{"evening is idle", costTestAt(2026, 11, 4, 18, 0), "official"},
		{"night is idle", costTestAt(2026, 11, 4, 2, 0), "official"},
		{"saturday is idle", costTestAt(2026, 11, 7, 10, 0), "official"},
		{"holiday exception is idle", costTestAt(2026, 10, 1, 10, 0), "official"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertOnly(t, costPicks(t, s, tc.at, nil, 6), tc.want)
		})
	}
}

func TestCostRoutingTieWithinToleranceSharesLoad(t *testing.T) {
	idle := costTestAt(2026, 11, 4, 20, 0)
	// 7.35 vs 7.60 per reference mix: 3.4% apart, one tier.
	near := newCostService(t, config.PoolRoutingCost,
		[]config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")},
		[]pricing.PricingTemplate{officialPeakValley("official"),
			flatTemplate("agg", usage.CostRates{InputPer1MTokens: 4.75, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 0.15})})
	assertShared(t, costPicks(t, near, idle, nil, 8), "official", "agg")

	// 7.35 vs 7.85: 6.8% apart, strict tiers.
	far := newCostService(t, config.PoolRoutingCost,
		[]config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")},
		[]pricing.PricingTemplate{officialPeakValley("official"),
			flatTemplate("agg", usage.CostRates{InputPer1MTokens: 5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 0.15})})
	assertOnly(t, costPicks(t, far, idle, nil, 8), "official")
}

func TestCostRoutingNeverReducesAvailability(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)

	t.Run("cheapest muted", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingCost)
		agg, _ := s.targetByName("agg")
		agg.MarkFailure(peak, time.Minute)
		assertOnly(t, costPicks(t, s, peak, nil, 4), "official")
	})

	t.Run("cheapest at hard capacity", func(t *testing.T) {
		targets := []config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")}
		targets[1].MaxConcurrency = 1
		s := newCostService(t, config.PoolRoutingCost, targets,
			[]pricing.PricingTemplate{officialPeakValley("official"), aggregatorHalfPrice("agg")})
		agg, _ := s.targetByName("agg")
		agg.inFlight.Store(1)
		assertOnly(t, costPicks(t, s, peak, nil, 4), "official")
	})

	t.Run("cheapest already attempted", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingCost)
		assertOnly(t, costPicks(t, s, peak, map[string]struct{}{"agg": {}}, 4), "official")
	})

	t.Run("saturated cheap tier falls back to load balancing", func(t *testing.T) {
		targets := []config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")}
		targets[0].MaxConcurrency, targets[1].MaxConcurrency = 10, 10
		s := newCostService(t, config.PoolRoutingCost, targets,
			[]pricing.PricingTemplate{officialPeakValley("official"), aggregatorHalfPrice("agg")})
		official, _ := s.targetByName("official")
		agg, _ := s.targetByName("agg")
		official.inFlight.Store(1)
		agg.inFlight.Store(7) // below the soft cap: price still wins
		assertOnly(t, costPicks(t, s, peak, nil, 4), "agg")
		agg.inFlight.Store(8) // at the soft cap: least loaded wins
		assertOnly(t, costPicks(t, s, peak, nil, 4), "official")
	})
}

func TestCostRoutingUnpricedAndOutsidePoolRankLast(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)
	s := newCostService(t, config.PoolRoutingCost,
		[]config.Target{costTarget("priced", "ds", ""), costTarget("unpriced", "ds", ""), costTarget("outside", "", "")},
		[]pricing.PricingTemplate{
			flatTemplate("priced", usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 10}),
			flatTemplate("outside", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1}),
		})
	assertOnly(t, costPicks(t, s, peak, nil, 4), "priced")
	assertOnly(t, costPicks(t, s, peak, map[string]struct{}{"priced": {}}, 4), "unpriced")
	assertOnly(t, costPicks(t, s, peak, map[string]struct{}{"priced": {}, "unpriced": {}}, 4), "outside")
}

func TestCostRoutingDisabledKeepsLoadBalancing(t *testing.T) {
	peak := costTestAt(2026, 11, 4, 10, 0)

	t.Run("load strategy", func(t *testing.T) {
		assertShared(t, costPicks(t, peakValleyService(t, ""), peak, nil, 8), "official", "agg")
		assertShared(t, costPicks(t, peakValleyService(t, config.PoolRoutingLoad), peak, nil, 8), "official", "agg")
	})

	t.Run("prices unavailable", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingCost)
		s.SetRateResolverSource(func() (usage.RateResolver, error) { return nil, errors.New("pricing store down") })
		assertShared(t, costPicks(t, s, peak, nil, 8), "official", "agg")
	})

	t.Run("hot switch through prepared config", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingCost)
		assertOnly(t, costPicks(t, s, peak, nil, 4), "agg")
		cfg := &config.Config{
			Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
			Targets: []config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")},
			Pools:   []config.DeploymentPool{{Name: "ds", RoutingStrategy: config.PoolRoutingLoad}},
		}
		prepared, err := s.PrepareConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		s.CommitPrepared(prepared)
		assertShared(t, costPicks(t, s, peak, nil, 8), "official", "agg")
	})

	t.Run("no resolver wired", func(t *testing.T) {
		s := newCostService(t, config.PoolRoutingCost,
			[]config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")}, nil)
		assertShared(t, costPicks(t, s, peak, nil, 8), "official", "agg")
	})
}

func costPrincipal(t *testing.T) *auth.Principal {
	t.Helper()
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	return principal
}

func TestCostRoutingAffinityFollowsCheapestTier(t *testing.T) {
	principal := costPrincipal(t)
	peak := costTestAt(2026, 11, 4, 10, 0)
	idle := costTestAt(2026, 11, 4, 20, 0)
	const ip = "10.0.0.8"

	t.Run("ip affinity yields to a cheaper tier", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingCost)
		s.affinity.Set(affinityKey(ip, principal.Name, costTestModel), "official", peak)
		state, lease, kind, err := s.selectTarget(principal, ip, "", "", nil, nil, costTestModel, costTestPath, peak)
		if err != nil {
			t.Fatal(err)
		}
		state.releaseProbe(lease)
		if state.Target().Name != "agg" || kind != selectionCostRebind {
			t.Fatalf("got %s kind=%v, want agg costRebind", state.Target().Name, kind)
		}
	})

	t.Run("session binding yields to a cheaper tier", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingCost)
		state, lease, kind, err := s.selectTarget(principal, ip, "official", "", nil, nil, costTestModel, costTestPath, peak)
		if err != nil {
			t.Fatal(err)
		}
		state.releaseProbe(lease)
		if state.Target().Name != "agg" || kind != selectionCostRebind {
			t.Fatalf("got %s kind=%v, want agg costRebind", state.Target().Name, kind)
		}
	})

	t.Run("affinity is kept inside the cheapest tier", func(t *testing.T) {
		s := newCostService(t, config.PoolRoutingCost,
			[]config.Target{costTarget("official", "ds", ""), costTarget("agg", "ds", "")},
			[]pricing.PricingTemplate{officialPeakValley("official"),
				flatTemplate("agg", usage.CostRates{InputPer1MTokens: 4.5, OutputPer1MTokens: 13.5, CacheReadPer1MToken: 0.15})})
		for i := 0; i < 4; i++ {
			state, lease, kind, err := s.selectTarget(principal, ip, "agg", "", nil, nil, costTestModel, costTestPath, idle)
			if err != nil {
				t.Fatal(err)
			}
			state.releaseProbe(lease)
			if state.Target().Name != "agg" || kind != selectionAffinityHit {
				t.Fatalf("got %s kind=%v, want sticky agg", state.Target().Name, kind)
			}
		}
	})

	t.Run("load strategy keeps the binding", func(t *testing.T) {
		s := peakValleyService(t, config.PoolRoutingLoad)
		state, lease, kind, err := s.selectTarget(principal, ip, "official", "", nil, nil, costTestModel, costTestPath, peak)
		if err != nil {
			t.Fatal(err)
		}
		state.releaseProbe(lease)
		if state.Target().Name != "official" || kind != selectionAffinityHit {
			t.Fatalf("got %s kind=%v, want sticky official", state.Target().Name, kind)
		}
	})
}

// The switch happens on the next request: the session binding moves to the
// cheaper tier once that request succeeds, and a load pool keeps the binding.
func TestCostRoutingNextRequestRebindsSession(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		want     string
	}{
		{config.PoolRoutingCost, "agg"},
		{config.PoolRoutingLoad, "official"},
	} {
		t.Run(tc.strategy, func(t *testing.T) {
			hits := map[string]int{}
			upstream := func(name string) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits[name]++
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
				}))
			}
			official, agg := upstream("official"), upstream("agg")
			defer official.Close()
			defer agg.Close()
			s := newCostService(t, tc.strategy,
				[]config.Target{costTarget("official", "ds", official.URL), costTarget("agg", "ds", agg.URL)},
				[]pricing.PricingTemplate{
					flatTemplate("official", usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 10, CacheReadPer1MToken: 10}),
					flatTemplate("agg", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1, CacheReadPer1MToken: 1}),
				})
			principal := costPrincipal(t)
			sessionKey := sessionAffinityKey(principal.Name, "sess-1", costTestModel)
			s.affinity.SetWithTTL(sessionKey, "official", time.Now(), sessionAffinityTTL)

			req := httptest.NewRequest(http.MethodPost, costTestPath, bytes.NewBufferString(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Session-Id", "sess-1")
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
			if hits[tc.want] != 1 || len(hits) != 1 {
				t.Fatalf("upstream hits = %v, want one request on %s", hits, tc.want)
			}
			if bound, _ := s.affinity.Get(sessionKey, time.Now()); bound != tc.want {
				t.Fatalf("session bound to %q, want %q", bound, tc.want)
			}
		})
	}
}
