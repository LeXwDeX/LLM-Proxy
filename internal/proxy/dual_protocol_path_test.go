package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// TestBuildURLDualProtocolVersionedPrefix locks the general rule: the effective
// base (prefix, or endpoint.path when the prefix is empty) declares a version
// segment, so the client's leading version segment is replaced instead of
// stacked. This is what lets providers like 百度千帆 use /v2 for OpenAI and
// /anthropic/v1 for Anthropic under one dual_protocol target.
func TestBuildURLDualProtocolVersionedPrefix(t *testing.T) {
	s := &Service{providerRegistry: DefaultProviderRegistry()}

	cases := []struct {
		name            string
		endpoint        string
		openaiPrefix    string
		anthropicPrefix string
		clientPath      string
		want            string
	}{
		// OpenAI path with a versioned prefix.
		{"qianfan chat", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/chat/completions", "https://qianfan.baidubce.com/v2/chat/completions"},
		{"qianfan responses", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/responses", "https://qianfan.baidubce.com/v2/responses"},
		{"qianfan embeddings", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/embeddings", "https://qianfan.baidubce.com/v2/embeddings"},
		{"qianfan models", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/models", "https://qianfan.baidubce.com/v2/models"},
		{"qianfan images", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/images/generations", "https://qianfan.baidubce.com/v2/images/generations"},
		{"client path without version", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/chat/completions", "https://qianfan.baidubce.com/v2/chat/completions"},

		// Anthropic path: prefix itself has no version, so the client /v1 stays.
		{"qianfan anthropic", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/messages", "https://qianfan.baidubce.com/anthropic/v1/messages"},
		{"qianfan anthropic count", "https://qianfan.baidubce.com", "/v2", "/anthropic", "/v1/messages/count_tokens", "https://qianfan.baidubce.com/anthropic/v1/messages/count_tokens"},

		// Nested/multi-segment version prefixes.
		{"anthropic versioned prefix", "https://host.example", "", "/anthropic/v1", "/v1/messages", "https://host.example/anthropic/v1/messages"},
		{"multi segment version", "https://host.example", "/api/paas/v4", "/anthropic", "/v1/chat/completions", "https://host.example/api/paas/v4/chat/completions"},
		{"version not last segment", "https://host.example", "/v2/openai", "/anthropic", "/v1/chat/completions", "https://host.example/v2/openai/v1/chat/completions"},
		{"idempotent version", "https://host.example", "/v1", "/anthropic", "/v1/chat/completions", "https://host.example/v1/chat/completions"},

		// Prefix empty: fall back to endpoint.path as the effective base.
		{"empty prefixes root endpoint", "https://host.example", "", "", "/v1/chat/completions", "https://host.example/v1/chat/completions"},
		{"empty prefix versioned endpoint", "https://host.example/v2", "", "/anthropic", "/v1/chat/completions", "https://host.example/v2/chat/completions"},
		{"empty prefix versioned endpoint anthropic", "https://host.example/v2", "", "/anthropic", "/v1/messages", "https://host.example/v2/anthropic/v1/messages"},

		// Existing prefixes without a version must not change behavior.
		{"bailian compatible mode", "https://dashscope.aliyuncs.com", "/compatible-mode", "/apps/anthropic", "/v1/chat/completions", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"bailian apps anthropic", "https://dashscope.aliyuncs.com", "/compatible-mode", "/apps/anthropic", "/v1/messages", "https://dashscope.aliyuncs.com/apps/anthropic/v1/messages"},
		{"trailing slash prefix", "https://qianfan.baidubce.com", "/v2/", "/anthropic/", "/v1/chat/completions", "https://qianfan.baidubce.com/v2/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, err := url.Parse(tc.endpoint)
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}
			target := &Target{
				Name:            tc.name,
				EndpointType:    config.EndpointTypeDualProtocol,
				Endpoint:        endpoint,
				OpenAIPrefix:    tc.openaiPrefix,
				AnthropicPrefix: tc.anthropicPrefix,
			}
			original := &url.URL{Path: tc.clientPath}
			got, err := s.buildURL(target, original)
			if err != nil {
				t.Fatalf("buildURL(%q) error: %v", tc.clientPath, err)
			}
			if got.String() != tc.want {
				t.Errorf("buildURL(%q) = %q, want %q", tc.clientPath, got.String(), tc.want)
			}
		})
	}
}

// TestServiceDualProtocolVersionedPrefixKeepsAnthropicAuth 验证路由与认证使用
// 同一原始路径：即使 endpoint.path 带版本段，/v1/messages 仍走 Anthropic 前缀
// 并携带 anthropic-version，不会被版本剥除误判成 OpenAI。
func TestServiceDualProtocolVersionedPrefixKeepsAnthropicAuth(t *testing.T) {
	var seenPath, seenVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	parsed, _ := url.Parse(upstream.URL + "/v1")
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:            "dual-versioned",
			EndpointType:    config.EndpointTypeDualProtocol,
			Endpoint:        parsed.String(),
			APIKey:          "sk-test",
			ModelMappings:   []config.ModelMapping{{Upstream: "qwen-plus"}},
			OpenAIPrefix:    "/v2",
			AnthropicPrefix: "/anthropic",
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}
	body := bytes.NewBufferString(`{"model":"qwen-plus","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenPath != "/v1/anthropic/v1/messages" {
		t.Fatalf("expected versioned endpoint path + anthropic prefix, got %q", seenPath)
	}
	if seenVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version, got %q", seenVersion)
	}
}
