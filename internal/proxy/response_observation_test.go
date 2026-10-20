package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/andybalholm/brotli"
)

func TestDeepSeekEncodingProbe(t *testing.T) {
	for _, adapter := range []string{"standard", "deepseek"} {
		for _, stream := range []bool{false, true} {
			for _, encoding := range []string{"identity", "gzip", "deflate", "zstd", "br"} {
				for _, hasUsage := range []bool{true, false} {
					name := adapter + "/" + encoding + "/json"
					if stream {
						name = adapter + "/" + encoding + "/sse"
					}
					if !hasUsage {
						name += "/no_usage"
					}
					t.Run(name, func(t *testing.T) {
						body := `{"model":"deepseek-chat","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}}`
						if !hasUsage {
							body = `{"model":"deepseek-chat","choices":[{"finish_reason":"stop"}]}`
						}
						ct := "application/json"
						if stream {
							ct = "text/event-stream"
							body = "data: " + body + "\n\ndata: [DONE]\n\n"
						}
						wire := []byte(body)
						var b bytes.Buffer
						switch encoding {
						case "gzip":
							w := gzip.NewWriter(&b)
							_, _ = w.Write(wire)
							_ = w.Close()
							wire = b.Bytes()
						case "deflate":
							w := zlib.NewWriter(&b)
							_, _ = w.Write(wire)
							_ = w.Close()
							wire = b.Bytes()
						case "br":
							w := brotli.NewWriter(&b)
							_, _ = w.Write(wire)
							_ = w.Close()
							wire = b.Bytes()
						}
						if encoding == "zstd" {
							fixture := "zstd-json"
							if stream {
								fixture = "zstd-sse"
							}
							if !hasUsage {
								fixture += "-no-usage"
							}
							var err error
							wire, err = base64.StdEncoding.DecodeString(map[string]string{"zstd-json": "KLUv/QRYpQMAokcYGYCpOktxM0FphK0U/USojzGCGQ7Ek9IYYSNJYgi7uQap0BUmLEbANN2PthRdjAkcL7uhDrm2FGl+x0ekTbgYEjd162Omm44gtcny4Fc43mOqQhe7pBBg/Nv+wsfHC11+AQUAQRUEFLpYbe4UGXOLaQY2rg/G", "zstd-json-no-usage": "KLUv/QRY8QEAeyJtb2RlbCI6ImRlZXBzZWVrLWNoYXQiLCJjaG9pY2VzIjpbeyJmaW5pc2hfcmVhc29uIjoic3RvcCJ9XX2WXTJs", "zstd-sse-no-usage": "KLUv/QRYoQIAZGF0YTogeyJtb2RlbCI6ImRlZXBzZWVrLWNoYXQiLCJjaG9pY2VzIjpbeyJmaW5pc2hfcmVhc29uIjoic3RvcCJ9XX0KCmRhdGE6IFtET05FXQoKrotsdA==", "zstd-sse": "KLUv/QRYTQQAosgcHmBFqwMAOIgZShMT2YGCEGIN4xANivegeBROdEQJlOcciITiYJzTNMnQ3VyhMbVs6UKJ8LjFVaDjdZTSIN9vE44qQscLPJNfAO2Ao6M55NjPP27dRbFury3OGPne1Zito0dlS5DsNtxFnlcWYXcmkBoVCQYAOTNKUAUBhS5WmztFht1imgHXWeMR"}[fixture])
							if err != nil {
								t.Fatal(err)
							}
						}
						up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", ct)
							w.Header().Set("Content-Encoding", encoding)
							w.Header().Set("X-Upstream-Fixture", "unchanged")
							_, _ = w.Write(wire)
						}))
						defer up.Close()
						svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: adapter, Endpoint: up.URL, APIKey: "fixture"}}}, newTestLogger())
						if err != nil {
							t.Fatal(err)
						}
						rec := &globalAuditRecorder{}
						svc.SetUsageRecorder(rec)
						requestBody := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`
						if stream {
							requestBody = `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
						}
						req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(requestBody))
						req.Header.Set("Accept-Encoding", encoding)
						req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
						rr := httptest.NewRecorder()
						svc.ServeHTTP(rr, req)
						if rr.Code != 200 {
							t.Fatalf("HTTP %d", rr.Code)
						}
						if !bytes.Equal(rr.Body.Bytes(), wire) {
							t.Errorf("wire bytes changed")
						}
						if rr.Header().Get("Content-Encoding") != encoding {
							t.Errorf("encoding header changed")
						}
						if rr.Header().Get("Content-Type") != ct || rr.Header().Get("X-Upstream-Fixture") != "unchanged" {
							t.Error("upstream headers changed")
						}
						if len(rec.events) != 1 {
							t.Fatalf("events=%d", len(rec.events))
						}
						e := rec.events[0]
						wantStatus := "complete"
						if !hasUsage {
							wantStatus = "incomplete"
						}
						if e.UsageStatus != wantStatus {
							t.Errorf("usage status got %s want %s", e.UsageStatus, wantStatus)
						}
						t.Logf("input=%d output=%d cached=%d usage_status=%s wire_equal=%v", e.InputTokens, e.OutputTokens, e.CachedTokens, e.UsageStatus, bytes.Equal(rr.Body.Bytes(), wire))
						wantInput, wantOutput, wantCached := int64(100), int64(20), int64(0)
						if adapter == "deepseek" {
							wantInput = 70
							wantCached = 30
						}
						if !hasUsage {
							wantInput = 0
							wantOutput = 0
							wantCached = 0
						}
						if e.InputTokens != wantInput || e.OutputTokens != wantOutput || e.CachedTokens != wantCached {
							t.Errorf("tokens got %d/%d/%d want %d/%d/%d", e.InputTokens, e.OutputTokens, e.CachedTokens, wantInput, wantOutput, wantCached)
						}
					})
				}
			}
		}
	}
}
