package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Role constants for admin session authorization.
const (
	RoleAdmin    = "admin"
	RoleReadonly = "readonly"
)

type sessionContextKey struct{}

// backgroundHeader marks requests made by UI polling rather than the user.
const backgroundHeader = "X-Admin-Background"

// Session represents a signed admin login session.
type Session struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionManager stores admin sessions in memory and signs cookies.
type SessionManager struct {
	mu       sync.RWMutex
	secret   []byte
	config   config.AdminSessionConfig
	sessions map[string]*Session
	logger   *slog.Logger
}

// NewSessionManager creates an admin session manager.
func NewSessionManager(cfg config.AdminSessionConfig, logger *slog.Logger) *SessionManager {
	return &SessionManager{
		secret:   []byte(strings.TrimSpace(cfg.Secret)),
		config:   cfg,
		sessions: make(map[string]*Session),
		logger:   logger,
	}
}

// UpdateConfig refreshes cookie/session settings.
func (m *SessionManager) UpdateConfig(cfg config.AdminSessionConfig) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.secret = []byte(strings.TrimSpace(cfg.Secret))
	m.config = cfg
	m.mu.Unlock()
}

// Config returns the current session config.
func (m *SessionManager) Config() config.AdminSessionConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// CreateSession creates a new signed session cookie.
func (m *SessionManager) CreateSession(username, role string) (*Session, *http.Cookie, error) {
	if m == nil {
		return nil, nil, errors.New("session manager is nil")
	}
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(m.Config().TTLSeconds) * time.Second)
	if role == "" {
		role = RoleAdmin
	}
	session := &Session{
		ID:        sessionID,
		Username:  strings.TrimSpace(username),
		Role:      strings.TrimSpace(role),
		CreatedAt: now,
		LastSeen:  now,
		ExpiresAt: expiresAt,
	}

	m.mu.Lock()
	// m10：登录时顺手清扫已过期会话。过期条目只在“同一 cookie 再次访问”时
	// 惰性删除，换浏览器/清 cookie 的旧会话永远不会被命中 → map 只增不减。
	// 登录频率低，全量扫描成本可忽略，无需后台 goroutine 生命周期管理。
	nowSweep := time.Now().UTC()
	for id, s := range m.sessions {
		if nowSweep.After(s.ExpiresAt) {
			delete(m.sessions, id)
		}
	}
	m.sessions[sessionID] = session
	m.mu.Unlock()

	cookie := m.newCookie(sessionID, expiresAt)
	return session, cookie, nil
}

// CurrentSession returns the session attached to the request.
func (m *SessionManager) CurrentSession(r *http.Request) (*Session, bool) {
	if m == nil || r == nil {
		return nil, false
	}
	cookie, err := r.Cookie(m.Config().CookieName)
	if err != nil {
		return nil, false
	}
	sessionID, ok := m.verifyCookie(cookie.Value)
	if !ok {
		return nil, false
	}
	return m.sessionByID(sessionID)
}

