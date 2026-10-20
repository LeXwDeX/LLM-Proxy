package pricing

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"testing"
	"time"
)

func TestImageCatalogTemplatePrecedenceAndOwnership(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatal(err)
	}
	cost := cat.LookupDefaultCost("openai", "gpt-image-2")
	if cost == nil {
		t.Fatal("missing image catalog fixture")
	}
	if err := json.Unmarshal([]byte(`{"image_input_per_1m_tokens":1.234567,"cached_image_input_per_1m_tokens":0,"image_output_per_1m_tokens":9.876543}`), cost); err != nil {
		t.Fatal(err)
	}
	cache := NewTargetPricingCache(func() ([]PricingTemplate, error) { return nil, nil }, cat, func() (map[string]string, error) { return map[string]string{"target": "openai"}, nil })
	got, ok := cache.Get().RateFor("target", "gpt-image-2", time.Now())
	if !ok || got.ImageInputPer1MTokens == nil || *got.ImageInputPer1MTokens != 1.234567 || got.CachedImageInputPer1MTokens == nil || *got.CachedImageInputPer1MTokens != 0 || got.ImageOutputPer1MTokens == nil || *got.ImageOutputPer1MTokens != 9.876543 {
		t.Fatalf("catalog price=%+v", got)
	}
	price := 2.345678
	templates := []PricingTemplate{{Model: "gpt-image-1", BaseRates: usage.CostRates{ImageInputPer1MTokens: &price}}, {Target: "target", Model: "gpt-image-1", BaseRates: usage.CostRates{ImageOutputPer1MTokens: &price}}}
	r := NewResolver(templates, nil)
	price = 99
	global, _ := r.RateFor("other", "gpt-image-1", time.Now())
	target, _ := r.RateFor("target", "gpt-image-1", time.Now())
	if global.ImageInputPer1MTokens == nil || *global.ImageInputPer1MTokens != 2.345678 || target.ImageOutputPer1MTokens == nil || *target.ImageOutputPer1MTokens != 2.345678 || target.ImageInputPer1MTokens != nil {
		t.Fatal("template precedence or pointer ownership lost")
	}
	*target.ImageOutputPer1MTokens = 0
	again, _ := r.RateFor("target", "gpt-image-1", time.Now())
	if *again.ImageOutputPer1MTokens != 2.345678 {
		t.Fatal("returned price aliases resolver")
	}
}
