package proxy

import (
	"math"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Issue #6: providers disagree on whether output text_tokens include reasoning.
// OpenAI-style counters may report reasoning inside text; the Gemini OpenAI
// compatibility layer reports them as disjoint parts (reasoning + text = output).
// Both are valid partitions; real overage must still be a conflict.
func TestOpenAIOutputTextPartitionAcceptsReasoningSemantics(t *testing.T) {
	imageOutRate := 40.0
	rates := usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 10, ImageOutputPer1MTokens: &imageOutRate}
	for _, tc := range []struct {
		name, details      string
		reasoning, imageOu int64
		conflict           string
	}{
		// Sample observed on 2026-10-20 from a real Gemini OpenAI-compatible Chat
		// request (redacted): 24 reasoning + 1 text = 25 output, no image_tokens.
		{"gemini disjoint reasoning", `{"reasoning_tokens":24,"text_tokens":1}`, 24, 0, ""},
		{"pure text no reasoning", `{"text_tokens":25}`, 0, 0, ""},
		{"pure text zero reasoning", `{"reasoning_tokens":0,"text_tokens":25}`, 0, 0, ""},
		{"reasoning included in text", `{"reasoning_tokens":24,"text_tokens":25}`, 24, 0, ""},
		{"image text reasoning disjoint", `{"reasoning_tokens":20,"text_tokens":1,"image_tokens":4}`, 20, 4, ""},
		{"image plus text including reasoning", `{"reasoning_tokens":10,"text_tokens":20,"image_tokens":5}`, 10, 5, ""},
		{"disjoint overage", `{"reasoning_tokens":24,"text_tokens":2}`, 24, 0, "image_output_text_partition_mismatch"},
		{"image disjoint overage", `{"reasoning_tokens":24,"text_tokens":1,"image_tokens":4}`, 24, 4, "image_output_text_partition_mismatch"},
		{"disjoint shortfall", `{"reasoning_tokens":10,"text_tokens":1}`, 10, 0, "image_output_text_partition_mismatch"},
		{"text exceeds output", `{"reasoning_tokens":0,"text_tokens":26}`, 0, 0, "image_output_text_partition_mismatch"},
		{"reasoning exceeds output", `{"reasoning_tokens":26,"text_tokens":1}`, 26, 0, "reasoning_exceeds_output"},
		{"image exceeds output", `{"text_tokens":0,"image_tokens":26}`, 0, 26, "image_partition_exceeds_total"},
		{"invalid text count", `{"reasoning_tokens":24,"text_tokens":-1}`, 24, 0, "invalid_count:text_tokens"},
	} {
		for _, op := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses} {
			for _, stream := range []bool{false, true} {
				name := tc.name + "/" + string(op) + map[bool]string{false: "/json", true: "/sse"}[stream]
				t.Run(name, func(t *testing.T) {
					usageJSON := `{"prompt_tokens":8,"completion_tokens":25,"total_tokens":33,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0,"text_tokens":8},"completion_tokens_details":` + tc.details + `}`
					var body, sse string
					if op == config.ProtocolOpenAIResponses {
						usageJSON = strings.NewReplacer("prompt_tokens", "input_tokens", "completion_tokens", "output_tokens").Replace(usageJSON)
						body = `{"object":"response","status":"completed","usage":` + usageJSON + `}`
						sse = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
					} else {
						body = `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + usageJSON + `}`
						sse = "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
							"data: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":" + usageJSON + "}\n\ndata: [DONE]\n\n"
					}
					p := usageParser{protocol: op}
					var got usageTokens
					var ok bool
					if stream {
						got, _, ok = p.extractSSE([]byte(sse))
					} else {
						got, _, ok = p.extractTokens("application/json", []byte(body))
					}
					if !ok || got.InputTokens != 8 || got.OutputTokens != 25 || got.ReasoningTokens != tc.reasoning {
						t.Fatalf("totals changed: %+v", got)
					}
					if tc.conflict != "" {
						if got.status != usageStatusConflict || !got.costIncomplete || got.diagnostic != tc.conflict {
							t.Fatalf("want conflict %q: status=%q incomplete=%v diagnostic=%q", tc.conflict, got.status, got.costIncomplete, got.diagnostic)
						}
						return
					}
					if got.status != usageStatusComplete || got.costIncomplete || got.diagnostic != "" || got.ImageOutputTokens != tc.imageOu {
						t.Fatalf("valid partition rejected: status=%q incomplete=%v diagnostic=%q imageOut=%d", got.status, got.costIncomplete, got.diagnostic, got.ImageOutputTokens)
					}
					// Reasoning stays a subset of output: billing uses the 25-token
					// total once, with image output priced separately.
					cost := rates.CostFor(got.InputTokens, got.OutputTokens, got.CachedTokens, got.CacheCreationTokens, got.ExplicitCacheReadTokens, got.CacheCreation5mTokens, got.CacheCreation1hTokens, got.ImageInputTokens, got.CachedImageInputTokens, got.ImageOutputTokens)
					want := (8*1 + float64(25-tc.imageOu)*10 + float64(tc.imageOu)*imageOutRate) / 1e6
					if math.Abs(cost-want) > 1e-12 {
						t.Fatalf("cost=%v want=%v", cost, want)
					}
				})
			}
		}
	}
}

// Vendor usage patches run before the image partition check; the disjoint
// reasoning partition must remain valid for every explicit adapter selection.
func TestOpenAIOutputTextPartitionReasoningAcrossAdapters(t *testing.T) {
	body := `{"usage":{"prompt_tokens":8,"completion_tokens":25,"total_tokens":33,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0,"text_tokens":8},"completion_tokens_details":{"reasoning_tokens":24,"text_tokens":1}}}`
	for _, adapter := range []string{"standard", "qwen", "glm", "deepseek", "kimi", "openrouter", "minimax"} {
		t.Run(adapter, func(t *testing.T) {
			p := usageParser{protocol: config.ProtocolOpenAIChat, usageAdapter: adapter, requestBody: []byte(`{"messages":[]}`)}
			got, _, _ := p.extractTokens("application/json", []byte(body))
			if got.OutputTokens != 25 || got.ReasoningTokens != 24 || got.status != usageStatusComplete || got.diagnostic == "image_output_text_partition_mismatch" {
				t.Fatalf("usage=%+v", got)
			}
		})
	}
}

// The Images protocol keeps deriving image output from text when image_tokens
// is absent; reasoning semantics do not change that contract.
func TestOpenAIImageOutputDerivationUnchangedByReasoningRule(t *testing.T) {
	p := usageParser{protocol: config.ProtocolOpenAIImage}
	got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":12,"output_tokens":20,"output_tokens_details":{"text_tokens":8}}}`))
	if got.ImageOutputTokens != 12 || got.status != usageStatusComplete || got.costIncomplete {
		t.Fatalf("usage=%+v", got)
	}
}

// Images keeps text + image = output even when reasoning is reported. The
// Chat/Responses disjoint-reasoning allowance must not hide missing image usage.
func TestOpenAIImageOutputRejectsDisjointReasoningPartition(t *testing.T) {
	body := `{"usage":{"input_tokens":8,"output_tokens":25,"output_tokens_details":{"text_tokens":1,"image_tokens":4,"reasoning_tokens":20}}}`
	for _, chatKeys := range []bool{false, true} {
		t.Run(map[bool]string{false: "image keys", true: "compatible chat keys"}[chatKeys], func(t *testing.T) {
			payload := body
			if chatKeys {
				payload = strings.NewReplacer("input_tokens", "prompt_tokens", "output_tokens", "completion_tokens").Replace(payload)
			}
			p := usageParser{protocol: config.ProtocolOpenAIImage}
			got, _, ok := p.extractTokens("application/json", []byte(payload))
			if !ok || got.InputTokens != 8 || got.OutputTokens != 25 || got.ImageOutputTokens != 4 || got.ReasoningTokens != 20 {
				t.Fatalf("totals changed: %+v", got)
			}
			if got.status != usageStatusConflict || !got.costIncomplete || got.diagnostic != "image_output_text_partition_mismatch" {
				t.Fatalf("want Images partition conflict: status=%q incomplete=%v diagnostic=%q", got.status, got.costIncomplete, got.diagnostic)
			}
		})
	}
}
