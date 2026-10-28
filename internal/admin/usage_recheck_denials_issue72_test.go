package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RecheckPreflightDenialsAreAudited(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	event := usage.Event{RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Timestamp: time.Now().UTC(), ClientName: "client"}
	original, _ := json.Marshal(event)
	if err := stores.usageStore.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	before, _, err := stores.usageStore.ListUsageRechecks(0, 100)
	if err != nil || len(before) != 1 {
		t.Fatalf("evidence setup: %+v %v", before, err)
	}
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "test", Secret: "test-secret", TTLSeconds: 3600}, nil)
	h := &Handler{usageStore: stores.usageStore, auditStore: stores.auditStore, sessionManager: sessions}
	h.router = h.routes()
	handler := sessions.Middleware(h)
	_, readonly, _ := sessions.CreateSession("reader", RoleReadonly)
	_, other, _ := sessions.CreateSession("other-user", "unrecognized")
	_, admin, _ := sessions.CreateSession("real-admin", RoleAdmin)
	tests := []struct {
		name          string
		cookie        *http.Cookie
		body          string
		status        int
		actor, reason string
	}{
		{"readonly", readonly, `{"actor":"attacker-secret"}`, 403, "reader", "readonly"},
		{"other role", other, `{"actor":"attacker-secret"}`, 403, "other-user", "admin_required"},
		{"forged actor", admin, `{"actor":"attacker-secret"}`, 400, "real-admin", "invalid_request"},
		{"invalid fields", admin, `{"evidence_id":"attacker-secret"}`, 400, "real-admin", "invalid_request"},
		{"malformed json", admin, `attacker-secret`, 400, "real-admin", "invalid_request"},
		{"duplicate fields", admin, `{"operation_id":"a","operation_id":"attacker-secret"}`, 400, "real-admin", "invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prior, err := stores.auditStore.List(100)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/data/usage/recheck", strings.NewReader(tc.body))
			r.AddCookie(tc.cookie)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			events, err := stores.auditStore.List(100)
			if err != nil {
				t.Fatal(err)
			}
			found, priorFound := 0, 0
			for _, e := range prior {
				if e.Actor == tc.actor && e.Action == "usage_recheck" && e.Result == "rejected" && e.Detail == tc.reason && e.Object == "" {
					priorFound++
				}
			}
			for _, e := range events {
				if e.Actor == tc.actor && e.Action == "usage_recheck" && e.Result == "rejected" && e.Detail == tc.reason && e.Object == "" {
					found++
				}
			}
			encoded, _ := json.Marshal(events)
			if len(events) != len(prior)+1 || found != priorFound+1 || strings.Contains(string(encoded), "attacker-secret") {
				t.Fatalf("audit missing or leaks request: %s", encoded)
			}
			after, _, err := stores.usageStore.ListUsageRechecks(0, 100)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("denial changed evidence: %+v %v", after, err)
			}
		})
	}
}

func TestIssue72RecheckDeniedWhenAuditUnavailable(t *testing.T) {
	for _, scenario := range []string{"nil_store", "closed_db", "missing_bucket"} {
		t.Run(scenario, func(t *testing.T) {
			stores := setupTestStores(t, t.TempDir())
			sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "test", Secret: "test-secret", TTLSeconds: 3600}, nil)
			var logs bytes.Buffer
			h := &Handler{usageStore: stores.usageStore, auditStore: stores.auditStore, sessionManager: sessions, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			if scenario == "nil_store" {
				h.auditStore = nil
			}
			if scenario == "closed_db" {
				if err := stores.db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing_bucket" {
				if err := stores.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(nosql.BucketAdminAudit)) }); err != nil {
					t.Fatal(err)
				}
			}
			h.router = h.routes()
			_, admin, _ := sessions.CreateSession("real-admin", RoleAdmin)
			_, readonly, _ := sessions.CreateSession("reader", RoleReadonly)
			for _, tc := range []struct {
				cookie *http.Cookie
				status int
			}{{admin, 400}, {readonly, 403}} {
				r := httptest.NewRequest(http.MethodPost, "/data/usage/recheck", strings.NewReader(`{"actor":"attacker-secret"}`))
				r.AddCookie(tc.cookie)
				w := httptest.NewRecorder()
				sessions.Middleware(h).ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			}
			if strings.Count(logs.String(), "usage recheck rejection audit") != 2 || strings.Contains(logs.String(), "attacker-secret") {
				t.Fatalf("audit failure log: %s", logs.String())
			}
		})
	}
}

func TestIssue72RecheckRejectsQueryActor(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "test", Secret: "test-secret", TTLSeconds: 3600}, nil)
	h := &Handler{usageStore: stores.usageStore, auditStore: stores.auditStore, sessionManager: sessions}
	h.router = h.routes()
	_, cookie, _ := sessions.CreateSession("real-admin", RoleAdmin)
	body := `{"evidence_id":"` + strings.Repeat("a", 64) + `","version":"` + strings.Repeat("b", 64) + `","operation_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`
	r := httptest.NewRequest(http.MethodPost, "/data/usage/recheck?actor=attacker-secret", strings.NewReader(body))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	sessions.Middleware(h).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	events, err := stores.auditStore.List(10)
	data, _ := json.Marshal(events)
	if err != nil || len(events) != 1 || events[0].Actor != "real-admin" || events[0].Detail != "invalid_request" || strings.Contains(string(data), "attacker-secret") {
		t.Fatalf("audit: %s %v", data, err)
	}
}

func TestIssue72RecheckUnauthenticatedDoesNotInventActor(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "test", Secret: "test-secret", TTLSeconds: 3600}, nil)
	h := &Handler{usageStore: stores.usageStore, auditStore: stores.auditStore, sessionManager: sessions}
	h.router = h.routes()
	r := httptest.NewRequest(http.MethodPost, "/data/usage/recheck", strings.NewReader(`{"actor":"attacker-secret"}`))
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	sessions.Middleware(h).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	events, err := stores.auditStore.List(10)
	if err != nil || len(events) != 0 {
		t.Fatalf("invented audit actor: %+v %v", events, err)
	}
}
