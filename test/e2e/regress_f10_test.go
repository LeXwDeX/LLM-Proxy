//go:build e2e

package e2e

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

func TestRegressF10OptInStreamUsage(t *testing.T) {
	alias := uniq("e2e-f10")
	tg := target(t, map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "qwen", "endpoint": w.urlA + "/qwen", "openai_prefix": "/compatible-mode", "anthropic_prefix": "/apps/anthropic", "api_key": uniq("sk-f10"), "request_stream_usage_if_missing": true, "model_mappings": []any{map[string]any{"upstream": "qwen3.8-max", "fallback": alias}}})
	name, key := client(t, []string{tg}, nil)
	call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "[[fake usage.in=100 usage.out=30 usage.cached=0 usage.cw5m=0 usage.reasoning=6]]"}}}, nil)
	ev := usageFor(t, name, 1)[0]
	if ev.UsageStatus != "complete" || ev.InputTokens != 100 || ev.OutputTokens != 30 || ev.ReasoningTokens != 6 || ev.CachedTokens != 0 {
		t.Fatalf("usage not complete: %+v", ev)
	}
}

func TestRegressF10StillIncompleteWithoutUpstreamUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options any
		noUsage bool
	}{{"client-false", map[string]any{"include_usage": false}, false}, {"upstream-no-usage", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			alias, keyUp := uniq("e2e-f10-boundary"), uniq("sk-f10-boundary")
			tg := target(t, map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": w.urlA + "/openai/v1", "api_key": keyUp, "request_stream_usage_if_missing": true, "model_mappings": []any{map[string]any{"upstream": "qwen-max", "fallback": alias}}})
			if tc.noUsage {
				w.fakeA.SetKeyScenario(keyUp, &fakellm.Scenario{NoUsage: true})
				defer w.fakeA.SetKeyScenario(keyUp, nil)
			}
			name, key := client(t, []string{tg}, nil)
			body := map[string]any{"model": alias, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if tc.options != nil {
				body["stream_options"] = tc.options
			}
			call(t, key, "POST", "/v1/chat/completions", body, nil)
			ev := usageFor(t, name, 1)[0]
			if ev.UsageStatus != "incomplete" || ev.UsageDiagnostic != "missing_usage" || !ev.CostIncomplete || ev.InputTokens != 0 || ev.OutputTokens != 0 {
				t.Fatalf("must not estimate: %+v", ev)
			}
		})
	}
}
