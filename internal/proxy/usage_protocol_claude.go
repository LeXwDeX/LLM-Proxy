package proxy

// Claude input_tokens excludes both cache partitions. TTL counters partition W.
func parseClaudeUsage(m map[string]any) usageTokens {
	var t usageTokens
	t.InputTokens = t.count(m, "input_tokens", usageFieldInput)
	t.OutputTokens = t.count(m, "output_tokens", usageFieldOutput)
	// Thinking is already included in output; preserve it for observation only.
	t.ReasoningTokens = t.count(usageDetails(m, "output_tokens_details"), "thinking_tokens", usageFieldReasoning)
	if t.ReasoningTokens > t.OutputTokens {
		t.conflict("reasoning_exceeds_output")
	}
	t.CachedTokens = t.count(m, "cache_read_input_tokens", usageFieldCached)
	t.CacheCreationTokens = t.count(m, "cache_creation_input_tokens", usageFieldCacheCreation)
	d := usageDetails(m, "cache_creation")
	t.CacheCreation5mTokens = t.count(d, "ephemeral_5m_input_tokens", usageFieldCacheCreation5m)
	t.CacheCreation1hTokens = t.count(d, "ephemeral_1h_input_tokens", usageFieldCacheCreation1h)
	if t.CacheCreation5mTokens > t.CacheCreationTokens || t.CacheCreation1hTokens > t.CacheCreationTokens-t.CacheCreation5mTokens {
		t.conflict("cache_ttl_exceeds_creation")
	}
	if iterations, ok := m["iterations"].([]any); ok && len(iterations) > 0 {
		t.costIncomplete = true
		t.diagnostic = "multi_model_iterations"
	}
	if claudeServerToolCharges(usageDetails(m, "server_tool_use")) {
		t.costIncomplete = true
		t.diagnostic = "server_tool_charges"
	}
	return t
}

// Only known, valid zero counters establish no tool charge. Unknown members or
// invalid counters keep billing conservative without inventing token usage.
func claudeServerToolCharges(tools map[string]any) bool {
	for key := range tools {
		switch key {
		case "web_fetch_requests", "web_search_requests":
			var observed usageTokens
			if observed.count(tools, key, 0) > 0 || observed.status == usageStatusConflict {
				return true
			}
		default:
			return true
		}
	}
	return false
}
