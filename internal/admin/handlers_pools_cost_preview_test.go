package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
)

type costPreviewResponse struct {
	Pool             string  `json:"pool"`
	RoutingStrategy  string  `json:"routing_strategy"`
	Active           bool    `json:"active"`
	Paused           bool    `json:"paused"`
	Model            string  `json:"model"`
	PricingAvailable bool    `json:"pricing_available"`
	PricingError     string  `json:"pricing_error"`
	Tolerance        float64 `json:"tolerance"`
	ReferenceTokens  struct {
		Input       int64 `json:"input"`
		CachedInput int64 `json:"cached_input"`
		Output      int64 `json:"output"`
	} `json:"reference_tokens"`
	Members []struct {
		Target           string   `json:"target"`
		Upstream         string   `json:"upstream"`
		ResolvedVia      string   `json:"resolved_via"`
		Eligible         bool     `json:"eligible"`
		Paused           bool     `json:"paused"`
		Priced           bool     `json:"priced"`
		EstimatedCostUSD *float64 `json:"estimated_cost_usd"`
		Tier             *int     `json:"tier"`
		Cheapest         bool     `json:"cheapest"`
		Reason           string   `json:"reason"`
		Phase            string   `json:"phase"`
	} `json:"members"`
}

func getCostPreview(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, costPreviewResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil))
	var resp costPreviewResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode preview %q: %v", rec.Body.String(), err)
		}
	}
	return rec, resp
}

