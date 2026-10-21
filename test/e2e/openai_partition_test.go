//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/gorilla/websocket"
)

// Only the Chat JSON 24/1 sample is observed (issue #6). All other partitions
// and transports here are synthetic regression cases, not vendor API evidence.
func TestOpenAIOutputPartitionsEndToEnd(t *testing.T) {
	for _, transport := range []string{"chat-json", "chat-sse", "responses-json", "responses-sse", "pooled-json", "pooled-sse", "native-ws"} {
		for _, adapter := range []string{"standard", "deepseek"} {
			for _, tc := range []struct {
				name, fields     string
				reasoning, image int64
				conflict         bool
			}{
				{"included", "usage.text_out=25", 24, 0, false},
				{"included-images", "usage.text_out=20 usage.image_out=5", 10, 5, false},
				{"disjoint", "usage.text_out=1", 24, 0, !strings.HasPrefix(transport, "chat-")},
				{"reasoning-exceeds-text", "usage.text_out=1 usage.image_out=24", 24, 24, true},
				{"overage", "usage.text_out=2", 24, 0, true},
			} {
				t.Run(transport+"/"+adapter+"/"+tc.name, func(t *testing.T) {
					alias := uniq("partition")
					pooled := strings.HasPrefix(transport, "pooled-")
					chat := strings.HasPrefix(transport, "chat-")
					tg := target(t, map[string]any{"endpoint_type": "openai", "usage_adapter": adapter,
						"endpoint": w.urlA + "/v1", "api_key": uniq("sk-partition"),
						"supports_responses": true, "prefer_websocket": pooled,
						"model_mappings": []any{map[string]any{"upstream": "Partition-Model", "fallback": alias}}})
					name, key := client(t, []string{tg}, nil)
					rates := openAIPartitionPrice(t, tg)
					directive := fmt.Sprintf("[[fake usage.in=8 usage.out=25 usage.reasoning=%d %s]]", tc.reasoning, tc.fields)
					path := "/v1/responses"
					before := w.fakeA.LastSeq()
					var final map[string]any
					if transport == "native-ws" {
						conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(w.base, "http")+path, http.Header{"Authorization": {"Bearer " + key}})
						if err != nil {
							t.Fatalf("dial: %v %v", err, resp)
						}
						defer conn.Close()
						events := wsTurn(t, conn, map[string]any{"type": "response.create", "model": alias, "input": wsInput(directive)})
						last := events[len(events)-1]
						if last["type"] != "response.completed" {
							t.Fatalf("terminal changed: %v", last)
						}
						final, _ = last["response"].(map[string]any)
					} else {
						stream := strings.HasSuffix(transport, "-sse")
						body := map[string]any{"model": alias, "stream": stream, "input": wsInput(directive)}
						if chat {
							path = "/v1/chat/completions"
							body = expand(chatBody(directive, stream), alias).(map[string]any)
						}
						resp, wire := call(t, key, "POST", path, body, nil)
						if resp.StatusCode != 200 {
							t.Fatalf("HTTP %d: %s", resp.StatusCode, wire)
						}
						if stream {
							if chat && !strings.Contains(string(wire), "data: [DONE]") {
								t.Fatalf("missing Chat terminal: %s", wire)
							}
							for _, line := range strings.Split(string(wire), "\n") {
								var event map[string]any
								if !strings.HasPrefix(line, "data: ") || json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
									continue
								}
								if chat && event["usage"] != nil {
									final = event
								} else if event["type"] == "response.completed" {
									final, _ = event["response"].(map[string]any)
								}
							}
						} else {
							final = decode(t, wire)
						}
					}
					details := "usage/output_tokens_details/"
					output := "usage/output_tokens"
					if chat {
						details, output = "usage/completion_tokens_details/", "usage/completion_tokens"
					}
					if dig(final, output) != 25.0 || dig(final, details+"reasoning_tokens") != float64(tc.reasoning) {
						t.Fatalf("wire usage changed: %v", final)
					}
					if tc.image > 0 && dig(final, details+"image_tokens") != float64(tc.image) {
						t.Fatalf("wire image count changed: %v", final)
					}
					requests := 0
					for _, rec := range w.fakeA.RequestsSince(before) {
						if rec.Status == 101 { // A pooled lane may prewarm another connection.
							continue
						}
						requests++
						if rec.WebSocket != (pooled || transport == "native-ws") || rec.Model != "Partition-Model" {
							t.Fatalf("unexpected transport/model: %+v", rec)
						}
					}
					events := compressionEvents(t, name, 1)
					if len(events) != 1 || requests != 1 {
						t.Fatalf("duplicate events/requests: %d/%d", len(events), requests)
					}
					event := events[0]
					status, diagnostic, wantAdapter := "complete", "", "standard_openai"
					if tc.conflict {
						status, diagnostic = "conflict", "image_output_text_partition_mismatch"
					}
					if adapter != "standard" {
						wantAdapter = adapter
					}
					if event.UsageStatus != status || event.CostIncomplete != tc.conflict || event.UsageDiagnostic != diagnostic || event.UsageAdapter != wantAdapter || event.Path != path || event.StatusCode != 200 || event.InputTokens != 8 || event.OutputTokens != 25 || event.ReasoningTokens != tc.reasoning || event.ImageOutputTokens != tc.image {
						t.Fatalf("persisted partition: %+v", event)
					}
					code, aggregate := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
					cost, ok := dig(decode(t, aggregate), "totals/estimated_cost").(float64)
					wantCost := rates.CostFor(8, 25, 0, 0, 0, 0, 0, 0, 0, tc.image)
					if code != 200 || !ok || math.Abs(cost-wantCost) > 1e-12 {
						t.Fatalf("cost=%v want=%v status=%d", cost, wantCost, code)
					}
				})
			}
		}
	}
}

