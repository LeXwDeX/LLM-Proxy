package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"net/url"
	"testing"
)

func TestProviderKeyExhaustionIsolation(t *testing.T) {
	tests := []struct {
		name, host, adapter, model string
		protocol                   config.ProtocolType
		status                     int
		body                       string
		exhausted                  bool
	}{
		{"openai financial", "api.openai.com", "", "gpt-5.6", config.ProtocolOpenAIChat, 429, `{"error":{"code":"credit_balance_exhausted"}}`, true},
		{"openai broad quota type", "api.openai.com", "", "gpt-5.6", config.ProtocolOpenAIChat, 429, `{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`, false},
		{"qwen tpm", "token-plan.cn-beijing.maas.aliyuncs.com", "qwen", "unlisted-model", config.ProtocolOpenAIChat, 429, `{"error":{"code":"Throttling.AllocationQuota","type":"insufficient_quota"}}`, false},
		{"qwen financial", "relay.example", "qwen", "unlisted-model", config.ProtocolOpenAIChat, 429, `{"error":{"code":"PrepaidBillOverdue"}}`, true},
		{"ali hosted glm selected qwen", "dashscope.aliyuncs.com", "qwen", "glm-5", config.ProtocolOpenAIChat, 429, `{"error":{"code":"CommodityNotPurchased"}}`, true},
		{"ali hosted glm selected glm", "dashscope.aliyuncs.com", "glm", "glm-5", config.ProtocolOpenAIChat, 429, `{"error":{"code":"CommodityNotPurchased"}}`, false},
		{"ali host empty no patch", "dashscope.aliyuncs.com", "", "glm-5", config.ProtocolOpenAIChat, 429, `{"error":{"code":"CommodityNotPurchased"}}`, false},
		{"ali host standard no patch", "dashscope.aliyuncs.com", "standard", "qwen3.8-max", config.ProtocolOpenAIChat, 429, `{"error":{"code":"Arrearage"}}`, false},
		{"qwen Gemini protocol financial", "relay.example", "qwen", "unlisted-model", config.ProtocolGemini, 429, `{"error":{"code":"PrepaidBillOverdue"}}`, true},
		{"deepseek Gemini protocol financial", "relay.example", "deepseek", "unlisted-model", config.ProtocolGemini, 402, `{"error":{"code":"insufficient_balance"}}`, true},
		{"deepseek does not use OpenAI financial code", "relay.example", "deepseek", "unlisted-model", config.ProtocolOpenAIChat, 429, `{"error":{"code":"credit_balance_exhausted"}}`, false},
		{"gemini quota temporary", "generativelanguage.googleapis.com", "", "gemini-3-pro", config.ProtocolGemini, 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, false},
		{"qwen code on google ignored", "generativelanguage.googleapis.com", "", "gemini-3-pro", config.ProtocolGemini, 429, `{"error":{"code":"PrepaidBillOverdue"}}`, false},
		{"embedded unrelated text", "api.openai.com", "", "gpt-5.6", config.ProtocolOpenAIChat, 429, `{"error":{"code":"rate_limit_exceeded","message":"example credit_balance_exhausted"}}`, false},
		{"deepseek balance", "api.deepseek.com", "", "deepseek-chat", config.ProtocolOpenAIChat, 402, `{"error":{"message":"Insufficient Balance"}}`, true},
		{"success never exhausts", "api.openai.com", "", "gpt-5.6", config.ProtocolOpenAIChat, 200, `{"error":{"code":"credit_balance_exhausted"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, _ := url.Parse("https://" + tt.host)
			got, _ := targetKeyExhausted(&Target{Endpoint: ep, UsageAdapter: tt.adapter}, tt.protocol, tt.status, []byte(tt.body))
			if got != tt.exhausted {
				t.Fatalf("exhausted=%v want %v", got, tt.exhausted)
			}
		})
	}
}

func TestGLMErrorPatchIdentity(t *testing.T) {
	for _, tt := range []struct {
		host, adapter, model string
		want                 bool
	}{
		{"open.bigmodel.cn", "", "glm-5", false}, {"dashscope.aliyuncs.com", "", "glm-5", false}, {"internal.example", "", "glm-5", false}, {"internal.example", "glm", "glm-5", true}, {"internal.example", "glm", "any-mapped-model", true}, {"open.bigmodel.cn", "standard", "glm-5", false},
	} {
		ep, _ := url.Parse("https://" + tt.host)
		if got := usesGLMErrorPatch(&Target{Endpoint: ep, UsageAdapter: tt.adapter}); got != tt.want {
			t.Fatalf("%+v got %v", tt, got)
		}
	}
}
