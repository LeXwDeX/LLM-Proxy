package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidateSuccess(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
		},
		Targets: []Target{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings:      []ModelMapping{{Upstream: "gpt-4o"}},
		}},
		DataStore: DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no validation error, got %v", err)
	}
}

// TestConfigValidateCommaFallbackSuccess confirms a comma-separated fallback
// list validates cleanly when all aliases are distinct from each upstream.
func TestConfigValidateCommaFallbackSuccess(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
		},
		Targets: []Target{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings: []ModelMapping{{
				Upstream: "gpt-4o",
				Fallback: "gpt-4, gpt4-turbo, my-gpt",
			}},
		}},
		DataStore: DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no validation error, got %v", err)
	}
}

// TestConfigValidateCommaFallbackConflicts confirms that a duplicate alias
// within a comma-separated fallback, and an alias colliding with an upstream
// name, are both reported as validation problems.
func TestConfigValidateCommaFallbackConflicts(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
		},
		Targets: []Target{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings: []ModelMapping{
				// alias "gpt-4o" collides with the upstream name above.
				{Upstream: "gpt-4o", Fallback: "gpt-4, gpt-4o"},
				// alias "dup" repeats across mappings.
				{Upstream: "gpt-4o-mini", Fallback: "dup, mini"},
				{Upstream: "gpt-5", Fallback: "dup"},
			},
		}},
		DataStore: DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for colliding/duplicate aliases")
	}
	msg := err.Error()
	if !strings.Contains(msg, "conflicts with an upstream name") {
		t.Errorf("expected upstream-collision problem, got: %s", msg)
	}
	if !strings.Contains(msg, "is duplicate") {
		t.Errorf("expected duplicate-alias problem, got: %s", msg)
	}
}

func TestConfigValidateAllowsBearerWithoutAPIKey(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
		},
		Targets: []Target{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			AllowBearer:        true,
		}},
		DataStore: DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no validation error when allow_bearer_passthrough is true, got %v", err)
	}
}

func TestConfigValidateAllowsOmittedAPIVersionField(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
		},
		Targets: []Target{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings:      []ModelMapping{{Upstream: "gpt-4o"}},
		}},
		DataStore: DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no validation error when API version field is omitted, got %v", err)
	}
}

func TestConfigValidateErrors(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{},
		Targets: []Target{{}},
		Logging: LoggingConfig{},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for incomplete configuration")
	}
}

