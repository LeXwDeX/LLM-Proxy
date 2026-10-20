//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// billed is the expected usage event. Input is the billable uncached input
// (total input minus cache reads and cache writes), as the proxy stores it.
type billed struct {
	in, out, cached, creation, cw5m, cw1h, reasoning int64
}

func checkBilled(t *testing.T, ev usageEvent, want billed) {
	t.Helper()
	got := billed{ev.InputTokens, ev.OutputTokens, ev.CachedTokens, ev.CacheCreationTokens, ev.CacheCreation5m, ev.CacheCreation1h, ev.ReasoningTokens}
	if got != want || ev.UsageStatus != "complete" {
		t.Errorf("billed %+v status=%q diag=%q adapter=%q, want %+v", got, ev.UsageStatus, ev.UsageDiagnostic, ev.UsageAdapter, want)
	}
}

// vendorCase is one (endpoint configuration, client protocol) pair.
type vendorCase struct {
	name     string
	target   map[string]any // without name/api_key/model_mappings
	upstream string         // upstream model name (case preserved)
	path     string         // client path; {m} is replaced by the alias
	body     func(directive string, stream bool) map[string]any
	upPath   string // expected upstream path
	auth     string // expected upstream auth scheme
	usage    string // directive for usage
	want     billed
	noStream bool
	hdr      map[string]string // extra client headers
}

func chatBody(d string, stream bool) map[string]any {
	b := map[string]any{"model": "{m}", "messages": []any{map[string]any{"role": "user", "content": d}}}
	if stream {
		b["stream"] = true
		b["stream_options"] = map[string]any{"include_usage": true}
	}
	return b
}

func messagesBody(d string, stream bool) map[string]any {
	return map[string]any{"model": "{m}", "max_tokens": 64, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": d}}}
}

// qwenCached marks the request as using Alibaba's explicit cache, the only
// mode in which cache_creation_input_tokens is billed.
func qwenCached(body func(string, bool) map[string]any) func(string, bool) map[string]any {
	return func(d string, stream bool) map[string]any {
		b := body(d, stream)
		b["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": d,
			"cache_control": map[string]any{"type": "ephemeral"}}}}}
		return b
	}
}

func responsesBody(d string, stream bool) map[string]any {
	return map[string]any{"model": "{m}", "input": d, "stream": stream}
}

func geminiBody(d string, _ bool) map[string]any {
	return map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": d}}}}}
}

const stdUsage = "[[fake usage.in=100 usage.out=30 usage.cached=40 usage.cw5m=20 usage.reasoning=6]]"

