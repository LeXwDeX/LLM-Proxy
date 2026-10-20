package proxy

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

func patchDeepSeekUsage(t *usageTokens, m map[string]any, op config.ProtocolType) {
	if op != config.ProtocolOpenAIChat {
		return
	}
	if _, exists := m["prompt_cache_hit_tokens"]; !exists {
		return
	}
	standard, hasStandard := usageDetails(m, "prompt_tokens_details")["cached_tokens"]
	hit := t.count(m, "prompt_cache_hit_tokens", usageFieldCached)
	if hasStandard && readInt64(standard) != hit {
		t.conflict("conflicting_cache_aliases")
	}
	t.CachedTokens = hit
	total := readInt64(m["prompt_tokens"])
	if _, exists := m["prompt_cache_miss_tokens"]; exists {
		miss := t.count(m, "prompt_cache_miss_tokens", usageFieldInput)
		if hit > total || miss != total-hit {
			t.conflict("deepseek_hit_miss_total")
		}
	}
	t.partition(total)
}
