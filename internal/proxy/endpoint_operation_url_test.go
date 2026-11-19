package proxy

import (
	"net/url"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func operationURLTarget(t *testing.T, spec config.Target) *Target {
	t.Helper()
	spec.Name = "op-url"
	spec.APIKey = "fixture-key"
	parsed, _, err := buildTargetStates([]config.Target{spec})
	if err != nil {
		t.Fatalf("buildTargetStates: %v", err)
	}
	return parsed["op-url"].Target()
}

// endpoint 填成完整接口地址（官方文档常见写法）时，接口后缀之前的部分是 base，
// 客户端路径的版本段被替换，而不是拼在接口路径之后。Issue #232。
func TestBuildURLEndpointWithOperationSuffix(t *testing.T) {
	cases := []struct {
		name       string
		spec       config.Target
		clientPath string
		want       string
	}{
		{"qianfan chat", config.Target{EndpointType: "openai", Endpoint: "https://qianfan.baidubce.com/v2/chat/completions"},
			"/v1/chat/completions", "https://qianfan.baidubce.com/v2/chat/completions"},
		{"qianfan responses", config.Target{EndpointType: "openai", Endpoint: "https://qianfan.baidubce.com/v2/chat/completions"},
			"/v1/responses", "https://qianfan.baidubce.com/v2/responses"},
		{"qianfan embeddings", config.Target{EndpointType: "openai", Endpoint: "https://qianfan.baidubce.com/v2/chat/completions"},
			"/v1/embeddings", "https://qianfan.baidubce.com/v2/embeddings"},
		{"client without version", config.Target{EndpointType: "openai", Endpoint: "https://qianfan.baidubce.com/v2/chat/completions"},
			"/chat/completions", "https://qianfan.baidubce.com/v2/chat/completions"},
		{"trailing slash", config.Target{EndpointType: "openai", Endpoint: "https://host.example/api/v3/chat/completions/"},
			"/v1/chat/completions", "https://host.example/api/v3/chat/completions"},
		{"upper case suffix", config.Target{EndpointType: "openai", Endpoint: "https://host.example/v2/Chat/Completions"},
			"/v1/chat/completions", "https://host.example/v2/chat/completions"},
		// base 末段不是版本段：完整地址已表明接口就在 base 之下，客户端版本段同样替换。
		{"non-version base", config.Target{EndpointType: "openai", Endpoint: "https://host.example/v1beta/openai/chat/completions"},
			"/v1/chat/completions", "https://host.example/v1beta/openai/chat/completions"},
		{"root base", config.Target{EndpointType: "openai", Endpoint: "https://host.example/chat/completions"},
			"/v1/chat/completions", "https://host.example/chat/completions"},
		{"claude messages", config.Target{EndpointType: "claude", Endpoint: "https://host.example/anthropic/v1/messages"},
			"/v1/messages", "https://host.example/anthropic/v1/messages"},
		{"claude count tokens", config.Target{EndpointType: "claude", Endpoint: "https://host.example/anthropic/v1/messages"},
			"/v1/messages/count_tokens", "https://host.example/anthropic/v1/messages/count_tokens"},
		{"dual empty prefixes", config.Target{EndpointType: "dual_protocol", Endpoint: "https://host.example/v2/chat/completions"},
			"/v1/chat/completions", "https://host.example/v2/chat/completions"},
		{"dual empty prefixes non-version base", config.Target{EndpointType: "dual_protocol", Endpoint: "https://host.example/gw/chat/completions"},
			"/v1/chat/completions", "https://host.example/gw/chat/completions"},
		{"dual prefix still decides", config.Target{EndpointType: "dual_protocol", Endpoint: "https://host.example/gw/chat/completions", OpenAIPrefix: "/compatible-mode"},
			"/v1/chat/completions", "https://host.example/gw/compatible-mode/v1/chat/completions"},

		// 反例：普通 base 的行为不变。
		{"plain versioned base", config.Target{EndpointType: "openai", Endpoint: "https://host.example/v1"},
			"/v1/chat/completions", "https://host.example/v1/chat/completions"},
		{"plain root", config.Target{EndpointType: "openai", Endpoint: "https://host.example"},
			"/v1/chat/completions", "https://host.example/v1/chat/completions"},
		{"plain non-version base", config.Target{EndpointType: "openai", Endpoint: "https://host.example/compatible-mode"},
			"/v1/chat/completions", "https://host.example/compatible-mode/v1/chat/completions"},
		{"suffix mid path", config.Target{EndpointType: "openai", Endpoint: "https://host.example/chat/completions/proxy"},
			"/v1/chat/completions", "https://host.example/chat/completions/proxy/v1/chat/completions"},
		{"terminal image url", config.Target{EndpointType: "openai_image", Endpoint: "https://host.example/v1/images/generations"},
			"/v1/images/generations", "https://host.example/v1/images/generations"},
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := operationURLTarget(t, tc.spec)
			got, err := s.buildURL(target, &url.URL{Path: tc.clientPath})
			if err != nil {
				t.Fatalf("buildURL: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("buildURL = %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestEndpointOperationSuffixRuntimeEndpoint(t *testing.T) {
	// 运行态 endpoint 为 base：状态展示、探测与模型测试共用。
	target := operationURLTarget(t, config.Target{EndpointType: "openai", Endpoint: "https://host.example/v2/chat/completions"})
	if got := target.Endpoint.String(); got != "https://host.example/v2" {
		t.Errorf("runtime endpoint = %q, want base", got)
	}
	// Azure 与终态 URL 类型不识别接口后缀。
	azure := operationURLTarget(t, config.Target{EndpointType: "azure_openai", Endpoint: "https://host.example/openai/deployments/d/chat/completions"})
	if got := azure.Endpoint.Path; got != "/openai/deployments/d/chat/completions" {
		t.Errorf("azure endpoint path = %q, want unchanged", got)
	}
	image := operationURLTarget(t, config.Target{EndpointType: "openai_image", Endpoint: "https://host.example/v1/images/generations"})
	if got := image.Endpoint.Path; got != "/v1/images/generations" {
		t.Errorf("image endpoint path = %q, want unchanged", got)
	}
}

func TestEndpointOperationSuffixIsConnectionIdentity(t *testing.T) {
	// /gw 与 /gw/chat/completions 的运行态 endpoint 相同，但转发路径不同
	// （后者替换客户端版本段），必须视为连接身份变化。
	plain := operationURLTarget(t, config.Target{EndpointType: "openai", Endpoint: "https://host.example/gw"})
	full := operationURLTarget(t, config.Target{EndpointType: "openai", Endpoint: "https://host.example/gw/chat/completions"})
	changed := targetIdentityChanges(plain, full)
	if len(changed) != 1 || changed[0] != "endpoint" {
		t.Errorf("identity changes = %v, want [endpoint]", changed)
	}
	if again := targetIdentityChanges(full, operationURLTarget(t, config.Target{EndpointType: "openai", Endpoint: "https://host.example/gw/chat/completions/"})); len(again) != 0 {
		t.Errorf("identical full URL reported changes %v", again)
	}
}
