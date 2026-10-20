package proxy

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestKimiErrorTypeIsolation(t *testing.T) {
	for _, tc := range []struct {
		status       int
		body, reason string
	}{
		{429, `{"error":{"type":"exceeded_current_quota_error"}}`, "quota_exceeded"},
		{401, `{"error":{"type":"invalid_authentication_error"}}`, "invalid_token"},
		{401, `{"error":{"type":"incorrect_api_key_error"}}`, "invalid_token"},
		{429, `{"error":{"type":"rate_limit_reached_error"}}`, ""},
		{429, `{"error":{"type":"engine_overloaded_error"}}`, ""},
		{403, `{"error":{"type":"permission_denied_error"}}`, ""},
		{500, `{"error":{"type":"exceeded_current_quota_error"}}`, ""},
		{401, `{"error":{"type":"exceeded_current_quota_error"}}`, ""},
		{429, `{"error":{"type":"incorrect_api_key_error"}}`, ""},
		{429, `{"error":{"code":"exceeded_current_quota_error","message":"exceeded_current_quota_error"}}`, ""},
		{401, `{"error":{"code":"incorrect_api_key_error","message":"invalid api key"}}`, ""},
		{429, `{"content":"exceeded_current_quota_error"}`, ""},
		{429, `{"error":{"type":"unknown"}}`, ""},
		{429, `{"error":{"type":"exceeded_current_quota_error "}}`, ""},
		{403, `{"error":{"type":"exceeded_current_quota_error"}}`, ""},
		{500, `{"error":{"type":"invalid_authentication_error"}}`, ""},
	} {
		for _, adapter := range []string{"kimi", "standard", "glm", "qwen", "deepseek"} {
			for _, op := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolGemini, config.ProtocolAnthropicMessages} {
				want := ""
				if adapter == "kimi" {
					want = tc.reason
				}
				// Other adapters retain generic invalid-key message handling.
				if adapter != "kimi" && tc.status == 401 && strings.Contains(tc.body, "invalid api key") {
					want = "invalid_token"
				}
				got, reason := targetKeyExhausted(&Target{UsageAdapter: adapter}, op, tc.status, []byte(tc.body))
				if got != (want != "") || reason != want {
					t.Errorf("adapter=%s op=%s status=%d body=%s got=%v,%q want=%q", adapter, op, tc.status, tc.body, got, reason, want)
				}
			}
		}
	}
	if exhausted, reason := targetKeyExhausted(&Target{UsageAdapter: "kimi"}, config.ProtocolOpenAIChat, 402, []byte(`{"error":{"type":"unknown"}}`)); !exhausted || reason != "billing_error" {
		t.Fatalf("common 402 changed: %v %q", exhausted, reason)
	}
	if exhausted, reason := targetKeyExhausted(&Target{UsageAdapter: "kimi"}, config.ProtocolOpenAIChat, 429, []byte(`{"error":{"code":"credit_balance_exhausted","type":"unknown"}}`)); exhausted || reason != "" {
		t.Fatalf("Kimi used generic protocol quota code: %v %q", exhausted, reason)
	}
}

func TestKimiCacheAliases(t *testing.T) {
	for _, tc := range []struct {
		name, extra, status string
		input, cached       int64
	}{
		{"fallback", `,"cached_tokens":40`, "complete", 60, 40},
		{"equal", `,"cached_tokens":40,"prompt_tokens_details":{"cached_tokens":40}`, "complete", 60, 40},
		{"nested", `,"prompt_tokens_details":{"cached_tokens":40}`, "complete", 60, 40},
		{"conflict", `,"cached_tokens":40,"prompt_tokens_details":{"cached_tokens":30}`, "conflict", 60, 40},
		{"zero", `,"cached_tokens":0`, "complete", 100, 0},
		{"both_zero", `,"cached_tokens":0,"prompt_tokens_details":{"cached_tokens":0}`, "complete", 100, 0},
		{"exceed", `,"cached_tokens":101`, "conflict", 0, 101},
		{"invalid", `,"cached_tokens":-1`, "conflict", 100, 0},
		{"wrong_type", `,"cached_tokens":"40"`, "conflict", 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7` + tc.extra + `}}`)
			p := usageParser{usageAdapter: "kimi", model: "unfamiliar-UPSTREAM", protocol: config.ProtocolOpenAIChat}
			p.endpoint, _ = url.Parse("https://fake.invalid")
			got, _, ok := p.extractTokens("application/json", body)
			if !ok || got.adapter != "kimi" || got.status != tc.status || got.InputTokens != tc.input || got.CachedTokens != tc.cached || got.costIncomplete != (tc.status == "conflict") {
				t.Fatalf("got %+v", got)
			}
			stream, _, ok := p.extractSSE([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: " + string(body) + "\n\ndata: [DONE]\n\n"))
			if !ok || stream.InputTokens != got.InputTokens || stream.CachedTokens != got.CachedTokens || stream.status != got.status {
				t.Fatalf("stream %+v differs from JSON %+v", stream, got)
			}
		})
	}
	p := usageParser{usageAdapter: "standard", protocol: config.ProtocolOpenAIChat}
	got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"cached_tokens":40}}`))
	if got.InputTokens != 100 || got.CachedTokens != 0 {
		t.Fatalf("standard opted in: %+v", got)
	}
}

