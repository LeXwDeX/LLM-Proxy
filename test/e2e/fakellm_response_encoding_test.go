//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/klauspost/compress/zstd"
)

type encodedWire struct {
	body    []byte
	headers http.Header
}
type encodingCaptureWriter struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (w *encodingCaptureWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	_, _ = w.body.Write(p[:n])
	return n, err
}
func (w *encodingCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func compressionUpstream(t *testing.T) (*fakellm.Server, *httptest.Server, <-chan encodedWire, *atomic.Int64) {
	t.Helper()
	fake := fakellm.New(fakellm.Options{Name: "encoding-e2e"})
	wires := make(chan encodedWire, 8)
	requests := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		capture := &encodingCaptureWriter{ResponseWriter: w}
		defer func() {
			wires <- encodedWire{body: append([]byte(nil), capture.body.Bytes()...), headers: capture.Header().Clone()}
		}()
		fake.ServeHTTP(capture, r)
	}))
	t.Cleanup(server.Close)
	return fake, server, wires, requests
}
func decodeCompression(encoding string, reader io.Reader) (io.ReadCloser, error) {
	switch encoding {
	case "gzip":
		return gzip.NewReader(reader)
	case "deflate":
		return zlib.NewReader(reader)
	case "zstd":
		d, err := zstd.NewReader(reader)
		if err != nil {
			return nil, err
		}
		return compressionZstdReader{d}, nil
	default:
		return io.NopCloser(reader), nil
	}
}

type compressionZstdReader struct{ *zstd.Decoder }

func (r compressionZstdReader) Close() error { r.Decoder.Close(); return nil }

func compressionEvents(t *testing.T, name string, want int) []usage.Event {
	t.Helper()
	usageFor(t, name, want)
	q := url.Values{"from": {time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)}, "to": {time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}, "limit": {"1000"}}
	code, raw := w.adminDo("GET", "/admin/data/usage/events?"+q.Encode(), nil)
	if code != 200 {
		t.Fatalf("events status=%d", code)
	}
	var page struct {
		Events []usage.Event `json:"events"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	var events []usage.Event
	for _, event := range page.Events {
		if event.ClientName == name {
			events = append(events, event)
		}
	}
	return events
}
func compressionPrice(t *testing.T, targetName, model string) usage.CostRates {
	t.Helper()
	rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 0.5}
	code, raw := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{"target": targetName, "model": model, "mode": "flat", "base_rates": rates})
	if code != 201 {
		t.Fatalf("price status=%d body=%s", code, raw)
	}
	var page struct {
		Template struct {
			ID string `json:"id"`
		} `json:"template"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Template.ID == "" {
		t.Fatal("price ID missing")
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+page.Template.ID, nil) })
	return rates
}

func TestFakeCompressionUsageBillingAndWire(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "claude"} {
		for _, encoding := range []string{"identity", "gzip", "deflate", "zstd"} {
			for _, stream := range []bool{false, true} {
				adapters := []string{"standard"}
				if protocol == "chat" {
					adapters = append(adapters, "deepseek")
				}
				for _, adapter := range adapters {
					t.Run(fmt.Sprintf("%s/%s/stream=%v/%s", protocol, encoding, stream, adapter), func(t *testing.T) {
						_, up, wires, requests := compressionUpstream(t)
						alias := uniq("encoded")
						endpointType, path := "openai", "/v1/chat/completions"
						makeBody := chatBody
						if protocol == "responses" {
							path = "/v1/responses"
							makeBody = responsesBody
						}
						if protocol == "claude" {
							endpointType = "claude"
							path = "/v1/messages"
							makeBody = messagesBody
						}
						tg := target(t, map[string]any{"endpoint_type": endpointType, "usage_adapter": adapter, "endpoint": up.URL + "/v1", "api_key": uniq("sk-encoding"), "supports_responses": true, "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
						name, key := client(t, []string{tg}, nil)
						rates := compressionPrice(t, tg, "unfamiliar-upstream")
						directive := "[[fake encoding=" + encoding + " pricing=standard usage.in=100 usage.out=20 usage.cached=30]]"
						if protocol == "chat" {
							directive = "[[fake flavor=deepseek encoding=" + encoding + " pricing=standard usage.in=100 usage.out=20 usage.cached=30]]"
						}
						body := expand(makeBody(directive, stream), alias).(map[string]any)
						body["service_tier"] = "fast"
						resp, raw := call(t, key, "POST", path, body, map[string]string{"Accept-Encoding": "identity"})
						wire := <-wires
						if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != encoding || !bytes.Equal(raw, wire.body) || !reflect.DeepEqual(resp.Header.Values("Content-Encoding"), wire.headers.Values("Content-Encoding")) {
							t.Fatalf("wire changed: status=%d encoding=%q bytes=%d/%d", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(raw), len(wire.body))
						}
						reader, err := decodeCompression(encoding, bytes.NewReader(raw))
						if err != nil {
							t.Fatal(err)
						}
						plain, err := io.ReadAll(reader)
						_ = reader.Close()
						if err != nil || !bytes.Contains(plain, []byte("fake reply")) {
							t.Fatalf("decode: %v %s", err, plain)
						}
						events := compressionEvents(t, name, 1)
						if len(events) != 1 || requests.Load() != 1 {
							t.Fatalf("duplicate request/events: %d/%d", requests.Load(), len(events))
						}
						event := events[0]
						c := event.PricingContext
						expectedAdapter := "standard_openai"
						if protocol == "claude" {
							expectedAdapter = "standard_anthropic"
						}
						if adapter == "deepseek" {
							expectedAdapter = "deepseek"
						}
						if event.InputTokens != 70 || event.OutputTokens != 20 || event.CachedTokens != 30 || event.UsageAdapter != expectedAdapter || event.UsageStatus != "complete" || event.CostIncomplete || c == nil || c.TotalInputTokens != 100 || !c.TotalInputValid || c.ActualServiceTier != "standard" || c.RequestServiceTier.Raw != "fast" {
							t.Fatalf("persisted event %+v context=%+v", event, c)
						}
						if protocol == "claude" && c.ActualSpeed != "standard" {
							t.Fatalf("Claude speed=%q", c.ActualSpeed)
						}
						code, aggregate := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(name), nil)
						cost, ok := dig(decode(t, aggregate), "totals/estimated_cost").(float64)
						if code != 200 || !ok || math.Abs(cost-rates.CostFor(70, 20, 30, 0)) > 1e-12 {
							t.Fatalf("cost=%v status=%d", cost, code)
						}
					})
				}
			}
		}
	}
}

