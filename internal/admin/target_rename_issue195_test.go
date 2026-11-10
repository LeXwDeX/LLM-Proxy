package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

func seedRenameBindings(t *testing.T, env *revEnv) pricing.PricingTemplate {
	t.Helper()
	tmpl, err := nosql.NewPricingTemplateStore(env.db).Upsert(pricing.PricingTemplate{Targets: []string{"target-a"}, Models: []string{"gpt-4o"},
		Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nosql.NewModelGroupStore(env.db).Create(config.ModelGroup{Name: "models", CostCoefficient: 1,
		Bindings: []config.QuotaGroupBinding{{Target: "target-a", Model: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	seedClients(t, env.stores.clientStore, []config.Client{{Name: "alice", AccessKey: "ak-alice-0001", AllowedTargets: []string{"target-a"}}})
	return tmpl
}

func renameAudits(t *testing.T, env *revEnv) []nosql.AuditEvent {
	t.Helper()
	events, err := env.stores.auditStore.List(100)
	if err != nil {
		t.Fatal(err)
	}
	var out []nosql.AuditEvent
	for _, e := range events {
		if e.Action == "rename_target" || e.Action == "merge_target" {
			out = append(out, e)
		}
	}
	return out
}

func dumpDB(t *testing.T, db *bolt.DB) string {
	t.Helper()
	var sb strings.Builder
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if string(name) == nosql.BucketAdminAudit {
				return nil
			}
			return b.ForEach(func(k, v []byte) error {
				sb.WriteString(string(name) + "/" + string(k) + "=" + string(v) + "\n")
				return nil
			})
		})
	}); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func TestIssue195AdminRenameTargetSyncsBindingsRuntimeAndAudit(t *testing.T) {
	env := newRevEnv(t, t.TempDir(), false)
	tmpl := seedRenameBindings(t, env)
	etag := env.view("target-a").etag(t)

	rec := env.do(http.MethodPost, "targets/target-a/rename", quoted(etag), map[string]any{"new_name": "target-renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		From             string   `json:"from"`
		To               string   `json:"to"`
		Merged           bool     `json:"merged"`
		PricingTemplates []string `json:"pricing_templates"`
		ModelGroups      []string `json:"model_groups"`
		Clients          []string `json:"clients"`
		ETag             string   `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.From != "target-a" || body.To != "target-renamed" || body.Merged ||
		!reflect.DeepEqual(body.PricingTemplates, []string{tmpl.ID}) || !reflect.DeepEqual(body.ModelGroups, []string{"models"}) ||
		!reflect.DeepEqual(body.Clients, []string{"alice"}) || !strings.HasPrefix(body.ETag, "t1-") {
		t.Fatalf("rename response: %s", rec.Body.String())
	}
	// Runtime and listing follow the new name; the old name is gone.
	if env.runtimeEndpoint("target-renamed") == "" || env.runtimeEndpoint("target-a") != "" {
		t.Fatal("runtime targets not switched to the new name")
	}
	env.view("target-renamed")
	// Pricing resolves for both names (history + new traffic).
	templates, err := nosql.NewPricingTemplateStore(env.db).List()
	if err != nil {
		t.Fatal(err)
	}
	resolver := pricing.NewResolver(templates, nil)
	for _, name := range []string{"target-a", "target-renamed"} {
		if rates, ok := resolver.RateFor(name, "gpt-4o", time.Now()); !ok || rates.InputPer1MTokens != 2 {
			t.Fatalf("RateFor(%s) = %+v %t", name, rates, ok)
		}
	}
	alice, err := env.stores.clientStore.Get("alice")
	if err != nil || !reflect.DeepEqual(alice.AllowedTargets, []string{"target-renamed"}) {
		t.Fatalf("client ACL: %+v %v", alice, err)
	}
	audits := renameAudits(t, env)
	if len(audits) != 1 || audits[0].Object != "target-a" || audits[0].Result != "success" ||
		!strings.Contains(audits[0].Detail, "to=target-renamed") || !strings.Contains(audits[0].Detail, "model_groups=models") ||
		!strings.Contains(audits[0].Detail, "clients=alice") {
		t.Fatalf("audit: %+v", audits)
	}
}

func TestIssue195AdminMergeTarget(t *testing.T) {
	env := newRevEnv(t, t.TempDir(), false)
	seedRenameBindings(t, env)
	rec := env.do(http.MethodPost, "targets/target-a/rename", "", map[string]any{"new_name": "target-b", "merge": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"merged":true`) {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
	if env.runtimeEndpoint("target-a") != "" || env.runtimeEndpoint("target-b") == "" {
		t.Fatal("merge runtime state")
	}
	groups, err := nosql.NewModelGroupStore(env.db).List()
	if err != nil || len(groups) != 1 || !reflect.DeepEqual(groups[0].Bindings, []config.QuotaGroupBinding{{Target: "target-a", Model: "gpt-4o"}}) {
		t.Fatalf("merge must keep source bindings for history: %+v %v", groups, err)
	}
	alice, err := env.stores.clientStore.Get("alice")
	if err != nil || !reflect.DeepEqual(alice.AllowedTargets, []string{"target-b"}) {
		t.Fatalf("client ACL after merge: %+v %v", alice, err)
	}
	if audits := renameAudits(t, env); len(audits) != 1 || audits[0].Action != "merge_target" {
		t.Fatalf("merge audit: %+v", audits)
	}
}

func TestIssue195AdminRenameRejectionsLeaveStateUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		ifMatch string
		body    any
		status  int
	}{
		{"missing source", "targets/nope/rename", "", map[string]any{"new_name": "x"}, http.StatusNotFound},
		{"destination exists", "targets/target-a/rename", "", map[string]any{"new_name": "TARGET-B"}, http.StatusConflict},
		{"merge destination missing", "targets/target-a/rename", "", map[string]any{"new_name": "x", "merge": true}, http.StatusNotFound},
		{"merge into itself", "targets/target-a/rename", "", map[string]any{"new_name": "Target-A", "merge": true}, http.StatusBadRequest},
		{"empty new name", "targets/target-a/rename", "", map[string]any{"new_name": " "}, http.StatusBadRequest},
		{"invalid body", "targets/target-a/rename", "", "not-an-object", http.StatusBadRequest},
		{"stale etag", "targets/target-a/rename", quoted("t1-stale"), map[string]any{"new_name": "x"}, http.StatusConflict},
		{"pricing conflict", "targets/target-a/rename", "", map[string]any{"new_name": "stale-priced"}, http.StatusConflict},
		{"new name has usage history", "targets/target-a/rename", "", map[string]any{"new_name": "retired"}, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRevEnv(t, t.TempDir(), false)
			seedRenameBindings(t, env)
			// A stale template still bound to a deleted target's name.
			if _, err := nosql.NewPricingTemplateStore(env.db).Upsert(pricing.PricingTemplate{Targets: []string{"stale-priced"}, Models: []string{"gpt-4o"}, Mode: pricing.ModeFlat}); err != nil {
				t.Fatal(err)
			}
			// History left by a deleted target named "retired".
			if err := env.stores.usageStore.Record(usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice",
				Target: "retired", Model: "gpt-4o", InputTokens: 10, StatusCode: 200}); err != nil {
				t.Fatal(err)
			}
			before := dumpDB(t, env.db)
			rec := env.do(http.MethodPost, tc.path, tc.ifMatch, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if after := dumpDB(t, env.db); after != before {
				t.Fatal("rejected rename changed the database")
			}
			if env.runtimeEndpoint("target-a") == "" {
				t.Fatal("rejected rename changed runtime targets")
			}
			if audits := renameAudits(t, env); len(audits) != 0 {
				t.Fatalf("rejected rename audited as success: %+v", audits)
			}
		})
	}
}

func TestIssue195AdminRenameRequiresWritableSession(t *testing.T) {
	env := newRevEnv(t, t.TempDir(), true)
	secured := chi.NewRouter()
	secured.Route("/admin", func(r chi.Router) {
		r.Use(env.sm.Middleware)
		r.Mount("/", env.h)
	})
	_, readonly, err := env.sm.CreateSession("viewer", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	post := func(cookie *http.Cookie) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/data/targets/target-a/rename", strings.NewReader(`{"new_name":"x"}`))
		req.Header.Set("Accept", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		secured.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rename status = %d", code)
	}
	if code := post(readonly); code != http.StatusForbidden {
		t.Fatalf("readonly rename status = %d", code)
	}
	if env.runtimeEndpoint("target-a") == "" {
		t.Fatal("readonly session renamed target")
	}
}
