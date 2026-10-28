package usage

import (
	"sort"
	"strings"
	"time"
)

const (
	UsageStatusComplete   = "complete"
	UsageStatusIncomplete = "incomplete"
	UsageStatusConflict   = "conflict"
	// Fallback describes legacy adapter provenance, not missing usage.
	UsageStatusFallback = "fallback"
)

// Usage field bits are a persisted schema: existing bit positions must never
// change. A set bit means the corresponding upstream counter was observed,
// including an explicit zero. An unset bit means unreported (or legacy unknown),
// not zero consumption. Derived arithmetic does not create observation bits.
// ExplicitCacheRead is the classification exception: its bit records that the
// actual request and verified model capability establish the cache read class.
const (
	UsageFieldInput             uint16 = 1 << 0
	UsageFieldOutput            uint16 = 1 << 1
	UsageFieldCached            uint16 = 1 << 2
	UsageFieldCacheCreation     uint16 = 1 << 3
	UsageFieldExplicitCacheRead uint16 = 1 << 4
	UsageFieldReasoning         uint16 = 1 << 5
	UsageFieldCacheCreation5m   uint16 = 1 << 6
	UsageFieldCacheCreation1h   uint16 = 1 << 7
	UsageFieldToolUsePrompt     uint16 = 1 << 8
	UsageFieldImageInput        uint16 = 1 << 9
	UsageFieldCachedImageInput  uint16 = 1 << 10
	UsageFieldImageOutput       uint16 = 1 << 11
)

