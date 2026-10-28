package nosql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

type unknownModelPrecisionResolver struct{}

func (unknownModelPrecisionResolver) RateFor(string, string, time.Time) (usage.CostRates, bool) {
	return usage.CostRates{InputPer1MTokens: 1}, true
}

func TestSourceLossRequiresModelWidePrecisionProof(t *testing.T) {
	db, store, _, day := cursorCostFixture(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexSourceMissing), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	flat := pricing.PricingTemplate{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}
	otherExact := pricing.PricingTemplate{Model: "other", Target: "hidden", Start: day.Add(time.Minute), Mode: pricing.ModeFlat}
	targetExact := pricing.PricingTemplate{Model: "qwen-max", Target: "hidden", Start: day.Add(time.Minute), Mode: pricing.ModeFlat}
	targetConditional := pricing.PricingTemplate{Model: "qwen-max", Target: "hidden", Mode: pricing.ModeFlat, Conditions: []pricing.PriceCondition{{ID: "conditional"}}}
	stable := pricing.NewResolver([]pricing.PricingTemplate{flat, otherExact}, nil)
	exact := pricing.NewResolver([]pricing.PricingTemplate{flat, targetExact}, nil)
	conditional := pricing.NewResolver([]pricing.PricingTemplate{flat, targetConditional}, nil)
	coefficient := func(string, string) float64 { return .5 }
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	filter := usage.Filter{ClientName: "alice", Model: " QWEN-MAX ", From: &from, To: &to}
	for _, tc := range []struct {
		name     string
		resolver usage.RateResolver
		wantCost float64
		blocked  bool
	}{
		{"stable other model exact", stable, 3, false},
		{"another target exact", exact, 0, true},
		{"another target conditional", conditional, 0, true},
		{"unknown resolver", unknownModelPrecisionResolver{}, 0, true},
		{"coefficient stable", usage.WithCoefficient(stable, coefficient), 1.5, false},
		{"coefficient exact", usage.WithCoefficient(exact, coefficient), 0, true},
		{"nil", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Even a model group containing only upstream cannot prove that the
			// lost index excluded another target of the same model.
			got, err := store.readCostUnitsInScope(context.Background(), filter, tc.resolver, true, nil, costReadHooks{}, func(target, _ string) bool { return target == "upstream" })
			if tc.blocked {
				if !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) || len(got.units) != 0 {
					t.Fatalf("unsafe scope accepted: units=%+v err=%v", got.units, err)
				}
			} else if err != nil || costUnitTotal(got.units) != tc.wantCost {
				t.Fatalf("cost=%v err=%v want=%v", costUnitTotal(got.units), err, tc.wantCost)
			}
		})
	}
	lastHour := day.Add(9*time.Hour + 30*time.Minute)
	got, err := store.readCostUnits(context.Background(), filter, stable, false, &lastHour, costReadHooks{})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) || len(got.units) != 0 || len(got.raw) != 0 {
		t.Fatalf("rawFrom must remain blocked: result=%+v err=%v", got, err)
	}
}
