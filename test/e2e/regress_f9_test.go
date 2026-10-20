//go:build e2e

package e2e

import "testing"

// TestRegressF9VendorFailureFinishReason guards #158 F9: a DeepSeek response
// that ends with finish_reason=insufficient_system_resource is an aborted
// generation and must be recorded as failed, while the usage it reports is
// still billed in full (failed finish != zero usage).
func TestRegressF9VendorFailureFinishReason(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			alias := uniq("e2e-f9-fin")
			tg := target(t, map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "deepseek", "endpoint": w.urlA + "/deepseek",
				"anthropic_prefix": "/anthropic", "api_key": uniq("sk-f9"), "model_mappings": []any{map[string]any{"upstream": "deepseek-chat", "fallback": alias}}})
			clientName, key := client(t, []string{tg}, nil)
			body := map[string]any{"model": alias, "stream": stream,
				"messages": []any{map[string]any{"role": "user", "content": "[[fake finish=insufficient_system_resource usage.in=100 usage.out=7 usage.cached=40]]"}}}
			if stream {
				body["stream_options"] = map[string]any{"include_usage": true}
			}
			resp, raw := call(t, key, "POST", "/v1/chat/completions", body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d %s", resp.StatusCode, raw)
			}
			ev := usageFor(t, clientName, 1)[0]
			if ev.ResponseStatus != "failed" || ev.FinishReason != "insufficient_system_resource" {
				t.Fatalf("aborted generation recorded as response_status=%q finish_reason=%q", ev.ResponseStatus, ev.FinishReason)
			}
			if ev.UsageStatus != "complete" || ev.InputTokens+ev.CachedTokens != 100 || ev.OutputTokens != 7 || ev.CachedTokens != 40 || ev.CostIncomplete {
				t.Fatalf("usage not billed on failed finish: %+v", ev)
			}
		})
	}
}
