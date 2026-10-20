package pricing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestDateExceptionOverridesWeekday(t *testing.T) {
	var template PricingTemplate
	if err := json.Unmarshal([]byte(`{"target":"t","model":"m","start":"2026-09-01T00:00:00Z","mode":"schedule","timezone":"+08:00","base_rates":{"input_per_1m_tokens":1},"periods":[{"weekdays":[1,2,3,4,5],"start":"09:00","end":"12:00","rates":{"input_per_1m_tokens":2}}],"date_exceptions":[{"start_date":"2026-10-01","end_date":"2026-10-08","rates":{"input_per_1m_tokens":1}}]}`), &template); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver([]PricingTemplate{template}, nil)
	at, _ := time.Parse(time.RFC3339, "2026-10-01T10:00:00+08:00")
	rates, found := resolver.RateFor("t", "m", at)
	if !found || rates.InputPer1MTokens != 1 {
		t.Fatalf("holiday price = %v, found = %v; want valley 1", rates.InputPer1MTokens, found)
	}
}

func TestConditionalPricesRequireEventTimestamps(t *testing.T) {
	var template PricingTemplate
	if err := json.Unmarshal([]byte(`{"target":"t","model":"m","mode":"flat","base_rates":{"input_per_1m_tokens":1},"conditions":[{"id":"long","min_input_tokens":100,"rates":{"input_per_1m_tokens":2}}]}`), &template); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver([]PricingTemplate{template}, nil)
	if !usage.RequiresEventTimestampsFor(resolver, "t", "m") {
		t.Fatal("conditional prices cannot use hourly totals")
	}
}

func ptr(v int64) *int64 { return &v }

func TestConditionalPriceBoundariesAndCompleteness(t *testing.T) {
	conditions := []PriceCondition{
		{ID: "fast-short", MaxInputTokens: ptr(100), ServiceTier: "fast", Rates: usage.CostRates{InputPer1MTokens: 3}},
		{ID: "standard-long", MinInputTokens: ptr(100), ServiceTier: "standard", Rates: usage.CostRates{InputPer1MTokens: 2}},
	}
	template := PricingTemplate{ID: "rule", Version: 7, Target: "t", Model: "m", Mode: ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}, Conditions: conditions}
	if err := ValidateTemplate(&template); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver([]PricingTemplate{template}, nil)
	for _, tc := range []struct {
		name     string
		total    int64
		tier     string
		state    string
		valid    bool
		want     float64
		complete bool
	}{
		{"threshold-minus-one", 99, "standard", usage.PricingValueValue, true, 1, true},
		{"threshold", 100, "standard", usage.PricingValueValue, true, 2, true},
		{"threshold-plus-one", 101, "standard", usage.PricingValueValue, true, 2, true},
		{"fast-short", 99, "fast", usage.PricingValueValue, true, 3, true},
		{"fast-long-unconfigured", 100, "fast", usage.PricingValueValue, true, 1, false},
		{"unknown-tier", 100, "future", usage.PricingValueValue, true, 1, false},
		{"null-tier", 100, "standard", usage.PricingValueNull, true, 1, false},
		{"empty-tier", 100, "standard", usage.PricingValueEmpty, true, 1, false},
		{"missing-tier", 100, "standard", "", true, 1, false},
		{"invalid-total", 100, "standard", usage.PricingValueValue, false, 1, false},
		{"explicit-zero", 0, "standard", usage.PricingValueValue, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evt := usage.Event{Target: "t", Model: "m", InputTokens: 25, CachedTokens: 75, PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: tc.total, TotalInputKnown: true, TotalInputValid: tc.valid, ActualServiceTier: tc.tier, ResponseServiceTier: usage.PricingValue{Raw: tc.tier, State: tc.state}}}
			result := resolver.ResolveEventRate(evt)
			if result.Rates.InputPer1MTokens != tc.want || result.Complete != tc.complete || result.Version != 7 {
				t.Fatalf("result=%+v", result)
			}
			usage.CheckEventPrices(&evt, resolver)
			if evt.CostIncomplete == tc.complete || evt.PricingRuleVersion != 7 || evt.PricingRuleID == "" {
				t.Fatalf("event=%+v", evt)
			}
		})
	}
	old := resolver.ResolveEventRate(usage.Event{Target: "t", Model: "m"})
	if old.Complete || old.Rates.InputPer1MTokens != 1 {
		t.Fatalf("legacy=%+v", old)
	}
}

