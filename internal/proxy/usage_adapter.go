package proxy

import "github.com/LeXwDeX/LLM-Proxy/internal/config"

const (
	usageAdapterStandardOpenAI    = "standard_openai"
	usageAdapterStandardAnthropic = "standard_anthropic"
	usageAdapterStandardGemini    = "standard_gemini"
	usageAdapterQwen              = "qwen"
	usageAdapterGLM               = "glm"
	usageAdapterDeepSeek          = "deepseek"
	usageAdapterKimi              = "kimi"
	usageAdapterOpenRouter        = "openrouter"
	usageAdapterMinimax           = "minimax"
)

// Usage patches follow the explicit target choice, never the host or model.
// Gemini usage stays native; error patches use targetVendor independently.
func resolveUsageAdapter(configured string, op config.ProtocolType) (string, bool) {
	standard := usageAdapterStandardOpenAI
	if op == config.ProtocolAnthropicMessages {
		standard = usageAdapterStandardAnthropic
	}
	if op == config.ProtocolGemini {
		standard = usageAdapterStandardGemini
	}
	configured = config.NormalizeUsageAdapter(configured)
	if op == config.ProtocolGemini {
		return standard, false
	}
	switch configured {
	case usageAdapterQwen, usageAdapterGLM, usageAdapterDeepSeek, usageAdapterKimi:
		return configured, true
	}
	return standard, false
}

func targetVendor(target *Target) string {
	if target != nil {
		switch vendor := config.NormalizeUsageAdapter(target.UsageAdapter); vendor {
		case usageAdapterQwen, usageAdapterGLM, usageAdapterDeepSeek, usageAdapterKimi, usageAdapterOpenRouter, usageAdapterMinimax:
			return vendor
		}
	}
	return config.UsageAdapterStandard
}