func TestFakeCompressionInvalidObservationPreservesWire(t *testing.T) {
	cases := []struct {
		name, encoding, fault string
		headers               []string
		stream                bool
	}{
		{"unknown", "gzip", "", []string{"x-unknown"}, false}, {"gzip_labeled_br", "gzip", "", []string{"br"}, false}, {"stacked", "gzip", "", []string{"gzip, identity"}, false}, {"multiple", "gzip", "", []string{"gzip", "identity"}, false}, {"missing", "gzip", "", []string{}, false},
	}
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		for _, fault := range []string{"corrupt", "truncated"} {
			for _, stream := range []bool{false, true} {
				cases = append(cases, struct {
					name, encoding, fault string
					headers               []string
					stream                bool
				}{encoding + "/" + fault + fmt.Sprint(stream), encoding, fault, nil, stream})
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, up, wires, requests := compressionUpstream(t)
			scenario := map[string]any{"response_encoding": tc.encoding, "response_encoding_fault": tc.fault}
			if tc.headers != nil {
				scenario["response_encoding_headers"] = tc.headers
			}
			control, _ := http.NewRequest("POST", up.URL+"/_control", bytes.NewReader(mustJSON(map[string]any{"scenario": scenario})))
			control.Header.Set("Content-Type", "application/json")
			controlResp, err := http.DefaultClient.Do(control)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, controlResp.Body)
			controlResp.Body.Close()
			<-wires
			requests.Store(0)
			_ = fake
			alias := uniq("faulted-encoding")
			tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-fault"), "model_mappings": []any{map[string]any{"upstream": "unknown", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			resp, raw := call(t, key, "POST", "/v1/chat/completions", expand(chatBody("[[fake pricing=standard usage.in=100 usage.out=20]]", tc.stream), alias), map[string]string{"Accept-Encoding": "identity"})
			wire := <-wires
			if resp.StatusCode != 200 || !bytes.Equal(raw, wire.body) || !reflect.DeepEqual(resp.Header.Values("Content-Encoding"), wire.headers.Values("Content-Encoding")) {
				t.Fatal("faulted wire changed")
			}
			events := compressionEvents(t, name, 1)
			if len(events) != 1 || requests.Load() != 1 || !events[0].CostIncomplete || events[0].UsageStatus == "complete" || events[0].UsageDiagnostic == "" {
				t.Fatalf("observation failure replayed or billed complete: requests=%d events=%+v", requests.Load(), events)
			}
		})
	}
}

func TestFakeCompressionCancellationDoesNotReplay(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			_, up, _, requests := compressionUpstream(t)
			alias := uniq("cancel-encoding")
			tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-cancel"), "model_mappings": []any{map[string]any{"upstream": "unknown", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body := expand(chatBody("[[fake encoding="+encoding+" chunks=20 chunk_delay_ms=200]]", true), alias)
			req, _ := http.NewRequestWithContext(ctx, "POST", w.base+"/v1/chat/completions", bytes.NewReader(mustJSON(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Accept-Encoding", "identity")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader, err := decodeCompression(encoding, resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if line, err := bufio.NewReader(reader).ReadString('\n'); err != nil || !strings.HasPrefix(line, "data:") {
				t.Fatalf("first event %q %v", line, err)
			}
			cancel()
			resp.Body.Close()
			events := compressionEvents(t, name, 1)
			if len(events) != 1 || requests.Load() != 1 || !events[0].CostIncomplete {
				t.Fatalf("cancel requests=%d events=%+v", requests.Load(), events)
			}
			// No official final usage before the disconnect: zero tokens + diagnostic.
			if e := events[0]; !strings.Contains(e.UsageDiagnostic, "client_disconnected_no_usage") || e.InputTokens != 0 || e.OutputTokens != 0 || e.UsageStatus != "incomplete" {
				t.Fatalf("client-gone settlement missing: %+v", e)
			}
		})
	}
}

