package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

func schemaTestHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(dir, 1, []string{"fixture-provider-key"})
	stores := setupTestStores(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return NewHandler(config.NewManager(filepath.Join(dir, "config.json")), auth.NewStore(), service,
		stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore,
		stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
}

func TestHandlerSchemaContract(t *testing.T) {
	h := schemaTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/data/schema", nil)
	req = req.WithContext(ContextWithSession(req.Context(), &Session{Username: "auditor", Role: RoleReadonly}))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("schema read must succeed for readonly sessions: status=%d", rec.Code)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if string(response["version"]) != "1" {
		t.Fatalf("unsupported schema version: %s", response["version"])
	}
	for _, field := range []string{"endpoint_types", "protocols", "request_schemas", "usage_adapters", "image_operations", "auth_modes", "pricing_modes", "pricing_service_tiers", "pricing_speeds", "roles", "quota_dimensions", "quota_scopes", "quota_sync_modes", "target_phases"} {
		var options []map[string]any
		if err := json.Unmarshal(response[field], &options); err != nil || len(options) == 0 {
			t.Errorf("schema field %s must contain options: %v", field, err)
			continue
		}
		seen := make(map[string]bool)
		for _, option := range options {
			key := "value"
			if field == "endpoint_types" {
				key = "code"
			}
			value, ok := option[key].(string)
			if !ok || seen[value] {
				t.Errorf("invalid or duplicate %s option: %v", field, option)
			}
			seen[value] = true
			label := "label"
			if field == "endpoint_types" {
				label = "display_name"
			}
			if text, _ := option[label].(string); text == "" {
				t.Errorf("%s option lacks label: %v", field, option)
			}
		}
	}
	for field, want := range map[string][]config.EnumOption{"pricing_service_tiers": pricing.AllServiceTierOptions(), "pricing_speeds": pricing.AllSpeedOptions()} {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(response[field]) != string(encoded) {
			t.Errorf("%s differs from domain enum", field)
		}
	}
	var defaults map[string]any
	if err := json.Unmarshal(response["defaults"], &defaults); err != nil {
		t.Fatal(err)
	}
	var adapters []config.EnumOption
	if err := json.Unmarshal(response["usage_adapters"], &adapters); err != nil || len(adapters) != 7 {
		t.Fatalf("explicit adapter options: %+v %v", adapters, err)
	}
	foundKimi := false
	for _, option := range adapters {
		if option.Value == config.UsageAdapterKimi && option.Label == "Kimi" {
			foundKimi = true
		}
	}
	if !foundKimi {
		t.Fatal("schema missing Kimi label/value")
	}
	for value, label := range map[string]string{"openrouter": "OpenRouter", "minimax": "MiniMax"} {
		found := false
		for _, option := range adapters {
			if option.Value == value && option.Label == label {
				found = true
			}
		}
		if !found {
			t.Fatalf("schema missing %s label/value", value)
		}
	}
	for field, want := range map[string]string{"endpoint_type": config.EndpointTypeAzureOpenAI, "usage_adapter": "", "image_operation": config.ImageOperationGenerations, "role": RoleAdmin} {
		if defaults[field] != want {
			t.Errorf("default %s=%q, want %q", field, defaults[field], want)
		}
	}
	for field, want := range map[string]any{"openai_prefix": "", "anthropic_prefix": "", "supports_responses": false} {
		if got, ok := defaults[field]; !ok || got != want {
			t.Errorf("canonical dual default %s=%v (present=%v), want %v", field, got, ok, want)
		}
	}
	legacy := httptest.NewRecorder()
	h.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/data/endpoint-types", nil))
	var existing struct {
		EndpointTypes json.RawMessage `json:"endpoint_types"`
	}
	if err := json.Unmarshal(legacy.Body.Bytes(), &existing); err != nil {
		t.Fatal(err)
	}
	if string(existing.EndpointTypes) != string(response["endpoint_types"]) {
		t.Error("legacy endpoint metadata differs from schema")
	}
	for _, secret := range []string{"fixture-provider-key", "password_hash", "admin_session"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("schema includes private configuration %q", secret)
		}
	}
}
