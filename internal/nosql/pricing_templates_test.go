package nosql

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func testRates(in, out float64) usage.CostRates {
	return usage.CostRates{InputPer1MTokens: in, OutputPer1MTokens: out}
}

func mustUpsert(t *testing.T, s *PricingTemplateStore, tp pricing.PricingTemplate) pricing.PricingTemplate {
	t.Helper()
	got, err := s.Upsert(tp)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return got
}

func TestPricingTemplateUpsertListRoundtrip(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	tp := pricing.PricingTemplate{
		Target:    "MyTarget",
		Model:     "GPT-4o",
		Start:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		End:       &end,
		Mode:      pricing.ModeSchedule,
		Timezone:  "Asia/Shanghai",
		BaseRates: testRates(1.0, 2.0),
		Periods: []pricing.PricingPeriod{
			{Weekdays: []int{1, 2, 3}, Start: "09:00", End: "18:00", Rates: testRates(3.0, 4.0)},
		},
	}

	got := mustUpsert(t, store, tp)
	if got.ID == "" {
		t.Fatalf("expected non-empty ID")
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 template, got %d", len(items))
	}
	it := items[0]
	if it.ID != got.ID || it.Target != "MyTarget" || it.Model != "GPT-4o" || it.Mode != pricing.ModeSchedule {
		t.Fatalf("identity fields mismatch: %+v", it)
	}
	if it.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone mismatch: %q", it.Timezone)
	}
	if !it.Start.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("start mismatch: %v", it.Start)
	}
	if it.End == nil || !it.End.Equal(end) {
		t.Fatalf("end mismatch: %v", it.End)
	}
	if it.BaseRates.InputPer1MTokens != 1.0 || it.BaseRates.OutputPer1MTokens != 2.0 {
		t.Fatalf("base rates mismatch: %+v", it.BaseRates)
	}
	if len(it.Periods) != 1 || it.Periods[0].Start != "09:00" || it.Periods[0].Rates.InputPer1MTokens != 3.0 {
		t.Fatalf("periods mismatch: %+v", it.Periods)
	}
}

func TestPricingTemplateListByTargetModelCaseInsensitive(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	mustUpsert(t, store, pricing.PricingTemplate{
		Target: "MyTarget", Model: "GPT-4o", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 2),
	})

	items, err := store.ListByTargetModel("mytarget", "gpt-4o")
	if err != nil {
		t.Fatalf("list by target/model: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 match, got %d", len(items))
	}

	items, err = store.ListByTargetModel("mytarget", "other-model")
	if err != nil {
		t.Fatalf("list by other model: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(items))
	}
}

func TestPricingTemplateOverlapRejectedAdjacentAllowed(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	jan1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	jan5 := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)
	jan10 := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)
	jan15 := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	jan20 := time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)

	mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Start: jan1, End: &jan10, Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})

	_, err := store.Upsert(pricing.PricingTemplate{
		Target: "T", Model: "M", Start: jan5, End: &jan15, Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})
	if err == nil {
		t.Fatalf("expected overlap error, got nil")
	}

	mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Start: jan10, End: &jan20, Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})

	items, err := store.ListByTargetModel("T", "M")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 templates (adjacent allowed), got %d", len(items))
	}
}

func TestPricingTemplate0CoexistsWithCoverage(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Start: start, End: &end, Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(2, 2),
	})

	items, err := store.ListByTargetModel("T", "M")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected template 0 + coverage, got %d", len(items))
	}
}

func TestPricingTemplateUpsertReplaceByID(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	first := mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})
	if first.ID == "" {
		t.Fatalf("expected ID")
	}

	mustUpsert(t, store, pricing.PricingTemplate{
		ID: first.ID, Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(5, 6),
	})

	items, err := store.ListByTargetModel("T", "M")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 template after replace, got %d", len(items))
	}
	if items[0].BaseRates.InputPer1MTokens != 5.0 || items[0].BaseRates.OutputPer1MTokens != 6.0 {
		t.Fatalf("expected replaced rates, got %+v", items[0].BaseRates)
	}
}

func TestPricingTemplateDelete(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	got := mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})

	if err := store.Delete(got.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty after delete, got %+v", items)
	}

	if err := store.Delete(got.ID); err == nil {
		t.Fatalf("expected not-found error for missing id")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestPricingTemplateUpsertSelfHealsDriftedDuplicate(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	// Seed a template, then plant a drifted duplicate under another key
	// directly in the bucket (simulating legacy ghost data).
	first := mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})
	ghost := first
	ghost.Target = "OTHER"
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		data, err := json.Marshal(ghost)
		if err != nil {
			return err
		}
		return b.Put(pricingTemplateKey(ghost.Target, ghost.Model, ghost.ID), data)
	}); err != nil {
		t.Fatalf("seed ghost: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 entries (ghost seeded), got %d", len(items))
	}

	// Re-upsert the template under the original key: the drifted duplicate
	// carrying the same ID must be removed.
	mustUpsert(t, store, pricing.PricingTemplate{
		ID: first.ID, Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(2, 3),
	})

	items, err = store.List()
	if err != nil {
		t.Fatalf("list after heal: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 entry after self-heal, got %+v", items)
	}
	if items[0].Target != "T" || items[0].BaseRates.InputPer1MTokens != 2.0 {
		t.Fatalf("unexpected survivor: %+v", items[0])
	}
}

