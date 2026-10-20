package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ---------------------------------------------------------------------------
// TestBodyPolicyPreserveMultipart — 每个 provider 的 PreserveMultipart 配置
// ---------------------------------------------------------------------------

func TestBodyPolicyPreserveMultipart(t *testing.T) {
	registry := DefaultProviderRegistry()

	cases := []struct {
		endpointType string
		wantPreserve bool
	}{
		{config.EndpointTypeAzureOpenAI, true},
		{config.EndpointTypeOpenAIImage, true},
		{config.EndpointTypeOpenAI, false},
		{config.EndpointTypeClaude, false},
		{config.EndpointTypeGemini, false},
		{config.EndpointTypeDualProtocol, false},
	}

	for _, tc := range cases {
		t.Run(tc.endpointType, func(t *testing.T) {
			profile := registry.Lookup(tc.endpointType)
			if profile == nil {
				t.Fatalf("provider %q not registered", tc.endpointType)
			}
			if profile.Body.PreserveMultipart != tc.wantPreserve {
				t.Errorf("%s.Body.PreserveMultipart = %v, want %v",
					tc.endpointType, profile.Body.PreserveMultipart, tc.wantPreserve)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestBodyPolicySanitizeFunc — Azure 有 SanitizeFunc，其他 provider 无
// ---------------------------------------------------------------------------

func TestBodyPolicySanitizeFunc(t *testing.T) {
	registry := DefaultProviderRegistry()

	// Azure 有 SanitizeFunc
	azure := registry.Lookup(config.EndpointTypeAzureOpenAI)
	if azure == nil {
		t.Fatal("azure_openai not registered")
	}
	if azure.Body.SanitizeFunc == nil {
		t.Fatal("azure_openai should have SanitizeFunc")
	}

	// 其他 provider 无 SanitizeFunc
	otherTypes := []string{
		config.EndpointTypeOpenAI,
		config.EndpointTypeClaude,
		config.EndpointTypeGemini,
		config.EndpointTypeDualProtocol,
		config.EndpointTypeOpenAIImage,
	}
	for _, epType := range otherTypes {
		p := registry.Lookup(epType)
		if p == nil {
			t.Fatalf("provider %q not registered", epType)
		}
		if p.Body.SanitizeFunc != nil {
			t.Errorf("%s should not have SanitizeFunc", epType)
		}
	}
}

// ---------------------------------------------------------------------------
// TestBodyPolicySanitizeFuncBehavior — Azure SanitizeFunc 实际行为
// ---------------------------------------------------------------------------

func TestBodyPolicySanitizeFuncBehavior(t *testing.T) {
	registry := DefaultProviderRegistry()
	azure := registry.Lookup(config.EndpointTypeAzureOpenAI)
	if azure == nil {
		t.Fatal("azure_openai not registered")
	}
	if azure.Body.SanitizeFunc == nil {
		t.Fatal("azure_openai SanitizeFunc is nil")
	}

	// 构造一个包含 Azure 不兼容字段的请求体
	// "custom_field" 不在 Azure 白名单中，应被剥离
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"custom_field":"should_be_stripped"}`)
	req, _ := http.NewRequest("POST", "http://proxy/v1/chat/completions", bytes.NewReader(body))

	sanitized, stripped := azure.Body.SanitizeFunc(req, body)
	if len(stripped) == 0 {
		t.Error("expected some fields to be stripped for Azure")
	}

	// 验证 stripped 字段不在 sanitized body 中
	var parsed map[string]interface{}
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("sanitized body is not valid JSON: %v", err)
	}
	for _, field := range stripped {
		if _, exists := parsed[field]; exists {
			t.Errorf("stripped field %q still present in sanitized body", field)
		}
	}
}