// Event is one usage record item.
type Event struct {
	Components              []Component     `json:"components,omitempty"`
	Supplementary           bool            `json:"supplementary,omitempty"`
	ParentRecordID          string          `json:"parent_record_id,omitempty"`
	IterationType           string          `json:"iteration_type,omitempty"`
	IterationIndex          int             `json:"iteration_index,omitempty"`
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
	Timestamp               time.Time       `json:"timestamp"`
	RecordID                string          `json:"record_id,omitempty"`
	ClientName              string          `json:"client_name"`
	EndpointType            string          `json:"endpoint_type,omitempty"`
	Model                   string          `json:"model,omitempty"`
	InputTokens             int64           `json:"input_tokens"`
	OutputTokens            int64           `json:"output_tokens"`
	CachedTokens            int64           `json:"cached_tokens"`
	CacheCreationTokens     int64           `json:"cache_creation_tokens,omitempty"`
	ExplicitCacheReadTokens int64           `json:"explicit_cache_read_tokens,omitempty"`
	ReasoningTokens         int64           `json:"reasoning_tokens,omitempty"`
	CacheCreation5mTokens   int64           `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens   int64           `json:"cache_creation_1h_tokens,omitempty"`
	ToolUsePromptTokens     int64           `json:"tool_use_prompt_tokens,omitempty"`
	// ImageInputTokens excludes cached images, like InputTokens excludes cached input.
	ImageInputTokens       int64  `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens int64  `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens      int64  `json:"image_output_tokens,omitempty"`
	RequestID              string `json:"request_id,omitempty"`
	Target                 string `json:"target,omitempty"`
	Path                   string `json:"path,omitempty"`
	StatusCode             int    `json:"status_code"`
}

// Filter controls list/aggregate query range.
type Filter struct {
	From        *time.Time
	To          *time.Time
	ClientName  string
	Model       string
	ModelGroups []string
	Limit       int
}

// CostRates maps model to token prices (per 1M tokens).
type CostRates struct {
	InputPer1MTokens            float64  `json:"input_per_1m_tokens"`
	OutputPer1MTokens           float64  `json:"output_per_1m_tokens"`
	CacheWritePer1MToken        float64  `json:"cache_write_per_1m_tokens"`
	CacheReadPer1MToken         float64  `json:"cache_read_per_1m_tokens"`
	ExplicitCacheReadPer1MToken *float64 `json:"explicit_cache_read_per_1m_tokens,omitempty"`
	CacheWrite5mPer1MToken      *float64 `json:"cache_write_5m_per_1m_tokens,omitempty"`
	CacheWrite1hPer1MToken      *float64 `json:"cache_write_1h_per_1m_tokens,omitempty"`
	ImageInputPer1MTokens       *float64 `json:"image_input_per_1m_tokens,omitempty"`
	CachedImageInputPer1MTokens *float64 `json:"cached_image_input_per_1m_tokens,omitempty"`
	ImageOutputPer1MTokens      *float64 `json:"image_output_per_1m_tokens,omitempty"`
}

// CostFor computes the USD cost for a single event's token counts using these
// rates. Single source of truth for the cost formula — all call sites
// (EstimateEventCost, quota.Manager.computeCost/Evaluate/Status, nosql agg)
// MUST delegate here so a future rate/field change touches exactly one place.
// Optional partitions are explicit cache reads, 5m writes, 1h writes, noncached
// image input, cached image input, and image output. Each is a subset of its
// corresponding total, never additional tokens. Cached image input takes
// precedence over explicit text cache reads when the subsets overlap.
// Missing image rates use provisional text estimates; callers must also use
// MissingImagePrice/CheckImagePrices to record that the bill is incomplete.
func (r CostRates) CostFor(input, output, cached, cacheCreation int64, partitions ...int64) float64 {
	subset := func(i int, total int64) int64 {
		if i >= len(partitions) {
			return 0
		}
		return max(0, min(partitions[i], total))
	}
	image := subset(3, input)
	cachedImage := subset(4, cached)
	imageOut := subset(5, output)
	explicit := subset(0, cached-cachedImage)
	five := subset(1, cacheCreation)
	hour := subset(2, cacheCreation-five)
	imageRate := optionalRate(r.ImageInputPer1MTokens, r.InputPer1MTokens)
	return (float64(input-image)*r.InputPer1MTokens + float64(output-imageOut)*r.OutputPer1MTokens +
		float64(image)*imageRate + float64(cachedImage)*optionalRate(r.CachedImageInputPer1MTokens, imageRate) +
		float64(imageOut)*optionalRate(r.ImageOutputPer1MTokens, r.OutputPer1MTokens) +
		float64(cached-cachedImage-explicit)*r.CacheReadPer1MToken + float64(explicit)*optionalRate(r.ExplicitCacheReadPer1MToken, r.CacheReadPer1MToken) +
		float64(cacheCreation-five-hour)*r.CacheWritePer1MToken + float64(five)*optionalRate(r.CacheWrite5mPer1MToken, r.CacheWritePer1MToken) +
		float64(hour)*optionalRate(r.CacheWrite1hPer1MToken, r.CacheWritePer1MToken)) / 1_000_000
}

func optionalRate(p *float64, fallback float64) float64 {
	if p != nil {
		return *p
	}
	return fallback
}

// CloneRate copies an optional price without conflating absence with free usage.
func CloneRate(p *float64) *float64 {
	if p == nil {
		return nil
	}
	value := *p
	return &value
}

func scaleRate(p *float64, factor float64) *float64 {
	if p == nil {
		return nil
	}
	value := *p * factor
	return &value
}

// CostTable holds rates keyed by model name.
type CostTable map[string]CostRates

// RateResolver resolves the token cost rates for a given upstream target and
// model at a specific instant. The (target, model) pair is the pricing key;
// at is the historical instant (event timestamp or UTC hour bucket) used to
// select a time-based pricing template. Implementations include pricing.Resolver
// (time-based) and CostTable (model-only, ignores target/time).
type RateResolver interface {
	RateFor(target, model string, at time.Time) (CostRates, bool)
}

// RequiresEventTimestamps reports whether a resolver can change prices within
// one UTC hour. Unknown implementations are conservative: an hourly rollup
// cannot safely replace their event timestamps.
func RequiresEventTimestamps(resolver RateResolver) bool {
	if resolver == nil {
		return false
	}
	if aligned, ok := resolver.(interface{ RequiresEventTimestamps() bool }); ok {
		return aligned.RequiresEventTimestamps()
	}
	return true
}

func (ct CostTable) RequiresEventTimestamps() bool { return false }

func RequiresEventTimestampsFor(resolver RateResolver, target, model string) bool {
	if scoped, ok := resolver.(interface{ RequiresEventTimestampsFor(string, string) bool }); ok {
		return scoped.RequiresEventTimestampsFor(target, model)
	}
	return RequiresEventTimestamps(resolver)
}

// RequiresEventTimestampsForModel covers every target, including global rules.
// Resolvers without this optional capability use their global requirement.
func RequiresEventTimestampsForModel(resolver RateResolver, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return RequiresEventTimestamps(resolver)
	}
	if scoped, ok := resolver.(interface{ RequiresEventTimestampsForModel(string) bool }); ok {
		return scoped.RequiresEventTimestampsForModel(model)
	}
	return RequiresEventTimestamps(resolver)
}

// LookupCost finds cost rates by model name only.
func (ct CostTable) LookupCost(model string) (CostRates, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return CostRates{}, false
	}
	rates, ok := ct[model]
	return rates.Scaled(1), ok
}

// RateFor implements RateResolver for the legacy model-only CostTable: it
// ignores target and at, delegating to LookupCost(model).
func (ct CostTable) RateFor(target, model string, at time.Time) (CostRates, bool) {
	return ct.LookupCost(model)
}

// Scaled returns a copy of the rates with every per-1M price multiplied by
// factor. CostFor is linear in the prices, so scaling rates is equivalent to
// scaling the final cost and keeps the cost formula SSOT in CostFor.
func (r CostRates) Scaled(factor float64) CostRates {
	return CostRates{
		InputPer1MTokens:            r.InputPer1MTokens * factor,
		OutputPer1MTokens:           r.OutputPer1MTokens * factor,
		CacheWritePer1MToken:        r.CacheWritePer1MToken * factor,
		CacheReadPer1MToken:         r.CacheReadPer1MToken * factor,
		ExplicitCacheReadPer1MToken: scaleRate(r.ExplicitCacheReadPer1MToken, factor),
		CacheWrite5mPer1MToken:      scaleRate(r.CacheWrite5mPer1MToken, factor),
		CacheWrite1hPer1MToken:      scaleRate(r.CacheWrite1hPer1MToken, factor),
		ImageInputPer1MTokens:       scaleRate(r.ImageInputPer1MTokens, factor),
		CachedImageInputPer1MTokens: scaleRate(r.CachedImageInputPer1MTokens, factor),
		ImageOutputPer1MTokens:      scaleRate(r.ImageOutputPer1MTokens, factor),
	}
}

// CoefficientFunc resolves the model-group cost coefficient for a (target,
// model) pair. Zero means free usage; negative values are invalid and fall
// back to 1 defensively.
type CoefficientFunc func(target, model string) float64

// WithCoefficient wraps a RateResolver so every resolved CostRates is scaled
// by the model-group coefficient fn resolves for that (target, model). It is
// the single exit where statistics apply the group discount, mirroring how
// quota.Manager scales consumption, so report costs and quota deductions stay
// on the same discounted basis.
func WithCoefficient(inner RateResolver, fn CoefficientFunc) RateResolver {
	if inner == nil || fn == nil {
		return inner
	}
	return coefficientResolver{inner: inner, fn: fn}
}

type coefficientResolver struct {
	inner RateResolver
	fn    CoefficientFunc
}

func (c coefficientResolver) RequiresEventTimestamps() bool {
	return RequiresEventTimestamps(c.inner)
}

func (c coefficientResolver) RequiresEventTimestampsFor(target, model string) bool {
	return RequiresEventTimestampsFor(c.inner, target, model)
}

func (c coefficientResolver) RequiresEventTimestampsForModel(model string) bool {
	return RequiresEventTimestampsForModel(c.inner, model)
}

func (c coefficientResolver) RateFor(target, model string, at time.Time) (CostRates, bool) {
	rates, ok := c.inner.RateFor(target, model, at)
	if !ok {
		return rates, false
	}
	factor := c.fn(target, model)
	if factor < 0 {
		factor = 1
	}
	return rates.Scaled(factor), true
}

// Totals is aggregated usage and estimated cost. The incomplete counts are
// per stored record, not per request: a request whose fallback attempt left a
// zero-output record contributes two records.
type Totals struct {
	IncompleteUsageCount    int64   `json:"incomplete_usage_count,omitempty"`
	IncompleteCostCount     int64   `json:"incomplete_cost_count,omitempty"`
	Requests                int64   `json:"requests"`
	InputTokens             int64   `json:"input_tokens"`
	OutputTokens            int64   `json:"output_tokens"`
	CachedTokens            int64   `json:"cached_tokens"`
	CacheCreationTokens     int64   `json:"cache_creation_tokens"`
	ExplicitCacheReadTokens int64   `json:"explicit_cache_read_tokens,omitempty"`
	ReasoningTokens         int64   `json:"reasoning_tokens,omitempty"`
	CacheCreation5mTokens   int64   `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens   int64   `json:"cache_creation_1h_tokens,omitempty"`
	ToolUsePromptTokens     int64   `json:"tool_use_prompt_tokens,omitempty"`
	ImageInputTokens        int64   `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens  int64   `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens       int64   `json:"image_output_tokens,omitempty"`
	EstimatedCost           float64 `json:"estimated_cost"`
}

