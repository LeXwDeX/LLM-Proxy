package proxy

import "math"

// GenerateContent candidates exclude thoughts. Tool input is an independent unit.
func parseGeminiUsage(m map[string]any) usageTokens {
	var t usageTokens
	total := t.count(m, "promptTokenCount", usageFieldInput)
	t.OutputTokens = t.count(m, "candidatesTokenCount", usageFieldOutput)
	t.CachedTokens = t.count(m, "cachedContentTokenCount", usageFieldCached)
	t.ReasoningTokens = t.count(m, "thoughtsTokenCount", usageFieldReasoning)
	if t.ReasoningTokens > 0 {
		if t.ReasoningTokens > math.MaxInt64-t.OutputTokens {
			t.conflict("output_tokens_overflow")
			// The combined output cannot be represented; do not emit a negative bill.
			t.OutputTokens = 0
		} else {
			t.OutputTokens += t.ReasoningTokens
		}
	}
	t.ToolUsePromptTokens = t.count(m, "toolUsePromptTokenCount", usageFieldToolUsePrompt)
	t.partition(total)
	if t.ToolUsePromptTokens > 0 {
		t.costIncomplete = true
		t.diagnostic = "tool_input_price_scope"
	}
	if t.CachedTokens > 0 {
		t.costIncomplete = true
		t.diagnostic = "cache_storage_not_in_usage"
	}
	return t
}
