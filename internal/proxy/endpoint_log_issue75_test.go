package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/errorlog"
	"github.com/LeXwDeX/LLM-Proxy/internal/tracestore"
)

func TestIssue75EndpointPathLoggingDoesNotChangeForwardOrTrace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		network bool
	}{{"http", false}, {"network", true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "errors.log")
			errorlog.Init(path)
			t.Cleanup(func() { _ = errorlog.Close() })
			s, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "t", EndpointType: "openai", Endpoint: "http://upstream.invalid/fixture-path-secret", APIKey: "fixture-key", UsageAdapter: "standard"}}}, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			traces := tracestore.New(tracestore.Config{Enabled: true, RingBufferSize: 4, MaxBodySize: 8 << 20, ChannelBuffer: 4}, newTestLogger())
			t.Cleanup(func() { _ = traces.Close() })
			s.traceStore = traces
			var sentPath string
			s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				sentPath = r.URL.Path
				if tc.network {
					return nil, errors.New("synthetic transport failure")
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture-error"}}`)), Request: r}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(headerProxyTarget, "t")
			req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "fixture-client")))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			wantStatus := http.StatusBadRequest
			if tc.network {
				wantStatus = http.StatusBadGateway
			}
			if rec.Code != wantStatus {
				t.Fatalf("status %d", rec.Code)
			}
			if sentPath != "/fixture-path-secret/v1/chat/completions" {
				t.Fatalf("forwarded path = %q", sentPath)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "fixture-path-secret") {
				t.Errorf("default error log leaked endpoint path: %s", data)
			}
			entries := readErrorLogEntries(t, path)
			if len(entries) != 1 || entries[0].UpstreamURL != "http://upstream.invalid" || entries[0].UpstreamFullURL != "http://upstream.invalid" {
				t.Errorf("bad error log URL fields: %+v", entries)
			}
			if !tc.network {
				trace := traces.Get("")
				if trace == nil || !strings.Contains(trace.UpstreamURL, "/fixture-path-secret/v1/chat/completions") {
					t.Errorf("explicit trace lost upstream path: %+v", trace)
				}
			}
		})
	}
}