// Bucket is one time-bucket usage aggregation.
type Bucket struct {
	BucketStart time.Time `json:"bucket_start"`
	BucketEnd   time.Time `json:"bucket_end"`
	Totals
}

// DimensionTotals is one dimension aggregate item.
type DimensionTotals struct {
	Key string `json:"key"`
	Totals
}

// AggregateResult is the full aggregation output.
type AggregateResult struct {
	From              time.Time               `json:"from"`
	To                time.Time               `json:"to"`
	GroupBy           string                  `json:"group_by"`
	Totals            Totals                  `json:"totals"`
	Buckets           []Bucket                `json:"buckets"`
	ByClient          []DimensionTotals       `json:"by_client"`
	ByModel           []DimensionTotals       `json:"by_model"`
	ByModelGroupModel []ModelGroupModelTotals `json:"by_model_group_model,omitempty"`
	ByTargetModel     []TargetModelTotals     `json:"by_target_model,omitempty"`
}

type ModelGroupModelTotals struct {
	ModelGroup  string `json:"model_group"`
	Model       string `json:"model"`
	TargetCount int    `json:"target_count"`
	Totals
}

type TargetModelTotals struct {
	ModelGroup    string `json:"model_group"`
	Target        string `json:"target"`
	Model         string `json:"model"`
	TargetDeleted bool   `json:"target_deleted"`
	Totals
}

