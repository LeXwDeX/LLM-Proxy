package config

import (
	"encoding/json"
	"testing"
)

func TestDeploymentPoolDisplayNameNormalization(t *testing.T) {
	for _, tc := range []struct{ raw, name, display string }{
		{`{"name":" OpenAI-PLAN ","description":" Desc "}`, "openai-plan", "OpenAI-PLAN"},
		{`{"name":"openai-plan","display_name":" OpenAI-PLAN "}`, "openai-plan", "OpenAI-PLAN"},
		{`{"name":"legacy"}`, "legacy", "legacy"},
	} {
		var pool DeploymentPool
		if err := json.Unmarshal([]byte(tc.raw), &pool); err != nil {
			t.Fatal(err)
		}
		pool = NormalizeDeploymentPool(pool)
		data, _ := json.Marshal(pool)
		var result map[string]any
		_ = json.Unmarshal(data, &result)
		if pool.Name != tc.name || result["display_name"] != tc.display {
			t.Fatalf("normalized = %s, want identity %q display %q", data, tc.name, tc.display)
		}
		if NormalizeDeploymentPool(pool) != pool {
			t.Fatal("normalization must be idempotent")
		}
	}
}
