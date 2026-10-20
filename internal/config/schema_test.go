package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEndpointCapabilityCopiesCannotChangeRouting(t *testing.T) {
	for _, getter := range []func() EndpointTypeMeta{
		func() EndpointTypeMeta { meta, _ := EndpointTypeMetaOf(EndpointTypeOpenAI); return meta },
		func() EndpointTypeMeta {
			for _, meta := range AllEndpointTypeMetas() {
				if meta.Code == EndpointTypeOpenAI {
					return meta
				}
			}
			panic("missing OpenAI")
		},
	} {
		meta := getter()
		meta.Protocols[0] = ProtocolAnthropicMessages
		meta.RequestSchemas[0] = RequestSchemaAnthropic
		meta.Aliases[0] = "caller-mutated"
		fresh, _ := EndpointTypeMetaOf(EndpointTypeOpenAI)
		if fresh.Protocols[0] != ProtocolOpenAIChat || !EndpointTypeSupportsSchema(EndpointTypeOpenAI, RequestSchemaOpenAICompat) || EndpointTypeSupportsSchema(EndpointTypeOpenAI, RequestSchemaAnthropic) {
			t.Fatal("mutating public metadata changed routing or native protocol declaration")
		}
		if NormalizeEndpointType("wangsu_openai") != EndpointTypeOpenAI {
			t.Fatal("mutating public metadata changed alias normalization")
		}
	}
}

func TestPublicEnumOptionsDriveValidationAndRemainIsolated(t *testing.T) {
	tests := []struct {
		name    string
		options func() []EnumOption
		valid   func(string) bool
	}{
		{"usage", AllUsageAdapterOptions, IsValidUsageAdapter},
		{"image", AllImageOperationOptions, IsValidImageOperation},
		{"auth", AllAuthModeOptions, IsValidAuthMode},
		{"protocol", AllProtocolOptions, func(v string) bool { return IsValidProtocolType(ProtocolType(v)) }},
		{"schema", AllRequestSchemaOptions, func(v string) bool { return IsValidRequestSchema(RequestSchema(v)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := tt.options()
			seen := map[string]bool{}
			for _, option := range options {
				if seen[option.Value] || option.Label == "" || !tt.valid(option.Value) {
					t.Fatalf("invalid published option: %+v", option)
				}
				seen[option.Value] = true
			}
			if tt.valid("unsupported-value") {
				t.Fatal("validator accepted an unpublished value")
			}
			original := options[0]
			options[0] = EnumOption{Value: "caller-mutated"}
			if tt.options()[0] != original {
				t.Fatal("caller changed the enum declaration")
			}
		})
	}
	if IsValidUsageAdapter("") || !IsValidImageOperation("") || !IsValidAuthMode("") {
		t.Fatal("usage adapter requires a selection; other legacy defaults remain valid")
	}
}

func TestEndpointMetadataPublishesCapabilitiesWithoutRuntimeStrategies(t *testing.T) {
	for _, meta := range AllEndpointTypeMetas() {
		if len(meta.Protocols) == 0 || len(meta.RequestSchemas) == 0 || meta.EndpointPlaceholder == "" || meta.EndpointHint == "" {
			t.Fatalf("incomplete metadata for %s", meta.Code)
		}
		for _, schema := range meta.RequestSchemas {
			if !IsValidRequestSchema(schema) {
				t.Fatalf("unpublished schema %s", schema)
			}
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"protocols", "request_schemas", "supports_auth_mode", "supports_image_operation", "supports_dual_protocol", "supports_bearer_passthrough"} {
			if _, ok := fields[field]; !ok {
				t.Fatalf("%s omits %s", meta.Code, field)
			}
		}
		for _, field := range []string{"AuthStrategy", "PathStrategy", "ProbePath", "SanitizeAzureBody"} {
			if strings.Contains(string(encoded), field) {
				t.Fatalf("runtime strategy leaked into metadata: %s", field)
			}
		}
	}
}

func TestQuotaDimensionMetadataReferencesStoredFields(t *testing.T) {
	modelFields, coefficientFields := map[string]bool{}, map[string]bool{}
	for _, pair := range []struct {
		typ    reflect.Type
		fields map[string]bool
	}{{reflect.TypeOf(ModelGroup{}), modelFields}, {reflect.TypeOf(JobLevel{}), coefficientFields}} {
		for i := 0; i < pair.typ.NumField(); i++ {
			pair.fields[strings.Split(pair.typ.Field(i).Tag.Get("json"), ",")[0]] = true
		}
	}
	for _, dimension := range AllQuotaDimensionMetas() {
		if !modelFields[dimension.BaseField] || !coefficientFields[dimension.JobCoefficientField] {
			t.Fatalf("dimension references nonexistent field: %+v", dimension)
		}
	}
}