func TestKimiCacheWriteTTLAndCost(t *testing.T) {
	five, hour := 5.0, 10.0
	rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 3, CacheReadPer1MToken: 1, CacheWritePer1MToken: 20, CacheWrite5mPer1MToken: &five, CacheWrite1hPer1MToken: &hour}
	for _, tc := range []struct {
		name, request string
		five, hour    int64
		incomplete    bool
	}{
		{"default", `{}`, 20, 0, false},
		{"empty_options", `{"prompt_cache_options":{}}`, 20, 0, false},
		{"5m", `{"prompt_cache_options":{"ttl":"5m"}}`, 20, 0, false},
		{"1h", `{"prompt_cache_options":{"ttl":"1h"}}`, 0, 20, false},
		{"missing", ``, 0, 0, true},
		{"invalid_json", `{`, 0, 0, true},
		{"null_request", `null`, 0, 0, true},
		{"array_request", `[]`, 0, 0, true},
		{"invalid_options", `{"prompt_cache_options":null}`, 0, 0, true},
		{"invalid_ttl", `{"prompt_cache_options":{"ttl":"2h"}}`, 0, 0, true},
		{"null_ttl", `{"prompt_cache_options":{"ttl":null}}`, 0, 0, true},
		{"numeric_ttl", `{"prompt_cache_options":{"ttl":3600}}`, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := usageParser{usageAdapter: "kimi", protocol: config.ProtocolOpenAIChat, requestBody: []byte(tc.request)}
			got, _, ok := p.extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"cached_tokens":40,"prompt_tokens_details":{"cache_write_tokens":20}}}`))
			if !ok || got.InputTokens != 40 || got.CachedTokens != 40 || got.CacheCreationTokens != 20 || got.CacheCreation5mTokens != tc.five || got.CacheCreation1hTokens != tc.hour || got.costIncomplete != tc.incomplete {
				t.Fatalf("got %+v", got)
			}
			if tc.incomplete && got.diagnostic == "" {
				t.Fatal("missing uncertainty diagnostic")
			}
			streamBody := "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"cached_tokens\":40,\"prompt_tokens_details\":{\"cache_write_tokens\":20}}}\n\ndata: [DONE]\n\n"
			stream, _, _ := p.extractSSE([]byte(streamBody))
			if stream.InputTokens != got.InputTokens || stream.CacheCreation5mTokens != got.CacheCreation5mTokens || stream.CacheCreation1hTokens != got.CacheCreation1hTokens || stream.costIncomplete != got.costIncomplete {
				t.Fatalf("SSE TTL differs: %+v JSON %+v", stream, got)
			}
			cost := rates.CostFor(got.InputTokens, got.OutputTokens, got.CachedTokens, got.CacheCreationTokens, got.ExplicitCacheReadTokens, got.CacheCreation5mTokens, got.CacheCreation1hTokens)
			want := (80.0 + 21 + 40 + float64(20-tc.five-tc.hour)*20 + float64(tc.five)*5 + float64(tc.hour)*10) / 1e6
			if math.Abs(cost-want) > 1e-12 {
				t.Fatalf("cost=%g want=%g", cost, want)
			}
		})
	}
	p := usageParser{usageAdapter: "kimi", protocol: config.ProtocolOpenAIResponses, requestBody: []byte(`{"prompt_cache_options":{"ttl":"1h"}}`)}
	got, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":20}}}`))
	if got.InputTokens != 40 || got.CacheCreationTokens != 20 || got.CacheCreation5mTokens != 0 || got.CacheCreation1hTokens != 0 || !got.costIncomplete || got.diagnostic == "" {
		t.Fatalf("Responses guessed Chat TTL: %+v", got)
	}
	got, _, _ = (usageParser{usageAdapter: "kimi", protocol: config.ProtocolOpenAIChat}).extractTokens("application/json", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"cached_tokens":40,"prompt_tokens_details":{"cache_write_tokens":0}}}`))
	if got.costIncomplete {
		t.Fatalf("zero writes need no TTL evidence: %+v", got)
	}
}

func TestKimiNativeUsageUntouched(t *testing.T) {
	for _, tc := range []struct {
		op                                config.ProtocolType
		body                              string
		input, cached, writes, five, hour int64
		adapter                           string
	}{
		{config.ProtocolAnthropicMessages, `{"usage":{"input_tokens":30,"output_tokens":7,"cache_read_input_tokens":40,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":15}}}`, 30, 40, 20, 5, 15, "kimi"},
		{config.ProtocolGemini, `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":7,"cachedContentTokenCount":40}}`, 60, 40, 0, 0, 0, "standard_gemini"},
	} {
		got, _, _ := (usageParser{usageAdapter: "kimi", protocol: tc.op}).extractTokens("application/json", []byte(tc.body))
		standard, _, _ := (usageParser{usageAdapter: "standard", protocol: tc.op}).extractTokens("application/json", []byte(tc.body))
		if got.InputTokens != tc.input || got.CachedTokens != tc.cached || got.CacheCreationTokens != tc.writes || got.CacheCreation5mTokens != tc.five || got.CacheCreation1hTokens != tc.hour || got.adapter != tc.adapter || got.costIncomplete != standard.costIncomplete || got.diagnostic != standard.diagnostic {
			t.Fatalf("native %s changed: %+v", tc.op, got)
		}
	}
}

