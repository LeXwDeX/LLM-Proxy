package quota

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"strings"
	"testing"
	"time"
)

func TestContextAccountingMatchesCalibrationAndPreservesEvidence(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: .5, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	var template pricing.PricingTemplate
	if err := json.Unmarshal([]byte(`{"target":"upstream","model":"qwen-max","mode":"flat","base_rates":{"input_per_1m_tokens":1},"conditions":[{"id":"long","min_input_tokens":101,"rates":{"input_per_1m_tokens":2}}]}`), &template); err != nil {
		t.Fatal(err)
	}
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) { return []pricing.PricingTemplate{template}, nil }, nil)
	m.Evaluate("alice")
	at := time.Now().UTC()
	for _, total := range []int64{100, 101} {
		event := usage.Event{Timestamp: at, ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: total, TotalInputKnown: true, TotalInputValid: true}}
		if err := m.RecordUsage(m.usageStore, event); err != nil {
			t.Fatal(err)
		}
	}
	before := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
	m.Evaluate("alice")
	after := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used
	if before != 1.5 || after != 1.5 {
		t.Fatalf("before=%v after=%v, want single coefficient charge 1.5", before, after)
	}
	events, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	for _, e := range events {
		if e.PricingContext == nil || e.PricingContext.TotalInputTokens == 0 {
			t.Fatalf("lost context: %+v", e)
		}
	}
}

func TestContextAccountingPersistsMissingEvidenceDiagnostic(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	var template pricing.PricingTemplate
	if err := json.Unmarshal([]byte(`{"target":"upstream","model":"qwen-max","version":3,"mode":"flat","base_rates":{"input_per_1m_tokens":1},"conditions":[{"id":"long","min_input_tokens":101,"rates":{"input_per_1m_tokens":2}}]}`), &template); err != nil {
		t.Fatal(err)
	}
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) { return []pricing.PricingTemplate{template}, nil }, nil)
	e := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	if err := m.RecordUsage(m.usageStore, e); err != nil {
		t.Fatal(err)
	}
	events, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	got := events[0]
	if !got.CostIncomplete || !strings.Contains(got.UsageDiagnostic, "pricing_context") || got.PricingRuleVersion != 3 {
		t.Fatalf("missing context not diagnosed: %+v", got)
	}
}