func vendorCases() []vendorCase {
	return []vendorCase{
		// ---- 原厂 ----
		{name: "openai-chat", target: map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": "{A}/openai/v1"},
			upstream: "GPT-Upstream-Case", path: "/v1/chat/completions", body: chatBody, upPath: "/openai/v1/chat/completions", auth: "bearer",
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}},
		{name: "openai-responses", target: map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": "{A}/openai/v1", "supports_responses": true},
			upstream: "gpt-resp", path: "/v1/responses", body: responsesBody, upPath: "/openai/v1/responses", auth: "bearer",
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}},
		{name: "azure-deployment", target: map[string]any{"endpoint_type": "azure_openai", "endpoint": "{A}/azure"},
			upstream: "gpt-azure", path: "/openai/deployments/{m}/chat/completions", body: chatBody, upPath: "/azure/openai/deployments/gpt-azure/chat/completions", auth: "api-key",
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}},
		{name: "claude-messages", target: map[string]any{"endpoint_type": "claude", "endpoint": "{A}/anthropic"},
			upstream: "claude-x", path: "/v1/messages", body: messagesBody, upPath: "/anthropic/v1/messages", auth: "x-api-key",
			usage: "[[fake usage.in=1000 usage.out=50 usage.cached=600 usage.cw5m=100 usage.cw1h=50]]", want: billed{in: 250, out: 50, cached: 600, creation: 150, cw5m: 100, cw1h: 50}},
		{name: "gemini-generate", target: map[string]any{"endpoint_type": "gemini", "endpoint": "{A}/gemini"},
			upstream: "gemini-x", path: "/v1beta/models/{m}:generateContent", body: geminiBody, upPath: ":generateContent", auth: "x-goog-api-key",
			usage: "[[fake usage.in=200 usage.out=40 usage.reasoning=10]]", want: billed{in: 200, out: 40, reasoning: 10}, noStream: true},
		{name: "gemini-stream", target: map[string]any{"endpoint_type": "gemini", "endpoint": "{A}/gemini"},
			upstream: "gemini-x", path: "/v1beta/models/{m}:streamGenerateContent?alt=sse", body: geminiBody, upPath: ":streamGenerateContent", auth: "x-goog-api-key",
			usage: "[[fake usage.in=200 usage.out=40 usage.reasoning=10]]", want: billed{in: 200, out: 40, reasoning: 10}, noStream: true},
		// Gemini OpenAI-compatible Chat reports reasoning and text as disjoint
		// output parts (issue #6); the standard rule must accept that partition.
		{name: "gemini-openai-chat", target: map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": "{A}/gemini/v1"},
			upstream: "gemini-x", path: "/v1/chat/completions", body: chatBody, upPath: "/gemini/v1/chat/completions", auth: "bearer",
			usage: "[[fake usage.in=8 usage.out=25 usage.reasoning=24]]", want: billed{in: 8, out: 25, reasoning: 24}},

		// ---- Patch：国产厂商 ----
		// Targets are configured as an operator does in the admin UI, which
		// only offers canonical endpoint types (dual_protocol + prefixes).
		{name: "deepseek-chat", target: map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "deepseek", "endpoint": "{A}/deepseek", "anthropic_prefix": "/anthropic"},
			upstream: "deepseek-chat", path: "/v1/chat/completions", body: chatBody, upPath: "/deepseek/v1/chat/completions", auth: "bearer",
			usage: "[[fake usage.in=100 usage.out=30 usage.cached=40 usage.reasoning=6]]", want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
		{name: "deepseek-anthropic", target: map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "deepseek", "endpoint": "{A}/deepseek", "anthropic_prefix": "/anthropic"},
			upstream: "deepseek-chat", path: "/v1/messages", body: messagesBody, upPath: "/deepseek/anthropic/v1/messages", auth: "bearer",
			usage: "[[fake usage.in=100 usage.out=30 usage.cached=40]]", want: billed{in: 60, out: 30, cached: 40}},
		{name: "qwen-chat", target: map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "qwen", "endpoint": "{A}/qwen", "openai_prefix": "/compatible-mode", "anthropic_prefix": "/apps/anthropic", "supports_responses": true},
			upstream: "qwen3.8-max", path: "/v1/chat/completions", body: qwenCached(chatBody), upPath: "/qwen/compatible-mode/v1/chat/completions", auth: "bearer",
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}},
		{name: "qwen-responses", target: map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "qwen", "endpoint": "{A}/qwen", "openai_prefix": "/compatible-mode", "anthropic_prefix": "/apps/anthropic", "supports_responses": true},
			upstream: "qwen3.8-max", path: "/v1/responses", body: responsesBody, upPath: "/qwen/compatible-mode/v1/responses", auth: "bearer",
			hdr:   map[string]string{"x-dashscope-session-cache": "enable"},
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}},
		{name: "qwen-anthropic", target: map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": "qwen", "endpoint": "{A}/qwen", "openai_prefix": "/compatible-mode", "anthropic_prefix": "/apps/anthropic", "supports_responses": true},
			upstream: "qwen3.8-max", path: "/v1/messages", body: qwenCached(messagesBody), upPath: "/qwen/apps/anthropic/v1/messages", auth: "bearer",
			usage: "[[fake usage.in=100 usage.out=30 usage.cached=40 usage.cw5m=20]]", want: billed{in: 40, out: 30, cached: 40, creation: 20, cw5m: 20}},
		{name: "glm-paas", target: map[string]any{"endpoint_type": "openai", "usage_adapter": "glm", "endpoint": "{A}/glm/api/paas/v4"},
			upstream: "glm-4.6", path: "/v1/chat/completions", body: chatBody, upPath: "/glm/api/paas/v4/chat/completions", auth: "bearer",
			usage: "[[fake usage.in=100 usage.out=30 usage.cached=40 usage.reasoning=6]]", want: billed{in: 60, out: 30, cached: 40, reasoning: 6}, noStream: true},
		{name: "glm-anthropic", target: map[string]any{"endpoint_type": "claude", "usage_adapter": "glm", "endpoint": "{A}/glm/api/anthropic"},
			upstream: "glm-4.6", path: "/v1/messages", body: messagesBody, upPath: "/glm/api/anthropic/v1/messages", auth: "x-api-key",
			usage: "[[fake usage.in=100 usage.out=30 usage.cached=40]]", want: billed{in: 60, out: 30, cached: 40}},
		{name: "kimi-chat", target: map[string]any{"endpoint_type": "openai", "usage_adapter": "kimi", "endpoint": "{A}/kimi/v1"},
			upstream: "Unfamiliar-Upstream", path: "/v1/chat/completions", body: chatBody, upPath: "/kimi/v1/chat/completions", auth: "bearer",
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, cw5m: 20, reasoning: 6}},
		{name: "openrouter-chat", target: map[string]any{"endpoint_type": "openai", "endpoint": "{A}/openrouter/api/v1"},
			upstream: "z-ai/glm-4.6", path: "/v1/chat/completions", body: chatBody, upPath: "/openrouter/api/v1/chat/completions", auth: "bearer",
			usage: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}},
	}
}

