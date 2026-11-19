package config

import "testing"

func TestSplitEndpointOperationPath(t *testing.T) {
	cases := []struct {
		path     string
		wantBase string
		wantOp   string
	}{
		{"/v2/chat/completions", "/v2", "/chat/completions"},
		{"/v2/chat/completions/", "/v2", "/chat/completions"},
		{"/V2/Chat/Completions", "/V2", "/chat/completions"},
		{"/chat/completions", "", "/chat/completions"},
		{"/v1/completions", "/v1", "/completions"},
		{"/api/v3/responses", "/api/v3", "/responses"},
		{"/v1/embeddings", "/v1", "/embeddings"},
		{"/anthropic/v1/messages", "/anthropic/v1", "/messages"},
		{"/v1/images/generations", "/v1", "/images/generations"},
		{"/v1/images/edits", "/v1", "/images/edits"},

		// 不是接口后缀：原样返回，operation 为空。
		{"", "", ""},
		{"/", "/", ""},
		{"/v1", "/v1", ""},
		{"/api/paas/v4/", "/api/paas/v4/", ""},
		{"/compatible-mode", "/compatible-mode", ""},
		{"/chat/completions/proxy", "/chat/completions/proxy", ""},
		{"/v1/mymessages", "/v1/mymessages", ""},
		{"/v1/models", "/v1/models", ""},
	}
	for _, tc := range cases {
		base, op := SplitEndpointOperationPath(tc.path)
		if base != tc.wantBase || op != tc.wantOp {
			t.Errorf("SplitEndpointOperationPath(%q) = (%q, %q), want (%q, %q)", tc.path, base, op, tc.wantBase, tc.wantOp)
		}
	}
}

func TestEndpointTypeAcceptsOperationURL(t *testing.T) {
	want := map[string]bool{
		EndpointTypeOpenAI:       true,
		EndpointTypeClaude:       true,
		EndpointTypeGemini:       true,
		EndpointTypeDualProtocol: true,
		EndpointTypeAzureOpenAI:  false,
		EndpointTypeOpenAIImage:  false,
		"unknown":                false,
	}
	for code, expect := range want {
		if got := EndpointTypeAcceptsOperationURL(code); got != expect {
			t.Errorf("EndpointTypeAcceptsOperationURL(%q) = %v, want %v", code, got, expect)
		}
	}
}

func TestEffectiveEndpoint(t *testing.T) {
	cases := []struct {
		endpointType string
		endpoint     string
		want         string
		wantOp       string
	}{
		{EndpointTypeOpenAI, "https://qianfan.baidubce.com/v2/chat/completions", "https://qianfan.baidubce.com/v2", "/chat/completions"},
		{EndpointTypeOpenAI, " https://host.example/chat/completions/ ", "https://host.example", "/chat/completions"},
		{EndpointTypeDualProtocol, "https://host.example/v2/chat/completions?x=1", "https://host.example/v2?x=1", "/chat/completions"},
		{EndpointTypeOpenAI, "https://host.example/v1", "https://host.example/v1", ""},
		{EndpointTypeAzureOpenAI, "https://host.example/openai/deployments/d/chat/completions", "https://host.example/openai/deployments/d/chat/completions", ""},
		{EndpointTypeOpenAIImage, "https://host.example/v1/images/generations", "https://host.example/v1/images/generations", ""},
		{EndpointTypeOpenAI, "::bad", "::bad", ""},
	}
	for _, tc := range cases {
		got, op := EffectiveEndpoint(tc.endpointType, tc.endpoint)
		if got != tc.want || op != tc.wantOp {
			t.Errorf("EffectiveEndpoint(%q, %q) = (%q, %q), want (%q, %q)", tc.endpointType, tc.endpoint, got, op, tc.want, tc.wantOp)
		}
	}
}
