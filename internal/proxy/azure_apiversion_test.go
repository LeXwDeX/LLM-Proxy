// azure_apiversion_test.go — Azure OpenAI api-version 注入的回归测试。
// v1 路径（/openai/v1/...）不注入 api-version，deployment-based 路径注入 2026-03-01-preview。
package proxy

import (
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestBuildURLAzureV1PathNoAPIVersion(t *testing.T) {
	endpoint, _ := url.Parse("https://example-east2-gpt.openai.azure.com/openai")
	target := &Target{
		Name:         "azure",
		EndpointType: config.EndpointTypeAzureOpenAI,
		Endpoint:     endpoint,
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}

	// v1 路径不应注入 api-version
	cases := []struct {
		clientPath string
		wantPath   string
		wantQuery  string
	}{
		{
			"/v1/images/edits",
			"/openai/v1/images/edits",
			"", // v1 路径：无 api-version
		},
		{
			"/v1/images/generations",
			"/openai/v1/images/generations",
			"", // v1 路径：无 api-version
		},
		{
			"/v1/chat/completions",
			"/openai/v1/chat/completions",
			"", // v1 路径：无 api-version
		},
		{
			"/v1/responses",
			"/openai/v1/responses",
			"", // v1 路径：无 api-version
		},
	}
	for _, tc := range cases {
		original := &url.URL{Path: tc.clientPath}
		got, err := s.buildURL(target, original)
		if err != nil {
			t.Fatalf("buildURL(%q) error: %v", tc.clientPath, err)
		}
		if got.Path != tc.wantPath {
			t.Errorf("buildURL(%q).Path = %q, want %q", tc.clientPath, got.Path, tc.wantPath)
		}
		if got.RawQuery != tc.wantQuery {
			t.Errorf("buildURL(%q).RawQuery = %q, want %q", tc.clientPath, got.RawQuery, tc.wantQuery)
		}
	}
}

func TestBuildURLAzureDeploymentPathInjectsAPIVersion(t *testing.T) {
	endpoint, _ := url.Parse("https://example-east2-gpt.openai.azure.com/openai")
	target := &Target{
		Name:         "azure",
		EndpointType: config.EndpointTypeAzureOpenAI,
		Endpoint:     endpoint,
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}

	// deployment-based 路径应注入 api-version=2026-03-01-preview
	cases := []struct {
		clientPath string
		wantPath   string
		wantQuery  string
	}{
		{
			"/deployments/gpt-4o/chat/completions",
			"/openai/deployments/gpt-4o/chat/completions",
			"api-version=2026-03-01-preview",
		},
		{
			"/deployments/gpt-image-2/images/edits",
			"/openai/deployments/gpt-image-2/images/edits",
			"api-version=2026-03-01-preview",
		},
		{
			"/deployments/text-embedding-3-large/embeddings",
			"/openai/deployments/text-embedding-3-large/embeddings",
			"api-version=2026-03-01-preview",
		},
	}
	for _, tc := range cases {
		original := &url.URL{Path: tc.clientPath}
		got, err := s.buildURL(target, original)
		if err != nil {
			t.Fatalf("buildURL(%q) error: %v", tc.clientPath, err)
		}
		if got.Path != tc.wantPath {
			t.Errorf("buildURL(%q).Path = %q, want %q", tc.clientPath, got.Path, tc.wantPath)
		}
		if got.RawQuery != tc.wantQuery {
			t.Errorf("buildURL(%q).RawQuery = %q, want %q", tc.clientPath, got.RawQuery, tc.wantQuery)
		}
	}
}

func TestBuildURLAzureStripsClientAPIVersion(t *testing.T) {
	endpoint, _ := url.Parse("https://example-east2-gpt.openai.azure.com/openai")
	target := &Target{
		Name:         "azure",
		EndpointType: config.EndpointTypeAzureOpenAI,
		Endpoint:     endpoint,
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}

	// v1 路径：客户端传的 api-version 被剥离，且不注入新值
	original := &url.URL{
		Path:     "/v1/images/edits",
		RawQuery: "api-version=2024-02-01&other=keep",
	}
	got, err := s.buildURL(target, original)
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}

	q := got.Query()
	if q.Get("api-version") != "" {
		t.Errorf("expected no api-version for v1 path, got %q", q.Get("api-version"))
	}
	if q.Get("other") != "keep" {
		t.Errorf("expected other=keep preserved, got %q", q.Get("other"))
	}

	// deployment 路径：客户端传的 api-version 被剥离，注入正确版本
	original2 := &url.URL{
		Path:     "/deployments/gpt-4o/chat/completions",
		RawQuery: "api-version=2024-02-01&other=keep",
	}
	got2, err := s.buildURL(target, original2)
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}

	q2 := got2.Query()
	if q2.Get("api-version") != "2026-03-01-preview" {
		t.Errorf("expected api-version=2026-03-01-preview for deployment path, got %q", q2.Get("api-version"))
	}
	if q2.Get("other") != "keep" {
		t.Errorf("expected other=keep preserved, got %q", q2.Get("other"))
	}
}

func TestBuildURLNonAzureDoesNotInjectAPIVersion(t *testing.T) {
	endpoint, _ := url.Parse("https://api.deepseek.com")
	target := &Target{
		Name:            "dual-proto",
		EndpointType:    config.EndpointTypeDualProtocol,
		Endpoint:        endpoint,
		AnthropicPrefix: "/anthropic",
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}

	original := &url.URL{Path: "/v1/chat/completions"}
	got, err := s.buildURL(target, original)
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}

	if got.RawQuery != "" {
		t.Errorf("expected no query for non-Azure target, got %q", got.RawQuery)
	}
}

func TestAppendAzureAPIVersion(t *testing.T) {
	cases := []struct {
		rawQuery string
		version  string
		want     string
	}{
		{"", "2026-03-01-preview", "api-version=2026-03-01-preview"},
		{"other=yes", "2026-03-01-preview", "other=yes&api-version=2026-03-01-preview"},
		{"foo=bar&baz=1", "2026-03-01-preview", "foo=bar&baz=1&api-version=2026-03-01-preview"},
	}
	for _, tc := range cases {
		got := appendAzureAPIVersion(tc.rawQuery, tc.version)
		if got != tc.want {
			t.Errorf("appendAzureAPIVersion(%q, %q) = %q, want %q", tc.rawQuery, tc.version, got, tc.want)
		}
	}
}

func TestIsDeploymentBasedPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/openai/v1/images/edits", false},
		{"/openai/v1/chat/completions", false},
		{"/openai/v1/responses", false},
		{"/openai/deployments/gpt-4o/chat/completions", true},
		{"/openai/deployments/gpt-image-2/images/edits", true},
		{"/openai/Deployments/model/completions", true}, // case insensitive
	}
	for _, tc := range cases {
		got := isDeploymentBasedPath(tc.path)
		if got != tc.want {
			t.Errorf("isDeploymentBasedPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
