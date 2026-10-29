package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

type issue94Env struct {
	router   http.Handler
	sessions *SessionManager
	h        *Handler
	db       *bolt.DB
}

func newIssue94Env(t *testing.T) *issue94Env {
	t.Helper()
	db, err := nosql.OpenDB(t.TempDir() + "/issue94.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "s", Secret: "issue94-secret", TTLSeconds: 3600, SlidingExpiration: true}, logger)
	h := &Handler{
		clientStore: nosql.NewClientStore(db), clientQuotaStore: nosql.NewClientQuotaStore(db),
		modelGroupStore: nosql.NewModelGroupStore(db), jobLevelStore: nosql.NewJobLevelStore(db),
		dictStore: nosql.NewDictStore(db), userStore: nosql.NewUserStore(db),
		auditStore: nosql.NewAuditStore(db), authStore: auth.NewStore(), sessionManager: sessions, logger: logger,
	}
	h.router = h.routes()
	router := chi.NewRouter()
	router.Use(sessions.Middleware)
	router.Mount("/", h)
	return &issue94Env{router: router, sessions: sessions, h: h, db: db}
}

func (e *issue94Env) do(t *testing.T, role, user, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	_, cookie, err := e.sessions.CreateSession(user, role)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestListClientsMasksAccessKeyForNonAdmin(t *testing.T) {
	e := newIssue94Env(t)
	const key = "sk-plaintext-secret-1234567890"
	if err := e.h.clientStore.Create(config.Client{Name: "alice", AccessKey: key}); err != nil {
		t.Fatal(err)
	}
	read := func(role string) string {
		rec := e.do(t, role, "u-"+role, http.MethodGet, "/data/clients", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s list: %d %s", role, rec.Code, rec.Body.String())
		}
		var resp struct {
			Clients []config.Client `json:"clients"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Clients) != 1 {
			t.Fatalf("decode: %v %s", err, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), key) && role != RoleAdmin {
			t.Fatalf("%s response leaks plaintext key: %s", role, rec.Body.String())
		}
		return resp.Clients[0].AccessKey
	}
	if got := read(RoleReadonly); got != maskKey(key) {
		t.Errorf("readonly access_key = %q, want %q", got, maskKey(key))
	}
	if got := read(RoleAdmin); got != key {
		t.Errorf("admin access_key = %q, want plaintext", got)
	}
	stored, _ := e.h.clientStore.Get("alice")
	if stored.AccessKey != key {
		t.Errorf("stored key mutated: %q", stored.AccessKey)
	}
}

func TestUpdateUserRejectsSelfDisable(t *testing.T) {
	e := newIssue94Env(t)
	if err := e.h.userStore.Create(config.AdminUser{Username: "operator", Role: RoleAdmin, PasswordHash: HashPassword("local-test-password", "salt")}); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/users/operator", `{"disabled":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot disable your own account") {
		t.Fatalf("self-disable: %d %s", rec.Code, rec.Body.String())
	}
	u, _ := e.h.userStore.Get("operator")
	if u.Disabled {
		t.Fatal("user was disabled")
	}
	if rec := e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/users/operator", `{"disabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("self disabled:false: %d %s", rec.Code, rec.Body.String())
	}
	_ = e.h.userStore.Create(config.AdminUser{Username: "other", Role: RoleReadonly, PasswordHash: HashPassword("local-test-password", "salt")})
	if rec := e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/users/other", `{"disabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("disable other: %d %s", rec.Code, rec.Body.String())
	}
}

// Names containing "/" arrive percent-encoded (%2F); every name-keyed handler
// must decode them so update and delete round-trip.
func TestSlashNamesRoundTripThroughEncodedPaths(t *testing.T) {
	e := newIssue94Env(t)
	if err := e.h.userStore.Create(config.AdminUser{Username: "operator", Role: RoleAdmin, PasswordHash: HashPassword("local-test-password", "salt")}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, collection, name0, createBody, updateBody string
	}{
		{"job level", "job-levels", "P5/P6", `{"name":"P5/P6","coefficient_daily":2}`, `{"name":"P5/P6","coefficient_daily":3}`},
		{"model group", "model-groups", "a/b", `{"name":"a/b","cost_coefficient":1}`, `{"name":"a/b","cost_coefficient":2}`},
		{"project group", "project-groups", "team/x", `{"name":"team/x"}`, `{"name":"team/x"}`},
		{"client", "clients", "c/1", `{"name":"c/1","access_key":"sk-slash-client-key-123"}`, `{"name":"c/1","access_key":"sk-slash-client-key-456"}`},
		{"user", "users", "dev/ops", `{"username":"dev/ops","password":"secret-123456","role":"readonly"}`, `{"disabled":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := "/data/" + tc.collection
			if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, base, tc.createBody); rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			enc := base + "/" + url.PathEscape(tc.name0)
			if rec := e.do(t, RoleAdmin, "operator", http.MethodPut, enc, tc.updateBody); rec.Code != http.StatusOK {
				t.Fatalf("update %s: %d %s", enc, rec.Code, rec.Body.String())
			}
			if rec := e.do(t, RoleAdmin, "operator", http.MethodDelete, enc, ""); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
				t.Fatalf("delete %s: %d %s", enc, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestQuotaClientParamDecodesSlash(t *testing.T) {
	e := newIssue94Env(t)
	if err := e.h.clientStore.Create(config.Client{Name: "q/1", AccessKey: "sk-slash-quota-key-123"}); err != nil {
		t.Fatal(err)
	}
	enc := "/data/quota/" + url.PathEscape("q/1")
	if rec := e.do(t, RoleAdmin, "op", http.MethodPut, enc, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("quota update: %d %s", rec.Code, rec.Body.String())
	}
}
