package usage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestImageCostPartitions(t *testing.T) {
	for _, tc := range []struct {
		name, prices                                        string
		input, output, cached, image, cachedImage, imageOut int64
		want                                                float64
	}{
		{"mixed", `,"image_input_per_1m_tokens":10,"cached_image_input_per_1m_tokens":3,"image_output_per_1m_tokens":20`, 1000000, 1000000, 1000000, 200000, 300000, 400000, 15.6},
		{"cached fallback", `,"image_input_per_1m_tokens":10`, 0, 0, 1000000, 0, 1000000, 0, 10},
		{"explicit free", `,"image_input_per_1m_tokens":0,"cached_image_input_per_1m_tokens":0,"image_output_per_1m_tokens":0`, 1000000, 1000000, 1000000, 1000000, 1000000, 1000000, 0},
		{"clamped", `,"image_input_per_1m_tokens":10,"image_output_per_1m_tokens":20`, 1000000, 1000000, 0, 2000000, -1, 2000000, 30},
		{"legacy", ``, 1000000, 1000000, 1000000, 0, 0, 0, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rates CostRates
			if err := json.Unmarshal([]byte(`{"input_per_1m_tokens":2,"output_per_1m_tokens":4,"cache_read_per_1m_tokens":1`+tc.prices+`}`), &rates); err != nil {
				t.Fatal(err)
			}
			got := rates.CostFor(tc.input, tc.output, tc.cached, 0, 0, 0, 0, tc.image, tc.cachedImage, tc.imageOut)
			if math.Abs(got-tc.want) > 1e-10 {
				t.Fatalf("cost=%v want=%v", got, tc.want)
			}
			if scaled := rates.Scaled(.5).CostFor(tc.input, tc.output, tc.cached, 0, 0, 0, 0, tc.image, tc.cachedImage, tc.imageOut); math.Abs(scaled-tc.want/2) > 1e-10 {
				t.Fatalf("scaled=%v", scaled)
			}
		})
	}
}

func TestImageEventPartitionsNormalizedBeforePricing(t *testing.T) {
	image := 10.0
	rates := CostTable{"m": {ImageInputPer1MTokens: &image, ImageOutputPer1MTokens: &image}}
	evt := Event{Model: "m", InputTokens: 10, CachedTokens: 5, OutputTokens: 20, ImageInputTokens: 30, CachedImageInputTokens: 7, ImageOutputTokens: 40, ExplicitCacheReadTokens: 5}
	CheckImagePrices(&evt, rates)
	if evt.ImageInputTokens != 10 || evt.CachedImageInputTokens != 5 || evt.ImageOutputTokens != 20 || evt.ExplicitCacheReadTokens != 0 || !evt.CostIncomplete || !strings.Contains(evt.UsageDiagnostic, "image_partition_exceeds_total") {
		t.Fatalf("event=%+v", evt)
	}
	first := evt
	CheckImagePrices(&evt, rates)
	if evt != first {
		t.Fatalf("normalization is not idempotent: before=%+v after=%+v", first, evt)
	}
}

func TestImagePriceCompleteness(t *testing.T) {
	free := 0.0
	for _, tc := range []struct {
		name    string
		rates   CostRates
		evt     Event
		missing string
	}{
		{"missing input", CostRates{}, Event{InputTokens: 1, ImageInputTokens: 1}, "missing_image_input_price"},
		{"missing cached", CostRates{}, Event{CachedTokens: 1, CachedImageInputTokens: 1}, "missing_cached_image_input_price"},
		{"missing output", CostRates{}, Event{OutputTokens: 1, ImageOutputTokens: 1}, "missing_image_output_price"},
		{"free cached fallback", CostRates{ImageInputPer1MTokens: &free}, Event{CachedTokens: 1, CachedImageInputTokens: 1}, ""},
		{"cached price alone", CostRates{CachedImageInputPer1MTokens: &free}, Event{CachedTokens: 1, CachedImageInputTokens: 1}, ""},
		{"legacy", CostRates{}, Event{InputTokens: 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.evt.Model = "model"
			CheckImagePrices(&tc.evt, CostTable{"model": tc.rates})
			if tc.evt.CostIncomplete != (tc.missing != "") || tc.evt.UsageDiagnostic != tc.missing {
				t.Fatalf("event=%+v", tc.evt)
			}
		})
	}
	old := Event{Model: "m", InputTokens: 1, ImageInputTokens: 1, CostIncomplete: true, UsageDiagnostic: "audio"}
	CheckImagePrices(&old, CostTable{"m": {ImageInputPer1MTokens: &free}})
	if !old.CostIncomplete || old.UsageDiagnostic != "audio" {
		t.Fatal("cleared existing diagnostic")
	}
}
