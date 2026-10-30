package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/go-chi/chi/v5"
)

// 模型组的 CRUD 错误、改名/删除级联与审计分支。绑定目标不存在时不落库，
// 因此这里只用空 bindings 验证目录 CRUD，级联通过客户端 plan 观察。
func TestModelGroupCRUDBranches(t *testing.T) {
	e := newIssue94Env(t)

	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/model-groups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		ModelGroups []config.ModelGroup `json:"model_groups"`
		Count       int                 `json:"count"`
	}
	decodeAdminJSON(t, rec, &list)
	if list.Count != 0 {
		t.Fatalf("empty list count = %d, want 0", list.Count)
	}

	for _, tc := range []struct {
		name, body, wantError string
	}{
		{"invalid json", `{"name":`, "invalid json body"},
		{"blank name", `{"name":"  "}`, "name must not be empty"},
		{"negative coefficient", `{"name":"bad","cost_coefficient":-1}`, "cost_coefficient must not be negative"},
	} {
		t.Run("create "+tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/model-groups", tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("create %s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
			if events := auditEventsWithAction(t, e.h.auditStore, "model_group_create"); len(events) != 0 {
				t.Fatalf("rejected create wrote audit: %+v", events)
			}
		})
	}

	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/model-groups", `{"name":"g1","cost_coefficient":1,"bindings":[]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create g1: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/model-groups", `{"name":"G1","cost_coefficient":1}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("duplicate create: %d %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		wantError                string
	}{
		{"update invalid json", http.MethodPut, "/data/model-groups/g1", `{`, http.StatusBadRequest, "invalid json body"},
		{"update invalid body", http.MethodPut, "/data/model-groups/g1", `{"name":"g1","cost_coefficient":-2}`, http.StatusBadRequest, "cost_coefficient must not be negative"},
		{"update missing", http.MethodPut, "/data/model-groups/missing", `{"name":"g9"}`, http.StatusNotFound, "not found"},
		{"delete missing", http.MethodDelete, "/data/model-groups/missing", "", http.StatusNotFound, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", tc.method, tc.path, tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/model-groups", `{"name":"g2","cost_coefficient":1}`); rec.Code != http.StatusCreated {
		t.Fatalf("create g2: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/model-groups/g1", `{"name":"g2"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("rename onto existing: %d %s", rec.Code, rec.Body.String())
	}

	// 客户端 plan 绑定 g1：改名级联到 plan，审计 detail 列出客户端。
	if err := e.h.clientQuotaStore.Replace("alice", []config.QuotaGroup{{Name: "g1", Coefficient: coefPtrAdmin(2)}}); err != nil {
		t.Fatalf("seed client plan: %v", err)
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/model-groups/g1", `{"name":"g3","cost_coefficient":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename g1 to g3: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Status           string   `json:"status"`
		RenamedClients   []string `json:"renamed_clients"`
		RemovedFromUsers []string `json:"removed_from_clients"`
	}
	decodeAdminJSON(t, rec, &updated)
	if updated.Status != "ok" || strings.Join(updated.RenamedClients, ",") != "alice" {
		t.Fatalf("rename response = %s, want renamed_clients [alice]", rec.Body.String())
	}
	groups, err := e.h.clientQuotaStore.Get("alice")
	if err != nil || len(groups) != 1 || groups[0].Name != "g3" {
		t.Fatalf("client plan after rename = %+v (err %v), want g3", groups, err)
	}
	updates := auditEventsWithAction(t, e.h.auditStore, "model_group_update")
	if len(updates) != 1 || updates[0].Object != "g1" || updates[0].Detail != "renamed_clients: alice" {
		t.Fatalf("rename audit = %+v, want g1 renamed_clients: alice", updates)
	}

	// 删除仍被客户端 plan 引用的组：从 plan 移除并在响应/审计中列出客户端。
	rec = e.do(t, RoleAdmin, "operator", http.MethodDelete, "/data/model-groups/g3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete g3: %d %s", rec.Code, rec.Body.String())
	}
	decodeAdminJSON(t, rec, &updated)
	if updated.Status != "ok" || strings.Join(updated.RemovedFromUsers, ",") != "alice" {
		t.Fatalf("delete response = %s, want removed_from_clients [alice]", rec.Body.String())
	}
	if groups, err := e.h.clientQuotaStore.Get("alice"); err != nil || len(groups) != 0 {
		t.Fatalf("client plan after delete = %+v (err %v), want empty", groups, err)
	}
	deletes := auditEventsWithAction(t, e.h.auditStore, "model_group_delete")
	if len(deletes) != 1 || deletes[0].Object != "g3" || deletes[0].Detail != "removed_from_clients: alice" {
		t.Fatalf("delete audit = %+v, want g3 removed_from_clients: alice", deletes)
	}

	rec = e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/model-groups", "")
	decodeAdminJSON(t, rec, &list)
	if list.Count != 1 || len(list.ModelGroups) != 1 || list.ModelGroups[0].Name != "g2" {
		t.Fatalf("list after delete = %+v, want only g2", list)
	}
}

func TestModelGroupListStoreFailureReturns500(t *testing.T) {
	e := newIssue94Env(t)
	if err := e.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/model-groups", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("list with closed store: %d %s", rec.Code, rec.Body.String())
	}
}

func adminRequestWithPathParam(name, value string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/model-groups/x", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(name, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestGroupNameParamTrimsAndRejectsEmpty(t *testing.T) {
	req := adminRequestWithPathParam("name", "  g1  ")
	name, err := groupNameParam(req)
	if err != nil || name != "g1" {
		t.Fatalf("groupNameParam = %q, %v; want g1", name, err)
	}
	for _, empty := range []string{"", "   ", "\t\n"} {
		if name, err := groupNameParam(adminRequestWithPathParam("name", empty)); err == nil || name != "" {
			t.Fatalf("groupNameParam(%q) = %q, %v; want empty-name error", empty, name, err)
		}
	}
}
