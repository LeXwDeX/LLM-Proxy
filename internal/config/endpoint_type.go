package config

import "strings"

// Canonical dual_protocol targets have no vendor-specific path defaults.
// Legacy aliases retain their compatibility defaults in NormalizeTargetForCompatibility.
const (
	DefaultDualProtocolOpenAIPrefix      = ""
	DefaultDualProtocolAnthropicPrefix   = ""
	DefaultDualProtocolSupportsResponses = false
)

// EndpointTypeMeta 是 endpoint_type 的单一信息源（Single Source of Truth）。
//
// 凡是涉及 endpoint_type 的「常量化业务数据」（机器名、UI 显示名、缩写、
// 徽章配色、是否需要 resource_path_prefix 等），全部在此声明，
// 由 admin API（GET /admin/data/endpoint-types）统一暴露给前端 UI 与下游消费方。
//
// 严禁在 admin/ui/index.html、handler、service 等其他层重复硬编码同一份元数据。
// 原生协议描述与兼容请求能力分开声明；兼容请求不意味着协议转换。
type EndpointTypeMeta struct {
	Code                       string          `json:"code"`                          // 机器名
	DisplayName                string          `json:"display_name"`                  // UI 长名（下拉、徽章）
	ShortLabel                 string          `json:"short_label"`                   // UI 缩写（客户端列表等紧凑场景）
	BadgeBackground            string          `json:"badge_background"`              // CSS 背景色
	BadgeForeground            string          `json:"badge_foreground"`              // CSS 前景色
	RequiresResourcePathPrefix bool            `json:"requires_resource_path_prefix"` // deprecated: 不再使用，保留避免 JSON 反序列化报错
	SupportsBearerPassthrough  bool            `json:"supports_bearer_passthrough"`   // 是否支持 allow_bearer_passthrough（仅 azure_openai）
	Protocols                  []ProtocolType  `json:"protocols"`                     // 原生协议，兼容请求能力见 RequestSchemas
	RequestSchemas             []RequestSchema `json:"request_schemas"`
	SupportsAuthMode           bool            `json:"supports_auth_mode"`
	SupportsImageOperation     bool            `json:"supports_image_operation"`
	SupportsDualProtocol       bool            `json:"supports_dual_protocol"`
	// SupportsPreferWebSocket: the Responses upstream WebSocket preference
	// (target prefer_websocket) applies to this type.
	SupportsPreferWebSocket bool                 `json:"supports_prefer_websocket"`
	EndpointPlaceholder     string               `json:"endpoint_placeholder"`
	EndpointHint            string               `json:"endpoint_hint"`
	Provider                string               `json:"-"`
	AuthStrategy            ProviderAuthStrategy `json:"-"`
	PathStrategy            ProviderPathStrategy `json:"-"`
	PreserveMultipart       bool                 `json:"-"`
	ProbePath               string               `json:"-"`
	ProbeUsesMappedPath     bool                 `json:"-"`
	SanitizeAzureBody       bool                 `json:"-"`
	Aliases                 []string             `json:"aliases,omitempty"` // 旧版/厂商变体名，NormalizeEndpointType 会归一化到 Code
}

