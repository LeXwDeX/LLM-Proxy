package pricing

import (
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestGPTTargetCatalogPricing(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]string{"target": "OPENAI"}
	fail := false
	var templates []PricingTemplate
	cache := NewTargetPricingCache(func() ([]PricingTemplate, error) { return templates, nil }, cat, func() (map[string]string, error) {
		if fail {
			return nil, errors.New("unavailable")
		}
		return providers, nil
	})
	check := func(provider string) {
		t.Helper()
		got, ok := cache.Get().RateFor("TARGET", "GPT-4", time.Now())
		want := cat.LookupDefaultCost(provider, "gpt-4")
		if !ok || want == nil || got.InputPer1MTokens != want.InputPer1MTokens || got.OutputPer1MTokens != want.OutputPer1MTokens {
			t.Fatalf("provider=%s rates=%+v want=%+v", provider, got, want)
		}
	}
	check("openai")
	providers["target"] = "azure_openai"
	check("openai") // cached snapshot is immutable
	cache.Invalidate()
	check("azure_openai")
	templates = []PricingTemplate{{Model: "gpt-4", BaseRates: usage.CostRates{}}}
	cache.Invalidate()
	if got, ok := cache.Get().RateFor("target", "gpt-4", time.Now()); !ok || got != (usage.CostRates{}) {
		t.Fatalf("explicit zero template lost: %+v %t", got, ok)
	}
	templates = nil
	cache.Invalidate()
	if _, ok := cache.Get().RateFor("target", "gpt-does-not-exist", time.Now()); ok {
		t.Fatal("unknown GPT received a rate")
	}
	fail = true
	cache.Invalidate()
	if _, ok := cache.Get().RateFor("target", "gpt-4", time.Now()); ok {
		t.Fatal("failed provider lookup guessed a GPT rate")
	}
	if cache.ptr.Load() != nil {
		t.Fatal("failed provider lookup was cached")
	}
}
