package nosql

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
)

// billingDataMigratedMetaKey marks completion of the one-shot legacy billing
// migration: billing_templates + time_segments → pricing_templates, consumer
// groups (groups + targets.group) → model_groups, and per-target model_costs
// entries → (target, model) template-0 pricing templates.
const billingDataMigratedMetaKey = "billing_data_migrated_v1"

// legacyTimeSegment mirrors the removed B2 config.TimeSegment as stored in the
// time_segments bucket (segments were evaluated at a fixed UTC+8 offset).
type legacyTimeSegment struct {
	Name      string `json:"name"`
	StartHour int    `json:"start_hour"`
	EndHour   int    `json:"end_hour"`
	Days      []int  `json:"days,omitempty"`
	DateFrom  string `json:"date_from,omitempty"`
	DateTo    string `json:"date_to,omitempty"`
}

// legacyBillingRule mirrors the removed B2 config.BillingRule.
type legacyBillingRule struct {
	Target            string  `json:"target"`
	Model             string  `json:"model"`
	Segment           string  `json:"segment,omitempty"`
	InputPer1MTokens  float64 `json:"input_per_1m_tokens"`
	OutputPer1MTokens float64 `json:"output_per_1m_tokens"`
	CacheWrite1M      float64 `json:"cache_write_per_1m_tokens,omitempty"`
	CacheRead1M       float64 `json:"cache_read_per_1m_tokens,omitempty"`
}

// legacyBillingTemplate mirrors the removed B2 config.BillingTemplate.
type legacyBillingTemplate struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Rules       []legacyBillingRule `json:"rules,omitempty"`
}

// legacyConsumerGroup mirrors the removed B1 config.ConsumerGroup.
type legacyConsumerGroup struct {
	Name            string  `json:"name"`
	Description     string  `json:"description,omitempty"`
	CostCoefficient float64 `json:"cost_coefficient"`
}

