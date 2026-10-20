package usage

import (
	"encoding/json"
	"testing"
)

func TestPricingContextRoundTripPreservesObservation(t *testing.T) {
	original := Event{PricingContext: &PricingContext{Version: 1, TotalInputTokens: 0, TotalInputKnown: true, TotalInputValid: true, RequestServiceTier: PricingValue{State: PricingValueNull}, ResponseServiceTier: PricingValue{Raw: " priority ", State: PricingValueValue}, ActualServiceTier: "fast", ResponseSpeed: PricingValue{State: PricingValueEmpty}}, PricingRuleID: "p/fast", PricingRuleVersion: 3}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PricingContext == nil || !decoded.PricingContext.TotalInputKnown || !decoded.PricingContext.TotalInputValid || decoded.PricingContext.TotalInputTokens != 0 || decoded.PricingContext.RequestServiceTier.State != PricingValueNull || decoded.PricingContext.ResponseSpeed.State != PricingValueEmpty || decoded.PricingContext.ResponseServiceTier.Raw != " priority " || decoded.PricingRuleID != "p/fast" || decoded.PricingRuleVersion != 3 {
		t.Fatalf("roundtrip=%+v", decoded)
	}
	var legacy Event
	if err = json.Unmarshal([]byte(`{"input_tokens":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.PricingContext != nil {
		t.Fatal("legacy event invented context")
	}
}

func TestEventPriceChecksPreserveExistingDiagnostics(t *testing.T) {
	evt := Event{CostIncomplete: true, UsageDiagnostic: "existing"}
	resolver := CostTable{"m": {InputPer1MTokens: 1}}
	evt.Model = "m"
	CheckEventPrices(&evt, resolver)
	CheckEventPrices(&evt, resolver)
	if !evt.CostIncomplete || evt.UsageDiagnostic != "existing" {
		t.Fatalf("event=%+v", evt)
	}
}
