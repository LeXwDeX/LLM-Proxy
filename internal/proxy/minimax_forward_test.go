package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

type minimaxCancelReader struct {
	cancel context.CancelFunc
	closes int
	read   bool
}

type minimaxReadFailure struct{}

func (*minimaxReadFailure) Read(p []byte) (int, error) {
	return copy(p, `{"base_resp":{"status_code":1008}}`), io.ErrUnexpectedEOF
}
func (*minimaxReadFailure) Close() error { return nil }

func TestMinimaxFailedPeekIsNotRetrySignal(t *testing.T) {
	s := newTestModelService(t, "openai", "https://fixture.invalid", func(tg *config.Target) { tg.UsageAdapter = "minimax"; tg.APIKeys = []string{"second"} })
	defer s.Close()
	s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: &minimaxReadFailure{}, Request: req}, nil
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	resp, cancel, _, err := s.forwardRequest(req, s.targetOrder[0], []byte(`{"model":"test-model"}`), "", nil)
	if cancel != nil {
		defer cancel()
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("failed read became retry signal: %v", err)
	}
}

func (b *minimaxCancelReader) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.EOF
	}
	b.read = true
	b.cancel()
	return copy(p, `{"base_resp":{"status_code":1008}}`), io.EOF
}
func (b *minimaxCancelReader) Close() error { b.closes++; return nil }

func TestMinimaxPeekCancellationDoesNotRetry(t *testing.T) {
	assertMinimaxCanceledPeek(t, 200, "minimax")
}

func TestMinimaxHTTP429CancellationMatchesStandard(t *testing.T) {
	for _, adapter := range []string{"standard", "minimax"} {
		assertMinimaxCanceledPeek(t, 429, adapter)
	}
}

func assertMinimaxCanceledPeek(t *testing.T, status int, adapter string) {
	t.Helper()
	s := newTestModelService(t, "openai", "https://fixture.invalid", func(tg *config.Target) { tg.UsageAdapter = adapter; tg.APIKeys = []string{"second"} })
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &minimaxCancelReader{cancel: cancel}
	s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: req}, nil
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	resp, release, _, err := s.forwardRequest(req, s.targetOrder[0], []byte(`{"model":"test-model"}`), "", nil)
	if release != nil {
		defer release()
	}
	if err != nil || resp == nil || resp.StatusCode != status {
		t.Fatalf("resp=%v err=%v closes=%d", resp, err, body.closes)
	}
	replayed, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || string(replayed) != `{"base_resp":{"status_code":1008}}` {
		t.Fatalf("prefix not handed off: %q err=%v", replayed, readErr)
	}
	if body.closes != 1 {
		t.Fatalf("closes=%d", body.closes)
	}
	state := s.targetOrder[0]
	for _, e := range state.keyPool.entries {
		if e.exhausted {
			t.Fatal("canceled request disabled key")
		}
	}
	for _, n := range state.keyPool.inFlight {
		if n != 0 {
			t.Fatal("inFlight leaked")
		}
	}
}

func TestMinimaxForwardPreservesBytes(t *testing.T) {
	for _, tc := range []struct {
		name, body, typ string
		gzip            bool
		failed          bool
	}{
		{"small", `{"base_resp":{"status_code":0},"usage":{"prompt_tokens":3,"completion_tokens":1}}`, "application/json", false, false},
		{"large", `{"content":"` + strings.Repeat("x", 6000) + `","base_resp":{"status_code":0},"usage":{"prompt_tokens":3,"completion_tokens":1}}`, "application/json", false, false},
		{"sse", "data: {\"base_resp\":{\"status_code\":0}}\n\ndata: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", "text/event-stream", false, false},
		{"unknown", `{"base_resp":{"status_code":2013}}`, "application/json", false, true},
		{"gzip", `{"base_resp":{"status_code":1008}}`, "application/json", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := []byte(tc.body)
			if tc.gzip {
				var b bytes.Buffer
				z := gzip.NewWriter(&b)
				z.Write(wire)
				z.Close()
				wire = b.Bytes()
			}
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", tc.typ)
				if tc.gzip {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.Write(wire)
			}))
			defer up.Close()
			s := newTestModelService(t, "openai", up.URL, func(tg *config.Target) { tg.UsageAdapter = "minimax"; tg.APIKeys = []string{"second"} })
			defer s.Close()
			billing := &adapterEventRecorder{}
			s.SetUsageRecorder(billing)
			req := statsRequest(t, "/v1/chat/completions")
			req.Header.Set("Accept-Encoding", "gzip")
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), wire) || calls.Load() != 1 {
				t.Fatalf("wire/calls changed: HTTP %d calls=%d", rr.Code, calls.Load())
			}
			if len(billing.events) != 1 || (billing.events[0].ResponseStatus == "failed") != tc.failed {
				t.Fatalf("events=%+v", billing.events)
			}
			if !tc.failed && (billing.events[0].InputTokens != 3 || billing.events[0].OutputTokens != 1) {
				t.Fatal("usage lost")
			}
			state, _ := s.targetByName("test-target")
			for _, count := range state.keyPool.inFlight {
				if count != 0 {
					t.Fatal("inFlight leak")
				}
			}
		})
	}
}