type ModelGroupUsageGroup struct {
	Name          string                   `json:"name"`
	TargetCount   int                      `json:"target_count"`
	ModelCount    int                      `json:"model_count"`
	Totals        Totals                   `json:"totals"`
	ByTargetModel []ModelGroupTargetTotals `json:"by_target_model"`
}

type ModelGroupTargetTotals struct {
	Target        string `json:"target"`
	Model         string `json:"model"`
	TargetDeleted bool   `json:"target_deleted"`
	Totals
}

type ModelGroupUsageResult struct {
	From              time.Time               `json:"from"`
	To                time.Time               `json:"to"`
	Groups            []ModelGroupUsageGroup  `json:"groups"`
	ByModelGroupModel []ModelGroupModelTotals `json:"by_model_group_model,omitempty"`
	ByTargetModel     []TargetModelTotals     `json:"by_target_model,omitempty"`
}

// SummaryResult provides predefined windows for UI.
type SummaryResult struct {
	GeneratedAt time.Time `json:"generated_at"`
	LastHour    Totals    `json:"last_hour"`
	Today       Totals    `json:"today"`
	Yesterday   Totals    `json:"yesterday"`
	Last7Days   Totals    `json:"last_7_days"`
	Last30Days  Totals    `json:"last_30_days"`
}

// Recorder records one usage event.
type Recorder interface {
	Record(event Event) error
}

// IdempotentRecorder reports whether this call newly accepted a durable bundle.
// Replays may repair persistence but must not charge live counters twice.
type IdempotentRecorder interface {
	Recorder
	RecordOnce(event Event) (bool, error)
}

// --- Utility functions used by nosql.UsageStore and other aggregation code ---

// FilterEvents filters events according to filter criteria.
func FilterEvents(events []Event, filter Filter) []Event {
	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	filtered := make([]Event, 0, len(events))
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if filter.From != nil && t.Before(filter.From.UTC()) {
			continue
		}
		if filter.To != nil && t.After(filter.To.UTC()) {
			continue
		}
		if clientKey != "" && evt.ClientName != clientKey {
			continue
		}
		if modelKey != "" && strings.ToLower(evt.Model) != modelKey {
			continue
		}
		filtered = append(filtered, evt)
	}
	return filtered
}

// NormalizeGroupBy normalises group_by values.
func NormalizeGroupBy(groupBy string) string {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy != "hour" {
		return "day"
	}
	return groupBy
}

// BucketStartFor computes the bucket boundary for the given time.
func BucketStartFor(t time.Time, groupBy string) time.Time {
	t = t.UTC()
	if groupBy == "hour" {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// StepDuration returns the duration for one bucket.
func StepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

// SortedDimensions converts a dimension map to a sorted slice.
func SortedDimensions(input map[string]*DimensionTotals) []DimensionTotals {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]DimensionTotals, 0, len(keys))
	for _, key := range keys {
		result = append(result, *input[key])
	}
	return result
}

