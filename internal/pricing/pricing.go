// Package pricing implements the time-based pricing engine that resolves token
// cost rates for a (target, model) pair at a given instant.
//
// It holds pricing templates — flat all-day rules and schedules with explicit date exceptions
// with half-open [Start, End) effective intervals — indexes them for
// case-insensitive lookup, and falls back through the default-target template
// and finally the catalog default price. The package depends only on usage and
// catalog/config to avoid import cycles with the storage/admin/quota layers.
package pricing

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// DefaultTimezone is the fixed UTC offset used to interpret schedule periods
// when a template does not specify one.
const DefaultTimezone = "+08:00"

// ModeFlat is the template mode for an all-day fixed price.
const ModeFlat = "flat"

// ModeSchedule is the template mode for weekday/time-window based pricing.
const ModeSchedule = "schedule"

// keySeparator separates target from model in the Resolver index key. It is a
// character that cannot appear in a trimmed target/model value.
const keySeparator = "\x00"

// PricingTemplate is one shared pricing rule for every target × model binding.
// Template 0 is the all-day fallback: flat mode, zero Start (sentinel), nil End.
type PricingTemplate struct {
	Version        int              `json:"version,omitempty"`
	Conditions     []PriceCondition `json:"conditions,omitempty"`
	DateExceptions []DateException  `json:"date_exceptions,omitempty"`
	ID             string           `json:"id"`
	Targets        []string         `json:"targets,omitempty"` // absent = legacy Target; [""] = global
	Models         []string         `json:"models,omitempty"`  // absent = legacy Model
	Target         string           `json:"target"`            // "" = default target; matched case-insensitively
	Model          string           `json:"model"`             // matched case-insensitively
	Start          time.Time        `json:"start"`             // UTC, inclusive; zero value = template 0 sentinel
	End            *time.Time       `json:"end,omitempty"`     // UTC, exclusive; nil = until now
	Mode           string           `json:"mode"`              // "flat" | "schedule"
	Timezone       string           `json:"timezone"`          // UTC offset, default "+08:00"; legacy IANA names are accepted
	BaseRates      usage.CostRates  `json:"base_rates"`        // flat: all-day; schedule: fallback when no period matches
	Periods        []PricingPeriod  `json:"periods,omitempty"`
}

// PricingPeriod is a weekday-based time window within a schedule template.
type PricingPeriod struct {
	Conditions []PriceCondition `json:"conditions,omitempty"`
	Weekdays   []int            `json:"weekdays"` // ISO: 1=Mon .. 7=Sun
	Start      string           `json:"start"`    // "HH:mm"
	End        string           `json:"end"`      // "HH:mm" or "24:00" (end-of-day), half-open [start, end)
	Rates      usage.CostRates  `json:"rates"`
}

// IsTemplate0 reports whether t is the all-day fallback template (zero Start
// sentinel).
func IsTemplate0(t *PricingTemplate) bool { return t.Start.IsZero() }

// NormalizeTemplate trims whitespace on target/model/timezone, defaults
// timezone to DefaultTimezone when empty, defaults mode to ModeFlat when empty,
// and trims each period's Start/End. It preserves original case on Target and
// Model (display).
func NormalizeTemplate(t *PricingTemplate) {
	t.Target = strings.TrimSpace(t.Target)
	t.Model = strings.TrimSpace(t.Model)
	if t.Targets != nil {
		t.Targets = uniqueBindings(t.Targets)
		if len(t.Targets) > 0 {
			t.Target = t.Targets[0]
		}
	}
	if t.Models != nil {
		t.Models = uniqueBindings(t.Models)
		if len(t.Models) > 0 {
			t.Model = t.Models[0]
		}
	}
	t.Timezone = strings.TrimSpace(t.Timezone)
	if t.Timezone == "" {
		t.Timezone = DefaultTimezone
	} else if canonical, _, matched, err := parseFixedUTCOffset(t.Timezone); matched && err == nil {
		t.Timezone = canonical
	}
	if t.Mode == "" {
		t.Mode = ModeFlat
	}
	for i := range t.Periods {
		t.Periods[i].Start = strings.TrimSpace(t.Periods[i].Start)
		t.Periods[i].End = strings.TrimSpace(t.Periods[i].End)
	}
}

