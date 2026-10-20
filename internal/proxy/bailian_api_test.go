package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestBuildURLDualProtocolBailianRouting(t *testing.T) {
	endpoint, _ := url.Parse("https://dashscope.aliyuncs.com")
	target := &Target{
		Name:              "bailian-api",
		EndpointType:      config.EndpointTypeDualProtocol,
		Endpoint:          endpoint,
		OpenAIPrefix:      "/compatible-mode",
		AnthropicPrefix:   "/apps/anthropic",
		SupportsResponses: true,
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}

	cases := []struct {
		clientPath string
		want       string
	}{
		{"/v1/chat/completions", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"/v1/embeddings", "https://dashscope.aliyuncs.com/compatible-mode/v1/embeddings"},
		{"/v1/responses", "https://dashscope.aliyuncs.com/compatible-mode/v1/responses"},
		{"/v1/responses/resp_123", "https://dashscope.aliyuncs.com/compatible-mode/v1/responses/resp_123"},
		{"/v1/messages", "https://dashscope.aliyuncs.com/apps/anthropic/v1/messages"},
		{"/v1/messages/count_tokens", "https://dashscope.aliyuncs.com/apps/anthropic/v1/messages/count_tokens"},
	}
	for _, tc := range cases {
		original := &url.URL{Path: tc.clientPath}
		got, err := s.buildURL(target, original)
		if err != nil {
			t.Fatalf("buildURL(%q) error: %v", tc.clientPath, err)
		}
		if got.String() != tc.want {
			t.Errorf("buildURL(%q) = %q, want %q", tc.clientPath, got.String(), tc.want)
		}
	}
}

func TestServiceDualProtocolRoutesOpenAIWithoutAnthropicBodyMutation(t *testing.T) {
	var seenPath, seenAuth, seenVersion string
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenVersion = r.Header.Get("anthropic-version")
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	service := newDualProtocolService(t, upstream.URL)
	principal := newDualProtocolTestPrincipal(t)
	body := bytes.NewBufferString(`{"model":"qwen-plus","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"},{"role":"user","content":"three"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenPath != "/compatible-mode/v1/chat/completions" {
		t.Fatalf("expected OpenAI-compatible path, got %q", seenPath)
	}
	if seenAuth != "Bearer "+"sk-dashscope" {
		t.Fatalf("expected bearer auth, got %q", seenAuth)
	}
	if seenVersion != "" {
		t.Fatalf("OpenAI-compatible request should not set anthropic-version, got %q", seenVersion)
	}
	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("unexpected messages payload: %#v", captured["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if _, ok := first["content"].(string); !ok {
		t.Fatalf("OpenAI-compatible content should remain a string, got %#v", first["content"])
	}
}

func TestServiceDualProtocolRoutesAnthropicWithVersion(t *testing.T) {
	var seenPath, seenAuth, seenVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	service := newDualProtocolService(t, upstream.URL)
	principal := newDualProtocolTestPrincipal(t)
	body := bytes.NewBufferString(`{"model":"qwen-plus","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenPath != "/apps/anthropic/v1/messages" {
		t.Fatalf("expected Anthropic-compatible path, got %q", seenPath)
	}
	if seenAuth != "Bearer "+"sk-dashscope" {
		t.Fatalf("expected bearer auth, got %q", seenAuth)
	}
	if seenVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version, got %q", seenVersion)
	}
}

func newDualProtocolService(t *testing.T, endpoint string) *Service {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:              "dual-proto",
			EndpointType:      config.EndpointTypeDualProtocol,
			Endpoint:          endpoint,
			APIKey:            "sk-dashscope",
			ModelMappings: []config.ModelMapping{{Upstream: "qwen-plus"}},
			OpenAIPrefix:      "/compatible-mode",
			AnthropicPrefix:   "/apps/anthropic",
			SupportsResponses: true,
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func newDualProtocolTestPrincipal(t *testing.T) *auth.Principal {
	t.Helper()
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	return principal
}
