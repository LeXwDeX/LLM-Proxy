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

func TestBundlePricesNilResolverDoesNotDiagnoseMissingIterationPrice(t *testing.T) {
	for _, rootType := range []string{"", "fallback_message"} {
		for _, tc := range []struct {
			name     string
			resolver RateResolver
			missing  bool
		}{
			{name: "nil"},
			{name: "missing", resolver: CostTable{}, missing: true},
			{name: "priced", resolver: CostTable{"executor": {}, "advisor": {}}},
		} {
			t.Run(rootType+"/"+tc.name, func(t *testing.T) {
				event := Event{Model: "executor", IterationType: rootType, InputTokens: 100,
					Components: []Component{
						{Model: "advisor", IterationType: "advisor", InputTokens: 200},
						{Model: "free", IterationType: "fallback_message", InputTokens: 300, NonBillable: true},
					}}
				CheckBundlePrices(&event, tc.resolver)
				var total Totals
				for _, row := range ExpandEvent(event) {
					wantMissing := tc.missing && !row.NonBillable && (row.Supplementary || rootType == "fallback_message")
					if row.CostIncomplete != wantMissing || strings.Contains(row.UsageDiagnostic, "missing_iteration_price") != wantMissing {
						t.Errorf("row=%+v, want missing price=%t", row, wantMissing)
					}
					AddEventTotals(&total, row, tc.resolver)
				}
				var wantIncomplete int64
				if tc.missing {
					wantIncomplete = 1
					if rootType == "fallback_message" {
						wantIncomplete++
					}
				}
				if total.IncompleteCostCount != wantIncomplete || total.Requests != 1 || total.InputTokens != 600 {
					t.Fatalf("totals=%+v, want incomplete=%d, one request and all tokens", total, wantIncomplete)
				}
			})
		}
	}
}

func TestNilResolverRetainsImagePriceAndPartitionChecks(t *testing.T) {
	for _, free := range []bool{false, true} {
		for _, imageTokens := range []int64{50, 150} {
			event := Event{Model: "advisor", Supplementary: true, NonBillable: free,
				InputTokens: 100, ImageInputTokens: imageTokens}
			CheckEventPrices(&event, nil)
			conflict := imageTokens > event.InputTokens
			if event.ImageInputTokens != min(imageTokens, event.InputTokens) ||
				(event.UsageStatus == UsageStatusConflict) != conflict ||
				strings.Contains(event.UsageDiagnostic, "image_partition_exceeds_total") != conflict ||
				strings.Contains(event.UsageDiagnostic, "missing_image_input_price") != !free ||
				strings.Contains(event.UsageDiagnostic, "missing_iteration_price") ||
				event.CostIncomplete != (!free || conflict) {
				t.Errorf("free=%t imageTokens=%d: image checks changed: %+v", free, imageTokens, event)
			}
		}
	}
}