func TestPoolCostRoutingPreviewHandler(t *testing.T) {
	h, _, _, service := newPoolTestHandler(t, t.TempDir())
	if rec := postPool(t, h, map[string]any{"name": "ds", "routing_strategy": "cost"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool: %d %s", rec.Code, rec.Body.String())
	}
	body := poolTargetUpdateBody("ds")
	body["model_mappings"] = []map[string]any{{"upstream": "Upstream-A", "fallback": "alias"}}
	if rec := putTarget(t, h, "target-1", body); rec.Code != http.StatusOK {
		t.Fatalf("update target-1: %d %s", rec.Code, rec.Body.String())
	}
	create, err := json.Marshal(map[string]any{
		"name": "target-2", "endpoint_type": "openai", "usage_adapter": "standard",
		"endpoint": "https://example.com", "api_key": "sk-target-2", "pool": "ds",
		"model_mappings": []map[string]any{{"upstream": "Upstream-B", "fallback": "alias"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(create)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create target-2: %d %s", rec.Code, rec.Body.String())
	}

	resolver := pricing.NewResolver([]pricing.PricingTemplate{
		{ID: "price-a", Targets: []string{"target-1"}, Models: []string{"Upstream-A"}, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
		{ID: "price-b", Targets: []string{"target-2"}, Models: []string{"Upstream-B"}, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}},
	}, nil)
	service.SetRateResolverSource(func() (usage.RateResolver, error) { return resolver, nil })

	rec, resp := getCostPreview(t, h, "/data/pools/ds/cost-routing?model=alias")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status: %d %s", rec.Code, rec.Body.String())
	}
	if resp.Pool != "ds" || resp.RoutingStrategy != "cost" || !resp.Active || resp.Paused {
		t.Fatalf("preview header = %+v", resp)
	}
	if resp.Model != "alias" || !resp.PricingAvailable || resp.PricingError != "" {
		t.Fatalf("preview pricing = %+v", resp)
	}
	if resp.Tolerance != 0.05 || resp.ReferenceTokens.Input != 1_000_000 || resp.ReferenceTokens.CachedInput != 1_000_000 || resp.ReferenceTokens.Output != 200_000 {
		t.Fatalf("preview mix = %+v", resp)
	}
	if len(resp.Members) != 2 {
		t.Fatalf("members = %+v", resp.Members)
	}
	byTarget := map[string]int{}
	for i, m := range resp.Members {
		byTarget[m.Target] = i
		if !m.Eligible || !m.Priced || m.ResolvedVia != "alias" || m.Phase != "ready" {
			t.Fatalf("member %q = %+v", m.Target, m)
		}
	}
	a, ok := byTarget["target-1"]
	if !ok {
		t.Fatalf("target-1 missing from %+v", resp.Members)
	}
	b, ok := byTarget["target-2"]
	if !ok {
		t.Fatalf("target-2 missing from %+v", resp.Members)
	}
	if got := resp.Members[a]; got.Upstream != "Upstream-A" || got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 10 || got.Tier == nil || *got.Tier != 1 || got.Cheapest {
		t.Fatalf("target-1 = %+v", got)
	}
	if got := resp.Members[b]; got.Upstream != "Upstream-B" || got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 5 || got.Tier == nil || *got.Tier != 0 || !got.Cheapest {
		t.Fatalf("target-2 = %+v", got)
	}

	if rec, _ := getCostPreview(t, h, "/data/pools/ds/cost-routing"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model status = %d", rec.Code)
	}
	if rec, _ := getCostPreview(t, h, "/data/pools/missing/cost-routing?model=alias"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pool status = %d", rec.Code)
	}
	if rec := postPool(t, h, map[string]any{"name": "load-pool", "routing_strategy": "load"}); rec.Code != http.StatusCreated {
		t.Fatalf("create load pool: %d %s", rec.Code, rec.Body.String())
	}
	rec, loadResp := getCostPreview(t, h, "/data/pools/load-pool/cost-routing?model=alias")
	if rec.Code != http.StatusOK || loadResp.Active || loadResp.RoutingStrategy != "load" {
		t.Fatalf("load pool preview = %d %+v", rec.Code, loadResp)
	}
	var inactive map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &inactive); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "members", "reference_tokens", "tolerance"} {
		if _, ok := inactive[key]; ok {
			t.Fatalf("inactive preview carries %q: %s", key, rec.Body.String())
		}
	}

	// 空策略（旧数据/默认）等同 cost：预览生效。
	if rec := postPool(t, h, map[string]any{"name": "default-pool"}); rec.Code != http.StatusCreated {
		t.Fatalf("create default pool: %d %s", rec.Code, rec.Body.String())
	}
	if rec, defResp := getCostPreview(t, h, "/data/pools/default-pool/cost-routing?model=alias"); rec.Code != http.StatusOK || !defResp.Active || defResp.RoutingStrategy != "cost" {
		t.Fatalf("default pool preview = %d %+v", rec.Code, defResp)
	}

	service.SetRateResolverSource(func() (usage.RateResolver, error) { return nil, errors.New("pricing down") })
	rec, unavailable := getCostPreview(t, h, "/data/pools/ds/cost-routing?model=alias")
	if rec.Code != http.StatusOK || unavailable.PricingAvailable || unavailable.PricingError != "pricing source unavailable" {
		t.Fatalf("unavailable preview = %d %+v", rec.Code, unavailable)
	}
	if len(unavailable.Members) != 2 {
		t.Fatalf("unavailable members = %+v", unavailable.Members)
	}
	for _, m := range unavailable.Members {
		if !m.Eligible || m.Priced || m.Tier != nil || m.Reason != "pricing_unavailable" {
			t.Fatalf("unavailable member = %+v", m)
		}
	}
}

// The preview route is in the read-only group: a readonly session can read it
// but still cannot write pools.
func TestPoolCostRoutingPreviewReadOnlySession(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, 1, nil)
	cfg.Targets[0].Pool = "ds"
	cfg.Targets[0].ModelMappings = []config.ModelMapping{{Upstream: "upstream-a", Fallback: "alias"}}
	cfg.Pools = []config.DeploymentPool{{Name: "ds", RoutingStrategy: config.PoolRoutingCost}}
	path := filepath.Join(dir, "config.json")
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	resolver := pricing.NewResolver([]pricing.PricingTemplate{{
		ID: "price", Targets: []string{cfg.Targets[0].Name}, Models: []string{"upstream-a"},
		Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1},
	}}, nil)
	service.SetRateResolverSource(func() (usage.RateResolver, error) { return resolver, nil })
	sessions := NewSessionManager(config.AdminSessionConfig{Secret: "fixture-session-secret", CookieName: "session", TTLSeconds: 3600}, logger)
	h := NewHandler(config.NewManager(path), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessions, logger)
	router := chi.NewRouter()
	router.Route("/admin", func(r chi.Router) {
		r.Use(sessions.Middleware)
		r.Mount("/", h)
	})
	_, cookie, err := sessions.CreateSession(RoleReadonly, RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/data/pools/ds/cost-routing?model=alias", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readonly preview status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp costPreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Active || !resp.PricingAvailable || len(resp.Members) != 1 || resp.Members[0].Target != cfg.Targets[0].Name || !resp.Members[0].Priced {
		t.Fatalf("readonly preview = %+v", resp)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/data/pools", bytes.NewBufferString(`{"name":"other"}`))
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly pool write status = %d", rec.Code)
	}
}
