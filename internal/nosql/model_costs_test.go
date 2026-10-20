package nosql

import (
	"encoding/json"
	"testing"
)

func TestModelCostUnmarshalLegacyKeyFallback(t *testing.T) {
	// Legacy bbolt data format (old key only).
	oldJSON := []byte(`{"model":"claude-test","input_per_1m_tokens":3,"cached_input_per_1m_tokens":3.75}`)
	var m ModelCost
	if err := json.Unmarshal(oldJSON, &m); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if m.CacheWritePer1MToken != 3.75 {
		t.Errorf("legacy key fallback: got %v, want 3.75", m.CacheWritePer1MToken)
	}

	// New key takes priority when both present.
	newJSON := []byte(`{"model":"claude-test","input_per_1m_tokens":3,"cache_write_per_1m_tokens":5,"cached_input_per_1m_tokens":3.75}`)
	var m2 ModelCost
	if err := json.Unmarshal(newJSON, &m2); err != nil {
		t.Fatalf("unmarshal new+legacy: %v", err)
	}
	if m2.CacheWritePer1MToken != 5 {
		t.Errorf("new key priority: got %v, want 5", m2.CacheWritePer1MToken)
	}
}

func TestModelCostStoreUpsertAndDelete(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.1, OutputPer1MTokens: 0.2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.3, OutputPer1MTokens: 0.4}); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].InputPer1MTokens != 0.3 {
		t.Fatalf("unexpected costs: %+v", items)
	}

	if err := store.Delete("gpt-4o"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty costs, got %+v", items)
	}
}

func TestModelCostStoreEndpointTypeOverwrites(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// The key is model-only; upserting the same model with different
	// endpoint_types overwrites (last-writer-wins).
	if err := store.Upsert(ModelCost{EndpointType: "azure_openai", Model: "gpt-4o", InputPer1MTokens: 0.1}); err != nil {
		t.Fatalf("upsert azure: %v", err)
	}
	if err := store.Upsert(ModelCost{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 0.5}); err != nil {
		t.Fatalf("upsert openai: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 cost (model-only key, last writer wins), got %d: %+v", len(items), items)
	}
	if items[0].InputPer1MTokens != 0.5 {
		t.Fatalf("expected overwritten rate 0.5, got %f", items[0].InputPer1MTokens)
	}

	// DeleteByKey delegates to Delete.
	if err := store.DeleteByKey("openai", "gpt-4o"); err != nil {
		t.Fatalf("delete by key: %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete by key: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 costs after delete by key, got %d", len(items))
	}
}

func TestModelCostStoreEmptyEndpointTypeNotDefaulted(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// Empty endpoint_type is kept as-is (no longer defaulted to azure_openai).
	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 cost, got %d", len(items))
	}
	if items[0].EndpointType != "" {
		t.Fatalf("expected empty endpoint_type, got %q", items[0].EndpointType)
	}

	// DeleteByKey with empty endpoint_type delegates to Delete by model.
	if err := store.DeleteByKey("", "gpt-4o"); err != nil {
		t.Fatalf("delete by key with empty endpoint_type: %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty costs after delete, got %+v", items)
	}
}

func TestModelCostPerTargetKeys(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// 全局层与 per-target 层同 model 共存，互不覆盖。
	if err := store.Upsert(ModelCost{Model: "m1", InputPer1MTokens: 1}); err != nil {
		t.Fatalf("upsert global: %v", err)
	}
	if err := store.Upsert(ModelCost{Target: "WS-OpenAI", Model: "m1", InputPer1MTokens: 8}); err != nil {
		t.Fatalf("upsert per-target: %v", err)
	}
	if err := store.Upsert(ModelCost{Target: "ws-openai", Model: "m1", InputPer1MTokens: 9}); err != nil {
		t.Fatalf("upsert per-target replace: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 entries (global + per-target), got %+v", items)
	}
	byKey := map[string]float64{}
	for _, c := range items {
		byKey[c.Target+"|"+c.Model] = c.InputPer1MTokens
	}
	if byKey["|m1"] != 1 || byKey["ws-openai|m1"] != 9 {
		t.Fatalf("unexpected layer values: %+v", byKey)
	}

	// 通配 target 拒绝。
	if err := store.Upsert(ModelCost{Target: "*", Model: "m1"}); err == nil {
		t.Fatal("target \"*\" must be rejected in model costs")
	}

	// 删除边界：全局 Delete 不影响 per-target 记录，反之亦然。
	if err := store.DeleteForTarget("ws-openai", "m1"); err != nil {
		t.Fatalf("delete per-target: %v", err)
	}
	if _, err := store.List(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := store.Delete("m1"); err != nil {
		t.Fatalf("delete global: %v", err)
	}
	items, _ = store.List()
	if len(items) != 0 {
		t.Fatalf("expected empty after both deletes, got %+v", items)
	}
	if err := store.DeleteForTarget("ghost", "m1"); err == nil {
		t.Fatal("missing per-target delete must fail")
	}
}

func TestModelCostMigrateKeysSkipsTargetKeys(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// per-target 键（含 \x00）即使 model 是 Ollama 风格含冒号名也不被 legacy 迁移误伤。
	if err := store.Upsert(ModelCost{Target: "ws", Model: "qwen2.5:7b", InputPer1MTokens: 2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.MigrateKeys(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Target != "ws" || items[0].Model != "qwen2.5:7b" || items[0].InputPer1MTokens != 2 {
		t.Fatalf("per-target entry must survive migration untouched, got %+v", items)
	}
}
