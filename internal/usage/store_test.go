package usage

import (
	"testing"
	"time"
)

func TestFilterEvents(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{Timestamp: now.Add(-2 * time.Hour), ClientName: "alice", Model: "gpt-4"},
		{Timestamp: now.Add(-1 * time.Hour), ClientName: "bob", Model: "gpt-4"},
		{Timestamp: now, ClientName: "alice", Model: "gpt-3.5"},
		{Timestamp: now.Add(1 * time.Hour), ClientName: "charlie", Model: "gpt-4"},
	}

	t.Run("no filter", func(t *testing.T) {
		result := FilterEvents(events, Filter{})
		if len(result) != 4 {
			t.Errorf("expected 4 events, got %d", len(result))
		}
	})

	t.Run("filter by client", func(t *testing.T) {
		result := FilterEvents(events, Filter{ClientName: "alice"})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("filter by model case insensitive", func(t *testing.T) {
		result := FilterEvents(events, Filter{Model: "GPT-4"})
		if len(result) != 3 {
			t.Errorf("expected 3 events, got %d", len(result))
		}
	})

	t.Run("filter by time range", func(t *testing.T) {
		from := now.Add(-90 * time.Minute)
		to := now.Add(30 * time.Minute)
		result := FilterEvents(events, Filter{From: &from, To: &to})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("filter by from only", func(t *testing.T) {
		from := now.Add(-30 * time.Minute)
		result := FilterEvents(events, Filter{From: &from})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("filter by to only", func(t *testing.T) {
		to := now.Add(-30 * time.Minute)
		result := FilterEvents(events, Filter{To: &to})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("combined filter", func(t *testing.T) {
		result := FilterEvents(events, Filter{ClientName: "alice", Model: "gpt-4"})
		if len(result) != 1 {
			t.Errorf("expected 1 event, got %d", len(result))
		}
	})

	t.Run("empty events", func(t *testing.T) {
		result := FilterEvents(nil, Filter{ClientName: "alice"})
		if len(result) != 0 {
			t.Errorf("expected 0 events, got %d", len(result))
		}
	})
}

func TestNormalizeGroupBy(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hour", "hour"},
		{"HOUR", "hour"},
		{" Hour ", "hour"},
		{"day", "day"},
		{"DAY", "day"},
		{"", "day"},
		{"week", "day"},
		{"month", "day"},
	}

	for _, tt := range tests {
		result := NormalizeGroupBy(tt.input)
		if result != tt.expected {
			t.Errorf("NormalizeGroupBy(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestBucketStartFor(t *testing.T) {
	t.Run("hour grouping", func(t *testing.T) {
		ts := time.Date(2024, 3, 15, 14, 35, 42, 123456789, time.UTC)
		result := BucketStartFor(ts, "hour")
		expected := time.Date(2024, 3, 15, 14, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("BucketStartFor hour = %v, want %v", result, expected)
		}
	})

	t.Run("day grouping", func(t *testing.T) {
		ts := time.Date(2024, 3, 15, 14, 35, 42, 123456789, time.UTC)
		result := BucketStartFor(ts, "day")
		expected := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("BucketStartFor day = %v, want %v", result, expected)
		}
	})

	t.Run("converts to UTC", func(t *testing.T) {
		loc := time.FixedZone("CST", 8*3600)
		ts := time.Date(2024, 3, 15, 22, 30, 0, 0, loc) // 2024-03-15 14:30 UTC
		result := BucketStartFor(ts, "hour")
		expected := time.Date(2024, 3, 15, 14, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("BucketStartFor with timezone = %v, want %v", result, expected)
		}
	})
}

func TestStepDuration(t *testing.T) {
	if StepDuration("hour") != time.Hour {
		t.Errorf("StepDuration(hour) = %v, want %v", StepDuration("hour"), time.Hour)
	}
	if StepDuration("day") != 24*time.Hour {
		t.Errorf("StepDuration(day) = %v, want %v", StepDuration("day"), 24*time.Hour)
	}
	if StepDuration("") != 24*time.Hour {
		t.Errorf("StepDuration('') = %v, want %v", StepDuration(""), 24*time.Hour)
	}
}

func TestSortedDimensions(t *testing.T) {
	input := map[string]*DimensionTotals{
		"charlie": {Key: "charlie", Totals: Totals{Requests: 3}},
		"alice":   {Key: "alice", Totals: Totals{Requests: 1}},
		"bob":     {Key: "bob", Totals: Totals{Requests: 2}},
	}

	result := SortedDimensions(input)
	if len(result) != 3 {
		t.Fatalf("expected 3 dimensions, got %d", len(result))
	}

	// Should be sorted alphabetically
	if result[0].Key != "alice" {
		t.Errorf("expected first key to be 'alice', got %q", result[0].Key)
	}
	if result[1].Key != "bob" {
		t.Errorf("expected second key to be 'bob', got %q", result[1].Key)
	}
	if result[2].Key != "charlie" {
		t.Errorf("expected third key to be 'charlie', got %q", result[2].Key)
	}

	// Verify totals are preserved
	if result[0].Requests != 1 {
		t.Errorf("expected alice requests=1, got %d", result[0].Requests)
	}
}

func TestSortedDimensionsEmpty(t *testing.T) {
	result := SortedDimensions(map[string]*DimensionTotals{})
	if len(result) != 0 {
		t.Errorf("expected 0 dimensions, got %d", len(result))
	}
}

func TestAddEventTotals(t *testing.T) {
	costs := CostTable{
		"gpt-4": {
			InputPer1MTokens:    30.0,
			OutputPer1MTokens:   60.0,
			CacheReadPer1MToken: 15.0,
		},
	}

	target := &Totals{}
	evt := Event{
		EndpointType: "openai",
		Model:        "gpt-4",
		InputTokens:  1000,
		OutputTokens: 500,
		CachedTokens: 200,
	}

	AddEventTotals(target, evt, costs)

	if target.Requests != 1 {
		t.Errorf("expected requests=1, got %d", target.Requests)
	}
	if target.InputTokens != 1000 {
		t.Errorf("expected input_tokens=1000, got %d", target.InputTokens)
	}
	if target.OutputTokens != 500 {
		t.Errorf("expected output_tokens=500, got %d", target.OutputTokens)
	}
	if target.CachedTokens != 200 {
		t.Errorf("expected cached_tokens=200, got %d", target.CachedTokens)
	}

	// Expected cost: (1000/1M * 30) + (500/1M * 60) + (200/1M * 15) = 0.03 + 0.03 + 0.003 = 0.063
	expectedCost := 0.063
	if target.EstimatedCost < expectedCost-0.001 || target.EstimatedCost > expectedCost+0.001 {
		t.Errorf("expected estimated_cost≈%.3f, got %.6f", expectedCost, target.EstimatedCost)
	}

	// Add another event
	evt2 := Event{
		EndpointType: "openai",
		Model:        "gpt-4",
		InputTokens:  2000,
		OutputTokens: 1000,
		CachedTokens: 0,
	}
	AddEventTotals(target, evt2, costs)

	if target.Requests != 2 {
		t.Errorf("expected requests=2, got %d", target.Requests)
	}
	if target.InputTokens != 3000 {
		t.Errorf("expected input_tokens=3000, got %d", target.InputTokens)
	}
}

func TestAddEventTotalsNoCost(t *testing.T) {
	costs := CostTable{} // No rates defined

	target := &Totals{}
	evt := Event{
		EndpointType: "unknown",
		Model:        "unknown-model",
		InputTokens:  1000,
		OutputTokens: 500,
	}

	AddEventTotals(target, evt, costs)

	if target.Requests != 1 {
		t.Errorf("expected requests=1, got %d", target.Requests)
	}
	if target.EstimatedCost != 0 {
		t.Errorf("expected estimated_cost=0, got %.6f", target.EstimatedCost)
	}
}

func TestEstimateEventCost(t *testing.T) {
	costs := CostTable{
		"gpt-4": {
			InputPer1MTokens:     30.0,
			OutputPer1MTokens:    60.0,
			CacheWritePer1MToken: 15.0,
			CacheReadPer1MToken:  7.5,
		},
	}

	t.Run("with cost rates", func(t *testing.T) {
		evt := Event{
			Model:               "gpt-4",
			InputTokens:         1_000_000,
			OutputTokens:        1_000_000,
			CachedTokens:        1_000_000,
			CacheCreationTokens: 1_000_000,
		}
		cost := EstimateEventCost(evt, costs)
		// Expected: 30 + 60 + 15 + 7.5 = 112.5
		if cost < 112.4 || cost > 112.6 {
			t.Errorf("expected cost≈112.5, got %.2f", cost)
		}
	})

	t.Run("no cost rates", func(t *testing.T) {
		evt := Event{Model: "unknown"}
		cost := EstimateEventCost(evt, costs)
		if cost != 0 {
			t.Errorf("expected cost=0, got %.2f", cost)
		}
	})

	t.Run("zero tokens", func(t *testing.T) {
		evt := Event{Model: "gpt-4"}
		cost := EstimateEventCost(evt, costs)
		if cost != 0 {
			t.Errorf("expected cost=0, got %.2f", cost)
		}
	})
}

func TestLookupCost(t *testing.T) {
	costs := CostTable{
		"gpt-4":   {InputPer1MTokens: 30.0},
		"gpt-3.5": {InputPer1MTokens: 1.0},
	}

	t.Run("exact match", func(t *testing.T) {
		rates, ok := costs.LookupCost("gpt-4")
		if !ok {
			t.Fatal("expected to find rates")
		}
		if rates.InputPer1MTokens != 30.0 {
			t.Errorf("expected 30.0, got %.1f", rates.InputPer1MTokens)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := costs.LookupCost("gpt-5")
		if ok {
			t.Error("expected not to find rates")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		rates, ok := costs.LookupCost("GPT-4")
		if !ok {
			t.Fatal("expected to find rates (case insensitive)")
		}
		if rates.InputPer1MTokens != 30.0 {
			t.Errorf("expected 30.0, got %.1f", rates.InputPer1MTokens)
		}
	})

	t.Run("empty model", func(t *testing.T) {
		_, ok := costs.LookupCost("")
		if ok {
			t.Error("expected not to find rates for empty model")
		}
	})
}

func TestCostRatesScaled(t *testing.T) {
	explicitCacheReadRate := 0.2
	rates := CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheWritePer1MToken: 3, CacheReadPer1MToken: 0.5, ExplicitCacheReadPer1MToken: &explicitCacheReadRate}

	half := rates.Scaled(0.5)
	if half.InputPer1MTokens != 5 || half.OutputPer1MTokens != 10 || half.CacheWritePer1MToken != 1.5 || half.CacheReadPer1MToken != 0.25 {
		t.Fatalf("unexpected scaled rates: %+v", half)
	}
	if half.ExplicitCacheReadPer1MToken == nil || *half.ExplicitCacheReadPer1MToken != 0.1 {
		t.Fatalf("unexpected scaled explicit cache-read rate: %+v", half.ExplicitCacheReadPer1MToken)
	}
	if half.ExplicitCacheReadPer1MToken == rates.ExplicitCacheReadPer1MToken {
		t.Fatal("Scaled must deep-copy the optional explicit cache-read rate")
	}
	*half.ExplicitCacheReadPer1MToken = 99
	if *rates.ExplicitCacheReadPer1MToken != 0.2 {
		t.Fatalf("scaled copy mutated source pointer: got %v", *rates.ExplicitCacheReadPer1MToken)
	}

	same := rates.Scaled(1)
	if same.ExplicitCacheReadPer1MToken == nil || *same.ExplicitCacheReadPer1MToken != 0.2 {
		t.Fatalf("factor 1 must preserve value, got %+v", same)
	}
	if same.ExplicitCacheReadPer1MToken == rates.ExplicitCacheReadPer1MToken {
		t.Fatal("factor 1 must still return an independent optional-rate pointer")
	}
}

func TestCostRatesCostForExplicitCacheReads(t *testing.T) {
	explicitRate := 8.0
	rates := CostRates{CacheReadPer1MToken: 2, ExplicitCacheReadPer1MToken: &explicitRate}

	if got, want := rates.CostFor(0, 0, 1_000_000, 0, 250_000), 3.5; got != want {
		t.Fatalf("mixed implicit/explicit cache-read cost=%v, want %v", got, want)
	}
	if got, want := rates.CostFor(0, 0, 1_000_000, 0), 2.0; got != want {
		t.Fatalf("legacy call cost=%v, want %v", got, want)
	}
	if got, want := rates.CostFor(0, 0, 1_000_000, 0, 2_000_000), 8.0; got != want {
		t.Fatalf("explicit subset must clamp to cached total: cost=%v, want %v", got, want)
	}
	if got, want := rates.CostFor(0, 0, 1_000_000, 0, -1), 2.0; got != want {
		t.Fatalf("negative explicit subset must clamp to zero: cost=%v, want %v", got, want)
	}
}

func TestCostRatesExplicitCacheReadNilFallbackAndZeroOverride(t *testing.T) {
	fallback := CostRates{CacheReadPer1MToken: 2}
	if got, want := fallback.CostFor(0, 0, 1_000_000, 0, 250_000), 2.0; got != want {
		t.Fatalf("nil explicit rate must inherit cache-read rate: cost=%v, want %v", got, want)
	}

	free := 0.0
	override := CostRates{CacheReadPer1MToken: 2, ExplicitCacheReadPer1MToken: &free}
	if got, want := override.CostFor(0, 0, 1_000_000, 0, 250_000), 1.5; got != want {
		t.Fatalf("explicit zero rate must remain a free override: cost=%v, want %v", got, want)
	}
}

func TestAddEventTotalsUsageMetadata(t *testing.T) {
	target := &Totals{}
	AddEventTotals(target, Event{
		OutputTokens:            100,
		ReasoningTokens:         40,
		CachedTokens:            80,
		ExplicitCacheReadTokens: 30,
		UsageStatus:             UsageStatusIncomplete,
	}, nil)
	AddEventTotals(target, Event{UsageStatus: UsageStatusFallback}, nil)
	AddEventTotals(target, Event{}, nil) // legacy empty status is unknown, not explicitly incomplete

	if target.ReasoningTokens != 40 || target.ExplicitCacheReadTokens != 30 {
		t.Fatalf("new token totals not accumulated: %+v", target)
	}
	if target.IncompleteUsageCount != 1 {
		t.Fatalf("incomplete usage count=%d, want 1", target.IncompleteUsageCount)
	}
}

func TestWithCoefficient(t *testing.T) {
	base := CostTable{"gpt-4o": {InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheWritePer1MToken: 3, CacheReadPer1MToken: 0.5}}
	at := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	t.Run("scales resolved rates", func(t *testing.T) {
		wrapped := WithCoefficient(base, func(target, model string) float64 { return 0.5 })
		rates, ok := wrapped.RateFor("t", "gpt-4o", at)
		if !ok {
			t.Fatal("expected resolution")
		}
		if rates.InputPer1MTokens != 5 || rates.OutputPer1MTokens != 10 {
			t.Fatalf("unexpected discounted rates: %+v", rates)
		}
	})

	t.Run("zero coefficient makes resolved rates free", func(t *testing.T) {
		wrapped := WithCoefficient(base, func(target, model string) float64 { return 0 })
		rates, ok := wrapped.RateFor("t", "gpt-4o", at)
		if !ok {
			t.Fatal("expected resolution")
		}
		if rates.InputPer1MTokens != 0 || rates.OutputPer1MTokens != 0 || rates.CacheWritePer1MToken != 0 || rates.CacheReadPer1MToken != 0 {
			t.Fatalf("expected free rates, got %+v", rates)
		}
	})

	t.Run("negative coefficient falls back to original rates", func(t *testing.T) {
		wrapped := WithCoefficient(base, func(target, model string) float64 { return -1 })
		rates, ok := wrapped.RateFor("t", "gpt-4o", at)
		if !ok || rates.InputPer1MTokens != 10 {
			t.Fatalf("expected original rates, got %+v, ok=%t", rates, ok)
		}
	})

	t.Run("missing rates unaffected", func(t *testing.T) {
		wrapped := WithCoefficient(base, func(target, model string) float64 { return 0.5 })
		if _, ok := wrapped.RateFor("t", "unknown-model", at); ok {
			t.Fatal("expected miss to propagate")
		}
	})

	t.Run("nil fn passthrough", func(t *testing.T) {
		wrapped := WithCoefficient(base, nil)
		if _, ok := wrapped.RateFor("t", "gpt-4o", at); !ok {
			t.Fatal("expected passthrough resolution")
		}
	})
}

func TestCostRatesCacheTTLPartitions(t *testing.T) {
	five, hour := 5.0, 8.0
	r := CostRates{CacheWritePer1MToken: 4, CacheWrite5mPer1MToken: &five, CacheWrite1hPer1MToken: &hour}
	if got := r.CostFor(0, 0, 0, 1_000_000, 0, 200_000, 300_000); got != 5.4 {
		t.Fatalf("partition cost = %v", got)
	}
}
