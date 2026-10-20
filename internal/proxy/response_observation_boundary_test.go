package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func observationFixture(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&b)
	case "deflate":
		w = zlib.NewWriter(&b)
	case "br":
		w = brotli.NewWriter(&b)
	case "zstd":
		var err error
		w, err = zstd.NewWriter(&b, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
	default:
		return body
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestObservationDecoderBoundaries(t *testing.T) {
	payload := []byte("data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	for _, encoding := range []string{"", "identity", "gzip", "deflate", "zstd", "br"} {
		t.Run("chunked/"+encoding, func(t *testing.T) {
			wire := observationFixture(t, encoding, payload)
			u := newUsageStream("text/event-stream", " \t"+strings.ToUpper(encoding)+" \t", usageParser{protocol: config.ProtocolOpenAIChat})
			for _, b := range wire {
				_, _ = u.Write([]byte{b})
			}
			u.Close()
			if u.err != nil || u.tokens.InputTokens != 12 || u.tokens.OutputTokens != 3 || u.tokens.status != usageStatusComplete {
				t.Fatalf("tokens=%+v err=%v", u.tokens, u.err)
			}
		})
	}
	first := observationFixture(t, "zstd", []byte("data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n"))
	last := observationFixture(t, "zstd", []byte("data: [DONE]\n\n"))
	u := newUsageStream("text/event-stream", "zstd", usageParser{protocol: config.ProtocolOpenAIChat})
	_, _ = u.Write(append(first, last...))
	u.Close()
	if u.err != nil || u.tokens.InputTokens != 12 || u.tokens.status != usageStatusComplete {
		t.Fatalf("concatenated frames: %+v err=%v", u.tokens, u.err)
	}
	compressed := observationFixture(t, "zstd", payload)
	corrupt := append([]byte(nil), compressed...)
	corrupt[len(corrupt)-1] ^= 0xff
	var dict bytes.Buffer
	dw, err := zstd.NewWriter(&dict, zstd.WithEncoderConcurrency(1), zstd.WithEncoderDictRaw(42, []byte(strings.Repeat("dictionary content ", 100))))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = dw.Write(payload)
	_ = dw.Close()
	brWire := observationFixture(t, "br", payload)
	cases := []struct {
		name, encoding string
		wire           []byte
	}{
		{"invalid", "zstd", []byte("not zstd")}, {"truncated", "zstd", compressed[:len(compressed)-3]},
		{"checksum", "zstd", corrupt}, {"dictionary", "zstd", dict.Bytes()},
		{"window_over_8MiB", "zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x78, 1, 0, 0}},
		{"unknown", "x-unknown", payload}, {"compress", "compress", payload}, {"stacked", "gzip, zstd", payload},
		{"stacked_br", "gzip, br", payload},
		{"br_invalid", "br", payload}, {"br_truncated", "br", brWire[:len(brWire)-3]},
		{"br_trailing_garbage", "br", append(append([]byte(nil), brWire...), "garbage"...)},
		{"br_gzip_mislabeled", "br", observationFixture(t, "gzip", payload)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := observationReader(bytes.NewReader(tc.wire), tc.encoding)
			if err == nil {
				_, err = io.ReadAll(r)
				_ = r.Close()
			}
			if err == nil {
				t.Fatal("invalid observation accepted")
			}
			done := make(chan struct{})
			go func() {
				u := newUsageStream("text/event-stream", tc.encoding, usageParser{protocol: config.ProtocolOpenAIChat})
				_, _ = u.Write(tc.wire)
				u.Close()
				if u.err == nil {
					t.Errorf("invalid stream missing decode error")
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("invalid observation blocked forwarding")
			}
		})
	}
}

func TestObservationTraceReadableBoundedAndPartial(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "zstd", "br"} {
		for _, partial := range []bool{false, true} {
			name := encoding
			if partial {
				name += "/checksum_error"
			}
			t.Run(name, func(t *testing.T) {
				body := []byte(strings.Repeat("readable ", 50))
				wire := observationFixture(t, encoding, body)
				if partial && encoding == "br" {
					// Brotli has no checksum; trailing input is its post-output error.
					wire = append(wire, "garbage"...)
				} else if partial {
					wire[len(wire)-1] ^= 0xff
					if encoding == "zstd" {
						wire = append(observationFixture(t, encoding, body), wire...)
					}
				}
				limit := 32
				if partial {
					limit = 1024
				}
				store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: limit, ChannelBuffer: 1}, newTestLogger())
				defer store.Close()
				s := &Service{traceStore: store}
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				req.Header.Set("Authorization", "private-fixture")
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {encoding}, "Set-Cookie": {"private-fixture"}}}
				s.recordTrace(req, nil, resp, nil, wire, time.Now(), -1, "deepseek-chat", usageTokens{})
				got := store.Get("")
				if got == nil {
					t.Fatal("missing trace")
				}
				want := string(body)
				if !partial {
					want = want[:limit]
				}
				if got.ResponseBody != want {
					t.Fatalf("trace body=%q want=%q", got.ResponseBody, want)
				}
				if got.RequestHeaders["Authorization"] != "***" || got.UpstreamHeaders["Set-Cookie"] != "***" {
					t.Fatal("sensitive headers were not masked")
				}
			})
		}
	}
}

