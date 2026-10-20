// url_provider_test.go — buildURL 通过 ProviderProfile.Path 改写路径的集成测试。
package proxy

import (
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestBuildURLWithProviderProfile(t *testing.T) {
	cases := []struct {
		name         string
		endpointType string
		endpoint     string
		clientPath   string
		wantURL      string
		// dual_protocol 专用
		openaiPrefix    string
		anthropicPrefix string
	}{
		// openai
		{"openai", config.EndpointTypeOpenAI, "https://api.openai.com", "/v1/chat/completions",
			"https://api.openai.com/v1/chat/completions", "", ""},

		// dual_protocol: DeepSeek style (no openai prefix, /anthropic for anthropic)
		{"dual_protocol deepseek openai", config.EndpointTypeDualProtocol, "https://api.deepseek.com", "/v1/chat/completions",
			"https://api.deepseek.com/v1/chat/completions", "", "/anthropic"},

		{"dual_protocol deepseek anthropic", config.EndpointTypeDualProtocol, "https://api.deepseek.com", "/v1/messages",
			"https://api.deepseek.com/anthropic/v1/messages", "", "/anthropic"},

		// dual_protocol: Bailian style (/compatible-mode for openai, /apps/anthropic for anthropic)
		{"dual_protocol bailian openai", config.EndpointTypeDualProtocol, "https://token-plan.cn-beijing.maas.aliyuncs.com", "/v1/chat/completions",
			"https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions", "/compatible-mode", "/apps/anthropic"},

		{"dual_protocol bailian anthropic", config.EndpointTypeDualProtocol, "https://token-plan.cn-beijing.maas.aliyuncs.com", "/v1/messages",
			"https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1/messages", "/compatible-mode", "/apps/anthropic"},

		// openai_image (terminal URL)
		{"openai_image terminal", config.EndpointTypeOpenAIImage, "https://api.openai.com/v1/images/generations", "/v1/images/generations",
			"https://api.openai.com/v1/images/generations", "", ""},

		// openai_image 终态 URL：网宿式固定地址，客户端 path 被整体覆盖
		{"openai_image wangsu terminal", config.EndpointTypeOpenAIImage, "https://aigateway.example.com/v1/abc123/openai-image", "/v1/images/generations",
			"https://aigateway.example.com/v1/abc123/openai-image", "", ""},

		// openai_image 级联 base（裸地址）：下一跳是另一台 llms-proxy，透传客户端 path
		{"openai_image cascade bare base", config.EndpointTypeOpenAIImage, "http://192.168.1.20:8000", "/v1/images/generations",
			"http://192.168.1.20:8000/v1/images/generations", "", ""},

		// openai_image 级联 base（仅版本段 /v1）：透传且版本段不叠加
		{"openai_image cascade v1 base", config.EndpointTypeOpenAIImage, "http://192.168.1.20:8000/v1", "/v1/images/generations",
			"http://192.168.1.20:8000/v1/images/generations", "", ""},

		// openai_image 级联 base：edits 路径同样透传
		{"openai_image cascade edits", config.EndpointTypeOpenAIImage, "http://192.168.1.20:8000/v1", "/v1/images/edits",
			"http://192.168.1.20:8000/v1/images/edits", "", ""},

		// azure deployment path
		{"azure deployment", config.EndpointTypeAzureOpenAI, "https://example.openai.azure.com", "/openai/deployments/gpt-4o/chat/completions",
			"https://example.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2026-03-01-preview", "", ""},
	}

	s := &Service{providerRegistry: DefaultProviderRegistry()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := url.Parse(tc.endpoint)
			target := &Target{
				Name:            tc.name,
				EndpointType:    tc.endpointType,
				Endpoint:        endpoint,
				OpenAIPrefix:    tc.openaiPrefix,
				AnthropicPrefix: tc.anthropicPrefix,
			}
			got, err := s.buildURL(target, &url.URL{Path: tc.clientPath})
			if err != nil {
				t.Fatalf("buildURL error: %v", err)
			}
			if got.String() != tc.wantURL {
				t.Errorf("buildURL = %q, want %q", got.String(), tc.wantURL)
			}
		})
	}
}
