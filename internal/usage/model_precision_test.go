package usage

import (
	"testing"
	"time"
)

type globalPrecisionResolver bool

func (r globalPrecisionResolver) RateFor(string, string, time.Time) (CostRates, bool) {
	return CostRates{}, true
}
func (r globalPrecisionResolver) RequiresEventTimestamps() bool { return bool(r) }

type embeddedExactResolver struct{ CostTable }

func (embeddedExactResolver) RequiresEventTimestamps() bool { return true }

type modelPrecisionResolver struct{ globalPrecisionResolver }

func (modelPrecisionResolver) RequiresEventTimestampsForModel(model string) bool {
	return model == "exact"
}

func TestModelPrecisionFallbackAndCoefficient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		r     RateResolver
		model string
		want  bool
	}{
		{"nil", nil, "model", false},
		{"cost table", CostTable{}, "model", false},
		{"unknown resolver", globalPrecisionResolver(true), "model", true},
		{"unknown aligned", globalPrecisionResolver(false), "model", false},
		{"embedded override", embeddedExactResolver{}, "model", true},
		{"model scoped", modelPrecisionResolver{true}, "stable", false},
		{"model exact", modelPrecisionResolver{true}, "exact", true},
		{"empty scope", modelPrecisionResolver{true}, " ", true},
		{"coefficient stable", WithCoefficient(modelPrecisionResolver{true}, func(string, string) float64 { return 0 }), "stable", false},
		{"coefficient exact", WithCoefficient(modelPrecisionResolver{true}, func(string, string) float64 { return 0 }), "exact", true},
		{"coefficient unknown", WithCoefficient(globalPrecisionResolver(true), func(string, string) float64 { return 1 }), "stable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequiresEventTimestampsForModel(tc.r, tc.model); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
