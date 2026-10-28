package pricing

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestModelPrecisionIncludesAllTargetsAndBindings(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	conditions := []PriceCondition{{ID: "context"}}
	r := NewResolver([]PricingTemplate{
		{Model: "other", Start: start.Add(time.Minute), Mode: ModeFlat},
		{Model: "stable", Target: "first", Mode: ModeFlat},
		{Model: "STABLE", Target: "second", Start: start, Mode: ModeSchedule, Timezone: "+00:00", Periods: []PricingPeriod{{Start: "09:00", End: "10:00"}}},
		{Model: "target-only", Target: "hidden", Start: start.Add(time.Minute), Mode: ModeFlat},
		{Model: "global", Start: start.Add(time.Minute), Mode: ModeFlat},
		{Targets: []string{"a", "b"}, Models: []string{"shared-a", "shared-b"}, Start: start, Mode: ModeSchedule, Timezone: "+00:00", Periods: []PricingPeriod{{Start: "09:30", End: "10:00"}}},
		{Target: "hidden", Model: "base-condition", Conditions: conditions, Mode: ModeFlat},
		{Target: "hidden", Model: "period-condition", Start: start, Mode: ModeSchedule, Timezone: "+00:00", Periods: []PricingPeriod{{Start: "09:00", End: "10:00", Conditions: conditions}}},
		{Target: "hidden", Model: "date-condition", Start: start, Mode: ModeSchedule, Timezone: "+00:00", DateExceptions: []DateException{{StartDate: "2026-10-01", EndDate: "2026-10-02", Conditions: conditions}}},
		{Target: "hidden", Model: "date-offset", Start: start, Mode: ModeSchedule, Timezone: "+05:30", DateExceptions: []DateException{{StartDate: "2026-10-01", EndDate: "2026-10-02"}}},
		{Target: "hidden", Model: "date-aligned", Start: start, Mode: ModeSchedule, Timezone: "+08:00", DateExceptions: []DateException{{StartDate: "2026-10-01", EndDate: "2026-10-02"}}},
	}, nil)
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{" stable ", false}, {"unconfigured", false}, {"TARGET-ONLY", true}, {"global", true}, {"shared-a", true}, {" SHARED-B ", true}, {"base-condition", true}, {"period-condition", true}, {"date-condition", true}, {"date-offset", true}, {"date-aligned", false}, {"", true},
	} {
		if got := usage.RequiresEventTimestampsForModel(r, tc.model); got != tc.want {
			t.Errorf("model=%q got=%v want=%v", tc.model, got, tc.want)
		}
	}
}
