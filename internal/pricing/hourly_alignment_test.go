package pricing

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestResolverRequiresEventTimestamps(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, tz, begin, end string
		coverage             time.Time
		want                 bool
	}{
		{"fixed UTC hours", "+00:00", "09:00", "17:00", start, false},
		{"default UTC+8 hours", "+08:00", "09:00", "17:00", start, false},
		{"minute start", "+08:00", "09:30", "17:00", start, true},
		{"minute end", "+08:00", "09:00", "17:30", start, true},
		{"half-hour offset", "+05:30", "09:00", "17:00", start, true},
		{"IANA schedule", "Asia/Kolkata", "09:00", "17:00", start, true},
		{"partial-hour coverage", "+00:00", "09:00", "17:00", start.Add(time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewResolver([]PricingTemplate{{Model: "qwen-max", Start: tc.coverage, Mode: ModeSchedule, Timezone: tc.tz,
				Periods: []PricingPeriod{{Weekdays: []int{1}, Start: tc.begin, End: tc.end}}}}, nil)
			if got := resolver.RequiresEventTimestamps(); got != tc.want {
				t.Fatalf("RequiresEventTimestamps=%v, want %v", got, tc.want)
			}
		})
	}
	end := start.Add(90 * time.Minute)
	resolver := NewResolver([]PricingTemplate{{Model: "qwen-max", Start: start, End: &end, Mode: ModeFlat}}, nil)
	if !resolver.RequiresEventTimestamps() {
		t.Fatal("partial-hour coverage End needs event timestamps")
	}
	if usage.RequiresEventTimestamps(usage.CostTable{}) || !usage.RequiresEventTimestamps(usage.WithCoefficient(resolver, func(string, string) float64 { return .2 })) {
		t.Fatal("static prices and coefficient wrapper must preserve resolver precision")
	}
}
