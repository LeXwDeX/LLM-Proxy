package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
)

func TestIssue74KeyPoolKeepsOnlyControlledDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		apply        func(*keyPool, string)
	}{
		{"401", "invalid_token", func(p *keyPool, body string) { p.markExhaustedWithError(0, "invalid_token", body) }},
		{"402", "billing_error", func(p *keyPool, body string) { p.markExhaustedWithError(0, "billing_error", body) }},
		{"429", glmRateLimitQPS, func(p *keyPool, body string) {
			p.markRateLimitedWithBody(0, []byte(`{"error":{"code":"1302","message":"`+body+`"}}`))
		}},
		{"429-unknown-code", glmRateLimitUnknown, func(p *keyPool, body string) { p.markRateLimitedWithBody(0, []byte(`{"error":{"code":"`+body+`"}}`)) }},
		{"glm-sse", glmRateLimitQuota, func(p *keyPool, body string) {
			p.markExhaustedWithCooldown(0, glmRateLimitQuota, time.Now().Add(time.Hour), body)
		}},
		{"minimax-json", "billing_error", func(p *keyPool, body string) { p.markExhaustedWithError(0, "billing_error", body) }},
		{"ws-terminal", "invalid_token", func(p *keyPool, body string) { p.markExhaustedWithError(0, "invalid_token", body) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			p := newKeyPool("fixture-target", []string{"synthetic-key"}, "", slog.New(slog.NewJSONHandler(&logs, nil)))
			tc.apply(p, "fixture-sensitive-response")
			status := p.status()[0]
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Exhausted || status.ExhaustReason != tc.reason || status.LastError == "" || status.LastErrorAt.IsZero() {
				t.Errorf("lost useful key state: %+v", status)
			}
			if strings.Contains(string(encoded), "fixture-sensitive-response") || strings.Contains(logs.String(), "fixture-sensitive-response") {
				t.Errorf("raw upstream content exposed: status=%s log=%s", encoded, logs.String())
			}
		})
	}
}

func TestIssue74NetworkErrorsAndURLsAreNotLoggedRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-error.log")
	errorlog.Init(path)
	t.Cleanup(func() { _ = errorlog.Close() })
	var logs bytes.Buffer
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "fixture-target", EndpointType: "openai", Endpoint: "http://fixture-user:fixture-password@fixture.invalid/v1", APIKey: "fixture-key", UsageAdapter: "standard"}},
	}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var wireURL *url.URL
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copyURL := *r.URL
		wireURL = &copyURL
		return nil, errors.New("fixture-raw-transport-error")
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api_key=fixture-api&access_token=fixture-token&arbitrary=fixture-query", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProxyTarget, "fixture-target")
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture-client")))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if wireURL == nil || wireURL.Query().Get("api_key") != "fixture-api" || wireURL.Query().Get("access_token") != "fixture-token" || wireURL.Query().Get("arbitrary") != "fixture-query" {
		t.Fatalf("upstream query changed: %v", wireURL)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	combined := logs.String() + string(data)
	for _, secret := range []string{"fixture-user", "fixture-password", "fixture-api", "fixture-token", "fixture-query", "fixture-raw-transport-error"} {
		if strings.Contains(combined, secret) {
			t.Errorf("default diagnostics exposed %q: %s", secret, combined)
		}
	}
	if !strings.Contains(string(data), "upstream_net_error") {
		t.Errorf("missing structured network diagnostic: %s", data)
	}
}

func TestIssue74AutomaticLargeRequestTraceHidesRawError(t *testing.T) {
	var logs bytes.Buffer
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "fixture-target", EndpointType: "openai", Endpoint: "http://fixture.invalid/v1", APIKey: "fixture-key", UsageAdapter: "standard"}},
	}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("fixture-large-request-raw-error")
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?other=fixture-large-query", nil)
	_, _, _, _ = s.forwardRequest(req, s.targetsByName["fixture-target"], bytes.Repeat([]byte("a"), 101*1024), "m", nil)
	if !strings.Contains(logs.String(), "upstream request trace") || strings.Contains(logs.String(), "fixture-large-request-raw-error") || strings.Contains(logs.String(), "fixture-large-query") {
		t.Fatalf("unsafe automatic request trace: %s", logs.String())
	}
}

func TestIssue74ExplicitTraceStillRecordsErrorBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.log")
	errorlog.Init(path)
	t.Cleanup(func() { _ = errorlog.Close() })
	wire := `{"error":{"message":"fixture-explicit-trace-body"}}`
	s, principal, store := newTraceCaptureService(t, true, http.StatusBadRequest, "application/json", wire)
	if rec := serveTraceCapture(s, principal); rec.Code != http.StatusBadRequest || rec.Body.String() != wire {
		t.Fatalf("response changed: %d %s", rec.Code, rec.Body.String())
	}
	if trace := store.Get(""); trace == nil || trace.ResponseBody != wire {
		t.Fatalf("explicit trace contract changed: %+v", trace)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "fixture-explicit-trace-body") {
		t.Fatalf("default error log retained body: %s %v", data, err)
	}
}

func TestIssue74HTTPErrorLogKeepsResponseAndQueryUntouched(t *testing.T) {
	for _, status := range []int{401, 402, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upstream-error.log")
			errorlog.Init(path)
			defer errorlog.Close()
			wire := `{"error":{"message":"fixture-sensitive-response"}}`
			s, principal, _ := newTraceCaptureService(t, false, status, "application/json", wire)
			// Avoid key-pool retries: this test observes the normal 4xx response.
			s.targetsByName["t"].keyPool = nil
			var gotQuery string
			s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gotQuery = r.URL.RawQuery
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(wire)), Request: r}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?arbitrary=fixture-query", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != status || rec.Body.String() != wire || gotQuery != "arbitrary=fixture-query" {
				t.Fatalf("forwarding changed status=%d body=%s query=%s", rec.Code, rec.Body.String(), gotQuery)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "fixture-sensitive-response") || strings.Contains(string(data), "fixture-query") {
				t.Errorf("unsafe error log: %s", data)
			}
			entries := readErrorLogEntries(t, path)
			if len(entries) != 1 || entries[0].UpstreamStatus != status || entries[0].RespBytes != len(wire) {
				t.Errorf("lost response diagnostic: %+v", entries)
			}
		})
	}
}
