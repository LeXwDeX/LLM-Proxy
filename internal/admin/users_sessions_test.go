package admin

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/go-chi/chi/v5"
)

func TestRoleChangeRevokesExistingAuthenticatedSession(t *testing.T) {
	base, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "target", []string{"mock-only"})
	h := base.(*Handler)
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "review_session", Secret: "local-test-secret", TTLSeconds: 3600, SlidingExpiration: true}, slog.Default())
	h.sessionManager = sessions
	h.router = h.routes()
	router := chi.NewRouter()
	router.Use(sessions.Middleware)
	router.Mount("/", h)
	for _, username := range []string{"admin", "operator"} {
		if err := stores.userStore.Create(config.AdminUser{Username: username, Role: RoleAdmin, PasswordHash: HashPassword("local-test-password", "test-salt")}); err != nil {
			t.Fatal(err)
		}
	}
	_, actorCookie, err := sessions.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, operatorCookie, err := sessions.CreateSession("operator", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	if response := do(http.MethodGet, "/data/users", "", operatorCookie); response.Code != http.StatusOK {
		t.Fatalf("initial admin session: %d", response.Code)
	}
	if response := do(http.MethodPut, "/data/users/OpErAtOr", `{"role":"readonly"}`, actorCookie); response.Code != http.StatusOK {
		t.Fatalf("role update: %d %s", response.Code, response.Body.String())
	}
	user, err := stores.userStore.Get("operator")
	if err != nil || user.Role != RoleReadonly {
		t.Fatalf("stored role was not changed: %v", err)
	}
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/data/users", ""},
		{http.MethodPost, "/data/project-groups", `{"name":"must-not-create"}`},
	} {
		response := do(request.method, request.path, request.body, operatorCookie)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("revoked session retained access to %s %s: %d", request.method, request.path, response.Code)
		}
	}
	_, readonlyCookie, err := sessions.CreateSession("operator", user.Role)
	if err != nil {
		t.Fatal(err)
	}
	if response := do(http.MethodGet, "/api/me", "", readonlyCookie); response.Code != http.StatusOK {
		t.Fatalf("fresh readonly session cannot read: %d", response.Code)
	}
	if response := do(http.MethodPost, "/data/project-groups", `{"name":"must-not-create"}`, readonlyCookie); response.Code != http.StatusForbidden {
		t.Fatalf("fresh readonly session can write: %d", response.Code)
	}
}

func TestOwnPasswordChangeRevokesOtherAuthenticatedSessions(t *testing.T) {
	for _, path := range []string{"/api/change-password", "/data/users/operator", "/data/users/OpErAtOr"} {
		t.Run(path, func(t *testing.T) {
			base, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "target", []string{"mock-only"})
			h := base.(*Handler)
			sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "review_session", Secret: "local-test-secret", TTLSeconds: 3600, SlidingExpiration: true}, slog.Default())
			h.sessionManager = sessions
			h.router = h.routes()
			router := chi.NewRouter()
			router.Use(sessions.Middleware)
			router.Mount("/", h)
			if err := stores.userStore.Create(config.AdminUser{Username: "operator", Role: RoleAdmin, PasswordHash: HashPassword("local-test-password", "test-salt")}); err != nil {
				t.Fatal(err)
			}
			_, currentCookie, err := sessions.CreateSession("operator", RoleAdmin)
			if err != nil {
				t.Fatal(err)
			}
			_, otherCookie, err := sessions.CreateSession("operator", RoleAdmin)
			if err != nil {
				t.Fatal(err)
			}
			do := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set("Accept", "application/json")
				request.AddCookie(cookie)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				return response
			}
			method, body := http.MethodPost, `{"old_password":"local-test-password","new_password":"local-new-password"}`
			if strings.HasPrefix(path, "/data/") {
				method, body = http.MethodPut, `{"password":"local-new-password"}`
			}
			if response := do(method, path, body, currentCookie); response.Code != http.StatusOK {
				t.Fatalf("password change: %d %s", response.Code, response.Body.String())
			}
			if _, err := AuthenticateUser(stores.userStore, "operator", "local-new-password"); err != nil {
				t.Fatal(err)
			}
			if response := do(http.MethodGet, "/api/me", "", currentCookie); response.Code != http.StatusOK {
				t.Fatalf("current password-change session was lost: %d", response.Code)
			}
			if response := do(http.MethodGet, "/api/me", "", otherCookie); response.Code != http.StatusUnauthorized {
				t.Fatalf("other cookie survived password change: %d", response.Code)
			}
		})
	}
}

func TestUserManagementGuardsUseStoredIdentity(t *testing.T) {
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPut, "/data/users/AdMiN", `{"role":"readonly"}`},
		{http.MethodDelete, "/data/users/AdMiN", ""},
		{http.MethodPut, "/data/users/OpErAtOr", `{"role":"readonly"}`},
		{http.MethodDelete, "/data/users/OpErAtOr", ""},
	} {
		t.Run(request.method+request.path, func(t *testing.T) {
			h, stores, _ := newNamedTargetUpdateTestHandler(t, t.TempDir(), "target", []string{"mock-only"})
			for _, username := range []string{"admin", "operator"} {
				if err := stores.userStore.Create(config.AdminUser{Username: username, Role: RoleAdmin, PasswordHash: "local-test-hash"}); err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
			r = r.WithContext(ContextWithSession(r.Context(), &Session{Username: "operator", Role: RoleAdmin}))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("identity guard bypass: %d %s", response.Code, response.Body.String())
			}
			for _, username := range []string{"admin", "operator"} {
				user, err := stores.userStore.Get(username)
				if err != nil || user.Role != RoleAdmin {
					t.Fatalf("protected user changed: %s %v", username, err)
				}
			}
		})
	}
}
