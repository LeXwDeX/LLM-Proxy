package usage

import (
	"strconv"

	"github.com/google/uuid"
)

// Component is additional model usage belonging to one request. It has no
// request identity or nested components; the parent supplies those fields when
// the durable bundle is expanded. Pricing evidence belongs to this model alone.
type Component struct {
	Model                   string          `json:"model"`
	IterationType           string          `json:"iteration_type"`
	IterationIndex          int             `json:"iteration_index"`
	NonBillable             bool            `json:"non_billable,omitempty"`
	PricingContext          *PricingContext `json:"pricing_context,omitempty"`
	PricingRuleID           string          `json:"pricing_rule_id,omitempty"`
	PricingRuleVersion      int             `json:"pricing_rule_version,omitempty"`
	UsageProtocol           string          `json:"usage_protocol,omitempty"`
	ResponseModel           string          `json:"response_model,omitempty"`
	ObservedFields          uint16          `json:"observed_fields,omitempty"`
	UsageStatus             string          `json:"usage_status,omitempty"`
	UsageAdapter            string          `json:"usage_adapter,omitempty"`
	CostIncomplete          bool            `json:"cost_incomplete,omitempty"`
	UsageDiagnostic         string          `json:"usage_diagnostic,omitempty"`
	ResponseStatus          string          `json:"response_status,omitempty"`
	FinishReason            string          `json:"finish_reason,omitempty"`
	InputTokens             int64           `json:"input_tokens"`
	OutputTokens            int64           `json:"output_tokens"`
	CachedTokens            int64           `json:"cached_tokens"`
	CacheCreationTokens     int64           `json:"cache_creation_tokens,omitempty"`
	ExplicitCacheReadTokens int64           `json:"explicit_cache_read_tokens,omitempty"`
	ReasoningTokens         int64           `json:"reasoning_tokens,omitempty"`
	CacheCreation5mTokens   int64           `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens   int64           `json:"cache_creation_1h_tokens,omitempty"`
	ToolUsePromptTokens     int64           `json:"tool_use_prompt_tokens,omitempty"`
	ImageInputTokens        int64           `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens  int64           `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens       int64           `json:"image_output_tokens,omitempty"`
}

// CloneEvent isolates a record from caller mutations while the journal and
// asynchronous writer retain it. PricingContext contains only value fields.
func CloneEvent(event Event) Event {
	if event.PricingContext != nil {
		context := *event.PricingContext
		event.PricingContext = &context
	}
	if event.Components != nil {
		event.Components = append([]Component{}, event.Components...)
		for i := range event.Components {
			if event.Components[i].PricingContext != nil {
				context := *event.Components[i].PricingContext
				event.Components[i].PricingContext = &context
			}
		}
	}
	return event
}

// RequestWeight preserves legacy rows' request count and excludes additional
// model usage from counting the same HTTP request again.
func RequestWeight(event Event) int64 {
	if event.Supplementary {
		return 0
	}
	return 1
}

// ExpandEvent returns the parent followed by its non-recursive components.
// The parent retains all component evidence for whole-bundle idempotency.
// RecordID must already be the parent's stable server-generated identity;
// RequestID is client-controlled and never identifies component rows.
func ExpandEvent(event Event) []Event {
	event = CloneEvent(event)
	rows := make([]Event, 1, 1+len(event.Components))
	rows[0] = event
	for _, component := range event.Components {
		rows = append(rows, componentEvent(event, component))
	}
	return rows
}

func componentEvent(parent Event, component Component) Event {
	event := Event{
		Timestamp: parent.Timestamp, ClientName: parent.ClientName,
		EndpointType: parent.EndpointType, RequestID: parent.RequestID,
		Target: parent.Target, Path: parent.Path, StatusCode: parent.StatusCode,
		Supplementary: true, ParentRecordID: parent.RecordID,
		RecordID:                uuid.NewSHA1(uuid.NameSpaceOID, []byte(parent.RecordID+"/iteration/"+strconv.Itoa(component.IterationIndex))).String(),
		Model:                   component.Model,
		IterationType:           component.IterationType,
		IterationIndex:          component.IterationIndex,
		NonBillable:             component.NonBillable,
		PricingContext:          component.PricingContext,
		PricingRuleID:           component.PricingRuleID,
		PricingRuleVersion:      component.PricingRuleVersion,
		UsageProtocol:           component.UsageProtocol,
		ResponseModel:           component.ResponseModel,
		ObservedFields:          component.ObservedFields,
		UsageStatus:             component.UsageStatus,
		UsageAdapter:            component.UsageAdapter,
		CostIncomplete:          component.CostIncomplete,
		UsageDiagnostic:         component.UsageDiagnostic,
		ResponseStatus:          component.ResponseStatus,
		FinishReason:            component.FinishReason,
		InputTokens:             component.InputTokens,
		OutputTokens:            component.OutputTokens,
		CachedTokens:            component.CachedTokens,
		CacheCreationTokens:     component.CacheCreationTokens,
		ExplicitCacheReadTokens: component.ExplicitCacheReadTokens,
		ReasoningTokens:         component.ReasoningTokens,
		CacheCreation5mTokens:   component.CacheCreation5mTokens,
		CacheCreation1hTokens:   component.CacheCreation1hTokens,
		ToolUsePromptTokens:     component.ToolUsePromptTokens,
		ImageInputTokens:        component.ImageInputTokens,
		CachedImageInputTokens:  component.CachedImageInputTokens,
		ImageOutputTokens:       component.ImageOutputTokens,
	}
	return CloneEvent(event)
}

// CheckBundlePrices checks each row using that model's independent evidence.
// Ordinary CheckEventPrices and EstimateEventCost only price the current row.
func CheckBundlePrices(event *Event, resolver RateResolver) {
	CheckEventPrices(event, resolver)
	for i, component := range event.Components {
		row := componentEvent(*event, component)
		CheckEventPrices(&row, resolver)
		event.Components[i] = Component{
			Model:                   row.Model,
			IterationType:           row.IterationType,
			IterationIndex:          row.IterationIndex,
			NonBillable:             row.NonBillable,
			PricingContext:          row.PricingContext,
			PricingRuleID:           row.PricingRuleID,
			PricingRuleVersion:      row.PricingRuleVersion,
			UsageProtocol:           row.UsageProtocol,
			ResponseModel:           row.ResponseModel,
			ObservedFields:          row.ObservedFields,
			UsageStatus:             row.UsageStatus,
			UsageAdapter:            row.UsageAdapter,
			CostIncomplete:          row.CostIncomplete,
			UsageDiagnostic:         row.UsageDiagnostic,
			ResponseStatus:          row.ResponseStatus,
			FinishReason:            row.FinishReason,
			InputTokens:             row.InputTokens,
			OutputTokens:            row.OutputTokens,
			CachedTokens:            row.CachedTokens,
			CacheCreationTokens:     row.CacheCreationTokens,
			ExplicitCacheReadTokens: row.ExplicitCacheReadTokens,
			ReasoningTokens:         row.ReasoningTokens,
			CacheCreation5mTokens:   row.CacheCreation5mTokens,
			CacheCreation1hTokens:   row.CacheCreation1hTokens,
			ToolUsePromptTokens:     row.ToolUsePromptTokens,
			ImageInputTokens:        row.ImageInputTokens,
			CachedImageInputTokens:  row.CachedImageInputTokens,
			ImageOutputTokens:       row.ImageOutputTokens,
		}
	}
}
