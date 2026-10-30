package admin

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 门户在依赖缺失时必须显式返回 500，而不是 panic 或开放未认证路径。
func TestPortalHandlersRejectMissingDependencies(t *testing.T) {
	var nilPortal *Portal
	for _, portal := range []*Portal{nilPortal, &Portal{}} {
		for name, handler := range map[string]http.HandlerFunc{
			"login page": portal.HandleLoginPage,
			"login":      portal.HandleLogin,
			"logout":     portal.HandleLogout,
		} {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "portal unavailable") {
				t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestHandleLoginRejectsMalformedForm(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	portal.HandleLogin(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid login payload") {
		t.Fatalf("malformed form: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCurrentSessionAuthenticatedAndAnonymous(t *testing.T) {
	portal, _ := newPortalTestFixture(t)

	anonymous := httptest.NewRecorder()
	portal.HandleCurrentSession(anonymous, httptest.NewRequest(http.MethodGet, "/api/session", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anonymous.Code)
	}
	var anonymousBody struct {
		Authenticated bool `json:"authenticated"`
	}
	decodeAdminJSON(t, anonymous, &anonymousBody)
	if anonymousBody.Authenticated {
		t.Fatalf("anonymous body = %s, want authenticated=false", anonymous.Body.String())
	}

	session, cookie, err := portal.sessions.CreateSession("operator", RoleReadonly)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	portal.HandleCurrentSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Authenticated bool      `json:"authenticated"`
		Username      string    `json:"username"`
		Role          string    `json:"role"`
		ExpiresAt     time.Time `json:"expires_at"`
	}
	decodeAdminJSON(t, rec, &body)
	if !body.Authenticated || body.Username != "operator" || body.Role != RoleReadonly || !body.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("authenticated body = %s", rec.Body.String())
	}
}

// 模板渲染失败时回退到静态错误页，并记录错误日志。
func TestRenderLoginPageWriteFailureFallsBack(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	var logs bytes.Buffer
	portal.logger = slog.New(slog.NewTextHandler(&logs, nil))
	writer := &failingResponseWriter{header: make(http.Header)}
	portal.renderLoginPage(writer, httptest.NewRequest(http.MethodGet, "/login", nil), http.StatusOK, loginPageData{})
	if !strings.Contains(writer.body.String(), "login page unavailable") {
		t.Fatalf("fallback body = %q", writer.body.String())
	}
	if !strings.Contains(logs.String(), "failed to render login page") {
		t.Fatalf("render failure was not logged: %s", logs.String())
	}
}

type failingResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *failingResponseWriter) Header() http.Header { return w.header }

func (w *failingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	_, _ = w.body.Write(p)
	return 0, errors.New("response writer is closed")
}

// recordAudit 对缺失审计存储与空操作者保持静默，空操作者记为 unknown。
func TestPortalRecordAuditUnknownActorAndMissingStore(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	portal.recordAudit(req, "", "login", "failure", "empty actor")
	events, err := portal.audit.List(10)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit events = %+v (err %v), want 1", events, err)
	}
	if events[0].Actor != "unknown" || events[0].Action != "login" || events[0].Result != "failure" {
		t.Fatalf("audit event = %+v, want actor unknown", events[0])
	}

	var nilPortal *Portal
	nilPortal.recordAudit(req, "operator", "login", "success", "nil portal")
	(&Portal{}).recordAudit(req, "operator", "login", "success", "nil store")
}

// 已认证会话访问登录页时的重定向必须经过 sanitizeNext。
func TestLoginPageRedirectUsesSanitizedNext(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	_, cookie, err := portal.sessions.CreateSession("operator", RoleAdmin)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/login?next=/admin/../outside", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	portal.HandleLoginPage(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin" {
		t.Fatalf("authenticated login page = %d %q, want 302 /admin", rec.Code, rec.Header().Get("Location"))
	}
}
