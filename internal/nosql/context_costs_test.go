package nosql

import (
	"encoding/json"
	"errors"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type contextCostResolver struct{}

func (contextCostResolver) RateFor(string, string, time.Time) (usage.CostRates, bool) {
	return usage.CostRates{InputPer1MTokens: 1}, true
}
func (contextCostResolver) RequiresEventTimestamps() bool { return true }
func (contextCostResolver) ResolveEventRate(e usage.Event) usage.RateResolution {
	rate := 1.0
	if e.PricingContext != nil && e.PricingContext.TotalInputTokens > 100 {
		rate = 2
	}
	return usage.RateResolution{Rates: usage.CostRates{InputPer1MTokens: rate}, Found: true, Complete: true}
}
func TestContextCostsKeepRequestsSeparateAndRejectPrunedHistory(t *testing.T) {
	store := NewUsageStore(testDB(t))
	at := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	for _, total := range []int64{100, 101} {
		e := usage.Event{Timestamp: at, ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: total, TotalInputKnown: true, TotalInputValid: true}}
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	end := at.Add(time.Hour)
	filter := usage.Filter{From: &at, To: &end, ClientName: "alice"}
	got, err := store.SumTargetModelCostsRange(filter, contextCostResolver{})
	if err != nil || len(got) != 1 || got[0].CostUSD != 3 {
		t.Fatalf("cost=%v err=%v, want separate request total 3", got, err)
	}
	if _, err := store.PruneEventsBefore(end, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SumTargetModelCostsRange(filter, contextCostResolver{}); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("pruned conditional history err=%v", err)
	}
}

func TestContextTemplateStorageRoundTripAndValidation(t *testing.T) {
	store := NewPricingTemplateStore(testDB(t))
	var template pricing.PricingTemplate
	err := json.Unmarshal([]byte(`{"target":"upstream","model":"qwen-max","version":7,"start":"2026-01-01T00:00:00Z","mode":"schedule","timezone":"+08:00","base_rates":{"input_per_1m_tokens":1},"conditions":[{"id":"long","min_input_tokens":101,"rates":{"input_per_1m_tokens":2}}],"periods":[{"weekdays":[1],"start":"09:00","end":"10:00","rates":{"input_per_1m_tokens":3},"conditions":[{"id":"fast","speed":"fast","rates":{"input_per_1m_tokens":4}}]}],"date_exceptions":[{"start_date":"2026-10-01","end_date":"2026-10-08","rates":{"input_per_1m_tokens":5},"conditions":[{"id":"priority","service_tier":"priority","rates":{"input_per_1m_tokens":6}}]}]}`), &template)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Upsert(template)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(saved.ID)
	if err != nil || !reflect.DeepEqual(*got, saved) {
		t.Fatalf("roundtrip got=%+v err=%v", got, err)
	}
	list, err := store.ListByTargetModel("upstream", "qwen-max")
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], saved) {
		t.Fatalf("list=%v err=%v", list, err)
	}
	template.Conditions[0].MinInputTokens = nil
	if _, err := store.Upsert(template); err == nil {
		t.Fatal("accepted condition with no constraint")
	}
}

func TestContextJournalReplayPreservesEvidenceAndDeduplicates(t *testing.T) {
	store := NewUsageStore(testDB(t))
	primary := &commitThenFailUsageRecorder{next: store, err: errors.New("commit acknowledgement lost"), failFirst: true}
	journal, err := NewUsageJournal(primary, filepath.Join(t.TempDir(), "journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, PricingRuleID: "long", PricingRuleVersion: 7, CostIncomplete: true, UsageDiagnostic: "missing_service_tier", PricingContext: &usage.PricingContext{Version: 1, TotalInputTokens: 101, TotalInputKnown: true, TotalInputValid: true, ResponseServiceTier: usage.PricingValue{Raw: "priority", State: "value"}}}
	if err := journal.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Replay(); err != nil {
		t.Fatal(err)
	}
	events, err := store.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	got := events[0]
	if !reflect.DeepEqual(got.PricingContext, event.PricingContext) || got.PricingRuleID != "long" || got.PricingRuleVersion != 7 || !got.CostIncomplete || got.UsageDiagnostic != event.UsageDiagnostic {
		t.Fatalf("evidence lost: %+v", got)
	}
	costs, err := store.SumTargetModelCostsRange(usage.Filter{ClientName: "alice"}, contextCostResolver{})
	if err != nil || len(costs) != 1 || costs[0].CostUSD != 2 {
		t.Fatalf("replay double charge: %v err=%v", costs, err)
	}
}
