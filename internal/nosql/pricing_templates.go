package nosql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
)

// pricingTemplatesMigratedMetaKey marks completion of the one-shot model_costs
// → pricing_templates migration (v1 schema).
const pricingTemplatesMigratedMetaKey = "pricing_templates_migrated_v1"

// PricingTemplateStore manages time-based pricing templates backed by bbolt.
type PricingTemplateStore struct {
	db *bolt.DB
}

// NewPricingTemplateStore creates a new bbolt-backed pricing template store.
func NewPricingTemplateStore(db *bolt.DB) *PricingTemplateStore {
	return &PricingTemplateStore{db: db}
}

// pricingTemplateKey builds the composite key for a template. target/model are
// lower-cased and trimmed (case-insensitive matching) before quotaKeyEncode;
// the value retains original case for display.
func pricingTemplateKey(target, model, id string) []byte {
	return []byte(quotaKeyEncode(strings.ToLower(strings.TrimSpace(target))) + "|" +
		quotaKeyEncode(strings.ToLower(strings.TrimSpace(model))) + "|" + id)
}

// pricingTemplatePrefix builds the stable prefix shared by all templates for a
// (target, model) pair, used when matching both legacy and shared bindings.
func pricingTemplatePrefix(target, model string) []byte {
	return []byte(quotaKeyEncode(strings.ToLower(strings.TrimSpace(target))) + "|" +
		quotaKeyEncode(strings.ToLower(strings.TrimSpace(model))) + "|")
}

// scanTemplatesByPrefix decodes templates with any binding matching the prefix.
// It must be called within an open transaction (view or write).
func scanTemplatesByPrefix(b *bolt.Bucket, prefix []byte) ([]pricing.PricingTemplate, error) {
	var templates []pricing.PricingTemplate
	err := b.ForEach(func(k, v []byte) error {
		var t pricing.PricingTemplate
		if err := json.Unmarshal(v, &t); err != nil {
			return fmt.Errorf("decode pricing template %q: %w", string(k), err)
		}
		// Old rows use one composite key; shared rows use their first binding.
		// Inspect values so all bindings are visible without duplicating price data.
		for _, target := range t.EffectiveTargets() {
			for _, model := range t.EffectiveModels() {
				if len(prefix) == 0 || bytes.Equal(pricingTemplatePrefix(target, model), prefix) {
					templates = append(templates, t)
					return nil
				}
			}
		}
		return nil
	})
	return templates, err
}

// List returns all pricing templates.
func (s *PricingTemplateStore) List() ([]pricing.PricingTemplate, error) {
	var templates []pricing.PricingTemplate
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var t pricing.PricingTemplate
			if err := json.Unmarshal(v, &t); err != nil {
				return fmt.Errorf("decode pricing template %q: %w", string(k), err)
			}
			templates = append(templates, t)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return templates, nil
}

// ListByTargetModel returns templates with a binding that matches
// (case-insensitive, trimmed).
func (s *PricingTemplateStore) ListByTargetModel(target, model string) ([]pricing.PricingTemplate, error) {
	prefix := pricingTemplatePrefix(target, model)
	var templates []pricing.PricingTemplate
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		if b == nil {
			return nil
		}
		var err error
		templates, err = scanTemplatesByPrefix(b, prefix)
		return err
	})
	if err != nil {
		return nil, err
	}
	return templates, nil
}

// Upsert inserts or updates one template. It normalizes, validates the single
// template AND atomically validates all intersecting bindings and time ranges.
// Each binding has one fallback. If t.ID is empty, generates a short
// unique ID. Returns the stored template (with ID).
func (s *PricingTemplateStore) Upsert(t pricing.PricingTemplate) (pricing.PricingTemplate, error) {
	if err := pricing.ValidateBindings(&t); err != nil {
		return pricing.PricingTemplate{}, err
	}
	pricing.NormalizeTemplate(&t)
	if err := pricing.ValidateTemplate(&t); err != nil {
		return pricing.PricingTemplate{}, err
	}
	t.ID = strings.TrimSpace(t.ID)
	if t.ID == "" {
		t.ID = uuid.NewString()
	}

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		if b == nil {
			return fmt.Errorf("pricing_templates bucket missing")
		}
		existing, err := scanTemplatesByPrefix(b, nil)
		if err != nil {
			return err
		}
		candidate := make([]pricing.PricingTemplate, 0, len(existing)+1)
		for _, e := range existing {
			if e.ID == t.ID {
				continue
			}
			candidate = append(candidate, e)
		}
		candidate = append(candidate, t)
		if err := pricing.ValidateCoverageSet(candidate); err != nil {
			return err
		}

		data, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("encode pricing template: %w", err)
		}
		newKey := pricingTemplateKey(t.Target, t.Model, t.ID)
		// Self-heal: remove stale entries carrying the same ID under a drifted
		// (target, model) key so one template can never bill from two keys.
		if err := b.ForEach(func(k, v []byte) error {
			if bytes.Equal(k, newKey) {
				return nil
			}
			var e pricing.PricingTemplate
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("decode pricing template %q: %w", string(k), err)
			}
			if e.ID == t.ID {
				return b.Delete(k)
			}
			return nil
		}); err != nil {
			return err
		}
		return b.Put(newKey, data)
	})
	if err != nil {
		return pricing.PricingTemplate{}, err
	}
	return t, nil
}

