package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

var pricingRequestCorpus = []string{
	`{"service_tier":"fast"}`, `{"speed":"fast","service_tier":"priority"}`, `{"service_tier":null}`,
	`{"service_tier":1}`, `{"service_tier":{"a":[1,2]},"speed":9007199254740993}`, `{"service_tier":"a","service_tier":"b"}`,
	`{"messages":[{"content":"service_tier"}],"service_tier":"flex"}`, `{}`, `[]`, `null`, `not-json`,
	`{"service_tier":"fast"} trailing`, `{"service_tier":"fast",}`, `{"service_tier":"fast"`, ``,
}

func TestNewTargetUsageParserUsesForwardBodyWithoutGetBody(t *testing.T) {
	body := []byte(`{"model":"m","service_tier":"fast"}`)
	var reads atomic.Int32
	outbound, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/chat/completions", bytes.NewReader(body))
	outbound.GetBody = func() (io.ReadCloser, error) {
		reads.Add(1)
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	outbound.Header.Set("X-Outbound", "1")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Client-Only", "1")
	p := newTargetUsageParser(r, &Target{UsageAdapter: "qwen"}, "m", body, outbound)
	if reads.Load() != 0 {
		t.Fatalf("GetBody was read %d times; the assembled body is already in hand", reads.Load())
	}
	if len(p.requestBody) != len(body) || &p.requestBody[0] != &body[0] {
		t.Fatal("parser does not use the immutable forward body")
	}
	if p.requestHeaders.Get("X-Outbound") != "1" || p.requestHeaders.Get("X-Client-Only") != "" {
		t.Fatalf("parser headers must be the outbound headers: %v", p.requestHeaders)
	}
	outbound.Header.Set("X-Outbound", "changed")
	if p.requestHeaders.Get("X-Outbound") != "1" {
		t.Fatal("parser headers are not a private copy")
	}
	if p.pricingRequestFields["service_tier"] != "fast" || !p.pricingRequestCaptured {
		t.Fatalf("pricing fields = %v", p.pricingRequestFields)
	}
}

func TestNewTargetUsageParserWithoutOutboundKeepsClientAndCustomHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Client", "1")
	p := newTargetUsageParser(r, &Target{CustomHeaders: map[string]string{"X-Custom": "2"}}, "m", nil, nil)
	if p.requestHeaders.Get("X-Client") != "1" || p.requestHeaders.Get("X-Custom") != "2" {
		t.Fatalf("headers = %v", p.requestHeaders)
	}
	if r.Header.Get("X-Custom") != "" {
		t.Fatal("client request headers were mutated")
	}
}

func TestPricingRequestFutureMatchesSynchronousProjection(t *testing.T) {
	for _, raw := range pricingRequestCorpus {
		body := []byte(raw)
		ctx := startPricingRequest(context.Background(), body)
		got, ok := pricingRequestFutureFrom(ctx).join(body)
		if len(body) == 0 {
			if ok {
				t.Fatal("empty body must not start a projection")
			}
			continue
		}
		if !ok || !reflect.DeepEqual(got, pricingRequest(body)) {
			t.Fatalf("body %q: future=%v ok=%v sync=%v", raw, got, ok, pricingRequest(body))
		}
		outbound, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.invalid", nil)
		p := newTargetUsageParser(nil, nil, "m", body, outbound)
		if !reflect.DeepEqual(p.pricingRequestFields, pricingRequest(body)) {
			t.Fatalf("body %q: parser fields %v", raw, p.pricingRequestFields)
		}
	}
}

// A future for one body never answers for another: the parser falls back to
// computing the body it was given.
func TestPricingRequestFutureRejectsDifferentBody(t *testing.T) {
	sent := []byte(`{"service_tier":"fast"}`)
	other := []byte(`{"service_tier":"flex"}`)
	ctx := startPricingRequest(context.Background(), sent)
	if _, ok := pricingRequestFutureFrom(ctx).join(other); ok {
		t.Fatal("future answered for a different body")
	}
	outbound, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.invalid", nil)
	if p := newTargetUsageParser(nil, nil, "m", other, outbound); p.pricingRequestFields["service_tier"] != "flex" {
		t.Fatalf("fields = %v", p.pricingRequestFields)
	}
}

// Cancellation abandons the projection; a later join still yields exactly
// the synchronous result.
func TestPricingRequestFutureCanceledFallsBackToSyncResult(t *testing.T) {
	body := perfChatBody(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = startPricingRequest(ctx, body)
	outbound, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.invalid", nil)
	p := newTargetUsageParser(nil, nil, "m", body, outbound)
	if p.pricingRequestFields["service_tier"] != "default" {
		t.Fatalf("fields = %v", p.pricingRequestFields)
	}
}

func waitGoroutines(t *testing.T, limit int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > limit {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines = %d, want <= %d\n%s", runtime.NumGoroutine(), limit, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPricingRequestFutureDoesNotLeakWhenNeverJoined(t *testing.T) {
	body := perfChatBody(256 << 10)
	baseline := runtime.NumGoroutine()
	for i := 0; i < 32; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		_ = startPricingRequest(ctx, body)
		cancel()
	}
	for i := 0; i < 8; i++ {
		_ = startPricingRequest(context.Background(), body) // runs to completion
	}
	waitGoroutines(t, baseline)
}

type pricingFutureTransport struct {
	started atomic.Bool
	block   chan struct{}
}

func (tr *pricingFutureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	tr.started.Store(pricingRequestFutureFrom(r.Context()) != nil)
	if tr.block != nil {
		select {
		case <-tr.block:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"service_tier":"default","usage":{"prompt_tokens":3,"completion_tokens":1}}`)), Request: r}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newPricingFutureService(t *testing.T, transport http.RoundTripper, customBody map[string]any) (*Service, *auth.Principal, *goneEventRecorder) {
	t.Helper()
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "t", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://upstream.invalid", APIKey: "fixture", UsageAdapter: "standard", CustomBody: customBody}},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.httpClient = &http.Client{Transport: transport}
	recorder := &goneEventRecorder{}
	s.SetUsageRecorder(recorder)
	return s, newAllowAllPrincipal(t, "fixture-pricing-future"), recorder
}

// The projection starts before the request is sent and the usage event still
// carries the request's pricing evidence.
func TestForwardStartsPricingProjectionBeforeSend(t *testing.T) {
	transport := &pricingFutureTransport{}
	s, principal, recorder := newPricingFutureService(t, transport, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","service_tier":"priority"}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !transport.started.Load() {
		t.Fatalf("status=%d future-before-send=%v", w.Code, transport.started.Load())
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].PricingContext == nil || events[0].PricingContext.RequestServiceTier.Raw != "priority" {
		t.Fatalf("events=%+v", events)
	}
}

// A client that leaves while response headers are pending cancels the
// attempt; the projection goroutine must end with it.
func TestPricingProjectionEndsWhenClientCancelsBeforeHeaders(t *testing.T) {
	transport := &pricingFutureTransport{block: make(chan struct{})}
	s, principal, _ := newPricingFutureService(t, transport, nil)
	baseline := runtime.NumGoroutine()
	body := string(perfChatBody(1 << 20))
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(auth.WithPrincipal(context.Background(), principal))
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), r) }()
		time.Sleep(20 * time.Millisecond)
		cancel()
		<-done
	}
	s.httpClient.CloseIdleConnections()
	waitGoroutines(t, baseline+2)
}
