package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestPoolRoutingStrategyCRUD(t *testing.T) {
	h, _, stores, _ := newPoolTestHandler(t, t.TempDir())
	pools := stores.targetStore.Pools()

	if rec := postPool(t, h, map[string]any{"name": "legacy"}); rec.Code != http.StatusCreated {
		t.Fatalf("create default: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetPool(t, pools, "legacy"); got.EffectiveRoutingStrategy() != config.PoolRoutingLoad {
		t.Fatalf("default strategy = %q", got.RoutingStrategy)
	}

	if rec := postPool(t, h, map[string]any{"name": "ds", "display_name": "DS", "routing_strategy": " Cost "}); rec.Code != http.StatusCreated {
		t.Fatalf("create cost: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetPool(t, pools, "ds"); got.RoutingStrategy != config.PoolRoutingCost {
		t.Fatalf("stored strategy = %q", got.RoutingStrategy)
	}

	// Partial updates keep the strategy; an explicit value replaces it.
	if rec := putPool(t, h, "ds", map[string]any{"description": "峰谷分流"}); rec.Code != http.StatusOK {
		t.Fatalf("update description: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetPool(t, pools, "ds"); got.RoutingStrategy != config.PoolRoutingCost {
		t.Fatalf("partial update dropped strategy: %q", got.RoutingStrategy)
	}
	if rec := putPool(t, h, "ds", map[string]any{"routing_strategy": "load"}); rec.Code != http.StatusOK {
		t.Fatalf("update strategy: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetPool(t, pools, "ds"); got.RoutingStrategy != config.PoolRoutingLoad {
		t.Fatalf("updated strategy = %q", got.RoutingStrategy)
	}

	for _, invalid := range []map[string]any{{"name": "bad", "routing_strategy": "cheapest"}} {
		if rec := postPool(t, h, invalid); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid create accepted: %d", rec.Code)
		}
	}
	if rec := putPool(t, h, "ds", map[string]any{"routing_strategy": "cheapest"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid update accepted: %d", rec.Code)
	}
	if got := mustGetPool(t, pools, "ds"); got.RoutingStrategy != config.PoolRoutingLoad {
		t.Fatalf("rejected update changed strategy: %q", got.RoutingStrategy)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/pools", nil))
	var listed struct {
		Pools []config.DeploymentPool `json:"pools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range listed.Pools {
		if p.Name == "ds" {
			found = p.RoutingStrategy == config.PoolRoutingLoad
		}
	}
	if !found {
		t.Fatalf("list lost strategy: %s", rec.Body.String())
	}
}

func TestSchemaPublishesPoolRoutingStrategies(t *testing.T) {
	h, _, _, _ := newPoolTestHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/schema", nil))
	var schema struct {
		Strategies []config.EnumOption `json:"pool_routing_strategies"`
		Defaults   struct {
			Strategy string `json:"pool_routing_strategy"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Strategies) != 2 || schema.Strategies[0].Value != config.PoolRoutingLoad || schema.Strategies[1].Value != config.PoolRoutingCost {
		t.Fatalf("strategies = %#v", schema.Strategies)
	}
	if schema.Defaults.Strategy != config.PoolRoutingLoad {
		t.Fatalf("default = %q", schema.Defaults.Strategy)
	}
}
