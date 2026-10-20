package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

func newQuotaSyncTestHandler(t *testing.T) (*Handler, *bolt.DB) {
	t.Helper()
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &Handler{
		clientStore: nosql.NewClientStore(db), clientQuotaStore: nosql.NewClientQuotaStore(db),
		modelGroupStore: nosql.NewModelGroupStore(db), jobLevelStore: nosql.NewJobLevelStore(db),
		auditStore: nosql.NewAuditStore(db), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h.router = h.routes()
	return h, db
}

func requestQuotaSync(h *Handler, source string, selectors any) *httptest.ResponseRecorder {
	body, err := json.Marshal(selectors)
	if err != nil {
		panic(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/data/quota/"+source+"/sync", bytes.NewReader(body)))
	return rec
}

// Keep a unique frame in this request's stack so the test observes its write-lock
// wait, rather than guessing timing or mistaking another DB operation for it.
//
//go:noinline
func serveQuotaSyncWhileWriterHeld(h *Handler, selectors any, done chan<- *httptest.ResponseRecorder) {
	done <- requestQuotaSync(h, "Source", selectors)
}

func waitQuotaSyncWriteLock(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	stack := make([]byte, 1<<20)
	for {
		n := runtime.Stack(stack, true)
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(goroutine, "serveQuotaSyncWhileWriterHeld(") && strings.Contains(goroutine, "bbolt.(*DB).beginRWTx(") {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("sync request did not reach the bbolt write-lock wait")
		case <-poll.C:
		}
	}
}

func putQuotaSyncFixture(t *testing.T, tx *bolt.Tx, bucket, name string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Bucket([]byte(bucket)).Put([]byte(strings.ToLower(name)), body); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaSyncConsistentSnapshot(t *testing.T) {
	for _, mode := range []string{"clients", "project_group", "job_level", "deleted_source"} {
		t.Run(mode, func(t *testing.T) {
			h, db := newQuotaSyncTestHandler(t)
			clients := []config.Client{
				{Name: "Source", AccessKey: "source-test", JobLevel: "A", ProjectGroup: "source"},
				{Name: "Old", AccessKey: "old-test", JobLevel: "B", ProjectGroup: "scope"},
				{Name: "New", AccessKey: "new-test", JobLevel: "C", ProjectGroup: "outside"},
			}
			seedClients(t, h.clientStore, clients)
			if err := h.modelGroupStore.Create(config.ModelGroup{Name: "plan", CostCoefficient: 1}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Source", "Old", "New"} {
				if err := h.clientQuotaStore.Replace(name, []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(1)}}); err != nil {
					t.Fatal(err)
				}
			}
			writer, err := db.Begin(true)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback()
			putQuotaSyncFixture(t, writer, nosql.BucketClientQuotaGroups, "Source", nosql.QuotaClientEntry{Name: "Source", Groups: []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(2)}}})
			for _, name := range []string{"Old", "New"} {
				putQuotaSyncFixture(t, writer, nosql.BucketClientQuotaGroups, name, nosql.QuotaClientEntry{Name: name, Groups: []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(3)}}})
			}
			selectors := map[string]any{"clients": []string{"Old"}}
			wantTargets := []string{"Old"}
			switch mode {
			case "project_group", "job_level":
				clients[1].ProjectGroup, clients[1].JobLevel = "outside", "C"
				clients[2].ProjectGroup, clients[2].JobLevel = "scope", "B"
				putQuotaSyncFixture(t, writer, nosql.BucketClients, "Old", clients[1])
				putQuotaSyncFixture(t, writer, nosql.BucketClients, "New", clients[2])
				selectors = map[string]any{mode: "scope"}
				if mode == "job_level" {
					selectors[mode] = " B "
				}
				wantTargets = []string{"New"}
			case "deleted_source":
				if err := writer.Bucket([]byte(nosql.BucketClients)).Delete([]byte("source")); err != nil {
					t.Fatal(err)
				}
				putQuotaSyncFixture(t, writer, nosql.BucketClientQuotaGroups, "Source", nosql.QuotaClientEntry{Name: "Source", DeletedAt: "2026-10-14T00:00:00Z", Groups: []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(2)}}})
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go serveQuotaSyncWhileWriterHeld(h, selectors, done)
			waitQuotaSyncWriteLock(t)
			if err := writer.Commit(); err != nil {
				t.Fatal(err)
			}
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("sync did not finish after writer commit")
			}
			if mode == "deleted_source" {
				if response.Code != http.StatusNotFound {
					t.Errorf("deleted source: want 404, got %d: %s", response.Code, response.Body.String())
				}
				wantTargets = nil
			} else {
				if response.Code != http.StatusOK {
					t.Fatalf("sync failed: %d: %s", response.Code, response.Body.String())
				}
				var result struct {
					SyncedClients []string `json:"synced_clients"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result.SyncedClients, wantTargets) {
					t.Errorf("scope must use committed membership: got %v, want %v", result.SyncedClients, wantTargets)
				}
			}
			for _, name := range []string{"Old", "New"} {
				groups, err := h.clientQuotaStore.Get(name)
				if err != nil || len(groups) != 1 {
					t.Fatalf("read %s: groups=%v err=%v", name, groups, err)
				}
				want := 3.0
				if len(wantTargets) > 0 && name == wantTargets[0] {
					want = 2
				}
				if groups[0].PlanCoefficient() != want {
					t.Errorf("%s coefficient: got %v, want %v from same transaction snapshot", name, groups[0].PlanCoefficient(), want)
				}
			}
		})
	}
}

func TestQuotaSyncRefreshesEffectiveLimits(t *testing.T) {
	for _, mode := range []string{"clients", "project_group", "job_level", "union"} {
		t.Run(mode, func(t *testing.T) {
			h, db := newQuotaSyncTestHandler(t)
			for _, level := range []config.JobLevel{
				{Name: "A", CoefficientDaily: coefPtrAdmin(1), CoefficientWeekly: coefPtrAdmin(2), CoefficientMonthly: coefPtrAdmin(3)},
				{Name: "B", CoefficientDaily: coefPtrAdmin(2), CoefficientWeekly: coefPtrAdmin(3), CoefficientMonthly: coefPtrAdmin(4)},
			} {
				if err := h.jobLevelStore.Create(level); err != nil {
					t.Fatal(err)
				}
			}
			for _, group := range []config.ModelGroup{
				{Name: "default", CostCoefficient: 0.5, DailyUSD: 10, WeeklyUSD: 20, MonthlyUSD: 30},
				{Name: "zero", CostCoefficient: 0.5, DailyUSD: 10, WeeklyUSD: 20, MonthlyUSD: 30},
				{Name: "positive", CostCoefficient: 0.5, DailyUSD: 7, WeeklyUSD: 11, MonthlyUSD: 13},
				{Name: "old", CostCoefficient: 1, DailyUSD: 1, WeeklyUSD: 1, MonthlyUSD: 1},
			} {
				if err := h.modelGroupStore.Create(group); err != nil {
					t.Fatal(err)
				}
			}
			seedClients(t, h.clientStore, []config.Client{
				{Name: "Source", AccessKey: "source-local", JobLevel: "A", ProjectGroup: "source"},
				{Name: "Target", AccessKey: "target-local", JobLevel: "B", ProjectGroup: "project", IPAddress: "192.0.2.7", AllowedTargets: []string{"allowed"}, AllowedPools: []string{"pool"}},
			})
			plan := []config.QuotaGroup{{Name: "default"}, {Name: "zero", Coefficient: coefPtrAdmin(0)}, {Name: "positive", Coefficient: coefPtrAdmin(2)}}
			if err := h.clientQuotaStore.Replace("Source", plan); err != nil {
				t.Fatal(err)
			}
			if err := h.clientQuotaStore.Replace("Target", []config.QuotaGroup{{Name: "old", Coefficient: coefPtrAdmin(3)}}); err != nil {
				t.Fatal(err)
			}
			h.usageStore = nosql.NewUsageStore(db)
			if err := h.usageStore.Record(usage.Event{Timestamp: time.Now().UTC().Add(-time.Hour), ClientName: "Target", Target: "historical", Model: "historical", InputTokens: 17, OutputTokens: 23}); err != nil {
				t.Fatal(err)
			}
			beforeUsage, err := h.usageStore.List(usage.Filter{ClientName: "Target"})
			if err != nil {
				t.Fatal(err)
			}
			beforeClient, err := h.clientStore.Get("Target")
			if err != nil {
				t.Fatal(err)
			}
			h.quotaManager, err = quota.New(quota.Options{ClientStore: h.clientStore, ClientQuotaStore: h.clientQuotaStore, ModelGroupStore: h.modelGroupStore, JobLevelStore: h.jobLevelStore, UsageStore: h.usageStore, CostStore: nosql.NewModelCostStore(db), Logger: h.logger})
			if err != nil {
				t.Fatal(err)
			}
			h.quotaManager.Evaluate("Target")
			selectors := map[string]any{"clients": []string{"target"}}
			switch mode {
			case "project_group":
				selectors = map[string]any{mode: "project"}
			case "job_level":
				selectors = map[string]any{mode: " b "}
			case "union":
				selectors = map[string]any{"clients": []string{"Target", "target", "source"}, "project_group": "project", "job_level": "B"}
			}
			response := requestQuotaSync(h, "Source", selectors)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"synced_clients":["Target"]`) {
				t.Fatalf("sync: %d %s", response.Code, response.Body.String())
			}
			status := h.quotaManager.Status("Target")
			if status.CalibrationError != "" || status.JobLevel != "B" {
				t.Fatalf("post-success status: %+v", status)
			}
			if _, present := status.Groups["old"]; present {
				t.Fatal("previous model group remains after replacement")
			}
			for _, tc := range []struct {
				name     string
				personal float64
				limits   [3]float64
			}{
				{"default", 1, [3]float64{20, 60, 120}}, {"zero", 0, [3]float64{0, 0, 0}}, {"positive", 2, [3]float64{28, 66, 104}},
			} {
				g, present := status.Groups[tc.name]
				if !present || !g.InPlan || g.PlanCoefficient != tc.personal || g.Coefficient != 0.5 {
					t.Fatalf("group formula inputs: %+v", g)
				}
				for i, dim := range []string{"daily", "weekly", "monthly"} {
					if g.Quotas[dim].Limit != tc.limits[i] || g.RankCoefficients[dim] != float64(i+2) {
						t.Fatalf("%s/%s after success: %+v", tc.name, dim, g)
					}
				}
			}
			storedPlan, err := h.clientQuotaStore.Get("Target")
			if err != nil || !reflect.DeepEqual(storedPlan, plan) {
				t.Fatalf("nil/zero/positive plan lost: %+v err=%v", storedPlan, err)
			}
			afterClient, err := h.clientStore.Get("Target")
			if err != nil || !reflect.DeepEqual(beforeClient, afterClient) {
				t.Fatalf("identity or ACL changed: %v", err)
			}
			afterUsage, err := h.usageStore.List(usage.Filter{ClientName: "Target"})
			if err != nil || !reflect.DeepEqual(beforeUsage, afterUsage) {
				t.Fatalf("historical usage changed: %v", err)
			}
			audits, err := h.auditStore.List(10)
			if err != nil || len(audits) != 1 || audits[0].Action != "quota_sync" || audits[0].Object != "Source" || audits[0].Detail != "Target" {
				t.Fatalf("canonical sync audit: %+v err=%v", audits, err)
			}
		})
	}
}

