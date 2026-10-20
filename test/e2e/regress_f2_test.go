//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestRegressF2ExplicitPatchAppliesToEveryModel guards #160 F2: the vendor
// patch is chosen per provider (target). An explicit usage_adapter applies to
// models that are not in any built-in model list.
func TestRegressF2ExplicitPatchAppliesToEveryModel(t *testing.T) {
	const cached = "[[fake usage.in=100 usage.out=30 usage.cached=40 usage.reasoning=6]]"
	cases := []struct {
		name, flavor, adapter, path, directive string
		body                                   func(string, bool) map[string]any
		hdr                                    map[string]string
		want                                   billed
		explicit                               int64
	}{
		{name: "deepseek-unlisted", flavor: "deepseek", adapter: "deepseek", body: chatBody,
			directive: cached, want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
		{name: "glm-unlisted", flavor: "glm", adapter: "glm", body: chatBody,
			directive: cached, want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
		{name: "qwen-unlisted-explicit", flavor: "qwen", adapter: "qwen", body: qwenCached(chatBody),
			directive: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}, explicit: 40},
		{name: "qwen-unlisted-implicit", flavor: "qwen", adapter: "qwen", body: chatBody,
			directive: cached, want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
		{name: "qwen-unlisted-session-cache", flavor: "qwen", adapter: "qwen", body: responsesBody, path: "/v1/responses",
			hdr:       map[string]string{"x-dashscope-session-cache": "enable"},
			directive: stdUsage, want: billed{in: 40, out: 30, cached: 40, creation: 20, reasoning: 6}, explicit: 40},
		{name: "qwen-unlisted-anthropic", flavor: "qwen", adapter: "qwen", body: qwenCached(messagesBody), path: "/v1/messages",
			directive: "[[fake usage.in=100 usage.out=30 usage.cached=40 usage.cw5m=20 usage.cw1h=10]]",
			want:      billed{in: 30, out: 30, cached: 40, creation: 30, cw5m: 20, cw1h: 10}, explicit: 40},
		// The same vendor-shaped response and request do not activate a Patch
		// when the operator selects standard, even with vendor-like model names.
		{name: "standard-deepseek", flavor: "deepseek", adapter: "standard", body: chatBody,
			directive: cached, want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
		{name: "standard-glm", flavor: "glm", adapter: "standard", body: chatBody,
			directive: cached, want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
		{name: "standard-qwen", flavor: "qwen", adapter: "standard", body: qwenCached(chatBody),
			directive: stdUsage, want: billed{in: 60, out: 30, cached: 40, reasoning: 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alias := uniq("e2e-f2")
			upstream := "Private-Future-Model-Not-In-Catalog"
			if tc.adapter == "standard" {
				upstream = tc.flavor + "-chat"
			}
			if tc.name == "standard-qwen" {
				upstream = "qwen3.8-max"
			}
			// w.urlA is a loopback fakellm host, never an official provider host.
			tg := target(t, map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": tc.adapter,
				"endpoint": w.urlA + "/" + tc.flavor, "anthropic_prefix": "/anthropic", "supports_responses": true,
				"api_key": uniq("sk-f2"), "model_mappings": []any{map[string]any{"upstream": upstream, "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			path := tc.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			resp, body := call(t, key, "POST", path, expand(tc.body(tc.directive, false), alias), tc.hdr)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
			}
			ev := usageFor(t, name, 1)[0]
			wantAdapter := tc.adapter
			if wantAdapter == "standard" {
				wantAdapter = "standard_openai"
			}
			if ev.UsageAdapter != wantAdapter {
				t.Errorf("usage_adapter = %q, want %q", ev.UsageAdapter, wantAdapter)
			}
			checkBilled(t, ev, tc.want)
			if ev.ExplicitCacheReadTokens != tc.explicit || ev.ExplicitCacheReadTokens > ev.CachedTokens {
				t.Errorf("explicit cache read=%d cached=%d, want explicit=%d (subset, not extra tokens)", ev.ExplicitCacheReadTokens, ev.CachedTokens, tc.explicit)
			}
			if ev.CostIncomplete {
				t.Errorf("unexpected cost incomplete: %s", ev.UsageDiagnostic)
			}
			rec := lastRecord(t, w.fakeA, path)
			if rec.Model != upstream || rec.Flavor != tc.flavor {
				t.Errorf("upstream model=%q flavor=%q, want %q/%q", rec.Model, rec.Flavor, upstream, tc.flavor)
			}
		})
	}
}

// Bypass target(): a fixture default must not hide the API's required field.
func TestRegressF2CreateTargetRequiresUsageAdapter(t *testing.T) {
	name := uniq("e2e-f2-missing")
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/targets/"+name, nil) })
	code, body := w.adminDo("POST", "/admin/data/targets", map[string]any{
		"name": name, "endpoint_type": "openai", "endpoint": w.urlA + "/openai/v1", "api_key": uniq("sk-f2")})
	if code != http.StatusBadRequest {
		t.Fatalf("missing usage_adapter: HTTP %d %s, want 400", code, body)
	}
	code, body = w.adminDo("GET", "/admin/data/targets", nil)
	if code != http.StatusOK {
		t.Fatalf("list targets: HTTP %d %s", code, body)
	}
	var page struct {
		Targets []struct {
			Name string `json:"name"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	for _, tg := range page.Targets {
		if tg.Name == name {
			t.Fatalf("rejected target %s was persisted", name)
		}
	}
}