// endpointTypes 是受支持的 endpoint_type 全集。
//
// 新增 endpoint_type 时仅需在此追加一条；下游代码（IsValidEndpointType、
// admin API、UI 下拉等）会自动派生，无需逐处修改。
//
// Alias 字段声明旧版/厂商变体名，NormalizeEndpointType 会将它们归一化到 Code。
var endpointTypes = []EndpointTypeMeta{
	{
		Provider: "azure", AuthStrategy: ProviderAuthAzure, PathStrategy: ProviderPathAzure,
		Protocols:         []ProtocolType{ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolOpenAIImage},
		RequestSchemas:    []RequestSchema{RequestSchemaOpenAICompat, RequestSchemaOpenAIChat, RequestSchemaOpenAIResponses, RequestSchemaOpenAIImage},
		PreserveMultipart: true, SanitizeAzureBody: true,
		EndpointPlaceholder: "https://<resource>.openai.azure.com", EndpointHint: "Azure OpenAI 资源根 URL。",
		Code: EndpointTypeAzureOpenAI, DisplayName: "Azure OpenAI", ShortLabel: "Azure",
		BadgeBackground: "#e0f2fe", BadgeForeground: "#0284c7",
		RequiresResourcePathPrefix: false, SupportsBearerPassthrough: true,
	},
	{
		ProbePath: "/v1/models", Provider: "openai", AuthStrategy: ProviderAuthBearer, PathStrategy: ProviderPathPassthrough,
		Protocols:           []ProtocolType{ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolOpenAIImage},
		RequestSchemas:      []RequestSchema{RequestSchemaOpenAICompat, RequestSchemaOpenAIChat, RequestSchemaOpenAIResponses, RequestSchemaOpenAIImage},
		EndpointPlaceholder: "https://api.openai.com", EndpointHint: "OpenAI 官方 API 根 URL。",
		SupportsPreferWebSocket: true,
		Code:                    EndpointTypeOpenAI, DisplayName: "OpenAI", ShortLabel: "OpenAI",
		BadgeBackground: "#dbeafe", BadgeForeground: "#3b82f6",
		Aliases: []string{"wangsu_openai"},
	},
	{
		ProbePath: "/v1/models", Provider: "anthropic", AuthStrategy: ProviderAuthAnthropic, PathStrategy: ProviderPathPassthrough,
		Protocols:           []ProtocolType{ProtocolAnthropicMessages},
		RequestSchemas:      []RequestSchema{RequestSchemaAnthropic},
		SupportsAuthMode:    true,
		EndpointPlaceholder: "https://api.anthropic.com", EndpointHint: "Anthropic 官方 API 根 URL。",
		Code: EndpointTypeClaude, DisplayName: "Claude", ShortLabel: "Claude",
		BadgeBackground: "#fce7f3", BadgeForeground: "#db2777",
		Aliases: []string{"wangsu_claude"},
	},
	{
		ProbePath: "/v1beta/models", Provider: "google", AuthStrategy: ProviderAuthGemini, PathStrategy: ProviderPathPassthrough,
		Protocols:           []ProtocolType{ProtocolGemini},
		RequestSchemas:      []RequestSchema{RequestSchemaGemini, RequestSchemaOpenAICompat, RequestSchemaOpenAIChat, RequestSchemaOpenAIImage},
		EndpointPlaceholder: "https://generativelanguage.googleapis.com", EndpointHint: "Google Gemini 官方 API 根 URL。",
		Code: EndpointTypeGemini, DisplayName: "Gemini", ShortLabel: "Gemini",
		BadgeBackground: "#dcfce7", BadgeForeground: "#16a34a",
		Aliases: []string{"wangsu_gemini"},
	},
	{
		ProbePath: "/v1/models", Provider: "openai_image", AuthStrategy: ProviderAuthBearer, PathStrategy: ProviderPathTerminal,
		Protocols:              []ProtocolType{ProtocolOpenAIImage},
		RequestSchemas:         []RequestSchema{RequestSchemaOpenAIImage},
		SupportsImageOperation: true, PreserveMultipart: true,
		EndpointPlaceholder: "https://<网关>/openai-image", EndpointHint: "图像终态 URL（请求 path 会被整体覆盖）；根 URL 或版本路径会透传图像请求 path。",
		Code: EndpointTypeOpenAIImage, DisplayName: "OpenAI Image", ShortLabel: "Image",
		BadgeBackground: "#fef3c7", BadgeForeground: "#d97706",
		Aliases: []string{"wangsu_openai_image", "wangsu_openai_image_edit"},
	},
	{
		ProbePath: "/v1/models", ProbeUsesMappedPath: true, Provider: "dual_protocol", AuthStrategy: ProviderAuthDual, PathStrategy: ProviderPathDual,
		Protocols:            []ProtocolType{ProtocolOpenAIChat, ProtocolAnthropicMessages},
		RequestSchemas:       []RequestSchema{RequestSchemaOpenAICompat, RequestSchemaOpenAIChat, RequestSchemaOpenAIResponses, RequestSchemaAnthropic, RequestSchemaOpenAIImage},
		SupportsDualProtocol: true,
		EndpointPlaceholder:  "https://api.example.com", EndpointHint: "双协议端点根 URL。按客户端路径选择 OpenAI 或 Anthropic 前缀，保留对应请求协议。",
		Code: EndpointTypeDualProtocol, DisplayName: "双协议兼容", ShortLabel: "双协议",
		BadgeBackground: "#eef2ff", BadgeForeground: "#4f46e5",
		Aliases: []string{"deepseek", "bailian", "bailian_api"},
	},
}

// AllEndpointTypeMetas returns a copy of all registered endpoint type metadata
// in declaration order. UI / admin API consumers should use this as the
// single source of truth for rendering dropdowns and badges.
func AllEndpointTypeMetas() []EndpointTypeMeta {
	out := make([]EndpointTypeMeta, len(endpointTypes))
	for i, meta := range endpointTypes {
		out[i] = cloneEndpointTypeMeta(meta)
	}
	return out
}

// EndpointTypeMetaOf returns the metadata for a given endpoint type code.
// The lookup is case-insensitive and trims whitespace, matching
// NormalizeEndpointType semantics.
func EndpointTypeMetaOf(code string) (EndpointTypeMeta, bool) {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, m := range endpointTypes {
		if m.Code == code {
			return cloneEndpointTypeMeta(m), true
		}
	}
	return EndpointTypeMeta{}, false
}

// endpointTypeFromAlias resolves a legacy/vendor alias to its canonical endpoint type code.
// Returns the canonical code and true if the alias is recognized.
// This is the data-driven replacement for the former hardcoded switch in NormalizeEndpointType.
func endpointTypeFromAlias(alias string) (string, bool) {
	for _, m := range endpointTypes {
		for _, a := range m.Aliases {
			if a == alias {
				return m.Code, true
			}
		}
	}
	return "", false
}

func cloneEndpointTypeMeta(meta EndpointTypeMeta) EndpointTypeMeta {
	meta.Aliases = append([]string(nil), meta.Aliases...)
	meta.Protocols = append([]ProtocolType(nil), meta.Protocols...)
	meta.RequestSchemas = append([]RequestSchema(nil), meta.RequestSchemas...)
	return meta
}

// EndpointTypeSupportsSchema reads the immutable declaration without allocating
// copies on the forwarding path. Unknown types retain the historical OpenAI
// compatibility fallback; configuration validation rejects them separately.
func EndpointTypeSupportsSchema(code string, schema RequestSchema) bool {
	for _, meta := range endpointTypes {
		if meta.Code != code {
			continue
		}
		for _, candidate := range meta.RequestSchemas {
			if schema == candidate {
				return true
			}
		}
		return false
	}
	return schema != RequestSchemaAnthropic && schema != RequestSchemaGemini
}