// Get returns the template with the given ID, or nil when not found.
func (s *PricingTemplateStore) Get(id string) (*pricing.PricingTemplate, error) {
	var found *pricing.PricingTemplate
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var t pricing.PricingTemplate
			if err := json.Unmarshal(v, &t); err != nil {
				return fmt.Errorf("decode pricing template %q: %w", string(k), err)
			}
			if t.ID == id {
				found = &t
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// Delete removes every template entry carrying the given ID (duplicate keys
// under drifted target/model prefixes are all removed). Returns an error
// ("not found") when no such template exists.
func (s *PricingTemplateStore) Delete(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		if b == nil {
			return fmt.Errorf("pricing template %q not found", id)
		}
		var deleteKeys [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var t pricing.PricingTemplate
			if err := json.Unmarshal(v, &t); err != nil {
				return fmt.Errorf("decode pricing template %q: %w", string(k), err)
			}
			if t.ID == id {
				deleteKeys = append(deleteKeys, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(deleteKeys) == 0 {
			return fmt.Errorf("pricing template %q not found", id)
		}
		for _, k := range deleteKeys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// MigrateModelCostsToPricingTemplates seeds a flat template 0 (default target,
// all-day fallback) for every model_costs entry. Idempotent via a meta marker.
func MigrateModelCostsToPricingTemplates(db *bolt.DB) error {
	var done bool
	err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(BucketMeta)); b != nil {
			done = b.Get([]byte(pricingTemplatesMigratedMetaKey)) != nil
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check pricing templates migration meta: %w", err)
	}
	if done {
		return nil
	}

	costs, err := NewModelCostStore(db).List()
	if err != nil {
		return fmt.Errorf("list model costs for pricing templates migration: %w", err)
	}

	type template0 struct {
		model string
		t     pricing.PricingTemplate
	}
	var toWrite []template0
	for _, cost := range costs {
		// Scoped costs belong to the separate billing-data migration. Promoting
		// them to the default target would charge unrelated providers this price.
		if strings.TrimSpace(cost.Target) != "" {
			continue
		}
		if strings.TrimSpace(cost.Model) == "" {
			continue
		}
		t := pricing.PricingTemplate{
			Target:   "",
			Model:    cost.Model,
			Mode:     pricing.ModeFlat,
			Timezone: pricing.DefaultTimezone,
			BaseRates: usage.CostRates{
				InputPer1MTokens:            cost.InputPer1MTokens,
				OutputPer1MTokens:           cost.OutputPer1MTokens,
				CacheWritePer1MToken:        cost.CacheWritePer1MToken,
				CacheReadPer1MToken:         cost.CacheReadPer1MToken,
				ExplicitCacheReadPer1MToken: usage.CloneRate(cost.ExplicitCacheReadPer1MToken),
				CacheWrite5mPer1MToken:      usage.CloneRate(cost.CacheWrite5mPer1MToken),
				CacheWrite1hPer1MToken:      usage.CloneRate(cost.CacheWrite1hPer1MToken),
				ImageInputPer1MTokens:       usage.CloneRate(cost.ImageInputPer1MTokens),
				CachedImageInputPer1MTokens: usage.CloneRate(cost.CachedImageInputPer1MTokens),
				ImageOutputPer1MTokens:      usage.CloneRate(cost.ImageOutputPer1MTokens),
			},
			// Start zero (sentinel), End nil, no ID → generate one.
		}
		toWrite = append(toWrite, template0{model: cost.Model, t: t})
	}

	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		if b == nil {
			return fmt.Errorf("pricing_templates bucket missing")
		}
		for _, item := range toWrite {
			prefix := pricingTemplatePrefix("", item.model)
			existing, err := scanTemplatesByPrefix(b, prefix)
			if err != nil {
				return err
			}
			hasTemplate0 := false
			for i := range existing {
				if pricing.IsTemplate0(&existing[i]) {
					hasTemplate0 = true
					break
				}
			}
			if hasTemplate0 {
				continue
			}
			t := item.t
			t.ID = uuid.NewString()
			data, err := json.Marshal(t)
			if err != nil {
				return fmt.Errorf("encode pricing template: %w", err)
			}
			if err := b.Put(pricingTemplateKey(t.Target, t.Model, t.ID), data); err != nil {
				return err
			}
		}

		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return fmt.Errorf("meta bucket missing")
		}
		marker, err := json.Marshal(map[string]any{
			"migrated_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			return err
		}
		return meta.Put([]byte(pricingTemplatesMigratedMetaKey), marker)
	})
}
