package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"testing"
)

func TestUsageReviewLifecycle(t *testing.T) {
	cases := []struct {
		name                 string
		protocol             config.ProtocolType
		body, status, reason string
		in, out              int64
	}{
		{"responses failed JSON", config.ProtocolOpenAIResponses, `{"status":"failed","usage":{"input_tokens":12,"output_tokens":3}}`, "failed", "", 12, 3},
		{"gemini blocked JSON", config.ProtocolGemini, `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":0}}`, "blocked", "SAFETY", 12, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, found := (usageParser{protocol: tc.protocol}).extractTokens("application/json", []byte(tc.body))
			if !found || got.InputTokens != tc.in || got.OutputTokens != tc.out || got.responseStatus != tc.status || got.finishReason != tc.reason {
				t.Fatalf("got %+v found=%v", got, found)
			}
		})
	}
}

func TestQwenMissingUsageDoesNotFabricatePresence(t *testing.T) {
	got, _, found := (usageParser{protocol: config.ProtocolOpenAIChat, usageAdapter: "qwen", model: "qwen3.8-max", requestBody: []byte(`{"messages":[]}`)}).extractTokens("application/json", []byte(`{"choices":[]}`))
	if found || got.fields != 0 || got.status != usageStatusIncomplete {
		t.Fatalf("invented usage: %+v found=%v", got, found)
	}
}

func TestGeminiBlockedStreamHasProtocolTerminal(t *testing.T) {
	wire := "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"},\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":0}}\n\n"
	got, _, found := (usageParser{protocol: config.ProtocolGemini}).extractSSE([]byte(wire))
	if !found || got.status != usageStatusComplete || got.responseStatus != "blocked" || got.finishReason != "SAFETY" {
		t.Fatalf("blocked response lost: %+v", got)
	}
}
