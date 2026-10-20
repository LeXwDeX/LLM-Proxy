// perf_body_benchmark_test.go — 请求体与用量观察热路径的性能回归基线。
// 与 perf_regression_test.go 并列：后者守护行为边界，本文件以 benchmark 记录
// 1 MB / 4 MB 请求体在模型提取、出站组装、用量解析器构造和响应观察上的耗时与分配。
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

var perfBodySizes = []struct {
	name string
	size int
}{{"1MB", 1 << 20}, {"4MB", 4 << 20}}

// perfChatBody builds a realistic Chat request: many messages with escapes,
// unicode and nested tool arguments, with the routing members at both ends.
func perfChatBody(size int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"qwen-max","stream":true,"service_tier":"default","messages":[`)
	chunk := `{"role":"user","content":"第 %d 段：explain \"alias\" model\\n with <tags> & 数字 9007199254740993 ` + strings.Repeat("lorem ipsum ", 40) + `"},`
	for i := 0; b.Len() < size-256; i++ {
		fmt.Fprintf(&b, chunk, i)
	}
	b.WriteString(`{"role":"assistant","content":"done","tool_calls":[{"function":{"arguments":"{\"model\":\"qwen-max\"}"}}]}]`)
	b.WriteString(`,"temperature":0.2,"seed":9007199254740993}`)
	return b.Bytes()
}

func BenchmarkPerfBodyExtractModel(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Content-Type", "application/json")
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if extractModel(r, body) != "qwen-max" {
					b.Fatal("model lost")
				}
			}
		})
	}
}

func BenchmarkPerfBodyPrepareTargetBody(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		target := &Target{RequestStreamUsageIfMissing: true, allowedModelIdx: map[string]string{"qwen-max": "QWEN3.8-MAX"}}
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Content-Type", "application/json")
				got := prepareTargetBody(target, r, body, "qwen-max", "QWEN3.8-MAX")
				if !bytes.HasPrefix(got, []byte(`{"model":"QWEN3.8-MAX"`)) || len(got) <= len(body) {
					b.Fatal("assembly did not rewrite")
				}
			}
		})
	}
}

// The pre-optimization assembly (re-marshal per edit), kept as the baseline.
func BenchmarkPerfBodyPrepareTargetBodyLegacy(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				got := requestStreamUsageIfMissingLegacy(replaceModelInBodyLegacy(body, "QWEN3.8-MAX", "qwen-max"))
				if len(got) <= len(body)-1024 {
					b.Fatal("assembly did not rewrite")
				}
			}
		})
	}
}

func BenchmarkPerfBodyExtractModelLegacy(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if extractModelLegacyJSON(body) != "qwen-max" {
					b.Fatal("model lost")
				}
			}
		})
	}
}

func BenchmarkPerfBodyNewTargetUsageParser(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		target := &Target{UsageAdapter: "standard"}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Content-Type", "application/json")
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				outbound, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/chat/completions", bytes.NewReader(body))
				outbound.Header = r.Header.Clone()
				p := newTargetUsageParser(r, target, "qwen-max", body, outbound)
				if p.pricingRequestFields["service_tier"] != "default" {
					b.Fatal("pricing evidence lost")
				}
			}
		})
	}
}

// newTargetUsageParserLegacy is the pre-optimization constructor: two header
// clones, a GetBody re-read of the whole body and a synchronous projection.
func newTargetUsageParserLegacy(r *http.Request, target *Target, model string, body []byte, outbound *http.Request) usageParser {
	p := usageParser{endpointType: targetEndpointType(target), model: model, requestBody: body}
	if r != nil {
		p.protocol = ResolveProtocol(r.URL.Path)
		p.requestHeaders = r.Header.Clone()
	}
	if target != nil {
		p.endpoint = target.Endpoint
		p.usageAdapter = target.UsageAdapter
	}
	if outbound != nil {
		p.requestHeaders = outbound.Header.Clone()
		if outbound.GetBody != nil {
			if reader, err := outbound.GetBody(); err == nil {
				if actual, err := io.ReadAll(reader); err == nil {
					p.requestBody = actual
				}
				_ = reader.Close()
			}
		}
	}
	return p.capturePricingRequest()
}

func BenchmarkPerfBodyNewTargetUsageParserLegacy(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		target := &Target{UsageAdapter: "standard"}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Content-Type", "application/json")
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				outbound, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/chat/completions", bytes.NewReader(body))
				outbound.Header = r.Header.Clone()
				p := newTargetUsageParserLegacy(r, target, "qwen-max", body, outbound)
				if p.pricingRequestFields["service_tier"] != "default" {
					b.Fatal("pricing evidence lost")
				}
			}
		})
	}
}

// TTFT critical path: the projection runs while the request is in flight
// (excluded from the timer); only constructing the parser and joining the
// finished future is measured, as in writeResponse after response headers.
func BenchmarkPerfBodyNewTargetUsageParserJoined(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		target := &Target{UsageAdapter: "standard"}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Content-Type", "application/json")
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				ctx := startPricingRequest(context.Background(), body)
				outbound, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.invalid/v1/chat/completions", bytes.NewReader(body))
				outbound.Header = r.Header.Clone()
				<-pricingRequestFutureFrom(ctx).done // upstream latency
				b.StartTimer()
				p := newTargetUsageParser(r, target, "qwen-max", body, outbound)
				if p.pricingRequestFields["service_tier"] != "default" {
					b.Fatal("pricing evidence lost")
				}
			}
		})
	}
}

func perfSSEWire(events int) []byte {
	var b bytes.Buffer
	for i := 0; i < events; i++ {
		fmt.Fprintf(&b, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"token %d\"}}]}\n\n", i)
	}
	b.WriteString("data: {\"id\":\"c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2000}}\n\ndata: [DONE]\n\n")
	return b.Bytes()
}

func BenchmarkPerfBodyUsageStreamSSE(b *testing.B) {
	wire := perfSSEWire(2000)
	b.SetBytes(int64(len(wire)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		stream := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIChat})
		for offset := 0; offset < len(wire); offset += 512 {
			end := min(offset+512, len(wire))
			_, _ = stream.Write(wire[offset:end])
		}
		stream.Close()
		if !stream.found || stream.tokens.OutputTokens != 2000 {
			b.Fatal("usage lost")
		}
	}
}

type perfBodyTransport struct{ wire []byte }

func (t *perfBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(t.wire)), Request: r}, nil
}

// End-to-end ServeHTTP: model extraction, assembly, parser construction,
// response observation and capture for a large streaming Chat request.
func BenchmarkPerfBodyServeHTTP(b *testing.B) {
	for _, size := range perfBodySizes {
		body := perfChatBody(size.size)
		b.Run(size.name, func(b *testing.B) {
			s, err := NewService(&config.Config{
				Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 30},
				Targets: []config.Target{{Name: "bench", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://upstream.invalid", APIKey: "fixture", UsageAdapter: "standard", RequestStreamUsageIfMissing: true, ModelMappings: []config.ModelMapping{{Upstream: "QWEN3.8-MAX", Fallback: "qwen-max"}}}},
			}, newTestLogger())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			s.httpClient = &http.Client{Transport: &perfBodyTransport{wire: perfSSEWire(200)}}
			store := auth.NewStore()
			if err := store.LoadFromConfig(testAuthClients("bench", "bench-token")); err != nil {
				b.Fatal(err)
			}
			principal, _ := store.Authenticate("bench-token")
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					b.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			}
		})
	}
}
