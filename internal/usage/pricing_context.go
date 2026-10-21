package usage

// PricingValue preserves the upstream spelling and observation state. An empty
// State is legacy missing; null and an explicit empty string are not values.
type PricingValue struct {
	Raw   string `json:"raw,omitempty"`
	State string `json:"state,omitempty"`
}

const (
	PricingValueMissing   = "missing"
	PricingValueNull      = "null"
	PricingValueEmpty     = "empty"
	PricingValueValue     = "value"
	PricingValueUnknown   = "unknown"
	PricingContextVersion = 1
)

// PricingContext contains billing evidence, not a request-side price estimate.
// Actual fields are protocol-normalized response values. Requested values must
// never be used to infer the service tier that actually executed.
type PricingContext struct {
	Version             int          `json:"version"`
	TotalInputTokens    int64        `json:"total_input_tokens"`
	TotalInputKnown     bool         `json:"total_input_known"`
	TotalInputValid     bool         `json:"total_input_valid"`
	RequestServiceTier  PricingValue `json:"request_service_tier"`
	ResponseServiceTier PricingValue `json:"response_service_tier"`
	RequestSpeed        PricingValue `json:"request_speed"`
	ResponseSpeed       PricingValue `json:"response_speed"`
	ActualServiceTier   string       `json:"actual_service_tier,omitempty"`
	ActualSpeed         string       `json:"actual_speed,omitempty"`
	Diagnostic          string       `json:"diagnostic,omitempty"`
}

// RateResolution identifies the rule used for this estimate. Rule IDs and
// versions are evidence, not immutable snapshots of editable pricing templates.
type RateResolution struct {
	Rates      CostRates
	Found      bool
	Complete   bool
	Diagnostic string
	RuleID     string
	Version    int
}

type EventRateResolver interface {
	ResolveEventRate(Event) RateResolution
}

// ResolveEventRate retains compatibility with resolvers using target/model/time.
func ResolveEventRate(evt Event, resolver RateResolver) RateResolution {
	if resolver == nil {
		return RateResolution{Complete: true}
	}
	if aware, ok := resolver.(EventRateResolver); ok {
		return aware.ResolveEventRate(evt)
	}
	rates, found := resolver.RateFor(evt.Target, evt.Model, evt.Timestamp)
	return RateResolution{Rates: rates, Found: found, Complete: true}
}

// CheckEventPrices records incomplete estimates without refusing usage records.
func CheckEventPrices(evt *Event, resolver RateResolver) {
	if evt.NonBillable {
		NormalizeImagePartitions(evt)
		return
	}
	resolution := ResolveEventRate(*evt, resolver)
	evt.PricingRuleID, evt.PricingRuleVersion = resolution.RuleID, resolution.Version
	if resolver != nil && (evt.Supplementary || evt.IterationType == "fallback_message") && !resolution.Found {
		evt.CostIncomplete = true
		appendUsageDiagnostic(evt, "missing_iteration_price")
	}
	if !resolution.Complete {
		evt.CostIncomplete = true
		appendUsageDiagnostic(evt, resolution.Diagnostic)
	}
	CheckImagePrices(evt, resolver)
}

func (c coefficientResolver) ResolveEventRate(evt Event) RateResolution {
	result := ResolveEventRate(evt, c.inner)
	factor := c.fn(evt.Target, evt.Model)
	if factor < 0 {
		factor = 1
	}
	result.Rates = result.Rates.Scaled(factor)
	return result
}
