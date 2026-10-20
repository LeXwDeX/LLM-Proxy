package proxy

import (
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestOfficialProtocolUsageParsers(t *testing.T) {
	tests := []struct {
		name   string
		parser usageParser
		body   string
		want   usageTokens
	}{
		{
			name:   "openai o-series uses official cache fields",
			parser: usageParser{endpointType: config.EndpointTypeOpenAI, model: "o3"},
			body:   `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":50}}}`,
			want: usageTokens{InputTokens: 20, OutputTokens: 20, CachedTokens: 30, CacheCreationTokens: 50,
				fields: usageFieldInput | usageFieldOutput | usageFieldCached | usageFieldCacheCreation,
				status: usageStatusComplete, adapter: usageAdapterStandardOpenAI},
		},
		{
			name:   "anthropic input excludes explicit cache fields",
			parser: usageParser{endpointType: config.EndpointTypeClaude, model: "claude-opus-5"},
			body:   `{"usage":{"input_tokens":20,"output_tokens":10,"cache_read_input_tokens":80,"cache_creation_input_tokens":40}}`,
			want: usageTokens{InputTokens: 20, OutputTokens: 10, CachedTokens: 80, CacheCreationTokens: 40,
				fields: usageFieldInput | usageFieldOutput | usageFieldCached | usageFieldCacheCreation, status: usageStatusComplete, adapter: usageAdapterStandardAnthropic},
		},
		{
			name:   "gemini thoughts are output tokens",
			parser: usageParser{endpointType: config.EndpointTypeGemini, model: "gemini-2.5-pro"},
			body:   `{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":30,"candidatesTokenCount":20,"thoughtsTokenCount":15}}`,
			want: usageTokens{InputTokens: 70, OutputTokens: 35, CachedTokens: 30, ReasoningTokens: 15, costIncomplete: true, diagnostic: "cache_storage_not_in_usage",
				fields: usageFieldInput | usageFieldOutput | usageFieldCached | usageFieldReasoning, status: usageStatusComplete, adapter: usageAdapterStandardGemini},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, found := tt.parser.extractTokens("application/json", []byte(tt.body))
			got.pricingContext = nil // Token-only contract; pricing evidence is covered separately.
			if !found || got != tt.want {
				t.Fatalf("got %+v found=%t, want %+v", got, found, tt.want)
			}
		})
	}
}

func TestQwenUsageAdapterFiveDimensions(t *testing.T) {
	endpoint, _ := url.Parse("https://dashscope.aliyuncs.com/compatible-mode/v1")
	base := usageParser{
		endpointType: config.EndpointTypeDualProtocol,
		protocol:     config.ProtocolOpenAIChat,
		model:        "qwen3.8-max",
		endpoint:     endpoint,
		usageAdapter: config.UsageAdapterQwen,
	}

	t.Run("implicit cache read", func(t *testing.T) {
		parser := base
		parser.requestBody = []byte(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`)
		got, _, ok := parser.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`))
		if !ok {
			t.Fatal("usage not found")
		}
		want := usageTokens{InputTokens: 70, OutputTokens: 20, CachedTokens: 30,
			fields: usageFieldInput | usageFieldOutput | usageFieldCached | usageFieldExplicitCacheRead, status: usageStatusComplete, adapter: usageAdapterQwen}
		got.pricingContext = nil // Token-only contract; pricing evidence is covered separately.
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})

	t.Run("explicit create and read", func(t *testing.T) {
		parser := base
		parser.requestBody = []byte(`{"model":"qwen3.8-max","messages":[{"role":"system","content":[{"type":"text","text":"prefix","cache_control":{"type":"ephemeral"}}]}]}`)
		got, _, ok := parser.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}}`))
		if !ok {
			t.Fatal("usage not found")
		}
		want := usageTokens{InputTokens: 20, OutputTokens: 20, CachedTokens: 30, CacheCreationTokens: 50, ExplicitCacheReadTokens: 30,
			fields: usageAllFields, status: usageStatusComplete, adapter: usageAdapterQwen}
		got.pricingContext = nil // Token-only contract; pricing evidence is covered separately.
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})

	t.Run("optional creation remains unobserved without invalidating standard totals", func(t *testing.T) {
		parser := base
		parser.requestBody = []byte(`{"messages":[{"content":[{"cache_control":{"type":"ephemeral"}}]}]}`)
		got, _, ok := parser.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`))
		if !ok {
			t.Fatal("usage not found")
		}
		if got.CacheCreationTokens != 0 || got.fields&usageFieldCacheCreation != 0 || got.status != usageStatusComplete {
			t.Fatalf("missing field was presented as zero/complete: %+v", got)
		}
	})
}

