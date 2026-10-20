package nosql

import (
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestModelGroupStoreCRUDAndClientResolution(t *testing.T) {
	db := testDB(t)
	groups := NewModelGroupStore(db)
	clients := NewClientStore(db)
	quotaStore := NewClientQuotaStore(db)
	group := config.ModelGroup{
		Name: "premium", CostCoefficient: 2,
		DailyUSD: 10,
		Bindings: []config.QuotaGroupBinding{{Target: "openai", Model: "gpt-4o"}},
	}
	if err := NewTargetStore(db).Create(config.Target{Name: "openai", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}}}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := groups.Create(group); err != nil {
		t.Fatalf("create model group: %v", err)
	}
	if err := clients.Create(config.Client{Name: "alice", AccessKey: "key-a", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := quotaStore.Replace("alice", []config.QuotaGroup{{Name: "PREMIUM", Coefficient: coefPtr(0.5)}}); err != nil {
		t.Fatalf("replace client quota groups: %v", err)
	}
	stored, err := quotaStore.Get("alice")
	if err != nil {
		t.Fatalf("get client quota groups: %v", err)
	}
	if len(stored) != 1 || !strings.EqualFold(stored[0].Name, "PREMIUM") || stored[0].PlanCoefficient() != 0.5 {
		t.Fatalf("unexpected stored client groups: %+v", stored)
	}
	catalog, err := groups.List()
	if err != nil {
		t.Fatalf("list model groups: %v", err)
	}
	premium := modelGroupByName(catalog, "premium")
	if premium == nil || premium.CostCoefficient != 2 || len(premium.Bindings) != 1 {
		t.Fatalf("global premium group wrong: %+v", premium)
	}

	group.CostCoefficient = 3
	group.Name = "premium-v2"
	// Rename cascades to the active client plan instead of being rejected
	// (alice references "PREMIUM" with case preserved).
	renamed, err := groups.Update("premium", group)
	if err != nil {
		t.Fatalf("referenced model group rename should cascade: %v", err)
	}
	if len(renamed) != 1 || renamed[0] != "alice" {
		t.Fatalf("renamed clients = %v, want [alice]", renamed)
	}
	migrated, err := quotaStore.Get("alice")
	if err != nil {
		t.Fatalf("get alice plan after rename: %v", err)
	}
	if len(migrated) != 1 || migrated[0].Name != "premium-v2" || migrated[0].PlanCoefficient() != 0.5 {
		t.Fatalf("alice plan not migrated to new name: %+v", migrated)
	}
	allGroups, err := groups.List()
	if err != nil {
		t.Fatalf("list model groups: %v", err)
	}
	if modelGroupByName(allGroups, "premium") != nil {
		t.Fatal("old model group name still exists")
	}
	if modelGroupByName(allGroups, "premium-v2") == nil {
		t.Fatal("renamed model group missing")
	}
}

func modelGroupByName(groups []config.ModelGroup, name string) *config.ModelGroup {
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i]
		}
	}
	return nil
}

func TestModelGroupStoreZeroCoefficientIsUnlimited(t *testing.T) {
	groups := NewModelGroupStore(testDB(t))
	group := config.ModelGroup{Name: "unlimited", CostCoefficient: 0, DailyUSD: 10}
	if err := groups.Create(group); err != nil {
		t.Fatalf("zero coefficient must be accepted: %v", err)
	}
	catalog, err := groups.List()
	stored := modelGroupByName(catalog, "unlimited")
	if err != nil || stored == nil || stored.CostCoefficient != 0 {
		t.Fatalf("zero coefficient must be preserved: %+v, %v", stored, err)
	}
	group.CostCoefficient = -1
	if _, err := groups.Update("unlimited", group); err == nil {
		t.Fatal("negative coefficient must be rejected")
	}
}

func TestClientQuotaStoreRejectsUnknownNameOnlyModelGroup(t *testing.T) {
	err := NewClientQuotaStore(testDB(t)).Replace("alice", []config.QuotaGroup{{Name: "missing"}})
	if err == nil {
		t.Fatal("expected unknown model group to be rejected")
	}
}