func TestMinimaxAllKeysExhaustedMappedStatus(t *testing.T) {
	for _, code := range []int{1004, 1008, 1002} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"base_resp":{"status_code":%d}}`, code)
			}))
			defer up.Close()
			s, rr := serveKeyExhaustionRequest(t, config.Target{Name: "pool", EndpointType: "openai", Endpoint: up.URL, UsageAdapter: "minimax", APIKey: "k0", APIKeys: []string{"k1"}}, "test-model")
			defer s.Close()
			if rr.Code != minimaxErrorStatus(minimaxReasonForCode(int64(code))) || calls.Load() != 2 {
				t.Fatalf("HTTP %d calls=%d body=%s", rr.Code, calls.Load(), rr.Body.String())
			}
		})
	}
}

func TestMinimaxForwardRotation(t *testing.T) {
	const status = 200
	for _, code := range []int{1008, 1002, 1004, 2049, 1041, 2045, 2056, 2013} {
		for _, sse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%d/sse=%v", status, code, sse), func(t *testing.T) {
				body := fmt.Sprintf(`{"base_resp":{"status_code":%d}}`, code)
				typ := "application/json"
				if sse {
					body = "data: " + body + "\n\ndata: [DONE]\n\n"
					typ = "text/event-stream"
				}
				var bad, good atomic.Int64
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.Header.Get("Authorization"), "k0") {
						bad.Add(1)
						w.Header().Set("Content-Type", typ)
						w.WriteHeader(status)
						w.Write([]byte(body))
						return
					}
					good.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
				}))
				defer up.Close()
				start := time.Now()
				svc, rr := serveKeyExhaustionRequest(t, config.Target{Name: "pool", EndpointType: "openai", Endpoint: up.URL, UsageAdapter: "minimax", APIKey: "k0", APIKeys: []string{"k1"}}, "test-model")
				defer svc.Close()
				if bad.Load() != 1 {
					t.Fatalf("bad key calls=%d", bad.Load())
				}
				if code == 2013 || sse {
					if good.Load() != 0 || rr.Code != status || rr.Body.String() != body {
						t.Fatalf("unknown changed: %d %q calls %d", rr.Code, rr.Body.String(), good.Load())
					}
				} else {
					if good.Load() != 1 || rr.Code != 200 {
						t.Fatalf("no rotation: %d %s calls %d", rr.Code, rr.Body.String(), good.Load())
					}
					if time.Since(start) >= time.Second {
						t.Fatal("unexpected same-key delay")
					}
				}
				state, _ := svc.targetByName("pool")
				if state.keyPool.entries[0].exhausted != (code != 2013 && !sse) {
					t.Fatal("wrong key exhaustion")
				}
				for _, count := range state.keyPool.inFlight {
					if count != 0 {
						t.Fatal("inFlight leaked")
					}
				}
			})
		}
	}
}

func TestMinimax429WithoutBaseRespPreservesStandardRetry(t *testing.T) {
	for _, adapter := range []string{"minimax", "standard"} {
		up, srv := newKeyedUpstream(t, "k0", `{"error":{"code":"rate_limit_exceeded"}}`)
		svc, rr := serveKeyExhaustionRequest(t, config.Target{Name: "pool", Endpoint: srv.URL, UsageAdapter: adapter, APIKey: "k0", APIKeys: []string{"k1"}}, "test-model")
		defer svc.Close()
		if rr.Code != 200 || up.count("k0") != 2 {
			t.Fatalf("%s: HTTP %d calls=%d", adapter, rr.Code, up.count("k0"))
		}
	}
}

func TestMinimaxLifecycle(t *testing.T) {
	for _, adapter := range []string{"minimax", "standard"} {
		for _, body := range []string{`{"base_resp":{"status_code":2013}}`, "data: {\"base_resp\":{\"status_code\":2013}}\n\ndata: {\"status\":\"completed\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"} {
			u := newUsageStream("application/json", "", usageParser{protocol: config.ProtocolOpenAIChat, usageAdapter: adapter})
			u.Write([]byte(body))
			u.Close()
			if (u.responseStatus == "failed") != (adapter == "minimax" && !strings.HasPrefix(body, "data:")) {
				t.Fatalf("%s: %s", adapter, u.responseStatus)
			}
		}
	}
}
