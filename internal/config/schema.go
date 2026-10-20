package config

// EnumOption is the public value and display label for a validated constant.
type EnumOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type RequestSchema string

// Stream usage is opt-in and applies to actual Chat requests, not provider hosts or adapters.
const DefaultRequestStreamUsageIfMissing = false
const StreamUsageRequestSchema = RequestSchemaOpenAIChat

const (
	RequestSchemaOpenAICompat    RequestSchema = "openai-compatible"
	RequestSchemaOpenAIChat      RequestSchema = "openai-chat"
	RequestSchemaOpenAIResponses RequestSchema = "openai-responses"
	RequestSchemaAnthropic       RequestSchema = "anthropic"
	RequestSchemaGemini          RequestSchema = "gemini"
	RequestSchemaOpenAIImage     RequestSchema = "openai-image"
)

const (
	AuthModeDefault = ""
	AuthModeBearer  = "bearer"
)

const (
	QuotaDimensionDaily   = "daily"
	QuotaDimensionWeekly  = "weekly"
	QuotaDimensionMonthly = "monthly"
)

// Provider strategy IDs describe runtime bindings, and are omitted from the
// public admin schema. New strategies require an explicit proxy factory case.
type ProviderAuthStrategy string
type ProviderPathStrategy string

const (
	ProviderAuthBearer      ProviderAuthStrategy = "bearer"
	ProviderAuthAzure       ProviderAuthStrategy = "azure"
	ProviderAuthAnthropic   ProviderAuthStrategy = "anthropic"
	ProviderAuthGemini      ProviderAuthStrategy = "gemini"
	ProviderAuthDual        ProviderAuthStrategy = "dual"
	ProviderPathPassthrough ProviderPathStrategy = "passthrough"
	ProviderPathAzure       ProviderPathStrategy = "azure"
	ProviderPathTerminal    ProviderPathStrategy = "terminal"
	ProviderPathDual        ProviderPathStrategy = "dual"
)

var usageAdapterOptions = []EnumOption{
	{UsageAdapterStandard, "原厂规则"},
	{UsageAdapterQwen, "通义千问"}, {UsageAdapterGLM, "智谱 GLM"}, {UsageAdapterDeepSeek, "DeepSeek"},
	{UsageAdapterKimi, "Kimi"},
	{UsageAdapterOpenRouter, "OpenRouter"}, {UsageAdapterMinimax, "MiniMax"},
}
var imageOperationOptions = []EnumOption{
	{ImageOperationGenerations, "生成"}, {ImageOperationEdits, "编辑"}, {ImageOperationVariations, "变体"},
}
var authModeOptions = []EnumOption{{AuthModeDefault, "默认（x-api-key）"}, {AuthModeBearer, "Bearer"}}
var protocolOptions = []EnumOption{
	{string(ProtocolOpenAIChat), "OpenAI Chat"}, {string(ProtocolOpenAIResponses), "OpenAI Responses"},
	{string(ProtocolAnthropicMessages), "Anthropic Messages"}, {string(ProtocolGemini), "Gemini"}, {string(ProtocolOpenAIImage), "OpenAI Image"},
}
var requestSchemaOptions = []EnumOption{
	{string(RequestSchemaOpenAICompat), "OpenAI 兼容"}, {string(RequestSchemaOpenAIChat), "OpenAI Chat"},
	{string(RequestSchemaOpenAIResponses), "OpenAI Responses"}, {string(RequestSchemaAnthropic), "Anthropic Messages"},
	{string(RequestSchemaGemini), "Gemini"}, {string(RequestSchemaOpenAIImage), "OpenAI Image"},
}

func cloneEnumOptions(options []EnumOption) []EnumOption {
	return append([]EnumOption(nil), options...)
}
func enumContains(options []EnumOption, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}
func EnumValues(options []EnumOption) []string {
	values := make([]string, len(options))
	for i, option := range options {
		values[i] = option.Value
	}
	return values
}
func AllUsageAdapterOptions() []EnumOption   { return cloneEnumOptions(usageAdapterOptions) }
func AllImageOperationOptions() []EnumOption { return cloneEnumOptions(imageOperationOptions) }
func AllAuthModeOptions() []EnumOption       { return cloneEnumOptions(authModeOptions) }
func AllProtocolOptions() []EnumOption       { return cloneEnumOptions(protocolOptions) }
func AllRequestSchemaOptions() []EnumOption  { return cloneEnumOptions(requestSchemaOptions) }
func IsValidAuthMode(mode string) bool       { return enumContains(authModeOptions, mode) }
func IsValidImageOperation(operation string) bool {
	return operation == "" || enumContains(imageOperationOptions, operation)
}
func IsValidRequestSchema(schema RequestSchema) bool {
	return enumContains(requestSchemaOptions, string(schema))
}

type QuotaDimensionMeta struct {
	Value               string `json:"value"`
	Label               string `json:"label"`
	BaseField           string `json:"base_field"`
	JobCoefficientField string `json:"job_coefficient_field"`
}

var quotaDimensionMetas = []QuotaDimensionMeta{
	{QuotaDimensionDaily, "日", "daily_usd", "coefficient_daily"},
	{QuotaDimensionWeekly, "周", "weekly_usd", "coefficient_weekly"},
	{QuotaDimensionMonthly, "月", "monthly_usd", "coefficient_monthly"},
}

func AllQuotaDimensionMetas() []QuotaDimensionMeta {
	return append([]QuotaDimensionMeta(nil), quotaDimensionMetas...)
}
