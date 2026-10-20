package proxy

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestPathSupportedByEndpointType(t *testing.T) {
	tests := []struct {
		epType string
		path   string
		want   bool
	}{
		// OpenAI 兼容类型支持 OpenAI Chat / Responses，不接 Anthropic Messages
		{config.EndpointTypeOpenAI, "/v1/responses", true},
		{config.EndpointTypeOpenAI, "/v1/chat/completions", true},
		{config.EndpointTypeOpenAI, "/v1/messages", false},
		{config.EndpointTypeAzureOpenAI, "/v1/responses", true},
		{config.EndpointTypeAzureOpenAI, "/openai/v1/responses", true},
		{config.EndpointTypeAzureOpenAI, "/openai/deployments/gpt-5/responses", true},
		{config.EndpointTypeAzureOpenAI, "/v1/messages", false},
		// Anthropic 类型只接 Anthropic Messages
		{config.EndpointTypeClaude, "/v1/messages", true},
		{config.EndpointTypeClaude, "/v1/chat/completions", false},
		{config.EndpointTypeClaude, "/v1/responses", false},
		// Gemini 视为 OpenAI Chat 兼容，不接 Anthropic / Responses
		{config.EndpointTypeGemini, "/v1/models/gemini:generateContent", true},
		{config.EndpointTypeGemini, "/v1/chat/completions", true},
		{config.EndpointTypeGemini, "/v1/messages", false},
		{config.EndpointTypeGemini, "/v1/responses", false},
		{config.EndpointTypeGemini, "/openai/deployments/gpt-5/responses", false},
		// 双协议类型按 path schema 自动分流
		{config.EndpointTypeDualProtocol, "/v1/messages", true},
		{config.EndpointTypeDualProtocol, "/v1/chat/completions", true},
		{config.EndpointTypeDualProtocol, "/v1/responses", true},
		// openai_image：仅图片路径
		{config.EndpointTypeOpenAIImage, "/v1/images/generations", true},
		{config.EndpointTypeOpenAIImage, "/v1/images/edits", true},
		{config.EndpointTypeOpenAIImage, "/v1/images/variations", true},
		{config.EndpointTypeOpenAIImage, "/v1/chat/completions", false},
		{config.EndpointTypeOpenAIImage, "/", false},
	}
	for _, tt := range tests {
		got := PathSupportedByEndpointType(tt.epType, tt.path)
		if got != tt.want {
			t.Errorf("PathSupportedByEndpointType(%q, %q) = %v, want %v", tt.epType, tt.path, got, tt.want)
		}
	}
}

func TestOpenAIImageOperationSupportsVariationsPath(t *testing.T) {
	if !openAIImageOperationSupportsPath(config.ImageOperationVariations, "/v1/images/variations") {
		t.Fatal("expected variations operation to support /v1/images/variations")
	}
	if openAIImageOperationSupportsPath(config.ImageOperationVariations, "/v1/images/generations") {
		t.Fatal("expected variations operation to reject /v1/images/generations")
	}
}

func TestDualProtocolResponsesFlagAppliesToAllResponsesPaths(t *testing.T) {
	target := &Target{EndpointType: config.EndpointTypeDualProtocol}
	for _, path := range []string{"/v1/responses", "/openai/v1/responses", "/openai/deployments/model/responses"} {
		if target.SupportsPath(path) {
			t.Errorf("disabled Responses accepted %s", path)
		}
		target.SupportsResponses = true
		if !target.SupportsPath(path) {
			t.Errorf("enabled Responses rejected %s", path)
		}
		target.SupportsResponses = false
	}
}

func TestUnknownEndpointHelperPreservesCompatibilityWithoutAcceptingUnknownEntry(t *testing.T) {
	for _, endpoint := range []string{"unknown", ""} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/images/generations", "/unknown"} {
			if !PathSupportedByEndpointType(endpoint, path) {
				t.Errorf("legacy default rejected %s %s", endpoint, path)
			}
		}
		for _, path := range []string{"/v1/messages", "/v1beta/models/test:generateContent"} {
			if PathSupportedByEndpointType(endpoint, path) {
				t.Errorf("legacy default accepted incompatible %s", path)
			}
		}
	}
	if ResolveProtocol("/unknown") != config.ProtocolOpenAIChat {
		t.Fatal("public protocol helper default changed")
	}
	if classifyClientPath("/unknown") != clientPathUnknown || ProtocolSupportsPath(config.ProtocolOpenAIChat, "/unknown") {
		t.Fatal("strict request entry accepted unknown path")
	}
}