func TestVendorUsageAdapterIdentityGuards(t *testing.T) {
	tests := []struct {
		name, endpoint, configured, model, want string
	}{
		{"empty on official host stays standard", "https://api.deepseek.com", "", "deepseek-chat", usageAdapterStandardOpenAI},
		{"legacy auto is invalid", "https://dashscope.aliyuncs.com", "auto", "qwen3.8-max", usageAdapterStandardOpenAI},
		{"standard on GLM host", "https://open.bigmodel.cn", "standard", "glm-5", usageAdapterStandardOpenAI},
		{"standard on workspace host", "https://workspace.cn-beijing.maas.aliyuncs.com", "standard", "qwen3.8-max", usageAdapterStandardOpenAI},
		{"explicit qwen gateway opt in", "https://models.internal.example/v1", config.UsageAdapterQwen, "qwen3.8-max", usageAdapterQwen},
		{"explicit qwen applies to any mapped model", "https://models.internal.example/v1", config.UsageAdapterQwen, "local-qwen-alias", usageAdapterQwen},
		{"explicit deepseek applies to any mapped model", "https://reseller.example/v1", config.UsageAdapterDeepSeek, "unlisted-model", usageAdapterDeepSeek},
		{"explicit glm applies to any mapped model", "https://reseller.example/v1", config.UsageAdapterGLM, "unlisted-model", usageAdapterGLM},
		{"explicit kimi applies to any mapped model", "https://reseller.example/v1", config.UsageAdapterKimi, "unlisted-model", usageAdapterKimi},
		{"explicit adapter is case insensitive", "https://reseller.example/v1", " DeepSeek ", "x", usageAdapterDeepSeek},
		{"unknown explicit adapter is standard", "https://api.deepseek.com", "no-such", "deepseek-chat", usageAdapterStandardOpenAI},
		{"standard disables patches", "https://dashscope.aliyuncs.com/compatible-mode/v1", config.UsageAdapterStandard, "qwen3.8-max", usageAdapterStandardOpenAI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, _ := url.Parse(tt.endpoint)
			got, _, found := (usageParser{usageAdapter: tt.configured, endpoint: ep, model: tt.model, protocol: config.ProtocolOpenAIChat}).extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			resolved, matched := resolveUsageAdapter(tt.configured, config.ProtocolOpenAIChat)
			if !found || got.adapter != tt.want || resolved != tt.want || matched != (tt.want != usageAdapterStandardOpenAI) {
				t.Fatalf("got %+v resolved=%q matched=%v want %q", got, resolved, matched, tt.want)
			}
		})
	}
	for _, op := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages, config.ProtocolGemini} {
		for _, configured := range []string{"standard", "", "auto", "unknown", "qwen", "glm", "deepseek", "kimi"} {
			t.Run(string(op)+"/"+configured, func(t *testing.T) {
				want := usageAdapterStandardOpenAI
				if op == config.ProtocolAnthropicMessages {
					want = usageAdapterStandardAnthropic
				}
				if op == config.ProtocolGemini {
					want = usageAdapterStandardGemini
				}
				patched := op != config.ProtocolGemini && (configured == "qwen" || configured == "glm" || configured == "deepseek" || configured == "kimi")
				if patched {
					want = configured
				}
				if got, matched := resolveUsageAdapter(configured, op); got != want || matched != patched {
					t.Fatalf("adapter=%q matched=%v want=%q patched=%v", got, matched, want, patched)
				}
			})
		}
	}
}

func TestDeepSeekNativeFieldsRequireMatchedAdapter(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":68011,"completion_tokens":449,"prompt_cache_hit_tokens":66816,"prompt_cache_miss_tokens":1195}}`)
	for _, tt := range []struct {
		name     string
		endpoint string
		adapter  string
		wantIn   int64
		wantHit  int64
	}{
		{"official standard", "https://api.deepseek.com", "standard", 68011, 0},
		{"official explicit", "https://api.deepseek.com", "deepseek", 1195, 66816},
		{"self hosted explicit", "https://models.internal.example", "deepseek", 1195, 66816},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ep, _ := url.Parse(tt.endpoint)
			parser := usageParser{endpointType: config.EndpointTypeDualProtocol, protocol: config.ProtocolOpenAIChat, model: "unlisted-model", endpoint: ep, usageAdapter: tt.adapter}
			got, _, ok := parser.extractTokens("application/json", body)
			if !ok || got.InputTokens != tt.wantIn || got.CachedTokens != tt.wantHit {
				t.Fatalf("got %+v found=%t", got, ok)
			}
		})
	}
}
