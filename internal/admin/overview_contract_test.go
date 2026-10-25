package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// TestOverviewTargetStatusJSONKeys locks the wire contract consumed by the
// overview target-health list: TargetStatus has no json tags, so its top-level
// keys are PascalCase, while the nested RuntimeWindow stays snake_case.
func TestOverviewTargetStatusJSONKeys(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(proxy.TargetStatus{
		Name:                      "contract-target",
		Phase:                     "ready",
		Paused:                    false,
		MutedUntil:                now,
		LastSuccess:               now,
		LastFailure:               now,
		ConsecutiveFailures:       1,
		TotalKeys:                 2,
		ActiveKeys:                1,
		ExhaustedSubscriptionKeys: 1,
		ExhaustedAPIKeys:          2,
		RateLimitedKeys:           3,
		BlockedKeys:               4,
		OtherExhaustedKeys:        5,
		Capacity:                  8,
		InFlight:                  2,
		Utilization:               0.25,
		Window15m: proxy.RuntimeWindow{
			InFlightPeak: 3,
			AffinityHits: 4,
			BreakerTrips: 5,
		},
	})
	if err != nil {
		t.Fatalf("marshal TargetStatus: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal TargetStatus: %v", err)
	}
	for _, key := range []string{
		"Name", "Phase", "Paused", "MutedUntil", "LastSuccess", "LastFailure",
		"ConsecutiveFailures", "TotalKeys", "ActiveKeys", "ExhaustedSubscriptionKeys",
		"ExhaustedAPIKeys", "RateLimitedKeys", "BlockedKeys", "OtherExhaustedKeys",
		"Capacity", "InFlight", "Utilization", "Window15m",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("overview target JSON missing PascalCase key %q", key)
		}
	}
	for _, key := range []string{"name", "phase", "total_keys", "active_keys", "window_15m"} {
		if _, ok := fields[key]; ok {
			t.Errorf("overview target JSON unexpectedly has snake_case top-level key %q", key)
		}
	}
	var window map[string]json.RawMessage
	if err := json.Unmarshal(fields["Window15m"], &window); err != nil {
		t.Fatalf("unmarshal Window15m: %v", err)
	}
	for _, key := range []string{"in_flight_peak", "affinity_hits", "breaker_trips"} {
		if _, ok := window[key]; !ok {
			t.Errorf("overview target Window15m missing snake_case key %q", key)
		}
	}
	if _, ok := window["InFlightPeak"]; ok {
		t.Error("overview target Window15m must not use PascalCase keys")
	}
}
