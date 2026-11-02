package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// decodeAdminJSON 解码管理接口返回的 JSON；失败时连同响应体一起报错。
func decodeAdminJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// auditEventsWithAction 返回指定审计动作的全部事件（最多 500 条）。
func auditEventsWithAction(t *testing.T, store *nosql.AuditStore, action string) []nosql.AuditEvent {
	t.Helper()
	events, err := store.List(500)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	var matched []nosql.AuditEvent
	for _, event := range events {
		if event.Action == action {
			matched = append(matched, event)
		}
	}
	return matched
}

// 项目组字典的 CRUD 错误分支与审计：重复名 409、未找到 404、非法 JSON/空名 400，
// 成功写入后列表内容与审计对象必须一致。既有行为正确，本用例守护这些契约。
func TestProjectGroupDictionaryCRUDBranches(t *testing.T) {
	e := newIssue94Env(t)

	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/project-groups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		ProjectGroups []string `json:"project_groups"`
		Count         int      `json:"count"`
	}
	decodeAdminJSON(t, rec, &list)
	if list.Count != 0 || len(list.ProjectGroups) != 0 {
		t.Fatalf("empty list = %+v, want count 0 and no entries", list)
	}

	// 非法 JSON 与空名称都不能落库，也不产生审计。
	for _, tc := range []struct {
		name, body, wantError string
	}{
		{"invalid json", `{"name":`, "invalid json body"},
		{"blank name", `{"name":"   "}`, "name must not be empty"},
	} {
		t.Run("create "+tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/project-groups", tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("create %s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
			if events := auditEventsWithAction(t, e.h.auditStore, "project_group_create"); len(events) != 0 {
				t.Fatalf("rejected create wrote audit: %+v", events)
			}
		})
	}

	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/project-groups", `{"name":"platform"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create platform: %d %s", rec.Code, rec.Body.String())
	}

	// 字典名大小写不敏感：Platform 与 platform 视为重复。
	rec = e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/project-groups", `{"name":"Platform"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("duplicate create: %d %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		wantError                string
	}{
		{"update invalid json", http.MethodPut, "/data/project-groups/platform", `{`, http.StatusBadRequest, "invalid json body"},
		{"update missing", http.MethodPut, "/data/project-groups/missing", `{"name":"renamed"}`, http.StatusNotFound, "not found"},
		{"delete missing", http.MethodDelete, "/data/project-groups/missing", "", http.StatusNotFound, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", tc.method, tc.path, tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	// 第二个条目用于验证重命名为已存在的名字返回 409。
	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/project-groups", `{"name":"data"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create data: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/project-groups/platform", `{"name":"data"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("rename onto existing: %d %s", rec.Code, rec.Body.String())
	}

	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/project-groups/platform", `{"name":"analytics"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/project-groups", "")
	decodeAdminJSON(t, rec, &list)
	if list.Count != 2 || strings.Join(list.ProjectGroups, ",") != "analytics,data" {
		t.Fatalf("list after rename = %+v, want [analytics data]", list)
	}

	if rec := e.do(t, RoleAdmin, "operator", http.MethodDelete, "/data/project-groups/analytics", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/project-groups", "")
	decodeAdminJSON(t, rec, &list)
	if list.Count != 1 || list.ProjectGroups[0] != "data" {
		t.Fatalf("list after delete = %+v, want [data]", list)
	}

	creates := auditEventsWithAction(t, e.h.auditStore, "project_group_create")
	if len(creates) != 2 {
		t.Fatalf("create audits = %+v, want 2 successful creates", creates)
	}
	updates := auditEventsWithAction(t, e.h.auditStore, "project_group_update")
	if len(updates) != 1 || updates[0].Object != "platform" || updates[0].Detail != "analytics" || updates[0].Result != "success" || updates[0].Actor != "operator" {
		t.Fatalf("update audit = %+v, want object platform detail analytics by operator", updates)
	}
	deletes := auditEventsWithAction(t, e.h.auditStore, "project_group_delete")
	if len(deletes) != 1 || deletes[0].Object != "analytics" || deletes[0].Result != "success" {
		t.Fatalf("delete audit = %+v, want analytics deleted", deletes)
	}
}

func TestProjectGroupListStoreFailureReturns500(t *testing.T) {
	e := newIssue94Env(t)
	if err := e.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/project-groups", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("list with closed store: %d %s", rec.Code, rec.Body.String())
	}
}

// 职级的错误与审计分支：非法 JSON、空名、重复名、未找到、改名级联与删除。
func TestJobLevelCRUDBranches(t *testing.T) {
	e := newIssue94Env(t)

	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/job-levels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		JobLevels []config.JobLevel `json:"job_levels"`
		Count     int               `json:"count"`
	}
	decodeAdminJSON(t, rec, &list)
	if list.Count != 0 || list.JobLevels == nil {
		t.Fatalf("empty job levels = %+v, want empty array", list)
	}

	for _, tc := range []struct {
		name, body, wantError string
	}{
		{"invalid json", `{"name":`, "invalid json body"},
		{"blank name", `{"name":"  "}`, "name must not be empty"},
	} {
		t.Run("create "+tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/job-levels", tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("create %s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if events := auditEventsWithAction(t, e.h.auditStore, "job_level_create"); len(events) != 0 {
		t.Fatalf("rejected creates wrote audit: %+v", events)
	}

	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/job-levels", `{"name":"L6","coefficient_daily":2}`); rec.Code != http.StatusCreated {
		t.Fatalf("create L6: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/job-levels", `{"name":"l6"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("duplicate create: %d %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		wantError                string
	}{
		{"update invalid json", http.MethodPut, "/data/job-levels/L6", `{`, http.StatusBadRequest, "invalid json body"},
		{"update missing", http.MethodPut, "/data/job-levels/missing", `{"name":"L9"}`, http.StatusNotFound, "not found"},
		{"delete missing", http.MethodDelete, "/data/job-levels/missing", "", http.StatusNotFound, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, RoleAdmin, "operator", tc.method, tc.path, tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	if rec := e.do(t, RoleAdmin, "operator", http.MethodPost, "/data/job-levels", `{"name":"L5"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create L5: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/job-levels/L6", `{"name":"L5"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("rename onto existing: %d %s", rec.Code, rec.Body.String())
	}

	// 无客户端绑定时 renamed_clients 必须是非 null 空数组。
	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/job-levels/L6", `{"name":"L7","coefficient_weekly":1.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename L6 to L7: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Status         string   `json:"status"`
		RenamedClients []string `json:"renamed_clients"`
	}
	decodeAdminJSON(t, rec, &updated)
	if updated.Status != "ok" || updated.RenamedClients == nil || len(updated.RenamedClients) != 0 {
		t.Fatalf("rename response = %s, want empty renamed_clients array", rec.Body.String())
	}

	// 有客户端绑定时级联改名，并在审计 detail 中列出客户端。
	if err := e.h.clientStore.Create(config.Client{Name: "carol", AccessKey: "carol-key", JobLevel: "L5"}); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodPut, "/data/job-levels/L5", `{"name":"L4"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename L5 to L4: %d %s", rec.Code, rec.Body.String())
	}
	decodeAdminJSON(t, rec, &updated)
	if strings.Join(updated.RenamedClients, ",") != "carol" {
		t.Fatalf("renamed_clients = %v, want [carol]", updated.RenamedClients)
	}
	carol, err := e.h.clientStore.Get("carol")
	if err != nil || carol == nil || carol.JobLevel != "L4" {
		t.Fatalf("client not cascaded: %+v (err %v)", carol, err)
	}

	updates := auditEventsWithAction(t, e.h.auditStore, "job_level_update")
	if len(updates) != 2 {
		t.Fatalf("update audits = %+v, want 2", updates)
	}
	var sawCascade bool
	for _, event := range updates {
		if event.Object == "L5" && event.Detail == "renamed_clients: carol" {
			sawCascade = true
		}
	}
	if !sawCascade {
		t.Fatalf("cascade audit detail missing: %+v", updates)
	}

	// L4 被 carol 引用时删除返回 409；改绑后才能删除。
	rec = e.do(t, RoleAdmin, "operator", http.MethodDelete, "/data/job-levels/L4", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "carol") {
		t.Fatalf("delete referenced: %d %s", rec.Code, rec.Body.String())
	}
	if err := e.h.clientStore.Update("carol", config.Client{Name: "carol", AccessKey: "carol-key", JobLevel: ""}); err != nil {
		t.Fatalf("unbind client: %v", err)
	}
	if rec := e.do(t, RoleAdmin, "operator", http.MethodDelete, "/data/job-levels/L4", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete unreferenced: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, RoleAdmin, "operator", http.MethodDelete, "/data/job-levels/L7", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete L7: %d %s", rec.Code, rec.Body.String())
	}

	deletes := auditEventsWithAction(t, e.h.auditStore, "job_level_delete")
	if len(deletes) != 2 {
		t.Fatalf("delete audits = %+v, want 2 successful deletes", deletes)
	}
	rec = e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/job-levels", "")
	decodeAdminJSON(t, rec, &list)
	if list.Count != 0 {
		t.Fatalf("job levels after deletes = %+v, want empty", list)
	}
}

func TestJobLevelListStoreFailureReturns500(t *testing.T) {
	e := newIssue94Env(t)
	if err := e.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	rec := e.do(t, RoleAdmin, "operator", http.MethodGet, "/data/job-levels", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("list with closed store: %d %s", rec.Code, rec.Body.String())
	}
}
