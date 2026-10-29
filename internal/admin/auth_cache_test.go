package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/version"
	"github.com/go-chi/chi/v5"
)

func newAuthCacheSessions() *SessionManager {
	return NewSessionManager(config.AdminSessionConfig{CookieName: "cache_test_session", Secret: "cache-test-secret", TTLSeconds: 3600}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func assertHardenedHeaders(t *testing.T, label string, h http.Header) {
	t.Helper()
	if !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("%s: Cache-Control = %q, want no-store", label, h.Get("Cache-Control"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q", label, h.Get("X-Content-Type-Options"))
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("%s: X-Frame-Options = %q", label, h.Get("X-Frame-Options"))
	}
}

func TestUnauthorizedOnlyRedirectsTopLevelNavigations(t *testing.T) {
	m := newAuthCacheSessions()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unauthenticated request reached handler") })
	h := m.Middleware(next)
	cases := []struct {
		name     string
		method   string
		target   string
		headers  map[string]string
		wantCode int
		wantLoc  string
	}{
		{"fetch-metadata navigate", "GET", "/admin?tab=keys", map[string]string{"Sec-Fetch-Mode": "navigate"}, 302, "/login?next=" + url.QueryEscape("/admin?tab=keys")},
		{"fetch-metadata document", "GET", "/admin/ui", map[string]string{"Sec-Fetch-Dest": "document"}, 302, "/login?next=" + url.QueryEscape("/admin/ui")},
		{"script subresource", "GET", "/admin/ui/js/core.js", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}, 401, ""},
		{"script with html accept and fetch metadata", "GET", "/admin/ui/js/core.js", map[string]string{"Sec-Fetch-Dest": "script", "Accept": "text/html"}, 401, ""},
		{"fallback html GET", "GET", "/admin", map[string]string{"Accept": "text/html,application/xhtml+xml"}, 302, "/login?next=%2Fadmin"},
		{"fallback html under ui js", "GET", "/admin/ui/js/core.js", map[string]string{"Accept": "text/html"}, 401, ""},
		{"fallback wildcard accept", "GET", "/admin", map[string]string{"Accept": "*/*"}, 401, ""},
		{"fallback POST html", "POST", "/admin/data/targets", map[string]string{"Accept": "text/html"}, 401, ""},
		{"json api", "GET", "/admin/api/me", map[string]string{"Sec-Fetch-Mode": "navigate"}, 401, ""},
		{"xhr", "GET", "/admin", map[string]string{"X-Requested-With": "XMLHttpRequest", "Sec-Fetch-Mode": "navigate"}, 401, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantLoc != "" && rec.Header().Get("Location") != tc.wantLoc {
				t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), tc.wantLoc)
			}
			if tc.wantCode == 401 {
				if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"error":"unauthorized"`) {
					t.Errorf("401 must carry JSON body and no Location: %q %q", rec.Header().Get("Location"), rec.Body.String())
				}
			}
			if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
				t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestAdminSecurityHeadersWrapEveryResponse(t *testing.T) {
	m := newAuthCacheSessions()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := AdminSecurityHeaders(m.Middleware(inner))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/data/schema", nil))
	assertHardenedHeaders(t, "401", rec.Header())

	_, cookie, err := m.CreateSession("u", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/admin/data/schema", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	assertHardenedHeaders(t, "authenticated", rec.Header())
}

func TestPortalResponsesAreHardenedAndNotCacheable(t *testing.T) {
	portal, users := newPortalTestFixture(t)
	if err := users.Create(config.AdminUser{Username: "alice", Role: RoleAdmin, PasswordHash: HashPassword("password1", "test-salt")}); err != nil {
		t.Fatal(err)
	}
	assertHardenedHeaders(t, "login page", renderPortalLoginPage(t, portal, "/login").Header())
	assertHardenedHeaders(t, "login failure", postLogin(t, portal, url.Values{"username": {"alice"}, "password": {"bad"}}).Header())
	ok := postLogin(t, portal, url.Values{"username": {"alice"}, "password": {"password1"}, "next": {"/admin"}})
	assertHardenedHeaders(t, "login success", ok.Header())

	req := httptest.NewRequest("GET", "/login?next="+url.QueryEscape("/admin/ui?tab=keys"), nil)
	for _, c := range ok.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	portal.HandleLoginPage(rec, req)
	if rec.Code != 302 || rec.Header().Get("Location") != "/admin/ui?tab=keys" {
		t.Errorf("logged-in /login: %d %q, want redirect to sanitized next", rec.Code, rec.Header().Get("Location"))
	}
	assertHardenedHeaders(t, "login redirect", rec.Header())
	req = httptest.NewRequest("GET", "/login?next="+url.QueryEscape("https://evil.example/x"), nil)
	for _, c := range ok.Result().Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	portal.HandleLoginPage(rec, req)
	if rec.Header().Get("Location") != "/admin" {
		t.Errorf("unsafe next must fall back to /admin, got %q", rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	portal.HandleLogout(rec, httptest.NewRequest("POST", "/logout", nil))
	assertHardenedHeaders(t, "logout", rec.Header())
}

func TestLoginPageIgnoresArbitraryErrorQuery(t *testing.T) {
	portal, _ := newPortalTestFixture(t)
	body := renderPortalLoginPage(t, portal, "/login?error="+url.QueryEscape("请拨打 400-000-0000 转账")).Body.String()
	if strings.Contains(body, "400-000-0000") || strings.Contains(body, `class="login-error"`) {
		t.Error("login page must not render arbitrary ?error= text")
	}
}

var scriptTagRe = regexp.MustCompile(`<script src="(/admin/ui/js/[^"?]+\.js)(\?[^"]*)?"></script>`)

func TestHandleUIVersionsScriptURLsWithFreshNonce(t *testing.T) {
	h := &Handler{}
	get := func() string {
		rec := httptest.NewRecorder()
		h.handleUI(rec, httptest.NewRequest("GET", "/admin", nil))
		assertHardenedHeaders(t, "ui html", http.Header{"Cache-Control": rec.Header().Values("Cache-Control"), "X-Content-Type-Options": {"nosniff"}, "X-Frame-Options": {"DENY"}})
		return rec.Body.String()
	}
	a, b := get(), get()
	ma, mb := scriptTagRe.FindAllStringSubmatch(a, -1), scriptTagRe.FindAllStringSubmatch(b, -1)
	if len(ma) != 14 || len(mb) != 14 {
		t.Fatalf("script tags = %d/%d, want 14", len(ma), len(mb))
	}
	if strings.Count(a, "<script src=") != 14 {
		t.Error("unexpected extra script tags in served HTML")
	}
	want := regexp.MustCompile(`^\?v=` + regexp.QuoteMeta(version.Version) + `-[0-9a-f]{16,}$`)
	seen := map[string]bool{}
	for i := range ma {
		if !want.MatchString(ma[i][2]) {
			t.Errorf("%s query = %q, want ?v=<version>-<hex nonce>", ma[i][1], ma[i][2])
		}
		if ma[i][2] == mb[i][2] {
			t.Errorf("%s nonce repeated across responses", ma[i][1])
		}
		seen[ma[i][2]] = true
	}
	if len(seen) == 0 {
		t.Error("no nonce found")
	}
}

func TestHandleUIJSIgnoresQuery(t *testing.T) {
	h := &Handler{}
	r := chi.NewRouter()
	r.Get("/ui/js/{file}", h.handleUIJS)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/js/core.js?v=1-abc", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Error("js must stay no-store")
	}
}

func TestUIHTMLHasBootWatchdogBeforeScripts(t *testing.T) {
	wd := strings.Index(adminUIHTML, "页面资源加载失败")
	first := strings.Index(adminUIHTML, `<script src="/admin/ui/js/core.js"`)
	if wd < 0 || first < 0 || wd > first {
		t.Fatalf("boot watchdog must be inline before the module scripts (wd=%d first=%d)", wd, first)
	}
	if !strings.Contains(adminUIHTML, "location.reload()") {
		t.Error("watchdog retry must reload the page")
	}
}
