package nosql

import (
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestOptionalProviderPricesPersistZeroAndAbsent(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)
	free, hour := 0.0, 8.0
	mustUpsert(t, store, pricing.PricingTemplate{Model: "m", BaseRates: usage.CostRates{CacheReadPer1MToken: 2, ExplicitCacheReadPer1MToken: &free, CacheWrite1hPer1MToken: &hour}})
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	r := items[0].BaseRates
	if r.ExplicitCacheReadPer1MToken == nil || *r.ExplicitCacheReadPer1MToken != 0 || r.CacheWrite5mPer1MToken != nil || r.CacheWrite1hPer1MToken == nil || *r.CacheWrite1hPer1MToken != 8 {
		t.Fatalf("optional values lost: %+v", r)
	}
	var legacy usage.Event
	if err := json.Unmarshal([]byte(`{"cached_tokens":100,"cache_creation_tokens":80}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ExplicitCacheReadTokens != 0 || legacy.CacheCreation5mTokens != 0 || legacy.CacheCreation1hTokens != 0 || legacy.UsageStatus != "" {
		t.Fatalf("legacy history reclassified: %+v", legacy)
	}
}
