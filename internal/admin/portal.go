package admin

import (
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"log/slog"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/version"
)

// Portal exposes login/logout and session utilities for the admin backend.
type Portal struct {
	users    *nosql.UserStore
	sessions *SessionManager
	audit    *nosql.AuditStore
	logger   *slog.Logger
	loginT   *template.Template
}

// NewPortal creates a new admin portal handler helper.
func NewPortal(users *nosql.UserStore, sessions *SessionManager, audit *nosql.AuditStore, logger *slog.Logger) *Portal {
	return &Portal{
		users:    users,
		sessions: sessions,
		audit:    audit,
		logger:   logger,
		loginT:   template.Must(template.New("login").Parse(adminLoginHTML)),
	}
}

// HandleLoginPage renders the login page.
func (p *Portal) HandleLoginPage(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.sessions == nil {
		http.Error(w, "portal unavailable", http.StatusInternalServerError)
		return
	}
	setHardenedHeaders(w.Header())
	if session, ok := p.sessions.CurrentSession(r); ok && session != nil {
		http.Redirect(w, r, sanitizeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	p.renderLoginPage(w, r, http.StatusOK, loginPageData{
		Next:  strings.TrimSpace(r.URL.Query().Get("next")),
		Error: loginErrorMessages[strings.TrimSpace(r.URL.Query().Get("error"))],
	})
}

// HandleLogin validates credentials and creates a session.
func (p *Portal) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.sessions == nil || p.users == nil {
		http.Error(w, "portal unavailable", http.StatusInternalServerError)
		return
	}
	setHardenedHeaders(w.Header())
	if err := r.ParseForm(); err != nil {
		p.renderLoginPage(w, r, http.StatusBadRequest, loginPageData{Error: "invalid login payload"})
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	next := strings.TrimSpace(r.FormValue("next"))
	if next == "" {
		next = "/admin"
	}

	user, err := AuthenticateUser(p.users, username, password)
	if err != nil {
		p.recordAudit(r, username, "login", "failure", err.Error())
		p.renderLoginPage(w, r, http.StatusUnauthorized, loginPageData{
			Next:  next,
			Error: "用户名或密码错误",
		})
		return
	}

	session, cookie, err := p.sessions.CreateSession(user.Username, user.Role)
	if err != nil {
		p.renderLoginPage(w, r, http.StatusInternalServerError, loginPageData{Error: "无法创建会话"})
		return
	}
	http.SetCookie(w, cookie)
	p.recordAudit(r, session.Username, "login", "success", "session created")
	http.Redirect(w, r, sanitizeNext(next), http.StatusFound)
}

// HandleLogout destroys the session and redirects to login.
func (p *Portal) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.sessions == nil {
		http.Error(w, "portal unavailable", http.StatusInternalServerError)
		return
	}
	setHardenedHeaders(w.Header())
	if session, ok := p.sessions.CurrentSession(r); ok && session != nil {
		p.recordAudit(r, session.Username, "logout", "success", "session destroyed")
	}
	p.sessions.DestroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// HandleCurrentSession exposes current admin session info.
func (p *Portal) HandleCurrentSession(w http.ResponseWriter, r *http.Request) {
	if session, ok := p.sessions.CurrentSession(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"username":      session.Username,
			"role":          session.Role,
			"expires_at":    session.ExpiresAt,
		})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
}

func (p *Portal) renderLoginPage(w http.ResponseWriter, r *http.Request, status int, data loginPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if data.Next == "" {
		data.Next = "/admin"
	}
	if data.Version == "" {
		data.Version = version.Version
	}
	if err := p.loginT.Execute(w, data); err != nil {
		if p.logger != nil {
			p.logger.Error("failed to render login page", "error", err)
		}
		_, _ = io.WriteString(w, "<html><body>login page unavailable</body></html>")
	}
}

func (p *Portal) recordAudit(r *http.Request, actor, action, result, detail string) {
	if p == nil || p.audit == nil {
		return
	}
	if actor == "" {
		actor = "unknown"
	}
	_ = p.audit.Record(nosql.AuditEvent{
		Timestamp: time.Now().UTC(),
		Actor:     actor,
		Action:    action,
		Result:    result,
		Detail:    detail,
	})
}

// loginErrorMessages maps the only ?error= codes /login accepts to fixed text.
// No code path redirects to /login with an error today, so the table is empty;
// arbitrary query text must never be rendered (phishing/spoofing).
var loginErrorMessages = map[string]string{}

func sanitizeNext(next string) string {
	next = strings.TrimSpace(next)
	u, err := url.Parse(next)
	if err == nil && u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/") &&
		!strings.ContainsRune(u.Path, '\\') && strings.IndexFunc(u.Path, unicode.IsControl) < 0 {
		cleaned := path.Clean(u.Path)
		if cleaned == "/admin" || strings.HasPrefix(cleaned, "/admin/") {
			return next
		}
	}
	return "/admin"
}

type loginPageData struct {
	Next    string
	Error   string
	Version string
}

var adminLoginHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>后台登录</title>
  <style>
    /* Theme tokens mirror the approved admin .app palette; this page follows the OS theme. */
    :root {
      color-scheme: light;
      --bg:#F4F5F7;--surface:#FFFFFF;--line:#E3E6EB;--line-2:#EEF0F3;--ink:#14171C;--ink-2:#444C59;--ink-3:#5C6573;--accent:#2B59D9;--accent-ink:#FFFFFF;--accent-soft:#E9EEFC;--accent-hover:#1E47BE;--bad:#B42318;--bad-soft:#FCE8E6;--brand:#2B59D9;--brand-ink:#FFFFFF;
      --shadow:0 1px 3px rgba(15,23,42,.06),0 12px 32px rgba(15,23,42,.06);
    }
    @media (prefers-color-scheme: dark) {
      :root {
        color-scheme: dark;
        --bg:#0D0F12;--surface:#15181D;--line:#272C34;--line-2:#20252C;--ink:#E7EAEE;--ink-2:#B9C0CA;--ink-3:#959EAB;--accent:#7D9CFF;--accent-ink:#0D0F12;--accent-soft:#1C2645;--accent-hover:#9DB5FF;--bad:#F2796D;--bad-soft:#3A1916;
        --shadow:0 1px 3px rgba(0,0,0,.4),0 16px 40px rgba(0,0,0,.35);
      }
    }
    * { box-sizing: border-box; }
    html { height: 100%; }
    body { margin:0; min-height:100vh; min-height:100dvh; display:grid; place-items:center; padding:24px 16px; background:var(--bg); color:var(--ink); font-family:-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Hiragino Sans GB", "Segoe UI", "Microsoft YaHei UI", "Microsoft YaHei", "Noto Sans SC", "Source Han Sans SC", Roboto, "Helvetica Neue", Arial, sans-serif; -webkit-font-smoothing:antialiased; -moz-osx-font-smoothing:grayscale; text-rendering:optimizeLegibility; font-size:14px; line-height:1.5; }
    .login-card { width:100%; max-width:440px; margin:0; padding:32px 36px; background:var(--surface); border:1px solid var(--line); border-radius:10px; box-shadow:var(--shadow); }
    .brand { display:flex; align-items:center; gap:10px; margin-bottom:20px; }
    .brand-mark { width:30px; height:30px; border-radius:8px; background:var(--brand); color:var(--brand-ink); display:grid; place-items:center; flex:0 0 auto; }
    .brand-name { font-size:15px; font-weight:700; letter-spacing:-.01em; }
    h1 { margin:0 0 20px; font-size:18px; font-weight:700; line-height:1.3; text-wrap:balance; }
    .login-error { margin:0 0 16px; padding:8px 10px; border-radius:8px; background:var(--bad-soft); color:var(--bad); font-size:13px; line-height:1.5; }
    .field { margin-bottom:14px; }
    label { display:block; margin-bottom:6px; color:var(--ink-2); font-size:12px; font-weight:500; }
    input { width:100%; height:40px; padding:0 12px; background:var(--surface); color:var(--ink); border:1px solid var(--line); border-radius:8px; font:inherit; font-size:14px; caret-color:var(--accent); }
    input:hover { border-color:var(--ink-3); }
    input:focus { border-color:var(--accent); }
    input:focus-visible, button:focus-visible { outline:2px solid var(--accent); outline-offset:2px; }
    button[type=submit] { width:100%; height:40px; margin-top:8px; border:0; border-radius:8px; background:var(--accent); color:var(--accent-ink); font:inherit; font-size:14px; font-weight:600; cursor:pointer; transition:background .15s ease; }
    button[type=submit]:hover { background:var(--accent-hover); }
    button[type=submit]:active { transform:translateY(1px); }
    .login-meta { display:flex; flex-wrap:wrap; justify-content:space-between; gap:8px 12px; margin-top:20px; padding-top:14px; border-top:1px solid var(--line-2); color:var(--ink-3); font-size:12px; }
    .login-version { font-variant-numeric:tabular-nums; }
    @media (max-width:480px) { .login-card { padding:24px 20px; } }
    ::selection { background:var(--accent-soft); color:var(--ink); }
  </style>
</head>
<body>
  <main>
    <form class="login-card" method="post" action="/login">
      <div class="brand">
        <div class="brand-mark" aria-hidden="true">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="11" width="14" height="10" rx="2"></rect><path d="M8 11V8a4 4 0 0 1 8 0v3"></path></svg>
        </div>
        <span class="brand-name">LLMs Proxy</span>
      </div>
      <h1>后台管理登录</h1>
      {{if .Error}}<div class="login-error" role="alert">{{.Error}}</div>{{end}}
      <input type="hidden" name="next" value="{{.Next}}">
      <div class="field">
        <label for="username">用户名</label>
        <input id="username" name="username" type="text" autocomplete="username" autocapitalize="none" spellcheck="false" required>
      </div>
      <div class="field">
        <label for="password">密码</label>
        <input id="password" name="password" type="password" autocomplete="current-password" required>
      </div>
      <button type="submit">登录</button>
      <div class="login-meta">
        <span>忘记密码？请联系管理员重置。</span>
        <span class="login-version">版本 {{.Version}}</span>
      </div>
    </form>
  </main>
  <script>
    // Deep links: the server never sees the URL fragment, so carry it into next.
    (function () {
      var next = document.querySelector('input[name="next"]');
      if (next && location.hash && next.value.indexOf("#") < 0 && (next.value === "" || next.value.indexOf("/admin") === 0))
        next.value = (next.value || "/admin") + location.hash;
    })();
  </script>
</body>
</html>`
