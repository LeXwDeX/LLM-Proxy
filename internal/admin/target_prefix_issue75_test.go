package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
)

func TestIssue75ProtocolPrefixesAreHiddenForReadonly(t *testing.T) {
	for _, tc := range []struct{ name, openai, anthropic string }{
		{"url-shaped", "https://fixture-openai-host-secret/suffix?key=fixture-openai-query", "http://fixture-anthropic-host-secret/suffix?key=fixture-anthropic-query"},
		{"path", "/fixture-openai-path-secret", "/fixture-anthropic-path-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(dir, 1, nil)
			cfg.Targets[0].EndpointType = "dual_protocol"
			cfg.Targets[0].ResourcePathPrefix = ""
			cfg.Targets[0].OpenAIPrefix, cfg.Targets[0].AnthropicPrefix = tc.openai, tc.anthropic
			path := filepath.Join(dir, "config.json")
			writeConfigFile(t, path, cfg)
			stores := setupTestStores(t, dir)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			service, err := proxy.NewService(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			sessions := NewSessionManager(config.AdminSessionConfig{Secret: "fixture-session-secret", CookieName: "session", TTLSeconds: 3600}, logger)
			h := NewHandler(config.NewManager(path), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessions, logger)
			router := chi.NewRouter()
			router.Route("/admin", func(r chi.Router) { r.Use(sessions.Middleware); r.Mount("/", h) })
			for _, role := range []string{RoleReadonly, RoleAdmin} {
				_, cookie, err := sessions.CreateSession(role, role)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodGet, "/admin/data/targets", nil)
				req.AddCookie(cookie)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s status = %d", role, rec.Code)
				}
				var body struct {
					Targets []map[string]any `json:"targets"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Targets) != 1 {
					t.Fatalf("invalid response: %v", err)
				}
				for field, raw := range map[string]string{"openai_prefix": tc.openai, "anthropic_prefix": tc.anthropic} {
					want := raw
					if role == RoleReadonly {
						want = ""
					}
					if got, exists := body.Targets[0][field]; !exists || got != want {
						t.Errorf("%s %s = %v, present=%v, want %q", role, field, got, exists, want)
					}
				}
				if role == RoleReadonly && strings.Contains(rec.Body.String(), "fixture-openai-") {
					t.Error("readonly response contains OpenAI prefix")
				}
				if role == RoleReadonly && strings.Contains(rec.Body.String(), "fixture-anthropic-") {
					t.Error("readonly response contains Anthropic prefix")
				}
			}
		})
	}
}
