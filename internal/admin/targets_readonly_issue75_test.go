package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
)

func TestIssue75TargetListSensitiveFields(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, 1, nil)
	target := &cfg.Targets[0]
	target.Endpoint = "https://fixture-user:fixture-password@example.invalid/fixture-path-secret?credential=fixture-query#fixture-fragment"
	target.EndpointType = "dual_protocol"
	target.OpenAIPrefix = "/fixture-openai-prefix-secret"
	target.AnthropicPrefix = "/fixture-anthropic-prefix-secret"
	target.ResourcePathPrefix = ""
	target.CustomHeaders = map[string]string{"X-fixture-header-name": "fixture-header-secret"}
	target.CustomBody = map[string]any{"fixture-body-name": map[string]any{"value": "fixture-body-secret"}}
	path := filepath.Join(dir, "config.json")
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "test-session-secret", CookieName: "session", TTLSeconds: 3600}, logger)
	h := NewHandler(config.NewManager(path), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	secured := chi.NewRouter()
	secured.Route("/admin", func(r chi.Router) {
		r.Use(sm.Middleware)
		r.Mount("/", h)
	})
	_, adminCookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, readonlyCookie, err := sm.CreateSession("viewer", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/data/targets", "/admin/healthz", "/admin/api/overview"} {
		unauthenticated := httptest.NewRecorder()
		unauthenticatedRequest := httptest.NewRequest(http.MethodGet, path, nil)
		unauthenticatedRequest.Header.Set("Accept", "application/json")
		secured.ServeHTTP(unauthenticated, unauthenticatedRequest)
		if unauthenticated.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status = %d", path, unauthenticated.Code)
		}
	}
	get := func(cookie *http.Cookie) (map[string]any, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/admin/data/targets", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		secured.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list status: %d", rec.Code)
		}
		var body struct {
			Targets []map[string]any `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Targets) != 1 {
			t.Fatalf("invalid targets response: %v", err)
		}
		return body.Targets[0], rec.Body.String()
	}
	ro, raw := get(readonlyCookie)
	for _, secret := range []string{"fixture-user", "fixture-password", "fixture-path-secret", "fixture-query", "fixture-fragment", "fixture-openai-prefix-secret", "fixture-anthropic-prefix-secret", "fixture-header-name", "fixture-header-secret", "fixture-body-name", "fixture-body-secret"} {
		if strings.Contains(raw, secret) {
			t.Errorf("readonly response contains %q", secret)
		}
	}
	if ro["endpoint"] != "https://example.invalid" {
		t.Errorf("readonly endpoint = %v", ro["endpoint"])
	}
	if ro["openai_prefix"] != "" || ro["anthropic_prefix"] != "" {
		t.Errorf("readonly prefixes not redacted: %v, %v", ro["openai_prefix"], ro["anthropic_prefix"])
	}
	if ro["custom_headers"] != nil || ro["custom_body"] != nil {
		t.Error("readonly custom fields must be hidden")
	}
	if ro["name"] != "target-1" || ro["model_mappings"] == nil {
		t.Error("readonly lost target display metadata")
	}
	admin, _ := get(adminCookie)
	if admin["openai_prefix"] != target.OpenAIPrefix || admin["anthropic_prefix"] != target.AnthropicPrefix {
		t.Error("admin prefixes changed")
	}
	if admin["endpoint"] != target.Endpoint || !reflect.DeepEqual(admin["custom_headers"], map[string]any{"X-fixture-header-name": "fixture-header-secret"}) || !reflect.DeepEqual(admin["custom_body"], map[string]any{"fixture-body-name": map[string]any{"value": "fixture-body-secret"}}) {
		t.Error("admin target editing values changed")
	}
	for _, path := range []string{"/admin/healthz", "/admin/api/overview"} {
		for _, tc := range []struct {
			name   string
			cookie *http.Cookie
			want   string
		}{{"readonly", readonlyCookie, "https://example.invalid"}, {"admin", adminCookie, target.Endpoint}} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.AddCookie(tc.cookie)
				rec := httptest.NewRecorder()
				secured.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
				}
				var body struct {
					Targets []map[string]any `json:"targets"`
					Count   int              `json:"target_count"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Targets) != 1 || body.Count != 1 {
					t.Fatalf("invalid status response: %s, %v", rec.Body.String(), err)
				}
				if got := body.Targets[0]["Endpoint"]; got != tc.want {
					t.Errorf("endpoint = %v, want %s", got, tc.want)
				}
				if body.Targets[0]["Name"] != "target-1" || body.Targets[0]["Phase"] == nil {
					t.Error("status metadata missing")
				}
				if tc.name == "readonly" {
					for _, secret := range []string{"fixture-user", "fixture-password", "fixture-path-secret", "fixture-query", "fixture-fragment"} {
						if strings.Contains(rec.Body.String(), secret) {
							t.Errorf("response contains %q", secret)
						}
					}
				}
			})
		}
	}
	update := []byte(`{"custom_headers":{"X-fixture-header-name":"fixture-header-secret"},"custom_body":{"fixture-body-name":{"value":"fixture-body-secret"}}}`)
	for _, tc := range []struct {
		cookie *http.Cookie
		want   int
	}{{readonlyCookie, http.StatusForbidden}, {adminCookie, http.StatusOK}} {
		req := httptest.NewRequest(http.MethodPut, "/admin/data/targets/target-1", bytes.NewReader(update))
		req.AddCookie(tc.cookie)
		rec := httptest.NewRecorder()
		secured.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("update status = %d, want %d", rec.Code, tc.want)
		}
	}
	updated, _ := get(adminCookie)
	if !reflect.DeepEqual(admin["custom_headers"], updated["custom_headers"]) || !reflect.DeepEqual(admin["custom_body"], updated["custom_body"]) || admin["endpoint"] != updated["endpoint"] || admin["openai_prefix"] != updated["openai_prefix"] || admin["anthropic_prefix"] != updated["anthropic_prefix"] {
		t.Error("admin edit did not preserve target configuration")
	}
}

func TestIssue75ReadonlyTargetEndpointInvalid(t *testing.T) {
	for _, raw := range []string{"", "not a url?secret=value", "https://example.invalid/%zz", "https:opaque-secret", "ftp://example.invalid/secret"} {
		if got := readonlyTargetEndpoint(raw); got != "" {
			t.Errorf("invalid endpoint %q returned %q", raw, got)
		}
	}
}
