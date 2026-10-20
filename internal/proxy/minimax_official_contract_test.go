package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

type minimaxObservedBody struct {
	io.Reader
	reads, closes int
}

func (b *minimaxObservedBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}
func (b *minimaxObservedBody) Close() error { b.closes++; return nil }

func TestMinimaxInBandProbeProtocolBoundary(t *testing.T) {
	for _, tc := range []struct{ path, typ, wire string }{
		{"/v1/chat/completions", "text/event-stream", "data: {\"base_resp\":{\"status_code\":1008}}\n\ndata: [DONE]\n\n"},
		{"/v1/chat/completions", "text/event-stream", "event: arbitrary\ndata: not JSON\n\n"},
		{"/v1/responses", "application/json", `{"base_resp":{"status_code":1008}}`},
		{"/v1/completions", "application/json", `{"base_resp":{"status_code":1008}}`},
		{"/anthropic/v1/messages", "application/json", `{"base_resp":{"status_code":1008}}`},
		{"/v1/chat/completions", "text/plain", `{"base_resp":{"status_code":1008}}`},
	} {
		t.Run(tc.path+"/"+tc.typ+"/"+tc.wire, func(t *testing.T) {
			s := newTestModelService(t, "openai", "https://fixture.invalid", func(tg *config.Target) { tg.UsageAdapter = "minimax"; tg.APIKeys = []string{"second"} })
			defer s.Close()
			body := &minimaxObservedBody{Reader: strings.NewReader(tc.wire)}
			s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.typ}}, Body: body, Request: req}, nil
			})
			resp, cancel, _, err := s.forwardRequest(httptest.NewRequest("POST", tc.path, nil), s.targetOrder[0], []byte(`{"model":"test-model"}`), "", nil)
			if cancel != nil {
				defer cancel()
			}
			if err != nil || resp == nil {
				t.Fatalf("unexpected patch: %v", err)
			}
			if body.reads != 0 {
				t.Errorf("read %d times before forwarding", body.reads)
			}
			wire, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(wire) != tc.wire || body.closes != 1 {
				t.Fatalf("wire/close changed: %q, %v, %d", wire, err, body.closes)
			}
			for i, entry := range s.targetOrder[0].keyPool.entries {
				if entry.exhausted || s.targetOrder[0].keyPool.inFlight[i] != 0 {
					t.Fatal("key exhausted or slot leaked")
				}
			}
		})
	}
}

type minimaxDeadlineBody struct {
	ctx    context.Context
	closed int
}

func (b *minimaxDeadlineBody) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	return copy(p, `{"base_resp":{"status_code":1008}}`), b.ctx.Err()
}
func (b *minimaxDeadlineBody) Close() error { b.closed++; return nil }

func TestMinimaxNon2xxDeadlineMatchesStandard(t *testing.T) {
	for _, status := range []int{401, 402, 429} {
		var standardReason string
		for _, adapter := range []string{"standard", "minimax"} {
			t.Run(fmt.Sprintf("%s/%d", adapter, status), func(t *testing.T) {
				s := newTestModelService(t, "openai", "https://fixture.invalid", func(tg *config.Target) { tg.UsageAdapter = adapter; tg.APIKeys = []string{"second"} })
				defer s.Close()
				req := statsRequest(t, "/v1/chat/completions")
				ctx, cancel := context.WithTimeout(req.Context(), 20*time.Millisecond)
				defer cancel()
				body := &minimaxDeadlineBody{ctx: ctx}
				calls := 0
				s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: req}, nil
				})
				resp, release, _, err := s.forwardRequest(req.WithContext(ctx), s.targetOrder[0], []byte(`{"model":"test-model"}`), "", nil)
				if release != nil {
					defer release()
				}
				if err != nil || resp == nil {
					t.Fatalf("baseline non-2xx path returns the response: %v", err)
				}
				resp.Body.Close()
				if calls != 1 || body.closed != 1 || resp.StatusCode != status {
					t.Fatalf("calls=%d closes=%d status=%d", calls, body.closed, resp.StatusCode)
				}
				state := s.targetOrder[0]
				if state.inFlightCount() != 0 {
					t.Fatal("node slot leaked")
				}
				for i := range state.keyPool.entries {
					if state.keyPool.inFlight[i] != 0 {
						t.Fatal("slot leaked")
					}
				}
				reason := state.keyPool.exhaustReasonAt(0)
				if adapter == "standard" {
					standardReason = reason
				} else if reason != standardReason {
					t.Fatalf("minimax reason %q differs from standard %q", reason, standardReason)
				}
			})
		}
	}
}

