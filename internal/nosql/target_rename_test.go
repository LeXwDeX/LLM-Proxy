package nosql

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

func renameTestTarget(name string) config.Target {
	return config.Target{Name: name, Endpoint: "https://upstream.invalid", APIKey: "k", UsageAdapter: config.UsageAdapterStandard,
		ModelMappings: []config.ModelMapping{{Upstream: "m1"}}}
}

type renameEnv struct {
	db        *bolt.DB
	targets   *TargetStore
	templates *PricingTemplateStore
	groups    *ModelGroupStore
	clients   *ClientStore
}

func newRenameEnv(t *testing.T, names ...string) *renameEnv {
	t.Helper()
	db := testDB(t)
	env := &renameEnv{db: db, targets: NewTargetStore(db), templates: NewPricingTemplateStore(db), groups: NewModelGroupStore(db), clients: NewClientStore(db)}
	for _, name := range names {
		if err := env.targets.Create(renameTestTarget(name)); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func dumpAll(t *testing.T, db *bolt.DB) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			return b.ForEach(func(k, v []byte) error {
				fmt.Fprintf(&buf, "%s/%s=%s\n", name, k, v)
				return nil
			})
		})
	}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTargetRenameExtendsBindingsAndRewritesACL(t *testing.T) {
	env := newRenameEnv(t, "Old", "other")
	legacy, err := env.templates.Upsert(pricing.PricingTemplate{Target: "Old", Model: "m1", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := env.templates.Upsert(pricing.PricingTemplate{Targets: []string{"other", "old"}, Models: []string{"m2"}, Mode: pricing.ModeFlat})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := env.templates.Upsert(pricing.PricingTemplate{Targets: []string{"other"}, Models: []string{"m1"}, Mode: pricing.ModeFlat})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.groups.Create(config.ModelGroup{Name: "g1", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "old", Model: "m1"}, {Target: "other"}}}); err != nil {
		t.Fatal(err)
	}
	if err := env.groups.Create(config.ModelGroup{Name: "g2", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "old"}}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []config.Client{
		{Name: "alice", AccessKey: "a", AllowedTargets: []string{"old"}},
		{Name: "bob", AccessKey: "b", AllowedTargets: []string{"other"}},
		{Name: "carol", AccessKey: "c", AllowedTargets: []string{"OLD", "new"}},
	} {
		if err := env.clients.Create(c); err != nil {
			t.Fatal(err)
		}
	}
	etag, err := env.targets.ETag("old")
	if err != nil {
		t.Fatal(err)
	}

	res, err := env.targets.Rename(TargetRenameRequest{From: "old", To: "New", ExpectedETag: &etag})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "Old" || res.To != "New" || res.Merged {
		t.Fatalf("result identity: %+v", res)
	}
	wantIDs := []string{legacy.ID, shared.ID}
	if wantIDs[0] > wantIDs[1] {
		wantIDs[0], wantIDs[1] = wantIDs[1], wantIDs[0]
	}
	if !reflect.DeepEqual(res.PricingTemplates, wantIDs) || !reflect.DeepEqual(res.ModelGroups, []string{"g1", "g2"}) || !reflect.DeepEqual(res.Clients, []string{"alice", "carol"}) {
		t.Fatalf("synced objects: %+v", res)
	}

	list, err := env.targets.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "New" || list[1].Name != "other" || list[0].ModelMappings[0].Upstream != "m1" {
		t.Fatalf("targets after rename (order and fields kept): %+v", list)
	}
	templates, err := env.templates.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range templates {
		got := tmpl.EffectiveTargets()
		switch tmpl.ID {
		case legacy.ID:
			if !reflect.DeepEqual(got, []string{"Old", "New"}) || tmpl.Target != "Old" {
				t.Fatalf("legacy template must keep old binding first: %+v", tmpl)
			}
		case shared.ID:
			if !reflect.DeepEqual(got, []string{"other", "old", "New"}) {
				t.Fatalf("shared template targets: %v", got)
			}
		case unrelated.ID:
			if !reflect.DeepEqual(got, []string{"other"}) {
				t.Fatalf("unrelated template changed: %v", got)
			}
		}
	}
	resolver := pricing.NewResolver(templates, nil)
	for _, target := range []string{"old", "new"} {
		if rates, ok := resolver.RateFor(target, "m1", time.Now()); !ok || rates.InputPer1MTokens != 1 {
			t.Fatalf("RateFor(%s) = %+v %t", target, rates, ok)
		}
	}
	groups, err := env.groups.List()
	if err != nil {
		t.Fatal(err)
	}
	wantGroups := map[string][]config.QuotaGroupBinding{
		"g1": {{Target: "old", Model: "m1"}, {Target: "other"}, {Target: "New", Model: "m1"}},
		"g2": {{Target: "old"}, {Target: "New"}},
	}
	for _, g := range groups {
		if !reflect.DeepEqual(g.Bindings, wantGroups[g.Name]) {
			t.Fatalf("group %s bindings = %+v", g.Name, g.Bindings)
		}
	}
	clients, err := env.clients.List()
	if err != nil {
		t.Fatal(err)
	}
	wantACL := map[string][]string{"alice": {"new"}, "bob": {"other"}, "carol": {"new"}}
	for _, c := range clients {
		if !reflect.DeepEqual(c.AllowedTargets, wantACL[c.Name]) {
			t.Fatalf("client %s allowed_targets = %v", c.Name, c.AllowedTargets)
		}
	}
	// Model groups stay editable with the retained old-name (now stale) binding.
	for _, g := range groups {
		if _, err := env.groups.Update(g.Name, g); err != nil {
			t.Fatalf("model group %s update with retained old-name binding: %v", g.Name, err)
		}
	}
}

