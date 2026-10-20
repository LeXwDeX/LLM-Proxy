package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"net/url"
	"strings"
	"testing"
)

// Fixtures follow the official sources indexed by protocol-handling-contract.md.
func TestUsageSnapshotsRespectProtocolAndPresence(t *testing.T) {
	tests := []struct {
		name                  string
		parser                usageParser
		wire                  string
		in, out, cache, write int64
	}{
		{"claude corrected zero replaces preliminary input", usageParser{protocol: config.ProtocolAnthropicMessages}, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"output_tokens\":8,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n", 0, 8, 100, 0},
		{"openai reclassifies partition", usageParser{protocol: config.ProtocolOpenAIChat}, "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":0}}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\ndata: [DONE]\n\n", 20, 5, 80, 0},
		{"multiline CR events", usageParser{protocol: config.ProtocolOpenAIChat}, "event: chunk\rdata: {\"usage\":\rdata: {\"prompt_tokens\":9,\"completion_tokens\":2}}\r\rdata: [DONE]\r\r", 9, 2, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUsageStream("text/event-stream", "", tt.parser)
			for _, b := range []byte(tt.wire) {
				_, _ = u.Write([]byte{b})
			}
			u.Close()
			got := u.tokens
			if !u.found || got.InputTokens != tt.in || got.OutputTokens != tt.out || got.CachedTokens != tt.cache || got.CacheCreationTokens != tt.write || got.status != usageStatusComplete {
				t.Fatalf("got %+v found=%v", got, u.found)
			}
		})
	}
}
func TestUsageLateSnapshotBeyondDiagnosticCapture(t *testing.T) {
	wire := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"padding\"}}]}\n\n", 5000) + "data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\n" + strings.Repeat("data: {\"type\":\"unknown\"}\n\n", 5000) + "data: [DONE]\n\n"
	got, _, ok := (usageParser{protocol: config.ProtocolOpenAIChat}).extractSSE([]byte(wire))
	if !ok || got.InputTokens != 12 || got.OutputTokens != 7 {
		t.Fatalf("lost middle usage: %+v", got)
	}
}
func TestUsageTerminalNullIsNotZeroUsage(t *testing.T) {
	u := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIResponses})
	_, _ = u.Write([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":null}}\n\n"))
	u.Close()
	if !u.terminal || u.found || u.tokens.status != usageStatusIncomplete || u.tokens.responseStatus != "failed" {
		t.Fatalf("terminal conflated with usage: %+v", u)
	}
}
func TestUsageKnownAdapterEdgeDimensions(t *testing.T) {
	endpoint, _ := url.Parse("https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/responses")
	p := usageParser{endpoint: endpoint, usageAdapter: "qwen", model: "qwen3.8-max", protocol: config.ProtocolOpenAIResponses}
	got, _, ok := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":100,"output_tokens":8,"input_tokens_details":{"cached_tokens":20},"x_details":[{"input_tokens":100,"input_tokens_details":{"cache_creation_input_tokens":30}}]}}`))
	if !ok || got.InputTokens != 50 || got.CacheCreationTokens != 30 {
		t.Fatalf("qwen nested write: %+v", got)
	}
	got, _, _ = (usageParser{protocol: config.ProtocolAnthropicMessages}).extractTokens("application/json", []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":30}}}`))
	if got.CacheCreation5mTokens != 20 || got.CacheCreation1hTokens != 30 || got.CacheCreationTokens != 50 {
		t.Fatalf("ttl partition: %+v", got)
	}
	got, _, _ = (usageParser{protocol: config.ProtocolOpenAIChat, model: "deepseek-chat", usageAdapter: "deepseek"}).extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20,"prompt_tokens_details":{"cached_tokens":70}}}`))
	if got.status != usageStatusConflict || !got.costIncomplete {
		t.Fatalf("conflicting aliases hidden: %+v", got)
	}
}

// Only the explicit choice activates the patch, regardless of host and model.
func TestUsageProviderPatchIgnoresModelAllowlist(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cache_creation_input_tokens":70}}}`)
	official, _ := url.Parse("https://dashscope.aliyuncs.com/compatible-mode/v1")
	got, _, _ := (usageParser{endpoint: official, usageAdapter: "standard", model: "qwen3-madeup", protocol: config.ProtocolOpenAIChat}).extractTokens("application/json", body)
	if got.adapter != usageAdapterStandardOpenAI || got.InputTokens != 100 || got.CacheCreationTokens != 0 {
		t.Fatalf("standard on official host patched: %+v", got)
	}
	selfHosted, _ := url.Parse("https://models.internal.example/v1")
	got, _, _ = (usageParser{endpoint: selfHosted, usageAdapter: "qwen", model: "unlisted-model", protocol: config.ProtocolOpenAIChat}).extractTokens("application/json", body)
	if got.adapter != usageAdapterQwen || got.InputTokens != 30 || got.CacheCreationTokens != 70 {
		t.Fatalf("explicit self hosted not patched: %+v", got)
	}
}
