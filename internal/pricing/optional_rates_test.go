package pricing

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"testing"
	"time"
)

func TestResolverOwnsOptionalRates(t *testing.T) {
	price := 2.0
	template := PricingTemplate{Model: "m", BaseRates: usage.CostRates{ExplicitCacheReadPer1MToken: &price}}
	r := NewResolver([]PricingTemplate{template}, nil)
	price = 99
	got, ok := r.RateFor("", "m", time.Now())
	if !ok || *got.ExplicitCacheReadPer1MToken != 2 {
		t.Fatalf("caller mutation leaked: %+v", got)
	}
	*got.ExplicitCacheReadPer1MToken = 80
	next, _ := r.RateFor("", "m", time.Now())
	if *next.ExplicitCacheReadPer1MToken != 2 {
		t.Fatal("returned pointer aliases resolver")
	}
}

func TestValidateOptionalPrices(t *testing.T) {
	negative, free := -1.0, 0.0
	tmpl := PricingTemplate{Model: "m", BaseRates: usage.CostRates{CacheWrite1hPer1MToken: &negative}}
	if ValidateTemplate(&tmpl) == nil {
		t.Fatal("accepted negative TTL price")
	}
	tmpl.BaseRates.CacheWrite1hPer1MToken = &free
	if err := ValidateTemplate(&tmpl); err != nil {
		t.Fatal(err)
	}
}
