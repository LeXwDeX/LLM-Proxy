package config

import "testing"

func TestExplicitUsageAdapterContract(t *testing.T) {
	for _, value := range []string{"", " ", "auto", " AUTO ", "unknown"} {
		if IsValidUsageAdapter(value) {
			t.Errorf("accepted non-explicit adapter %q", value)
		}
	}
	if got := NormalizeUsageAdapter("  "); got != "" {
		t.Errorf("empty normalization = %q", got)
	}
	if got := NormalizeUsageAdapter(" QWEN "); got != UsageAdapterQwen {
		t.Errorf("normalization = %q", got)
	}
	if !IsValidUsageAdapter(" KIMI ") || NormalizeUsageAdapter(" KIMI ") != "kimi" {
		t.Error("explicit Kimi selection must be accepted and normalized")
	}
	for _, value := range []string{" OPENROUTER ", " MINIMAX "} {
		if !IsValidUsageAdapter(value) {
			t.Errorf("rejected explicit adapter %q", value)
		}
	}
	if len(AllUsageAdapterOptions()) != 7 {
		t.Errorf("options = %+v", AllUsageAdapterOptions())
	}
}

func TestLegacyUsageAdapterForEndpoint(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"https://dashscope.aliyuncs.com/compatible-mode", UsageAdapterQwen},
		{"https://dashscope-intl.aliyuncs.com", UsageAdapterQwen},
		{"https://dashscope-us.aliyuncs.com", UsageAdapterQwen},
		{"https://workspace-1.cn-beijing.maas.aliyuncs.com", UsageAdapterQwen},
		{"https://workspace.cn-shanghai.maas.aliyuncs.com", UsageAdapterQwen},
		{"https://workspace.ap-southeast-1.maas.aliyuncs.com", UsageAdapterQwen},
		{"https://workspace.us-east-1.maas.aliyuncs.com", UsageAdapterQwen},
		{"https://open.bigmodel.cn/api", UsageAdapterGLM},
		{"https://API.Z.AI:443/v1", UsageAdapterGLM},
		{"https://user:secret@api.deepseek.com:443/v1?key=secret", UsageAdapterDeepSeek},
		{"https://custom.example.com", UsageAdapterStandard},
		{"https://api.moonshot.cn/v1", UsageAdapterStandard},
		{"https://api.kimi.com/v1", UsageAdapterStandard},
		{"https://dashscope.aliyuncs.com.example.com", UsageAdapterStandard},
		{"https://workspace.cn-hangzhou.maas.aliyuncs.com", UsageAdapterStandard},
		{"https://nested.workspace.cn-beijing.maas.aliyuncs.com", UsageAdapterStandard},
		{"http://127.0.0.1:9000", UsageAdapterStandard},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			got, err := LegacyUsageAdapterForEndpoint(tc.endpoint)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, endpoint := range []string{"", "not-a-url", "/relative", "https://", "https://%zz", "https://example.com:bad"} {
		if _, err := LegacyUsageAdapterForEndpoint(endpoint); err == nil {
			t.Errorf("accepted invalid endpoint %q", endpoint)
		}
	}
}

func TestUsageAdapterImportProvenanceSurvivesNormalizationAndClone(t *testing.T) {
	cfg, err := parse([]byte(`{"targets":[{"name":"missing"},{"name":"empty","usage_adapter":""},{"name":"auto","usage_adapter":" AUTO "}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Targets {
		cfg.Targets[i].UsageAdapter = NormalizeUsageAdapter(cfg.Targets[i].UsageAdapter)
	}
	clone := cfg.Clone()
	for i, want := range []struct {
		value   string
		present bool
	}{{"", false}, {"", true}, {" AUTO ", true}} {
		value, present := clone.Targets[i].UsageAdapterImportInput()
		if value != want.value || present != want.present {
			t.Fatalf("target %d input: %q %v", i, value, present)
		}
	}
	clone.Targets[2].UsageAdapter = UsageAdapterQwen
	if value, present := clone.Targets[2].UsageAdapterImportInput(); value != UsageAdapterQwen || !present {
		t.Fatalf("new selection used stale provenance: %q %v", value, present)
	}
}