func TestMinimaxUnnormalizedAdapterNon200Lifecycle(t *testing.T) {
	s := newTestModelService(t, "openai", "https://fixture.invalid", func(tg *config.Target) { tg.UsageAdapter = " MiniMax " })
	defer s.Close()
	// Bypass configuration normalization to exercise an in-memory target.
	s.targetOrder[0].target.UsageAdapter = " MiniMax "
	billing := &adapterEventRecorder{}
	s.SetUsageRecorder(billing)
	s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"base_resp":{"status_code":2013},"usage":{"prompt_tokens":7,"completion_tokens":3}}`)), Request: req}, nil
	})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, statsRequest(t, "/v1/chat/completions"))
	if rr.Code != 201 || len(billing.events) != 1 || billing.events[0].ResponseStatus == "failed" {
		t.Fatalf("non-200 Patch activated: status=%d events=%+v", rr.Code, billing.events)
	}
}

func TestMinimaxNon2xxMatchesStandard(t *testing.T) {
	for _, tc := range []struct {
		path, endpoint, body                string
		status, badCalls, goodCalls, result int
	}{
		{"/v1/chat/completions", "openai", `{"base_resp":{"status_code":1008}}`, 429, 2, 1, 200},
		{"/v1/chat/completions", "openai", `{"base_resp":{"status_code":1004}}`, 401, 1, 0, 401},
		{"/v1/messages", "claude", `{"type":"error","request_id":"req_fake","error":{"type":"rate_limit_error","message":"Rate limit reached"}}`, 429, 2, 1, 200},
		{"/v1/messages", "claude", `{"type":"error","request_id":"req_fake","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529, 1, 0, 529},
	} {
		for _, adapter := range []string{"minimax", "standard"} {
			t.Run(fmt.Sprintf("%s/%s/%d", adapter, tc.path, tc.status), func(t *testing.T) {
				bad, good := 0, 0
				s := newTestModelService(t, tc.endpoint, "https://fixture.invalid", func(tg *config.Target) {
					tg.UsageAdapter, tg.APIKey, tg.APIKeys = adapter, "k0", []string{"k1"}
				})
				defer s.Close()
				s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
					status, body := tc.status, tc.body
					if req.Header.Get("Authorization") == "Bearer k1" || req.Header.Get("X-Api-Key") == "k1" {
						good++
						status, body = 200, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`
					} else {
						bad++
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})
				rr := httptest.NewRecorder()
				s.ServeHTTP(rr, statsRequest(t, tc.path))
				if bad != tc.badCalls || good != tc.goodCalls || rr.Code != tc.result {
					t.Fatalf("bad=%d good=%d status=%d; want %d/%d/%d", bad, good, rr.Code, tc.badCalls, tc.goodCalls, tc.result)
				}
				if tc.goodCalls == 0 && rr.Body.String() != tc.body {
					t.Fatal("response changed")
				}
				state := s.targetOrder[0]
				if state.keyPool.entries[0].exhausted != (tc.status == 429) {
					t.Fatal("wrong key state")
				}
				if tc.status == 429 && state.keyPool.exhaustReasonAt(0) != "rate_limited" {
					t.Fatal("base_resp changed generic cooldown")
				}
				for _, n := range state.keyPool.inFlight {
					if n != 0 {
						t.Fatal("slot leaked")
					}
				}
			})
		}
	}
}

func TestMinimaxLifecycleProtocolBoundary(t *testing.T) {
	for _, protocol := range []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages} {
		u := newUsageStream("application/json", "", usageParser{protocol: protocol, usageAdapter: "minimax"})
		u.Write([]byte(`{"base_resp":{"status_code":2013}}`))
		u.Close()
		if (u.responseStatus == "failed") != (protocol == config.ProtocolOpenAIChat) {
			t.Errorf("%s status=%s", protocol, u.responseStatus)
		}
	}
}
