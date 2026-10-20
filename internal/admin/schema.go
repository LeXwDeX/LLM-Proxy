package admin

import (
	"net/http"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

const adminSchemaVersion = 1

// AdminSchema is a public presentation contract, composed from domain-owned
// definitions. It contains no credentials, mutable settings or database data.
type AdminSchema struct {
	PricingServiceTiers      []config.EnumOption         `json:"pricing_service_tiers"`
	PricingSpeeds            []config.EnumOption         `json:"pricing_speeds"`
	Version                  int                         `json:"version"`
	EndpointTypes            []config.EndpointTypeMeta   `json:"endpoint_types"`
	Protocols                []config.EnumOption         `json:"protocols"`
	RequestSchemas           []config.EnumOption         `json:"request_schemas"`
	UsageAdapters            []config.EnumOption         `json:"usage_adapters"`
	ImageOperations          []config.EnumOption         `json:"image_operations"`
	AuthModes                []config.EnumOption         `json:"auth_modes"`
	PricingModes             []config.EnumOption         `json:"pricing_modes"`
	Roles                    []config.EnumOption         `json:"roles"`
	QuotaDimensions          []config.QuotaDimensionMeta `json:"quota_dimensions"`
	QuotaScopes              []config.EnumOption         `json:"quota_scopes"`
	QuotaSyncModes           []config.EnumOption         `json:"quota_sync_modes"`
	TargetPhases             []config.EnumOption         `json:"target_phases"`
	Defaults                 SchemaDefaults              `json:"defaults"`
	StreamUsageRequestSchema config.RequestSchema        `json:"stream_usage_request_schema"`
}

type SchemaDefaults struct {
	EndpointType                string `json:"endpoint_type"`
	UsageAdapter                string `json:"usage_adapter"`
	ImageOperation              string `json:"image_operation"`
	AuthMode                    string `json:"auth_mode"`
	PricingMode                 string `json:"pricing_mode"`
	PricingTimezone             string `json:"pricing_timezone"`
	Role                        string `json:"role"`
	OpenAIPrefix                string `json:"openai_prefix"`
	AnthropicPrefix             string `json:"anthropic_prefix"`
	SupportsResponses           bool   `json:"supports_responses"`
	RequestStreamUsageIfMissing bool   `json:"request_stream_usage_if_missing"`
}

func (h *Handler) handleSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, AdminSchema{
		Version:                  adminSchemaVersion,
		StreamUsageRequestSchema: config.StreamUsageRequestSchema,
		EndpointTypes:            config.AllEndpointTypeMetas(),
		Protocols:                config.AllProtocolOptions(),
		RequestSchemas:           config.AllRequestSchemaOptions(),
		UsageAdapters:            config.AllUsageAdapterOptions(),
		ImageOperations:          config.AllImageOperationOptions(),
		AuthModes:                config.AllAuthModeOptions(),
		PricingModes:             pricing.AllModeOptions(),
		PricingServiceTiers:      pricing.AllServiceTierOptions(),
		PricingSpeeds:            pricing.AllSpeedOptions(),
		Roles: []config.EnumOption{
			{Value: RoleAdmin, Label: "管理（可读写全部管理功能）"},
			{Value: RoleReadonly, Label: "只读（仅查看，不可修改）"},
		},
		QuotaDimensions: config.AllQuotaDimensionMetas(),
		QuotaScopes:     quota.AllScopeOptions(),
		QuotaSyncModes: []config.EnumOption{
			{Value: "clients", Label: "到指定客户端"},
			{Value: "project_group", Label: "到整个项目组"},
			{Value: "job_level", Label: "到整个职级"},
		},
		TargetPhases: proxy.AllTargetPhaseOptions(),
		Defaults: SchemaDefaults{
			EndpointType:                config.NormalizeEndpointType(""),
			UsageAdapter:                "", // No default: the administrator must select a policy.
			ImageOperation:              config.ImageOperationGenerations,
			AuthMode:                    config.AuthModeDefault,
			PricingMode:                 pricing.ModeFlat,
			PricingTimezone:             pricing.DefaultTimezone,
			Role:                        RoleAdmin,
			OpenAIPrefix:                config.DefaultDualProtocolOpenAIPrefix,
			AnthropicPrefix:             config.DefaultDualProtocolAnthropicPrefix,
			SupportsResponses:           config.DefaultDualProtocolSupportsResponses,
			RequestStreamUsageIfMissing: config.DefaultRequestStreamUsageIfMissing,
		},
	})
}
