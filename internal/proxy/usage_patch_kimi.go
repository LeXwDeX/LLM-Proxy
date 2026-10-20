package proxy

import (
	"encoding/json"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func (p usageParser) patchKimiUsage(t *usageTokens, m map[string]any, op config.ProtocolType) {
	if op == config.ProtocolOpenAIChat {
		if _, exists := m["cached_tokens"]; exists {
			hit := t.count(m, "cached_tokens", usageFieldCached)
			if _, nested := usageDetails(m, "prompt_tokens_details")["cached_tokens"]; nested && t.CachedTokens != hit {
				t.conflict("conflicting_cache_aliases")
			}
			t.CachedTokens = hit
			t.partition(readInt64(m["prompt_tokens"]))
		}
	}
	// Anthropic already reports its native TTL subsets. Responses has no proven
	// request TTL contract; never borrow Chat defaults for that operation.
	if t.CacheCreationTokens == 0 || (op != config.ProtocolOpenAIChat && op != config.ProtocolOpenAIResponses) {
		return
	}
	ttl := ""
	if op == config.ProtocolOpenAIChat {
		ttl = kimiChatCacheTTL(p.requestBody)
	}
	switch ttl {
	case "5m":
		t.CacheCreation5mTokens = t.CacheCreationTokens
		t.fields |= usageFieldCacheCreation5m
	case "1h":
		t.CacheCreation1hTokens = t.CacheCreationTokens
		t.fields |= usageFieldCacheCreation1h
	default:
		t.costIncomplete = true
		if t.diagnostic == "" {
			t.diagnostic = "unknown_cache_write_ttl"
		}
	}
}

// Defaults are evidence only with a valid object for the actual outbound Chat
// request. A malformed or unavailable context must not silently select 5m.
func kimiChatCacheTTL(body []byte) string {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return ""
	}
	raw, present := request["prompt_cache_options"]
	if !present {
		return "5m"
	}
	var options map[string]json.RawMessage
	if json.Unmarshal(raw, &options) != nil || options == nil {
		return ""
	}
	raw, present = options["ttl"]
	if !present {
		return "5m"
	}
	var ttl string
	if json.Unmarshal(raw, &ttl) != nil || (ttl != "5m" && ttl != "1h") {
		return ""
	}
	return ttl
}