// AddEventTotals adds one event's contribution to aggregate totals.
func AddEventTotals(target *Totals, evt Event, resolver RateResolver) {
	CheckEventPrices(&evt, resolver)
	target.Requests += RequestWeight(evt)
	if evt.UsageStatus == UsageStatusIncomplete || evt.UsageStatus == UsageStatusConflict {
		target.IncompleteUsageCount++
	}
	if evt.CostIncomplete {
		target.IncompleteCostCount++
	}
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.CacheCreationTokens += evt.CacheCreationTokens
	target.ExplicitCacheReadTokens += evt.ExplicitCacheReadTokens
	target.ReasoningTokens += evt.ReasoningTokens
	target.CacheCreation5mTokens += evt.CacheCreation5mTokens
	target.CacheCreation1hTokens += evt.CacheCreation1hTokens
	target.ToolUsePromptTokens += evt.ToolUsePromptTokens
	target.ImageInputTokens += evt.ImageInputTokens
	target.CachedImageInputTokens += evt.CachedImageInputTokens
	target.ImageOutputTokens += evt.ImageOutputTokens
	target.EstimatedCost += EstimateEventCost(evt, resolver)
}

// EstimateEventCost returns the estimated cost for a single event, delegating
// to the RateResolver (which selects rates by target/model and the event's
// historical timestamp).
func EstimateEventCost(evt Event, resolver RateResolver) float64 {
	if resolver == nil || evt.NonBillable {
		return 0
	}
	resolution := ResolveEventRate(evt, resolver)
	rate := resolution.Rates
	if !resolution.Found {
		return 0
	}
	return rate.CostFor(evt.InputTokens, evt.OutputTokens, evt.CachedTokens, evt.CacheCreationTokens, evt.ExplicitCacheReadTokens, evt.CacheCreation5mTokens, evt.CacheCreation1hTokens, evt.ImageInputTokens, evt.CachedImageInputTokens, evt.ImageOutputTokens)
}

// MissingImagePrice distinguishes a provisional text-price estimate from a
// complete image bill. A nil cached-image price inherits the image-input price.
func (r CostRates) MissingImagePrice(input, cached, output int64) string {
	if input > 0 && r.ImageInputPer1MTokens == nil {
		return "missing_image_input_price"
	}
	if cached > 0 && r.CachedImageInputPer1MTokens == nil && r.ImageInputPer1MTokens == nil {
		return "missing_cached_image_input_price"
	}
	if output > 0 && r.ImageOutputPer1MTokens == nil {
		return "missing_image_output_price"
	}
	return ""
}

// NormalizeImagePartitions bounds billable image subsets at the event boundary,
// before raw detail, hourly sums, and live quota increments can diverge. It is
// idempotent. Original invalid counts are not used for billing or aggregation.
func NormalizeImagePartitions(evt *Event) {
	clamp := func(value *int64, total int64) {
		normalized := max(0, min(*value, max(0, total)))
		if normalized != *value {
			evt.CostIncomplete = true
			evt.UsageStatus = UsageStatusConflict
			appendUsageDiagnostic(evt, "image_partition_exceeds_total")
			*value = normalized
		}
	}
	clamp(&evt.ImageInputTokens, evt.InputTokens)
	clamp(&evt.CachedImageInputTokens, evt.CachedTokens)
	clamp(&evt.ImageOutputTokens, evt.OutputTokens)
	// Image-cache reads take precedence over explicit text-cache reads.
	if evt.CachedImageInputTokens > 0 {
		evt.ExplicitCacheReadTokens = max(0, min(evt.ExplicitCacheReadTokens, evt.CachedTokens-evt.CachedImageInputTokens))
	}
}

func appendUsageDiagnostic(evt *Event, reason string) {
	for _, existing := range strings.Split(evt.UsageDiagnostic, ";") {
		if existing == reason {
			return
		}
	}
	if evt.UsageDiagnostic != "" {
		evt.UsageDiagnostic += ";"
	}
	evt.UsageDiagnostic += reason
}

// CheckImagePrices records price completeness before persistence. It never
// clears a protocol or audio diagnostic. Missing prices still yield estimates.
func CheckImagePrices(evt *Event, resolver RateResolver) {
	NormalizeImagePartitions(evt)
	if evt.NonBillable {
		return
	}
	var rate CostRates
	if resolver != nil {
		rate = ResolveEventRate(*evt, resolver).Rates
	}
	if reason := rate.MissingImagePrice(evt.ImageInputTokens, evt.CachedImageInputTokens, evt.ImageOutputTokens); reason != "" {
		evt.CostIncomplete = true
		appendUsageDiagnostic(evt, reason)
	}
}
