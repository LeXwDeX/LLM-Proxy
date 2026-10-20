// protocol.go — 协议解析与协议-路径兼容性检查。
// 协议与路由能力共用请求 schema 分类，严格入口校验仍由 request_path.go 负责。
package proxy

import (
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ResolveProtocol 从客户端请求路径解析协议类型。
// 判断顺序：Anthropic path → Responses path → Image path → Gemini path → Chat path → 默认 OpenAIChat
func ResolveProtocol(path string) config.ProtocolType {
	switch requestSchemaForPath(path) {
	case config.RequestSchemaAnthropic:
		return config.ProtocolAnthropicMessages
	case config.RequestSchemaOpenAIResponses:
		return config.ProtocolOpenAIResponses
	case config.RequestSchemaOpenAIImage:
		return config.ProtocolOpenAIImage
	case config.RequestSchemaGemini:
		return config.ProtocolGemini
	default:
		return config.ProtocolOpenAIChat // 保留旧公共 helper 的 OpenAI 兼容默认
	}
}

// ProtocolSupportsPath 检查协议类型是否支持给定路径。
func ProtocolSupportsPath(protocol config.ProtocolType, path string) bool {
	if classifyClientPath(path) == clientPathUnknown {
		return false
	}
	return config.IsValidProtocolType(protocol) && ResolveProtocol(path) == protocol
}

// isOpenAIImageStylePath 判断路径是否为 OpenAI 图像 API 风格。
func isOpenAIImageStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return strings.HasSuffix(pl, "/images/generations") ||
		strings.HasSuffix(pl, "/images/edits") ||
		strings.HasSuffix(pl, "/images/variations")
}

// isGeminiStylePath 判断路径是否为 Gemini API 风格。
// Gemini 路径形如 /v1beta/models/{model}:generateContent。
func isGeminiStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return isGeminiModelMethodPath(pl)
}
