package config

// ProtocolType 标识底层请求/响应协议形态。
// 协议由请求结构和响应结构锚定，不由 provider 名字决定。
type ProtocolType string

const (
	ProtocolOpenAIChat        ProtocolType = "openai_chat"
	ProtocolOpenAIResponses   ProtocolType = "openai_responses"
	ProtocolAnthropicMessages ProtocolType = "anthropic_messages"
	ProtocolGemini            ProtocolType = "gemini"
	ProtocolOpenAIImage       ProtocolType = "openai_image"
)

// AllProtocolTypes returns a copy of all valid protocol types.
func AllProtocolTypes() []ProtocolType {
	out := make([]ProtocolType, len(protocolOptions))
	for i, option := range protocolOptions {
		out[i] = ProtocolType(option.Value)
	}
	return out
}

// IsValidProtocolType reports whether p is a known protocol type.
func IsValidProtocolType(p ProtocolType) bool {
	return enumContains(protocolOptions, string(p))
}
