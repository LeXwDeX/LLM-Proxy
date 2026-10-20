package proxy

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"strings"
)

func (p usageParser) patchQwenUsage(t *usageTokens, m map[string]any, op config.ProtocolType) {
	if op == config.ProtocolGemini {
		return
	}
	if op == config.ProtocolOpenAIChat {
		d := usageDetails(m, "prompt_tokens_details")
		// Alibaba creation fields differ from OpenAI's cache_write_tokens.
		t.fields &^= usageFieldCacheCreation
		t.CacheCreationTokens = t.count(d, "cache_creation_input_tokens", usageFieldCacheCreation)
		t.partition(readInt64(m["prompt_tokens"]))
	}
	if op == config.ProtocolOpenAIResponses {
		// Built-in tool calls have billing dimensions beyond model tokens. Keep
		// their accounting uncertainty inside the Qwen adapter.
		if len(usageDetails(m, "x_tools")) > 0 {
			t.costIncomplete = true
			t.diagnostic = "server_tool_charges"
		}
		t.fields &^= usageFieldCacheCreation
		t.CacheCreationTokens = 0
		details, _ := m["x_details"].([]any)
		for _, detail := range details {
			d, _ := detail.(map[string]any)
			if len(usageDetails(d, "plugins")) > 0 {
				t.costIncomplete = true
				t.diagnostic = "server_tool_charges"
			}
		}
		if len(details) == 1 {
			d, _ := details[0].(map[string]any)
			t.CacheCreationTokens = t.count(usageDetails(d, "input_tokens_details"), "cache_creation_input_tokens", usageFieldCacheCreation)
		}
		if len(details) > 1 {
			t.costIncomplete = true
			t.diagnostic = "ambiguous_qwen_x_details"
		}
		t.partition(readInt64(m["input_tokens"]))
	}
	// Cache mode follows the actual outbound request for every Qwen target.
	explicit := false
	if op == config.ProtocolOpenAIResponses {
		explicit = strings.EqualFold(p.requestHeaders.Get("x-dashscope-session-cache"), "enable")
	} else {
		var request any
		if json.Unmarshal(p.requestBody, &request) == nil {
			explicit = hasEphemeralCacheControl(request)
		}
	}
	missingContext := len(p.requestBody) == 0
	if op == config.ProtocolOpenAIResponses {
		missingContext = p.requestHeaders == nil
	}
	if missingContext && t.CachedTokens > 0 {
		t.costIncomplete = true
		t.diagnostic = "missing_cache_request_context"
		return
	}
	t.fields |= usageFieldExplicitCacheRead
	if explicit {
		t.ExplicitCacheReadTokens = t.CachedTokens
		if t.fields&usageFieldCacheCreation == 0 {
			t.costIncomplete = true
			t.diagnostic = "unobserved_cache_creation"
		}
	}
}

// Inspect only API cache-control locations, not arbitrary tool schemas or text.
func hasEphemeralCacheControl(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}
	marked := func(m map[string]any) bool {
		control, _ := m["cache_control"].(map[string]any)
		return control["type"] == "ephemeral"
	}
	if marked(root) {
		return true
	}
	blocksMarked := func(value any) bool {
		blocks, _ := value.([]any)
		for _, block := range blocks {
			m, _ := block.(map[string]any)
			if marked(m) {
				return true
			}
		}
		return false
	}
	if blocksMarked(root["system"]) {
		return true
	}
	messages, _ := root["messages"].([]any)
	for _, message := range messages {
		m, _ := message.(map[string]any)
		if marked(m) || blocksMarked(m["content"]) {
			return true
		}
	}
	return false
}
