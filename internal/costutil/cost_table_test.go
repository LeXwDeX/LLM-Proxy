package costutil

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// TestToCostTable_Layer1Only verifies catalog entries are loaded into the table
// when no custom costs are provided.
func TestToCostTable_Layer1Only(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	table := ToCostTable(nil, cat)
	// gpt-4o should be in catalog with default cost
	rate, ok := table.LookupCost("gpt-4o")
	if !ok {
		t.Fatal("expected catalog default cost for gpt-4o")
	}
	if rate.InputPer1MTokens <= 0 || rate.OutputPer1MTokens <= 0 {
		t.Fatalf("expected positive rates, got %+v", rate)
	}
}

// TestToCostTable_Layer2Override verifies custom costs override catalog defaults.
func TestToCostTable_Layer2Override(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	customCosts := []nosql.ModelCost{
		{Model: "gpt-4o", InputPer1MTokens: 999, OutputPer1MTokens: 888},
	}
	table := ToCostTable(customCosts, cat)
	rate, ok := table.LookupCost("gpt-4o")
	if !ok {
		t.Fatal("expected cost for gpt-4o after custom override")
	}
	if rate.InputPer1MTokens != 999 || rate.OutputPer1MTokens != 888 {
		t.Fatalf("expected custom rates (999, 888), got %+v", rate)
	}
}

// TestToCostTable_Layer2NewModel verifies custom costs for models not in catalog.
func TestToCostTable_Layer2NewModel(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	customCosts := []nosql.ModelCost{
		{Model: "custom-model-xyz", InputPer1MTokens: 42, OutputPer1MTokens: 84},
	}
	table := ToCostTable(customCosts, cat)
	rate, ok := table.LookupCost("custom-model-xyz")
	if !ok {
		t.Fatal("expected cost for custom-model-xyz")
	}
	if rate.InputPer1MTokens != 42 || rate.OutputPer1MTokens != 84 {
		t.Fatalf("expected (42, 84), got %+v", rate)
	}
}

// TestToCostTable_ModelOnlyKey verifies the table uses model-only keys (no composite ep:model keys).
func TestToCostTable_ModelOnlyKey(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	table := ToCostTable(nil, cat)
	// "gpt-4o" model-only key should exist
	rate, ok := table.LookupCost("gpt-4o")
	if !ok {
		t.Fatal("expected gpt-4o key in table")
	}
	if rate.InputPer1MTokens <= 0 {
		t.Fatalf("expected positive rate, got %+v", rate)
	}
	// No composite keys should exist (spot check for "openai:gpt-4o")
	_, hasComposite := table["openai:gpt-4o"]
	if hasComposite {
		t.Fatal("should not have composite 'openai:gpt-4o' key in model-only table")
	}
}

// TestToCostTable_CustomNoCat verifies custom cost without catalog writes model-only key.
func TestToCostTable_CustomNoCat(t *testing.T) {
	customCosts := []nosql.ModelCost{
		{Model: "gpt-4o", InputPer1MTokens: 100, OutputPer1MTokens: 200},
	}
	table := ToCostTable(customCosts, nil)
	// Should be accessible by model-only key
	rate, ok := table["gpt-4o"]
	if !ok {
		t.Fatal("expected gpt-4o key")
	}
	if rate.InputPer1MTokens != 100 || rate.OutputPer1MTokens != 200 {
		t.Fatalf("expected (100, 200), got %+v", rate)
	}
	// No composite keys
	_, ok = table["openai:gpt-4o"]
	if ok {
		t.Fatal("should not have composite 'openai:gpt-4o' key")
	}
}

// TestToCostTable_NilCatalog verifies nil catalog only uses custom costs.
func TestToCostTable_NilCatalog(t *testing.T) {
	customCosts := []nosql.ModelCost{
		{Model: "gpt-4o", InputPer1MTokens: 55, OutputPer1MTokens: 66},
	}
	table := ToCostTable(customCosts, nil)
	rate, ok := table.LookupCost("gpt-4o")
	if !ok || rate.InputPer1MTokens != 55 {
		t.Fatalf("expected custom rate with nil catalog, got ok=%v rate=%+v", ok, rate)
	}
	// gpt-4o-mini not in custom, should not exist
	_, ok = table.LookupCost("gpt-4o-mini")
	if ok {
		t.Fatal("expected no cost for gpt-4o-mini with nil catalog and no custom entry")
	}
}

// TestToCostTable_EmptyCosts verifies empty custom costs returns only catalog prices.
func TestToCostTable_EmptyCosts(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	table := ToCostTable([]nosql.ModelCost{}, cat)
	rate, ok := table.LookupCost("gpt-4o")
	if !ok {
		t.Fatal("expected catalog default for gpt-4o with empty costs")
	}
	if rate.InputPer1MTokens <= 0 {
		t.Fatalf("expected positive rate, got %+v", rate)
	}
}

// TestToCostTable_SkipsPerTarget verifies per-target entries do not pollute
// the global table, and ToTargetCostTable extracts them into the exact layer.
func TestToCostTable_SkipsPerTarget(t *testing.T) {
	costs := []nosql.ModelCost{
		{Model: "m-global", InputPer1MTokens: 1},
		{Target: "ws-openai", Model: "m-shared", InputPer1MTokens: 8},
	}
	global := ToCostTable(costs, nil)
	if _, ok := global.LookupCost("m-global"); !ok {
		t.Fatal("global entry must stay in the global table")
	}
	if _, ok := global.LookupCost("m-shared"); ok {
		t.Fatal("per-target entry must not leak into the global table")
	}

	exact := ToTargetCostTable(costs)
	rate, ok := exact["ws-openai"]["m-shared"]
	if !ok || rate.InputPer1MTokens != 8 {
		t.Fatalf("expected ws-openai/m-shared (8) in exact layer, got ok=%v rate=%+v", ok, rate)
	}
	if _, ok := exact["ws-openai"]["m-global"]; ok {
		t.Fatal("global entry must not leak into the exact layer")
	}
}