func TestObservationStreamCancellationUnblocks(t *testing.T) {
	reader, writer := io.Pipe()
	u := newUsageStream("text/event-stream", "zstd", usageParser{protocol: config.ProtocolOpenAIChat})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = io.Copy(u, reader); u.Close(); close(done) }()
	go func() { <-ctx.Done(); _ = writer.CloseWithError(ctx.Err()) }()
	// Partial frame followed by cancellation must release observer and pipe.
	_, _ = writer.Write([]byte{0x28, 0xb5, 0x2f, 0xfd})
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled observation hung")
	}
	_ = reader.Close()
}

func TestObservationUsageBeyondTraceLimit(t *testing.T) {
	// Usage follows generated content larger than the trace capture budget.
	body := []byte(`{"model":"deepseek-chat","choices":[{"message":{"content":"` + strings.Repeat("generated text ", 50000) + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}}`)
	wire := observationFixture(t, "zstd", body)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(wire)
	}))
	defer up.Close()
	svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: "deepseek", Endpoint: up.URL, APIKey: "fixture"}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: 64, ChannelBuffer: 1}, newTestLogger())
	defer store.Close()
	svc.traceStore = store
	recorder := &globalAuditRecorder{}
	svc.SetUsageRecorder(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat"}`))
	req.Header.Set("Accept-Encoding", "zstd")
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), wire) {
		t.Fatal("forwarded response changed")
	}
	if len(recorder.events) != 1 {
		t.Fatal("missing event")
	}
	e := recorder.events[0]
	if e.InputTokens != 70 || e.OutputTokens != 20 || e.CachedTokens != 30 || e.UsageStatus != "complete" {
		t.Fatalf("tail usage lost: %+v", e)
	}
	// Request ID may be empty when calling Service directly.
	record := store.Get("")
	if record == nil || len(record.ResponseBody) > 64 {
		t.Fatal("trace was not bounded")
	}
}

func TestObservationZstdOutputExceedsWindowLimit(t *testing.T) {
	body := []byte(strings.Repeat("x", 9<<20))
	wire := observationFixture(t, "zstd", body)
	reader, err := observationReader(bytes.NewReader(wire), "zstd")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(decoded, body) {
		t.Fatalf("window bound incorrectly limits output: bytes=%d err=%v", len(decoded), err)
	}
}

func TestObservationServiceCancellationDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	ready := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte{0x28, 0xb5, 0x2f, 0xfd})
		w.(http.Flusher).Flush()
		close(ready)
		<-r.Context().Done()
	}))
	defer up.Close()
	svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "probe", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: "deepseek", Endpoint: up.URL, APIKeys: []string{"fixture-one", "fixture-two"}}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	recorder := &globalAuditRecorder{}
	svc.SetUsageRecorder(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","stream":true}`))
	req.Header.Set("Accept-Encoding", "zstd")
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture")))
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	done := make(chan struct{})
	forwarded := make(chan struct{})
	rr := &observationNotifyingRecorder{ResponseRecorder: httptest.NewRecorder(), written: forwarded}
	go func() { svc.ServeHTTP(rr, req); close(done) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not started")
	}
	select {
	case <-forwarded:
	case <-time.After(3 * time.Second):
		t.Fatal("response not forwarded")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled service hung")
	}
	if calls.Load() != 1 {
		t.Fatalf("canceled compressed request retried %d times", calls.Load())
	}
	if len(recorder.events) != 1 || recorder.events[0].UsageStatus != "incomplete" {
		t.Fatalf("canceled usage event lost: %+v", recorder.events)
	}
	// The client left before official final usage: zero tokens + diagnostic.
	if e := recorder.events[0]; !strings.Contains(e.UsageDiagnostic, diagnosticClientDisconnectedNoUsage) || e.InputTokens != 0 || e.OutputTokens != 0 || !e.CostIncomplete {
		t.Fatalf("client-gone settlement missing: %+v", e)
	}
}

// Signals cancellation only after ServeHTTP has entered response forwarding.
type observationNotifyingRecorder struct {
	*httptest.ResponseRecorder
	written chan struct{}
	once    sync.Once
}

func (w *observationNotifyingRecorder) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	w.once.Do(func() { close(w.written) })
	return n, err
}

func TestObservationTraceNonpositiveLimitPreservesIdentity(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		for _, encoding := range []string{"", "identity"} {
			t.Run(fmt.Sprintf("limit=%d/encoding=%s", limit, encoding), func(t *testing.T) {
				store := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 1, MaxBodySize: limit, ChannelBuffer: 1}, newTestLogger())
				defer store.Close()
				svc := &Service{traceStore: store}
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				resp := &http.Response{StatusCode: 200, Header: http.Header{}}
				if encoding != "" {
					resp.Header.Set("Content-Encoding", encoding)
				}
				body := []byte(`{"usage":{"prompt_tokens":12}}`)
				svc.recordTrace(req, nil, resp, nil, body, time.Now(), -1, "deepseek-chat", usageTokens{})
				record := store.Get("")
				if record == nil || record.ResponseBody != string(body) {
					t.Fatalf("identity trace lost: %+v", record)
				}
			})
		}
	}
}
