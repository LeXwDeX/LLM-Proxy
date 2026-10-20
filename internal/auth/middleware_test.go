package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestMiddlewareAcceptsAzureStyleAuth(t *testing.T) {
	store := NewStore()
	if err := store.LoadFromConfig([]config.Client{
		{Name: "bearer", AccessKey: "token", AllowedTargets: nil},
		{Name: "apikey", AccessKey: "apikey-value", AllowedTargets: nil},
		{Name: "apikey-plus", AccessKey: "abc+def", AllowedTargets: nil},
		{Name: "claude-client", AccessKey: "x-api-key-value", AllowedTargets: nil},
	}); err != nil {
		t.Fatalf("load config: %v", err)
	}

	tests := []struct {
		name       string
		headerKey  string
		headerVal  string
		queryValue string
		rawQuery   string
		wantStatus int
	}{
		{name: "bearer ok", headerKey: "Authorization", headerVal: "Bearer token", wantStatus: http.StatusOK},
		{name: "api-key header ok", headerKey: "api-key", headerVal: "apikey-value", wantStatus: http.StatusOK},
		{name: "x-api-key header ok (Anthropic native)", headerKey: "x-api-key", headerVal: "x-api-key-value", wantStatus: http.StatusOK},
		{name: "api-key query ok", queryValue: "apikey-value", wantStatus: http.StatusOK},
		{name: "api-key query preserves plus", rawQuery: "api-key=abc+def", wantStatus: http.StatusOK},
		{name: "key query ok (Gemini native)", rawQuery: "key=apikey-value", wantStatus: http.StatusOK},
		{name: "key query with plus", rawQuery: "key=abc+def", wantStatus: http.StatusOK},
		{name: "KEY query uppercase", rawQuery: "KEY=apikey-value", wantStatus: http.StatusOK},
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "bad bearer scheme", headerKey: "Authorization", headerVal: "Basic token", wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
			handler := Middleware(store, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			url := "/"
			if tt.queryValue != "" {
				url = "/?api-key=" + tt.queryValue
			}
			if tt.rawQuery != "" {
				url = "/?" + tt.rawQuery
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tt.headerKey != "" {
				req.Header.Set(tt.headerKey, tt.headerVal)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, rr.Code)
			}
		})
	}
}

func TestMiddlewareEnforcesClientIPv4RulesWithoutTrustingClientHeaders(t *testing.T) {
	t.Setenv("AUTH_TRUST_PROXY_XFF", "false")
	store := NewStore()
	if err := store.LoadFromConfig([]config.Client{
		{Name: "restricted", AccessKey: "restricted-key", IPAddress: "192.168.0.0/16,10.0.1.1"},
		{Name: "open", AccessKey: "open-key"},
	}); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := Middleware(store, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, test := range []struct {
		name, key, remote, forwarded string
		want                         int
	}{
		{"CIDR allowed", "restricted-key", "192.168.50.83:12345", "", http.StatusOK},
		{"single IP allowed", "restricted-key", "10.0.1.1:12345", "", http.StatusOK},
		{"outside denied", "restricted-key", "10.0.1.2:12345", "", http.StatusForbidden},
		{"spoofed forwarded header denied", "restricted-key", "10.0.1.2:12345", "192.168.50.83", http.StatusForbidden},
		{"unrestricted client", "open-key", "10.0.1.2:12345", "", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
			req.RemoteAddr = test.remote
			req.Header.Set("Authorization", "Bearer "+test.key)
			if test.forwarded != "" {
				req.Header.Set("X-Forwarded-For", test.forwarded)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status = %d, want %d", rec.Code, test.want)
			}
		})
	}
}

func TestMiddlewareUsesForwardedIPOnlyForTrustedProxyMode(t *testing.T) {
	t.Setenv("AUTH_TRUST_PROXY_XFF", "true")
	store := NewStore()
	if err := store.LoadFromConfig([]config.Client{{Name: "restricted", AccessKey: "key", IPAddress: "192.168.0.0/16"}}); err != nil {
		t.Fatalf("load client: %v", err)
	}
	handler := Middleware(store, slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, test := range []struct {
		name, remote, forwarded string
		want                    int
	}{
		{"proxy forwarded allowed", "10.0.5.1:12345", "192.168.50.83", http.StatusOK},
		{"proxy forwarded denied", "10.0.5.1:12345", "203.0.113.4", http.StatusForbidden},
		{"public peer cannot spoof", "203.0.113.4:12345", "192.168.50.83", http.StatusForbidden},
		{"malformed forwarded denied", "10.0.5.1:12345", "bad-ip", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
			req.RemoteAddr = test.remote
			req.Header.Set("Authorization", "Bearer key")
			req.Header.Set("X-Forwarded-For", test.forwarded)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status = %d, want %d", rec.Code, test.want)
			}
		})
	}
}
