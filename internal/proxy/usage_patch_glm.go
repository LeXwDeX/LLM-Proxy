package proxy

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

// GLM publishes standard Chat cache reads, but no independent reasoning count.
// Text in reasoning_content must never be converted into fabricated token usage.
func patchGLMUsage(t *usageTokens, m map[string]any, op config.ProtocolType) {
	// No arithmetic override: official GLM fields use the base protocol contract.
}
