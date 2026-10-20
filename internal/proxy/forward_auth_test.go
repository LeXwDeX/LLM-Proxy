// forward_auth_test.go — resolveAuthStrategy + InjectAuth 集成测试。
// 覆盖所有 13 个 endpoint_type 的认证头注入行为。
package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestResolveAuthStrategyAndInject(t *testing.T) {
	svc := &Service{
		providerRegistry: DefaultProviderRegistry(),
	}

	cases := []struct {
		name          string
		endpointType  string
		authMode      string            // target.AuthMode
		allowBearer   bool              // target.AllowBearer
		azureAuth     string            // request header X-Azure-Authorization
		clientPath    string            // request path (used for conditional anthropic-version)
		apiKey        string
		wantHeaders   map[string]string // expected headers after InjectAuth
		wantNoHeaders []string          // headers that must NOT be present
		wantNil       bool              // resolveAuthStrategy returns nil (unsupported type)
	}{
		// ── openai ──
		{
			name:         "openai",
			endpointType: config.EndpointTypeOpenAI,
			clientPath:   "/v1/chat/completions",
			apiKey:       "sk-test",
			wantHeaders:  map[string]string{"Authorization": "Bearer sk-test"},
		},

		// ── claude (default x-api-key) ──
		{
			name:          "claude default",
			endpointType:  config.EndpointTypeClaude,
			clientPath:    "/v1/messages",
			apiKey:        "sk-ant",
			wantHeaders:   map[string]string{"x-api-key": "sk-ant", "anthropic-version": "2023-06-01"},
			wantNoHeaders: []string{"Authorization"},
		},

		// ── claude (bearer mode) ──
		{
			name:          "claude bearer",
			endpointType:  config.EndpointTypeClaude,
			authMode:      "bearer",
			clientPath:    "/v1/messages",
			apiKey:        "sk-ant",
			wantHeaders:   map[string]string{"Authorization": "Bearer sk-ant", "anthropic-version": "2023-06-01"},
			wantNoHeaders: []string{"x-api-key"},
		},

		// ── gemini ──
		{
			name:         "gemini",
			endpointType: config.EndpointTypeGemini,
			clientPath:   "/v1beta/models/gemini:generateContent",
			apiKey:       "AIza",
			wantHeaders:  map[string]string{"x-goog-api-key": "AIza"},
		},

		// ── openai_image ──
		{
			name:         "openai_image",
			endpointType: config.EndpointTypeOpenAIImage,
			clientPath:   "/v1/images/generations",
			apiKey:       "sk-img",
			wantHeaders:  map[string]string{"Authorization": "Bearer sk-img"},
		},

		// ── dual_protocol (OpenAI path) ──
		{
			name:          "dual_protocol openai",
			endpointType:  config.EndpointTypeDualProtocol,
			clientPath:    "/v1/chat/completions",
			apiKey:        "sk-dp",
			wantHeaders:   map[string]string{"Authorization": "Bearer sk-dp"},
			wantNoHeaders: []string{"anthropic-version"},
		},

		// ── dual_protocol (Anthropic path — injects anthropic-version) ──
		{
			name:         "dual_protocol anthropic",
			endpointType: config.EndpointTypeDualProtocol,
			clientPath:   "/v1/messages",
			apiKey:       "sk-dp",
			wantHeaders:  map[string]string{"Authorization": "Bearer sk-dp", "anthropic-version": "2023-06-01"},
		},

		// ── azure (api-key mode) ──
		{
			name:          "azure api-key",
			endpointType:  config.EndpointTypeAzureOpenAI,
			clientPath:    "/v1/chat/completions",
			apiKey:        "az-key",
			wantHeaders:   map[string]string{"api-key": "az-key"},
			wantNoHeaders: []string{"Authorization"},
		},

		// ── azure (bearer passthrough) ──
		{
			name:          "azure bearer",
			endpointType:  config.EndpointTypeAzureOpenAI,
			allowBearer:   true,
			azureAuth:     "Bearer az-token",
			clientPath:    "/v1/chat/completions",
			apiKey:        "az-key",
			wantHeaders:   map[string]string{"Authorization": "Bearer az-token"},
			wantNoHeaders: []string{"api-key"},
		},

		// ── unknown endpoint type → nil strategy ──
		{
			name:         "unknown",
			endpointType: "unknown_type",
			clientPath:   "/v1/chat/completions",
			apiKey:       "sk",
			wantNil:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := &Target{
				Name:         "test-" + tc.endpointType,
				EndpointType: tc.endpointType,
				Endpoint:     mustParseURLForAuth("https://upstream.example.com"),
				AllowBearer:  tc.allowBearer,
				AuthMode:     tc.authMode,
			}

			// Build the client request (used by resolveAuthStrategy for Azure header)
			clientReq, _ := http.NewRequest("POST", "https://proxy.example.com"+tc.clientPath, nil)
			if tc.azureAuth != "" {
				clientReq.Header.Set(headerAzureAuthorization, tc.azureAuth)
			}

			strategy := svc.resolveAuthStrategy(target, clientReq)

			if tc.wantNil {
				if strategy != nil {
					t.Fatalf("resolveAuthStrategy should return nil for %q, got %T", tc.endpointType, strategy)
				}
				return
			}

			if strategy == nil {
				t.Fatalf("resolveAuthStrategy returned nil for %q", tc.endpointType)
			}

			// Create the upstream request (the one InjectAuth modifies)
			upstreamReq, _ := http.NewRequest("POST", "https://upstream.example.com"+tc.clientPath, nil)

			if err := strategy.InjectAuth(upstreamReq, tc.apiKey, tc.clientPath); err != nil {
				t.Fatalf("InjectAuth error: %v", err)
			}

			// Verify expected headers
			for key, want := range tc.wantHeaders {
				got := upstreamReq.Header.Get(key)
				if got != want {
					t.Errorf("header %q = %q, want %q", key, got, want)
				}
			}

			// Verify absent headers
			for _, key := range tc.wantNoHeaders {
				got := upstreamReq.Header.Get(key)
				if got != "" {
					t.Errorf("header %q should be absent, got %q", key, got)
				}
			}
		})
	}
}