func TestConfigCloneProducesDeepCopy(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "0.0.0.0:8080",
			RequestTimeoutSeconds: 15,
		},
		Targets: []Target{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			Paused:             true,
			ModelMappings:      []ModelMapping{{Upstream: "gpt-4o"}},
			CustomHeaders:      map[string]string{"X-Custom": "value"},
			CustomBody:         map[string]any{"temperature": 0.7},
		}},
		DataStore: DataStore{DBPath: "test.db"},
		DataFiles: DataFiles{
			ClientsFile:     "config/clients.json",
			ModelCostsFile:  "config/model_costs.json",
			UsageEventsFile: "config/usage_events.jsonl",
		},
		Logging: LoggingConfig{
			Level:     "debug",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	cloned := cfg.Clone()
	if cloned == cfg {
		t.Fatal("expected clone to allocate new struct")
	}

	cloned.Server.Bind = "127.0.0.1:9999"
	cloned.Targets[0].Name = "secondary"
	cloned.Targets[0].ModelMappings[0].Upstream = "other"
	cloned.Targets[0].CustomHeaders["X-Custom"] = "mutated"
	cloned.Targets[0].CustomBody["temperature"] = 0.9
	cloned.DataFiles.ClientsFile = "other/clients.json"
	cloned.DataStore.DBPath = "other.db"

	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("original server bind mutated: %s", cfg.Server.Bind)
	}
	if cfg.Targets[0].Name != "primary" {
		t.Errorf("original target mutated: %s", cfg.Targets[0].Name)
	}
	if cfg.Targets[0].ModelMappings[0].Upstream != "gpt-4o" {
		t.Errorf("original target model mappings mutated: %v", cfg.Targets[0].ModelMappings)
	}
	if cfg.Targets[0].CustomHeaders["X-Custom"] != "value" {
		t.Errorf("original target custom_headers mutated: %v", cfg.Targets[0].CustomHeaders)
	}
	if cfg.Targets[0].CustomBody["temperature"] != 0.7 {
		t.Errorf("original target custom_body mutated: %v", cfg.Targets[0].CustomBody)
	}
	if !cfg.Targets[0].Paused || !cloned.Targets[0].Paused {
		t.Fatalf("expected paused to be preserved in clone")
	}
	if cfg.DataFiles.ClientsFile != "config/clients.json" {
		t.Errorf("original data_files mutated: %s", cfg.DataFiles.ClientsFile)
	}
	if cfg.DataStore.DBPath != "test.db" {
		t.Errorf("original data_store mutated: %s", cfg.DataStore.DBPath)
	}
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
		"server":{
			"bind":"0.0.0.0:8080",
			"request_timeout_seconds":10
		},
		"targets":[{
			"name":"primary",
			"endpoint":"https://example.com",
			"resource_path_prefix":"/openai",
			"api_key":"key",
			"paused":true
		}],
		"data_store":{
			"db_path":"llms-proxy.db"
		},
		"data_files":{
			"clients_file":"clients.json",
			"model_costs_file":"model_costs.json",
			"usage_events_file":"usage_events.jsonl",
			"admin_users_file":"admin_users.json",
			"admin_audit_file":"admin_audit.jsonl"
		},
		"admin_session":{
			"cookie_name":"admin_sid",
			"secret":"test-secret",
			"ttl_seconds":3600
		},
		"logging":{
			"level":"info",
			"access_log":"logs/access.log",
			"error_log":"logs/error.log"
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}

	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("unexpected bind: %s", cfg.Server.Bind)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Name != "primary" {
		t.Fatalf("unexpected targets: %#v", cfg.Targets)
	}
	if !cfg.Targets[0].Paused {
		t.Fatalf("expected paused target flag to be loaded")
	}
	if cfg.DataFiles.ClientsFile != filepath.Join(dir, "clients.json") {
		t.Fatalf("expected clients file resolved to absolute path, got %q", cfg.DataFiles.ClientsFile)
	}
}

