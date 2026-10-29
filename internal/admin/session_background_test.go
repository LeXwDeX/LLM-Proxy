package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/version"
)

func newSlidingSessions() *SessionManager {
	return NewSessionManager(config.AdminSessionConfig{CookieName: "bg_session", Secret: "bg-secret", TTLSeconds: 3600, SlidingExpiration: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Background polls (X-Admin-Background: 1) must not renew a sliding session,
// otherwise an unattended tab keeps the login alive forever.
func TestBackgroundRequestsDoNotSlideSession(t *testing.T) {
	m := newSlidingSessions()
	session, cookie, err := m.CreateSession("alice", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	// Age the session so a renewal is observable.
	m.mu.Lock()
	m.sessions[session.ID].ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
	before := m.sessions[session.ID].ExpiresAt
	m.mu.Unlock()

	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	req := httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-Admin-Background", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("background request status = %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("background request must not re-issue the session cookie: %v", rec.Result().Cookies())
	}
	m.mu.RLock()
	after := m.sessions[session.ID].ExpiresAt
	m.mu.RUnlock()
	if !after.Equal(before) {
		t.Errorf("background request extended the session: %v -> %v", before, after)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	m.mu.RLock()
	renewed := m.sessions[session.ID].ExpiresAt
	m.mu.RUnlock()
	if !renewed.After(before) || len(rec.Result().Cookies()) == 0 {
		t.Errorf("foreground request must still slide the session (expires %v, cookies %d)", renewed, len(rec.Result().Cookies()))
	}
}

func TestBackgroundRequestStillRequiresSession(t *testing.T) {
	m := newSlidingSessions()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("reached handler") }))
	req := httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req.Header.Set("X-Admin-Background", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleMeReportsServerVersion(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req = req.WithContext(ContextWithSession(req.Context(), &Session{Username: "alice", Role: RoleAdmin}))
	rec := httptest.NewRecorder()
	h.handleMe(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != version.Version {
		t.Errorf("version = %v, want %q", body["version"], version.Version)
	}
}