// Use the existing raw-body scenario for Images/cache counterexamples instead
// of teaching the fake a new image/cache partition language.
func TestOpenAIImageAndCachePartitionGuardsEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name, details, diagnostic string
		image                     int64
	}{
		{"image-derived", `{"text_tokens":1}`, "", 24},
		{"image-included-overflow", `{"text_tokens":1,"image_tokens":24,"reasoning_tokens":24}`, "image_output_text_partition_mismatch", 24},
		{"image-derived-overflow", `{"text_tokens":1,"reasoning_tokens":24}`, "image_output_text_partition_mismatch", 24},
		{"image-disjoint", `{"text_tokens":1,"image_tokens":4,"reasoning_tokens":20}`, "image_output_text_partition_mismatch", 4},
		{"cached-mismatch", `{"text_tokens":1,"reasoning_tokens":24}`, "cached_image_text_partition_mismatch", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, endpointType := "/v1/images/generations", "openai_image"
			payload := `{"usage":{"input_tokens":8,"output_tokens":25,"output_tokens_details":` + tc.details + `}}`
			if tc.name == "cached-mismatch" {
				path, endpointType = "/v1/chat/completions", "openai"
				payload = `{"choices":[{"index":0,"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":25,"prompt_tokens_details":{"text_tokens":60,"image_tokens":40,"cached_tokens":10,"cached_tokens_details":{"text_tokens":7,"image_tokens":5}},"completion_tokens_details":` + tc.details + `}}`
			}
			upKey, alias := uniq("sk-partition-guard"), uniq("partition-guard")
			w.fakeA.SetKeyScenario(upKey, &fakellm.Scenario{Status: 200, Body: payload})
			t.Cleanup(func() { w.fakeA.SetKeyScenario(upKey, nil) })
			endpoint := w.urlA + "/v1"
			if endpointType == "openai_image" {
				endpoint = w.urlA + path
			}
			tg := target(t, map[string]any{"endpoint_type": endpointType, "usage_adapter": "standard", "endpoint": endpoint,
				"api_key": upKey, "model_mappings": []any{map[string]any{"upstream": "partition-model", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			rates := openAIPartitionPrice(t, tg)
			resp, raw := call(t, key, "POST", path, map[string]any{"model": alias, "prompt": "synthetic partition", "messages": []any{}}, nil)
			if resp.StatusCode != 200 || string(raw) != payload {
				t.Fatalf("wire changed: %d %s", resp.StatusCode, raw)
			}
			events := compressionEvents(t, name, 1)
			if len(events) != 1 {
				t.Fatalf("events=%d", len(events))
			}
			event := events[0]
			wantStatus, in, cached := "complete", int64(8), int64(0)
			if tc.diagnostic != "" {
				wantStatus = "conflict"
			}
			if tc.name == "cached-mismatch" {
				in, cached = 90, 10
				if event.ImageInputTokens != 35 || event.CachedImageInputTokens != 5 {
					t.Fatalf("cache split changed: %+v", event)
				}
			}
			if event.InputTokens != in || event.OutputTokens != 25 || event.CachedTokens != cached || event.ImageOutputTokens != tc.image || event.UsageStatus != wantStatus || event.UsageDiagnostic != tc.diagnostic || event.CostIncomplete != (tc.diagnostic != "") {
				t.Fatalf("partition guard: %+v", event)
			}
			code, aggregate := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
			cost, ok := dig(decode(t, aggregate), "totals/estimated_cost").(float64)
			want := rates.CostFor(in, 25, cached, 0, 0, 0, 0, event.ImageInputTokens, event.CachedImageInputTokens, tc.image)
			if code != 200 || !ok || math.Abs(cost-want) > 1e-12 {
				t.Fatalf("cost=%v want=%v status=%d", cost, want, code)
			}
		})
	}
}

func openAIPartitionPrice(t *testing.T, targetName string) usage.CostRates {
	t.Helper()
	imageIn, cachedImage, imageOut := 10.0, 3.0, 40.0
	rates := usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 10, CacheReadPer1MToken: 0.5,
		ImageInputPer1MTokens: &imageIn, CachedImageInputPer1MTokens: &cachedImage, ImageOutputPer1MTokens: &imageOut}
	code, raw := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{"target": targetName, "model": "partition-model", "mode": "flat", "base_rates": rates})
	id, _ := dig(decode(t, raw), "template/id").(string)
	if code != 201 || id == "" {
		t.Fatalf("price: %d %s", code, raw)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+id, nil) })
	return rates
}
