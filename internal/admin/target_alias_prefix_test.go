package admin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Issue #157 F1: admin writes with a legacy endpoint_type alias must apply the
// same vendor defaults as config loading (config.NormalizeTargetForCompatibility).

type prefixWant struct {
	epType, openai, anthropic string
}

func assertTargetPrefixes(t *testing.T, manager *config.Manager, stores testStores, name string, want prefixWant) {
	t.Helper()
	check := func(src string, got config.Target) {
		t.Helper()
		if got.EndpointType != want.epType || got.OpenAIPrefix != want.openai || got.AnthropicPrefix != want.anthropic {
			t.Fatalf("%s: got type=%q openai_prefix=%q anthropic_prefix=%q, want type=%q openai_prefix=%q anthropic_prefix=%q",
				src, got.EndpointType, got.OpenAIPrefix, got.AnthropicPrefix, want.epType, want.openai, want.anthropic)
		}
	}
	check("store", mustGetTarget(t, stores.targetStore, name))
	cfg, err := manager.Current()
	if err != nil {
		t.Fatalf("current config: %v", err)
	}
	for _, tg := range cfg.Targets {
		if strings.EqualFold(tg.Name, name) {
			check("runtime config", tg)
			return
		}
	}
	t.Fatalf("target %q not in runtime config", name)
}

func aliasCreateBody(name, epType string) map[string]any {
	return map[string]any{
		"name":           name,
		"endpoint_type":  epType,
		"usage_adapter":  config.UsageAdapterStandard,
		"endpoint":       "https://vendor.example.com",
		"api_key":        "sk-alias-real-0001",
		"model_mappings": []map[string]any{{"upstream": "vendor-chat"}},
	}
}

func TestCreateTargetAliasAppliesVendorDefaultPrefixes(t *testing.T) {
	cases := []struct {
		alias string
		want  prefixWant
	}{
		{"deepseek", prefixWant{config.EndpointTypeDualProtocol, "", "/anthropic"}},
		{"bailian", prefixWant{config.EndpointTypeDualProtocol, "/compatible-mode", "/apps/anthropic"}},
	}
	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			h, manager, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-real-0001"})
			rec := wsCreateTarget(t, h, aliasCreateBody("alias-target", tc.alias))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create expected 201, got %d body=%s", rec.Code, rec.Body.String())
			}
			assertTargetPrefixes(t, manager, stores, "alias-target", tc.want)
		})
	}
}

func TestCreateTargetAliasKeepsExplicitPrefixes(t *testing.T) {
	h, manager, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-real-0001"})
	body := aliasCreateBody("alias-target", "bailian")
	body["openai_prefix"] = "/custom-openai"
	body["anthropic_prefix"] = "/custom-anthropic"
	rec := wsCreateTarget(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertTargetPrefixes(t, manager, stores, "alias-target",
		prefixWant{config.EndpointTypeDualProtocol, "/custom-openai", "/custom-anthropic"})
}

func TestUpdateTargetAliasAppliesVendorDefaultPrefixes(t *testing.T) {
	h, manager, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-real-0001"})
	body := targetUpdateBody("__existing_key_index__:0", nil)
	body["endpoint_type"] = "bailian"
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertTargetPrefixes(t, manager, stores, "target-1",
		prefixWant{config.EndpointTypeDualProtocol, "/compatible-mode", "/apps/anthropic"})
}

func TestUpdateTargetAliasKeepsExplicitAndStoredPrefixes(t *testing.T) {
	h, manager, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"sk-primary-real-0001"})
	create := aliasCreateBody("alias-target", "dual_protocol")
	create["anthropic_prefix"] = "/stored-anthropic"
	if rec := wsCreateTarget(t, h, create); rec.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	// PUT with alias, omitting anthropic_prefix but sending an explicit openai_prefix:
	// the stored anthropic prefix and the explicit openai prefix must both survive.
	body := map[string]any{
		"endpoint_type":  "bailian",
		"api_key":        "__existing_key_index__:0",
		"openai_prefix":  "/explicit-openai",
		"model_mappings": []map[string]any{{"upstream": "vendor-chat"}},
	}
	rec := putTarget(t, h, "alias-target", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertTargetPrefixes(t, manager, stores, "alias-target",
		prefixWant{config.EndpointTypeDualProtocol, "/explicit-openai", "/stored-anthropic"})

	// PUT without endpoint_type and without prefix fields: nothing is cleared.
	rec = putTarget(t, h, "alias-target", map[string]any{"api_key": "__existing_key_index__:0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("second update expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertTargetPrefixes(t, manager, stores, "alias-target",
		prefixWant{config.EndpointTypeDualProtocol, "/explicit-openai", "/stored-anthropic"})
}
