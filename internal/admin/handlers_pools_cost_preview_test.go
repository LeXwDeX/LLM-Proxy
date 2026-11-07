package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
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
	if rec := postPool(t, h, map[string]any{"name": "load-pool"}); rec.Code != http.StatusCreated {
		t.Fatalf("create load pool: %d %s", rec.Code, rec.Body.String())
	}
	rec, loadResp := getCostPreview(t, h, "/data/pools/load-pool/cost-routing?model=alias")
	if rec.Code != http.StatusOK || loadResp.Active || loadResp.RoutingStrategy != "load" {
		t.Fatalf("load pool preview = %d %+v", rec.Code, loadResp)
	}

	service.SetRateResolverSource(func() (usage.RateResolver, error) { return nil, errors.New("pricing down") })
	rec, unavailable := getCostPreview(t, h, "/data/pools/ds/cost-routing?model=alias")
	if rec.Code != http.StatusOK || unavailable.PricingAvailable || unavailable.PricingError != "pricing down" {
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
