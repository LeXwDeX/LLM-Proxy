package admin

import (
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

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
	if session, ok := p.sessions.CurrentSession(r); ok && session != nil {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	p.renderLoginPage(w, r, http.StatusOK, loginPageData{
		Next:  strings.TrimSpace(r.URL.Query().Get("next")),
		Error: strings.TrimSpace(r.URL.Query().Get("error")),
	})
}

// HandleLogin validates credentials and creates a session.
func (p *Portal) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.sessions == nil || p.users == nil {
		http.Error(w, "portal unavailable", http.StatusInternalServerError)
		return
	}
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

func sanitizeNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return "/admin"
	}
	if strings.HasPrefix(next, "/admin") {
		return next
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
    :root { color-scheme: light; --bg:#f5f7fb; --card:#fff; --text:#1f2a37; --line:#d9e2ef; --primary:#2563eb; --danger:#dc2626; }
    * { box-sizing: border-box; }
    html { height: 100%; }
    body { margin:0; min-height:100vh; min-height:100dvh; display:flex; align-items:center; justify-content:center; background:var(--bg); color:var(--text); font-family: -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Arial,"PingFang SC","Microsoft YaHei",sans-serif; }
    .card { width:min(420px, calc(100vw - 24px)); background:var(--card); border:1px solid var(--line); border-radius:16px; padding:28px; box-shadow:0 18px 40px rgba(15,23,42,.08); }
    h1 { margin:0 0 8px; font-size:24px; }
    p { margin:0 0 20px; color:#64748b; }
    label { display:block; margin-bottom:14px; font-size:13px; color:#475569; }
    input { width:100%; margin-top:6px; padding:12px 14px; border:1px solid #cbd5e1; border-radius:10px; font-size:14px; }
    button { width:100%; margin-top:8px; border:0; border-radius:10px; padding:12px 14px; font-size:15px; color:#fff; background:var(--primary); cursor:pointer; }
    .error { background:#fef2f2; color:var(--danger); border:1px solid #fecaca; padding:10px 12px; border-radius:10px; margin-bottom:14px; font-size:13px; }
    .hint { margin-top:14px; color:#94a3b8; font-size:12px; }
  </style>
</head>
<body>
  <form class="card" method="post" action="/login">
    <h1>后台管理登录</h1>
    <p>请输入独立后台账号密码。</p>
    {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
    <input type="hidden" name="next" value="{{.Next}}">
    <label>用户名
      <input name="username" autocomplete="username" required>
    </label>
    <label>密码
      <input name="password" type="password" autocomplete="current-password" required>
    </label>
    <button type="submit">登录</button>
    <div class="hint">登录成功后将进入 /admin 管理台。</div>
    <div class="hint" style="text-align:center;">版本 {{.Version}}</div>
  </form>
</body>
</html>`
