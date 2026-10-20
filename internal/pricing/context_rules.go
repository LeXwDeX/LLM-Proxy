package pricing

import (
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// PriceCondition is a complete rate replacement for a matching billing context.
// Input bounds are half-open. Empty service fields are wildcards; overlapping
// conditions are rejected rather than resolved using array order.
type PriceCondition struct {
	ID             string          `json:"id"`
	MinInputTokens *int64          `json:"min_input_tokens,omitempty"`
	MaxInputTokens *int64          `json:"max_input_tokens,omitempty"`
	ServiceTier    string          `json:"service_tier,omitempty"`
	Speed          string          `json:"speed,omitempty"`
	Rates          usage.CostRates `json:"rates"`
}

// DateException overrides the weekday schedule for local calendar dates in
// [StartDate, EndDate). Dates are explicit configuration, never inferred from a
// provider name or the current year's holiday calendar.
type DateException struct {
	StartDate  string           `json:"start_date"`
	EndDate    string           `json:"end_date"`
	Rates      usage.CostRates  `json:"rates"`
	Conditions []PriceCondition `json:"conditions,omitempty"`
}

// Public business enums are consumed by the admin schema, not duplicated in UI.
func ServiceTiers() []string {
	return []string{"standard", "fast", "ultrafast", "flex", "batch", "priority"}
}
func Speeds() []string { return []string{"standard", "fast"} }
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func validateContextRules(t *PricingTemplate) error {
	if t.Version < 0 {
		return fmt.Errorf("pricing: version must be non-negative")
	}
	if err := validateConditions(t.Conditions); err != nil {
		return err
	}
	for _, p := range t.Periods {
		if err := validateConditions(p.Conditions); err != nil {
			return err
		}
	}
	if len(t.DateExceptions) > 0 && (t.Mode != ModeSchedule || IsTemplate0(t)) {
		return fmt.Errorf("pricing: date exceptions require a coverage schedule")
	}
	for i, e := range t.DateExceptions {
		start, err := time.Parse(time.DateOnly, e.StartDate)
		if err != nil {
			return fmt.Errorf("pricing: invalid exception start date %q", e.StartDate)
		}
		end, err := time.Parse(time.DateOnly, e.EndDate)
		if err != nil || !end.After(start) {
			return fmt.Errorf("pricing: exception end must follow start")
		}
		if err := validateRates("date exception", e.Rates); err != nil {
			return err
		}
		if err := validateConditions(e.Conditions); err != nil {
			return err
		}
		for _, other := range t.DateExceptions[:i] {
			if e.StartDate < other.EndDate && other.StartDate < e.EndDate {
				return fmt.Errorf("pricing: overlapping date exceptions")
			}
		}
	}
	return nil
}

func validateConditions(conditions []PriceCondition) error {
	ids := map[string]bool{}
	for i, c := range conditions {
		if strings.TrimSpace(c.ID) == "" || ids[c.ID] {
			return fmt.Errorf("pricing: condition IDs must be nonempty and unique")
		}
		ids[c.ID] = true
		if c.MinInputTokens == nil && c.MaxInputTokens == nil && c.ServiceTier == "" && c.Speed == "" {
			return fmt.Errorf("pricing: condition %q has no constraint", c.ID)
		}
		if c.MinInputTokens != nil && *c.MinInputTokens < 0 || c.MaxInputTokens != nil && *c.MaxInputTokens <= 0 || c.MinInputTokens != nil && c.MaxInputTokens != nil && *c.MinInputTokens >= *c.MaxInputTokens {
			return fmt.Errorf("pricing: invalid input bounds for %q", c.ID)
		}
		if c.ServiceTier != "" && !contains(ServiceTiers(), c.ServiceTier) || c.Speed != "" && !contains(Speeds(), c.Speed) {
			return fmt.Errorf("pricing: unknown condition tier or speed for %q", c.ID)
		}
		if err := validateRates("condition "+c.ID, c.Rates); err != nil {
			return err
		}
		for _, other := range conditions[:i] {
			if conditionsOverlap(c, other) {
				return fmt.Errorf("pricing: overlapping conditions %q and %q", c.ID, other.ID)
			}
		}
	}
	return nil
}

func conditionsOverlap(a, b PriceCondition) bool {
	if a.ServiceTier != "" && b.ServiceTier != "" && a.ServiceTier != b.ServiceTier || a.Speed != "" && b.Speed != "" && a.Speed != b.Speed {
		return false
	}
	if a.MaxInputTokens != nil && b.MinInputTokens != nil && *a.MaxInputTokens <= *b.MinInputTokens || b.MaxInputTokens != nil && a.MinInputTokens != nil && *b.MaxInputTokens <= *a.MinInputTokens {
		return false
	}
	return true
}

func cloneConditions(in []PriceCondition) []PriceCondition {
	out := append([]PriceCondition(nil), in...)
	for i := range out {
		out[i].Rates = out[i].Rates.Scaled(1)
		if in[i].MinInputTokens != nil {
			v := *in[i].MinInputTokens
			out[i].MinInputTokens = &v
		}
		if in[i].MaxInputTokens != nil {
			v := *in[i].MaxInputTokens
			out[i].MaxInputTokens = &v
		}
	}
	return out
}
func cloneContextRules(t PricingTemplate) PricingTemplate {
	t.Conditions = cloneConditions(t.Conditions)
	t.Periods = append([]PricingPeriod(nil), t.Periods...)
	for i := range t.Periods {
		t.Periods[i].Conditions = cloneConditions(t.Periods[i].Conditions)
	}
	t.DateExceptions = append([]DateException(nil), t.DateExceptions...)
	for i := range t.DateExceptions {
		t.DateExceptions[i].Rates = t.DateExceptions[i].Rates.Scaled(1)
		t.DateExceptions[i].Conditions = cloneConditions(t.DateExceptions[i].Conditions)
	}
	return t
}

func (r *Resolver) rateScope(t *PricingTemplate, at time.Time) (usage.CostRates, []PriceCondition) {
	if t.Mode != ModeSchedule {
		return t.BaseRates, t.Conditions
	}
	loc := r.locations[t.Timezone]
	if loc == nil {
		loc = time.UTC
	}
	local := at.In(loc)
	date := local.Format(time.DateOnly)
	for _, e := range t.DateExceptions {
		if e.StartDate <= date && date < e.EndDate {
			return e.Rates, e.Conditions
		}
	}
	iso, minutes := isoWeekday(local.Weekday()), local.Hour()*60+local.Minute()
	for _, p := range t.Periods {
		start, a := parseHM(p.Start)
		end, b := parseHM(p.End)
		if a != nil || b != nil {
			continue
		}
		for _, wd := range p.Weekdays {
			if wd == iso && start <= minutes && minutes < end {
				return p.Rates, p.Conditions
			}
		}
	}
	return t.BaseRates, t.Conditions
}

func (r *Resolver) templateForKey(target, model string, at time.Time) *PricingTemplate {
	list := r.index[templateKey(target, model)]
	var fallback, selected *PricingTemplate
	for i := range list {
		t := &list[i]
		if IsTemplate0(t) {
			fallback = t
			continue
		}
		if at.Before(t.Start) || t.End != nil && !at.Before(*t.End) {
			continue
		}
		if selected == nil || t.Start.After(selected.Start) {
			selected = t
		}
	}
	if selected != nil {
		return selected
	}
	return fallback
}

// ResolveEventRate selects rates using persisted response context. Legacy
// RateFor remains the time-scope estimate for callers lacking event evidence.
func (r *Resolver) ResolveEventRate(evt usage.Event) usage.RateResolution {
	target, model := strings.ToLower(strings.TrimSpace(evt.Target)), strings.ToLower(strings.TrimSpace(evt.Model))
	if model == "" {
		return usage.RateResolution{Complete: true}
	}
	template := r.templateForKey(target, model, evt.Timestamp)
	if template == nil && target != "" {
		template = r.templateForKey("", model, evt.Timestamp)
	}
	if template == nil {
		rates, found := r.RateFor(target, model, evt.Timestamp)
		return usage.RateResolution{Rates: rates, Found: found, Complete: true}
	}
	rates, conditions := r.rateScope(template, evt.Timestamp)
	result := usage.RateResolution{Rates: rates.Scaled(1), Found: true, Complete: true, RuleID: template.ID, Version: template.Version}
	if len(conditions) == 0 {
		return result
	}
	input, tier, speed := false, false, false
	for _, c := range conditions {
		input = input || c.MinInputTokens != nil || c.MaxInputTokens != nil
		tier = tier || c.ServiceTier != ""
		speed = speed || c.Speed != ""
	}
	ctx := evt.PricingContext
	incomplete := func(reason string) usage.RateResolution {
		result.Complete = false
		result.Diagnostic = reason
		return result
	}
	if ctx == nil || ctx.Version != usage.PricingContextVersion {
		return incomplete("pricing_context_missing_or_unsupported")
	}
	if input && (!ctx.TotalInputKnown || !ctx.TotalInputValid || ctx.TotalInputTokens < 0) {
		return incomplete("pricing_total_input_unknown")
	}
	if tier && (ctx.ResponseServiceTier.State != usage.PricingValueValue || !contains(ServiceTiers(), ctx.ActualServiceTier)) {
		return incomplete("pricing_service_tier_unknown")
	}
	if speed && (ctx.ResponseSpeed.State != usage.PricingValueValue || !contains(Speeds(), ctx.ActualSpeed)) {
		return incomplete("pricing_speed_unknown")
	}
	matches := 0
	for _, c := range conditions {
		if c.MinInputTokens != nil && ctx.TotalInputTokens < *c.MinInputTokens || c.MaxInputTokens != nil && ctx.TotalInputTokens >= *c.MaxInputTokens || c.ServiceTier != "" && ctx.ActualServiceTier != c.ServiceTier || c.Speed != "" && ctx.ActualSpeed != c.Speed {
			continue
		}
		matches++
		result.Rates = c.Rates.Scaled(1)
		result.RuleID = template.ID + "/" + c.ID
	}
	if matches > 1 {
		result.Rates = rates.Scaled(1)
		result.RuleID = template.ID
		return incomplete("pricing_condition_conflict")
	}
	if matches == 0 && (tier && ctx.ActualServiceTier != "standard" || speed && ctx.ActualSpeed != "standard") {
		return incomplete("pricing_combination_unconfigured")
	}
	return result
}

// AllServiceTierOptions supplies display labels from the same domain enum.
func AllServiceTierOptions() []config.EnumOption {
	labels := map[string]string{"standard": "标准服务", "fast": "快速服务", "ultrafast": "超快速服务", "flex": "弹性服务", "batch": "批量服务", "priority": "优先服务"}
	out := make([]config.EnumOption, 0, len(ServiceTiers()))
	for _, v := range ServiceTiers() {
		out = append(out, config.EnumOption{Value: v, Label: labels[v]})
	}
	return out
}
func AllSpeedOptions() []config.EnumOption {
	labels := map[string]string{"standard": "标准速度", "fast": "快速模式"}
	out := make([]config.EnumOption, 0, len(Speeds()))
	for _, v := range Speeds() {
		out = append(out, config.EnumOption{Value: v, Label: labels[v]})
	}
	return out
}
