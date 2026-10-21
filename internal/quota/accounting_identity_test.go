package quota

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestRecordUsageRejectsExplicitIdentityWithoutTimestampWithoutSideEffects(t *testing.T) {
	m := bundleManager(t, 1)
	before := bundleUsed(m)
	event := quotaBundle()
	event.Timestamp = time.Time{}

	if err := m.RecordUsage(m.usageStore, event); err == nil {
		t.Fatal("quota accepted explicit record ID without a timestamp")
	}
	if got := bundleUsed(m); got != before {
		t.Fatalf("rejected identity charged quota: got=%v want=%v", got, before)
	}
	if status := m.Status("alice"); status.CalibrationError != "" {
		t.Fatalf("rejected identity set calibration error: %q", status.CalibrationError)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
		t.Fatalf("rejected identity blocked admission: %+v", info)
	}
	rows, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected identity wrote usage rows=%d err=%v", len(rows), err)
	}
}

func TestRecordUsageAllowsGeneratedIdentityWithoutTimestamp(t *testing.T) {
	m := bundleManager(t, 1)
	event := quotaBundle()
	event.RecordID = ""
	event.Timestamp = time.Time{}

	for i := 0; i < 2; i++ {
		if err := m.RecordUsage(m.usageStore, event); err != nil {
			t.Fatalf("ordinary generated identity %d: %v", i, err)
		}
	}
	if got := bundleUsed(m); got != 24 {
		t.Fatalf("ordinary events were not independent: used=%v want=24", got)
	}
	if status := m.Status("alice"); status.CalibrationError != "" {
		t.Fatalf("ordinary events set calibration error: %q", status.CalibrationError)
	}
	rows, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 6 {
		t.Fatalf("ordinary bundle rows=%d err=%v", len(rows), err)
	}
}

func TestRecordUsageRetryIgnoresPricingRuleVersion(t *testing.T) {
	m := bundleManager(t, 1)
	version := 1
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{
			{ID: "executor-rate", Version: version, Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
			{ID: "advisor-rate", Version: version, Target: "anthropic", Model: "advisor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}},
		}, nil
	}, nil)
	event := quotaBundle()
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	before := bundleUsed(m)
	version = 2
	m.costCache.Invalidate()
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatalf("version-only retry: %v", err)
	}
	if got := bundleUsed(m); got != before {
		t.Fatalf("version-only retry changed charge: got=%v want=%v", got, before)
	}
	if status := m.Status("alice"); status.CalibrationError != "" {
		t.Fatalf("version-only retry set calibration error: %q", status.CalibrationError)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
		t.Fatalf("version-only retry blocked admission: %+v", info)
	}
	rows, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 3 {
		t.Fatalf("version-only retry rows=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.Supplementary && row.NonBillable {
			continue
		}
		if row.PricingRuleVersion != 1 {
			t.Fatalf("retry overwrote first accepted pricing evidence: %+v", row)
		}
	}

	changed := usage.CloneEvent(event)
	changed.Components[0].InputTokens++
	if err := m.RecordUsage(m.usageStore, changed); err == nil {
		t.Fatal("changed component payload was accepted")
	}
	if got := bundleUsed(m); got != before {
		t.Fatalf("conflict changed charge: got=%v want=%v", got, before)
	}
	rows, err = m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(rows) != 3 {
		t.Fatalf("conflict changed rows=%d err=%v", len(rows), err)
	}

	changedContext := usage.CloneEvent(event)
	changedContext.Components[1].PricingContext = &usage.PricingContext{Version: 1, TotalInputTokens: 9, TotalInputKnown: true}
	if err := m.RecordUsage(m.usageStore, changedContext); err == nil {
		t.Fatal("changed component pricing context was accepted")
	}
	if got := bundleUsed(m); got != before {
		t.Fatalf("component context conflict changed charge: got=%v want=%v", got, before)
	}
}