func TestModelGroupStoreDeleteCascadesClientRefs(t *testing.T) {
	db := testDB(t)
	groups := NewModelGroupStore(db)
	quotaStore := NewClientQuotaStore(db)
	for _, name := range []string{"a", "b", "c"} {
		if err := groups.Create(config.ModelGroup{Name: name, CostCoefficient: 1}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if err := quotaStore.Replace("alice", []config.QuotaGroup{{Name: "a"}}); err != nil {
		t.Fatalf("replace alice: %v", err)
	}
	if err := quotaStore.Replace("bob", []config.QuotaGroup{{Name: "A", Coefficient: coefPtr(3)}, {Name: "b"}}); err != nil {
		t.Fatalf("replace bob: %v", err)
	}
	// Archived (deleted client) snapshot referencing "a": must never be rewritten.
	if err := quotaStore.Replace("carol", []config.QuotaGroup{{Name: "a"}}); err != nil {
		t.Fatalf("replace carol: %v", err)
	}
	if err := quotaStore.Archive(config.Client{Name: "carol", IPAddress: "192.0.2.9", JobLevel: "L5", ProjectGroup: "AI"}); err != nil {
		t.Fatalf("archive carol: %v", err)
	}

	// Deleting an unreferenced group succeeds and touches nobody, even though
	// the bucket holds references to other groups (guards the C1 deserialization bug).
	removed, err := groups.Delete("c")
	if err != nil {
		t.Fatalf("delete unreferenced group c: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("delete c affected clients = %v, want none", removed)
	}

	// Deleting referenced group "a" cascades: alice's entry is removed entirely
	// (plan becomes empty), bob keeps only "b", archived carol is untouched.
	removed, err = groups.Delete("a")
	if err != nil {
		t.Fatalf("delete referenced group a should cascade: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed clients = %v, want alice+bob", removed)
	}
	seen := map[string]bool{removed[0]: true, removed[1]: true}
	if !seen["alice"] || !seen["bob"] {
		t.Fatalf("removed clients = %v, want alice+bob", removed)
	}
	if got, err := quotaStore.Get("alice"); err != nil || got != nil {
		t.Fatalf("alice entry should be gone, got %+v (err %v)", got, err)
	}
	bobGroups, err := quotaStore.Get("bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if len(bobGroups) != 1 || !strings.EqualFold(bobGroups[0].Name, "b") {
		t.Fatalf("bob plan wrong after cascade: %+v", bobGroups)
	}
	entries, err := quotaStore.ListEntries()
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	carol := entries["carol"]
	if carol.DeletedAt == "" || len(carol.Groups) != 1 || carol.Groups[0].Name != "a" {
		t.Fatalf("archived snapshot must not be rewritten: %+v", carol)
	}
}

func TestModelGroupStoreRejectsBindingConflict(t *testing.T) {
	db := testDB(t)
	createBindingTarget(t, db) // target "openai-prod" with upstream "gpt-4o"
	groups := NewModelGroupStore(db)
	if err := groups.Create(config.ModelGroup{Name: "g1", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "openai-prod", Model: "gpt-4o"}}}); err != nil {
		t.Fatalf("create g1: %v", err)
	}
	if err := groups.Create(config.ModelGroup{Name: "g2", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "OPENAI-PROD", Model: "GPT-4O"}}}); err == nil {
		t.Fatal("expected exact binding conflict")
	}
	if err := groups.Create(config.ModelGroup{Name: "g3", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "openai-prod"}}}); err != nil {
		t.Fatalf("whole-target binding should coexist with exact binding: %v", err)
	}
	if err := groups.Create(config.ModelGroup{Name: "g4", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "openai-prod"}}}); err == nil {
		t.Fatal("expected whole-target binding conflict")
	}
}

func TestModelGroupStoreCreateRejectsFictionalTarget(t *testing.T) {
	db := testDB(t)
	groups := NewModelGroupStore(db)
	if err := groups.Create(config.ModelGroup{Name: "g", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "does-not-exist"}}}); err == nil {
		t.Fatal("expected fictional target to be rejected on create")
	}
}

func TestModelGroupStoreUpdatePreservesStaleBinding(t *testing.T) {
	db := testDB(t)
	groups := NewModelGroupStore(db)
	targets := NewTargetStore(db)
	if err := targets.Create(config.Target{Name: "t1", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := groups.Create(config.ModelGroup{Name: "g", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "t1"}}}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := targets.Delete("t1"); err != nil {
		t.Fatalf("delete target: %v", err)
	}
	if _, err := groups.Update("g", config.ModelGroup{Name: "g", CostCoefficient: 2, Bindings: []config.QuotaGroupBinding{{Target: "t1"}}}); err != nil {
		t.Fatalf("update with preserved stale binding: %v", err)
	}
	if _, err := groups.Update("g", config.ModelGroup{Name: "g", CostCoefficient: 2, Bindings: []config.QuotaGroupBinding{{Target: "t1"}, {Target: "t2"}}}); err == nil {
		t.Fatal("expected new fictional binding to be rejected on update")
	}
}

func TestModelGroupCoefficientLookup(t *testing.T) {
	db := testDB(t)
	groups := NewModelGroupStore(db)
	if err := NewTargetStore(db).Create(config.Target{Name: "openai", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}, {Upstream: "gpt-4o-mini"}}}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	// premium: whole-target discount 0.5; vip: exact (openai, gpt-4o) discount 0.2.
	if err := groups.Create(config.ModelGroup{Name: "premium", CostCoefficient: 0.5, Bindings: []config.QuotaGroupBinding{{Target: "openai"}}}); err != nil {
		t.Fatalf("create premium: %v", err)
	}
	if err := groups.Create(config.ModelGroup{Name: "vip", CostCoefficient: 0.2, Bindings: []config.QuotaGroupBinding{{Target: "openai", Model: "gpt-4o"}}}); err != nil {
		t.Fatalf("create vip: %v", err)
	}

	lookup, err := groups.CoefficientLookup()
	if err != nil {
		t.Fatalf("coefficient lookup: %v", err)
	}

	cases := []struct {
		target, model string
		want          float64
	}{
		{"openai", "gpt-4o", 0.2},      // exact binding wins
		{" OpenAI ", "GPT-4O", 0.2},    // case-insensitive, trimmed
		{"openai", "gpt-4o-mini", 0.5}, // falls back to whole-target binding
		{"other", "any", 1},            // unmatched → 1
		{"", "gpt-4o", 1},              // empty target → 1
	}
	for _, tc := range cases {
		if got := lookup(tc.target, tc.model); got != tc.want {
			t.Errorf("lookup(%q, %q) = %v, want %v", tc.target, tc.model, got, tc.want)
		}
	}
}