func TestConditionsRejectAmbiguity(t *testing.T) {
	for _, conditions := range [][]PriceCondition{
		{{ID: "a", MinInputTokens: ptr(100)}, {ID: "b", ServiceTier: "fast"}},
		{{ID: "a", ServiceTier: "fast"}, {ID: "b", ServiceTier: "fast", Speed: "fast"}},
		{{ID: "a", MinInputTokens: ptr(100), MaxInputTokens: ptr(100)}},
		{{ID: "a", ServiceTier: "mystery"}},
		{{ID: "a"}},
	} {
		template := PricingTemplate{Target: "t", Model: "m", Conditions: conditions}
		if err := ValidateTemplate(&template); err == nil {
			t.Fatalf("accepted ambiguous/invalid conditions %+v", conditions)
		}
	}
}

func TestSpeedDimensionDoesNotRequireServiceTier(t *testing.T) {
	template := PricingTemplate{Target: "t", Model: "m", Conditions: []PriceCondition{{ID: "fast", Speed: "fast", Rates: usage.CostRates{InputPer1MTokens: 4}}}}
	result := NewResolver([]PricingTemplate{template}, nil).ResolveEventRate(usage.Event{Target: "t", Model: "m", PricingContext: &usage.PricingContext{Version: 1, ActualServiceTier: "priority", ActualSpeed: "fast", ResponseSpeed: usage.PricingValue{State: usage.PricingValueValue}}})
	if !result.Complete || result.Rates.InputPer1MTokens != 4 {
		t.Fatalf("result=%+v", result)
	}
}

func TestConditionalRatesCoefficientAndClone(t *testing.T) {
	free := 0.0
	min := int64(100)
	template := PricingTemplate{ID: "p", Version: 2, Target: "t", Model: "m", Conditions: []PriceCondition{{ID: "long", MinInputTokens: &min, Rates: usage.CostRates{InputPer1MTokens: 2, ExplicitCacheReadPer1MToken: &free}}}}
	resolver := NewResolver([]PricingTemplate{template}, nil)
	min = 1000
	free = 99
	template.Conditions[0].Rates.InputPer1MTokens = 99
	evt := usage.Event{Target: "t", Model: "m", InputTokens: 25, CachedTokens: 75, ExplicitCacheReadTokens: 75, PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: 100, TotalInputKnown: true, TotalInputValid: true}}
	for _, factor := range []float64{0, 0.2, 1} {
		wrapped := usage.WithCoefficient(resolver, func(string, string) float64 { return factor })
		result := usage.ResolveEventRate(evt, wrapped)
		if !result.Complete || result.Rates.InputPer1MTokens != 2*factor || result.Rates.ExplicitCacheReadPer1MToken == nil || *result.Rates.ExplicitCacheReadPer1MToken != 0 {
			t.Fatalf("result=%+v", result)
		}
		if cost := usage.EstimateEventCost(evt, wrapped); cost != float64(25)*2*factor/1e6 {
			t.Fatalf("cost=%v", cost)
		}
	}
}