// ValidateTemplate validates a single template's own fields (it does NOT check
// cross-template overlap). It returns a descriptive error on the first
// violation.
func ValidateTemplate(t *PricingTemplate) error {
	if err := ValidateBindings(t); err != nil {
		return err
	}

	mode := t.Mode
	if mode == "" {
		mode = ModeFlat
	}
	if !IsValidMode(mode) {
		return fmt.Errorf("pricing: invalid mode %q (want %q or %q)", t.Mode, ModeFlat, ModeSchedule)
	}

	tz := strings.TrimSpace(t.Timezone)
	if tz == "" {
		tz = DefaultTimezone
	}
	if _, err := loadTimezoneLocation(tz); err != nil {
		return fmt.Errorf("pricing: invalid timezone %q: use a UTC offset such as +08:00 or -12:00", t.Timezone)
	}

	if err := validateContextRules(t); err != nil {
		return err
	}
	if err := validateRates("base", t.BaseRates); err != nil {
		return err
	}

	if IsTemplate0(t) {
		if mode != ModeFlat {
			return errors.New("pricing: template 0 must use flat mode")
		}
		if t.End != nil {
			return errors.New("pricing: template 0 must not have an End")
		}
		return nil
	}

	if t.End != nil && !t.End.After(t.Start) {
		return errors.New("pricing: End must be after Start")
	}

	if mode == ModeSchedule {
		if err := validatePeriods(t.Periods); err != nil {
			return err
		}
	}
	return nil
}

// validateRates rejects any negative rate field.
func validateRates(name string, r usage.CostRates) error {
	for _, value := range []float64{r.InputPer1MTokens, r.OutputPer1MTokens, r.CacheWritePer1MToken, r.CacheReadPer1MToken} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("pricing: %s rates must be finite", name)
		}
	}
	for _, value := range []*float64{r.ExplicitCacheReadPer1MToken, r.CacheWrite5mPer1MToken, r.CacheWrite1hPer1MToken, r.ImageInputPer1MTokens, r.CachedImageInputPer1MTokens, r.ImageOutputPer1MTokens} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("pricing: %s optional rates must be finite and non-negative", name)
		}
	}
	if r.InputPer1MTokens < 0 || r.OutputPer1MTokens < 0 ||
		r.CacheWritePer1MToken < 0 || r.CacheReadPer1MToken < 0 {
		return fmt.Errorf("pricing: %s rates must be non-negative", name)
	}
	return nil
}

// validatePeriods validates schedule periods: weekday ranges, "HH:mm" parsing,
// non-cross-midnight windows, and non-overlapping windows per weekday.
func validatePeriods(periods []PricingPeriod) error {
	type span struct{ start, end int }
	byWeekday := make(map[int][]span)

	for i, p := range periods {
		start, err := parseHM(p.Start)
		if err != nil {
			return fmt.Errorf("pricing: period %d: invalid start %q", i, p.Start)
		}
		end, err := parseHM(p.End)
		if err != nil {
			return fmt.Errorf("pricing: period %d: invalid end %q", i, p.End)
		}
		if start >= end {
			return fmt.Errorf("pricing: period %d: start must be before end (cross-midnight windows are not supported)", i)
		}
		if err := validateRates(fmt.Sprintf("period %d", i), p.Rates); err != nil {
			return err
		}
		for _, wd := range p.Weekdays {
			if wd < 1 || wd > 7 {
				return fmt.Errorf("pricing: period %d: weekday must be in 1..7, got %d", i, wd)
			}
			byWeekday[wd] = append(byWeekday[wd], span{start, end})
		}
	}

	for wd, spans := range byWeekday {
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		for i := 0; i+1 < len(spans); i++ {
			if spans[i].end > spans[i+1].start {
				return fmt.Errorf("pricing: overlapping periods on weekday %d", wd)
			}
		}
	}
	return nil
}

// ValidateCoverageSet validates that coverage templates (non-template-0) within
// the same (target, model) do not overlap on their [Start, End) intervals.
// Each binding has at most one template 0; it may coexist with coverage rules.
func ValidateCoverageSet(templates []PricingTemplate) error {
	groups := make(map[string][]PricingTemplate)
	defaults := make(map[string]bool)
	for _, t := range templates {
		for _, target := range t.EffectiveTargets() {
			for _, model := range t.EffectiveModels() {
				key := templateKey(target, model)
				if IsTemplate0(&t) {
					if defaults[key] {
						return fmt.Errorf("pricing: duplicate fallback for target %q model %q", target, model)
					}
					defaults[key] = true
				} else {
					groups[key] = append(groups[key], t)
				}
			}
		}
	}
	for key, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].Start.Before(group[j].Start) })
		parts := strings.SplitN(key, keySeparator, 2)
		for i := 0; i+1 < len(group); i++ {
			if group[i].End == nil || group[i].End.After(group[i+1].Start) {
				return fmt.Errorf("pricing: overlapping coverage for target %q model %q", parts[0], parts[1])
			}
		}
	}
	return nil
}

// templateKey builds the index key for a (target, model) pair using
// case-insensitive, whitespace-trimmed values.
func templateKey(target, model string) string {
	return strings.ToLower(strings.TrimSpace(target)) + keySeparator + strings.ToLower(strings.TrimSpace(model))
}