func TestServiceKimi429QuotaResetAndTransient(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			up, srv := newKeyedUpstream(t, "k0", `{"error":{"type":"exceeded_current_quota_error"}}`)
			target := config.Target{Name: "pool", Endpoint: srv.URL, UsageAdapter: "kimi", APIKey: "k0", APIKeys: []string{"k1"}}
			if reset {
				target.KeyResetTime = time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04")
			}
			svc, rr := serveKeyExhaustionRequest(t, target, "not-a-known-kimi-model")
			if rr.Code != 200 || up.count("k0") != 1 || up.count("k1") != 1 {
				t.Fatalf("status=%d hits=%d/%d", rr.Code, up.count("k0"), up.count("k1"))
			}
			state, _ := svc.targetByName("pool")
			e := state.keyPool.entries[0]
			if reset {
				if e.exhaustReason != "quota_exceeded_subscription" || !e.cooldownEnd.Equal(state.keyPool.resetTime) {
					t.Fatalf("reset state=%+v", e)
				}
			} else if e.exhaustReason != "quota_exceeded_api" || !e.cooldownEnd.Equal(permanentCooldownEnd) || state.keyPool.status()[0].Status != "额度超限(人工恢复)" {
				t.Fatalf("manual state=%+v", e)
			}
		})
	}
	for _, typ := range []string{"rate_limit_reached_error", "engine_overloaded_error"} {
		t.Run(typ, func(t *testing.T) {
			up, srv := newKeyedUpstream(t, "k0", `{"error":{"type":"`+typ+`"}}`)
			svc, rr := serveKeyExhaustionRequest(t, config.Target{Name: "pool", Endpoint: srv.URL, UsageAdapter: "kimi", APIKey: "k0", APIKeys: []string{"k1"}}, "unfamiliar")
			if rr.Code != 200 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			state, _ := svc.targetByName("pool")
			e := state.keyPool.entries[0]
			if rr.Code != 200 || up.count("k0") != 2 || isAccountLevelExhaustion(e.exhaustReason) || time.Until(e.cooldownEnd) > time.Hour {
				t.Fatalf("transient status=%d hits=%d state=%+v", rr.Code, up.count("k0"), e)
			}
		})
	}
}