func expand(v any, alias string) any {
	switch x := v.(type) {
	case string:
		return strings.NewReplacer("{m}", alias, "{A}", w.urlA, "{B}", w.urlB).Replace(x)
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = expand(e, alias)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = expand(e, alias)
		}
		return out
	}
	return v
}

// TestVendorMatrix covers 原厂 + Patch vendors, non-stream and stream.
func TestVendorMatrix(t *testing.T) {
	for _, vc := range vendorCases() {
		t.Run(vc.name, func(t *testing.T) {
			alias := "e2e-" + vc.name
			spec := expand(vc.target, alias).(map[string]any)
			spec["api_key"] = "sk-up-" + vc.name
			spec["model_mappings"] = []any{map[string]any{"upstream": vc.upstream, "fallback": alias}}
			tg := target(t, spec)
			name, key := client(t, []string{tg}, nil)
			modes := []bool{false, true}
			if vc.noStream {
				modes = []bool{false}
			}
			for _, stream := range modes {
				path := expand(vc.path, alias).(string)
				resp, body := call(t, key, "POST", path, expand(vc.body(vc.usage, stream), alias), vc.hdr)
				if resp.StatusCode != 200 {
					t.Fatalf("stream=%v: HTTP %d %s", stream, resp.StatusCode, body)
				}
				if !strings.Contains(string(body), "fake reply") {
					t.Fatalf("stream=%v: no fake reply in %s", stream, body)
				}
				rec := lastRecord(t, w.fakeA, vc.upPath)
				if !strings.HasSuffix(rec.Path, vc.upPath) || rec.Auth != vc.auth {
					t.Errorf("upstream path=%s auth=%s, want suffix %s auth %s", rec.Path, rec.Auth, vc.upPath, vc.auth)
				}
				if rec.Model != vc.upstream {
					t.Errorf("upstream model %q, want %q (case preserved)", rec.Model, vc.upstream)
				}
			}
			for _, ev := range usageFor(t, name, len(modes)) {
				checkBilled(t, ev, vc.want)
			}
		})
	}
}