// parseHM parses an "HH:mm" string into minutes since midnight. The special
// value "24:00" (end-of-day, 1440) is accepted so late-night windows can cover
// the final minute of the day; time.Parse alone rejects it because hour 24 is
// out of range.
func parseHM(s string) (int, error) {
	if s == "24:00" {
		return 24 * 60, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// loadTimezoneLocation resolves the preferred fixed-offset representation and
// keeps IANA names readable for templates created before fixed offsets became
// the admin/API default.
func loadTimezoneLocation(value string) (*time.Location, error) {
	if canonical, offset, matched, err := parseFixedUTCOffset(value); matched {
		if err != nil {
			return nil, err
		}
		return time.FixedZone(canonical, offset), nil
	}
	return time.LoadLocation(strings.TrimSpace(value))
}

// parseFixedUTCOffset parses +8, -12, +08:00, UTC+8, and similar fixed UTC
// offsets. The sign is required so ordinary values such as "8" are not
// mistaken for timezones. The canonical representation is always ±HH:MM.
// matched is false for non-offset values such as legacy IANA names.
func parseFixedUTCOffset(value string) (canonical string, offsetSeconds int, matched bool, err error) {
	s := strings.TrimSpace(value)
	if len(s) >= 3 && strings.EqualFold(s[:3], "UTC") {
		s = strings.TrimSpace(s[3:])
		if s == "" {
			return "", 0, false, nil
		}
	}
	if s == "" {
		return "", 0, false, nil
	}

	sign := 1
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		sign = -1
		s = s[1:]
	default:
		return "", 0, false, nil
	}
	matched = true
	if s == "" {
		return "", 0, true, errors.New("empty UTC offset")
	}

	parts := strings.Split(s, ":")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 2 {
		return "", 0, true, errors.New("invalid UTC offset format")
	}
	hours, parseErr := strconv.Atoi(parts[0])
	if parseErr != nil {
		return "", 0, true, errors.New("invalid UTC offset hour")
	}
	minutes := 0
	if len(parts) == 2 {
		if len(parts[1]) != 2 {
			return "", 0, true, errors.New("UTC offset minutes must use two digits")
		}
		minutes, parseErr = strconv.Atoi(parts[1])
		if parseErr != nil || minutes < 0 || minutes > 59 {
			return "", 0, true, errors.New("invalid UTC offset minutes")
		}
	}

	totalMinutes := sign * (hours*60 + minutes)
	if totalMinutes < -12*60 || totalMinutes > 14*60 {
		return "", 0, true, errors.New("UTC offset must be between -12:00 and +14:00")
	}
	if totalMinutes == 0 {
		sign = 1
	}
	absMinutes := totalMinutes
	if absMinutes < 0 {
		absMinutes = -absMinutes
	}
	signChar := '+'
	if sign < 0 {
		signChar = '-'
	}
	canonical = fmt.Sprintf("%c%02d:%02d", signChar, absMinutes/60, absMinutes%60)
	return canonical, totalMinutes * 60, true, nil
}

// isoWeekday maps Go's time.Weekday (Sunday=0) to ISO weekday (Monday=1 ..
// Sunday=7).
func isoWeekday(wd time.Weekday) int {
	if wd == time.Sunday {
		return 7
	}
	return int(wd)
}

// Resolver resolves cost rates for a (target, model) at an instant using the
// time-based templates plus a catalog default fallback.
type Resolver struct {
	index                 map[string][]PricingTemplate // key = lower(target) + "\x00" + lower(model), sorted by Start ascending
	fallback              map[string]usage.CostRates   // model (lower) -> catalog default rates
	locations             map[string]*time.Location    // timezone string -> loaded location (fallback UTC)
	targetProviders       map[string]string
	providerFallback      map[string]usage.CostRates
	gptCatalogUnavailable bool
}

// NewResolver builds a Resolver. cat may be nil (no catalog fallback).
func NewResolver(templates []PricingTemplate, cat *catalog.Catalog) *Resolver {
	r := &Resolver{
		index:            make(map[string][]PricingTemplate),
		fallback:         make(map[string]usage.CostRates),
		locations:        make(map[string]*time.Location),
		providerFallback: make(map[string]usage.CostRates),
	}

	for _, t := range templates {
		t = cloneContextRules(t)
		t.BaseRates = t.BaseRates.Scaled(1)
		t.Periods = append([]PricingPeriod(nil), t.Periods...)
		for i := range t.Periods {
			t.Periods[i].Rates = t.Periods[i].Rates.Scaled(1)
			t.Periods[i].Weekdays = append([]int(nil), t.Periods[i].Weekdays...)
		}
		NormalizeTemplate(&t)
		if t.Model == "" {
			continue
		}
		for _, target := range t.EffectiveTargets() {
			for _, model := range t.EffectiveModels() {
				key := templateKey(target, model)
				r.index[key] = append(r.index[key], t)
			}
		}
		if _, ok := r.locations[t.Timezone]; !ok {
			loc, err := loadTimezoneLocation(t.Timezone)
			if err != nil {
				loc = time.UTC
			}
			r.locations[t.Timezone] = loc
		}
	}

	for key, list := range r.index {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
		r.index[key] = list
	}

	if cat != nil {
		for _, entry := range cat.ListAll() {
			if entry.DefaultCost == nil || strings.TrimSpace(entry.Model) == "" {
				continue
			}
			model := strings.ToLower(strings.TrimSpace(entry.Model))
			r.fallback[model] = usage.CostRates{
				InputPer1MTokens:            entry.DefaultCost.InputPer1MTokens,
				OutputPer1MTokens:           entry.DefaultCost.OutputPer1MTokens,
				CacheWritePer1MToken:        entry.DefaultCost.CacheWritePer1MToken,
				CacheReadPer1MToken:         entry.DefaultCost.CacheReadPer1MToken,
				ImageInputPer1MTokens:       usage.CloneRate(entry.DefaultCost.ImageInputPer1MTokens),
				CachedImageInputPer1MTokens: usage.CloneRate(entry.DefaultCost.CachedImageInputPer1MTokens),
				ImageOutputPer1MTokens:      usage.CloneRate(entry.DefaultCost.ImageOutputPer1MTokens),
			}
			r.providerFallback[entry.EndpointType+keySeparator+model] = r.fallback[model]
		}
	}

	return r
}

// RateFor implements usage.RateResolver. Fallback order (§5.3):
//  1. (target, model) coverage template covering at, else that key's template 0;
//  2. ("", model) coverage template, else its template 0 (only when target != "");
//  3. catalog default for the model;
//  4. not found (returns false).
func (r *Resolver) RateFor(target, model string, at time.Time) (usage.CostRates, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return usage.CostRates{}, false
	}
	target = strings.ToLower(strings.TrimSpace(target))

	if rates, ok := r.rateForKey(target, model, at); ok {
		return rates.Scaled(1), true
	}
	if target != "" {
		if rates, ok := r.rateForKey("", model, at); ok {
			return rates.Scaled(1), true
		}
	}
	// Scoped GPT lookups must not inherit another provider's same-name price.
	// Explicit target/global templates above remain authoritative, including zero.
	if strings.HasPrefix(model, "gpt") && r.gptCatalogUnavailable {
		return usage.CostRates{}, false
	}
	provider := r.targetProviders[target]
	if provider == config.EndpointTypeOpenAI || provider == config.EndpointTypeAzureOpenAI {
		if rates, ok := r.providerFallback[provider+keySeparator+model]; ok {
			return rates.Scaled(1), true
		}
		if strings.HasPrefix(model, "gpt") {
			// Scoped GPT lookups must not inherit another provider's same-name
			// price: a provider-scoped miss stays unpriced instead of falling
			// through to the generic catalog fallback below.
			return usage.CostRates{}, false
		}
	}
	if rates, ok := r.fallback[model]; ok {
		return rates.Scaled(1), true
	}
	return usage.CostRates{}, false
}

