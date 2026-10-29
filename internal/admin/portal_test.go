package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/version"
)

func newPortalTestFixture(t *testing.T) (*Portal, *nosql.UserStore) {
	t.Helper()
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatalf("open portal test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := NewSessionManager(config.AdminSessionConfig{
		CookieName:   "portal_test_session",
		Secret:       "portal-test-secret",
		TTLSeconds:   3600,
		SecureCookie: false,
	}, logger)
	users := nosql.NewUserStore(db)
	return NewPortal(users, sessions, nosql.NewAuditStore(db), logger), users
}

func renderPortalLoginPage(t *testing.T, portal *Portal, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	portal.HandleLoginPage(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func postLogin(t *testing.T, portal *Portal, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	portal.HandleLogin(recorder, request)
	return recorder
}

func loginCSSBlock(t *testing.T, html, selector string) string {
	t.Helper()
	match := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(selector) + `\s*\{(.*?)\}`).FindStringSubmatch(html)
	if match == nil {
		t.Fatalf("login page missing CSS block %s", selector)
	}
	return match[1]
}

func loginDarkCSSBlock(t *testing.T, html string) string {
	t.Helper()
	match := regexp.MustCompile(`(?s)@media\s*\(prefers-color-scheme:\s*dark\)\s*\{\s*:root\s*\{(.*?)\}\s*\}`).FindStringSubmatch(html)
	if match == nil {
		t.Fatalf("login page missing prefers-color-scheme dark token block")
	}
	return match[1]
}

func loginCSSValue(t *testing.T, block, name string) string {
	t.Helper()
	match := regexp.MustCompile(`--` + regexp.QuoteMeta(name) + `\s*:\s*([^;]+);`).FindStringSubmatch(block)
	if match == nil {
		t.Fatalf("login page CSS block missing token --%s", name)
	}
	return strings.TrimSpace(match[1])
}

func TestLoginPageRedesign(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	oldVersion := version.Version
	version.Version = "20261026.7"
	t.Cleanup(func() { version.Version = oldVersion })

	body := renderPortalLoginPage(t, portal, "/login").Body.String()

	for _, want := range []string{
		`lang="zh-CN"`,
		`method="post"`,
		`action="/login"`,
		`class="login-card"`,
		`for="username"`,
		`for="password"`,
		`name="username"`,
		`name="password"`,
		`type="password"`,
		`autocomplete="username"`,
		`autocomplete="current-password"`,
		`name="next" value="/admin"`,
		`type="submit"`,
		`class="brand-name">LLMs Proxy</span>`,
		`aria-hidden="true"`,
		`后台管理登录`,
		`忘记密码？请联系管理员重置。`,
		`版本 20261026.7`,
		`prefers-color-scheme: dark`,
		`-apple-system`,
		`BlinkMacSystemFont`,
		`"Segoe UI"`,
		`"PingFang SC"`,
		`"Microsoft YaHei"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}

	if got := strings.Count(body, "<form"); got != 1 {
		t.Errorf("login page must contain exactly one form, got %d", got)
	}
	if got := strings.Count(body, `class="login-card"`); got != 1 {
		t.Errorf("login page must contain exactly one centered login card, got %d", got)
	}
	if regexp.MustCompile(`(?i)<p(?:\s|>)`).MatchString(body) {
		t.Error("login page must not contain helper or marketing paragraphs")
	}
	for _, removed := range []string{
		"请输入独立后台账号密码",
		"登录成功后将进入",
		".hint",
		`style="text-align:center;"`,
		"#f5f7fb",
		"#64748b",
		"#cbd5e1",
		"#fef2f2",
		"#fecaca",
		"#94a3b8",
		"#2563eb",
		"#dc2626",
	} {
		if strings.Contains(body, removed) {
			t.Errorf("login page still contains removed legacy content %q", removed)
		}
	}
	for _, forbidden := range []string{"<link", "<script", "@import", "url(", "http://", "https://"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("login page must not load external resources, found %q", forbidden)
		}
	}
	if strings.Contains(body, `value="admin"`) {
		t.Error("login page must not prefill a fake username")
	}
	if strings.Contains(body, "<a") {
		t.Error("login page must not offer a reset or external link; it only names the admin contact")
	}
	username := regexp.MustCompile(`<input[^>]*name="username"[^>]*>`).FindString(body)
	if username == "" {
		t.Fatal("login page username input is missing")
	}
	if strings.Contains(username, "value=") {
		t.Errorf("username input must not be prefilled: %s", username)
	}
	if strings.Contains(body, `role="alert"`) {
		t.Error("login page must not render an alert without an error")
	}
	if got := loginCSSValue(t, loginCSSBlock(t, body, ":root"), "bg"); got != "#F4F5F7" {
		t.Errorf("light --bg = %q, want #F4F5F7", got)
	}
	if got := loginCSSValue(t, loginCSSBlock(t, body, ":root"), "accent"); got != "#2B59D9" {
		t.Errorf("light --accent = %q, want #2B59D9", got)
	}
	if got := loginCSSValue(t, loginDarkCSSBlock(t, body), "bg"); got != "#0D0F12" {
		t.Errorf("dark --bg = %q, want #0D0F12", got)
	}
	if got := loginCSSValue(t, loginDarkCSSBlock(t, body), "surface"); got != "#15181D" {
		t.Errorf("dark --surface = %q, want #15181D", got)
	}
	// A 360px card felt cramped on desktop; keep it roomy but let phones shrink it.
	card := loginCSSBlock(t, body, ".login-card")
	width := regexp.MustCompile(`max-width:(\d+)px`).FindStringSubmatch(card)
	if width == nil {
		t.Fatalf(".login-card must declare a px max-width: %s", card)
	}
	if n, _ := strconv.Atoi(width[1]); n < 420 || n > 480 {
		t.Errorf(".login-card max-width = %spx, want 420–480px", width[1])
	}
	if !strings.Contains(card, "width:100%") {
		t.Error(".login-card must keep width:100% so narrow screens are not overflowed")
	}
	if !regexp.MustCompile(`@media \(max-width:480px\) \{ \.login-card \{ padding:24px 20px; \} \}`).MatchString(body) {
		t.Error("narrow screens must reduce .login-card padding")
	}
}

func TestLoginPagePreservesNext(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	next := "/admin/clients?tab=1&view=all"
	body := renderPortalLoginPage(t, portal, "/login?next="+url.QueryEscape(next)).Body.String()
	if !strings.Contains(body, `name="next" value="/admin/clients?tab=1&amp;view=all"`) {
		t.Errorf("login page did not preserve the quoted next target: %s", body)
	}
}

func TestLoginFailureKeepsAlertMarkup(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	body := postLogin(t, portal, url.Values{"username": {"nobody"}, "password": {"bad"}}).Body.String()
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, `class="login-error"`) {
		t.Error("login failure must keep the alert markup and style hook")
	}
}

func TestLoginPostSessionLifecycle(t *testing.T) {
	portal, users := newPortalTestFixture(t)
	if err := users.Create(config.AdminUser{
		Username:     "operator",
		Role:         RoleAdmin,
		PasswordHash: HashPassword("local-test-password", "test-salt"),
	}); err != nil {
		t.Fatalf("create test user: %v", err)
	}

	failed := postLogin(t, portal, url.Values{
		"username": {"operator"},
		"password": {"wrong-password"},
		"next":     {"/admin/quota"},
	})
	if failed.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want %d", failed.Code, http.StatusUnauthorized)
	}
	if cookies := failed.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("wrong password issued cookies: %v", cookies)
	}
	failedBody := failed.Body.String()
	for _, want := range []string{"用户名或密码错误", `role="alert"`, `name="next" value="/admin/quota"`} {
		if !strings.Contains(failedBody, want) {
			t.Errorf("failed login page missing %q", want)
		}
	}

	next := "/admin/quota?tab=1&view=all"
	success := postLogin(t, portal, url.Values{
		"username": {"operator"},
		"password": {"local-test-password"},
		"next":     {next},
	})
	if success.Code != http.StatusFound {
		t.Fatalf("successful login status = %d, want %d (%s)", success.Code, http.StatusFound, success.Body.String())
	}
	if location := success.Header().Get("Location"); location != next {
		t.Fatalf("successful login redirect = %q, want %q", location, next)
	}
	cookies := success.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("successful login cookies = %v, want one session cookie", cookies)
	}
	sessionCookie := cookies[0]
	if sessionCookie.Value == "" || sessionCookie.MaxAge < 0 {
		t.Fatalf("successful login did not issue a live session cookie: %+v", sessionCookie)
	}

	authenticatedPage := httptest.NewRequest(http.MethodGet, "/login", nil)
	authenticatedPage.AddCookie(sessionCookie)
	authenticatedRecorder := httptest.NewRecorder()
	portal.HandleLoginPage(authenticatedRecorder, authenticatedPage)
	if authenticatedRecorder.Code != http.StatusFound {
		t.Fatalf("authenticated login page status = %d, want redirect", authenticatedRecorder.Code)
	}
	if location := authenticatedRecorder.Header().Get("Location"); location != "/admin" {
		t.Fatalf("authenticated login page redirect = %q, want /admin", location)
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutRequest.AddCookie(sessionCookie)
	logoutRecorder := httptest.NewRecorder()
	portal.HandleLogout(logoutRecorder, logoutRequest)
	if logoutRecorder.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want %d", logoutRecorder.Code, http.StatusFound)
	}
	if location := logoutRecorder.Header().Get("Location"); location != "/login" {
		t.Fatalf("logout redirect = %q, want /login", location)
	}
	cleared := logoutRecorder.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout did not clear the session cookie: %v", cleared)
	}

	afterLogout := httptest.NewRequest(http.MethodGet, "/login", nil)
	afterLogout.AddCookie(sessionCookie)
	afterLogoutRecorder := httptest.NewRecorder()
	portal.HandleLoginPage(afterLogoutRecorder, afterLogout)
	if afterLogoutRecorder.Code != http.StatusOK {
		t.Fatalf("destroyed session still authenticates /login: status %d", afterLogoutRecorder.Code)
	}

	for _, externalNext := range []string{"https://example.com/", "//example.com/admin", "/not-admin"} {
		redirect := postLogin(t, portal, url.Values{
			"username": {"operator"},
			"password": {"local-test-password"},
			"next":     {externalNext},
		})
		if redirect.Code != http.StatusFound {
			t.Fatalf("login with next %q status = %d", externalNext, redirect.Code)
		}
		if location := redirect.Header().Get("Location"); location != "/admin" {
			t.Errorf("login with next %q redirected to %q, want /admin", externalNext, location)
		}
	}
}
