//go:build e2e

package e2e

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// A real proxy process persists the same protocol evidence for JSON and SSE.
// All upstream calls terminate in the in-process deterministic fakellm server.
func TestPricingContextWirePersistence(t *testing.T) {
	for _, tc := range []struct {
		name, path, endpoint, preset, tier, speed string
		body                                      func(string, bool) map[string]any
	}{
		{"chat", "/v1/chat/completions", "{A}/openai/v1", "standard", "standard", "", chatBody},
		{"responses", "/v1/responses", "{A}/openai/v1", "ultrafast", "ultrafast", "", responsesBody},
		{"claude", "/v1/messages", "{A}/anthropic", "priority_standard", "priority", "standard", messagesBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alias := "e2e-pricing-" + tc.name
			endpointType := "openai"
			if tc.name == "claude" {
				endpointType = "claude"
			}
			spec := map[string]any{"endpoint_type": endpointType, "endpoint": expand(tc.endpoint, alias), "api_key": "sk-fake-pricing", "supports_responses": true, "custom_body": map[string]any{"service_tier": "fast"}, "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}}
			tg := target(t, spec)
			name, key := client(t, []string{tg}, nil)
			for _, stream := range []bool{false, true} {
				body := expand(tc.body("[[fake pricing="+tc.preset+" usage.in=272001 usage.cached=272000 usage.out=2]]", stream), alias)
				resp, raw := call(t, key, "POST", tc.path, body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("HTTP %d %s", resp.StatusCode, raw)
				}
			}
			usageFor(t, name, 2)
			q := url.Values{"from": {time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)}, "to": {time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}, "limit": {"1000"}}
			code, raw := w.adminDo("GET", "/admin/data/usage/events?"+q.Encode(), nil)
			if code != 200 {
				t.Fatalf("events %d", code)
			}
			var page struct {
				Events []usage.Event `json:"events"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, ev := range page.Events {
				if ev.ClientName != name {
					continue
				}
				count++
				c := ev.PricingContext
				if c == nil || c.TotalInputTokens != 272001 || !c.TotalInputValid || c.ActualServiceTier != tc.tier || c.ActualSpeed != tc.speed || c.RequestServiceTier.Raw != "fast" {
					t.Fatalf("persisted context %+v", c)
				}
			}
			if count != 2 {
				t.Fatalf("events=%d", count)
			}
		})
	}
}