func TestNormalizeEndpointType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", EndpointTypeAzureOpenAI},
		{"  ", EndpointTypeAzureOpenAI},
		{"azure_openai", EndpointTypeAzureOpenAI},
		{"Azure_OpenAI", EndpointTypeAzureOpenAI},
		{"  AZURE_OPENAI  ", EndpointTypeAzureOpenAI},
		{"openai", EndpointTypeOpenAI},
		{"OpenAI", EndpointTypeOpenAI},
		{"claude", EndpointTypeClaude},
		{"CLAUDE", EndpointTypeClaude},
		{"gemini", EndpointTypeGemini},
		{"GEMINI", EndpointTypeGemini},
		{"openai_image", EndpointTypeOpenAIImage},
		{"OpenAI_Image", EndpointTypeOpenAIImage},
		{"dual_protocol", EndpointTypeDualProtocol},
		{"Dual_Protocol", EndpointTypeDualProtocol},
		{"unknown", "unknown"},
		// Legacy/vendor aliases — resolved via EndpointTypeMeta.Aliases
		{"wangsu_openai", EndpointTypeOpenAI},
		{"wangsu_claude", EndpointTypeClaude},
		{"wangsu_gemini", EndpointTypeGemini},
		{"wangsu_openai_image", EndpointTypeOpenAIImage},
		{"wangsu_openai_image_edit", EndpointTypeOpenAIImage},
		{"deepseek", EndpointTypeDualProtocol},
		{"bailian", EndpointTypeDualProtocol},
		{"bailian_api", EndpointTypeDualProtocol},
	}
	for _, tt := range tests {
		got := NormalizeEndpointType(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeEndpointType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestConfigValidateNormalizesLegacyEndpointTypes(t *testing.T) {
	cfg := validConfigForEndpointCompatTest()
	cfg.Targets = []Target{
		{Name: "bailian-api", EndpointType: "bailian_api", Endpoint: "https://dashscope.example.com", APIKey: "key"},
		{Name: "deepseek", EndpointType: "deepseek", Endpoint: "https://deepseek.example.com", APIKey: "key"},
		{Name: "deepseek-custom", EndpointType: "deepseek", Endpoint: "https://deepseek.example.com", APIKey: "key", AnthropicPrefix: "/custom-anthropic"},
		{Name: "wangsu-gemini", EndpointType: "wangsu_gemini", Endpoint: "https://gemini.example.com", APIKey: "key"},
		{Name: "wangsu-image-edit", EndpointType: "wangsu_openai_image_edit", Endpoint: "https://image.example.com/v1/images/edits", APIKey: "key"},
		{Name: "openai-image-variations", EndpointType: "openai_image", Endpoint: "https://image.example.com/v1/images/variations", APIKey: "key"},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected legacy endpoint_type config to validate: %v", err)
	}

	assertTargetCompat(t, cfg.Targets[0], EndpointTypeDualProtocol, "", "/compatible-mode", "/apps/anthropic", true)
	assertTargetCompat(t, cfg.Targets[1], EndpointTypeDualProtocol, "", "", "/anthropic", false)
	assertTargetCompat(t, cfg.Targets[2], EndpointTypeDualProtocol, "", "", "/custom-anthropic", false)
	assertTargetCompat(t, cfg.Targets[3], EndpointTypeGemini, "", "", "", false)
	assertTargetCompat(t, cfg.Targets[4], EndpointTypeOpenAIImage, ImageOperationEdits, "", "", false)
	assertTargetCompat(t, cfg.Targets[5], EndpointTypeOpenAIImage, ImageOperationVariations, "", "", false)
}

func validConfigForEndpointCompatTest() *Config {
	return &Config{
		Server:       ServerConfig{Bind: "127.0.0.1:8080", RequestTimeoutSeconds: 30},
		DataStore:    DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{CookieName: "admin_sid", Secret: "test-secret", TTLSeconds: 3600},
		Logging:      LoggingConfig{Level: "info", AccessLog: "logs/access.log", ErrorLog: "logs/error.log"},
	}
}

func assertTargetCompat(t *testing.T, target Target, epType, imageOp, openAIPrefix, anthropicPrefix string, supportsResponses bool) {
	t.Helper()
	if target.EndpointType != epType {
		t.Fatalf("%s endpoint_type=%q want %q", target.Name, target.EndpointType, epType)
	}
	if target.ImageOperation != imageOp {
		t.Fatalf("%s image_operation=%q want %q", target.Name, target.ImageOperation, imageOp)
	}
	if target.OpenAIPrefix != openAIPrefix || target.AnthropicPrefix != anthropicPrefix || target.SupportsResponses != supportsResponses {
		t.Fatalf("%s prefixes/supports got openai=%q anthropic=%q responses=%v", target.Name, target.OpenAIPrefix, target.AnthropicPrefix, target.SupportsResponses)
	}
}

func TestIsValidEndpointType(t *testing.T) {
	for _, valid := range ValidEndpointTypes {
		if !IsValidEndpointType(valid) {
			t.Errorf("expected %q to be valid", valid)
		}
	}
	if IsValidEndpointType("unknown") {
		t.Error("expected 'unknown' to be invalid")
	}
	if IsValidEndpointType("") {
		t.Error("expected empty string to be invalid")
	}
}

func TestConfigValidateEndpointTypes(t *testing.T) {
	base := func() *Config {
		return &Config{
			Server: ServerConfig{
				Bind:                  "127.0.0.1:8080",
				RequestTimeoutSeconds: 30,
			},
			DataStore: DataStore{DBPath: "test.db"},
			AdminSession: AdminSessionConfig{
				CookieName: "admin_sid",
				Secret:     "test-secret",
				TTLSeconds: 3600,
			},
			Logging: LoggingConfig{
				Level:     "info",
				AccessLog: "logs/access.log",
				ErrorLog:  "logs/error.log",
			},
		}
	}

	// openai target: resource_path_prefix not required
	t.Run("openai without resource_path_prefix", func(t *testing.T) {
		cfg := base()
		cfg.Targets = []Target{{
			Name:         "openai-target",
			EndpointType: "openai",
			Endpoint:     "https://api.openai.com",
			APIKey:       "sk-test",
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected no validation error, got %v", err)
		}
	})

	// claude target: resource_path_prefix not required
	t.Run("claude without resource_path_prefix", func(t *testing.T) {
		cfg := base()
		cfg.Targets = []Target{{
			Name:         "claude-target",
			EndpointType: "claude",
			Endpoint:     "https://api.anthropic.com",
			APIKey:       "sk-ant-test",
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected no validation error, got %v", err)
		}
	})

	// gemini target: resource_path_prefix not required
	t.Run("gemini without resource_path_prefix", func(t *testing.T) {
		cfg := base()
		cfg.Targets = []Target{{
			Name:         "gemini-target",
			EndpointType: "gemini",
			Endpoint:     "https://generativelanguage.googleapis.com",
			APIKey:       "AIza-test",
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected no validation error, got %v", err)
		}
	})

	// azure_openai target: resource_path_prefix no longer required (deprecated)
	t.Run("azure_openai without resource_path_prefix", func(t *testing.T) {
		cfg := base()
		cfg.Targets = []Target{{
			Name:     "azure-target",
			Endpoint: "https://example.com",
			APIKey:   "key",
		}}
		err := cfg.Validate()
		if err != nil {
			t.Fatalf("expected no validation error, got %v", err)
		}
	})
}

func TestConfigValidateInvalidEndpointType(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
		},
		Targets: []Target{{
			Name:               "bad-type",
			EndpointType:       "gcp_vertex",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
		}},
		DataStore: DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid endpoint_type")
	}
	if !contains(err.Error(), "endpoint_type") || !contains(err.Error(), "gcp_vertex") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigValidateUsageAdapter(t *testing.T) {
	for _, adapter := range []string{"", UsageAdapterAuto, UsageAdapterStandard, UsageAdapterQwen, UsageAdapterGLM, UsageAdapterDeepSeek, UsageAdapterKimi, " QWEN ", " KIMI "} {
		cfg := validConfigForEndpointCompatTest()
		cfg.Targets = []Target{{Name: "adapter", EndpointType: EndpointTypeDualProtocol, Endpoint: "https://example.com", APIKey: "key", UsageAdapter: adapter}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("adapter %q rejected: %v", adapter, err)
		}
		if got := cfg.Targets[0].UsageAdapter; got != NormalizeUsageAdapter(adapter) {
			t.Fatalf("adapter %q normalized to %q", adapter, got)
		}
	}

	cfg := validConfigForEndpointCompatTest()
	cfg.Targets = []Target{{Name: "adapter", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: "model-name-only"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "usage_adapter") {
		t.Fatalf("invalid adapter error = %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRequestTimeoutDefaultIs1800(t *testing.T) {
	cfg := DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig returned nil")
	}
	if got := cfg.Server.RequestTimeoutSeconds; got != 1800 {
		t.Fatalf("expected default RequestTimeoutSeconds=1800, got %d", got)
	}
}

func validPoolTestConfig() *Config {
	return &Config{
		Server:    ServerConfig{Bind: "127.0.0.1:8080", RequestTimeoutSeconds: 30},
		DataStore: DataStore{DBPath: "/tmp/test.db"},
		AdminSession: AdminSessionConfig{
			CookieName: "sess", Secret: "s", TTLSeconds: 60,
		},
		Logging: LoggingConfig{Level: "info", AccessLog: "a.log", ErrorLog: "e.log"},
	}
}

func TestValidatePoolNames(t *testing.T) {
	cfg := validPoolTestConfig()
	cfg.Pools = []DeploymentPool{
		{Name: "alpha"},
		{Name: "beta"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid pool names must pass: %v", err)
	}

	cfg = validPoolTestConfig()
	cfg.Pools = []DeploymentPool{{Name: "  "}}
	if err := cfg.Validate(); err == nil || !searchSubstring(err.Error(), "pools[0].name must not be empty") {
		t.Fatalf("expected empty pool name rejection, got: %v", err)
	}

	cfg = validPoolTestConfig()
	cfg.Pools = []DeploymentPool{{Name: "alpha"}, {Name: "ALPHA"}}
	if err := cfg.Validate(); err == nil || !searchSubstring(err.Error(), "duplicate") {
		t.Fatalf("expected case-insensitive duplicate pool rejection, got: %v", err)
	}
}

func TestClonePools(t *testing.T) {
	cfg := validPoolTestConfig()
	cfg.Pools = []DeploymentPool{
		{Name: "alpha", Description: "first", Paused: true},
		{Name: "beta"},
	}

	clone := cfg.Clone()
	if len(clone.Pools) != 2 || clone.Pools[0].Name != "alpha" || clone.Pools[0].Paused != true || clone.Pools[1].Name != "beta" {
		t.Fatalf("unexpected cloned pools: %+v", clone.Pools)
	}

	clone.Pools[0].Name = "mutated"
	clone.Pools[0].Paused = false
	if cfg.Pools[0].Name != "alpha" || !cfg.Pools[0].Paused {
		t.Fatalf("Clone must deep-copy pools: %+v", cfg.Pools)
	}
}

func TestModelMappingPausedJSONRoundTrip(t *testing.T) {
	raw := `{"name":"t1","endpoint":"https://example.com","api_key":"k","model_mappings":[{"upstream":"gpt-4o"},{"upstream":"gpt-4o-mini","fallback":"mini","paused":true}]}`
	var target Target
	if err := json.Unmarshal([]byte(raw), &target); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(target.ModelMappings) != 2 || target.ModelMappings[0].Paused || !target.ModelMappings[1].Paused {
		t.Fatalf("paused flag lost on unmarshal: %+v", target.ModelMappings)
	}
	data, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"paused":true`) {
		t.Fatalf("paused flag lost on marshal: %s", data)
	}
	if strings.Contains(string(data), `"paused":false`) {
		t.Fatalf("paused=false must be omitted: %s", data)
	}
	var round Target
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !round.ModelMappings[1].Paused || round.ModelMappings[1].Fallback != "mini" {
		t.Fatalf("round-trip lost fields: %+v", round.ModelMappings)
	}
}

func TestConfigValidateToleratesStoredAuthModeAndImageOperation(t *testing.T) {
	// Runtime keeps the provider default auth and skips image routing for
	// unknown values; Validate must not block startup on them either.
	cfg := &Config{
		Server: ServerConfig{Bind: "127.0.0.1:8080", RequestTimeoutSeconds: 30},
		Targets: []Target{{
			Name: "legacy", Endpoint: "https://example.com", EndpointType: EndpointTypeClaude, APIKey: "key",
			AuthMode: "x-api-key", ImageOperation: "edit",
		}},
		DataStore:    DataStore{DBPath: "test.db"},
		AdminSession: AdminSessionConfig{CookieName: "admin_sid", Secret: "test-secret", TTLSeconds: 3600},
		Logging:      LoggingConfig{Level: "info", AccessLog: "logs/access.log", ErrorLog: "logs/error.log"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("stored legacy enum values blocked config load: %v", err)
	}
}
