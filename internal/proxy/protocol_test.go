package proxy

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestResolveProtocol(t *testing.T) {
	tests := []struct {
		path     string
		expected config.ProtocolType
	}{
		// OpenAI Chat
		{"/v1/chat/completions", config.ProtocolOpenAIChat},
		{"/chat/completions", config.ProtocolOpenAIChat},

		// OpenAI Responses
		{"/v1/responses", config.ProtocolOpenAIResponses},
		{"/v1/responses/resp_abc123", config.ProtocolOpenAIResponses},
		{"/openai/v1/responses", config.ProtocolOpenAIResponses},
		{"/openai/deployments/gpt-5/responses", config.ProtocolOpenAIResponses},

		// Anthropic Messages
		{"/v1/messages", config.ProtocolAnthropicMessages},
		{"/v1/messages/count_tokens", config.ProtocolAnthropicMessages},

		// OpenAI Image
		{"/v1/images/generations", config.ProtocolOpenAIImage},
		{"/v1/images/edits", config.ProtocolOpenAIImage},
		{"/v1/images/variations", config.ProtocolOpenAIImage},

		// Gemini
		{"/v1beta/models/gemini-2.5-flash:generateContent", config.ProtocolGemini},
		{"/v1beta/models/gemini-2.5-flash:streamGenerateContent", config.ProtocolGemini},
		{"/v1beta/models/gemini-2.5-flash:countTokens", config.ProtocolGemini},

		// Default → OpenAI Chat (catch-all for generic OpenAI-compatible paths)
		{"/v1/embeddings", config.ProtocolOpenAIChat},
		{"/v1/models", config.ProtocolOpenAIChat},
		{"/v1/completions", config.ProtocolOpenAIChat},
		{"/", config.ProtocolOpenAIChat},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := ResolveProtocol(tc.path)
			if got != tc.expected {
				t.Errorf("ResolveProtocol(%q) = %q, want %q", tc.path, got, tc.expected)
			}
		})
	}
}

func TestProtocolSupportsPath(t *testing.T) {
	// Protocol × Path compatibility matrix.
	// Each row: protocol → list of (path, expected) pairs.
	tests := []struct {
		protocol config.ProtocolType
		path     string
		expected bool
	}{
		// OpenAIChat: supports chat/completions, embeddings, models (default catch-all)
		{config.ProtocolOpenAIChat, "/v1/chat/completions", true},
		{config.ProtocolOpenAIChat, "/v1/embeddings", true},
		{config.ProtocolOpenAIChat, "/v1/models", true},
		{config.ProtocolOpenAIChat, "/v1/responses", false},
		{config.ProtocolOpenAIChat, "/responses", false},
		{config.ProtocolOpenAIChat, "/v1/messages", false},
		{config.ProtocolOpenAIChat, "/v1/images/generations", false},
		{config.ProtocolOpenAIChat, "/v1beta/models/gemini-2.5-flash:generateContent", false},

		// OpenAIResponses: only responses paths
		{config.ProtocolOpenAIResponses, "/v1/responses", true},
		{config.ProtocolOpenAIResponses, "/v1/responses/resp_abc", true},
		{config.ProtocolOpenAIResponses, "/v1/chat/completions", false},
		{config.ProtocolOpenAIResponses, "/v1/messages", false},

		// AnthropicMessages: only messages paths
		{config.ProtocolAnthropicMessages, "/v1/messages", true},
		{config.ProtocolAnthropicMessages, "/v1/messages/count_tokens", true},
		{config.ProtocolAnthropicMessages, "/v1/chat/completions", false},
		{config.ProtocolAnthropicMessages, "/v1/responses", false},

		// Gemini: only Gemini-style paths
		{config.ProtocolGemini, "/v1beta/models/gemini-2.5-flash:generateContent", true},
		{config.ProtocolGemini, "/v1beta/models/gemini-2.5-flash:streamGenerateContent", true},
		{config.ProtocolGemini, "/v1/chat/completions", false},
		{config.ProtocolGemini, "/v1/messages", false},

		// OpenAIImage: only image paths
		{config.ProtocolOpenAIImage, "/v1/images/generations", true},
		{config.ProtocolOpenAIImage, "/v1/images/edits", true},
		{config.ProtocolOpenAIImage, "/v1/images/variations", true},
		{config.ProtocolOpenAIImage, "/v1/chat/completions", false},
		{config.ProtocolOpenAIImage, "/v1/messages", false},
	}

	for _, tc := range tests {
		name := string(tc.protocol) + "_" + tc.path
		t.Run(name, func(t *testing.T) {
			got := ProtocolSupportsPath(tc.protocol, tc.path)
			if got != tc.expected {
				t.Errorf("ProtocolSupportsPath(%q, %q) = %v, want %v", tc.protocol, tc.path, got, tc.expected)
			}
		})
	}
}