// MigrateBillingData migrates the removed B1/B2 billing data into the unified
// pricing-template / model-group schema:
//
//   - billing_templates + time_segments → pricing_templates. Same-(target,model)
//     segmented rules are merged into one schedule template (periods embedded,
//     fixed "+08:00" timezone, active from the migration instant); all-day
//     rules seed that key's template 0. Existing target-side entries win and
//     are reported, never silently overwritten.
//   - groups + targets.group membership → model_groups (name, cost coefficient,
//     whole-target bindings).
//   - per-target model_costs entries ("target\x00model" keys) → template 0 for
//     that (target, model); the earlier MigrateModelCostsToPricingTemplates
//     pass only seeded default-target template 0s.
//
// Date-range segments (date_from/date_to) have no equivalent yet and are
// approximated by their weekday pattern with a warning. model="*" rules cannot
// be expressed and are skipped with a warning.
//
// The whole migration is one write transaction: the meta marker commits together
// with the writes, so a crash leaves the migration re-runnable. Legacy buckets
// are read-only inputs and are never mutated or deleted (rollback window).
// logger may be nil.
func MigrateBillingData(db *bolt.DB, logger *slog.Logger) error {
	// Issue #125: a maintenance write; the integrity classification is stale
	// from here until a rebuild that starts after this write ended.
	defer integrityMutationBegin()()
	var done bool
	if err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(BucketMeta)); b != nil {
			done = b.Get([]byte(billingDataMigratedMetaKey)) != nil
		}
		return nil
	}); err != nil {
		return fmt.Errorf("check billing data migration meta: %w", err)
	}
	if done {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// ---- Read legacy inputs (view tx, read-only) ----
	segments := map[string]legacyTimeSegment{}
	var templates []legacyBillingTemplate
	var groups []legacyConsumerGroup
	targetGroup := map[string]string{} // target(lower) → group name
	var perTargetCosts []ModelCost

	if err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte("time_segments")); b != nil {
			if err := b.ForEach(func(_, v []byte) error {
				var seg legacyTimeSegment
				if err := json.Unmarshal(v, &seg); err != nil {
					return fmt.Errorf("decode time segment: %w", err)
				}
				segments[strings.ToLower(strings.TrimSpace(seg.Name))] = seg
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket([]byte("billing_templates")); b != nil {
			if err := b.ForEach(func(_, v []byte) error {
				var tpl legacyBillingTemplate
				if err := json.Unmarshal(v, &tpl); err != nil {
					return fmt.Errorf("decode billing template: %w", err)
				}
				templates = append(templates, tpl)
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket([]byte("groups")); b != nil {
			if err := b.ForEach(func(_, v []byte) error {
				var g legacyConsumerGroup
				if err := json.Unmarshal(v, &g); err != nil {
					return fmt.Errorf("decode consumer group: %w", err)
				}
				groups = append(groups, g)
				return nil
			}); err != nil {
				return err
			}
		}
		// Target group membership lived on the removed Target.Group field.
		if b := tx.Bucket([]byte(BucketTargets)); b != nil {
			if err := b.ForEach(func(_, v []byte) error {
				var t struct {
					Name  string `json:"name"`
					Group string `json:"group"`
				}
				if err := json.Unmarshal(v, &t); err != nil {
					return fmt.Errorf("decode target for group membership: %w", err)
				}
				if g := strings.ToLower(strings.TrimSpace(t.Group)); g != "" {
					targetGroup[strings.ToLower(strings.TrimSpace(t.Name))] = g
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket([]byte(BucketModelCosts)); b != nil {
			return b.ForEach(func(k, v []byte) error {
				if !strings.Contains(string(k), targetKeySep) {
					return nil // global-layer entries were seeded by the earlier pass
				}
				var c ModelCost
				if err := json.Unmarshal(v, &c); err != nil {
					return fmt.Errorf("decode per-target model cost %q: %w", string(k), err)
				}
				perTargetCosts = append(perTargetCosts, c)
				return nil
			})
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read legacy billing data: %w", err)
	}

	// ---- Build pricing templates ----
	now := time.Now().UTC().Truncate(time.Hour)
	type keyGroup struct {
		target, model string
		allDay        *legacyBillingRule
		segmentRules  []struct {
			seg  legacyTimeSegment
			rule legacyBillingRule
		}
	}
	byKey := map[string]*keyGroup{}
	var keyOrder []string
	migrated, skipped, warned := 0, 0, 0

	getGroup := func(target, model string) *keyGroup {
		k := target + "\x00" + model
		g, ok := byKey[k]
		if !ok {
			g = &keyGroup{target: target, model: model}
			byKey[k] = g
			keyOrder = append(keyOrder, k)
		}
		return g
	}

	for _, tpl := range templates {
		for _, rule := range tpl.Rules {
			target := strings.ToLower(strings.TrimSpace(rule.Target))
			if target == "*" {
				target = "" // wildcard target == default-target template
			}
			model := strings.ToLower(strings.TrimSpace(rule.Model))
			if model == "" || model == "*" {
				logger.Warn("billing migration: skip rule with wildcard/empty model (not expressible as pricing template)",
					"template", tpl.Name, "target", rule.Target, "model", rule.Model)
				warned++
				skipped++
				continue
			}
			g := getGroup(target, model)
			if rule.Segment == "" {
				g.allDay = &rule
			} else {
				seg, ok := segments[strings.ToLower(strings.TrimSpace(rule.Segment))]
				if !ok {
					logger.Warn("billing migration: skip rule referencing unknown time segment",
						"template", tpl.Name, "segment", rule.Segment, "target", target, "model", model)
					warned++
					skipped++
					continue
				}
				g.segmentRules = append(g.segmentRules, struct {
					seg  legacyTimeSegment
					rule legacyBillingRule
				}{seg: seg, rule: rule})
			}
		}
	}

	buildRates := func(r legacyBillingRule) usage.CostRates {
		return usage.CostRates{
			InputPer1MTokens:     r.InputPer1MTokens,
			OutputPer1MTokens:    r.OutputPer1MTokens,
			CacheWritePer1MToken: r.CacheWrite1M,
			CacheReadPer1MToken:  r.CacheRead1M,
		}
	}
	hm := func(hour int) string {
		if hour >= 24 {
			return "24:00"
		}
		return fmt.Sprintf("%02d:00", hour)
	}

	type pendingTemplate struct {
		t pricing.PricingTemplate
	}
	var toWrite []pendingTemplate

	for _, k := range keyOrder {
		g := byKey[k]
		// All-day rule → template 0 for the key.
		if g.allDay != nil {
			t := pricing.PricingTemplate{
				Target:    g.target,
				Model:     g.model,
				Mode:      pricing.ModeFlat,
				Timezone:  pricing.DefaultTimezone,
				BaseRates: buildRates(*g.allDay),
			}
			if err := pricing.ValidateTemplate(&t); err != nil {
				logger.Warn("billing migration: skip invalid template0", "target", g.target, "model", g.model, "error", err)
				warned++
				skipped++
			} else {
				toWrite = append(toWrite, pendingTemplate{t: t})
			}
		}
		// Segmented rules → one schedule template (active from migration instant).
		if len(g.segmentRules) > 0 {
			base := usage.CostRates{}
			if g.allDay != nil {
				base = buildRates(*g.allDay)
			} else {
				base = buildRates(g.segmentRules[0].rule)
				logger.Warn("billing migration: segmented template has no all-day rule; base rates fall back to first period rates",
					"target", g.target, "model", g.model)
				warned++
			}
			t := pricing.PricingTemplate{
				Target:    g.target,
				Model:     g.model,
				Mode:      pricing.ModeSchedule,
				Timezone:  "+08:00", // legacy segments were evaluated at fixed UTC+8
				Start:     now,
				BaseRates: base,
			}
			for _, sr := range g.segmentRules {
				weekdays := sr.seg.Days
				if len(weekdays) == 0 {
					weekdays = []int{1, 2, 3, 4, 5, 6, 7}
				}
				if sr.seg.DateFrom != "" || sr.seg.DateTo != "" {
					logger.Warn("billing migration: date-range segment approximated by weekday pattern (date ranges not supported yet)",
						"segment", sr.seg.Name, "date_from", sr.seg.DateFrom, "date_to", sr.seg.DateTo,
						"target", g.target, "model", g.model)
					warned++
				}
				t.Periods = append(t.Periods, pricing.PricingPeriod{
					Weekdays: weekdays,
					Start:    hm(sr.seg.StartHour),
					End:      hm(sr.seg.EndHour),
					Rates:    buildRates(sr.rule),
				})
			}
			if err := pricing.ValidateTemplate(&t); err != nil {
				logger.Warn("billing migration: skip invalid schedule template",
					"target", g.target, "model", g.model, "error", err)
				warned++
				skipped++
			} else {
				toWrite = append(toWrite, pendingTemplate{t: t})
			}
		}
	}

	// Per-target model_costs entries → template 0 for (target, model).
	for _, c := range perTargetCosts {
		target := strings.ToLower(strings.TrimSpace(c.Target))
		model := strings.ToLower(strings.TrimSpace(c.Model))
		if target == "" || model == "" {
			continue
		}
		t := pricing.PricingTemplate{
			Target:   target,
			Model:    model,
			Mode:     pricing.ModeFlat,
			Timezone: pricing.DefaultTimezone,
			BaseRates: usage.CostRates{
				InputPer1MTokens:            c.InputPer1MTokens,
				OutputPer1MTokens:           c.OutputPer1MTokens,
				CacheWritePer1MToken:        c.CacheWritePer1MToken,
				CacheReadPer1MToken:         c.CacheReadPer1MToken,
				ExplicitCacheReadPer1MToken: usage.CloneRate(c.ExplicitCacheReadPer1MToken),
				CacheWrite5mPer1MToken:      usage.CloneRate(c.CacheWrite5mPer1MToken),
				CacheWrite1hPer1MToken:      usage.CloneRate(c.CacheWrite1hPer1MToken),
				ImageInputPer1MTokens:       usage.CloneRate(c.ImageInputPer1MTokens),
				CachedImageInputPer1MTokens: usage.CloneRate(c.CachedImageInputPer1MTokens),
				ImageOutputPer1MTokens:      usage.CloneRate(c.ImageOutputPer1MTokens),
			},
		}
		if err := pricing.ValidateTemplate(&t); err != nil {
			logger.Warn("billing migration: skip invalid per-target model cost template",
				"target", target, "model", model, "error", err)
			warned++
			skipped++
			continue
		}
		toWrite = append(toWrite, pendingTemplate{t: t})
	}

	// ---- Build model groups ----
	type pendingGroup struct {
		g       []byte // encoded config.ModelGroup-compatible JSON
		rawName string
	}
	var groupsToWrite []pendingGroup
	for _, cg := range groups {
		name := strings.ToLower(strings.TrimSpace(cg.Name))
		if name == "" {
			continue
		}
		coefficient := cg.CostCoefficient
		if coefficient <= 0 {
			coefficient = 1
		}
		var bindings []map[string]string
		members := make([]string, 0, len(targetGroup))
		for t, g := range targetGroup {
			if g == name {
				members = append(members, t)
				bindings = append(bindings, map[string]string{"target": t})
			}
		}
		sort.Strings(members)
		payload := map[string]any{
			"name":             name,
			"cost_coefficient": coefficient,
			"bindings":         bindings,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode model group %q: %w", name, err)
		}
		groupsToWrite = append(groupsToWrite, pendingGroup{g: data, rawName: name})
	}

	// ---- Write phase: single transaction with the meta marker ----
	err := db.Update(func(tx *bolt.Tx) error {
		pt := tx.Bucket([]byte(BucketPricingTemplates))
		if pt == nil {
			return fmt.Errorf("pricing_templates bucket missing")
		}
		for _, item := range toWrite {
			prefix := pricingTemplatePrefix(item.t.Target, item.t.Model)
			existing, err := scanTemplatesByPrefix(pt, prefix)
			if err != nil {
				return err
			}
			if pricing.IsTemplate0(&item.t) {
				// Conflict policy: an existing template 0 for the key wins.
				hasT0 := false
				for i := range existing {
					if pricing.IsTemplate0(&existing[i]) {
						hasT0 = true
						break
					}
				}
				if hasT0 {
					logger.Warn("billing migration: template0 already exists, keep target side",
						"target", item.t.Target, "model", item.t.Model)
					warned++
					skipped++
					continue
				}
			} else if err := pricing.ValidateCoverageSet(append(existing, item.t)); err != nil {
				// Overlap with an existing coverage template: keep target side.
				logger.Warn("billing migration: coverage overlap with existing template, keep target side",
					"target", item.t.Target, "model", item.t.Model, "error", err)
				warned++
				skipped++
				continue
			}
			t := item.t
			t.ID = uuid.NewString()
			data, err := json.Marshal(t)
			if err != nil {
				return fmt.Errorf("encode pricing template: %w", err)
			}
			if err := pt.Put(pricingTemplateKey(t.Target, t.Model, t.ID), data); err != nil {
				return err
			}
			migrated++
		}

		mg := tx.Bucket([]byte(BucketModelGroups))
		if mg == nil {
			return fmt.Errorf("model_groups bucket missing")
		}
		for _, item := range groupsToWrite {
			if mg.Get([]byte(item.rawName)) != nil {
				logger.Warn("billing migration: model group already exists, keep target side", "group", item.rawName)
				warned++
				skipped++
				continue
			}
			if err := mg.Put([]byte(item.rawName), item.g); err != nil {
				return err
			}
			migrated++
		}

		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return fmt.Errorf("meta bucket missing")
		}
		marker, err := json.Marshal(map[string]any{
			"migrated_at": time.Now().UTC().Format(time.RFC3339),
			"migrated":    migrated,
			"skipped":     skipped,
		})
		if err != nil {
			return err
		}
		return meta.Put([]byte(billingDataMigratedMetaKey), marker)
	})
	if err != nil {
		return fmt.Errorf("billing data migration write: %w", err)
	}

	logger.Info("billing data migration done",
		"migrated", migrated, "skipped", skipped, "warned", warned,
		"segments", len(segments), "templates", len(templates), "groups", len(groups), "per_target_costs", len(perTargetCosts))
	return nil
}
