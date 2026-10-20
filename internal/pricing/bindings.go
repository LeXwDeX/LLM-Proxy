package pricing

import (
	"fmt"
	"strings"
)

func uniqueBindings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if !seen[key] {
			seen[key] = true
			out = append(out, value)
		}
	}
	return out
}

// EffectiveTargets returns a detached, case-insensitively deduplicated set.
// A single empty target denotes global pricing, as in legacy rules.
func (t PricingTemplate) EffectiveTargets() []string {
	if t.Targets != nil {
		return uniqueBindings(t.Targets)
	}
	return []string{strings.TrimSpace(t.Target)}
}

// EffectiveModels returns the unprefixed billing model names.
func (t PricingTemplate) EffectiveModels() []string {
	if t.Models != nil {
		return uniqueBindings(t.Models)
	}
	return []string{strings.TrimSpace(t.Model)}
}

// ValidateBindings rejects empty arrays, empty models, mixed global/specific
// targets, and ambiguous combinations of legacy fields and new arrays.
func ValidateBindings(t *PricingTemplate) error {
	targets, models := t.EffectiveTargets(), t.EffectiveModels()
	if len(targets) == 0 {
		return fmt.Errorf("pricing: targets must not be empty")
	}
	if len(models) == 0 {
		return fmt.Errorf("pricing: models must not be empty")
	}
	for _, target := range targets {
		if strings.ContainsRune(target, 0) {
			return fmt.Errorf("pricing: invalid target")
		}
		if target == "" && len(targets) > 1 {
			return fmt.Errorf("pricing: global target cannot be combined with specific targets")
		}
	}
	for _, model := range models {
		if model == "" {
			return fmt.Errorf("pricing: model is required")
		}
		if strings.ContainsRune(model, 0) {
			return fmt.Errorf("pricing: invalid model")
		}
	}
	if t.Targets != nil && strings.TrimSpace(t.Target) != "" && !strings.EqualFold(strings.TrimSpace(t.Target), targets[0]) {
		return fmt.Errorf("pricing: target conflicts with targets")
	}
	if t.Models != nil && strings.TrimSpace(t.Model) != "" && !strings.EqualFold(strings.TrimSpace(t.Model), models[0]) {
		return fmt.Errorf("pricing: model conflicts with models")
	}
	return nil
}

// Shared reports whether legacy single-binding writes would affect other bindings.
func (t PricingTemplate) Shared() bool {
	return len(t.EffectiveTargets()) > 1 || len(t.EffectiveModels()) > 1
}

// SameBindings compares binding sets without depending on case or order.
func (t PricingTemplate) SameBindings(other PricingTemplate) bool {
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		set := map[string]bool{}
		for _, v := range a {
			set[strings.ToLower(v)] = true
		}
		for _, v := range b {
			if !set[strings.ToLower(v)] {
				return false
			}
		}
		return true
	}
	return equal(t.EffectiveTargets(), other.EffectiveTargets()) && equal(t.EffectiveModels(), other.EffectiveModels())
}