func TestPricingTemplateDeleteRemovesAllDuplicates(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	first := mustUpsert(t, store, pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(1, 1),
	})
	ghost := first
	ghost.Model = "OTHER-M"
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPricingTemplates))
		data, err := json.Marshal(ghost)
		if err != nil {
			return err
		}
		return b.Put(pricingTemplateKey(ghost.Target, ghost.Model, ghost.ID), data)
	}); err != nil {
		t.Fatalf("seed ghost: %v", err)
	}

	if err := store.Delete(first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected all duplicates removed, got %+v", items)
	}
}

func TestPricingTemplateUpsertValidationBubblesUp(t *testing.T) {
	db := testDB(t)
	store := NewPricingTemplateStore(db)

	_, err := store.Upsert(pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "UTC",
		BaseRates: usage.CostRates{InputPer1MTokens: -1},
	})
	if err == nil {
		t.Fatalf("expected negative-rate error, got nil")
	}

	_, err = store.Upsert(pricing.PricingTemplate{
		Target: "T", Model: "M", Mode: pricing.ModeFlat, Timezone: "Not/AZone",
		BaseRates: testRates(1, 1),
	})
	if err == nil {
		t.Fatalf("expected invalid-timezone error, got nil")
	}
}

func TestMigrateModelCostsToPricingTemplates(t *testing.T) {
	db := testDB(t)
	costs := NewModelCostStore(db)

	if err := costs.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.1, OutputPer1MTokens: 0.2}); err != nil {
		t.Fatalf("seed model cost: %v", err)
	}
	if err := costs.Upsert(ModelCost{Model: "claude", InputPer1MTokens: 0.5, OutputPer1MTokens: 0.6}); err != nil {
		t.Fatalf("seed model cost: %v", err)
	}

	if err := MigrateModelCostsToPricingTemplates(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := NewPricingTemplateStore(db)
	all, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(all))
	}

	byModel := map[string]pricing.PricingTemplate{}
	for _, tp := range all {
		byModel[tp.Model] = tp
	}
	gpt := byModel["gpt-4o"]
	if gpt.Target != "" || gpt.Mode != pricing.ModeFlat || !gpt.Start.IsZero() {
		t.Fatalf("template 0 shape wrong: %+v", gpt)
	}
	if gpt.BaseRates.InputPer1MTokens != 0.1 || gpt.BaseRates.OutputPer1MTokens != 0.2 {
		t.Fatalf("gpt rates wrong: %+v", gpt.BaseRates)
	}

	// Idempotent: second run adds no duplicates.
	if err := MigrateModelCostsToPricingTemplates(db); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	all, err = store.List()
	if err != nil {
		t.Fatalf("list after second migrate: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected still 2 templates after second migrate, got %d", len(all))
	}
}

func TestMigrateModelCostsSkipsExistingTemplate0(t *testing.T) {
	db := testDB(t)
	costs := NewModelCostStore(db)

	if err := costs.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.1, OutputPer1MTokens: 0.2}); err != nil {
		t.Fatalf("seed model cost: %v", err)
	}

	// Pre-existing template 0 with distinct rates must be preserved.
	store := NewPricingTemplateStore(db)
	mustUpsert(t, store, pricing.PricingTemplate{
		Target: "", Model: "gpt-4o", Mode: pricing.ModeFlat, Timezone: "UTC", BaseRates: testRates(9, 9),
	})

	if err := MigrateModelCostsToPricingTemplates(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	items, err := store.ListByTargetModel("", "gpt-4o")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 template 0 preserved, got %d", len(items))
	}
	if items[0].BaseRates.InputPer1MTokens != 9.0 {
		t.Fatalf("expected preserved rates 9, got %+v", items[0].BaseRates)
	}
}

func TestMigrateModelCostsDoesNotPromoteTargetPriceToGlobal(t *testing.T) {
	db := testDB(t)
	costs := NewModelCostStore(db)
	// The target key sorts before the global model key. Migration order must
	// never let a scoped rate replace the independent global fallback.
	for _, cost := range []ModelCost{
		{Target: "a-target", Model: "qwen-max", InputPer1MTokens: 9},
		{Model: "qwen-max", InputPer1MTokens: 2},
		{Target: "a-target", Model: "target-only", InputPer1MTokens: 7},
	} {
		if err := costs.Upsert(cost); err != nil {
			t.Fatal(err)
		}
	}
	if err := MigrateModelCostsToPricingTemplates(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateBillingData(db, nil); err != nil {
		t.Fatal(err)
	}
	templates, err := NewPricingTemplateStore(db).List()
	if err != nil {
		t.Fatal(err)
	}
	resolver := pricing.NewResolver(templates, nil)
	for _, tc := range []struct {
		target, model string
		want          float64
		priced        bool
	}{
		{"a-target", "qwen-max", 9, true},
		{"another-target", "qwen-max", 2, true},
		{"a-target", "target-only", 7, true},
		{"another-target", "target-only", 0, false},
	} {
		rates, ok := resolver.RateFor(tc.target, tc.model, time.Now())
		if ok != tc.priced || rates.InputPer1MTokens != tc.want {
			t.Errorf("%s/%s: got price %v priced=%v, want %v priced=%v", tc.target, tc.model, rates.InputPer1MTokens, ok, tc.want, tc.priced)
		}
	}
}
