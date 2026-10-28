package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestIssue72RecheckSessionAndActor(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	event := usage.Event{RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Timestamp: time.Now().UTC(), ClientName: "private-client", Model: "private-model"}
	raw, _ := json.Marshal(event)
	if err := stores.usageStore.RegisterUnresolvedUsage(event, raw); err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionManager(config.AdminSessionConfig{CookieName: "test", Secret: "test-secret", TTLSeconds: 3600}, nil)
	h := &Handler{usageStore: stores.usageStore, auditStore: stores.auditStore, sessionManager: sessions}
	h.router = h.routes()
	handler := sessions.Middleware(h)
	_, adminCookie, _ := sessions.CreateSession("actual-admin", RoleAdmin)
	_, readonlyCookie, _ := sessions.CreateSession("reader", RoleReadonly)
	request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Accept", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/data/usage/unresolved", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	w := request("GET", "/data/usage/unresolved", "", adminCookie)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-client") || strings.Contains(w.Body.String(), "private-model") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Entries []nosql.UsageRecheckEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Entries) != 1 {
		t.Fatalf("list decode: %s %v", w.Body, err)
	}
	entry := list.Entries[0]
	body := `{"evidence_id":"` + entry.ID + `","version":"` + entry.Version + `","operation_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`
	if w := request("POST", "/data/usage/recheck", body, readonlyCookie); w.Code != http.StatusForbidden {
		t.Fatalf("readonly: %d", w.Code)
	}
	forged := strings.TrimSuffix(body, "}") + `,"actor":"impostor"}`
	if w := request("POST", "/data/usage/recheck", forged, adminCookie); w.Code != http.StatusBadRequest {
		t.Fatalf("forged actor: %d %s", w.Code, w.Body)
	}
	if w := request("POST", "/data/usage/recheck", body, adminCookie); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "insufficient_evidence") {
		t.Fatalf("recheck: %d %s", w.Code, w.Body)
	}
	events, err := stores.auditStore.List(10)
	if err != nil || len(events) != 3 {
		t.Fatalf("audit: %+v %v", events, err)
	}
	var business, readonly, invalid bool
	for _, event := range events {
		business = business || (event.Actor == "actual-admin" && event.Result == "insufficient_evidence")
		readonly = readonly || (event.Actor == "reader" && event.Result == "rejected" && event.Detail == "readonly")
		invalid = invalid || (event.Actor == "actual-admin" && event.Result == "rejected" && event.Detail == "invalid_request")
	}
	if !business || !readonly || !invalid {
		t.Fatalf("audit events missing: %+v", events)
	}
}
