//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestMisassembledUpstreamPathFailsE2E：代理拼出的上游路径有形态错误时，虚假上游与
// 真实上游一样返回 404，客户端看到失败而不是一次“成功”的调用（Issue #232）。
// 此前 fakellm 按路径后缀应答，这类配置或拼接缺陷在内部测试中始终是 200。
func TestMisassembledUpstreamPathFailsE2E(t *testing.T) {
	cases := []struct {
		name   string
		target map[string]any
		upPath string
	}{
		// endpoint 与 openai_prefix 各带一个版本段：/v2/v2/chat/completions。
		{"stacked-versions", map[string]any{"endpoint_type": "dual_protocol", "endpoint": "{A}/stack/v2", "openai_prefix": "/v2"},
			"/stack/v2/v2/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alias := "e2e-bad-path-" + tc.name
			spec := expand(tc.target, alias).(map[string]any)
			spec["api_key"] = "sk-up-" + tc.name
			spec["model_mappings"] = []any{map[string]any{"upstream": "bad-path-x", "fallback": alias}}
			tg := target(t, spec)
			_, key := client(t, []string{tg}, nil)
			resp, body := call(t, key, "POST", "/v1/chat/completions", expand(chatBody("", false), alias), nil)
			if resp.StatusCode != 404 {
				t.Fatalf("HTTP %d %s, want the upstream 404", resp.StatusCode, body)
			}
			if strings.Contains(string(body), "fake reply") {
				t.Fatalf("misassembled path was answered: %s", body)
			}
			rec := lastRecord(t, w.fakeA, tc.upPath)
			if rec.Status != 404 {
				t.Errorf("fake recorded status %d for %s, want 404", rec.Status, rec.Path)
			}
		})
	}
}
