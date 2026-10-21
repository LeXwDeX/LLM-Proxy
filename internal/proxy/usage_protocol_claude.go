package proxy

import "math"

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
		if reason := claudeIterationsIncomplete(iterations, m); reason != "" {
			t.costIncomplete = true
			t.diagnostic = reason
		}
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

// claudeIterationCounters are the top-level counters each executor iteration
// may repeat. Top-level usage is the sum of all type "message" iterations.
var claudeIterationCounters = [...]string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}

// claudeIterationsIncomplete decides whether top-level usage alone prices a
// response whose usage carries a non-empty iterations array. It never adds
// iteration counters to the billed tokens; it only names why the cost may be
// incomplete, or returns "" when top-level usage covers every iteration.
//
// Official contracts checked 2026-10-21 (a documentation check is not
// acceptance against the real API):
//   - advisor tool: "message" items are executor sampling billed at the
//     executor price and every top-level field is their sum; "advisor_message"
//     items carry their own model and price and are excluded from top-level
//     https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool
//   - threshold compaction: "compaction" items are excluded from top-level;
//     with the beta every response carries iterations, items may omit cache
//     counters https://platform.claude.com/docs/en/build-with-claude/compaction-threshold
//   - on-demand compaction: top-level is zero, usage only in the compaction item
//     https://platform.claude.com/docs/en/build-with-claude/compaction-on-demand
//   - server-side fallback: "fallback_message" items are billed at the
//     fallback model price; top-level covers only the returned attempt
//     https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
//
// Priority: multi_model_iterations > compaction_iterations >
// unknown_usage_iterations > iterations_usage_mismatch. Items without a type,
// with an unknown type, invalid or missing required counters, a non-string or
// empty model, or non-object items have unknown
// attribution and stay conservative. Applies to every target parsed by the
// Anthropic rule; it never infers anything from host or model name.
func claudeIterationsIncomplete(iterations []any, top map[string]any) string {
	var multiModel, compaction, unknown, mismatch bool
	model := ""
	var sums [len(claudeIterationCounters)]int64
	var reported [len(claudeIterationCounters)]int
	messages := 0
	for _, raw := range iterations {
		item, ok := raw.(map[string]any)
		if !ok {
			unknown = true
			continue
		}
		var observed usageTokens
		var counts [len(claudeIterationCounters)]int64
		for i, key := range claudeIterationCounters {
			counts[i] = observed.count(item, key, 0)
		}
		if observed.status == usageStatusConflict {
			unknown = true
		}
		if v, present := item["model"]; present {
			// A non-string or empty model is unknown attribution, not "no
			// model seen yet".
			name, ok := v.(string)
			switch {
			case !ok || name == "":
				unknown = true
			case model == "":
				model = name
			case name != model:
				multiModel = true
			}
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			messages++
			for i, key := range claudeIterationCounters {
				if _, present := item[key]; !present {
					if key == "input_tokens" || key == "output_tokens" {
						unknown = true
					}
					continue
				}
				reported[i]++
				if counts[i] > math.MaxInt64-sums[i] {
					mismatch = true
				}
				sums[i] += counts[i]
			}
		case "advisor_message", "fallback_message":
			multiModel = true
		case "compaction":
			compaction = true
		default:
			unknown = true
		}
	}
	// A complete sum must equal top-level. A partial sum is only a lower
	// bound, and can disprove consistency only against a reported top-level
	// count; missing optional counters must not be inferred as zero.
	for i, key := range claudeIterationCounters {
		if messages == 0 || reported[i] == 0 {
			continue
		}
		if key == "cache_read_input_tokens" || key == "cache_creation_input_tokens" {
			if _, present := top[key]; !present {
				// Known positive cache usage cannot be priced from a missing
				// top-level counter, even if other iterations omit it too.
				if sums[i] > 0 {
					unknown = true
				}
				continue
			}
		}
		var observed usageTokens
		topCount := observed.count(top, key, 0)
		if reported[i] == messages {
			if topCount != sums[i] {
				mismatch = true
			}
		} else if _, present := top[key]; present && sums[i] > topCount {
			mismatch = true
		}
	}
	switch {
	case multiModel:
		return "multi_model_iterations"
	case compaction:
		return "compaction_iterations"
	case unknown:
		return "unknown_usage_iterations"
	case mismatch:
		return "iterations_usage_mismatch"
	}
	return ""
}
