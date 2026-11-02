package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
)

// seedUnresolvedUsage 写入一条待核实用量记录。
func seedUnresolvedUsage(t *testing.T, store *nosql.UsageStore, recordID, client string) {
	t.Helper()
	event := usage.Event{RecordID: recordID, Timestamp: time.Now().UTC(), ClientName: client, Model: "private-model"}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal usage event: %v", err)
	}
	if err := store.RegisterUnresolvedUsage(event, raw); err != nil {
		t.Fatalf("register unresolved usage: %v", err)
	}
}

func recheckJSONBody(evidenceID, version, operationID string) string {
	return `{"evidence_id":"` + evidenceID + `","version":"` + version + `","operation_id":"` + operationID + `"}`
}

// 未解决列表的查询校验、空态、分页与 opaque 输出。
func TestListUsageUnresolvedQueryBranches(t *testing.T) {
	e := newIssue94Env(t)
	e.h.usageStore = nosql.NewUsageStore(e.db)

	for _, tc := range []struct {
		name, path, wantError string
	}{
		{"unknown parameter", "/data/usage/unresolved?bogus=1", "invalid query"},
		{"repeated parameter", "/data/usage/unresolved?offset=0&offset=1", "invalid query"},
		{"non-numeric offset", "/data/usage/unresolved?offset=abc", "invalid offset"},
		{"negative offset", "/data/usage/unresolved?offset=-1", "invalid offset"},
		{"zero limit", "/data/usage/unresolved?limit=0", "invalid limit"},
		{"limit too large", "/data/usage/unresolved?limit=501", "invalid limit"},
		{"non-numeric limit", "/data/usage/unresolved?limit=x", "invalid limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", http.MethodGet, tc.path, "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/usage/unresolved", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Entries []nosql.UsageRecheckEntry `json:"entries"`
		More    bool                      `json:"more"`
	}
	decodeAdminJSON(t, rec, &empty)
	if empty.Entries == nil || len(empty.Entries) != 0 || empty.More {
		t.Fatalf("empty list = %s, want [] and more=false", rec.Body.String())
	}
}

func TestListUsageUnresolvedUnavailableAndPagination(t *testing.T) {
	t.Run("nil usage store", func(t *testing.T) {
		e := newIssue94Env(t)
		rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/usage/unresolved", "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "usage store unavailable") {
			t.Fatalf("nil store: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("closed usage store", func(t *testing.T) {
		e := newIssue94Env(t)
		e.h.usageStore = nosql.NewUsageStore(e.db)
		if err := e.db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
		rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/usage/unresolved", "")
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "usage evidence unavailable") {
			t.Fatalf("closed store: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("pagination", func(t *testing.T) {
		e := newIssue94Env(t)
		e.h.usageStore = nosql.NewUsageStore(e.db)
		for _, id := range []string{
			"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		} {
			seedUnresolvedUsage(t, e.h.usageStore, id, "private-client")
		}
		all := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/usage/unresolved?limit=500", "")
		if all.Code != http.StatusOK {
			t.Fatalf("list all: %d %s", all.Code, all.Body.String())
		}
		var full struct {
			Entries []nosql.UsageRecheckEntry `json:"entries"`
			More    bool                      `json:"more"`
		}
		decodeAdminJSON(t, all, &full)
		if len(full.Entries) != 3 || full.More {
			t.Fatalf("full list = %s, want 3 entries and more=false", all.Body.String())
		}
		for _, entry := range full.Entries {
			if len(entry.ID) != 64 || len(entry.Version) != 64 {
				t.Fatalf("opaque identifiers = %+v, want 64-char hashes", entry)
			}
		}
		for _, secret := range []string{"private-client", "private-model"} {
			if strings.Contains(all.Body.String(), secret) {
				t.Fatalf("list leaks %q: %s", secret, all.Body.String())
			}
		}

		page := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/usage/unresolved?offset=1&limit=1", "")
		if page.Code != http.StatusOK {
			t.Fatalf("page: %d %s", page.Code, page.Body.String())
		}
		var one struct {
			Entries []nosql.UsageRecheckEntry `json:"entries"`
			More    bool                      `json:"more"`
		}
		decodeAdminJSON(t, page, &one)
		if len(one.Entries) != 1 || !one.More {
			t.Fatalf("page = %s, want one entry and more=true", page.Body.String())
		}
		if one.Entries[0].ID != full.Entries[1].ID || one.Entries[0].Version != full.Entries[1].Version {
			t.Fatalf("page entry = %+v, want the second entry of the full list %+v", one.Entries[0], full.Entries[1])
		}

		beyond := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/usage/unresolved?offset=5", "")
		if beyond.Code != http.StatusOK {
			t.Fatalf("beyond: %d %s", beyond.Code, beyond.Body.String())
		}
		var none struct {
			Entries []nosql.UsageRecheckEntry `json:"entries"`
			More    bool                      `json:"more"`
		}
		decodeAdminJSON(t, beyond, &none)
		if none.Entries == nil || len(none.Entries) != 0 || none.More {
			t.Fatalf("beyond = %s, want [] and more=false", beyond.Body.String())
		}
	})
}

// recheck 剩余的拒绝与不可用分支：值类型错误、截断/尾随 JSON、非规范 UUID、
// nil/关闭的存储、版本冲突。
func TestUsageRecheckErrorBranches(t *testing.T) {
	e := newIssue94Env(t)
	e.h.usageStore = nosql.NewUsageStore(e.db)
	admin := func(body string) *httptest.ResponseRecorder {
		return e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/usage/recheck", body)
	}
	validID := strings.Repeat("a", 64)
	validVersion := strings.Repeat("b", 64)
	operationID := uuid.NewString()
	validBody := recheckJSONBody(validID, validVersion, operationID)

	for _, tc := range []struct {
		name, body, wantError string
	}{
		{"non-string value", `{"evidence_id":123}`, "invalid request"},
		{"truncated object", `{"evidence_id":"` + validID + `"`, "invalid request"},
		{"trailing object", `{}{}`, "invalid request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := admin(tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	uppercaseUUID := strings.ToUpper(operationID)
	rec := admin(recheckJSONBody(validID, validVersion, uppercaseUUID))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid request") {
		t.Fatalf("non-canonical operation id: %d %s", rec.Code, rec.Body.String())
	}

	t.Run("nil usage store", func(t *testing.T) {
		bare := newIssue94Env(t)
		rec := bare.do(t, RoleAdmin, "operator", http.MethodPost, "/data/usage/recheck", validBody)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "usage store unavailable") {
			t.Fatalf("nil store: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("version conflict", func(t *testing.T) {
		seedUnresolvedUsage(t, e.h.usageStore, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "private-client")
		entries, _, err := e.h.usageStore.ListUsageRechecks(0, 500)
		if err != nil || len(entries) == 0 {
			t.Fatalf("list evidence: %+v %v", entries, err)
		}
		entry := entries[0]
		rec := admin(recheckJSONBody(entry.ID, strings.Repeat("0", 64), uuid.NewString()))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "usage recheck conflict") {
			t.Fatalf("version conflict: %d %s", rec.Code, rec.Body.String())
		}
		after, _, err := e.h.usageStore.ListUsageRechecks(0, 500)
		if err != nil || len(after) != 1 || after[0].Resolved {
			t.Fatalf("conflict resolved evidence: %+v %v", after, err)
		}
	})

	t.Run("closed usage store", func(t *testing.T) {
		closed := newIssue94Env(t)
		closed.h.usageStore = nosql.NewUsageStore(closed.db)
		if err := closed.db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
		rec := closed.do(t, RoleAdmin, "operator", http.MethodPost, "/data/usage/recheck", validBody)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "usage recheck unavailable") {
			t.Fatalf("closed store: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// 会话存在但用户名为空时，拒绝仍必须审计可定位的原因（日志），且响应为 403。
func TestUsageRecheckEmptySessionUsernameLogsMissingIdentity(t *testing.T) {
	e := newIssue94Env(t)
	var logs bytes.Buffer
	e.h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	rec := e.do(t, RoleAdmin, "", http.MethodPost, "/data/usage/recheck", `{}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin role required") {
		t.Fatalf("empty username: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "session identity missing") {
		t.Fatalf("missing identity was not logged: %s", logs.String())
	}
}