func TestDateExceptionBoundariesAndHistory(t *testing.T) {
	start, _ := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2027-02-01T00:00:00Z")
	template := PricingTemplate{Target: "t", Model: "m", Start: start, End: &end, Mode: ModeSchedule, Timezone: "+08:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []PricingPeriod{{Weekdays: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "12:00", Rates: usage.CostRates{InputPer1MTokens: 2}}, {Weekdays: []int{1, 2, 3, 4, 5}, Start: "14:00", End: "18:00", Rates: usage.CostRates{InputPer1MTokens: 2}}}, DateExceptions: []DateException{{StartDate: "2026-10-01", EndDate: "2026-10-08", Rates: usage.CostRates{InputPer1MTokens: 1}}, {StartDate: "2026-12-31", EndDate: "2027-01-02", Rates: usage.CostRates{InputPer1MTokens: 1}}}}
	resolver := NewResolver([]PricingTemplate{template}, nil)
	for _, tc := range []struct {
		at    string
		want  float64
		found bool
	}{
		{"2026-10-01T10:00:00+08:00", 1, true},
		{"2026-10-07T23:59:59+08:00", 1, true},
		{"2026-10-08T09:00:00+08:00", 2, true},
		{"2026-10-10T10:00:00+08:00", 1, true},
		{"2026-10-09T08:59:59+08:00", 1, true},
		{"2026-10-09T09:00:00+08:00", 2, true},
		{"2026-10-09T12:00:00+08:00", 1, true},
		{"2026-10-09T14:00:00+08:00", 2, true},
		{"2026-10-09T18:00:00+08:00", 1, true},
		{"2027-01-01T10:00:00+08:00", 1, true},
		{"2026-08-31T23:59:59Z", 0, false},
		{"2027-02-01T00:00:00Z", 0, false},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		rates, found := resolver.RateFor("t", "m", at)
		if rates.InputPer1MTokens != tc.want || found != tc.found {
			t.Fatalf("%s rates=%+v found=%v", tc.at, rates, found)
		}
	}
	template.Timezone = "+05:30"
	template.Periods = nil
	shifted := NewResolver([]PricingTemplate{template}, nil)
	if !shifted.RequiresEventTimestampsFor("t", "m") {
		t.Fatal("local date midnight falls inside UTC hour")
	}
	template.DateExceptions = append(template.DateExceptions, DateException{StartDate: "2026-10-07", EndDate: "2026-10-09"})
	if err := ValidateTemplate(&template); err == nil {
		t.Fatal("overlap accepted")
	}
}

func TestDateAndPeriodConditionScopes(t *testing.T) {
	start, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	template := PricingTemplate{ID: "s", Target: "t", Model: "m", Start: start, Mode: ModeSchedule, Timezone: "+08:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Conditions: []PriceCondition{{ID: "base-long", MinInputTokens: ptr(100), Rates: usage.CostRates{InputPer1MTokens: 3}}}, Periods: []PricingPeriod{{Weekdays: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "12:00", Rates: usage.CostRates{InputPer1MTokens: 2}, Conditions: []PriceCondition{{ID: "peak-long", MinInputTokens: ptr(100), Rates: usage.CostRates{InputPer1MTokens: 6}}}}}, DateExceptions: []DateException{{StartDate: "2026-10-01", EndDate: "2026-10-08", Rates: usage.CostRates{InputPer1MTokens: 1}, Conditions: []PriceCondition{{ID: "holiday-long", MinInputTokens: ptr(100), Rates: usage.CostRates{InputPer1MTokens: 4}}}}}}
	resolver := NewResolver([]PricingTemplate{template}, nil)
	for _, tc := range []struct {
		at, id string
		rate   float64
	}{
		{"2026-10-01T10:00:00+08:00", "s/holiday-long", 4},
		{"2026-10-08T10:00:00+08:00", "s/peak-long", 6},
		{"2026-10-08T13:00:00+08:00", "s/base-long", 3},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		result := resolver.ResolveEventRate(usage.Event{Target: "t", Model: "m", Timestamp: at, PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: 100, TotalInputKnown: true, TotalInputValid: true}})
		if !result.Complete || result.Rates.InputPer1MTokens != tc.rate || result.RuleID != tc.id {
			t.Fatalf("%s result=%+v", tc.at, result)
		}
	}
}
