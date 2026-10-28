package pricing

import (
	"strings"
	"time"
)

// RequiresEventTimestamps reports whether any configured rate boundary falls
// inside a UTC hour, or a rate needs per-request billing context. Such prices
// must use each event's actual timestamp and context rather
// than the hour of a pre-aggregated cell. Legacy IANA schedules are treated
// conservatively because their offsets can change over historical periods.
func (r *Resolver) RequiresEventTimestamps() bool {
	for _, templates := range r.index {
		for _, template := range templates {
			if templateRequiresEventTimestamps(template) {
				return true
			}
		}
	}
	return false
}

// RequiresEventTimestampsFor keeps unrelated targets/models on the hourly
// path even when another configured pricing key has finer boundaries.
func (r *Resolver) RequiresEventTimestampsFor(target, model string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	model = strings.ToLower(strings.TrimSpace(model))
	for _, key := range []string{templateKey(target, model), templateKey("", model)} {
		for _, template := range r.index[key] {
			if templateRequiresEventTimestamps(template) {
				return true
			}
		}
	}
	return false
}

// RequiresEventTimestampsForModel checks every target binding for the model.
func (r *Resolver) RequiresEventTimestampsForModel(model string) bool {
	if r == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || strings.Contains(model, keySeparator) {
		return r.RequiresEventTimestamps()
	}
	for key, templates := range r.index {
		_, indexedModel, ok := strings.Cut(key, keySeparator)
		if !ok || strings.Contains(indexedModel, keySeparator) {
			return true
		}
		if indexedModel != model {
			continue
		}
		for _, template := range templates {
			if templateRequiresEventTimestamps(template) {
				return true
			}
		}
	}
	return false
}

func templateRequiresEventTimestamps(template PricingTemplate) bool {
	if len(template.Conditions) > 0 {
		return true
	}
	for _, p := range template.Periods {
		if len(p.Conditions) > 0 {
			return true
		}
	}
	for _, e := range template.DateExceptions {
		if len(e.Conditions) > 0 {
			return true
		}
	}
	if !template.Start.IsZero() && !template.Start.Equal(template.Start.Truncate(time.Hour)) {
		return true
	}
	if template.End != nil && !template.End.Equal(template.End.Truncate(time.Hour)) {
		return true
	}
	if template.Mode != ModeSchedule || (len(template.Periods) == 0 && len(template.DateExceptions) == 0) {
		return false
	}
	_, offset, fixed, err := parseFixedUTCOffset(template.Timezone)
	if (!fixed && template.Timezone != "UTC") || err != nil || offset%3600 != 0 {
		return true
	}
	for _, period := range template.Periods {
		start, startErr := parseHM(period.Start)
		end, endErr := parseHM(period.End)
		if startErr != nil || endErr != nil || start%60 != 0 || end%60 != 0 {
			return true
		}
	}
	return false
}
