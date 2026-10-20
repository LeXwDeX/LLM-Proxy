package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// A complete JSON prefix does not prove a successful body read. Keep a suffix
// after the error to check replay of both consumed bytes and the remaining body.
type minimaxNon2xxReadErrorBody struct {
	read   bool
	closed int
}

const minimaxReadErrorJSON = `{"base_resp":{"status_code":1008}}`

func (b *minimaxNon2xxReadErrorBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, minimaxReadErrorJSON), io.ErrUnexpectedEOF
	}
	return copy(p, "\n"), io.EOF
}

func (b *minimaxNon2xxReadErrorBody) Close() error { b.closed++; return nil }

func TestMinimaxNon2xxReadFailureUsesStandardPolicy(t *testing.T) {
	for _, adapter := range []string{"minimax", "standard"} {
		for _, status := range []int{429, 402, 400} {
			t.Run(fmt.Sprintf("%s/%d", adapter, status), func(t *testing.T) {
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}}
				for _, name := range []string{"primary", "alternative"} {
					cfg.Targets = append(cfg.Targets, config.Target{Name: name, EndpointType: "openai", Endpoint: "https://" + name + ".invalid", UsageAdapter: adapter, APIKey: "k0", APIKeys: []string{"k1"}, MaxConcurrency: 1, ModelMappings: []config.ModelMapping{{Upstream: "test-model"}}})
				}
				s, err := NewService(cfg, newTestLogger())
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				calls, healthy, firstHost := 0, 0, ""
				var bodies []*minimaxNon2xxReadErrorBody
				s.httpClient.Transport = coreReviewTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if firstHost == "" {
						firstHost = req.URL.Host
					}
					if req.URL.Host != firstHost {
						t.Error("unexpected target failover")
					}
					if req.Header.Get("Authorization") == "Bearer k1" {
						healthy++
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)), Request: req}, nil
					}
					body := &minimaxNon2xxReadErrorBody{}
					bodies = append(bodies, body)
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: req}, nil
				})
				stats := &completionRecorder{}
				s.SetRequestStatsRecorder(stats)
				rr := httptest.NewRecorder()
				s.ServeHTTP(rr, statsRequest(t, "/v1/chat/completions"))
				wantCalls, wantHealthy, wantStatus := 1, 0, status
				// Main's standard policy ignores this read error: 402 rotates
				// immediately; 429 first retries the same key, then rotates.
				if status != 400 {
					wantCalls, wantHealthy, wantStatus = 2, 1, 200
					if status == 429 {
						wantCalls = 3
					}
				}
				if calls != wantCalls || healthy != wantHealthy || rr.Code != wantStatus {
					t.Fatalf("calls=%d healthy=%d HTTP=%d; want %d/%d/%d", calls, healthy, rr.Code, wantCalls, wantHealthy, wantStatus)
				}
				if wantCalls == 1 && rr.Body.String() != minimaxReadErrorJSON+"\n" {
					t.Fatalf("wire changed: %q", rr.Body.String())
				}
				if status == 400 {
					assertCompletions(t, stats, requeststats.Failure)
				} else {
					assertCompletions(t, stats, requeststats.Success)
				}
				for _, body := range bodies {
					if body.closed != 1 {
						t.Fatalf("body closed %d times", body.closed)
					}
				}
				for _, state := range s.targetOrder {
					if state.inFlightCount() != 0 {
						t.Fatal("node slot leaked")
					}
					state.keyPool.mu.Lock()
					for i := range state.keyPool.entries {
						if state.keyPool.inFlight[i] != 0 {
							t.Errorf("key %d inFlight leaked", i)
						}
					}
					state.keyPool.mu.Unlock()
				}
			})
		}
	}
}
