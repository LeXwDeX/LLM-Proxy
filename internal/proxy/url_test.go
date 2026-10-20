package proxy

import (
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestNormalizeForwardQuery(t *testing.T) {
	cases := []struct {
		name  string
		input string // raw query string (without '?')
		want  string // expected encoded query string after normalization
	}{
		{
			name:  "strips key parameter",
			input: "key=sk-example-key&foo=bar",
			want:  "foo=bar",
		},
		{
			name:  "strips KEY uppercase",
			input: "KEY=sk-example-key&foo=bar",
			want:  "foo=bar",
		},
		{
			name:  "strips Key mixed case",
			input: "Key=sk-example-key&foo=bar",
			want:  "foo=bar",
		},
		{
			name:  "strips mixed query auth params",
			input: "KEY=xxx&api-key=yyy&model=gemini-3-pro-image",
			want:  "model=gemini-3-pro-image",
		},
		{
			name:  "strips all proxy-internal params including key",
			input: "target=gemini-1&api-key=abc&key=xyz&api-version=2024-01-01&model=gpt-4",
			want:  "model=gpt-4",
		},
		{
			name:  "key only param results in empty string",
			input: "key=sk-example-key",
			want:  "",
		},
		{
			name:  "no key param preserves original",
			input: "foo=bar&baz=qux",
			want:  "baz=qux&foo=bar", // url.Values.Encode sorts keys
		},
		{
			name:  "empty query",
			input: "",
			want:  "",
		},
		{
			name:  "nil url",
			input: "",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &url.URL{RawQuery: tc.input}
			if tc.name == "nil url" {
				u = nil
			}
			got := normalizeForwardQuery(u)
			if got != tc.want {
				t.Errorf("normalizeForwardQuery(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDeduplicateV1Path(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// 无重复，原样返回
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/compatible-mode/v1/chat/completions", "/compatible-mode/v1/chat/completions"},
		{"/apps/anthropic/v1/messages", "/apps/anthropic/v1/messages"},
		{"/v1/responses", "/v1/responses"},
		{"/", "/"},
		{"", ""},

		// /v1/v1 重复 → 去重
		{"/v1/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/v1/responses", "/v1/responses"},
		{"/v1/v1/embeddings", "/v1/embeddings"},
		{"/v1/v1/messages", "/v1/messages"},
		{"/v1/v1", "/v1"},

		// 带前缀的 /v1/v1 重复
		{"/compatible-mode/v1/v1/chat/completions", "/compatible-mode/v1/chat/completions"},
		{"/compatible-mode/v1/v1/responses", "/compatible-mode/v1/responses"},
		{"/apps/anthropic/v1/v1/messages", "/apps/anthropic/v1/messages"},

		// 不应误伤：/v2/v1 不是重复
		{"/v2/v1/chat/completions", "/v2/v1/chat/completions"},
		// /v1/v2 不是重复
		{"/v1/v2/something", "/v1/v2/something"},
	}
	for _, tc := range cases {
		got := deduplicateV1Path(tc.input)
		if got != tc.want {
			t.Errorf("deduplicateV1Path(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestBuildURLStripsRedundantClientVersion(t *testing.T) {
	// 场景：endpoint 已声明版本段，客户端 SDK 也带 /v1 前缀，应剥除客户端版本前缀
	// 避免版本叠加（如智谱 GLM 的 /v4/ + OpenAI SDK 的 /v1/chat/completions）。
	cases := []struct {
		name       string
		endpoint   string
		clientPath string
		want       string
	}{
		{
			name:       "GLM v4 endpoint + client /v1/chat/completions → no /v4/v1",
			endpoint:   "https://open.bigmodel.cn/api/paas/v4/",
			clientPath: "/v1/chat/completions",
			want:       "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		},
		{
			name:       "GLM v4 endpoint + client /v1/responses",
			endpoint:   "https://open.bigmodel.cn/api/paas/v4/",
			clientPath: "/v1/responses",
			want:       "https://open.bigmodel.cn/api/paas/v4/responses",
		},
		{
			name:       "GLM v4 endpoint + client /v1/embeddings",
			endpoint:   "https://open.bigmodel.cn/api/paas/v4/",
			clientPath: "/v1/embeddings",
			want:       "https://open.bigmodel.cn/api/paas/v4/embeddings",
		},
		{
			name:       "v4 endpoint + client /chat/completions (no client v1)",
			endpoint:   "https://open.bigmodel.cn/api/paas/v4/",
			clientPath: "/chat/completions",
			want:       "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		},
		// 标准场景不受影响
		{
			name:       "OpenAI /v1 endpoint + client /v1/chat/completions (same version, strips)",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
		{
			name:       "Claude root endpoint + client /v1/messages (no endpoint version, keep)",
			endpoint:   "https://api.anthropic.com",
			clientPath: "/v1/messages",
			want:       "https://api.anthropic.com/v1/messages",
		},
		{
			name:       "Gemini root endpoint + client /v1beta/models (no endpoint version, keep)",
			endpoint:   "https://generativelanguage.googleapis.com",
			clientPath: "/v1beta/models/gemini-flash:generateContent",
			want:       "https://generativelanguage.googleapis.com/v1beta/models/gemini-flash:generateContent",
		},
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := url.Parse(tc.endpoint)
			target := &Target{
				Name:         "version-test",
				EndpointType: config.EndpointTypeOpenAI,
				Endpoint:     endpoint,
			}
			got, err := s.buildURL(target, &url.URL{Path: tc.clientPath})
			if err != nil {
				t.Fatalf("buildURL error: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("buildURL = %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestStripRedundantClientVersion(t *testing.T) {
	cases := []struct {
		name         string
		endpointPath string
		clientPath   string
		want         string
	}{
		{"both v4/v1 stripped", "/api/paas/v4/", "/v1/chat/completions", "/chat/completions"},
		{"both v1 stripped", "/v1", "/v1/chat/completions", "/chat/completions"},
		{"both v1beta", "/v1beta", "/v1beta/models", "/models"},
		{"endpoint no version keep", "", "/v1/chat/completions", "/v1/chat/completions"},
		{"endpoint root keep", "/", "/v1/messages", "/v1/messages"},
		{"client no version keep", "/v4/", "/chat/completions", "/chat/completions"},
		{"empty client", "/v4/", "", ""},
		{"empty endpoint", "", "/v1/chat", "/v1/chat"},
		{"client only version becomes root", "/v4/", "/v1", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripRedundantClientVersion(tc.endpointPath, tc.clientPath)
			if got != tc.want {
				t.Errorf("stripRedundantClientVersion(%q, %q) = %q, want %q",
					tc.endpointPath, tc.clientPath, got, tc.want)
			}
		})
	}
}

func TestBuildURLDualProtocolFallbackToPrefixMode(t *testing.T) {
	// 场景：双 endpoint 都未填，回退到 prefix 模式（向后兼容百炼等配置）
	baseEP, _ := url.Parse("https://token-plan.cn-beijing.maas.aliyuncs.com")
	s := &Service{providerRegistry: DefaultProviderRegistry()}
	target := &Target{
		Name:            "bailian-fallback",
		EndpointType:    config.EndpointTypeDualProtocol,
		Endpoint:        baseEP,
		OpenAIPrefix:    "/compatible-mode",
		AnthropicPrefix: "/apps/anthropic",
	}

	got, err := s.buildURL(target, &url.URL{Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}
	if want := "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions"; got.String() != want {
		t.Errorf("prefix mode OpenAI: got %q, want %q", got.String(), want)
	}

	got, err = s.buildURL(target, &url.URL{Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}
	if want := "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1/messages"; got.String() != want {
		t.Errorf("prefix mode Anthropic: got %q, want %q", got.String(), want)
	}
}

func TestBuildURLOpenAIDeduplicatesV1Path(t *testing.T) {
	// 场景：target endpoint URL 自带 /v1，客户端也发 /v1/...，不应出现 /v1/v1
	cases := []struct {
		name       string
		endpoint   string
		clientPath string
		want       string
	}{
		{
			name:       "endpoint with /v1 + client /v1/chat/completions",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
		{
			name:       "endpoint with /v1 + client /v1/responses",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/responses",
			want:       "https://api.openai.com/v1/responses",
		},
		{
			name:       "endpoint with /v1 + client /v1/embeddings",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/embeddings",
			want:       "https://api.openai.com/v1/embeddings",
		},
		{
			name:       "endpoint without /v1 + client /v1/chat/completions",
			endpoint:   "https://api.openai.com",
			clientPath: "/v1/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
		{
			name:       "endpoint without /v1 + client /chat/completions",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := url.Parse(tc.endpoint)
			target := &Target{
				Name:         "openai-test",
				EndpointType: config.EndpointTypeOpenAI,
				Endpoint:     endpoint,
			}
			got, err := s.buildURL(target, &url.URL{Path: tc.clientPath})
			if err != nil {
				t.Fatalf("buildURL error: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("buildURL = %q, want %q", got.String(), tc.want)
			}
		})
	}
}
