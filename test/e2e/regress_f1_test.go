//go:build e2e

package e2e

import "testing"

// TestRegressF1AdminAliasVendorPrefixes (Issue #157 F1): a target created via
// the admin API with a legacy endpoint_type alias gets the same vendor default
// prefixes as one loaded from config, so requests reach the vendor sub-path.
func TestRegressF1AdminAliasVendorPrefixes(t *testing.T) {
	messages := func(alias string) map[string]any {
		return map[string]any{"model": alias, "max_tokens": 8,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}
	chat := func(alias string) map[string]any {
		return map[string]any{"model": alias,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}
	cases := []struct {
		name, epType, adapter, base, upstream, path string
		body                                        func(string) map[string]any
		suffix, upPath                              string
	}{
		{"deepseek-anthropic", "deepseek", "deepseek", "/deepseek", "deepseek-chat", "/v1/messages", messages, "/messages", "/deepseek/anthropic/v1/messages"},
		{"bailian-chat", "bailian", "qwen", "/qwen", "qwen3.8-max", "/v1/chat/completions", chat, "/chat/completions", "/qwen/compatible-mode/v1/chat/completions"},
		{"bailian-anthropic", "bailian", "qwen", "/qwen", "qwen3.8-max", "/v1/messages", messages, "/messages", "/qwen/apps/anthropic/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alias := uniq("e2e-f1")
			tg := target(t, map[string]any{"endpoint_type": tc.epType, "usage_adapter": tc.adapter, "endpoint": w.urlA + tc.base,
				"api_key": uniq("sk-f1"), "model_mappings": []any{map[string]any{"upstream": tc.upstream, "fallback": alias}}})
			_, key := client(t, []string{tg}, nil)
			resp, body := call(t, key, "POST", tc.path, tc.body(alias), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d %s", resp.StatusCode, body)
			}
			if rec := lastRecord(t, w.fakeA, tc.suffix); rec.Path != tc.upPath {
				t.Fatalf("upstream path %s, want %s", rec.Path, tc.upPath)
			}
		})
	}
}