// DestroySession removes the current session and clears the cookie.
func (m *SessionManager) DestroySession(w http.ResponseWriter, r *http.Request) {
	if m == nil {
		return
	}
	if cookie, err := r.Cookie(m.Config().CookieName); err == nil {
		if sessionID, ok := m.verifyCookie(cookie.Value); ok {
			m.mu.Lock()
			delete(m.sessions, sessionID)
			m.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.Config().CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   m.Config().SecureCookie,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// RevokeByUsername removes ALL sessions belonging to the given username and
// returns the number of revoked sessions. Must be called when an admin user is
// deleted, disabled, or changes role. Password changes revoke other sessions
// while preserving the current authenticated session via revokeByUsernameExcept.
func (m *SessionManager) RevokeByUsername(username string) int {
	return m.revokeByUsernameExcept(username, "")
}

// revokeByUsernameExcept preserves only the current authenticated session when
// its owner changes their password, revoking all other devices atomically.
func (m *SessionManager) revokeByUsernameExcept(username, keepID string) int {
	if m == nil {
		return 0
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	revoked := 0
	for id, s := range m.sessions {
		if strings.EqualFold(s.Username, username) && id != keepID {
			delete(m.sessions, id)
			revoked++
		}
	}
	return revoked
}

// Middleware enforces admin session authentication.
func (m *SessionManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := m.CurrentSession(r)
		if !ok {
			m.unauthorized(w, r)
			return
		}

		// Background polls (X-Admin-Background: 1) only observe the session: they
		// must not slide its expiry, or an unattended tab never logs out.
		if r.Header.Get(backgroundHeader) == "1" {
			r = r.WithContext(ContextWithSession(r.Context(), session))
		} else if updated, ok := m.touch(session.ID); ok {
			http.SetCookie(w, m.newCookie(updated.ID, updated.ExpiresAt))
			r = r.WithContext(ContextWithSession(r.Context(), updated))
		} else {
			r = r.WithContext(ContextWithSession(r.Context(), session))
		}
		next.ServeHTTP(w, r)
	})
}

// ReadonlyGuard returns middleware that blocks mutating requests (POST/PUT/DELETE/PATCH)
// for sessions with the "readonly" role. GET/HEAD/OPTIONS remain allowed.
func (m *SessionManager) ReadonlyGuard() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if session, ok := m.CurrentSession(r); ok && session != nil && session.Role == RoleReadonly {
				switch r.Method {
				case http.MethodGet, http.MethodHead, http.MethodOptions:
					// read operations are allowed
				default:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":"forbidden: readonly role"}`))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ContextWithSession stores a session in context.
func ContextWithSession(ctx context.Context, session *Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

// SessionFromContext extracts a session from context.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	if ctx == nil {
		return nil, false
	}
	session, ok := ctx.Value(sessionContextKey{}).(*Session)
	if !ok || session == nil {
		return nil, false
	}
	return session, true
}

// isAdminSession reports whether the request carries an admin-role session.
// Sessionless calls are treated as non-admin so an internal caller cannot
// accidentally receive unmasked secret material.
func isAdminSession(r *http.Request) bool {
	session, ok := SessionFromContext(r.Context())
	return ok && session != nil && session.Role == RoleAdmin
}

// AdminSecurityHeaders marks every /admin response as non-cacheable (it carries
// plaintext secrets and session-dependent shells), non-sniffable and non-frameable.
// Mount it outside SessionManager.Middleware so 302/401 responses are covered too.
func AdminSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHardenedHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

func setHardenedHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
}

// isTopLevelNavigation reports whether the request is a browser document
// navigation. Only those may be answered with a redirect to the login page:
// browsers store a 302 given to a script/XHR subresource and replay it later,
// where HTML would be executed as JavaScript.
func isTopLevelNavigation(r *http.Request) bool {
	mode, dest := r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest")
	if mode != "" || dest != "" {
		return strings.EqualFold(mode, "navigate") || strings.EqualFold(dest, "document")
	}
	return r.Method == http.MethodGet &&
		strings.Contains(r.Header.Get("Accept"), "text/html") &&
		!strings.HasPrefix(r.URL.Path, "/admin/ui/")
}

func (m *SessionManager) unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// m12：数据接口挂在 /admin/data/*，且 UI 的 fetch 只带 X-Requested-With
	// 不带 Accept。会话过期后非导航请求必须得到 401，而不是可被缓存的 302。
	if strings.HasPrefix(r.URL.Path, "/admin/api/") ||
		strings.HasPrefix(r.URL.Path, "/admin/data/") ||
		strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") ||
		!isTopLevelNavigation(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	target := "/login"
	if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
		next := r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		if sanitizeNext(next) == next {
			target += "?next=" + url.QueryEscape(next)
		}
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (m *SessionManager) sessionByID(sessionID string) (*Session, bool) {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if now.After(session.ExpiresAt) {
		delete(m.sessions, sessionID)
		return nil, false
	}
	cloned := *session
	return &cloned, true
}

func (m *SessionManager) touch(sessionID string) (*Session, bool) {
	if !m.Config().SlidingExpiration {
		if session, ok := m.sessionByID(sessionID); ok {
			return session, true
		}
		return nil, false
	}

	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[sessionID]
	if !ok || now.After(session.ExpiresAt) {
		delete(m.sessions, sessionID)
		return nil, false
	}
	session.LastSeen = now
	session.ExpiresAt = now.Add(time.Duration(m.config.TTLSeconds) * time.Second)
	cloned := *session
	return &cloned, true
}

func (m *SessionManager) newCookie(sessionID string, expiresAt time.Time) *http.Cookie {
	cfg := m.Config()
	value := m.sign(sessionID)
	return &http.Cookie{
		Name:     cfg.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cfg.SecureCookie,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		Expires:  expiresAt,
	}
}

func (m *SessionManager) sign(sessionID string) string {
	mac := hmac.New(sha256.New, m.secretBytes())
	_, _ = mac.Write([]byte(sessionID))
	return sessionID + "." + hex.EncodeToString(mac.Sum(nil))
}

func (m *SessionManager) verifyCookie(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", false
	}
	expected := m.sign(parts[0])
	if subtleConstantTimeStringCompare(expected, value) {
		return parts[0], true
	}
	return "", false
}

func (m *SessionManager) secretBytes() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]byte(nil), m.secret...)
}

func generateSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func subtleConstantTimeStringCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