func TestTargetMergeRemovesSourceKeepsBindingsAndReplacesACL(t *testing.T) {
	env := newRenameEnv(t, "a", "b")
	if _, err := env.templates.Upsert(pricing.PricingTemplate{Targets: []string{"a"}, Models: []string{"m1"}, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.templates.Upsert(pricing.PricingTemplate{Targets: []string{"b"}, Models: []string{"m1"}, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := env.groups.Create(config.ModelGroup{Name: "ga", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if err := env.groups.Create(config.ModelGroup{Name: "gb", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := env.clients.Create(config.Client{Name: "alice", AccessKey: "a", AllowedTargets: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	templatesBefore, err := env.templates.List()
	if err != nil {
		t.Fatal(err)
	}
	groupsBefore, err := env.groups.List()
	if err != nil {
		t.Fatal(err)
	}

	res, err := env.targets.Rename(TargetRenameRequest{From: "a", To: "b", Merge: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged || len(res.PricingTemplates) != 0 || len(res.ModelGroups) != 0 || !reflect.DeepEqual(res.Clients, []string{"alice"}) {
		t.Fatalf("merge result: %+v", res)
	}
	list, err := env.targets.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "b" {
		t.Fatalf("targets after merge: %+v", list)
	}
	templatesAfter, err := env.templates.List()
	if err != nil {
		t.Fatal(err)
	}
	groupsAfter, err := env.groups.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(templatesBefore, templatesAfter) || !reflect.DeepEqual(groupsBefore, groupsAfter) {
		t.Fatal("merge must not change pricing templates or model groups")
	}
	resolver := pricing.NewResolver(templatesAfter, nil)
	if rates, ok := resolver.RateFor("a", "m1", time.Now()); !ok || rates.InputPer1MTokens != 3 {
		t.Fatalf("merged source history price = %+v %t, want source template", rates, ok)
	}
	alice, err := env.clients.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(alice.AllowedTargets, []string{"b"}) {
		t.Fatalf("ACL after merge must not become empty (allow-all): %v", alice.AllowedTargets)
	}
}

func TestTargetRenameRejectsConflictsAtomically(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, env *renameEnv)
		req   TargetRenameRequest
		want  error
	}{
		{name: "missing source", req: TargetRenameRequest{From: "nope", To: "x"}, want: ErrTargetNotFound},
		{name: "destination exists without merge", req: TargetRenameRequest{From: "a", To: "b"}, want: ErrTargetRenameConflict},
		{name: "merge destination missing", req: TargetRenameRequest{From: "a", To: "x", Merge: true}, want: ErrTargetNotFound},
		{name: "merge into itself", req: TargetRenameRequest{From: "a", To: "A", Merge: true}, want: ErrTargetRenameConflict},
		{name: "stale etag", req: TargetRenameRequest{From: "a", To: "x", ExpectedETag: ptrString("t1-stale")}, want: ErrTargetETagConflict},
		{
			name: "pricing overlap with stale new-name template",
			setup: func(t *testing.T, env *renameEnv) {
				for _, target := range []string{"a", "x"} {
					if _, err := env.templates.Upsert(pricing.PricingTemplate{Targets: []string{target}, Models: []string{"m1"}, Mode: pricing.ModeFlat}); err != nil {
						t.Fatal(err)
					}
				}
			},
			req: TargetRenameRequest{From: "a", To: "x"}, want: ErrTargetRenameConflict,
		},
		{
			name: "model group binding uniqueness",
			setup: func(t *testing.T, env *renameEnv) {
				if err := env.groups.Create(config.ModelGroup{Name: "ga", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "a"}}}); err != nil {
					t.Fatal(err)
				}
				// A stale whole-target binding for "x" left over from a deleted target.
				if err := env.targets.Create(renameTestTarget("x")); err != nil {
					t.Fatal(err)
				}
				if err := env.groups.Create(config.ModelGroup{Name: "gx", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "x"}}}); err != nil {
					t.Fatal(err)
				}
				if err := env.targets.Delete("x"); err != nil {
					t.Fatal(err)
				}
			},
			req: TargetRenameRequest{From: "a", To: "x"}, want: ErrTargetRenameConflict,
		},
		{
			// A previously deleted target named "X" left usage history; extending
			// bindings to it would reprice that history.
			name: "new name has usage history",
			setup: func(t *testing.T, env *renameEnv) {
				if err := NewUsageStore(env.db).Record(usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice",
					Target: "X", Model: "m1", InputTokens: 10, StatusCode: 200}); err != nil {
					t.Fatal(err)
				}
			},
			req: TargetRenameRequest{From: "a", To: "x"}, want: ErrTargetRenameConflict,
		},
		{name: "invalid new name", req: TargetRenameRequest{From: "a", To: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRenameEnv(t, "a", "b")
			if err := env.clients.Create(config.Client{Name: "alice", AccessKey: "k", AllowedTargets: []string{"a"}}); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, env)
			}
			before := dumpAll(t, env.db)
			_, err := env.targets.Rename(tc.req)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if after := dumpAll(t, env.db); !bytes.Equal(before, after) {
				t.Fatal("rejected rename changed the database")
			}
		})
	}
}

// Merging into a destination that has history is the normal case and must not
// be blocked by the history guard (which only protects plain renames).
func TestTargetMergeAllowsDestinationWithHistory(t *testing.T) {
	env := newRenameEnv(t, "a", "b")
	if err := NewUsageStore(env.db).Record(usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice",
		Target: "b", Model: "m1", InputTokens: 10, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.targets.Rename(TargetRenameRequest{From: "a", To: "b", Merge: true}); err != nil {
		t.Fatal(err)
	}
}

func TestTargetRenameCaseOnlyKeepsBindings(t *testing.T) {
	env := newRenameEnv(t, "a")
	if _, err := env.templates.Upsert(pricing.PricingTemplate{Targets: []string{"a"}, Models: []string{"m1"}, Mode: pricing.ModeFlat}); err != nil {
		t.Fatal(err)
	}
	if err := NewUsageStore(env.db).Record(usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice",
		Target: "a", Model: "m1", InputTokens: 10, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	res, err := env.targets.Rename(TargetRenameRequest{From: "a", To: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PricingTemplates)+len(res.ModelGroups)+len(res.Clients) != 0 {
		t.Fatalf("case-only rename touched bindings: %+v", res)
	}
	list, err := env.targets.List()
	if err != nil || len(list) != 1 || list[0].Name != "A" {
		t.Fatalf("case-only rename: %+v %v", list, err)
	}
}

func ptrString(s string) *string { return &s }
