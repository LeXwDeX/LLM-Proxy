package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/admin"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestRouterAuthenticationBoundaries(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := admin.NewSessionManager(config.AdminSessionConfig{CookieName: "fixture_session", Secret: "fixture-session-signing-key", TTLSeconds: 3600}, logger)
	clients := auth.NewStore()
	if err := clients.LoadFromConfig([]config.Client{{Name: "fixture-client", AccessKey: "fixture-client-key"}}); err != nil {
		t.Fatal(err)
	}
	adminCalls, proxyCalls := 0, 0
	router := newRouter(routeDependencies{
		Sessions: sessions, Clients: clients, Portal: admin.NewPortal(nil, sessions, nil, logger),
		Admin: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if session, ok := admin.SessionFromContext(r.Context()); !ok || session.Role != admin.RoleReadonly {
				t.Error("admin handler lost its authenticated readonly session")
			}
			adminCalls++
			w.WriteHeader(http.StatusOK)
		}),
		Proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if principal, ok := auth.PrincipalFromContext(r.Context()); !ok || principal.Name != "fixture-client" {
				t.Error("proxy handler lost its client principal")
			}
			proxyCalls++
			w.WriteHeader(http.StatusAccepted)
		}),
		AppLogger: logger, AccessLogger: logger,
	})
	for _, path := range []string{"/healthz", "/login"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("public %s status=%d", path, rec.Code)
		}
	}
	for _, path := range []string{"/admin/data/schema", "/admin/ui/js/core.js"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		want := http.StatusUnauthorized // non-navigation requests never get a cacheable redirect
		if rec.Code != want || adminCalls != 0 {
			t.Errorf("unauthenticated %s reached admin: status=%d calls=%d", path, rec.Code, adminCalls)
		}
	}
	nav := httptest.NewRequest(http.MethodGet, "/admin", nil)
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, nav)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?next=%2Fadmin" || adminCalls != 0 {
		t.Errorf("top-level navigation: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	for _, h := range []string{"Cache-Control", "X-Content-Type-Options", "X-Frame-Options"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("unauthenticated /admin response missing %s", h)
		}
	}
	_, cookie, err := sessions.CreateSession("fixture-auditor", admin.RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/data/schema", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("authenticated admin response lacks hardening headers: %v", rec.Header())
	}
	if rec.Code != http.StatusOK || adminCalls != 1 {
		t.Errorf("authenticated admin read failed: status=%d calls=%d", rec.Code, adminCalls)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusUnauthorized || proxyCalls != 0 {
		t.Errorf("unauthenticated proxy request was forwarded: status=%d calls=%d", rec.Code, proxyCalls)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer fixture-client-key")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || proxyCalls != 1 {
		t.Errorf("authenticated proxy request failed: status=%d calls=%d", rec.Code, proxyCalls)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer fixture-client-key")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var ping map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &ping); err != nil || rec.Code != http.StatusOK || ping["client"] != "fixture-client" {
		t.Errorf("authenticated ping failed: status=%d error=%v", rec.Code, err)
	}
}
