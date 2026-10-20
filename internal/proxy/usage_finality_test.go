package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"testing"
)

func TestUsageFailureCannotPromotePreliminarySnapshot(t *testing.T) {
	tests := []struct {
		name     string
		protocol config.ProtocolType
		wire     string
	}{
		{"responses failed null", config.ProtocolOpenAIResponses, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":null}}\n\n"},
		{"responses incomplete null", config.ProtocolOpenAIResponses, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"usage\":null}}\n\n"},
		{"claude start then error", config.ProtocolAnthropicMessages, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"},
		{"claude stop without final delta", config.ProtocolAnthropicMessages, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, found := (usageParser{protocol: tt.protocol}).extractSSE([]byte(tt.wire))
			if !found || got.InputTokens != 10 || got.status != usageStatusIncomplete || !got.costIncomplete {
				t.Fatalf("preliminary usage promoted to complete: %+v found=%v", got, found)
			}
		})
	}
}
func TestUsageFailureWithFinalSnapshotRemainsComplete(t *testing.T) {
	wire := "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	got, _, found := (usageParser{protocol: config.ProtocolOpenAIResponses}).extractSSE([]byte(wire))
	if !found || got.InputTokens != 10 || got.OutputTokens != 2 || got.status != usageStatusComplete || got.costIncomplete {
		t.Fatalf("lost authoritative usage on failure: %+v", got)
	}
}
func TestUsageGeminiOutputOverflowRejected(t *testing.T) {
	got, _, found := (usageParser{protocol: config.ProtocolGemini}).extractTokens("application/json", []byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":9223372036854775807,"thoughtsTokenCount":1}}`))
	if !found || got.OutputTokens < 0 || got.status != usageStatusConflict || !got.costIncomplete {
		t.Fatalf("overflow treated as valid output: %+v", got)
	}
}
