package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func auditActions(t *testing.T, e *revEnv, action string) int {
	t.Helper()
	events, err := e.stores.auditStore.List(50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Action == action {
			n++
		}
	}
	return n
}

func TestUnmuteTargetSucceedsAndAudits(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	rec := e.do(http.MethodPost, "targets/Target-A/unmute", "", nil) // 大小写不敏感
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("unmute: %d %q", rec.Code, rec.Body.String())
	}
	events, err := e.stores.auditStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "unmute_target" && ev.Object == "Target-A" && ev.Result == "success" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing unmute_target audit event: %+v", events)
	}
}

func TestUnmuteTargetNotFound(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	rec := e.do(http.MethodPost, "targets/ghost/unmute", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unmute ghost: %d %s", rec.Code, rec.Body.String())
	}
	if n := auditActions(t, e, "unmute_target"); n != 0 {
		t.Fatalf("not-found unmute must not be audited as success, got %d events", n)
	}
}

func TestUnmuteTargetReadonlyForbidden(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), true)
	router := chi.NewRouter()
	router.Use(e.sm.Middleware)
	router.Mount("/", e.h)
	_, roCookie, err := e.sm.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	_, adminCookie, err := e.sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	do := func(c *http.Cookie) int {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/data/targets/target-a/unmute", nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(roCookie); code != http.StatusForbidden {
		t.Fatalf("readonly unmute expected 403, got %d", code)
	}
	if n := auditActions(t, e, "unmute_target"); n != 0 {
		t.Fatalf("rejected readonly request wrote %d audit events", n)
	}
	if code := do(adminCookie); code != http.StatusOK {
		t.Fatalf("admin unmute expected 200, got %d", code)
	}
	if n := auditActions(t, e, "unmute_target"); n != 1 {
		t.Fatalf("admin unmute audit events = %d, want 1", n)
	}
}