// rateForKey selects the template for an already-normalized (target, model)
// key. It returns the latest-Start coverage template covering at, else the
// key's template 0.
func (r *Resolver) rateForKey(target, model string, at time.Time) (usage.CostRates, bool) {
	list, ok := r.index[target+keySeparator+model]
	if !ok {
		return usage.CostRates{}, false
	}

	var tmpl0 *PricingTemplate
	var selected *PricingTemplate
	for i := range list {
		t := &list[i]
		if IsTemplate0(t) {
			tmpl0 = t
			continue
		}
		if t.Start.After(at) {
			continue
		}
		if t.End != nil && !at.Before(*t.End) {
			continue
		}
		if selected == nil || t.Start.After(selected.Start) {
			selected = t
		}
	}

	if selected != nil {
		return r.ratesForTemplate(selected, at), true
	}
	if tmpl0 != nil {
		return r.ratesForTemplate(tmpl0, at), true
	}
	return usage.CostRates{}, false
}

// ratesForTemplate resolves a selected template's rates at instant at. Flat
// templates always return BaseRates. Schedule templates convert at into the
// template timezone, compute the ISO weekday and minutes, and return the first
// matching period's rates (else BaseRates), with date exceptions taking precedence.
func (r *Resolver) ratesForTemplate(t *PricingTemplate, at time.Time) usage.CostRates {
	rates, _ := r.rateScope(t, at)
	return rates
}
