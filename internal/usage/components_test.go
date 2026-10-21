package usage

import (
	"reflect"
	"strings"
	"testing"
)

func TestExpandEventUsesStableServerIdentityAndIndependentContext(t *testing.T) {
	event := Event{RecordID: "server-one", RequestID: "same-client-id", Model: "executor", PricingContext: &PricingContext{TotalInputTokens: 1000},
		Components: []Component{{Model: "advisor", IterationIndex: 7, PricingContext: &PricingContext{TotalInputTokens: 20}}}}
	rows := ExpandEvent(event)
	if len(rows) != 2 || rows[1].ParentRecordID != event.RecordID || rows[1].RequestID != event.RequestID || !rows[1].Supplementary || len(rows[1].Components) != 0 || rows[1].PricingContext.TotalInputTokens != 20 {
		t.Fatalf("expanded rows = %+v", rows)
	}
	if !reflect.DeepEqual(rows, ExpandEvent(event)) {
		t.Fatal("expansion changed across retries")
	}
	other := CloneEvent(event)
	other.RecordID = "server-two"
	if ExpandEvent(other)[1].RecordID == rows[1].RecordID {
		t.Fatal("client RequestID caused child identity collision")
	}
	event.PricingContext.TotalInputTokens = 1
	event.Components[0].PricingContext.TotalInputTokens = 2
	event.Components[0].Model = "changed"
	if rows[0].PricingContext.TotalInputTokens != 1000 || rows[0].Components[0].Model != "advisor" || rows[1].PricingContext.TotalInputTokens != 20 {
		t.Fatal("expanded bundle retained mutable caller evidence")
	}
}

func TestSupplementaryUsageCountsTokensWithoutAnotherRequest(t *testing.T) {
	var total Totals
	AddEventTotals(&total, Event{Model: "executor", InputTokens: 100}, nil)
	AddEventTotals(&total, Event{Model: "advisor", InputTokens: 200, Supplementary: true}, nil)
	if total.Requests != 1 || total.InputTokens != 300 {
		t.Fatalf("totals = %+v, want one request and both models' tokens", total)
	}
}

func TestNonBillableEventRetainsTokensWithoutCostOrMissingPrice(t *testing.T) {
	event := Event{Model: "advisor", InputTokens: 1_000_000, ImageInputTokens: 100, Supplementary: true, NonBillable: true}
	var total Totals
	AddEventTotals(&total, event, CostTable{"advisor": {InputPer1MTokens: 7}})
	if total.EstimatedCost != 0 || total.InputTokens != event.InputTokens || total.IncompleteCostCount != 0 {
		t.Fatalf("nonbillable totals = %+v", total)
	}
}

func TestSupplementaryMissingPriceIsIncompleteButLegacySemanticsRemain(t *testing.T) {
	event := Event{Model: "advisor", Supplementary: true, InputTokens: 100}
	CheckEventPrices(&event, CostTable{})
	if !event.CostIncomplete || !strings.Contains(event.UsageDiagnostic, "missing_iteration_price") {
		t.Fatalf("missing iteration price not diagnosed: %+v", event)
	}
	legacy := Event{Model: "unknown", InputTokens: 100}
	CheckEventPrices(&legacy, CostTable{})
	if legacy.CostIncomplete {
		t.Fatalf("changed legacy missing-price behavior: %+v", legacy)
	}
}

func TestFallbackRootMissingPriceIsIncomplete(t *testing.T) {
	event := Event{Model: "fallback", IterationType: "fallback_message", InputTokens: 100}
	CheckEventPrices(&event, CostTable{})
	if !event.CostIncomplete || !strings.Contains(event.UsageDiagnostic, "missing_iteration_price") {
		t.Fatalf("final fallback root price not diagnosed: %+v", event)
	}
	free := Event{Model: "fallback", IterationType: "fallback_message", NonBillable: true, InputTokens: 100}
	CheckEventPrices(&free, CostTable{})
	if free.CostIncomplete {
		t.Fatalf("explicitly free fallback requires no model price: %+v", free)
	}
}
