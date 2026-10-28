package quota

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestScopedCalibrationPreservesCurrentSpend(t *testing.T) {
	for _, tc := range []struct {
		name        string
		coefficient float64
		rate        float64
		unknown     bool
	}{
		{"old_paid", 1, 2, false},
		{"old_free_group", 0, 2, false},
		{"old_zero_price", 1, 0, false},
		{"unknown_paid", 1, 2, true},
		{"unknown_free_group", 0, 2, true},
		{"unknown_zero_price", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bundleManager(t, tc.coefficient)
			m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
				return []pricing.PricingTemplate{{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: tc.rate}}}, nil
			}, nil)
			current := quotaBundle()
			current.Components = nil
			if err := m.RecordUsage(m.usageStore, current); err != nil {
				t.Fatal(err)
			}
			want := tc.coefficient * tc.rate
			if got := bundleUsed(m); got != want {
				t.Fatalf("live spend = %v, want %v", got, want)
			}
			evidence := quotaBundle()
			evidence.Components = nil
			evidence.Timestamp = time.Now().UTC().AddDate(0, -2, -7)
			if tc.unknown {
				evidence.Timestamp = time.Time{}
			}
			if err := m.usageStore.RegisterUnresolvedUsage(evidence, []byte(tc.name)); err != nil {
				t.Fatal(err)
			}
			journal, err := nosql.NewUsageJournal(m.usageStore, filepath.Join(t.TempDir(), "usage.journal"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			m.usageJournal = journal
			m.Evaluate("alice")
			status := m.Status("alice")
			if (status.CalibrationError != "") != tc.unknown {
				t.Fatalf("calibration error = %q; want blocked = %v", status.CalibrationError, tc.unknown)
			}
			if got := bundleUsed(m); got != want {
				t.Errorf("spend after calibration = %v, want %v", got, want)
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked != tc.unknown || (blocked && info.Scope != ScopeAccounting) {
				t.Errorf("admission blocked = %v, info = %+v; want accounting blocked = %v", blocked, info, tc.unknown)
			}
		})
	}
}