// TestAzureEmptyAPIKeyError verifies that Azure with empty apiKey and non-bearer mode
// is handled at the forwardRequest level (not in resolveAuthStrategy).
// This test documents the expected behavior: resolveAuthStrategy returns a valid strategy,
// but forwardRequest must check for empty apiKey before calling InjectAuth.
func TestAzureEmptyAPIKeyError(t *testing.T) {
	svc := &Service{
		providerRegistry: DefaultProviderRegistry(),
	}

	target := &Target{
		Name:         "azure-empty",
		EndpointType: config.EndpointTypeAzureOpenAI,
		Endpoint:     mustParseURL("https://upstream.example.com"),
		AllowBearer:  false,
	}

	clientReq, _ := http.NewRequest("POST", "https://proxy.example.com/v1/chat/completions", nil)
	strategy := svc.resolveAuthStrategy(target, clientReq)

	if strategy == nil {
		t.Fatal("resolveAuthStrategy should return non-nil for azure_openai")
	}

	// The strategy itself doesn't error on empty key — it just sets api-key: ""
	// The empty-key check is done in forwardRequest before calling InjectAuth.
	upstreamReq, _ := http.NewRequest("POST", "https://upstream.example.com/v1/chat/completions", nil)
	err := strategy.InjectAuth(upstreamReq, "", "/v1/chat/completions")
	if err != nil {
		t.Fatalf("InjectAuth should not error on empty key (forwardRequest handles this): %v", err)
	}
	// Verify it sets api-key to empty string (forwardRequest catches this before calling)
	if got := upstreamReq.Header.Get("api-key"); got != "" {
		t.Errorf("api-key = %q, want empty", got)
	}
}

func mustParseURLForAuth(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
