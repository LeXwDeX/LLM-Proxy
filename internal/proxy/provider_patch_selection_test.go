package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestDeepSeekAnthropicAliasRiskUsesSelectedVendor(t *testing.T) {
	for _, host := range []string{"api.deepseek.com", "relay.example"} {
		for _, adapter := range []string{"standard", "qwen", "glm", "deepseek", "kimi"} {
			for _, model := range []string{"claude-unlisted", "unlisted-model"} {
				t.Run(host+"/"+adapter+"/"+model, func(t *testing.T) {
					ep, _ := url.Parse("https://" + host)
					p := usageParser{protocol: config.ProtocolAnthropicMessages, usageAdapter: adapter, model: model, endpoint: ep}
					got, _, found := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":20,"output_tokens":10}}`))
					wantRisk := adapter == "deepseek" && strings.HasPrefix(model, "claude-")
					if !found || got.costIncomplete != wantRisk || (got.diagnostic == "upstream_model_alias_requires_price_mapping") != wantRisk {
						t.Fatalf("alias risk mismatch: %+v found=%v want=%v", got, found, wantRisk)
					}
				})
			}
		}
	}
}

func TestQwenCacheClassificationIgnoresModel(t *testing.T) {
	for _, model := range []string{"qwen3.8-max", "unlisted-model"} {
		for _, tc := range []struct {
			name, body, response string
			op                   config.ProtocolType
			headers              http.Header
			explicit             int64
			missing              bool
		}{
			{"chat explicit", `{"messages":[{"content":[{"cache_control":{"type":"ephemeral"}}]}]}`, `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}`, config.ProtocolOpenAIChat, nil, 30, false},
			{"chat implicit", `{"messages":[]}`, `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}`, config.ProtocolOpenAIChat, nil, 0, false},
			{"chat missing context", "", `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}`, config.ProtocolOpenAIChat, nil, 0, true},
			{"responses explicit", `{"input":"hi"}`, `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":30},"x_details":[{"input_tokens_details":{"cache_creation_input_tokens":50}}]}`, config.ProtocolOpenAIResponses, http.Header{"X-Dashscope-Session-Cache": {"enable"}}, 30, false},
			{"responses implicit", `{"input":"hi"}`, `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":30},"x_details":[{"input_tokens_details":{"cache_creation_input_tokens":50}}]}`, config.ProtocolOpenAIResponses, http.Header{}, 0, false},
			{"responses missing context", "", `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":30},"x_details":[{"input_tokens_details":{"cache_creation_input_tokens":50}}]}`, config.ProtocolOpenAIResponses, nil, 0, true},
		} {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				p := usageParser{protocol: tc.op, usageAdapter: "qwen", model: model, requestBody: []byte(tc.body), requestHeaders: tc.headers}
				got, _, found := p.extractTokens("application/json", []byte(`{"usage":`+tc.response+`}`))
				if !found || got.InputTokens != 20 || got.CachedTokens != 30 || got.CacheCreationTokens != 50 || got.ExplicitCacheReadTokens != tc.explicit || got.ExplicitCacheReadTokens > got.CachedTokens {
					t.Fatalf("cache partition mismatch: %+v", got)
				}
				if got.costIncomplete != tc.missing || (got.diagnostic == "missing_cache_request_context") != tc.missing {
					t.Fatalf("context mismatch: %+v", got)
				}
			})
		}
	}
}

func TestRuntimeUsageAdapterEmptyCompatibilityAndInvalidSkip(t *testing.T) {
	for _, adapter := range []string{"", "  ", "standard", "qwen", "kimi", "auto", "unknown"} {
		t.Run("adapter="+adapter, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			parsed, order, err := buildTargetStates([]config.Target{{Name: "test", Endpoint: "https://dashscope.aliyuncs.com", EndpointType: "openai", APIKey: "test-only", UsageAdapter: adapter}}, logger)
			if err != nil {
				t.Fatal(err)
			}
			if adapter == "auto" || adapter == "unknown" {
				if len(parsed) != 0 || len(order) != 0 || !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "skipping target with unsupported provider settings") {
					t.Fatalf("invalid target not skipped/logged: %v %s", parsed, logs.String())
				}
				return
			}
			want := strings.TrimSpace(adapter)
			if want == "" {
				want = "standard"
			}
			if len(order) != 1 || parsed["test"].Target().UsageAdapter != want {
				t.Fatalf("runtime adapter: %v want %s", parsed, want)
			}
		})
	}
}

func TestUsageAdapterHotChangePreservesKeyLifecycle(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "UpdateTargets", true: "ApplyConfig"}[apply], func(t *testing.T) {
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "test", Endpoint: "https://relay.example", EndpointType: "openai", UsageAdapter: "standard", APIKeys: []string{"k0", "k1", "k2"}}}}
			svc, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			old := svc.targetsByName["test"]
			reset := time.Now().Add(time.Hour)
			old.keyPool.markExhaustedWithCooldown(0, glmRateLimitQuota, reset, "quota")
			old.keyPool.blockKey(1)
			cfg.Targets[0].UsageAdapter = "qwen"
			if apply {
				err = svc.ApplyConfig(cfg)
			} else {
				err = svc.UpdateTargets(cfg.Targets)
			}
			if err != nil {
				t.Fatal(err)
			}
			next := svc.targetsByName["test"]
			next.keyPool.mu.Lock()
			cooling, blocked := next.keyPool.entries[0], next.keyPool.entries[1].blocked
			next.keyPool.mu.Unlock()
			if next.keyPool != old.keyPool || !cooling.exhausted || !cooling.cooldownEnd.Equal(reset) || !blocked {
				t.Fatal("adapter change reset key lifecycle")
			}
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			body := []byte(`{"messages":[{"content":[{"cache_control":{"type":"ephemeral"}}]}]}`)
			p := newTargetUsageParser(r, next.Target(), "unlisted-model", body, nil)
			got, _, found := p.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}}`))
			if !found || got.adapter != "qwen" || got.ExplicitCacheReadTokens != 30 {
				t.Fatalf("new request ignored new adapter: %+v", got)
			}
			if exhausted, _ := targetKeyExhausted(next.Target(), config.ProtocolGemini, 429, []byte(`{"error":{"code":"Arrearage"}}`)); !exhausted {
				t.Fatal("new error policy ignored adapter")
			}
		})
	}
}

func TestSelectedGLMErrorPolicyOnGeminiProtocol(t *testing.T) {
	for _, adapter := range []string{"standard", "qwen", " GLM ", "deepseek"} {
		t.Run(adapter, func(t *testing.T) {
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "test", Endpoint: "https://relay.example", EndpointType: "gemini", UsageAdapter: adapter, APIKeys: []string{"k0", "k1"}}}}
			svc, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			state := svc.targetsByName["test"]
			body := []byte(`{"error":{"code":"1308","message":"quota exhausted"}}`)
			if !svc.observeResponsesKeyFailure(state, state.Target(), "unlisted-model", config.ProtocolGemini, 0, 429, body) {
				t.Fatal("429 did not update key state")
			}
			want := "rate_limited"
			if adapter == " GLM " {
				want = glmReasonForCode("1308")
			}
			if got := state.keyPool.exhaustReasonAt(0); got != want {
				t.Fatalf("Gemini error policy reason=%q want=%q", got, want)
			}
		})
	}
}
