//go:build e2e

package e2e

import (
	"bytes"
	"math"
	"net/url"
	"testing"
)

// TestImageUsageBilled (F4, #134 step 1): the Images API reports
// input_tokens/output_tokens; generations must be billed from them, both as a
// JSON response and as an image_generation SSE stream.
func TestImageUsageBilled(t *testing.T) {
	for _, details := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := "json"
			if stream {
				name = "stream"
			}
			if details {
				name += "/with-output-details"
			} else {
				name += "/without-output-details"
			}
			t.Run(name, func(t *testing.T) {
				alias := uniq("e2e-img")
				// openai_image endpoints are terminal URLs: the client path is replaced.
				tg := target(t, map[string]any{"endpoint_type": "openai_image", "endpoint": w.urlA + "/openai/v1/images/generations", "api_key": uniq("sk-img"),
					"model_mappings": []any{map[string]any{"upstream": "gpt-image-1", "fallback": alias}}})
				client, key := client(t, []string{tg}, nil)
				code, data := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{"target": tg, "model": "gpt-image-1", "mode": "flat", "base_rates": map[string]any{"input_per_1m_tokens": 2, "output_per_1m_tokens": 4, "image_input_per_1m_tokens": 10, "image_output_per_1m_tokens": 20}})
				if code != 201 {
					t.Fatalf("image prices %d %s", code, data)
				}
				// _files is a fake-only fixture signal, not a real upstream call.
				body := map[string]any{"model": alias, "prompt": "[[fake usage.in=50 usage.out=272]]", "_files": 1}
				body["_output_tokens_details"] = details
				if stream {
					body["stream"] = true
				}
				resp, raw := call(t, key, "POST", "/v1/images/generations", body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("HTTP %d %.200s", resp.StatusCode, raw)
				}
				if bytes.Contains(raw, []byte(`"output_tokens_details"`)) != details {
					t.Fatalf("wrong fake output shape: %s", raw)
				}
				ev := usageFor(t, client, 1)[0]
				if ev.InputTokens != 115 || ev.OutputTokens != 272 || ev.ImageInputTokens != 65 || ev.ImageOutputTokens != 272 || ev.UsageStatus != "complete" || ev.CostIncomplete {
					t.Fatalf("billed in=%d out=%d status=%q diag=%q", ev.InputTokens, ev.OutputTokens, ev.UsageStatus, ev.UsageDiagnostic)
				}
				code, data = w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(client), nil)
				if code != 200 {
					t.Fatalf("aggregate %d %s", code, data)
				}
				got, ok := dig(decode(t, data), "totals/estimated_cost").(float64)
				if !ok || math.Abs(got-.006190) > 1e-10 {
					t.Fatalf("image bill=%v want=.006190 data=%s", got, data)
				}
			})
		}
	}
}