func TestQuotaSyncSessionGuards(t *testing.T) {
	h, _ := newQuotaSyncTestHandler(t)
	seedClients(t, h.clientStore, []config.Client{{Name: "Source", AccessKey: "source-local"}, {Name: "Target", AccessKey: "target-local"}})
	if err := h.modelGroupStore.Create(config.ModelGroup{Name: "plan", CostCoefficient: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.clientQuotaStore.Replace("Source", []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := h.clientQuotaStore.Replace("Target", []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(3)}}); err != nil {
		t.Fatal(err)
	}
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "sync-local-secret", CookieName: "sync-local-session", TTLSeconds: 3600}, h.logger)
	h.sessionManager = sm
	h.router = h.routes()
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/admin", h)
	for _, tc := range []struct {
		name, role string
		want       int
	}{{"unauthenticated", "", http.StatusUnauthorized}, {"readonly", RoleReadonly, http.StatusForbidden}, {"admin", RoleAdmin, http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/data/quota/Source/sync", strings.NewReader(`{"clients":["Target"]}`))
			req.Header.Set("Accept", "application/json")
			if tc.role != "" {
				_, cookie, err := sm.CreateSession(tc.name, tc.role)
				if err != nil {
					t.Fatal(err)
				}
				req.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("guard: want %d got %d: %s", tc.want, response.Code, response.Body.String())
			}
			groups, err := h.clientQuotaStore.Get("Target")
			want := 3.0
			if tc.want == http.StatusOK {
				want = 2
			}
			if err != nil || len(groups) != 1 || groups[0].PlanCoefficient() != want {
				t.Fatalf("guard changed target plan: %+v err=%v", groups, err)
			}
		})
	}
}

func TestQuotaSyncHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, source, body, corrupt string
		want                        int
	}{
		{"invalid JSON", "Source", `{`, "", http.StatusBadRequest},
		{"source spelling", "source", `{"clients":["Target"]}`, "", http.StatusNotFound},
		{"source missing", "missing", `{"clients":["Target"]}`, "", http.StatusNotFound},
		{"empty rank", "Source", `{"job_level":" "}`, "", http.StatusBadRequest},
		{"missing target", "Source", `{"clients":["Target","zz-missing"]}`, "", http.StatusBadRequest},
		{"source decode failure", "Source", `{"clients":["Target"]}`, nosql.BucketClients, http.StatusInternalServerError},
		{"plan decode failure", "Source", `{"clients":["Target"]}`, nosql.BucketClientQuotaGroups, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newQuotaSyncTestHandler(t)
			seedClients(t, h.clientStore, []config.Client{{Name: "Source", AccessKey: "source-local"}, {Name: "Target", AccessKey: "target-local"}})
			if err := h.modelGroupStore.Create(config.ModelGroup{Name: "plan", CostCoefficient: 1}); err != nil {
				t.Fatal(err)
			}
			if err := h.clientQuotaStore.Replace("Target", []config.QuotaGroup{{Name: "plan", Coefficient: coefPtrAdmin(3)}}); err != nil {
				t.Fatal(err)
			}
			if tc.corrupt != "" {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(tc.corrupt)).Put([]byte("source"), []byte("sensitive-source-secret"))
				}); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/data/quota/"+tc.source+"/sync", strings.NewReader(tc.body)))
			if response.Code != tc.want {
				t.Fatalf("error mapping: want %d got %d: %s", tc.want, response.Code, response.Body.String())
			}
			if tc.want == http.StatusInternalServerError && strings.TrimSpace(response.Body.String()) != `{"error":"internal server error"}` {
				t.Fatalf("internal error details exposed: %s", response.Body.String())
			}
			groups, err := h.clientQuotaStore.Get("Target")
			if err != nil || len(groups) != 1 || groups[0].PlanCoefficient() != 3 {
				t.Fatalf("failed sync changed target: %+v err=%v", groups, err)
			}
			audits, err := h.auditStore.List(10)
			if err != nil || len(audits) != 0 {
				t.Fatalf("failed sync recorded success audit: %+v err=%v", audits, err)
			}
		})
	}
}