func TestFakeCompressionHeaderSpellingPreserved(t *testing.T) {
	_, up, wires, requests := compressionUpstream(t)
	control, _ := http.NewRequest("POST", up.URL+"/_control", bytes.NewReader(mustJSON(map[string]any{"scenario": map[string]any{"response_encoding": "gzip", "response_encoding_headers": []string{" \tGZiP\t "}}})))
	resp, err := http.DefaultClient.Do(control)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	<-wires
	requests.Store(0)
	alias := uniq("encoding-header")
	tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-header"), "model_mappings": []any{map[string]any{"upstream": "unknown", "fallback": alias}}})
	name, key := client(t, []string{tg}, nil)
	resp, raw := call(t, key, "POST", "/v1/chat/completions", expand(chatBody("[[fake pricing=standard usage.in=100 usage.out=20]]", false), alias), map[string]string{"Accept-Encoding": "identity"})
	wire := <-wires
	// net/http removes surrounding optional whitespace while parsing the wire
	// header. The proxy retains the remaining spelling rather than normalizing it.
	if resp.Header.Get("Content-Encoding") != "GZiP" || !bytes.Equal(raw, wire.body) {
		t.Fatal("encoding spelling/body changed")
	}
	events := compressionEvents(t, name, 1)
	if len(events) != 1 || requests.Load() != 1 || events[0].InputTokens != 100 || events[0].CostIncomplete {
		t.Fatalf("case/whitespace observation failed %+v", events)
	}
}

func TestFakeCompressionTransportCutDoesNotRepairOrReplay(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			_, up, wires, requests := compressionUpstream(t)
			alias := uniq("cut-encoding")
			tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-cut"), "model_mappings": []any{map[string]any{"upstream": "unknown", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			body := expand(chatBody("[[fake encoding="+encoding+" cut_after=2]]", true), alias)
			req, _ := http.NewRequest("POST", w.base+"/v1/chat/completions", bytes.NewReader(mustJSON(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Accept-Encoding", "identity")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			wire := <-wires
			if resp.StatusCode != 200 || !bytes.Equal(raw, wire.body) || resp.Header.Get("Content-Encoding") != encoding {
				t.Fatal("transport cut wire changed")
			}
			reader, err := decodeCompression(encoding, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			_, decodeErr := io.ReadAll(reader)
			reader.Close()
			if decodeErr == nil {
				t.Fatal("transport cut was repaired into complete encoded stream")
			}
			events := compressionEvents(t, name, 1)
			if len(events) != 1 || requests.Load() != 1 || !events[0].CostIncomplete || events[0].UsageStatus == "complete" {
				t.Fatalf("cut repeated/completed requests=%d events=%+v", requests.Load(), events)
			}
		})
	}
}

// A client offering br must not be able to exhaust the shared br observation
// budget: observed requests reach the upstream without br, the remaining
// codings unchanged, and the response bytes, client decoding and usage stay
// exact.
func TestFakeCompressionUpstreamNeverOfferedBrotli(t *testing.T) {
	fake := fakellm.New(fakellm.Options{Name: "no-br-e2e"})
	offered := make(chan string, 4)
	wires := make(chan []byte, 4)
	up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		offered <- strings.Join(r.Header.Values("Accept-Encoding"), "|")
		capture := &encodingCaptureWriter{ResponseWriter: rw}
		fake.ServeHTTP(capture, r)
		wires <- append([]byte(nil), capture.body.Bytes()...)
	}))
	t.Cleanup(up.Close)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			alias := uniq("no-br")
			tg := target(t, map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": up.URL + "/v1", "api_key": uniq("sk-no-br"), "model_mappings": []any{map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias}}})
			name, key := client(t, []string{tg}, nil)
			body := expand(chatBody("[[fake encoding=gzip pricing=standard usage.in=100 usage.out=20]]", stream), alias)
			resp, raw := call(t, key, "POST", "/v1/chat/completions", body, map[string]string{"Accept-Encoding": "br, gzip"})
			if got := <-offered; got != "gzip" {
				t.Fatalf("upstream offered %q, want gzip", got)
			}
			wire := <-wires
			if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(raw, wire) {
				t.Fatalf("wire changed: status=%d encoding=%q bytes=%d/%d", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(raw), len(wire))
			}
			reader, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			plain, err := io.ReadAll(reader)
			if err != nil || !bytes.Contains(plain, []byte("fake reply")) {
				t.Fatalf("decode: %v %s", err, plain)
			}
			events := compressionEvents(t, name, 1)
			if len(events) != 1 || events[0].InputTokens != 100 || events[0].OutputTokens != 20 || events[0].UsageStatus != "complete" || events[0].CostIncomplete {
				t.Fatalf("usage events %+v", events)
			}
		})
	}
}
