package proxy

import (
	"net/http"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ---------------------------------------------------------------------------
// TestProviderProfileSupportsProtocol — 每个 provider 的协议支持矩阵
// ---------------------------------------------------------------------------

func TestProviderProfileSupportsProtocol(t *testing.T) {
	reg := DefaultProviderRegistry()

	// protocol matrix: endpoint_type → expected supported protocols
	matrix := map[string]map[config.ProtocolType]bool{
		config.EndpointTypeOpenAI: {
			config.ProtocolOpenAIChat:        true,
			config.ProtocolOpenAIResponses:   true,
			config.ProtocolOpenAIImage:       true,
			config.ProtocolAnthropicMessages: false,
			config.ProtocolGemini:            false,
		},
		config.EndpointTypeAzureOpenAI: {
			config.ProtocolOpenAIChat:        true,
			config.ProtocolOpenAIResponses:   true,
			config.ProtocolOpenAIImage:       true,
			config.ProtocolAnthropicMessages: false,
			config.ProtocolGemini:            false,
		},
		config.EndpointTypeClaude: {
			config.ProtocolAnthropicMessages: true,
			config.ProtocolOpenAIChat:        false,
			config.ProtocolOpenAIResponses:   false,
			config.ProtocolOpenAIImage:       false,
			config.ProtocolGemini:            false,
		},
		config.EndpointTypeGemini: {
			config.ProtocolGemini:            true,
			config.ProtocolOpenAIChat:        false,
			config.ProtocolOpenAIResponses:   false,
			config.ProtocolAnthropicMessages: false,
			config.ProtocolOpenAIImage:       false,
		},
		config.EndpointTypeDualProtocol: {
			config.ProtocolOpenAIChat:        true,
			config.ProtocolAnthropicMessages: true,
			config.ProtocolOpenAIResponses:   false,
			config.ProtocolOpenAIImage:       false,
			config.ProtocolGemini:            false,
		},
		config.EndpointTypeOpenAIImage: {
			config.ProtocolOpenAIImage:       true,
			config.ProtocolOpenAIChat:        false,
			config.ProtocolOpenAIResponses:   false,
			config.ProtocolAnthropicMessages: false,
			config.ProtocolGemini:            false,
		},
	}

	for epType, protoMap := range matrix {
		profile := reg.Lookup(epType)
		if profile == nil {
			t.Fatalf("provider %q not registered", epType)
		}
		for proto, want := range protoMap {
			got := profile.SupportsProtocol(proto)
			if got != want {
				t.Errorf("%s.SupportsProtocol(%q) = %v, want %v", epType, proto, got, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// TestProviderProfileSupportsPath — openai_image 终态 URL
// ---------------------------------------------------------------------------

func TestProviderProfileSupportsPath(t *testing.T) {
	reg := DefaultProviderRegistry()

	tests := []struct {
		endpointType string
		path         string
		expected     bool
	}{
		// openai_image: terminal URL, supports image paths
		{config.EndpointTypeOpenAIImage, "/v1/images/generations", true},
		{config.EndpointTypeOpenAIImage, "/v1/images/edits", true},
		{config.EndpointTypeOpenAIImage, "/v1/images/variations", true},
		{config.EndpointTypeOpenAIImage, "/v1/chat/completions", false},

		// openai: no path restrictions (nil SupportedPaths)
		{config.EndpointTypeOpenAI, "/v1/chat/completions", true},
		{config.EndpointTypeOpenAI, "/v1/responses", true},
		{config.EndpointTypeOpenAI, "/v1/images/generations", true},
		{config.EndpointTypeOpenAI, "/v1/messages", false}, // Anthropic protocol not supported

		// claude: only Anthropic paths
		{config.EndpointTypeClaude, "/v1/messages", true},
		{config.EndpointTypeClaude, "/v1/messages/count_tokens", true},
		{config.EndpointTypeClaude, "/v1/chat/completions", false},
	}

	for _, tc := range tests {
		profile := reg.Lookup(tc.endpointType)
		if profile == nil {
			t.Fatalf("provider %q not registered", tc.endpointType)
		}
		name := tc.endpointType + "_" + tc.path
		t.Run(name, func(t *testing.T) {
			got := profile.SupportsPath(tc.path)
			if got != tc.expected {
				t.Errorf("%s.SupportsPath(%q) = %v, want %v", tc.endpointType, tc.path, got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestDefaultProviderRegistry — 9 个 endpoint_type 全部注册、Lookup 正确
// ---------------------------------------------------------------------------

func TestDefaultProviderRegistry(t *testing.T) {
	reg := DefaultProviderRegistry()

	expectedTypes := []string{
		config.EndpointTypeOpenAI,
		config.EndpointTypeAzureOpenAI,
		config.EndpointTypeClaude,
		config.EndpointTypeGemini,
		config.EndpointTypeDualProtocol,
		config.EndpointTypeOpenAIImage,
	}

	if len(expectedTypes) != 6 {
		t.Fatalf("expected 6 endpoint types in test, got %d", len(expectedTypes))
	}

	for _, epType := range expectedTypes {
		profile := reg.Lookup(epType)
		if profile == nil {
			t.Errorf("provider %q not registered in DefaultProviderRegistry", epType)
			continue
		}
		if profile.EndpointType != epType {
			t.Errorf("Lookup(%q).EndpointType = %q", epType, profile.EndpointType)
		}
		if profile.Provider == "" {
			t.Errorf("provider %q has empty Provider name", epType)
		}
		if len(profile.Protocols) == 0 {
			t.Errorf("provider %q has no protocols", epType)
		}
		if profile.Auth == nil {
			t.Errorf("provider %q has nil Auth strategy", epType)
		}
		if profile.Path == nil {
			t.Errorf("provider %q has nil Path mapper", epType)
		}
	}

	// Unknown type returns nil
	if reg.Lookup("nonexistent") != nil {
		t.Error("Lookup(nonexistent) should return nil")
	}
}

// ---------------------------------------------------------------------------
// TestAuthStrategies — 每个 AuthStrategy 的 InjectAuth 行为
// ---------------------------------------------------------------------------

func TestAuthStrategies(t *testing.T) {
	t.Run("BearerAuth", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1/chat/completions", nil)
		auth := &BearerAuth{}
		if err := auth.InjectAuth(req, "sk-test-key", "/v1/chat/completions"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer sk-test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test-key")
		}
	})

	t.Run("AnthropicAuth", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1/messages", nil)
		auth := &AnthropicAuth{}
		if err := auth.InjectAuth(req, "ant-test-key", "/v1/messages"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("x-api-key"); got != "ant-test-key" {
			t.Errorf("x-api-key = %q, want %q", got, "ant-test-key")
		}
		if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization should be empty, got %q", got)
		}
	})

	t.Run("AnthropicAuth_BearerMode", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1/messages", nil)
		auth := &AnthropicAuth{BearerMode: true}
		if err := auth.InjectAuth(req, "bearer-key", "/v1/messages"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer bearer-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer bearer-key")
		}
		if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
		}
		if got := req.Header.Get("x-api-key"); got != "" {
			t.Errorf("x-api-key should be empty in bearer mode, got %q", got)
		}
	})

	t.Run("AnthropicAuth_PreservesExistingVersion", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1/messages", nil)
		req.Header.Set("anthropic-version", "2024-01-01")
		auth := &AnthropicAuth{}
		if err := auth.InjectAuth(req, "key", "/v1/messages"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("anthropic-version"); got != "2024-01-01" {
			t.Errorf("anthropic-version should be preserved, got %q", got)
		}
	})

	t.Run("GeminiAuth", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1beta/models/gemini:generateContent", nil)
		auth := &GeminiAuth{}
		if err := auth.InjectAuth(req, "gemini-key", "/v1beta/models/gemini:generateContent"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("x-goog-api-key"); got != "gemini-key" {
			t.Errorf("x-goog-api-key = %q, want %q", got, "gemini-key")
		}
	})

	t.Run("BearerWithConditionalAnthropicVersion_AnthropicPath", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1/messages", nil)
		auth := &BearerWithConditionalAnthropicVersion{}
		if err := auth.InjectAuth(req, "ds-key", "/v1/messages"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ds-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer ds-key")
		}
		if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
		}
	})

	t.Run("BearerWithConditionalAnthropicVersion_OpenAIPath", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/v1/chat/completions", nil)
		auth := &BearerWithConditionalAnthropicVersion{}
		if err := auth.InjectAuth(req, "ds-key", "/v1/chat/completions"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ds-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer ds-key")
		}
		if got := req.Header.Get("anthropic-version"); got != "" {
			t.Errorf("anthropic-version should be empty for OpenAI path, got %q", got)
		}
	})

	t.Run("AzureAuth_ApiKey", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/openai/v1/chat/completions", nil)
		auth := &AzureAuth{}
		if err := auth.InjectAuth(req, "azure-key", "/v1/chat/completions"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("api-key"); got != "azure-key" {
			t.Errorf("api-key = %q, want %q", got, "azure-key")
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization should be empty, got %q", got)
		}
	})

	t.Run("AzureAuth_BearerPassthrough", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://upstream/openai/v1/chat/completions", nil)
		auth := &AzureAuth{AllowBearer: true, AzureAuthValue: "Bearer eyJ0eXAi..."}
		if err := auth.InjectAuth(req, "azure-key", "/v1/chat/completions"); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer eyJ0eXAi..." {
			t.Errorf("Authorization = %q, want %q", got, "Bearer eyJ0eXAi...")
		}
		if got := req.Header.Get("api-key"); got != "" {
			t.Errorf("api-key should be empty in bearer mode, got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// TestPathMappers — 每个 PathMapper 的 RewritePath 行为
// ---------------------------------------------------------------------------

func TestPathMappers(t *testing.T) {
	t.Run("PassthroughPath", func(t *testing.T) {
		m := &PassthroughPath{}
		if m.IsTerminalURL() {
			t.Error("PassthroughPath should not be terminal")
		}
		got := m.RewritePath("/v1/chat/completions", "")
		if got != "/v1/chat/completions" {
			t.Errorf("RewritePath = %q, want %q", got, "/v1/chat/completions")
		}
		got = m.RewritePath("/v1/chat/completions", "/openai")
		if got != "/openai/v1/chat/completions" {
			t.Errorf("RewritePath with prefix = %q, want %q", got, "/openai/v1/chat/completions")
		}
	})

	t.Run("DualProtocolPath_Anthropic", func(t *testing.T) {
		m := &DualProtocolPath{AnthropicPrefix: "/anthropic"}
		if m.IsTerminalURL() {
			t.Error("DualProtocolPath should not be terminal")
		}
		got := m.RewritePath("/v1/messages", "")
		if got != "/anthropic/v1/messages" {
			t.Errorf("RewritePath = %q, want %q", got, "/anthropic/v1/messages")
		}
	})

	t.Run("DualProtocolPath_OpenAI", func(t *testing.T) {
		m := &DualProtocolPath{OpenAIPrefix: ""}
		got := m.RewritePath("/v1/chat/completions", "")
		if got != "/v1/chat/completions" {
			t.Errorf("RewritePath = %q, want %q", got, "/v1/chat/completions")
		}
	})

	t.Run("DualProtocolPath_BailianAnthropic", func(t *testing.T) {
		m := &DualProtocolPath{AnthropicPrefix: "/apps/anthropic"}
		got := m.RewritePath("/v1/messages", "")
		if got != "/apps/anthropic/v1/messages" {
			t.Errorf("RewritePath = %q, want %q", got, "/apps/anthropic/v1/messages")
		}
	})

	t.Run("DualProtocolPath_BailianOpenAI", func(t *testing.T) {
		m := &DualProtocolPath{OpenAIPrefix: "/compatible-mode"}
		got := m.RewritePath("/v1/chat/completions", "")
		if got != "/compatible-mode/v1/chat/completions" {
			t.Errorf("RewritePath = %q, want %q", got, "/compatible-mode/v1/chat/completions")
		}
	})

	t.Run("DualProtocolPath_Responses", func(t *testing.T) {
		m := &DualProtocolPath{OpenAIPrefix: "/compatible-mode", SupportsResponses: true}
		got := m.RewritePath("/v1/responses", "")
		if got != "/compatible-mode/v1/responses" {
			t.Errorf("RewritePath = %q, want %q", got, "/compatible-mode/v1/responses")
		}
	})

	t.Run("TerminalURLPath", func(t *testing.T) {
		m := &TerminalURLPath{}
		if !m.IsTerminalURL() {
			t.Error("TerminalURLPath should be terminal")
		}
		got := m.RewritePath("/v1/images/generations", "")
		if got != "" {
			t.Errorf("RewritePath = %q, want empty", got)
		}
	})
}

// ---------------------------------------------------------------------------
// TestPathMapperPrepareEndpointPath — 每个 PathMapper 的 PrepareEndpointPath 行为
// ---------------------------------------------------------------------------

func TestPathMapperPrepareEndpointPath(t *testing.T) {
	cases := []struct {
		name   string
		mapper PathMapper
		input  string
		want   string
	}{
		{"passthrough", &PassthroughPath{}, "/compatible-mode/v1", "/compatible-mode/v1"},
		{"dual_protocol", &DualProtocolPath{}, "/v1", "/v1"},
		{"dual_protocol with prefix", &DualProtocolPath{OpenAIPrefix: "/compatible-mode"}, "/compatible-mode", "/compatible-mode"},
		{"terminal", &TerminalURLPath{}, "/openai-image", "/openai-image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.mapper.PrepareEndpointPath(tc.input)
			if got != tc.want {
				t.Errorf("PrepareEndpointPath(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestProviderProfilePathCapabilitiesMatchRouting(t *testing.T) {
	registry := DefaultProviderRegistry()
	paths := []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1beta/models/test:generateContent", "/v1/images/generations", "/v1/files", "/unknown"}
	for _, endpoint := range config.AllEndpointTypeMetas() {
		for _, path := range paths {
			if got, want := registry.Lookup(endpoint.Code).SupportsPath(path), PathSupportedByEndpointType(endpoint.Code, path); got != want {
				t.Errorf("%s %s: profile=%v routing=%v", endpoint.Code, path, got, want)
			}
		}
	}
}

func TestProviderRegistryRejectsIncompleteOrInvalidProfiles(t *testing.T) {
	for _, mutate := range []func(*ProviderProfile){
		func(p *ProviderProfile) { p.Auth = nil },
		func(p *ProviderProfile) { p.Path = nil },
		func(p *ProviderProfile) { p.Auth = (*BearerAuth)(nil) },
		func(p *ProviderProfile) { p.Path = (*PassthroughPath)(nil) },
		func(p *ProviderProfile) { p.RequestSchemas = []config.RequestSchema{"typo-schema"} },
		func(p *ProviderProfile) { p.Protocols = []config.ProtocolType{"typo-protocol"} },
	} {
		profile := *DefaultProviderRegistry().Lookup(config.EndpointTypeOpenAI)
		mutate(&profile)
		registry := NewProviderRegistry()
		if err := registry.Register(&profile); err == nil {
			t.Error("invalid provider registration omitted its error")
		}
		if registry.Lookup(profile.EndpointType) != nil {
			t.Error("invalid provider profile was published")
		}
	}
}

func TestBuildURLReturnsErrorForMissingProfilePath(t *testing.T) {
	registry := NewProviderRegistry()
	// Simulate a broken legacy integration which bypassed Register validation.
	registry.profiles[config.EndpointTypeOpenAI] = &ProviderProfile{EndpointType: config.EndpointTypeOpenAI}
	service := &Service{providerRegistry: registry}
	if _, err := service.buildURL(&Target{EndpointType: config.EndpointTypeOpenAI, Endpoint: mustParseURL("https://upstream.invalid")}, mustParseURL("https://proxy.invalid/v1/chat/completions")); err == nil {
		t.Fatal("missing path strategy should return an error")
	}
}

func TestProviderRegistryMetadataRejectsUnknownPoliciesAndCapabilities(t *testing.T) {
	for _, mutate := range []func(*config.EndpointTypeMeta){
		func(m *config.EndpointTypeMeta) { m.AuthStrategy = "typo-auth" },
		func(m *config.EndpointTypeMeta) { m.PathStrategy = "typo-path" },
		func(m *config.EndpointTypeMeta) { m.RequestSchemas = nil },
		func(m *config.EndpointTypeMeta) { m.Protocols = nil },
		func(m *config.EndpointTypeMeta) { m.RequestSchemas = []config.RequestSchema{"typo-schema"} },
		func(m *config.EndpointTypeMeta) { m.Protocols = []config.ProtocolType{"typo-protocol"} },
	} {
		meta, _ := config.EndpointTypeMetaOf(config.EndpointTypeOpenAI)
		mutate(&meta)
		if registry, err := providerRegistryFromMetadata([]config.EndpointTypeMeta{meta}); err == nil || registry != nil {
			t.Fatal("invalid built-in metadata must fail before publishing a registry")
		}
	}
}

func TestRuntimeTargetConfigNeverAppliesUnpublishedConstants(t *testing.T) {
	// Stored targets are tolerated individually (see
	// TestBuildTargetStatesToleratesDirtyStoredTargets), but an unpublished
	// constant must never take effect at runtime.
	for name, tc := range map[string]struct {
		mutate func(*config.Target)
		check  func(*testing.T, map[string]*targetState)
	}{
		"endpoint_type": {func(t *config.Target) { t.EndpointType = "unknown" }, func(t *testing.T, got map[string]*targetState) {
			if got["test"] != nil {
				t.Fatal("unknown endpoint type was published")
			}
		}},
		"usage_adapter": {func(t *config.Target) { t.UsageAdapter = "unknown" }, func(t *testing.T, got map[string]*targetState) {
			if got["test"] != nil {
				t.Fatal("unknown usage adapter was published")
			}
		}},
		"auth_mode": {func(t *config.Target) { t.AuthMode = "unknown" }, func(t *testing.T, got map[string]*targetState) {
			if got["test"] == nil || got["test"].Target().AuthMode != "" {
				t.Fatal("unknown auth mode took effect instead of the provider default")
			}
		}},
		"image_operation": {func(t *config.Target) { t.EndpointType = config.EndpointTypeOpenAIImage; t.ImageOperation = "unknown" }, func(t *testing.T, got map[string]*targetState) {
			for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/images/variations"} {
				if got["test"] != nil && got["test"].Target().SupportsPath(path) {
					t.Fatalf("unknown image operation routed %s", path)
				}
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			target := config.Target{Name: "test", Endpoint: "https://upstream.invalid", APIKey: "test-only", EndpointType: config.EndpointTypeOpenAI}
			tc.mutate(&target)
			parsed, _, err := buildTargetStates([]config.Target{target})
			if err != nil {
				t.Fatalf("one stored target failed the whole configuration: %v", err)
			}
			tc.check(t, parsed)
		})
	}
}
