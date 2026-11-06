package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPoolRoutingStrategyNormalizeAndValidate(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"", "", true},
		{" LOAD ", PoolRoutingLoad, true},
		{"Cost", PoolRoutingCost, true},
		{"cheapest", "cheapest", false},
	}
	for _, tc := range cases {
		got := NormalizeDeploymentPool(DeploymentPool{Name: "p", RoutingStrategy: tc.in})
		if got.RoutingStrategy != tc.want {
			t.Fatalf("normalize %q = %q, want %q", tc.in, got.RoutingStrategy, tc.want)
		}
		if IsValidPoolRoutingStrategy(got.RoutingStrategy) != tc.valid {
			t.Fatalf("valid(%q) = %v, want %v", got.RoutingStrategy, !tc.valid, tc.valid)
		}
	}
	if (DeploymentPool{}).EffectiveRoutingStrategy() != PoolRoutingLoad {
		t.Fatal("empty strategy must mean load")
	}
	if (DeploymentPool{RoutingStrategy: PoolRoutingCost}).EffectiveRoutingStrategy() != PoolRoutingCost {
		t.Fatal("cost strategy must be effective")
	}
}

func TestPoolRoutingStrategyOptionsArePublicEnum(t *testing.T) {
	values := EnumValues(AllPoolRoutingStrategyOptions())
	if strings.Join(values, ",") != "load,cost" {
		t.Fatalf("options = %v", values)
	}
	for _, v := range values {
		if !IsValidPoolRoutingStrategy(v) {
			t.Fatalf("option %q must validate", v)
		}
	}
}

func TestPoolRoutingStrategyLegacyJSONOmitted(t *testing.T) {
	var pool DeploymentPool
	if err := json.Unmarshal([]byte(`{"name":"legacy","paused":true}`), &pool); err != nil {
		t.Fatal(err)
	}
	if pool.EffectiveRoutingStrategy() != PoolRoutingLoad {
		t.Fatalf("legacy pool strategy = %q", pool.EffectiveRoutingStrategy())
	}
	data, err := json.Marshal(DeploymentPool{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "routing_strategy") {
		t.Fatalf("default strategy must stay omitted: %s", data)
	}
}

func TestConfigValidateRejectsUnknownPoolRoutingStrategy(t *testing.T) {
	cfg := validConfigForEndpointCompatTest()
	cfg.Pools = []DeploymentPool{{Name: "p", RoutingStrategy: "cheapest"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "routing_strategy") {
		t.Fatalf("Validate error = %v, want routing_strategy rejection", err)
	}
	cfg.Pools = []DeploymentPool{{Name: "p", RoutingStrategy: PoolRoutingCost}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cost strategy must validate: %v", err)
	}
}
