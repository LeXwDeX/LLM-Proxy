//go:build e2e

package e2e

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// #178: exercise the real binary, persisted events and pricing with compressed
// responses. These synthetic zstd fixtures came from the reference zstd CLI;
// neither the fixture content nor the compressed bytes come from a user trace.
func TestCompressedResponseUsageAndBilling(t *testing.T) {
	const payload = `{"model":"deepseek-chat","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}}`
	fixtures := []struct {
		name, contentType, plain, zstd string
		stream                         bool
	}{
		{"json", "application/json", payload, "KLUv/QRYpQMAokcYGYCpOktxM0FphK0U/USojzGCGQ7Ek9IYYSNJYgi7uQap0BUmLEbANN2PthRdjAkcL7uhDrm2FGl+x0ekTbgYEjd162Omm44gtcny4Fc43mOqQhe7pBBg/Nv+wsfHC11+AQUAQRUEFLpYbe4UGXOLaQY2rg/G", false},
		{"sse", "text/event-stream", "data: " + payload + "\n\ndata: [DONE]\n\n", "KLUv/QRYTQQAosgcHmBFqwMAOIgZShMT2YGCEGIN4xANivegeBROdEQJlOcciITiYJzTNMnQ3VyhMbVs6UKJ8LjFVaDjdZTSIN9vE44qQscLPJNfAO2Ao6M55NjPP27dRbFury3OGPne1Zito0dlS5DsNtxFnlcWYXcmkBoVCQYAOTNKUAUBhS5WmztFht1imgHXWeMR", true},
	}
	for _, fixture := range fixtures {
		for _, encoding := range []string{"zstd", "deflate"} {
			for _, adapter := range []string{"standard", "deepseek"} {
				t.Run(fixture.name+"/"+encoding+"/"+adapter, func(t *testing.T) {
					wire, err := base64.StdEncoding.DecodeString(fixture.zstd)
					if err != nil {
						t.Fatal(err)
					}
					if encoding == "deflate" {
						var compressed bytes.Buffer
						writer := zlib.NewWriter(&compressed)
						if _, err := writer.Write([]byte(fixture.plain)); err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
						wire = compressed.Bytes()
					}
					var requests atomic.Int64
					up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						if r.Header.Get("Accept-Encoding") != encoding {
							t.Errorf("outbound compression preference changed: %q", r.Header.Get("Accept-Encoding"))
						}
						rw.Header().Set("Content-Type", fixture.contentType)
						rw.Header().Set("Content-Encoding", encoding)
						rw.Header().Set("Content-Length", strconv.Itoa(len(wire)))
						_, _ = rw.Write(wire)
					}))
					defer up.Close()
					alias := uniq("compressed-usage")
					tg := target(t, map[string]any{
						"endpoint_type": "openai", "usage_adapter": adapter, "endpoint": up.URL + "/v1", "api_key": uniq("sk-compressed"),
						"model_mappings": []any{map[string]any{"upstream": "deepseek-chat", "fallback": alias}},
					})
					name, key := client(t, []string{tg}, nil)
					rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 0.5}
					code, body := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{"target": tg, "model": "deepseek-chat", "mode": "flat", "base_rates": rates})
					if code != http.StatusCreated {
						t.Fatalf("create price: %d %s", code, body)
					}
					var price struct {
						Template struct {
							ID string `json:"id"`
						} `json:"template"`
					}
					if err := json.Unmarshal(body, &price); err != nil || price.Template.ID == "" {
						t.Fatalf("price response missing ID: %v", err)
					}
					t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+price.Template.ID, nil) })
					request := map[string]any{"model": alias, "stream": fixture.stream, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}}
					if fixture.stream {
						request["stream_options"] = map[string]any{"include_usage": true}
					}
					resp, raw := call(t, key, "POST", "/v1/chat/completions", request, map[string]string{"Accept-Encoding": encoding})
					if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != encoding || resp.Header.Get("Content-Length") != fmt.Sprint(len(wire)) || !bytes.Equal(raw, wire) {
						t.Fatalf("compressed response changed: status=%d encoding=%q bytes=%d", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(raw))
					}
					events := usageFor(t, name, 1)
					if len(events) != 1 || requests.Load() != 1 {
						t.Fatalf("request replayed or accounted twice: requests=%d events=%d", requests.Load(), len(events))
					}
					input, cached := int64(100), int64(0)
					wantAdapter := "standard_openai"
					if adapter == "deepseek" {
						input, cached = 70, 30
						wantAdapter = "deepseek"
					}
					ev := events[0]
					if ev.InputTokens != input || ev.OutputTokens != 20 || ev.CachedTokens != cached || ev.UsageAdapter != wantAdapter || ev.UsageStatus != "complete" || ev.CostIncomplete {
						t.Fatalf("compressed usage was lost or patched implicitly: %+v", ev)
					}
					code, body = w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
					cost, ok := dig(decode(t, body), "totals/estimated_cost").(float64)
					want := rates.CostFor(input, 20, cached, 0)
					if code != http.StatusOK || !ok || math.Abs(cost-want) > 1e-12 {
						t.Fatalf("compressed cost %.12f, want %.12f (status %d)", cost, want, code)
					}
				})
			}
		}
	}
}
