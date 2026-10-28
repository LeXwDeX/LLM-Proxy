package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestIssue75InvalidEndpointErrorIsControlled(t *testing.T) {
	for _, endpoint := range []string{"https://fixture-user:fixture-password@example.invalid/%zz?token=fixture-query", "https://fixture-user:fixture-password@example.invalid:bad?token=fixture-query"} {
		_, _, err := buildTargetStates([]config.Target{{Name: "fixture-target", EndpointType: "openai", Endpoint: endpoint, APIKey: "fixture-key"}})
		if err == nil {
			t.Fatal("invalid endpoint accepted")
		}
		for _, secret := range []string{"fixture-user", "fixture-password", "fixture-query"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks %q: %v", secret, err)
			}
		}
		if !strings.Contains(err.Error(), "fixture-target") || !strings.Contains(err.Error(), "invalid endpoint") {
			t.Errorf("missing diagnostic: %v", err)
		}
	}
}

func TestIssue75DualProtocolPrefixForwardingUnchanged(t *testing.T) {
	s, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "fixture-target", EndpointType: "dual_protocol", Endpoint: "http://upstream.invalid", APIKey: "fixture-key", UsageAdapter: "standard", OpenAIPrefix: "/fixture-openai-secret", AnthropicPrefix: "/fixture-anthropic-secret"}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var path string
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":1}}`)), Request: r}, nil
	})}
	for _, tc := range []struct{ in, want string }{{"/v1/chat/completions", "/fixture-openai-secret/v1/chat/completions"}, {"/v1/messages", "/fixture-anthropic-secret/v1/messages"}} {
		req := httptest.NewRequest(http.MethodPost, tc.in, strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerProxyTarget, "fixture-target")
		req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture-client")))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || path != tc.want {
			t.Errorf("%s: status %d, path %q", tc.in, rec.Code, path)
		}
	}
}
